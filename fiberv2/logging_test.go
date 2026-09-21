package fiberv2

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

func TestLoggingUsesUserContext(t *testing.T) {
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(Logging(zerolog.New(&buf)))
	app.Get("/x", func(c *fiber.Ctx) error {
		zerolog.Ctx(c.UserContext()).Info().Msg("from handler")
		if logging.RequestID(c.UserContext()) == "" {
			t.Error("request id must be in the user context")
		}
		// The fasthttp ctx is NOT where the logger lives — documented caveat.
		if zerolog.Ctx(c.Context()) != zerolog.DefaultContextLogger && zerolog.Ctx(c.Context()).GetLevel() != zerolog.Disabled {
			t.Error("c.Context() must not resolve the request logger")
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get(logging.RequestIDKey) == "" {
		t.Error("a request id must be minted and echoed")
	}
	var got []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not JSON: %q", ln)
		}
		got = append(got, m)
	}
	if len(got) != 2 || got[0]["message"] != "from handler" || got[0]["path"] != "/x" || got[1]["status"] != float64(204) {
		t.Errorf("lines = %v", got)
	}
}
