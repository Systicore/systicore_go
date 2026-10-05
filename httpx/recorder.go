package httpx

import "net/http"

// ResponseRecorder captures the status and byte count a handler wrote,
// for middleware that needs them afterwards (Recover, metrics). Unwrap
// keeps http.ResponseController — Flush, Hijack, deadlines — working
// through it.
type ResponseRecorder struct {
	http.ResponseWriter
	Status  int  // 200 until WriteHeader says otherwise
	Bytes   int  // body bytes written
	Written bool // the header went out; the status can no longer change
}

// NewResponseRecorder wraps w.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader records the first status and passes every call on.
func (r *ResponseRecorder) WriteHeader(code int) {
	if !r.Written {
		r.Status = code
		r.Written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write marks the header as sent (an implicit 200) and counts the bytes.
func (r *ResponseRecorder) Write(b []byte) (int, error) {
	r.Written = true
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += n
	return n, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
