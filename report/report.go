// Package report is the client of the systicore_reports backend for Go
// services: it submits error reports to the same ingest endpoint the
// mobile and web apps use (POST /api/v1/ingest with a per-component
// ingest key), so a backend crash or a failed background job lands in the
// same grouping, triage and automation pipeline as an app error.
//
// Reporting is off when REPORTS_URL or REPORTS_KEY is empty: New then
// returns a disabled client, and every method of a disabled or nil
// *Client is a no-op. Services wire it unconditionally and let the
// environment decide.
//
// Two ways in: Submit (synchronous, for tests and one-offs) and Capture /
// CaptureError (non-blocking: a bounded queue and one sender goroutine
// with retry, so reporting can never slow down or take down the caller).
// Recover guards goroutines, Hook and SlogHandler are opt-in bridges from
// log lines, and the fiberv2 / fiberv3 modules report panics and 5xx.
package report

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
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

// Report is one error occurrence. The reports backend groups occurrences
// by source + Type (or Code when Type is empty) + the normalized Message
// (numbers, UUIDs, hex ids, emails stripped) + the top in-app frames of
// Trace, falling back to Action when Trace has no frames. Fingerprint
// overrides that computation when the caller knows better.
type Report struct {
	// Type is the error's Go type, e.g. "*fs.PathError" (see ErrorType).
	Type string
	// Code is a stable application code, e.g. "db.deadlock" (see Coder).
	Code string
	// Message is the error text; variable parts are fine, the backend
	// normalizes them for grouping.
	Message string
	// Trace is a stack trace; its top in-app frames take part in grouping.
	Trace string
	// Action is the route template or operation ("GET /orders/:id",
	// "eventpush.deliver"), never a raw URL with ids in it.
	Action   string
	Severity Severity
	// Fingerprint optionally overrides the backend's grouping key:
	// at most 64 characters of [A-Za-z0-9._:-].
	Fingerprint string
	User        User
	Context     Context
}

// User is the authenticated user the failing request ran for. With a
// secret (scsk_) key the backend stores it as verified; Issuer defaults
// to "backend:<source>" server-side.
type User struct {
	ID     string
	Issuer string
}

// Context locates the occurrence: the route template, the request id that
// joins the report with the service logs, and up to 10 free-form tags.
type Context struct {
	Route     string
	RequestID string
	Tags      map[string]string
}

// Release identifies the build that produced the report, as stamped by
// the service's -ldflags (Version, Commit, BuildTime).
type Release struct {
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
}

// Device is what the backend calls the reporting installation. For a
// server it is the service instance — see ServerDevice.
type Device struct {
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OSVersion  string `json:"osVersion"`
	APILevel   int    `json:"apiLevel"`
	AppVersion string `json:"appVersion"`
	InstallID  string `json:"installId"`
}

// Options configures a Client. Map REPORTS_URL, REPORTS_KEY and
// INSTANCE_ID onto BaseURL, Key and InstanceID; when BaseURL or Key is
// empty the client is disabled and nothing is sent.
type Options struct {
	// BaseURL of the reports backend, e.g. https://reports.systicore.hu.
	// Plain http only on a private network: the key travels in a header.
	BaseURL string
	// Key is the component's secret ingest key (scsk_live_… /
	// scsk_test_…), sent as X-Systicore-Key. It identifies the project and
	// environment.
	Key string
	// Source is the service name, e.g. "blacklab_backend". It only feeds
	// the default InstanceID; the backend takes the source from the key.
	Source string
	// Environment is informational ("production", "development",
	// "staging"); the key's environment is authoritative.
	Environment string
	// Release is the build stamped into the binary.
	Release Release
	// InstanceID names this instance stably across redeploys (the device
	// install id). Default "<Source>-<Environment>".
	InstanceID string
	// Device defaults to ServerDevice(Release.Version, InstanceID).
	Device Device
	// HTTPClient defaults to a 10s-timeout client. The client uses a copy
	// that never follows redirects, so the key cannot travel to another
	// host: a 3xx answer drops the report (ErrRejected).
	HTTPClient *http.Client
	// QueueSize bounds Capture's buffer; a full queue drops (default 256).
	QueueSize int
	// Attempts per report before giving up on 429 / 5xx / network
	// failures (default 3).
	Attempts int
	// Backoff between attempts after a 5xx or network failure, doubled
	// each time (default 1s).
	Backoff time.Duration
	// Logger for the client's own failures (dropped / rejected reports).
	// Pass a logger WITHOUT the report Hook attached. Default: silent.
	Logger zerolog.Logger

	// clock is replaced by tests to control time.
	clock clock
}

var (
	// ErrRejected is returned (or logged) when the backend answered a
	// permanent 4xx: the payload or the key is wrong, retrying cannot help.
	ErrRejected = errors.New("report: rejected by the reports backend")
	// ErrUnauthorized accompanies ErrRejected on a 401/403: the key is
	// unknown, revoked, expired or not allowed. The client stops sending
	// for circuitCooldown afterwards.
	ErrUnauthorized = errors.New("report: ingest key rejected")
	// ErrCircuitOpen is returned while sending is paused after a 401/403.
	ErrCircuitOpen = errors.New("report: sending paused after the ingest key was rejected")
	// ErrRateLimited is returned when the attempts ran out on 429 answers.
	ErrRateLimited = errors.New("report: rate limited by the reports backend")
)

const (
	defaultQueueSize   = 256
	defaultAttempts    = 3
	defaultBackoff     = time.Second
	defaultHTTPTimeout = 10 * time.Second
	ingestPath         = "/api/v1/ingest"
)

// Client submits reports. Create one per process with New and Close it on
// shutdown so queued reports get flushed. A nil *Client is a valid,
// disabled client.
type Client struct {
	options   Options
	device    Device
	transport ingestTransport
	gate      *deliveryGate
	clock     clock
	logger    zerolog.Logger

	queue         chan Report
	closeMutex    sync.RWMutex
	closed        bool
	senderContext context.Context
	stopSender    context.CancelFunc
	done          chan struct{}
	dropped       dropCounter
}

// New validates the options and starts the sender goroutine. BaseURL and
// Key are trimmed first (a CRLF .env leaves a trailing \r). With an empty
// BaseURL or Key it returns a disabled client and no error, so "reporting
// is off" is a configuration state, not a failure. A malformed BaseURL or
// a Key that is not shaped like an ingest key is an error.
func New(options Options) (*Client, error) {
	options.BaseURL = strings.TrimSpace(options.BaseURL)
	options.Key = strings.TrimSpace(options.Key)
	if options.BaseURL == "" || options.Key == "" {
		return &Client{}, nil
	}
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	options = withDefaults(options)
	senderContext, stopSender := context.WithCancel(context.Background())
	client := &Client{
		options: options,
		device:  options.Device,
		transport: ingestTransport{
			endpoint:       strings.TrimRight(options.BaseURL, "/") + ingestPath,
			key:            options.Key,
			httpClient:     options.HTTPClient,
			attemptTimeout: attemptTimeout(options.HTTPClient),
		},
		gate:          &deliveryGate{clock: options.clock},
		clock:         options.clock,
		logger:        options.Logger.With().Str("component", "report").Logger(),
		queue:         make(chan Report, options.QueueSize),
		senderContext: senderContext,
		stopSender:    stopSender,
		done:          make(chan struct{}),
	}
	go client.run()
	return client, nil
}

func validateOptions(options Options) error {
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("report: BaseURL %q is not an absolute http(s) URL", options.BaseURL)
	}
	if !ingestKeyPattern.MatchString(options.Key) {
		// The key is a secret: the error describes the shape, never the value.
		return errors.New("report: Key is not an ingest key (expected scsk_ or scpk_, then live_ or test_, then 32 letters or digits)")
	}
	return nil
}

// ingestKeyPattern is the key format of the ingest contract (§1, v1.1).
// A key that does not match would fail every request, often inside
// net/http as an invalid header value, which the policy would retry
// forever. The Systicore type prefixes (scsk_ secret, scpk_ public)
// replaced sk_ / pk_, whose shape secret scanners take for Stripe keys.
var ingestKeyPattern = regexp.MustCompile(`^(scpk|scsk)_(live|test)_[0-9A-Za-z]{32}$`)

func withDefaults(options Options) Options {
	if options.InstanceID == "" {
		options.InstanceID = defaultInstanceID(options.Source, options.Environment)
	}
	if options.Device == (Device{}) {
		options.Device = ServerDevice(options.Release.Version, options.InstanceID)
	}
	if options.Device.InstallID == "" {
		options.Device.InstallID = options.InstanceID
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	options.HTTPClient = withoutRedirects(options.HTTPClient)
	if options.QueueSize <= 0 {
		options.QueueSize = defaultQueueSize
	}
	if options.Attempts <= 0 {
		options.Attempts = defaultAttempts
	}
	if options.Backoff <= 0 {
		options.Backoff = defaultBackoff
	}
	if options.clock == nil {
		options.clock = systemClock{}
	}
	return options
}

// defaultInstanceID is "<source>-<environment>" with empty parts left
// out, "server" when both are empty: stable across redeploys, unlike the
// container hostname.
func defaultInstanceID(source, environment string) string {
	var parts []string
	for _, part := range []string{source, environment} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "server"
	}
	return strings.Join(parts, "-")
}

func attemptTimeout(httpClient *http.Client) time.Duration {
	if httpClient.Timeout > 0 {
		return httpClient.Timeout
	}
	return defaultHTTPTimeout
}

// Enabled reports whether the client sends anything: false for a nil
// client and for one built from an empty BaseURL or Key.
func (c *Client) Enabled() bool {
	return c != nil && c.queue != nil
}

// Submit sends one report synchronously, honouring the circuit breaker,
// Retry-After and the retry policy. It returns ErrRejected (wrapped) on a
// permanent 4xx, ErrCircuitOpen while the key is known to be rejected,
// and nil on a disabled client.
func (c *Client) Submit(ctx context.Context, report Report) error {
	if !c.Enabled() {
		return nil
	}
	return c.deliver(ctx, c.prepare(ctx, report))
}
