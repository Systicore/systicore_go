package report

import (
	"context"

	"github.com/rs/zerolog"
)

// Hook returns a zerolog hook that turns every log event at level Error or
// worse into a report: Code "log.<level>", Message the log message,
// Severity error (critical for fatal/panic). It is the zero-effort way to
// get a service's error lines into the triage list.
//
// zerolog hooks see the level and message only, not the fields or the
// attached error — CaptureError at the error site gives the richer report
// (stable code, stack, request id). Attach it AFTER creating the client
// from a logger without it:
//
//	rep, _ := report.New(report.Options{..., Logger: root})
//	root = root.Hook(rep.Hook())
func (c *Client) Hook() zerolog.Hook {
	return hook{c: c}
}

type hook struct{ c *Client }

func (h hook) Run(_ *zerolog.Event, level zerolog.Level, msg string) {
	if level < zerolog.ErrorLevel || msg == "" {
		return
	}
	severity := SeverityError
	if level >= zerolog.FatalLevel {
		severity = SeverityCritical
	}
	h.c.Capture(context.Background(), Report{
		Code:     "log." + level.String(),
		Message:  msg,
		Severity: severity,
	})
}
