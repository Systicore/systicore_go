package fiberv3

import (
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/Systicore/systicore_go/report"
)

// WrittenErrorOptions configures Reporter.WrittenServerErrors. Every hook
// is optional and called defensively: a panic inside one is swallowed.
type WrittenErrorOptions struct {
	// Cause returns the error behind a 5xx the handler wrote itself, when
	// the app recorded one — typically read from c.Locals, where its "log
	// and answer 500" helper stored it:
	//
	//	Cause: func(c fiber.Ctx) error {
	//		cause, _ := c.Locals(localServerError).(error)
	//		return cause
	//	},
	//
	// The report is then built from that error like one the ErrorHandler
	// saw: Type from the error chain, Code from a report.Coder, Message the
	// error text. When Cause is nil, returns nil or panics, the report is
	// built from the status and Code instead.
	//
	// Such a report always has a Type — the error's Go type, or "error" —
	// and the backend groups by Type before Code, so a report.Coder's code
	// does not split groups: causes of one type whose Error() texts
	// normalize alike share a group whatever their codes, unless Error()
	// includes the code. An app that records only a response code sets
	// Code instead of wrapping the code in an error for Cause.
	Cause func(c fiber.Ctx) error
	// Code returns the application code the handler answered with, when
	// the app keeps one — typically the error code of its response
	// envelope, stored in c.Locals by the helper that wrote it:
	//
	//	Code: func(c fiber.Ctx) string {
	//		code, _ := c.Locals(localErrorCode).(string)
	//		return code
	//	},
	//
	// A non-empty code replaces "http.<status>" in a report without a
	// cause. That report has no Type, so the backend groups it by this
	// code, the message and the route: different codes on one route stay
	// apart. With a cause, the cause's own report.Coder code wins and Code
	// fills in only when it has none; the grouping then follows the
	// cause's Type. When Code is nil, returns "" or panics, the code stays
	// "http.<status>" or the cause's.
	Code func(c fiber.Ctx) string
	// Skip reports whether a written 5xx is expected and must not become a
	// report, e.g. a health probe's 503 that restates an outage the failing
	// routes already report with their real cause. It runs after the
	// handler, so c.Route() is the route that answered. A panic inside it
	// counts as false.
	Skip func(c fiber.Ctx) bool
}

// WrittenServerErrors is the middleware for the 5xx answers the
// ErrorHandler never sees: a handler that writes the status itself —
// c.Status(500).JSON(…) — and returns nil hands Fiber no error. Register
// it after the recover middleware and Logging:
//
//	app.Use(fiberrecover.New(reporter.RecoverConfig()))
//	app.Use(fiberv3.Logging(root))
//	app.Use(reporter.WrittenServerErrors(fiberv3.WrittenErrorOptions{Cause: recordedCause}))
//
// After the chain it reports a 5xx status once, at severity error, with
// the same Action, route, request id, status tag and user as the
// ErrorHandler's reports. The response body is never read: it can echo
// internal details, and the handler already chose what the client sees.
// Without a recorded cause the report is Code "http.<status>" (or the
// app's own code, see WrittenErrorOptions.Code) and Message "handler
// answered <status> <status text>"; it has no type and no trace, so the
// backend groups it per route, status and code.
//
// Nothing is reported twice:
//   - A returned error passes through untouched: the ErrorHandler reports
//     it after the chain, or already did inside Fiber's logger middleware,
//     which calls it and returns nil.
//   - A panic unwinds past this middleware to the recover reporter, or
//     arrives here already turned into an error.
//   - A request the ErrorHandler, the panic path or this middleware
//     (registered twice) already reported is skipped.
//   - It installs report.WithScope on c.Context(), so a handler's own
//     Capture / CaptureError on that ctx counts, and the 5xx the handler
//     then writes is not reported again. report.MarkReported only
//     silences a log line and does not count (see report.Captured).
//
// 2xx, 3xx and 4xx answers pass untouched. A nil or disabled client makes
// it a no-op apart from the scope.
func (r *Reporter) WrittenServerErrors(options WrittenErrorOptions) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.SetContext(report.WithScope(c.Context()))
		if err := c.Next(); err != nil {
			return err
		}
		status := c.Response().StatusCode()
		if status < fiber.StatusInternalServerError || r.alreadyReported(c) || options.skip(c) {
			return nil
		}
		r.captureRequestFailure(c, options.writtenServerError(c, status), status)
		return nil
	}
}

func (options WrittenErrorOptions) skip(c fiber.Ctx) bool {
	if options.Skip == nil {
		return false
	}
	return callHook(func() bool { return options.Skip(c) }, false)
}

// writtenServerError is the report of a written 5xx: built from the cause
// the app recorded, else from the status alone, with the app's response
// code where the report has none of its own. Nothing in it comes from the
// response.
func (options WrittenErrorOptions) writtenServerError(c fiber.Ctx, status int) report.Report {
	if failure := options.causeReport(c); failure != nil {
		if failure.Code == "" {
			failure.Code = options.responseCode(c)
		}
		return *failure
	}
	failure := statusReport(status)
	if code := options.responseCode(c); code != "" {
		failure.Code = code
	}
	return failure
}

// causeReport is the report built from the error the app recorded, nil
// when there is none. The cause's Error() and Code() run under the same
// guard as the hook, since they are host code too.
func (options WrittenErrorOptions) causeReport(c fiber.Ctx) *report.Report {
	if options.Cause == nil {
		return nil
	}
	return callHook(func() *report.Report {
		cause := options.Cause(c)
		if cause == nil {
			return nil
		}
		failure := report.FromError(cause)
		return &failure
	}, nil)
}

func (options WrittenErrorOptions) responseCode(c fiber.Ctx) string {
	if options.Code == nil {
		return ""
	}
	return callHook(func() string { return options.Code(c) }, "")
}

// statusReport names a written 5xx by its status alone: Code
// "http.<status>", Message "handler answered <status> <status text>".
func statusReport(status int) report.Report {
	statusCode := strconv.Itoa(status)
	message := "handler answered " + statusCode
	if statusText := http.StatusText(status); statusText != "" {
		message += " " + statusText
	}
	return report.Report{Code: "http." + statusCode, Message: message}
}
