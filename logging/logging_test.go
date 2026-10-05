package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// rootTo builds a root logger that writes JSON lines into buf.
func rootTo(buf *bytes.Buffer) zerolog.Logger {
	return zerolog.New(buf).With().Str("service", "test").Logger()
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not JSON: %q", ln)
		}
		out = append(out, m)
	}
	return out
}

func TestNewWritesFileAndFallsBackOnBadLevel(t *testing.T) {
	dir := t.TempDir()
	root, closeFn, err := New(Options{Service: "svc", Level: "nope", File: &FileOptions{Dir: dir, MaxSizeMB: 1}})
	if err != nil {
		t.Fatal(err)
	}
	root.Info().Msg("hello")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "svc-app.log"))
	if len(matches) != 1 {
		t.Fatalf("log file not created in %s", dir)
	}
	if root.GetLevel() != zerolog.InfoLevel {
		t.Errorf("level = %s, want info fallback", root.GetLevel())
	}
	// Outside any request, zerolog.Ctx yields the root logger, not a Nop.
	if zerolog.Ctx(context.Background()).GetLevel() == zerolog.Disabled {
		t.Error("DefaultContextLogger must be the root logger")
	}
}

func TestMiddlewareScopesLoggerAndLogsAccessLine(t *testing.T) {
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /reports/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Domain code: only zerolog.Ctx, nothing from this package.
		zerolog.Ctx(r.Context()).Warn().Msg("from handler")
		Enrich(r.Context(), func(c zerolog.Context) zerolog.Context { return c.Int64("user_id", 42) })
		zerolog.Ctx(r.Context()).Info().Msg("after enrich")
		if RequestID(r.Context()) == "" {
			t.Error("request id must be in the context")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})
	handler := Middleware(rootTo(&buf))(mux)

	req := httptest.NewRequest(http.MethodGet, "/reports/7", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set(RequestIDKey, "req-abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get(RequestIDKey) != "req-abc" {
		t.Error("the request id must be echoed in the response header")
	}

	got := lines(t, &buf)
	if len(got) != 3 {
		t.Fatalf("want 3 lines (handler, enriched, access), got %d: %s", len(got), buf.String())
	}
	handlerLine, enriched, access := got[0], got[1], got[2]
	if handlerLine["message"] != "from handler" || handlerLine["method"] != "GET" || handlerLine["path"] != "/reports/7" || handlerLine["ip"] != "10.0.0.9:1234" || handlerLine["request_id"] != "req-abc" {
		t.Errorf("handler line lacks request fields: %v", handlerLine)
	}
	if _, present := handlerLine["user_id"]; present {
		t.Error("user_id must not exist before Enrich")
	}
	if enriched["user_id"] != float64(42) {
		t.Errorf("Enrich must add user_id in place: %v", enriched)
	}
	if access["message"] != "request" || access["status"] != float64(201) || access["bytes"] != float64(2) || access["service"] != "test" {
		t.Errorf("access line wrong: %v", access)
	}
	if access["request_id"] != handlerLine["request_id"] {
		t.Error("all lines of one request share the request id")
	}
}

func TestMiddlewareMintsRequestIDAndKeepsDefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	handler := Middleware(rootTo(&buf))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("implicit 200"))
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get(RequestIDKey) == "" {
		t.Error("a request id must be minted when the caller sends none")
	}
	got := lines(t, &buf)
	if len(got) != 1 || got[0]["status"] != float64(200) || got[0]["bytes"] != float64(12) {
		t.Errorf("access line = %v", got)
	}
}

func TestAccessLevel(t *testing.T) {
	cases := []struct {
		path   string
		status int
		want   zerolog.Level
	}{
		{"/health", http.StatusOK, zerolog.DebugLevel},
		{"/health/live", http.StatusOK, zerolog.DebugLevel},
		{"/health/ready", http.StatusOK, zerolog.DebugLevel},
		{"/healthz", http.StatusNoContent, zerolog.DebugLevel},
		{"/ready", http.StatusOK, zerolog.DebugLevel},
		{"/metrics", http.StatusOK, zerolog.DebugLevel},
		// A failing probe is the line someone will look for.
		{"/health/ready", http.StatusServiceUnavailable, zerolog.InfoLevel},
		{"/metrics", http.StatusForbidden, zerolog.InfoLevel},
		// Exact paths only: nothing that merely starts like a probe.
		{"/healthcheck", http.StatusOK, zerolog.InfoLevel},
		{"/health/", http.StatusOK, zerolog.InfoLevel},
		{"/api/v1/report", http.StatusOK, zerolog.InfoLevel},
	}
	for _, tc := range cases {
		if got := AccessLevel(tc.path, tc.status); got != tc.want {
			t.Errorf("AccessLevel(%q, %d) = %s, want %s", tc.path, tc.status, got, tc.want)
		}
	}
}

func TestMiddlewareLogsSuccessfulProbesAtDebug(t *testing.T) {
	cases := []struct {
		path   string
		status int
		want   string
	}{
		{"/health", http.StatusOK, "debug"},
		{"/health/ready", http.StatusServiceUnavailable, "info"},
		{"/reports/7", http.StatusOK, "info"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		handler := Middleware(rootTo(&buf))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))
		got := lines(t, &buf)
		if len(got) != 1 || got[0]["message"] != "request" || got[0]["level"] != tc.want {
			t.Errorf("%s %d: access line = %v, want level %s", tc.path, tc.status, got, tc.want)
		}
	}

	// A root logger at info drops the successful probe's line entirely.
	var buf bytes.Buffer
	quiet := Middleware(rootTo(&buf).Level(zerolog.InfoLevel))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	quiet.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if buf.Len() != 0 {
		t.Errorf("an info-level root must not log a successful probe: %s", buf.String())
	}
}
