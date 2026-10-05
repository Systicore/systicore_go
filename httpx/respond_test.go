package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONWritesCompactBody(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]int{"id": 7})
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":7}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("got %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestJSONUnmarshalableValueIs500(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusOK, func() {})
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"Internal Server Error"}` {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestErrorBodies(t *testing.T) {
	cases := []struct {
		name  string
		write func(http.ResponseWriter)
		code  int
		body  string
	}{
		// Field order of the services' common.IResponse.
		{"envelope", func(w http.ResponseWriter) { Error(w, http.StatusBadRequest, "invalid body") }, 400, `{"status":"error","message":"invalid body"}`},
		{"status error", func(w http.ResponseWriter) { StatusError(w, http.StatusRequestEntityTooLarge) }, 413, `{"error":"Request Entity Too Large"}`},
		{"method not allowed", func(w http.ResponseWriter) {
			MethodNotAllowed(w, httptest.NewRequest(http.MethodPut, "/x", nil))
		}, 405, `{"error":"Method Not Allowed"}`},
		{"not found, as Fiber said it", func(w http.ResponseWriter) {
			NotFound(w, httptest.NewRequest(http.MethodDelete, "/api/v1/nope", nil))
		}, 404, `{"error":"Cannot DELETE /api/v1/nope"}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		tc.write(rec)
		if rec.Code != tc.code || rec.Body.String() != tc.body {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct{ Name string }
	// Unknown fields and a missing Content-Type are fine, as with Fiber.
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"a","extra":1}`))
	if err := DecodeJSON(r, &v); err != nil || v.Name != "a" {
		t.Fatalf("DecodeJSON = %v, %+v", err, v)
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":`))
	if err := DecodeJSON(r, &v); err == nil || errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("malformed body: err = %v, want a syntax error", err)
	}

	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+strings.Repeat("x", 64)+`"}`))
	r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 16)
	if err := DecodeJSON(r, &v); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("capped body: err = %v, want ErrBodyTooLarge", err)
	}
}
