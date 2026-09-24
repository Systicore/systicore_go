package report

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/rs/zerolog"
)

// PanicError is a recovered panic as an error: the panic value plus the
// stack of the panicking goroutine, taken inside the deferred recover —
// before the stack unwinds — and trimmed to start at the panic site.
// Error() is the text Fiber's default panic handler would produce, and
// Unwrap exposes a panic value that is itself an error.
type PanicError struct {
	Value any
	Stack string
}

// NewPanicError wraps a recovered value and the debug.Stack() taken in
// the same deferred call.
func NewPanicError(value any, stack []byte) *PanicError {
	return &PanicError{Value: value, Stack: panicSiteStack(stack)}
}

func (e *PanicError) Error() string {
	if err, isError := e.Value.(error); isError {
		return err.Error()
	}
	return fmt.Sprint(e.Value)
}

func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// FromPanic builds the report of a recovered panic: Type the panic value's
// error type ("panic" for a non-error value), Code "panic", Message the
// value, Trace the stack from the panic site, Severity critical.
func FromPanic(panicError *PanicError) Report {
	panicType := "panic"
	if err, isError := panicError.Value.(error); isError {
		if typed := firstTypedError(err); typed != nil {
			panicType = ErrorType(typed)
		}
	}
	return Report{
		Type:     panicType,
		Code:     "panic",
		Message:  panicError.Error(),
		Trace:    panicError.Stack,
		Severity: SeverityCritical,
	}
}

// Recover is the deferred guard for goroutines and job loops, which no
// HTTP recover middleware covers. It must be deferred directly:
//
//	go func() {
//		defer report.Recover(ctx, reports, "orders.sync")
//		...
//	}()
//
// A panic is captured with the real panic stack (critical), logged with
// the stack, and swallowed, so one bad job cannot take the process down.
// The line goes to zerolog.Ctx(ctx); when ctx carries no enabled logger it
// goes to stderr as JSON, where Go would have printed the crash, so a
// swallowed panic is never silent. Without a panic it does nothing; a nil
// or disabled client still logs.
func Recover(ctx context.Context, client *Client, action string) {
	value := recover()
	if value == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	panicError := NewPanicError(value, debug.Stack())
	report := FromPanic(panicError)
	report.Action = action
	client.Capture(ctx, report)
	panicLogger(ctx).Error().
		Ctx(MarkReported(ctx)).
		Str("action", action).
		Str("panic", panicError.Error()).
		Str("stack", panicError.Stack).
		Msg("recovered panic")
}

// panicFallbackOutput receives Recover's line when ctx carries no logger.
// Tests replace it.
var panicFallbackOutput io.Writer = os.Stderr

// panicLogger is zerolog.Ctx(ctx), or a stderr logger when ctx carries
// none: zerolog.Ctx then hands out a disabled logger, and a Nop logger is
// never stored in a ctx in the first place.
func panicLogger(ctx context.Context) *zerolog.Logger {
	if logger := zerolog.Ctx(ctx); logger.GetLevel() != zerolog.Disabled {
		return logger
	}
	fallback := zerolog.New(panicFallbackOutput).With().Timestamp().Logger()
	return &fallback
}

// panicSiteStack trims a debug.Stack() taken inside a deferred recover so
// it starts at the frame that panicked: the goroutine header is kept, and
// everything above the topmost panic( frame (debug.Stack, the recovering
// function) plus the runtime frames right under it (runtime.panicmem,
// runtime.sigpanic, …) is dropped. The backend groups by the top in-app
// frames, so the recover helper itself must not be one of them. A stack
// without a panic( frame is returned unchanged.
func panicSiteStack(stack []byte) string {
	lines := strings.Split(strings.TrimRight(string(stack), "\n"), "\n")
	panicLine := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "panic(") {
			panicLine = index
			break
		}
	}
	if panicLine < 0 {
		return string(stack)
	}
	siteLine := panicLine + 2 // skip panic( and its file line
	for siteLine+1 < len(lines) && strings.HasPrefix(lines[siteLine], "runtime.") {
		siteLine += 2
	}
	var builder strings.Builder
	if strings.HasPrefix(lines[0], "goroutine ") {
		builder.WriteString(lines[0])
		builder.WriteByte('\n')
	}
	for _, line := range lines[min(siteLine, len(lines)):] {
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}
