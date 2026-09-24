package report

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// KeyHeader carries the ingest key.
	KeyHeader = "X-Systicore-Key"
	userAgent = "systicore_go-report"

	// defaultRetryAfter applies to a 429 without a usable Retry-After:
	// the backend's rate windows are one minute long.
	defaultRetryAfter = time.Minute
	// maxRetryAfter caps what a 429 can make the client wait.
	maxRetryAfter = circuitCooldown
)

// ingestTransport performs one POST of an encoded report. It knows HTTP,
// not policy: classifying the answer and retrying is the caller's job.
type ingestTransport struct {
	endpoint       string
	key            string
	httpClient     *http.Client
	attemptTimeout time.Duration
}

// withoutRedirects returns a copy of httpClient that never follows a
// redirect. The key travels in a custom header, which net/http copies to
// the redirect target (it strips only Authorization, Cookie and the like),
// even on a cross-host or https-to-http redirect; so the client stops at
// the 3xx and the policy drops the report instead. The caller's client is
// left untouched: it may be shared with code that wants redirects.
func withoutRedirects(httpClient *http.Client) *http.Client {
	copied := *httpClient
	copied.CheckRedirect = refuseRedirect
	return &copied
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// ingestResponse is what policy needs from one answer.
type ingestResponse struct {
	status     int
	retryAfter time.Duration
}

func (t ingestTransport) send(ctx context.Context, body []byte) (ingestResponse, error) {
	attemptContext, cancel := context.WithTimeout(ctx, t.attemptTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(attemptContext, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return ingestResponse{}, fmt.Errorf("report: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set(KeyHeader, t.key)
	response, err := t.httpClient.Do(request)
	if err != nil {
		return ingestResponse{}, fmt.Errorf("report: send: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return ingestResponse{
		status:     response.StatusCode,
		retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}, nil
}

// parseRetryAfter reads delta-seconds or an HTTP date, falling back to
// defaultRetryAfter and capping at maxRetryAfter.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultRetryAfter
	}
	var wait time.Duration
	if seconds, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(value); err == nil {
		wait = date.Sub(now)
	} else {
		return defaultRetryAfter
	}
	if wait < 0 {
		return 0
	}
	return min(wait, maxRetryAfter)
}

// outcome is the policy class of one delivery attempt (contract §3).
type outcome int

const (
	outcomeDelivered    outcome = iota // 2xx (202 accepted)
	outcomeRejected                    // 3xx, 400, 404, 413, 422: permanent, drop
	outcomeUnauthorized                // 401, 403: permanent, open the circuit
	outcomeRateLimited                 // 429: wait Retry-After, then retry
	outcomeRetryable                   // 5xx, other statuses, network errors: back off, retry
)

func classify(response ingestResponse, sendError error) outcome {
	if sendError != nil {
		return outcomeRetryable
	}
	switch status := response.status; {
	case status >= 200 && status < 300:
		return outcomeDelivered
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return outcomeUnauthorized
	case status == http.StatusTooManyRequests:
		return outcomeRateLimited
	case isRedirect(status), status == http.StatusBadRequest, status == http.StatusNotFound,
		status == http.StatusRequestEntityTooLarge, status == http.StatusUnprocessableEntity:
		return outcomeRejected
	}
	return outcomeRetryable
}

// isRedirect reports a 3xx, which the client answers without following.
func isRedirect(status int) bool {
	return status >= 300 && status < 400
}
