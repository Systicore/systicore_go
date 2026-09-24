package report

import (
	"context"
	"sync/atomic"
)

// scope is the mutable "this request / job already reported" state carried
// by a context. It has to be mutable: CaptureError cannot hand a new ctx
// back to the log line that usually follows it, so it flips a flag in
// place and every ctx derived from the scoped one sees the change.
//
// The two flags answer different questions. captured means an explicit
// Capture / CaptureError ran, so the Fiber adapters must not report the
// same failure again. silenced (MarkReported) only keeps log lines out of
// the log bridges and says nothing about whether the failure was reported.
type scope struct {
	captured atomic.Bool
	silenced atomic.Bool
}

type scopeKey struct{}

// WithScope returns ctx carrying a fresh report scope, or ctx itself when
// it already has one. Install it once per request or job run: Capture /
// CaptureError on that ctx (or one derived from it) marks the scope, and
// the zerolog Hook and the slog handler then skip log lines whose ctx
// carries it — so
//
//	reports.CaptureError(ctx, err, "push.deliver", report.SeverityError)
//	log.Error().Ctx(ctx).Err(err).Msg("push failed")
//
// produces one report, not two.
func WithScope(ctx context.Context) context.Context {
	if scopeFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, &scope{})
}

// MarkReported keeps log lines out of the log bridges: it marks ctx's
// scope as silenced and returns ctx, or, when ctx has no scope, returns a
// child ctx with an already silenced one. Log with the returned ctx.
//
// It does not count as a capture: a Fiber adapter still reports a 5xx on
// a silenced scope, since nothing was sent (see Captured).
func MarkReported(ctx context.Context) context.Context {
	if current := scopeFrom(ctx); current != nil {
		current.silenced.Store(true)
		return ctx
	}
	silenced := &scope{}
	silenced.silenced.Store(true)
	return context.WithValue(ctx, scopeKey{}, silenced)
}

// Reported reports whether the log bridges skip ctx: its scope was
// captured into (Capture / CaptureError) or silenced (MarkReported).
func Reported(ctx context.Context) bool {
	current := scopeFrom(ctx)
	return current != nil && (current.captured.Load() || current.silenced.Load())
}

// Captured reports whether an explicit Capture / CaptureError ran on
// ctx's scope — the question an adapter asks before reporting a failure
// itself. MarkReported alone does not make it true.
func Captured(ctx context.Context) bool {
	current := scopeFrom(ctx)
	return current != nil && current.captured.Load()
}

// markCaptured flips the captured flag of ctx's scope, if it has one.
func markCaptured(ctx context.Context) {
	if current := scopeFrom(ctx); current != nil {
		current.captured.Store(true)
	}
}

func scopeFrom(ctx context.Context) *scope {
	if ctx == nil {
		return nil
	}
	current, _ := ctx.Value(scopeKey{}).(*scope)
	return current
}
