package fiberv3

import (
	"runtime/debug"
	"strconv"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report"
)

// ReportOptions configures a Reporter.
type ReportOptions struct {
	// User returns the authenticated user of the request, typically read
	// from c.Locals where the host's auth middleware stored it. Optional;
	// a panic inside it is swallowed and the report goes out without a user.
	User func(c fiber.Ctx) report.User
}

// Reporter reports a Fiber v3 app's panics and 5xx answers to a
// report.Client — nothing else: 4xx are expected traffic and never become
// reports. Wiring:
//
//	reporter := fiberv3.NewReporter(reports, fiberv3.ReportOptions{User: currentUser})
//	app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(errorHandler)})
//	app.Use(fiberrecover.New(reporter.RecoverConfig()))
//
// Both reports carry Action "<METHOD> <route template>" (c.Route().Path,
// never the raw path with ids), the route, the request id and the user.
// A nil or disabled client makes every hook a no-op, so the wiring can
// stay in place when REPORTS_URL / REPORTS_KEY are empty.
type Reporter struct {
	client  *report.Client
	options ReportOptions
}

// NewReporter binds client to the adapter.
func NewReporter(client *report.Client, options ReportOptions) *Reporter {
	return &Reporter{client: client, options: options}
}

// reportedKey marks a request whose failure is already reported (the
// panic path), so the ErrorHandler does not report the resulting 500.
type reportedKey struct{}

// RecoverConfig is the recover middleware configuration that reports
// panics. Its PanicHandler runs inside recover's deferred call, before the
// stack unwinds, so debug.Stack() there still shows the panicking frame —
// the stock middleware turns the panic into a plain error and the stack
// is gone by the time the ErrorHandler runs. The handler returns a
// *report.PanicError whose Error() matches Fiber's default text and whose
// Stack the host's error handler may log.
func (r *Reporter) RecoverConfig() fiberrecover.Config {
	return fiberrecover.Config{PanicHandler: r.handlePanic}
}

func (r *Reporter) handlePanic(c fiber.Ctx, value any) error {
	panicError := report.NewPanicError(value, debug.Stack())
	r.client.Capture(c.Context(), r.describeRequest(c, report.FromPanic(panicError), fiber.StatusInternalServerError))
	c.Locals(reportedKey{}, true)
	return panicError
}

// ErrorHandler wraps the app's error handler (nil = fiber's default). The
// wrapped handler still decides the response; afterwards, when the answer
// is a 5xx (or the wrapped handler itself failed), the error is reported
// once — not when the panic path or a CaptureError on the request's
// scoped ctx already did. A report.MarkReported in a handler does not
// count: it only silences a log line (see report.Captured).
//
// Before delegating it marks c.Context() as reported, so the "request
// error" line the host's handler logs with .Ctx(c.Context()) is skipped by
// report.Hook: the adapter owns request errors, 4xx included.
//
// The report has no Trace on purpose: a Go error carries no stack, and the
// error handler's own stack is the same for every request, which would
// merge unrelated failures into one group. Without frames the backend
// groups by type, normalized message and Action — per route.
func (r *Reporter) ErrorHandler(next fiber.ErrorHandler) fiber.ErrorHandler {
	if next == nil {
		next = fiber.DefaultErrorHandler
	}
	return func(c fiber.Ctx, err error) error {
		reportedEarlier := r.alreadyReported(c)
		c.SetContext(report.MarkReported(c.Context()))
		handlerError := next(c, err)
		if !reportedEarlier {
			r.captureServerError(c, err, responseStatus(c, handlerError))
		}
		return handlerError
	}
}

func (r *Reporter) alreadyReported(c fiber.Ctx) bool {
	reported, _ := c.Locals(reportedKey{}).(bool)
	return reported || report.Captured(c.Context())
}

// responseStatus is the status the client will see: the one the error
// handler wrote, or 500 when the handler failed and Fiber answers itself.
func responseStatus(c fiber.Ctx, handlerError error) int {
	if handlerError != nil {
		return fiber.StatusInternalServerError
	}
	return c.Response().StatusCode()
}

func (r *Reporter) captureServerError(c fiber.Ctx, err error, status int) {
	if err == nil || status < fiber.StatusInternalServerError {
		return
	}
	serverError := report.FromError(err)
	serverError.Severity = report.SeverityError
	r.client.Capture(c.Context(), r.describeRequest(c, serverError, status))
	c.Locals(reportedKey{}, true)
}

// describeRequest fills in what the request knows about the failure.
func (r *Reporter) describeRequest(c fiber.Ctx, failure report.Report, status int) report.Report {
	route := c.Route().Path
	failure.Action = c.Method() + " " + route
	failure.Context.Route = route
	failure.Context.RequestID = requestID(c)
	failure.Context.Tags = map[string]string{"status": strconv.Itoa(status)}
	failure.User = r.user(c)
	return failure
}

// requestID prefers the id the Logging middleware put in the context and
// falls back to the X-Request-ID response header, which Fiber's own
// requestid middleware sets.
func requestID(c fiber.Ctx) string {
	if id := logging.RequestID(c.Context()); id != "" {
		return id
	}
	return c.GetRespHeader(logging.RequestIDKey)
}

// user calls the host's getter defensively: it runs inside the recover
// middleware's deferred call, where a second panic would crash the process.
func (r *Reporter) user(c fiber.Ctx) (user report.User) {
	if r.options.User == nil {
		return report.User{}
	}
	defer func() {
		if recover() != nil {
			user = report.User{}
		}
	}()
	return r.options.User(c)
}
