package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ErrBodyTooLarge is what DecodeJSON returns when the body ran past the
// cap MaxBody set. Handlers answer it with 413 instead of the generic 400.
var ErrBodyTooLarge = errors.New("httpx: request body too large")

// JSON writes v as the response body with status. Marshal, then write —
// not json.Encoder — so the bytes match what the Fiber services sent: no
// trailing newline, Content-Type application/json. A value that does not
// marshal is a programming error and gets a bare 500 instead.
func JSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"Internal Server Error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// envelope is the {"status","message"} answer of the Systicore handlers,
// field order and omitempty as in the services' common.IResponse.
type envelope struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Error writes the {"status":"error","message":...} envelope the
// Systicore handlers answer a failure with.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, envelope{Status: "error", Message: message})
}

// StatusError writes {"error": <status text>}, e.g.
// {"error":"Request Entity Too Large"}: what Fiber's error handler gave
// a framework error, now the same in every service.
func StatusError(w http.ResponseWriter, status int) {
	JSON(w, status, map[string]string{"error": http.StatusText(status)})
}

// NotFound is chi's r.NotFound handler, with Fiber's body:
// {"error":"Cannot GET /path"}.
func NotFound(w http.ResponseWriter, r *http.Request) {
	JSON(w, http.StatusNotFound, map[string]string{"error": "Cannot " + r.Method + " " + r.URL.Path})
}

// MethodNotAllowed is chi's r.MethodNotAllowed handler:
// {"error":"Method Not Allowed"}.
func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	StatusError(w, http.StatusMethodNotAllowed)
}

// DecodeJSON unmarshals the request body into v. Like Fiber's
// Bind().JSON() it ignores Content-Type and tolerates unknown fields, so
// clients that worked against a Fiber service keep working. A body cut
// off by MaxBody comes back as ErrBodyTooLarge.
func DecodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return ErrBodyTooLarge
		}
		return err
	}
	return json.Unmarshal(body, v)
}
