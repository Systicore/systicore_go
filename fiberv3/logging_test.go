package fiberv3

import (
	"bytes"
	"encoding/json"
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
