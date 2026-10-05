package httpx

import "net/http"

// MaxBody caps the request body at max bytes. A declared Content-Length
// above the cap is answered by reject before the client uploads the
// payload; a chunked or lying body is cut off by http.MaxBytesReader,
// which DecodeJSON reports as ErrBodyTooLarge. A nil reject answers
// 413 {"error":"Request Entity Too Large"}.
func MaxBody(max int64, reject http.HandlerFunc) Middleware {
	if reject == nil {
		reject = func(w http.ResponseWriter, _ *http.Request) {
			StatusError(w, http.StatusRequestEntityTooLarge)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				reject(w, r)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}
