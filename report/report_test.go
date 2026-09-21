package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

// fakeBackend records every POST /api/v1/report and answers with the
// scripted statuses in order (last one repeats).
type fakeBackend struct {
	mu       sync.Mutex
	bodies   []map[string]any
	auth     []string
	statuses []int
	calls    int
	srv      *httptest.Server
}

func newBackend(t *testing.T, statuses ...int) *fakeBackend {
	t.Helper()
	b := &fakeBackend{statuses: statuses}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/report" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.bodies = append(b.bodies, body)
		b.auth = append(b.auth, r.Header.Get("Authorization"))
		i := b.calls
		b.calls++
		b.mu.Unlock()
		if i >= len(b.statuses) {
			i = len(b.statuses) - 1
		}
		w.WriteHeader(b.statuses[i])
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func newClient(t *testing.T, b *fakeBackend, opts ...func(*Options)) *Client {
	t.Helper()
	o := Options{BaseURL: b.srv.URL + "/", Source: "blacklab_backend", Tokens: StaticToken("svc-token"), Backoff: time.Millisecond, Attempts: 3}
	for _, f := range opts {
		f(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestSubmitSendsTheIngestContract(t *testing.T) {
	b := newBackend(t, 201)
	c := newClient(t, b)
	if err := c.Submit(context.Background(), Report{Code: "db.timeout", Message: "context deadline exceeded", Action: "GET /x"}); err != nil {
		t.Fatal(err)
	}
	body := b.bodies[0]
	if body["source"] != "blacklab_backend" || b.auth[0] != "Bearer svc-token" {
		t.Errorf("envelope wrong: source=%v auth=%q", body["source"], b.auth[0])
	}
	dev := body["device"].(map[string]any)
	host, _ := os.Hostname()
	if dev["brand"] != "server" || dev["model"] != host || dev["appVersion"] != "dev" || !strings.Contains(dev["osVersion"].(string), "go") {
		t.Errorf("device wrong: %v", dev)
	}
	e := body["error"].(map[string]any)
	if e["code"] != "db.timeout" || e["message"] != "context deadline exceeded" || e["action"] != "GET /x" || e["severity"] != "error" {
		t.Errorf("error wrong: %v (severity must default to error)", e)
	}
}

func TestSubmitRetriesTransientAndStopsOnRejected(t *testing.T) {
	b := newBackend(t, 503, 500, 200)
	if err := newClient(t, b).Submit(context.Background(), Report{Code: "x", Message: "y"}); err != nil {
		t.Fatalf("must succeed on the third attempt: %v", err)
	}
	if b.count() != 3 {
		t.Errorf("attempts = %d, want 3", b.count())
	}

	rejected := newBackend(t, 400)
	err := newClient(t, rejected).Submit(context.Background(), Report{Code: "x", Message: "y"})
	if !errors.Is(err, ErrRejected) || rejected.count() != 1 {
		t.Errorf("400 must be permanent: err=%v attempts=%d", err, rejected.count())
	}

	exhausted := newBackend(t, 502)
	if err := newClient(t, exhausted).Submit(context.Background(), Report{Code: "x", Message: "y"}); err == nil || exhausted.count() != 3 {
		t.Errorf("must give up after Attempts: err=%v attempts=%d", err, exhausted.count())
	}
}

func TestCaptureIsAsyncAndCloseFlushes(t *testing.T) {
	b := newBackend(t, 200)
	c := newClient(t, b)
	for i := 0; i < 5; i++ {
		c.Capture(context.Background(), Report{Code: "job.failed", Message: "boom"})
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.count() != 5 || c.Dropped() != 0 {
		t.Errorf("delivered=%d dropped=%d, want 5/0", b.count(), c.Dropped())
	}
	// After Close nothing is accepted, and it is a counted drop, not a panic.
	c.Capture(context.Background(), Report{Code: "late", Message: "late"})
	if c.Dropped() != 1 {
		t.Errorf("post-close capture must be dropped, dropped=%d", c.Dropped())
	}
}

func TestCaptureDropsWhenQueueIsFullAndLogsOnce(t *testing.T) {
	var logBuf bytes.Buffer
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Source: "s", QueueSize: 2, Logger: zerolog.New(&logBuf), Backoff: time.Millisecond, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 1 in flight (blocked in the handler) + 2 queued + 3 dropped.
	for i := 0; i < 6; i++ {
		c.Capture(context.Background(), Report{Code: "x", Message: "y"})
		time.Sleep(5 * time.Millisecond)
	}
	if c.Dropped() != 3 {
		t.Errorf("dropped = %d, want 3", c.Dropped())
	}
	if n := strings.Count(logBuf.String(), "error reports dropped"); n != 1 {
		t.Errorf("drop log must be rate-limited to one line, got %d: %s", n, logBuf.String())
	}
	close(blocked)
	_ = c.Close(context.Background())
}

type codedErr struct{}

func (codedErr) Error() string { return "deadlock detected" }
func (codedErr) Code() string  { return "db.deadlock" }

func TestErrorCodeAndCaptureError(t *testing.T) {
	cases := map[string]error{
		"db.deadlock":   codedErr{},
		"*fs.PathError": &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist},
		"error":         errors.New("plain"),
	}
	cases["db.deadlock (wrapped)"] = errors.Join(errors.New("outer"), codedErr{})
	if got := ErrorCode(errors.Join(errors.New("outer"), codedErr{})); got != "db.deadlock" {
		t.Errorf("wrapped Coder: got %q", got)
	}
	for want, err := range cases {
		if strings.HasSuffix(want, "(wrapped)") {
			continue
		}
		if got := ErrorCode(err); got != want {
			t.Errorf("ErrorCode(%T) = %q, want %q", err, got, want)
		}
	}
	if got := ErrorCode(fmt.Errorf("ctx: %w", fmt.Errorf("inner: %w", errors.New("x")))); got != "error" {
		t.Errorf("plain wrapped = %q", got)
	}
	if got := ErrorCode(fmt.Errorf("opening config: %w", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist})); got != "*fs.PathError" {
		t.Errorf("typed error behind fmt wrapper = %q", got)
	}

	b := newBackend(t, 200)
	c := newClient(t, b)
	ctx := logging.WithRequestID(context.Background(), "req-9")
	c.CaptureError(ctx, &fs.PathError{Op: "open", Path: "/etc/x", Err: fs.ErrNotExist}, "sitecheck.sweep", SeverityWarning)
	c.CaptureError(ctx, nil, "ignored", SeverityError)
	_ = c.Close(context.Background())
	if b.count() != 1 {
		t.Fatalf("nil error must be a no-op; sent=%d", b.count())
	}
	e := b.bodies[0]["error"].(map[string]any)
	if e["code"] != "*fs.PathError" || e["action"] != "sitecheck.sweep" || e["severity"] != "warning" {
		t.Errorf("captured error wrong: %v", e)
	}
	trace := e["trace"].(string)
	if !strings.HasPrefix(trace, "request_id: req-9\n") || !strings.Contains(trace, "report.TestErrorCodeAndCaptureError") {
		t.Errorf("trace must carry the request id and the caller frame:\n%s", trace)
	}
}

func TestHookReportsErrorLevelOnly(t *testing.T) {
	b := newBackend(t, 200)
	c := newClient(t, b)
	log := zerolog.New(&bytes.Buffer{}).Hook(c.Hook())
	log.Info().Msg("fine")
	log.Warn().Msg("meh")
	log.Error().Msg("payment failed")
	log.WithLevel(zerolog.FatalLevel).Msg("cannot open db")
	_ = c.Close(context.Background())
	if b.count() != 2 {
		t.Fatalf("hook must forward error+ only, sent=%d", b.count())
	}
	e0, e1 := b.bodies[0]["error"].(map[string]any), b.bodies[1]["error"].(map[string]any)
	if e0["code"] != "log.error" || e0["message"] != "payment failed" || e0["severity"] != "error" {
		t.Errorf("error line = %v", e0)
	}
	if e1["code"] != "log.fatal" || e1["severity"] != "critical" {
		t.Errorf("fatal line = %v", e1)
	}
}
