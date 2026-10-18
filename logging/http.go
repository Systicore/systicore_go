package logging

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Middleware is the net/http adapter — it works unchanged under chi,
// gorilla/mux or a plain ServeMux because it is just
// func(http.Handler) http.Handler and imports no router.
//
// It derives a request-scoped logger from root (request id, method, path,
// client address), stores it in the request context so
// zerolog.Ctx(r.Context()) finds it in every layer below, and writes one
// access-log line when the handler returns. The request id is taken from
// the incoming X-Request-ID header (a caller or an upstream proxy set it)
// or minted, and echoed back in the response header.
//
// Order: after whatever sets r.RemoteAddr from X-Forwarded-For (chi's
// RealIP), before the recoverer, so a panic is logged with request fields.
func Middleware(root zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDKey)
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set(RequestIDKey, id)

			l := root.With().
				Str("request_id", id).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("ip", r.RemoteAddr).
				Logger()
			ctx := l.WithContext(WithRequestID(r.Context(), id))

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r.WithContext(ctx))

			l.WithLevel(AccessLevel(r.URL.Path, rec.status)).
				Int("status", rec.status).
				Int("bytes", rec.bytes).
				Dur("latency", time.Since(start)).
				Msg("request")
		})
	}
}

// responseRecorder captures status and size without changing behaviour;
// Unwrap keeps http.ResponseController (Flush, Hijack, deadlines) working.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
