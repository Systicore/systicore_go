package fiberv3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

func TestLoggingScopesContextLoggerAndEchoesRequestID(t *testing.T) {
	var buf bytes.Buffer
	root := zerolog.New(&buf)

	app := fiber.New()
	app.Use(Logging(root))
	app.Get("/reports/:id", func(c fiber.Ctx) error {
		// Domain code sees only context.Context.
		zerolog.Ctx(c.Context()).Warn().Msg("from handler")
		if logging.RequestID(c.Context()) != "req-f3" {
			t.Errorf("request id in ctx = %q", logging.RequestID(c.Context()))
		}
		return c.Status(fiber.StatusCreated).SendString("ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/reports/7", nil)
	req.Header.Set(logging.RequestIDKey, "req-f3")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get(logging.RequestIDKey) != "req-f3" {
		t.Error("request id must be echoed in the response")
	}

	var got []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not JSON: %q", ln)
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("want handler + access line, got %d: %s", len(got), buf.String())
	}
	if got[0]["message"] != "from handler" || got[0]["request_id"] != "req-f3" || got[0]["method"] != "GET" || got[0]["path"] != "/reports/7" {
		t.Errorf("handler line = %v", got[0])
	}
	if got[1]["message"] != "request" || got[1]["status"] != float64(201) || got[1]["bytes"] != float64(2) {
		t.Errorf("access line = %v", got[1])
	}
}

func TestLoggingWritesSuccessfulProbesAtDebug(t *testing.T) {
	cases := []struct {
		path   string
		status int
		want   string
	}{
		{"/health", fiber.StatusOK, "debug"},
		{"/health/ready", fiber.StatusServiceUnavailable, "info"},
		{"/reports", fiber.StatusOK, "info"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		app := fiber.New()
		app.Use(Logging(zerolog.New(&buf)))
		app.Get(tc.path, func(c fiber.Ctx) error { return c.SendStatus(tc.status) })

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		var line map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
			t.Fatalf("%s: want exactly one JSON access line, got %q", tc.path, buf.String())
		}
		if line["message"] != "request" || line["level"] != tc.want || line["status"] != float64(tc.status) {
			t.Errorf("%s %d: access line = %v, want level %s", tc.path, tc.status, line, tc.want)
		}
	}
}

func TestLoggingRecordsTheStatusOfAReturnedError(t *testing.T) {
	cases := []struct {
		name, path string
		handler    fiber.Handler
		want       int
	}{
		{"fiber error", "/unavailable", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusServiceUnavailable, "down") }, fiber.StatusServiceUnavailable},
		{"wrapped fiber error", "/gone", func(fiber.Ctx) error { return fmt.Errorf("lookup: %w", fiber.ErrNotFound) }, fiber.StatusNotFound},
		{"plain error", "/boom", func(fiber.Ctx) error { return errors.New("boom") }, fiber.StatusInternalServerError},
		{"unmatched route", "/nowhere", nil, fiber.StatusNotFound},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		app := fiber.New()
		app.Use(Logging(zerolog.New(&buf)))
		if tc.handler != nil {
			app.Get(tc.path, tc.handler)
		}

		resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: response %d, want %d", tc.name, resp.StatusCode, tc.want)
		}

		var line map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
			t.Fatalf("%s: want exactly one JSON access line, got %q", tc.name, buf.String())
		}
		if line["status"] != float64(tc.want) {
			t.Errorf("%s: access line status = %v, want %d (what the client got)", tc.name, line["status"], tc.want)
		}
		if line["error"] == nil {
			t.Errorf("%s: access line carries no error: %v", tc.name, line)
		}
		if _, counted := line["bytes"]; counted {
			t.Errorf("%s: bytes logged before the error handler wrote the body: %v", tc.name, line)
		}
	}
}
