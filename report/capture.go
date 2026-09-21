package report

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"

	"github.com/Systicore/systicore_go/logging"
)

// CaptureError reports err: Code is the error's kind (see ErrorCode),
// Message its text, Trace the caller's stack plus the request id from ctx
// so the report can be joined with the service logs, Action what was
// being done ("POST /api/v1/orders", "eventpush.deliver"). Nil err is a
// no-op.
func (c *Client) CaptureError(ctx context.Context, err error, action string, severity Severity) {
	if err == nil {
		return
	}
	c.Capture(ctx, Report{
		Code:     ErrorCode(err),
		Message:  err.Error(),
		Trace:    traceWithRequestID(ctx, callerStack(2)),
		Action:   action,
		Severity: severity,
	})
}

// Coder lets an error name its own stable code (e.g. "db.deadlock");
// CaptureError prefers it over the type name.
type Coder interface {
	Code() string
}

// ErrorCode derives a stable dedup code from an error: a Coder's code if
// any error in the chain has one, else the concrete type of the outermost
// error that is not a generic wrapper ("*fs.PathError", "*net.OpError"),
// else "error" for plain errors.New / fmt.Errorf chains, whose only
// identity is their message.
func ErrorCode(err error) string {
	var coder Coder
	if errors.As(err, &coder) {
		return coder.Code()
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if t := reflect.TypeOf(e); t != nil && !genericErrorType(t.String()) {
			return t.String()
		}
	}
	return "error"
}

func genericErrorType(name string) bool {
	switch name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return true
	}
	return false
}

// ServerDevice describes this host the way the backend describes a phone:
// Brand "server", Model = hostname, OSVersion = GOOS/GOARCH + Go version,
// AppVersion = the build version the service passes (ldflags), "dev" if
// empty. Two replicas of one service are two devices, which is what you
// want when only one of them is failing.
func ServerDevice(appVersion string) Device {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	if appVersion == "" {
		appVersion = "dev"
	}
	return Device{
		Brand:      "server",
		Model:      host,
		OSVersion:  runtime.GOOS + "/" + runtime.GOARCH + " " + runtime.Version(),
		AppVersion: appVersion,
	}
}

// callerStack renders the calling goroutine's stack from skip frames up
// (runtime frames dropped), capped so a deep recursion cannot balloon the
// payload; the backend truncates again server-side.
func callerStack(skip int) string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip+1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") {
			fmt.Fprintf(&b, "%s\n\t%s:%d\n", f.Function, f.File, f.Line)
		}
		if !more || b.Len() > 8*1024 {
			break
		}
	}
	return b.String()
}

func traceWithRequestID(ctx context.Context, stack string) string {
	if id := logging.RequestID(ctx); id != "" {
		return "request_id: " + id + "\n" + stack
	}
	return stack
}
