package report

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
)

// CaptureError reports err as it happened at the call site: Type from the
// error chain (see ErrorType), Code from a Coder in the chain, Message the
// error text as is (the backend normalizes ids and numbers away for
// grouping), Trace the caller's stack, and the request id from ctx. Action
// is the caller's route template or operation ("POST /api/v1/orders",
// "eventpush.deliver"). A nil err is a no-op; so is a disabled client.
func (c *Client) CaptureError(ctx context.Context, err error, action string, severity Severity) {
	if err == nil {
		return
	}
	report := FromError(err)
	report.Trace = callerStack(2)
	report.Action = action
	report.Severity = severity
	c.Capture(ctx, report)
}

// FromError fills Type, Code and Message from err, for callers that
// complete the rest of the Report themselves (the Fiber adapters add the
// route, the user and the request id, and deliberately no Trace).
func FromError(err error) Report {
	return Report{
		Type:    ErrorType(err),
		Code:    ErrorCode(err),
		Message: err.Error(),
	}
}

// Coder lets an error name its own stable application code (e.g.
// "db.deadlock"), which becomes Report.Code.
type Coder interface {
	Code() string
}

// ErrorCode is the code of the first Coder in err's chain, "" without one.
func ErrorCode(err error) string {
	var coder Coder
	if errors.As(err, &coder) {
		return coder.Code()
	}
	return ""
}

// ErrorType names err's kind for grouping: the Go type of the first error
// in the chain (depth-first, through fmt.Errorf %w and errors.Join) that
// is not a generic wrapper, e.g. "*fs.PathError" or "*net.OpError"; the
// generic "error" when the chain is only errors.New / fmt.Errorf values,
// whose identity is their message.
func ErrorType(err error) string {
	if typed := firstTypedError(err); typed != nil {
		return reflect.TypeOf(typed).String()
	}
	return "error"
}

func firstTypedError(err error) error {
	if err == nil {
		return nil
	}
	if !genericErrorType(reflect.TypeOf(err).String()) {
		return err
	}
	switch wrapper := err.(type) {
	case interface{ Unwrap() error }:
		return firstTypedError(wrapper.Unwrap())
	case interface{ Unwrap() []error }:
		for _, wrapped := range wrapper.Unwrap() {
			if typed := firstTypedError(wrapped); typed != nil {
				return typed
			}
		}
	}
	return nil
}

func genericErrorType(name string) bool {
	switch name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return true
	}
	return false
}

// ServerDevice describes this service instance the way the backend
// describes a phone: Brand "server", Model GOOS/GOARCH, OSVersion the Go
// version, AppVersion the build version ("dev" if empty) and InstallID
// the stable instance id — not the hostname, which in Docker is the
// container id and would turn every redeploy into a new device.
func ServerDevice(appVersion, instanceID string) Device {
	if appVersion == "" {
		appVersion = "dev"
	}
	return Device{
		Brand:      "server",
		Model:      runtime.GOOS + "/" + runtime.GOARCH,
		OSVersion:  runtime.Version(),
		AppVersion: appVersion,
		InstallID:  instanceID,
	}
}

// maxStackBytes caps a rendered stack well under the backend's trace cap.
const maxStackBytes = 8 * 1024

// callerStack renders the calling goroutine's stack in the
// "function\n\tfile:line" shape of debug.Stack, starting skip frames above
// callerStack (1 = its caller, 2 = the caller's caller). Runtime frames are
// dropped everywhere; frames whose function starts with one of
// leadingPrefixes are dropped only while they lead, which peels a logging
// library off the top without hiding the application code under it.
func callerStack(skip int, leadingPrefixes ...string) string {
	programCounters := make([]uintptr, 48)
	count := runtime.Callers(skip+1, programCounters)
	frames := runtime.CallersFrames(programCounters[:count])
	var builder strings.Builder
	leading := true
	for {
		frame, more := frames.Next()
		if leading && hasAnyPrefix(frame.Function, leadingPrefixes) {
			if !more {
				break
			}
			continue
		}
		leading = false
		if !strings.HasPrefix(frame.Function, "runtime.") {
			fmt.Fprintf(&builder, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		}
		if !more || builder.Len() > maxStackBytes {
			break
		}
	}
	return builder.String()
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
