package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaxBody(t *testing.T) {
	var decodeErr error
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]string
		decodeErr = DecodeJSON(r, &v)
		w.WriteHeader(http.StatusNoContent)
	})

	// A declared length over the cap never reaches the handler.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"0123456789"}`))
	MaxBody(8, nil)(echo).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != `{"error":"Request Entity Too Large"}` {
		t.Errorf("declared too large: %d %s", rec.Code, rec.Body.String())
	}

	// A service's own 413 body.
	rec = httptest.NewRecorder()
	custom := func(w http.ResponseWriter, _ *http.Request) { Error(w, http.StatusRequestEntityTooLarge, "too big") }
	MaxBody(8, custom)(echo).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"0123456789"}`)))
	if rec.Body.String() != `{"status":"error","message":"too big"}` {
		t.Errorf("custom reject: %s", rec.Body.String())
	}

	// No declared length (chunked): MaxBytesReader cuts it, DecodeJSON says so.
	req = httptest.NewRequest(http.MethodPost, "/", io.NopCloser(strings.NewReader(`{"a":"0123456789"}`)))
	req.ContentLength = -1
	MaxBody(8, nil)(echo).ServeHTTP(httptest.NewRecorder(), req)
	if !errors.Is(decodeErr, ErrBodyTooLarge) {
		t.Errorf("chunked over the cap: DecodeJSON = %v", decodeErr)
	}

	// Under the cap it is untouched.
	MaxBody(64, nil)(echo).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"b"}`)))
	if decodeErr != nil {
		t.Errorf("under the cap: DecodeJSON = %v", decodeErr)
	}
}

func TestSkipPathsAndUnless(t *testing.T) {
	marking := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Marked", "1")
			next.ServeHTTP(w, r)
		})
	}
	marked := func(mw Middleware, path string) bool {
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("X-Marked") == "1"
	}

	skip := SkipPaths(marking, "/health", "/api/v1/ingest/")
	for path, want := range map[string]bool{
		"/health":         false,
		"/health/":        false, // trailing slash ignored on the request side
		"/api/v1/ingest":  false, // ...and on the listed side
		"/api/v1/ingest/": false,
		"/healthz":        true, // exact paths only
		"/api/v1/report":  true,
	} {
		if got := marked(skip, path); got != want {
			t.Errorf("SkipPaths %s: marked = %v, want %v", path, got, want)
		}
	}

	swagger := Unless(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/swagger") }, marking)
	if marked(swagger, "/swagger/openapi.yaml") || !marked(swagger, "/api/v1/user") {
		t.Error("Unless must skip exactly the requests the predicate picks")
	}
}
