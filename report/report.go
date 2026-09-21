// Package report is the client of the systicore_reports backend for Go
// services: it submits error reports the same way the mobile and web apps
// do (POST /api/v1/report), so a backend crash or a failed background job
// lands in the same triage list, dedup, severity escalation and
// automation pipeline as an app error.
//
// Two ways in: Submit (synchronous, for tests and one-offs) and Capture /
// CaptureError (non-blocking: a bounded queue and one sender goroutine
// with retry, so reporting can never slow down or take down the caller).
// Hook turns every Error-or-worse zerolog line into a report.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Severity mirrors the reports backend's enum. Empty = error.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Report is one error occurrence. Code + Message + Action (with the
// device) form the server-side dedup identity: repeats of the same triple
// become occurrences of one report, so keep Code stable (an error kind,
// not a formatted message) and put variable detail into Trace.
type Report struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Trace    string   `json:"trace,omitempty"`
	Action   string   `json:"action,omitempty"`
	Severity Severity `json:"severity,omitempty"`
}

// Device is what the backend calls the reporting installation. For a
// server it is the host — see ServerDevice.
type Device struct {
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OSVersion  string `json:"osVersion"`
	APILevel   int    `json:"apiLevel"`
	AppVersion string `json:"appVersion"`
}

// TokenSource supplies the bearer token for the reports backend. Services
// plug in their client_credentials token source; an empty token with a
// nil error means "send no Authorization header" (development).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a TokenSource for a fixed token (tests, scripts).
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// Options configures a Client. BaseURL and Source are required.
type Options struct {
	// BaseURL of the reports backend, e.g. https://reports.systicore.hu.
	BaseURL string
	// Source identifies the reporting service to the projects backend's
	// source → project mapping, e.g. "blacklab_backend".
	Source string
	// Device defaults to ServerDevice("").
	Device Device
	// Tokens is nil in development (no Authorization header).
	Tokens TokenSource
	// HTTPClient defaults to a 10s-timeout client.
	HTTPClient *http.Client
	// QueueSize bounds Capture's buffer; a full queue drops (default 256).
	QueueSize int
	// Attempts per report before giving up on transient failures (default 3).
	Attempts int
	// Backoff between attempts, doubled each time (default 1s).
	Backoff time.Duration
	// Logger for the client's own failures (dropped / rejected reports). Pass
	// a logger WITHOUT the report Hook attached, or the hook would report
	// the reporter. Default: silent.
	Logger zerolog.Logger
}

// ErrRejected is returned (or logged) when the backend answered a
// permanent 4xx: the payload itself is wrong, retrying cannot help.
var ErrRejected = errors.New("report: rejected by the reports backend")

// Client submits reports. Create one per process with New and Close it on
// shutdown so queued reports get flushed.
type Client struct {
	opt      Options
	endpoint string
	http     *http.Client
	log      zerolog.Logger

	queue   chan queued
	done    chan struct{}
	closing chan struct{}
	dropped dropCounter
}

type queued struct {
	report Report
}

// New validates the options and starts the sender goroutine.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" || o.Source == "" {
		return nil, errors.New("report: BaseURL and Source are required")
	}
	if o.Device == (Device{}) {
		o.Device = ServerDevice("")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	if o.Backoff <= 0 {
		o.Backoff = time.Second
	}
	c := &Client{
		opt:      o,
		endpoint: strings.TrimRight(o.BaseURL, "/") + "/api/v1/report",
		http:     o.HTTPClient,
		log:      o.Logger.With().Str("component", "report").Logger(),
		queue:    make(chan queued, o.QueueSize),
		done:     make(chan struct{}),
		closing:  make(chan struct{}),
	}
	go c.run()
	return c, nil
}

// Submit sends one report synchronously, with retries on transient
// failures. Returns ErrRejected (wrapped) on a permanent 4xx.
func (c *Client) Submit(ctx context.Context, r Report) error {
	var lastErr error
	backoff := c.opt.Backoff
	for attempt := 1; attempt <= c.opt.Attempts; attempt++ {
		err := c.post(ctx, r)
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, ErrRejected) || ctx.Err() != nil {
			return err
		}
		if attempt < c.opt.Attempts {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
	}
	return lastErr
}

// post is one HTTP attempt. Status classes follow the outbox pusher's
// rules: 2xx ok; 400/404/413/422 permanent; 401/403 (token not yet
// provisioned), 429 and 5xx transient.
func (c *Client) post(ctx context.Context, r Report) error {
	if r.Severity == "" {
		r.Severity = SeverityError
	}
	body, err := json.Marshal(struct {
		Source string `json:"source"`
		Device Device `json:"device"`
		Error  Report `json:"error"`
	}{c.opt.Source, c.opt.Device, r})
	if err != nil {
		return fmt.Errorf("report: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("report: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.opt.Tokens != nil {
		token, err := c.opt.Tokens.Token(ctx)
		if err != nil {
			return fmt.Errorf("report: token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("report: send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusRequestEntityTooLarge, resp.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w (HTTP %d)", ErrRejected, resp.StatusCode)
	default:
		return fmt.Errorf("report: reports backend answered HTTP %d", resp.StatusCode)
	}
}
