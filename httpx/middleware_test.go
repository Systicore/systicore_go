package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

func TestRecoverAnswers500AndLogsWithRequestFields(t *testing.T) {
	for _, stack := range []bool{false, true} {
		var buf bytes.Buffer
		var reported any
		h := logging.Middleware(zerolog.New(&buf))(Recover(RecoverOptions{
			Stack:   stack,
			OnPanic: func(_ *http.Request, value any, trace []byte) { reported = value; _ = trace },
		})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/7", nil))

		if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"Internal Server Error"}` {
			t.Errorf("stack=%v: answer %d %s", stack, rec.Code, rec.Body.String())
		}
		if reported != "boom" {
			t.Errorf("stack=%v: OnPanic got %v", stack, reported)
		}
		var panicLine map[string]any
		if err := json.Unmarshal([]byte(strings.SplitN(buf.String(), "\n", 2)[0]), &panicLine); err != nil {
			t.Fatalf("first log line is not JSON: %q", buf.String())
		}
		if panicLine["message"] != "request error" || panicLine["panic"] != "boom" || panicLine["path"] != "/reports/7" || panicLine["request_id"] == nil {
			t.Errorf("stack=%v: panic line = %v", stack, panicLine)
		}
		if _, has := panicLine["stack"]; has != stack {
			t.Errorf("stack=%v: stack field present = %v", stack, has)
		}
	}
}

func TestRecoverKeepsAnAnswerAlreadyStarted(t *testing.T) {
	h := Recover(RecoverOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("late")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Errorf("got %d %q: no second answer may be written", rec.Code, rec.Body.String())
	}
}

func TestRecoverRepanicsErrAbortHandler(t *testing.T) {
	defer func() {
		if p := recover(); p != http.ErrAbortHandler { //nolint:errorlint // a recovered value
			t.Errorf("recovered %v, want http.ErrAbortHandler to reach the server", p)
		}
	}()
	Recover(RecoverOptions{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestSecurityHeaders(t *testing.T) {
	serve := func(o SecurityHeadersOptions) http.Header {
		rec := httptest.NewRecorder()
		SecurityHeaders(o)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header()
	}

	// The zero value is Fiber helmet's ConfigDefault (fiber v3.5.0).
	defaults := map[string]string{
		"X-XSS-Protection":                  "0",
		"X-Content-Type-Options":            "nosniff",
		"X-Frame-Options":                   "SAMEORIGIN",
		"Referrer-Policy":                   "no-referrer",
		"Cross-Origin-Embedder-Policy":      "require-corp",
		"Cross-Origin-Opener-Policy":        "same-origin",
		"Cross-Origin-Resource-Policy":      "same-origin",
		"Origin-Agent-Cluster":              "?1",
		"X-DNS-Prefetch-Control":            "off",
		"X-Download-Options":                "noopen",
		"X-Permitted-Cross-Domain-Policies": "none",
	}
	got := serve(SecurityHeadersOptions{})
	for name, want := range defaults {
		if got.Get(name) != want {
			t.Errorf("default %s = %q, want %q", name, got.Get(name), want)
		}
	}
	for _, absent := range []string{"Content-Security-Policy", "Strict-Transport-Security"} {
		if got.Get(absent) != "" {
			t.Errorf("%s must not be sent by default", absent)
		}
	}

	// The authenticator's settings.
	got = serve(SecurityHeadersOptions{
		CrossOriginResourcePolicy: "cross-origin",
		FrameOptions:              "DENY",
		ContentSecurityPolicy:     "default-src 'none'; frame-ancestors 'none'",
		HSTSMaxAge:                31536000,
		HSTSPreload:               true,
	})
	for name, want := range map[string]string{
		"Cross-Origin-Resource-Policy": "cross-origin",
		"X-Frame-Options":              "DENY",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains; preload",
	} {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want)
		}
	}
	if hsts := serve(SecurityHeadersOptions{HSTSMaxAge: 60}).Get("Strict-Transport-Security"); hsts != "max-age=60; includeSubDomains" {
		t.Errorf("HSTS without preload = %q", hsts)
	}
}
