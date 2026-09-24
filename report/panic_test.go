package report

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/report/reporttest"
)

func panicWithString() {
	panic("inventory drifted")
}

func panicWithError() {
	panic(&fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist})
}

func panicWithIndex(values []int, index int) int {
	return values[index]
}

func runGuarded(ctx context.Context, client *Client, work func()) {
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		defer Recover(ctx, client, "inventory.sync")
		work()
	}()
	group.Wait()
}

func TestRecoverCapturesThePanicSite(t *testing.T) {
	cases := []struct {
		name      string
		work      func()
		wantType  string
		wantFrame string
		wantText  string
	}{
		{"string value", panicWithString, "panic", "report.panicWithString", "inventory drifted"},
		{"error value", panicWithError, "*fs.PathError", "report.panicWithError", "open /x: file does not exist"},
		{"runtime error", func() { panicWithIndex(nil, 3) }, "runtime.boundsError", "report.panicWithIndex", "index out of range"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			captureFallbackOutput(t)
			server := reporttest.NewServer(t)
			client, _ := newTestClient(t, server)
			runGuarded(context.Background(), client, testCase.work)
			_ = client.Close(context.Background())

			if server.Count() != 1 {
				t.Fatalf("reports = %d, want 1", server.Count())
			}
			errorObject := server.Requests()[0].Error()
			if errorObject["type"] != testCase.wantType || errorObject["code"] != "panic" ||
				errorObject["severity"] != "critical" || errorObject["action"] != "inventory.sync" ||
				!strings.Contains(errorObject["message"].(string), testCase.wantText) {
				t.Errorf("panic report = %v", errorObject)
			}
			trace := errorObject["trace"].(string)
			lines := strings.Split(trace, "\n")
			if len(lines) < 2 || !strings.HasPrefix(lines[0], "goroutine ") || !strings.Contains(lines[1], testCase.wantFrame) {
				t.Errorf("the trace must start at the panicking function %s:\n%s", testCase.wantFrame, trace)
			}
			if strings.Contains(trace, "runtime/debug.Stack") || strings.Contains(trace, "report.Recover") {
				t.Errorf("the recover machinery must be trimmed off the trace:\n%s", trace)
			}
		})
	}
}

func TestRecoverLogsWithoutReportingTwice(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	var logBuffer bytes.Buffer
	logger := zerolog.New(&logBuffer).Hook(client.Hook())
	ctx := logger.WithContext(context.Background())

	runGuarded(ctx, client, panicWithString)
	_ = client.Close(context.Background())

	if !strings.Contains(logBuffer.String(), "recovered panic") || !strings.Contains(logBuffer.String(), "panicWithString") {
		t.Errorf("the panic must be logged with its stack: %s", logBuffer.String())
	}
	if server.Count() != 1 {
		t.Errorf("the hook must skip the recover log line; reports = %d", server.Count())
	}
}

func TestRecoverWithoutPanicDoesNothing(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	runGuarded(context.Background(), client, func() {})
	_ = client.Close(context.Background())
	if server.Count() != 0 {
		t.Errorf("reports = %d, want 0", server.Count())
	}
}

func TestPanicSiteStack(t *testing.T) {
	stack := strings.Join([]string{
		"goroutine 7 [running]:",
		"runtime/debug.Stack()",
		"\t/usr/local/go/src/runtime/debug/stack.go:26 +0x5e",
		"github.com/Systicore/systicore_go/report.Recover({0x0, 0x0}, 0x0, {0x0, 0x0})",
		"\t/src/report/panic.go:80 +0x45",
		"panic({0x4a1e0?, 0x4f4f68?})",
		"\t/usr/local/go/src/runtime/panic.go:785 +0x132",
		"runtime.goPanicIndex(0x3, 0x0)",
		"\t/usr/local/go/src/runtime/panic.go:115 +0x74",
		"main.lookup(...)",
		"\t/app/main.go:14",
		"main.main()",
		"\t/app/main.go:20 +0x45",
		"",
	}, "\n")
	want := "goroutine 7 [running]:\nmain.lookup(...)\n\t/app/main.go:14\nmain.main()\n\t/app/main.go:20 +0x45\n"
	if got := panicSiteStack([]byte(stack)); got != want {
		t.Errorf("panicSiteStack =\n%s\nwant\n%s", got, want)
	}
	withoutPanic := "goroutine 1 [running]:\nmain.main()\n\t/app/main.go:3\n"
	if got := panicSiteStack([]byte(withoutPanic)); got != withoutPanic {
		t.Errorf("a stack without panic( must be unchanged, got\n%s", got)
	}
}

// captureFallbackOutput redirects Recover's stderr fallback into a buffer
// for the rest of the test.
func captureFallbackOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := panicFallbackOutput
	panicFallbackOutput = &output
	t.Cleanup(func() { panicFallbackOutput = previous })
	return &output
}

func TestRecoverLogsToStderrWhenTheContextHasNoLogger(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"plain ctx":       context.Background(),
		"Nop logger only": zerolog.Nop().WithContext(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			output := captureFallbackOutput(t)
			runGuarded(ctx, nil, panicWithString)
			if !strings.Contains(output.String(), "recovered panic") || !strings.Contains(output.String(), "panicWithString") {
				t.Errorf("a swallowed panic must still be logged with its stack: %q", output.String())
			}
		})
	}
	t.Run("ctx logger wins", func(t *testing.T) {
		output := captureFallbackOutput(t)
		var contextOutput bytes.Buffer
		runGuarded(zerolog.New(&contextOutput).WithContext(context.Background()), nil, panicWithString)
		if output.Len() != 0 || !strings.Contains(contextOutput.String(), "recovered panic") {
			t.Errorf("the ctx logger must get the line: ctx=%q fallback=%q", contextOutput.String(), output.String())
		}
	})
}
