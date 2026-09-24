package report

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report/reporttest"
)

type codedError struct{}

func (codedError) Error() string { return "deadlock detected" }
func (codedError) Code() string  { return "db.deadlock" }

func TestErrorTypeAndErrorCode(t *testing.T) {
	pathError := &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}
	cases := []struct {
		name     string
		err      error
		wantType string
		wantCode string
	}{
		{"typed error", pathError, "*fs.PathError", ""},
		{"typed error behind fmt wrappers", fmt.Errorf("load config: %w", fmt.Errorf("open: %w", pathError)), "*fs.PathError", ""},
		{"typed error inside errors.Join", errors.Join(errors.New("outer"), pathError), "*fs.PathError", ""},
		{"coder", codedError{}, "report.codedError", "db.deadlock"},
		{"wrapped coder", fmt.Errorf("tx: %w", codedError{}), "report.codedError", "db.deadlock"},
		{"plain chain", fmt.Errorf("ctx: %w", fmt.Errorf("inner: %w", errors.New("x"))), "error", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ErrorType(testCase.err); got != testCase.wantType {
				t.Errorf("ErrorType = %q, want %q", got, testCase.wantType)
			}
			if got := ErrorCode(testCase.err); got != testCase.wantCode {
				t.Errorf("ErrorCode = %q, want %q", got, testCase.wantCode)
			}
		})
	}
}

func TestCaptureErrorKeepsTheMessageAndTypesTheChain(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	ctx := logging.WithRequestID(context.Background(), "req-9")
	err := fmt.Errorf("sync order 8812: %w", &fs.PathError{Op: "open", Path: "/etc/x", Err: fs.ErrNotExist})

	client.CaptureError(ctx, err, "orders.sync", SeverityWarning)
	client.CaptureError(ctx, nil, "ignored", SeverityError)
	_ = client.Close(context.Background())

	if server.Count() != 1 {
		t.Fatalf("a nil error must be a no-op; sent=%d", server.Count())
	}
	request := server.Requests()[0]
	errorObject := request.Error()
	if errorObject["type"] != "*fs.PathError" || errorObject["message"] != err.Error() ||
		errorObject["action"] != "orders.sync" || errorObject["severity"] != "warning" {
		t.Errorf("captured error = %v", errorObject)
	}
	if _, hasCode := errorObject["code"]; hasCode {
		t.Errorf("no Coder in the chain, so no code: %v", errorObject)
	}
	trace := errorObject["trace"].(string)
	if !strings.HasPrefix(trace, "github.com/Systicore/systicore_go/report.TestCaptureErrorKeepsTheMessageAndTypesTheChain\n") {
		t.Errorf("the trace must start at the caller of CaptureError:\n%s", trace)
	}
	if request.Context()["requestId"] != "req-9" {
		t.Errorf("request id must travel in context.requestId: %v", request.Context())
	}
}

func TestScopeIsMarkedByCaptureAndMarkReported(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)

	if Reported(context.Background()) || Reported(nil) { //nolint:staticcheck // nil ctx must be tolerated
		t.Fatal("a plain ctx is never reported")
	}
	scoped := WithScope(context.Background())
	if WithScope(scoped) != scoped {
		t.Error("WithScope must keep an existing scope")
	}
	derived := logging.WithRequestID(scoped, "req-1")
	client.CaptureError(derived, errors.New("boom"), "job", SeverityError)
	if !Reported(scoped) || !Reported(derived) || !Captured(scoped) || !Captured(derived) {
		t.Error("CaptureError must mark the scope for every ctx sharing it")
	}

	marked := MarkReported(context.Background())
	if !Reported(marked) {
		t.Error("MarkReported without a scope must return a marked child")
	}
	fresh := WithScope(context.Background())
	if MarkReported(fresh) != fresh || !Reported(fresh) {
		t.Error("MarkReported must mark an existing scope in place")
	}
	// Silencing a log line is not a capture: an adapter asking Captured
	// must still report the failure itself.
	if Captured(marked) || Captured(fresh) || Captured(nil) { //nolint:staticcheck // nil ctx must be tolerated
		t.Error("MarkReported must not count as a capture")
	}
}
