package report

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/report/reporttest"
)

const testKey = "scsk_test_0123456789abcdefghijklmnopqrstuv"

// fakeClock is a clock whose Sleep returns at once and advances Now, and
// records every wait so tests can assert backoff and Retry-After.
type fakeClock struct {
	mutex  sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.sleeps = append(f.sleeps, duration)
	f.now = f.now.Add(duration)
	return nil
}

func (f *fakeClock) Advance(duration time.Duration) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.now = f.now.Add(duration)
}

func (f *fakeClock) Sleeps() []time.Duration {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.sleeps)
}

func newTestClient(t *testing.T, server *reporttest.Server, customize ...func(*Options)) (*Client, *fakeClock) {
	t.Helper()
	testClock := newFakeClock()
	options := Options{
		BaseURL:     server.URL + "/",
		Key:         testKey,
		Source:      "blacklab_backend",
		Environment: "production",
		Backoff:     time.Second,
		Attempts:    3,
		clock:       testClock,
	}
	for _, apply := range customize {
		apply(&options)
	}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, testClock
}

func sortedKeys(object map[string]any) []string {
	return slices.Sorted(maps.Keys(object))
}

func assertKeys(t *testing.T, name string, object map[string]any, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := sortedKeys(object); !slices.Equal(got, want) {
		t.Errorf("%s keys = %v, want exactly %v", name, got, want)
	}
}

func TestSubmitSendsTheContractPayload(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server, func(options *Options) {
		options.Release = Release{Version: "1.4.2", Commit: "abc1234", BuildTime: "2026-09-24T10:00:00Z"}
	})
	err := client.Submit(context.Background(), Report{
		Type:        "*fs.PathError",
		Code:        "config.missing",
		Message:     "open /etc/app.yaml: no such file or directory",
		Trace:       "main.load\n\t/app/main.go:12\n",
		Action:      "GET /api/v1/orders/:id",
		Severity:    SeverityWarning,
		Fingerprint: "config.missing",
		User:        User{ID: "42", Issuer: "https://auth.systicore.hu"},
		Context:     Context{Route: "/api/v1/orders/:id", RequestID: "req-1", Tags: map[string]string{"status": "500"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	if request.Method != http.MethodPost || request.Path != reporttest.IngestPath {
		t.Errorf("sent %s %s, want POST %s", request.Method, request.Path, reporttest.IngestPath)
	}
	if request.Header.Get(KeyHeader) != testKey || request.Header.Get("Authorization") != "" {
		t.Errorf("the key must travel in %s only: key=%q authorization=%q", KeyHeader, request.Header.Get(KeyHeader), request.Header.Get("Authorization"))
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", request.Header.Get("Content-Type"))
	}

	body := request.Body
	assertKeys(t, "body", body, "error", "release", "environment", "platform", "device", "user", "context")
	assertKeys(t, "error", request.Error(), "type", "code", "message", "trace", "action", "severity", "fingerprint")
	assertKeys(t, "release", body["release"].(map[string]any), "version", "commit", "buildTime")
	assertKeys(t, "device", request.Device(), "brand", "model", "osVersion", "apiLevel", "appVersion", "installId")
	assertKeys(t, "user", request.User(), "id", "issuer")
	assertKeys(t, "context", request.Context(), "route", "requestId", "tags")

	if body["platform"] != "server" || body["environment"] != "production" {
		t.Errorf("platform=%v environment=%v", body["platform"], body["environment"])
	}
	errorObject := request.Error()
	if errorObject["type"] != "*fs.PathError" || errorObject["code"] != "config.missing" || errorObject["severity"] != "warning" ||
		errorObject["action"] != "GET /api/v1/orders/:id" || errorObject["fingerprint"] != "config.missing" {
		t.Errorf("error = %v", errorObject)
	}
	device := request.Device()
	if device["brand"] != "server" || device["model"] != runtime.GOOS+"/"+runtime.GOARCH || device["osVersion"] != runtime.Version() ||
		device["appVersion"] != "1.4.2" || device["installId"] != "blacklab_backend-production" || device["apiLevel"] != float64(0) {
		t.Errorf("device = %v", device)
	}
	if release := body["release"].(map[string]any); release["commit"] != "abc1234" || release["buildTime"] != "2026-09-24T10:00:00Z" {
		t.Errorf("release = %v", release)
	}
	if user := request.User(); user["id"] != "42" || user["issuer"] != "https://auth.systicore.hu" {
		t.Errorf("user = %v", user)
	}
	if contextObject := request.Context(); contextObject["requestId"] != "req-1" || contextObject["tags"].(map[string]any)["status"] != "500" {
		t.Errorf("context = %v", contextObject)
	}
}

func TestSubmitOmitsEmptyOptionalObjects(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server, func(options *Options) { options.Environment = "" })
	if err := client.Submit(context.Background(), Report{Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	assertKeys(t, "body", request.Body, "error", "platform", "device")
	assertKeys(t, "error", request.Error(), "message", "severity")
	if request.Error()["severity"] != "error" {
		t.Errorf("severity must default to error: %v", request.Error())
	}
	if request.Device()["appVersion"] != "dev" || request.Device()["installId"] != "blacklab_backend" {
		t.Errorf("device defaults = %v", request.Device())
	}
}

func TestInstanceIDIsStableAndOverridable(t *testing.T) {
	cases := []struct {
		name, source, environment, instanceID, want string
	}{
		{"source and environment", "blacklab_backend", "production", "", "blacklab_backend-production"},
		{"explicit INSTANCE_ID wins", "blacklab_backend", "production", "blacklab-api-2", "blacklab-api-2"},
		{"environment missing", "conduo_backend", "", "", "conduo_backend"},
		{"nothing known", "", "", "", "server"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := reporttest.NewServer(t)
			client, _ := newTestClient(t, server, func(options *Options) {
				options.Source, options.Environment, options.InstanceID = testCase.source, testCase.environment, testCase.instanceID
			})
			if err := client.Submit(context.Background(), Report{Message: "x"}); err != nil {
				t.Fatal(err)
			}
			if got := server.Requests()[0].Device()["installId"]; got != testCase.want {
				t.Errorf("installId = %v, want %q", got, testCase.want)
			}
		})
	}
}

func TestEmptyURLOrKeyGivesADisabledClient(t *testing.T) {
	server := reporttest.NewServer(t)
	for name, options := range map[string]Options{
		"no key": {BaseURL: server.URL},
		"no url": {Key: testKey},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := New(options)
			if err != nil || client == nil {
				t.Fatalf("New must not fail when reporting is off: client=%v err=%v", client, err)
			}
			exerciseDisabledClient(t, client)
		})
	}
	t.Run("nil client", func(t *testing.T) {
		exerciseDisabledClient(t, nil)
	})
	if server.Count() != 0 {
		t.Errorf("a disabled client must send nothing, sent %d", server.Count())
	}
}

func exerciseDisabledClient(t *testing.T, client *Client) {
	t.Helper()
	fallbackOutput := captureFallbackOutput(t)
	ctx := context.Background()
	if client.Enabled() {
		t.Error("client must report itself disabled")
	}
	client.Capture(ctx, Report{Message: "x"})
	client.CaptureError(ctx, errors.New("x"), "job", SeverityError)
	if err := client.Submit(ctx, Report{Message: "x"}); err != nil {
		t.Errorf("Submit on a disabled client = %v, want nil", err)
	}
	hookedLogger := zerolog.New(&bytes.Buffer{}).Hook(client.Hook())
	hookedLogger.Error().Msg("x")
	next := slog.NewTextHandler(&bytes.Buffer{}, nil)
	if client.SlogHandler(next) != slog.Handler(next) {
		t.Error("SlogHandler of a disabled client must return next unchanged")
	}
	func() {
		defer Recover(ctx, client, "job")
		panic("x")
	}()
	if !strings.Contains(fallbackOutput.String(), "recovered panic") {
		t.Error("Recover must log the panic even when reporting is off")
	}
	if client.Dropped() != 0 {
		t.Errorf("dropped = %d", client.Dropped())
	}
	if err := client.Close(ctx); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestNewRejectsMalformedConfiguration(t *testing.T) {
	for name, options := range map[string]Options{
		"url without scheme":      {BaseURL: "reports.systicore.hu", Key: testKey},
		"non-http scheme":         {BaseURL: "ftp://reports.systicore.hu", Key: testKey},
		"not an ingest key":       {BaseURL: "https://reports.systicore.hu", Key: "Bearer abc"},
		"unknown environment":     {BaseURL: "https://reports.systicore.hu", Key: "scsk_prod_0123456789abcdefghijklmnopqrstuv"},
		"random part too short":   {BaseURL: "https://reports.systicore.hu", Key: "scsk_live_0123456789"},
		"random part too long":    {BaseURL: "https://reports.systicore.hu", Key: testKey + "w"},
		"non-base62 random part":  {BaseURL: "https://reports.systicore.hu", Key: "scsk_test_0123456789abcdefghijklmnopqrst-_"},
		"carriage return inside":  {BaseURL: "https://reports.systicore.hu", Key: "scsk_test_0123456789abcdef\rghijklmnopqrstuv"},
		"type in the wrong case":  {BaseURL: "https://reports.systicore.hu", Key: "SCSK_test_0123456789abcdefghijklmnopqrstuv"},
		"unknown type":            {BaseURL: "https://reports.systicore.hu", Key: "scrk_test_0123456789abcdefghijklmnopqrstuv"},
		"missing environment tag": {BaseURL: "https://reports.systicore.hu", Key: "scsk_0123456789abcdefghijklmnopqrstuv"},
		// The pre-v1.1 types, built rather than spelled out: written as
		// literals they have the shape secret scanners flag as Stripe keys.
		"legacy secret type": {BaseURL: "https://reports.systicore.hu", Key: strings.TrimPrefix(testKey, "sc")},
		"legacy public type": {BaseURL: "https://reports.systicore.hu", Key: "pk_" + strings.TrimPrefix(testKey, "scsk_")},
	} {
		client, err := New(options)
		if err == nil || client != nil {
			t.Errorf("%s: want an error and no client, got client=%v err=%v", name, client, err)
			continue
		}
		if strings.Contains(err.Error(), options.Key) {
			t.Errorf("%s: the error must not echo the key: %v", name, err)
		}
	}
}

func TestNewAcceptsBothKeyTypesInBothEnvironments(t *testing.T) {
	const randomPart = "0123456789abcdefghijklmnopqrstuv"
	for _, key := range []string{
		"scsk_live_" + randomPart,
		"scsk_test_" + randomPart,
		"scpk_live_" + randomPart,
		"scpk_test_" + randomPart,
	} {
		client, err := New(Options{BaseURL: "https://reports.systicore.hu", Key: key})
		if err != nil {
			t.Errorf("%s: %v", key[:10], err)
			continue
		}
		if !client.Enabled() {
			t.Errorf("%s: want an enabled client", key[:10])
		}
		_ = client.Close(context.Background())
	}
}

func TestNewTrimsWhitespaceAroundTheURLAndKey(t *testing.T) {
	// A CRLF .env leaves "\r" on the value; unnoticed, it would fail every
	// request inside net/http as an invalid header value.
	server := reporttest.NewServer(t)
	client, err := New(Options{BaseURL: " " + server.URL + "\r\n", Key: testKey + "\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := client.Submit(context.Background(), Report{Message: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := server.Requests()[0].Header.Get(KeyHeader); got != testKey {
		t.Errorf("sent key %q, want the trimmed key", got)
	}

	blank, err := New(Options{BaseURL: server.URL, Key: " \r\n"})
	if err != nil || blank.Enabled() {
		t.Errorf("a key of whitespace only must mean reporting is off: enabled=%v err=%v", blank.Enabled(), err)
	}
}

func TestResponseStatusHandling(t *testing.T) {
	cases := []struct {
		name         string
		statuses     []int
		wantCalls    int
		wantSleeps   []time.Duration
		wantError    error
		wantAnyError bool
	}{
		{name: "202 accepted", statuses: []int{202}, wantCalls: 1},
		{name: "400 is permanent", statuses: []int{400}, wantCalls: 1, wantError: ErrRejected},
		{name: "413 is permanent", statuses: []int{413}, wantCalls: 1, wantError: ErrRejected},
		{name: "5xx retried with doubling backoff", statuses: []int{503, 500, 202}, wantCalls: 3, wantSleeps: []time.Duration{time.Second, 2 * time.Second}},
		{name: "5xx gives up after Attempts", statuses: []int{502}, wantCalls: 3, wantSleeps: []time.Duration{time.Second, 2 * time.Second}, wantAnyError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := reporttest.NewStatusServer(t, testCase.statuses...)
			client, testClock := newTestClient(t, server)
			err := client.Submit(context.Background(), Report{Message: "x"})
			switch {
			case testCase.wantError != nil && !errors.Is(err, testCase.wantError):
				t.Errorf("err = %v, want %v", err, testCase.wantError)
			case testCase.wantError == nil && !testCase.wantAnyError && err != nil:
				t.Errorf("err = %v, want nil", err)
			case testCase.wantAnyError && (err == nil || errors.Is(err, ErrRejected)):
				t.Errorf("err = %v, want a transient failure", err)
			}
			if server.Count() != testCase.wantCalls {
				t.Errorf("calls = %d, want %d", server.Count(), testCase.wantCalls)
			}
			if sleeps := testClock.Sleeps(); !slices.Equal(sleeps, testCase.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", sleeps, testCase.wantSleeps)
			}
		})
	}
}

func TestRedirectsAreNeverFollowedSoTheKeyCannotLeak(t *testing.T) {
	for name, httpClient := range map[string]*http.Client{
		"default client":         nil,
		"caller-supplied client": {Timeout: 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			var targetCalls, redirectorCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				targetCalls.Add(1)
				writer.WriteHeader(http.StatusAccepted)
			}))
			defer target.Close()
			redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				redirectorCalls.Add(1)
				http.Redirect(writer, request, target.URL+request.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer redirector.Close()

			testClock := newFakeClock()
			client, err := New(Options{BaseURL: redirector.URL, Key: testKey, HTTPClient: httpClient, clock: testClock})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()

			err = client.Submit(context.Background(), Report{Message: "x"})
			if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "redirect") {
				t.Errorf("a redirect must be a permanent rejection that names the cause, got %v", err)
			}
			if calls := targetCalls.Load(); calls != 0 {
				t.Errorf("the key must never reach the redirect target, it was called %d times", calls)
			}
			if calls := redirectorCalls.Load(); calls != 1 || len(testClock.Sleeps()) != 0 {
				t.Errorf("a redirect must not be retried: calls=%d sleeps=%v", calls, testClock.Sleeps())
			}
			if httpClient != nil && httpClient.CheckRedirect != nil {
				t.Error("the caller's client must not be modified")
			}
		})
	}
}

func TestNetworkFailureIsRetried(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	testClock := newFakeClock()
	client, err := New(Options{BaseURL: unreachable.URL, Key: testKey, Attempts: 2, Backoff: time.Second, clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := client.Submit(context.Background(), Report{Message: "x"}); err == nil || errors.Is(err, ErrRejected) {
		t.Errorf("a network failure must be a transient error, got %v", err)
	}
	if sleeps := testClock.Sleeps(); !slices.Equal(sleeps, []time.Duration{time.Second}) {
		t.Errorf("sleeps = %v, want one backoff", sleeps)
	}
}

func TestUnauthorizedOpensTheCircuitForFiveMinutes(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var logBuffer bytes.Buffer
			server := reporttest.NewStatusServer(t, status, http.StatusAccepted)
			client, testClock := newTestClient(t, server, func(options *Options) { options.Logger = zerolog.New(&logBuffer) })
			ctx := context.Background()

			err := client.Submit(ctx, Report{Message: "x"})
			if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrUnauthorized) || server.Count() != 1 {
				t.Fatalf("a %d must be permanent and not retried: err=%v calls=%d", status, err, server.Count())
			}
			if err := client.Submit(ctx, Report{Message: "x"}); !errors.Is(err, ErrCircuitOpen) || server.Count() != 1 {
				t.Errorf("the circuit must be open: err=%v calls=%d", err, server.Count())
			}
			testClock.Advance(circuitCooldown - time.Second)
			if err := client.Submit(ctx, Report{Message: "x"}); !errors.Is(err, ErrCircuitOpen) {
				t.Errorf("still inside the 5 minutes: err=%v", err)
			}
			testClock.Advance(2 * time.Second)
			if err := client.Submit(ctx, Report{Message: "x"}); err != nil || server.Count() != 2 {
				t.Errorf("after 5 minutes the client must try again: err=%v calls=%d", err, server.Count())
			}
			if lines := strings.Count(logBuffer.String(), "rejected the ingest key"); lines != 1 {
				t.Errorf("the circuit opening must be logged once, got %d:\n%s", lines, logBuffer.String())
			}
		})
	}
}

func TestCaptureDropsWhileTheCircuitIsOpen(t *testing.T) {
	server := reporttest.NewStatusServer(t, http.StatusUnauthorized)
	client, _ := newTestClient(t, server)
	for range 4 {
		client.Capture(context.Background(), Report{Message: "x"})
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.Count() != 1 || client.Dropped() != 4 {
		t.Errorf("one probe, then drops: calls=%d dropped=%d", server.Count(), client.Dropped())
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter map[string]string
		wantWait   time.Duration
	}{
		{"seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"missing header", nil, defaultRetryAfter},
		{"capped", map[string]string{"Retry-After": "3600"}, maxRetryAfter},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := reporttest.NewScriptedServer(t,
				reporttest.Response{Status: http.StatusTooManyRequests, Header: testCase.retryAfter},
				reporttest.Response{Status: http.StatusAccepted},
			)
			client, testClock := newTestClient(t, server)
			if err := client.Submit(context.Background(), Report{Message: "x"}); err != nil {
				t.Fatalf("must succeed after waiting: %v", err)
			}
			if server.Count() != 2 {
				t.Errorf("calls = %d, want 2", server.Count())
			}
			if sleeps := testClock.Sleeps(); !slices.Equal(sleeps, []time.Duration{testCase.wantWait}) {
				t.Errorf("waits = %v, want [%s]", sleeps, testCase.wantWait)
			}
		})
	}
	t.Run("exhausted", func(t *testing.T) {
		server := reporttest.NewScriptedServer(t, reporttest.Response{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "1"}})
		client, _ := newTestClient(t, server)
		if err := client.Submit(context.Background(), Report{Message: "x"}); !errors.Is(err, ErrRateLimited) || server.Count() != 3 {
			t.Errorf("err=%v calls=%d, want ErrRateLimited after 3 attempts", err, server.Count())
		}
	})
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              defaultRetryAfter,
		"garbage":                       defaultRetryAfter,
		"0":                             0,
		"-5":                            0,
		"30":                            30 * time.Second,
		"999999":                        maxRetryAfter,
		"Thu, 24 Sep 2026 10:00:45 GMT": 45 * time.Second,
	}
	for value, want := range cases {
		if got := parseRetryAfter(value, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestCaptureNeverBlocksTheCallerAndCloseIsBounded(t *testing.T) {
	// A real clock and a 5-minute Retry-After: the sender sits in the
	// pause, the queue fills, Capture keeps returning at once.
	server := reporttest.NewScriptedServer(t, reporttest.Response{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "300"}})
	client, err := New(Options{BaseURL: server.URL, Key: testKey, QueueSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for range 50 {
		client.Capture(context.Background(), Report{Message: "x"})
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Capture blocked for %s", elapsed)
	}
	closeContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started = time.Now()
	if err := client.Close(closeContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want the deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Close must honour its deadline, took %s", elapsed)
	}
	if client.Dropped() == 0 {
		t.Error("a full queue must count drops")
	}
}

func TestCaptureIsAsyncAndCloseFlushes(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	for range 5 {
		client.Capture(context.Background(), Report{Code: "job.failed", Message: "boom"})
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.Count() != 5 || client.Dropped() != 0 {
		t.Errorf("delivered=%d dropped=%d, want 5/0", server.Count(), client.Dropped())
	}
	// After Close nothing is accepted, and it is a counted drop, not a panic.
	client.Capture(context.Background(), Report{Code: "late", Message: "late"})
	if client.Dropped() != 1 {
		t.Errorf("post-close capture must be dropped, dropped=%d", client.Dropped())
	}
	if err := client.Close(context.Background()); err != nil {
		t.Errorf("a second Close = %v", err)
	}
}

func TestCaptureDetachesStringsFromTheCallersBuffers(t *testing.T) {
	// Fiber hands out strings that alias request buffers; the queued
	// report must not change when the next request reuses them.
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	buffer := []byte("GET /orders/:id")
	aliased := unsafe.String(&buffer[0], len(buffer))
	client.Capture(context.Background(), Report{
		Message: aliased,
		Action:  aliased,
		Context: Context{RequestID: aliased, Tags: map[string]string{aliased: aliased}},
	})
	copy(buffer, "POST /users/:x!")
	_ = client.Close(context.Background())

	request := server.Requests()[0]
	if request.Error()["message"] != "GET /orders/:id" || request.Error()["action"] != "GET /orders/:id" ||
		request.Context()["requestId"] != "GET /orders/:id" || request.Context()["tags"].(map[string]any)["GET /orders/:id"] != "GET /orders/:id" {
		t.Errorf("the queued report changed with the caller's buffer: %v / %v", request.Error(), request.Context())
	}
}

func TestCaptureDropsWhenQueueIsFullAndLogsOnce(t *testing.T) {
	var logBuffer bytes.Buffer
	blocked := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-blocked
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()
	client, err := New(Options{BaseURL: backend.URL, Key: testKey, QueueSize: 2, Logger: zerolog.New(&logBuffer), Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 1 in flight (blocked in the handler) + 2 queued + 3 dropped.
	for range 6 {
		client.Capture(context.Background(), Report{Message: "x"})
		time.Sleep(5 * time.Millisecond)
	}
	if client.Dropped() != 3 {
		t.Errorf("dropped = %d, want 3", client.Dropped())
	}
	if lines := strings.Count(logBuffer.String(), "error reports dropped"); lines != 1 {
		t.Errorf("drop log must be rate-limited to one line, got %d: %s", lines, logBuffer.String())
	}
	close(blocked)
	_ = client.Close(context.Background())
}
