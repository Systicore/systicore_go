package fiberv2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report"
	"github.com/Systicore/systicore_go/report/reporttest"
)

func newReportClient(t *testing.T, server *reporttest.Server) *report.Client {
	t.Helper()
	client, err := report.New(report.Options{
		BaseURL:     server.URL,
		Key:         "scsk_test_0123456789abcdefghijklmnopqrstuv",
		Source:      "spike_backend",
		Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func currentUser(c *fiber.Ctx) report.User {
	userID, _ := c.Locals("user_id").(string)
	return report.User{ID: userID}
}

// newReportingApp is the wiring the README documents: the wrapped error
// handler, recover with the reporter's config, then the logging adapter.
func newReportingApp(reporter *Reporter, hostErrorHandler fiber.ErrorHandler) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(hostErrorHandler)})
	app.Use(fiberrecover.New(reporter.RecoverConfig()))
	app.Use(Logging(zerolog.Nop()))
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("user_id", "42")
		return c.Next()
	})
	return app
}

func send(t *testing.T, app *fiber.App, method, target string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set(logging.RequestIDKey, "req-f2")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func explodeMatch() {
	panic("match state corrupted")
}

func TestRecoverConfigReportsPanicsWithThePanicSite(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newReportingApp(NewReporter(client, ReportOptions{User: currentUser}), nil)
	app.Get("/matches/:id", func(*fiber.Ctx) error {
		explodeMatch()
		return nil
	})

	response := send(t, app, http.MethodGet, "/matches/77")
	_ = client.Close(context.Background())

	if response.StatusCode != fiber.StatusInternalServerError {
		t.Errorf("status = %d, want 500", response.StatusCode)
	}
	if server.Count() != 1 {
		t.Fatalf("reports = %d, want exactly one (the ErrorHandler must not repeat the panic)", server.Count())
	}
	request := server.Requests()[0]
	errorObject := request.Error()
	if errorObject["type"] != "panic" || errorObject["code"] != "panic" || errorObject["severity"] != "critical" ||
		errorObject["message"] != "match state corrupted" || errorObject["action"] != "GET /matches/:id" {
		t.Errorf("panic report = %v", errorObject)
	}
	traceLines := strings.Split(errorObject["trace"].(string), "\n")
	if len(traceLines) < 2 || !strings.Contains(traceLines[1], "fiberv2.explodeMatch") {
		t.Errorf("the trace must start at the panicking function:\n%s", errorObject["trace"])
	}
	contextObject := request.Context()
	if contextObject["route"] != "/matches/:id" || contextObject["requestId"] != "req-f2" ||
		contextObject["tags"].(map[string]any)["status"] != "500" {
		t.Errorf("context = %v", contextObject)
	}
	if request.User()["id"] != "42" {
		t.Errorf("user = %v", request.User())
	}
}

func TestErrorHandlerReportsServerErrorsOnly(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newReportingApp(NewReporter(client, ReportOptions{User: currentUser}), nil)
	app.Get("/shards/:id/sync", func(*fiber.Ctx) error { return errors.New("dial tcp 10.0.0.5:3306: connection refused") })
	app.Get("/maintenance", func(*fiber.Ctx) error { return fiber.NewError(fiber.StatusServiceUnavailable, "maintenance") })
	app.Get("/bad-request", func(*fiber.Ctx) error { return fiber.NewError(fiber.StatusBadRequest, "bad") })

	for _, call := range []struct {
		target     string
		wantStatus int
	}{
		{"/shards/7/sync", 500},
		{"/maintenance", 503},
		{"/bad-request", 400},
		{"/nowhere", 404},
	} {
		if response := send(t, app, http.MethodGet, call.target); response.StatusCode != call.wantStatus {
			t.Errorf("GET %s answered %d, want %d", call.target, response.StatusCode, call.wantStatus)
		}
	}
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("reports = %d, want the two 5xx only", len(requests))
	}
	database, maintenance := requests[0].Error(), requests[1].Error()
	if database["type"] != "error" || database["action"] != "GET /shards/:id/sync" || database["severity"] != "error" {
		t.Errorf("500 report = %v", database)
	}
	if _, hasTrace := database["trace"]; hasTrace {
		t.Error("an error-handler report must carry no trace, or every route would group together")
	}
	if requests[0].User()["id"] != "42" || requests[0].Context()["requestId"] != "req-f2" {
		t.Errorf("user/context = %v / %v", requests[0].User(), requests[0].Context())
	}
	if maintenance["type"] != "*fiber.Error" || requests[1].Context()["tags"].(map[string]any)["status"] != "503" {
		t.Errorf("503 report = %v / %v", maintenance, requests[1].Context())
	}
}

func TestErrorHandlerReportsAFailingHostHandlerAs500(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	failingHandler := func(*fiber.Ctx, error) error { return errors.New("encoding the error body failed") }
	app := newReportingApp(NewReporter(client, ReportOptions{}), failingHandler)
	app.Get("/x", func(*fiber.Ctx) error { return errors.New("original failure") })

	if response := send(t, app, http.MethodGet, "/x"); response.StatusCode != fiber.StatusInternalServerError {
		t.Errorf("status = %d", response.StatusCode)
	}
	_ = client.Close(context.Background())
	if server.Count() != 1 || server.Requests()[0].Error()["message"] != "original failure" {
		t.Errorf("the original error must be reported once: %v", server.Requests())
	}
	if tags := server.Requests()[0].Context()["tags"].(map[string]any); tags["status"] != "500" {
		t.Errorf("a failing host handler means Fiber answers 500: tags=%v", tags)
	}
}

func TestErrorHandlerKeepsTheHostLogLineOutOfTheHook(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	var logBuffer bytes.Buffer
	hookedLogger := zerolog.New(&logBuffer).Hook(client.Hook())
	hostErrorHandler := func(c *fiber.Ctx, err error) error {
		hookedLogger.Error().Ctx(c.UserContext()).Err(err).Msg("request error")
		return fiber.DefaultErrorHandler(c, err)
	}
	app := newReportingApp(NewReporter(client, ReportOptions{}), hostErrorHandler)
	app.Get("/boom", func(*fiber.Ctx) error { return errors.New("boom") })
	app.Get("/missing", func(*fiber.Ctx) error { return fiber.ErrNotFound })

	send(t, app, http.MethodGet, "/boom")
	send(t, app, http.MethodGet, "/missing")
	_ = client.Close(context.Background())

	if strings.Count(logBuffer.String(), "request error") != 2 {
		t.Errorf("the host must still log both errors:\n%s", logBuffer.String())
	}
	if server.Count() != 1 || server.Requests()[0].Error()["action"] != "GET /boom" {
		t.Errorf("one report (the 500 from the adapter), none from the hook: %d", server.Count())
	}
}

func TestExplicitCaptureInAScopedHandlerIsNotRepeated(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newReportingApp(NewReporter(client, ReportOptions{}), nil)
	app.Use(func(c *fiber.Ctx) error {
		c.SetUserContext(report.WithScope(c.UserContext()))
		return c.Next()
	})
	app.Get("/import", func(c *fiber.Ctx) error {
		err := errors.New("import failed")
		client.CaptureError(c.UserContext(), err, "import.run", report.SeverityCritical)
		return err
	})

	send(t, app, http.MethodGet, "/import")
	_ = client.Close(context.Background())
	if server.Count() != 1 || server.Requests()[0].Error()["action"] != "import.run" {
		t.Errorf("the handler's own capture must be the only report: %d", server.Count())
	}
}

func TestMarkReportedInAScopedHandlerDoesNotHideTheServerError(t *testing.T) {
	// The handler only silences its own log line; nothing was captured, so
	// the 500 must still be reported by the adapter.
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newReportingApp(NewReporter(client, ReportOptions{}), nil)
	app.Use(func(c *fiber.Ctx) error {
		c.SetUserContext(report.WithScope(c.UserContext()))
		return c.Next()
	})
	app.Get("/sync", func(c *fiber.Ctx) error {
		report.MarkReported(c.UserContext())
		return errors.New("sync failed")
	})

	send(t, app, http.MethodGet, "/sync")
	_ = client.Close(context.Background())
	if server.Count() != 1 || server.Requests()[0].Error()["action"] != "GET /sync" {
		t.Errorf("a silenced scope must not hide the 500 from the adapter: reports=%d", server.Count())
	}
}

func TestUserGetterPanicAndDisabledClientAreHarmless(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	brokenUser := func(*fiber.Ctx) report.User { panic("locals type drift") }
	for name, reporter := range map[string]*Reporter{
		"panicking user getter": NewReporter(client, ReportOptions{User: brokenUser}),
		"nil client":            NewReporter(nil, ReportOptions{User: currentUser}),
	} {
		t.Run(name, func(t *testing.T) {
			app := newReportingApp(reporter, nil)
			app.Get("/panic", func(*fiber.Ctx) error { panic("boom") })
			response := send(t, app, http.MethodGet, "/panic")
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != fiber.StatusInternalServerError || string(body) != "boom" {
				t.Errorf("the app must answer like Fiber's default: %d %q", response.StatusCode, body)
			}
		})
	}
	_ = client.Close(context.Background())
	if server.Count() != 1 {
		t.Fatalf("reports = %d, want the one from the enabled client", server.Count())
	}
	if _, hasUser := server.Requests()[0].Body["user"]; hasUser {
		t.Error("a failing user getter must leave the report without a user")
	}
}
