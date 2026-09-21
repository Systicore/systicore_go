package logging

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// Middleware is the chi middleware: it derives a request-scoped logger from
// root (request id, method, path, client ip), stores it in the request
// context so zerolog.Ctx(r.Context()) finds it in every layer below, and
// writes one access-log line when the handler returns.
//
// Order in the chain: after chi's RequestID and RealIP (it reads both),
// before Recoverer (so a panic is logged with the request fields).
func Middleware(root zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := middleware.GetReqID(r.Context())
			if id == "" {
				id = r.Header.Get(RequestIDKey)
			}
			l := root.With().
				Str("request_id", id).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("ip", r.RemoteAddr).
				Logger()
			ctx := l.WithContext(WithRequestID(r.Context(), id))

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r.WithContext(ctx))

			l.Info().
				Int("status", ww.Status()).
				Int("bytes", ww.BytesWritten()).
				Dur("latency", time.Since(start)).
				Msg("request")
		})
	}
}
