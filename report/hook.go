package report

import (
	"github.com/rs/zerolog"
)

// Hook returns an opt-in zerolog hook that turns Error, Fatal and Panic
// log events into reports: Code "log.<level>", Message the log message,
// Trace the stack of the logging call site, Severity error (critical for
// fatal/panic), the request id from the event's ctx. A line without a
// message (log.Error().Err(err).Send()) is reported too, under the
// placeholder message emptyLogMessage.
//
// Error events are queued like Capture. Fatal and Panic events are
// delivered before the hook returns, bounded by finalDeliveryTimeout:
// zerolog's Fatal calls os.Exit right after its hooks, and an unrecovered
// Panic crashes the process, so a queued report would never go out.
//
// Do not attach it blindly to the logger of an HTTP error handler. A hook
// sees the level and the message only, not the fields or the error, and
// error handlers log one constant line ("request error") for every 4xx
// and 5xx — so the backend would fold every HTTP failure, 404s included,
// into one "log.error / request error" group, and double-report the 5xx
// the Fiber adapters already capture. Report request errors with the
// fiberv2 / fiberv3 ErrorHandler and CaptureError at the error site; use
// the hook for background code that only logs.
//
// Events whose ctx (zerolog Event.Ctx or Logger.With().Ctx) carries a
// scope marked by Capture / CaptureError / MarkReported are skipped, so an
// explicitly reported error is not reported again by the log line that
// follows it. Attach the hook AFTER creating the client from a logger
// without it:
//
//	reports, _ := report.New(report.Options{..., Logger: root})
//	jobLog := root.Hook(reports.Hook())
func (c *Client) Hook() zerolog.Hook {
	return hook{client: c}
}

type hook struct{ client *Client }

// hookFramePrefix peels zerolog's own frames off the top of the stack.
const hookFramePrefix = "github.com/rs/zerolog."

// emptyLogMessage stands in for the message of a log line that has none.
// Such a report still groups by its code and its call-site frames; the
// placeholder only gives it a readable title.
const emptyLogMessage = "(log line without a message)"

func (h hook) Run(event *zerolog.Event, level zerolog.Level, message string) {
	if !h.client.Enabled() || !reportableLevel(level) {
		return
	}
	ctx := event.GetCtx()
	if Reported(ctx) {
		return
	}
	report := Report{
		Code:     "log." + level.String(),
		Message:  messageOrPlaceholder(message),
		Trace:    callerStack(2, hookFramePrefix),
		Severity: SeverityError,
	}
	if endsTheProcess(level) {
		report.Severity = SeverityCritical
		h.client.deliverBeforeExit(ctx, report)
		return
	}
	h.client.enqueue(ctx, report)
}

// reportableLevel is Error, Fatal or Panic — not NoLevel (log.Log()) or
// Disabled, which sort above Panic in zerolog's numbering.
func reportableLevel(level zerolog.Level) bool {
	return level >= zerolog.ErrorLevel && level <= zerolog.PanicLevel
}

// endsTheProcess is Fatal (os.Exit after the hooks) or Panic (a crash
// unless something recovers). Only called for reportable levels.
func endsTheProcess(level zerolog.Level) bool {
	return level >= zerolog.FatalLevel
}

func messageOrPlaceholder(message string) string {
	if message == "" {
		return emptyLogMessage
	}
	return message
}
