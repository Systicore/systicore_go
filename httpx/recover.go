package httpx

import (
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"
)

// RecoverOptions configure Recover.
type RecoverOptions struct {
	// Stack puts the stack trace into the log line: on in development,
	// off in production, so a panic never dumps internals into a log
	// shipper.
	Stack bool
	// OnPanic, when set, is called with every recovered panic and its
	// stack, after the 500 went out — the hook for an error reporter
	// such as report.Client.
	OnPanic func(r *http.Request, value any, stack []byte)
}

// Recover turns a handler panic into a 500 {"error":"Internal Server
// Error"} and an error line on the request-scoped logger, so the line
// carries request_id, method, path and user_id. Mount it after
// logging.Middleware.
//
// When the handler had already started its answer no second one is
// written. http.ErrAbortHandler is re-panicked: it is net/http's own
// signal to abort the response silently and must reach the server.
func Recover(o RecoverOptions) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := NewResponseRecorder(w)
			defer func() {
				value := recover()
				if value == nil {
					return
				}
				if value == http.ErrAbortHandler { //nolint:errorlint // a recovered value, not a wrapped error
					panic(value)
				}
				var stack []byte
				if o.Stack || o.OnPanic != nil {
					stack = debug.Stack()
				}
				event := zerolog.Ctx(r.Context()).Error().
					Int("status", http.StatusInternalServerError).
					Interface("panic", value)
				if o.Stack {
					event = event.Bytes("stack", stack)
				}
				event.Msg("request error")
				if !rec.Written {
					StatusError(rec, http.StatusInternalServerError)
				}
				if o.OnPanic != nil {
					o.OnPanic(r, value, stack)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
