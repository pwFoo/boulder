package akamai

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
)

const (
	v3PurgePath     = "/ccu/v3/delete/url/"
	timestampFormat = "20060102T15:04:05-0700"
)

type v3PurgeRequest struct {
	Objects []string `json:"objects"`
}

type purgeResponse struct {
	HTTPStatus       int    `json:"httpStatus"`
	Detail           string `json:"detail"`
	EstimatedSeconds int    `json:"estimatedSeconds"`
	PurgeID          string `json:"purgeId"`
}

// CachePurgeClient talks to the Akamai CCU REST API. It is safe to make concurrent
// purge requests.
type CachePurgeClient struct {
	client       *http.Client
	apiEndpoint  string
	apiHost      string
	apiScheme    string
	clientToken  string
	clientSecret string
	accessToken  string
	v3Network    string
	retries      int
	retryBackoff time.Duration
	log          blog.Logger
	stats        metrics.Scope
	clk          clock.Clock
}

// errFatal is used by CachePurgeClient.purge to indicate that it failed for a
// reason that cannot be remediated by retying a purge request
type errFatal string

func (e errFatal) Error() string { return string(e) }

var (
	// ErrAllRetriesFailed lets the caller of Purge to know if all the purge submission
	// attempts failed
	ErrAllRetriesFailed = errors.New("All attempts to submit purge request failed")
)

// NewCachePurgeClient constructs a new CachePurgeClient
func NewCachePurgeClient(
	endpoint,
	clientToken,
	clientSecret,
	accessToken string,
	v3Network string,
	retries int,
	retryBackoff time.Duration,
	log blog.Logger,
	stats metrics.Scope,
) (*CachePurgeClient, error) {
	stats = stats.NewScope("CCU")
	if strings.HasSuffix(endpoint, "/") {
		endpoint = endpoint[:len(endpoint)-1]
	}
	apiURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	// The network string must be either "production" or "staging".
	if v3Network != "production" && v3Network != "staging" {
		return nil, fmt.Errorf(
			"Invalid CCU v3 network: %q. Must be \"staging\" or \"production\"", v3Network)
	}
	return &CachePurgeClient{
		client:       new(http.Client),
		apiEndpoint:  endpoint,
		apiHost:      apiURL.Host,
		apiScheme:    strings.ToLower(apiURL.Scheme),
		clientToken:  clientToken,
		clientSecret: clientSecret,
		accessToken:  accessToken,
		v3Network:    v3Network,
		retries:      retries,
		retryBackoff: retryBackoff,
		log:          log,
		stats:        stats,
		clk:          clock.Default(),
	}, nil
}

// Akamai uses a special authorization header to identify clients to their EdgeGrid
// APIs, their docs (https://developer.akamai.com/introduction/Client_Auth.html)
// provide a  description of the required generation process.
func (cpc *CachePurgeClient) constructAuthHeader(body []byte, apiPath string, nonce string) (string, error) {
	// The akamai API is very time sensitive (recommending reliance on a stratum 2
	// or better time source) and, although it doesn't say it anywhere, really wants
	// the timestamp to be in the UTC timezone for some reason.
	timestamp := cpc.clk.Now().UTC().Format(timestampFormat)
	header := fmt.Sprintf(
		"EG1-HMAC-SHA256 client_token=%s;access_token=%s;timestamp=%s;nonce=%s;",
		cpc.clientToken,
		cpc.accessToken,
		timestamp,
		nonce,
	)
	bodyHash := sha256.Sum256(body)
	tbs := fmt.Sprintf(
		"%s\t%s\t%s\t%s\t%s\t%s\t%s",
		"POST",
		cpc.apiScheme,
		cpc.apiHost,
		apiPath,
		"", // We don't need to send any signed headers for a purge so this can be blank
		base64.StdEncoding.EncodeToString(bodyHash[:]),
		header,
	)

	cpc.log.Debugf("To-be-signed Akamai EdgeGrid authentication: %q", tbs)

	h := hmac.New(sha256.New, signingKey(cpc.clientSecret, timestamp))
	h.Write([]byte(tbs))
	return fmt.Sprintf(
		"%ssignature=%s",
		header,
		base64.StdEncoding.EncodeToString(h.Sum(nil)),
	), nil
}

// signingKey makes a signing key by HMAC'ing the timestamp
// using a client secret as the key.
func signingKey(clientSecret string, timestamp string) []byte {
	h := hmac.New(sha256.New, []byte(clientSecret))
	h.Write([]byte(timestamp))
	key := make([]byte, base64.StdEncoding.EncodedLen(32))
	base64.StdEncoding.Encode(key, h.Sum(nil))
	return key
}

// purge actually sends the individual requests to the Akamai endpoint and checks
// if they are successful
func (cpc *CachePurgeClient) purge(urls []string) error {
	purgeReq := v3PurgeRequest{
		Objects: urls,
	}
	endpoint := fmt.Sprintf("%s%s%s", cpc.apiEndpoint, v3PurgePath, cpc.v3Network)

	reqJSON, err := json.Marshal(purgeReq)
	if err != nil {
		return errFatal(err.Error())
	}
	req, err := http.NewRequest(
		"POST",
		endpoint,
		bytes.NewBuffer(reqJSON),
	)
	if err != nil {
		return errFatal(err.Error())
	}

	// Create authorization header for request
	authHeader, err := cpc.constructAuthHeader(
		reqJSON,
		v3PurgePath+cpc.v3Network,
		core.RandomString(16),
	)
	if err != nil {
		return errFatal(err.Error())
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	cpc.log.Debugf("POSTing to %s with Authorization %s: %s",
		endpoint, authHeader, reqJSON)

	rS := cpc.clk.Now()
	resp, err := cpc.client.Do(req)
	cpc.stats.TimingDuration("PurgeRequestLatency", time.Since(rS))
	if err != nil {
		return err
	}
	if resp.Body == nil {
		return fmt.Errorf("No response body")
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}

	// Check purge was successful
	var purgeInfo purgeResponse
	err = json.Unmarshal(body, &purgeInfo)
	if err != nil {
		return fmt.Errorf("%s. Body was: %s", err, body)
	}
	if purgeInfo.HTTPStatus != http.StatusCreated || resp.StatusCode != http.StatusCreated {
		if purgeInfo.HTTPStatus == http.StatusForbidden {
			return errFatal(fmt.Sprintf("Unauthorized to purge URLs %q", urls))
		}
		return fmt.Errorf("Unexpected HTTP status code '%d': %s", resp.StatusCode, string(body))
	}

	cpc.log.Infof("Sent successful purge request purgeID: %s, purge expected in: %ds, for URLs: %s",
		purgeInfo.PurgeID, purgeInfo.EstimatedSeconds, urls)

	return nil
}

func (cpc *CachePurgeClient) purgeBatch(urls []string) error {
	successful := false
	for i := 0; i <= cpc.retries; i++ {
		cpc.clk.Sleep(core.RetryBackoff(i, cpc.retryBackoff, time.Minute, 1.3))

		err := cpc.purge(urls)
		if err != nil {
			if _, ok := err.(errFatal); ok {
				cpc.stats.Inc("FatalFailures", 1)
				return err
			}
			cpc.log.AuditErrf("Akamai cache purge failed, retrying: %s", err)
			cpc.stats.Inc("RetryableFailures", 1)
			continue
		}
		successful = true
		break
	}

	if !successful {
		cpc.stats.Inc("FatalFailures", 1)
		return ErrAllRetriesFailed
	}

	cpc.stats.Inc("SuccessfulPurges", 1)
	return nil
}

var akamaiBatchSize = 100

// Purge attempts to send a purge request to the Akamai CCU API cpc.retries number
//  of times before giving up and returning ErrAllRetriesFailed
func (cpc *CachePurgeClient) Purge(urls []string) error {
	for i := 0; i < len(urls); {
		sliceEnd := i + akamaiBatchSize
		if sliceEnd > len(urls) {
			sliceEnd = len(urls)
		}
		err := cpc.purgeBatch(urls[i:sliceEnd])
		if err != nil {
			return err
		}
		i += akamaiBatchSize
	}
	return nil
}

// CheckSignature is used for tests, it exported so that it can be used in akamai-test-srv
func CheckSignature(secret string, url string, r *http.Request, body []byte) error {
	bodyHash := sha256.Sum256(body)
	bodyHashB64 := base64.StdEncoding.EncodeToString(bodyHash[:])

	authorization := r.Header.Get("Authorization")
	authValues := make(map[string]string)
	for _, v := range strings.Split(authorization, ";") {
		splitValue := strings.Split(v, "=")
		authValues[splitValue[0]] = splitValue[1]
	}
	headerTimestamp := authValues["timestamp"]
	splitHeader := strings.Split(authorization, "signature=")
	shortenedHeader, signature := splitHeader[0], splitHeader[1]
	hostPort := strings.Split(url, "://")[1]
	h := hmac.New(sha256.New, signingKey(secret, headerTimestamp))
	input := []byte(fmt.Sprintf("POST\thttp\t%s\t%s\t\t%s\t%s",
		hostPort,
		r.URL.Path,
		bodyHashB64,
		shortenedHeader,
	))
	h.Write(input)
	expectedSignature := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if signature != expectedSignature {
		return fmt.Errorf("Wrong signature %q in %q. Expected %q\n",
			signature, authorization, expectedSignature)
	}
	return nil
}
