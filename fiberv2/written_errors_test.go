package fiberv2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	fiberlogger "github.com/gofiber/fiber/v2/middleware/logger"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/Systicore/systicore_go/report"
	"github.com/Systicore/systicore_go/report/reporttest"
)

// newWrittenErrorApp is the documented wiring plus WrittenServerErrors,
// registered after recover and Logging.
func newWrittenErrorApp(reporter *Reporter, options WrittenErrorOptions) *fiber.App {
	app := newReportingApp(reporter, nil)
	app.Use(reporter.WrittenServerErrors(options))
	return app
}

func TestWrittenServerErrorsReportsTheStatusButNeverTheBody(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{User: currentUser}), WrittenErrorOptions{})
	const responseBody = "upstream said: password authentication failed for user orders_rw"
	app.Get("/orders/:id/invoice", func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusBadGateway).SendString(responseBody)
	})

	response := send(t, app, http.MethodGet, "/orders/8812/invoice")
	body, _ := io.ReadAll(response.Body)
	_ = client.Close(context.Background())

	if response.StatusCode != fiber.StatusBadGateway || string(body) != responseBody {
		t.Errorf("the response must pass untouched: %d %q", response.StatusCode, body)
	}
	if server.Count() != 1 {
		t.Fatalf("reports = %d, want one", server.Count())
	}
	request := server.Requests()[0]
	errorObject := request.Error()
	if errorObject["code"] != "http.502" || errorObject["message"] != "handler answered 502 Bad Gateway" ||
		errorObject["action"] != "GET /orders/:id/invoice" || errorObject["severity"] != "error" {
		t.Errorf("written 502 report = %v", errorObject)
	}
	if _, hasType := errorObject["type"]; hasType {
		t.Error("a status-only report must have no type, so the backend groups it by its code")
	}
	if _, hasTrace := errorObject["trace"]; hasTrace {
		t.Error("a written-5xx report must carry no trace, or every route would group together")
	}
	contextObject := request.Context()
	if contextObject["route"] != "/orders/:id/invoice" || contextObject["requestId"] != "req-f2" ||
		contextObject["tags"].(map[string]any)["status"] != "502" {
		t.Errorf("context = %v", contextObject)
	}
	if request.User()["id"] != "42" {
		t.Errorf("user = %v", request.User())
	}
	if bytes.Contains(request.Raw, []byte("password authentication")) {
		t.Errorf("the response body leaked into the report: %s", request.Raw)
	}
}

func TestWrittenServerErrorsIgnoresAnswersBelow500(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{})
	app.Get("/written/:status", func(c *fiber.Ctx) error {
		status, _ := c.ParamsInt("status")
		return c.SendStatus(status)
	})

	for _, target := range []string{"/written/200", "/written/302", "/written/404", "/written/422", "/written/499", "/nowhere"} {
		send(t, app, http.MethodGet, target)
	}
	_ = client.Close(context.Background())
	if server.Count() != 0 {
		t.Errorf("reports = %d, want none below 500", server.Count())
	}
}

// storageError is an application error with its own report code.
type storageError struct{}

func (storageError) Error() string { return "invoice store unavailable" }
func (storageError) Code() string  { return "invoice.store_unavailable" }

func TestWrittenServerErrorsReportsTheRecordedCause(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	const serverErrorKey = "server_error"
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{
		Cause: func(c *fiber.Ctx) error {
			cause, _ := c.Locals(serverErrorKey).(error)
			return cause
		},
	})
	app.Get("/invoices/:id", func(c *fiber.Ctx) error {
		c.Locals(serverErrorKey, fmt.Errorf("load invoice 8812: %w", storageError{}))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"message": "internal error"})
	})
	app.Get("/unrecorded", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusServiceUnavailable)
	})

	send(t, app, http.MethodGet, "/invoices/8812")
	send(t, app, http.MethodGet, "/unrecorded")
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("reports = %d, want two", len(requests))
	}
	recorded, unrecorded := requests[0].Error(), requests[1].Error()
	if recorded["type"] != "fiberv2.storageError" || recorded["code"] != "invoice.store_unavailable" ||
		recorded["message"] != "load invoice 8812: invoice store unavailable" ||
		recorded["action"] != "GET /invoices/:id" || recorded["severity"] != "error" {
		t.Errorf("report with a recorded cause = %v", recorded)
	}
	if unrecorded["code"] != "http.503" || unrecorded["message"] != "handler answered 503 Service Unavailable" {
		t.Errorf("without a recorded cause the report must name the status: %v", unrecorded)
	}
}

func TestWrittenServerErrorsNamesAStatusOnlyReportByTheAppsCode(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	const errorCodeKey = "error_code"
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{
		Code: func(c *fiber.Ctx) string {
			code, _ := c.Locals(errorCodeKey).(string)
			return code
		},
	})
	// The app's helper stores the envelope code it answered with; the
	// handler then returns nil, so there is no cause, only the code.
	app.Post("/avatars/:id", func(c *fiber.Ctx) error {
		code := c.Query("code")
		if code != "" {
			c.Locals(errorCodeKey, code)
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"code": code})
	})

	for _, target := range []string{"/avatars/7?code=avatars.update_failed", "/avatars/7?code=avatars.storage_full", "/avatars/7"} {
		send(t, app, http.MethodPost, target)
	}
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 3 {
		t.Fatalf("reports = %d, want three", len(requests))
	}
	for index, wantCode := range []string{"avatars.update_failed", "avatars.storage_full", "http.500"} {
		errorObject := requests[index].Error()
		if errorObject["code"] != wantCode || errorObject["message"] != "handler answered 500 Internal Server Error" ||
			errorObject["action"] != "POST /avatars/:id" {
			t.Errorf("report %d = %v, want code %q", index, errorObject, wantCode)
		}
		// Without a type the backend groups by the code, so two codes on
		// one route and status stay two groups.
		if _, hasType := errorObject["type"]; hasType {
			t.Errorf("report %d must have no type, or it would group by the type instead of the code: %v", index, errorObject)
		}
	}
}

func TestWrittenServerErrorsFillsInTheCodeACauseLacks(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	const serverErrorKey, errorCodeKey = "server_error", "error_code"
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{
		Cause: func(c *fiber.Ctx) error {
			cause, _ := c.Locals(serverErrorKey).(error)
			return cause
		},
		Code: func(c *fiber.Ctx) string {
			code, _ := c.Locals(errorCodeKey).(string)
			return code
		},
	})
	app.Get("/invoices/:id", func(c *fiber.Ctx) error {
		c.Locals(serverErrorKey, fmt.Errorf("load invoice 8812: %w", storageError{}))
		c.Locals(errorCodeKey, "invoices.load_failed")
		return c.SendStatus(fiber.StatusInternalServerError)
	})
	app.Get("/exports/:id", func(c *fiber.Ctx) error {
		c.Locals(serverErrorKey, errors.New("export queue full"))
		c.Locals(errorCodeKey, "exports.queue_full")
		return c.SendStatus(fiber.StatusServiceUnavailable)
	})

	send(t, app, http.MethodGet, "/invoices/8812")
	send(t, app, http.MethodGet, "/exports/3")
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("reports = %d, want two", len(requests))
	}
	coded, uncoded := requests[0].Error(), requests[1].Error()
	if coded["type"] != "fiberv2.storageError" || coded["code"] != "invoice.store_unavailable" {
		t.Errorf("the cause's own code must win over the response code: %v", coded)
	}
	if uncoded["type"] != "error" || uncoded["code"] != "exports.queue_full" || uncoded["message"] != "export queue full" {
		t.Errorf("the response code must fill in for a cause without one: %v", uncoded)
	}
}

func TestWrittenServerErrorsLeavesReturnedErrorsToTheErrorHandler(t *testing.T) {
	// One logger per app: it binds the first app's ErrorHandler it sees.
	quietLogger := func() fiber.Handler { return fiberlogger.New(fiberlogger.Config{Output: io.Discard}) }
	for name, register := range map[string]func(app *fiber.App, reporter *Reporter){
		"alone": func(app *fiber.App, reporter *Reporter) {
			app.Use(reporter.WrittenServerErrors(WrittenErrorOptions{}))
		},
		// Fiber's logger calls the ErrorHandler itself and returns nil, so
		// the middleware sees a written 500 that is already reported.
		"outside Fiber's logger": func(app *fiber.App, reporter *Reporter) {
			app.Use(reporter.WrittenServerErrors(WrittenErrorOptions{}))
			app.Use(quietLogger())
		},
		"inside Fiber's logger": func(app *fiber.App, reporter *Reporter) {
			app.Use(quietLogger())
			app.Use(reporter.WrittenServerErrors(WrittenErrorOptions{}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := reporttest.NewServer(t)
			client := newReportClient(t, server)
			reporter := NewReporter(client, ReportOptions{})
			app := newReportingApp(reporter, nil)
			register(app, reporter)
			app.Get("/shards/:id/sync", func(*fiber.Ctx) error { return errors.New("dial tcp 10.0.0.5:3306: connection refused") })

			if response := send(t, app, http.MethodGet, "/shards/7/sync"); response.StatusCode != fiber.StatusInternalServerError {
				t.Errorf("status = %d, want 500", response.StatusCode)
			}
			_ = client.Close(context.Background())
			if server.Count() != 1 {
				t.Fatalf("reports = %d, want exactly one", server.Count())
			}
			if errorObject := server.Requests()[0].Error(); errorObject["type"] != "error" ||
				errorObject["message"] != "dial tcp 10.0.0.5:3306: connection refused" {
				t.Errorf("the ErrorHandler's report of the returned error is the one: %v", errorObject)
			}
		})
	}
}

func TestWrittenServerErrorsDoesNotRepeatAPanic(t *testing.T) {
	for name, newApp := range map[string]func(reporter *Reporter) *fiber.App{
		"inside recover": func(reporter *Reporter) *fiber.App {
			return newWrittenErrorApp(reporter, WrittenErrorOptions{})
		},
		"outside recover": func(reporter *Reporter) *fiber.App {
			app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(nil)})
			app.Use(reporter.WrittenServerErrors(WrittenErrorOptions{}))
			app.Use(fiberrecover.New(reporter.RecoverConfig()))
			return app
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := reporttest.NewServer(t)
			client := newReportClient(t, server)
			app := newApp(NewReporter(client, ReportOptions{}))
			app.Get("/matches/:id", func(*fiber.Ctx) error {
				explodeMatch()
				return nil
			})

			if response := send(t, app, http.MethodGet, "/matches/77"); response.StatusCode != fiber.StatusInternalServerError {
				t.Errorf("status = %d, want 500", response.StatusCode)
			}
			_ = client.Close(context.Background())
			if server.Count() != 1 {
				t.Fatalf("reports = %d, want the panic report only", server.Count())
			}
			if errorObject := server.Requests()[0].Error(); errorObject["code"] != "panic" || errorObject["severity"] != "critical" {
				t.Errorf("report = %v", errorObject)
			}
		})
	}
}

func TestWrittenServerErrorsRespectsTheHandlersOwnCapture(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{})
	// No WithScope anywhere in the app: the middleware installs it, or the
	// capture below would go unnoticed.
	app.Get("/import", func(c *fiber.Ctx) error {
		client.CaptureError(c.UserContext(), errors.New("import failed"), "import.run", report.SeverityCritical)
		return c.SendStatus(fiber.StatusInternalServerError)
	})
	// MarkReported only silences a log line: the written 500 is still
	// reported.
	app.Get("/sync", func(c *fiber.Ctx) error {
		report.MarkReported(c.UserContext())
		return c.SendStatus(fiber.StatusInternalServerError)
	})

	send(t, app, http.MethodGet, "/import")
	send(t, app, http.MethodGet, "/sync")
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("reports = %d, want the handler's capture and the silenced 500", len(requests))
	}
	if action := requests[0].Error()["action"]; action != "import.run" {
		t.Errorf("the handler's own capture must be the only report of /import, got action %v", action)
	}
	if action := requests[1].Error()["action"]; action != "GET /sync" {
		t.Errorf("a silenced scope must not hide the written 500, got action %v", action)
	}
}

func TestWrittenServerErrorsSkipsWhatTheAppExpects(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	app := newWrittenErrorApp(NewReporter(client, ReportOptions{}), WrittenErrorOptions{
		Skip: func(c *fiber.Ctx) bool { return c.Route().Path == "/health" },
	})
	app.Get("/health", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusServiceUnavailable) })
	app.Get("/orders", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusServiceUnavailable) })

	send(t, app, http.MethodGet, "/health")
	send(t, app, http.MethodGet, "/orders")
	_ = client.Close(context.Background())
	if server.Count() != 1 || server.Requests()[0].Error()["action"] != "GET /orders" {
		t.Errorf("only the non-skipped 503 must be reported: %v", server.Requests())
	}
}

func TestWrittenServerErrorsReportsOnceWhenRegisteredTwice(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	reporter := NewReporter(client, ReportOptions{})
	app := newWrittenErrorApp(reporter, WrittenErrorOptions{})
	api := app.Group("/api", reporter.WrittenServerErrors(WrittenErrorOptions{}))
	api.Get("/orders", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusInternalServerError) })

	send(t, app, http.MethodGet, "/api/orders")
	_ = client.Close(context.Background())
	if server.Count() != 1 {
		t.Errorf("reports = %d, want one", server.Count())
	}
}

func TestWrittenServerErrorsHooksAndDisabledClientAreHarmless(t *testing.T) {
	server := reporttest.NewServer(t)
	client := newReportClient(t, server)
	panickingHooks := WrittenErrorOptions{
		Cause: func(*fiber.Ctx) error { panic("locals type drift") },
		Code:  func(*fiber.Ctx) string { panic("locals type drift") },
		Skip:  func(*fiber.Ctx) bool { panic("route table drift") },
	}
	for name, app := range map[string]*fiber.App{
		"panicking hooks": newWrittenErrorApp(NewReporter(client, ReportOptions{}), panickingHooks),
		"nil client":      newWrittenErrorApp(NewReporter(nil, ReportOptions{}), WrittenErrorOptions{}),
	} {
		t.Run(name, func(t *testing.T) {
			app.Get("/orders", func(c *fiber.Ctx) error {
				return c.Status(fiber.StatusServiceUnavailable).SendString("try again later")
			})
			response := send(t, app, http.MethodGet, "/orders")
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != fiber.StatusServiceUnavailable || string(body) != "try again later" {
				t.Errorf("the written answer must survive: %d %q", response.StatusCode, body)
			}
		})
	}
	_ = client.Close(context.Background())
	if server.Count() != 1 {
		t.Fatalf("reports = %d, want the one from the enabled client", server.Count())
	}
	if errorObject := server.Requests()[0].Error(); errorObject["code"] != "http.503" {
		t.Errorf("panicking Cause and Code hooks must fall back to the status-only report: %v", errorObject)
	}
}
