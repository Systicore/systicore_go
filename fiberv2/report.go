package fiberv2

import (
	"runtime/debug"
	"strconv"

	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report"
)

// ReportOptions configures a Reporter.
type ReportOptions struct {
	// User returns the authenticated user of the request, typically read
	// from c.Locals where the host's auth middleware stored it. Optional;
	// a panic inside it is swallowed and the report goes out without a user.
	User func(c *fiber.Ctx) report.User
}

// Reporter reports a Fiber v2 app's panics and 5xx answers to a
// report.Client — nothing else: 4xx are expected traffic and never become
// reports. Same contract as fiberv3.Reporter; the request's
// context.Context is c.UserContext(), not the fasthttp c.Context().
// Wiring:
//
//	reporter := fiberv2.NewReporter(reports, fiberv2.ReportOptions{User: currentUser})
//	app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(errorHandler)})
//	app.Use(fiberrecover.New(reporter.RecoverConfig()))
//	app.Use(reporter.WrittenServerErrors(fiberv2.WrittenErrorOptions{})) // 5xx a handler wrote itself
//
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

// reportedKey marks a request whose failure is already reported — by the
// panic path, the ErrorHandler or WrittenServerErrors — so none of the
// others reports it again.
type reportedKey struct{}

// RecoverConfig is the recover middleware configuration that reports
// panics. Fiber v2 has no PanicHandler, so the report is taken in the
// StackTraceHandler, which runs inside recover's deferred call before the
// stack unwinds: debug.Stack() there still shows the panicking frame. The
// middleware then turns the panic into its usual error for the
// ErrorHandler, which knows not to report it again. The stock stderr dump
// is replaced by the report.
func (r *Reporter) RecoverConfig() fiberrecover.Config {
	return fiberrecover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: r.handlePanic,
	}
}

func (r *Reporter) handlePanic(c *fiber.Ctx, value interface{}) {
	panicError := report.NewPanicError(value, debug.Stack())
	r.client.Capture(c.UserContext(), r.describeRequest(c, report.FromPanic(panicError), fiber.StatusInternalServerError))
	c.Locals(reportedKey{}, true)
}

// ErrorHandler wraps the app's error handler (nil = fiber's default). The
// wrapped handler still decides the response; afterwards, when the answer
// is a 5xx (or the wrapped handler itself failed), the error is reported
// once — not when the panic path or a CaptureError on the request's
// scoped c.UserContext() already did. A report.MarkReported in a handler
// does not count: it only silences a log line (see report.Captured).
//
// Before delegating it marks c.UserContext() as reported, so the "request
// error" line the host's handler logs with .Ctx(c.UserContext()) is
// skipped by report.Hook: the adapter owns request errors, 4xx included.
//
// The report has no Trace on purpose: a Go error carries no stack, and the
// error handler's own stack is the same for every request, which would
// merge unrelated failures into one group. Without frames the backend
// groups by type, normalized message and Action — per route.
func (r *Reporter) ErrorHandler(next fiber.ErrorHandler) fiber.ErrorHandler {
	if next == nil {
		next = fiber.DefaultErrorHandler
	}
	return func(c *fiber.Ctx, err error) error {
		reportedEarlier := r.alreadyReported(c)
		c.SetUserContext(report.MarkReported(c.UserContext()))
		handlerError := next(c, err)
		if !reportedEarlier {
			r.captureServerError(c, err, responseStatus(c, handlerError))
		}
		return handlerError
	}
}

func (r *Reporter) alreadyReported(c *fiber.Ctx) bool {
	reported, _ := c.Locals(reportedKey{}).(bool)
	return reported || report.Captured(c.UserContext())
}

// responseStatus is the status the client will see: the one the error
// handler wrote, or 500 when the handler failed and Fiber answers itself.
func responseStatus(c *fiber.Ctx, handlerError error) int {
	if handlerError != nil {
		return fiber.StatusInternalServerError
	}
	return c.Response().StatusCode()
}

func (r *Reporter) captureServerError(c *fiber.Ctx, err error, status int) {
	if err == nil || status < fiber.StatusInternalServerError {
		return
	}
	r.captureRequestFailure(c, report.FromError(err), status)
}

// captureRequestFailure reports a request's 5xx at severity error and
// marks the request as reported, so no other path repeats it.
func (r *Reporter) captureRequestFailure(c *fiber.Ctx, failure report.Report, status int) {
	failure.Severity = report.SeverityError
	r.client.Capture(c.UserContext(), r.describeRequest(c, failure, status))
	c.Locals(reportedKey{}, true)
}

// describeRequest fills in what the request knows about the failure.
func (r *Reporter) describeRequest(c *fiber.Ctx, failure report.Report, status int) report.Report {
	route := c.Route().Path
	failure.Action = c.Method() + " " + route
	failure.Context.Route = route
	failure.Context.RequestID = requestID(c)
	failure.Context.Tags = map[string]string{"status": strconv.Itoa(status)}
	failure.User = r.user(c)
	return failure
}

// requestID prefers the id the Logging middleware put in the user context
// and falls back to the X-Request-ID response header, which Fiber's own
// requestid middleware sets.
func requestID(c *fiber.Ctx) string {
	if id := logging.RequestID(c.UserContext()); id != "" {
		return id
	}
	return c.GetRespHeader(logging.RequestIDKey)
}

// user calls the host's getter defensively: it runs inside the recover
// middleware's deferred call, where a second panic would crash the process.
func (r *Reporter) user(c *fiber.Ctx) report.User {
	if r.options.User == nil {
		return report.User{}
	}
	return callHook(func() report.User { return r.options.User(c) }, report.User{})
}

// callHook runs a hook the host supplied and returns fallback when it
// panics. The hooks run where a panic would do harm: inside recover's
// deferred call, or after the handler wrote its answer, which a panic
// would replace with a 500.
func callHook[Result any](hook func() Result, fallback Result) (result Result) {
	defer func() {
		if recover() != nil {
			result = fallback
		}
	}()
	return hook()
}
