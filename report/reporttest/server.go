// Package reporttest is a fake reports backend for tests: an httptest
// server that records every POST /api/v1/ingest and answers with scripted
// statuses. Services can use it to assert their report wiring without a
// real backend.
package reporttest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// IngestPath is the route the fake serves.
const IngestPath = "/api/v1/ingest"

// Response is one scripted answer.
type Response struct {
	Status int
	// Header is added to the answer, e.g. {"Retry-After": "2"}.
	Header map[string]string
}

// Request is one recorded ingest call.
type Request struct {
	Method string
	Path   string
	Header http.Header
	// Raw is the body as sent.
	Raw []byte
	// Body is Raw decoded into generic JSON (nil if it was not an object).
	Body map[string]any
}

// Error is the decoded "error" object of the body.
func (r Request) Error() map[string]any { return r.object("error") }

// Device is the decoded "device" object of the body.
func (r Request) Device() map[string]any { return r.object("device") }

// Context is the decoded "context" object of the body.
func (r Request) Context() map[string]any { return r.object("context") }

// User is the decoded "user" object of the body.
func (r Request) User() map[string]any { return r.object("user") }

func (r Request) object(key string) map[string]any {
	value, _ := r.Body[key].(map[string]any)
	return value
}

// Server is the fake backend. Its URL is the BaseURL for report.Options.
type Server struct {
	URL string

	mutex     sync.Mutex
	responses []Response
	requests  []Request
}

// NewServer starts a fake answering 202 to everything.
func NewServer(t testing.TB) *Server {
	return NewScriptedServer(t, Response{Status: http.StatusAccepted})
}

// NewStatusServer starts a fake answering the statuses in order; the last
// one repeats.
func NewStatusServer(t testing.TB, statuses ...int) *Server {
	responses := make([]Response, len(statuses))
	for index, status := range statuses {
		responses[index] = Response{Status: status}
	}
	return NewScriptedServer(t, responses...)
}

// NewScriptedServer starts a fake answering the responses in order; the
// last one repeats. It is closed when the test ends.
func NewScriptedServer(t testing.TB, responses ...Response) *Server {
	t.Helper()
	if len(responses) == 0 {
		responses = []Response{{Status: http.StatusAccepted}}
	}
	server := &Server{responses: responses}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(httpServer.Close)
	server.URL = httpServer.URL
	return server
}

func (s *Server) handle(writer http.ResponseWriter, request *http.Request) {
	raw, _ := io.ReadAll(request.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	s.mutex.Lock()
	index := min(len(s.requests), len(s.responses)-1)
	s.requests = append(s.requests, Request{
		Method: request.Method,
		Path:   request.URL.Path,
		Header: request.Header.Clone(),
		Raw:    raw,
		Body:   body,
	})
	response := s.responses[index]
	s.mutex.Unlock()

	for name, value := range response.Header {
		writer.Header().Set(name, value)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.Status)
	_, _ = io.WriteString(writer, `{"status":"`+statusWord(response.Status)+`"}`)
}

// statusWord mirrors the backend's {"status": …} answers (contract §3).
func statusWord(status int) string {
	switch status {
	case http.StatusAccepted:
		return "accepted"
	case http.StatusBadRequest:
		return "invalid"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	return "unavailable"
}

// Requests returns a copy of every recorded call, in arrival order.
func (s *Server) Requests() []Request {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count is the number of recorded calls.
func (s *Server) Count() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.requests)
}
