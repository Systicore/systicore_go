package report

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report/reporttest"
)

func TestSlogHandlerReportsErrorRecordsAndPassesEverythingOn(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	var output bytes.Buffer
	logger := slog.New(client.SlogHandler(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})))

	logger.Debug("not even logged")
	logger.Info("started")
	logger.Warn("slow query")
	requestContext := logging.WithRequestID(context.Background(), "req-3")
	logger.With("worker", "push").WithGroup("job").ErrorContext(requestContext, "sync failed",
		"attempt", 2,
		"err", fmt.Errorf("device 42: %w", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}))
	logger.Error("plain failure")
	_ = client.Close(context.Background())

	if server.Count() != 2 {
		t.Fatalf("reports = %d, want the two error records", server.Count())
	}
	withError, plain := server.Requests()[0], server.Requests()[1]
	if errorObject := withError.Error(); errorObject["type"] != "*fs.PathError" || errorObject["code"] != "log.error" ||
		errorObject["message"] != "sync failed: device 42: open /x: file does not exist" {
		t.Errorf("record with an error attribute = %v", errorObject)
	}
	if withError.Context()["requestId"] != "req-3" {
		t.Errorf("request id from the ctx: %v", withError.Context())
	}
	if trace := withError.Error()["trace"].(string); !strings.HasPrefix(trace, "github.com/Systicore/systicore_go/report.TestSlogHandlerReportsErrorRecordsAndPassesEverythingOn\n") {
		t.Errorf("the trace must start at the logging call site, not inside log/slog:\n%s", trace)
	}
	if errorObject := plain.Error(); errorObject["message"] != "plain failure" || errorObject["code"] != "log.error" {
		t.Errorf("plain record = %v", errorObject)
	}
	for _, line := range []string{"started", "slow query", "sync failed", "worker=push", "job.attempt=2", "plain failure"} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("next handler must still get %q:\n%s", line, output.String())
		}
	}
	if strings.Contains(output.String(), "not even logged") {
		t.Error("next handler's level must still apply")
	}
}

func TestSlogHandlerUsesCoderCodeAndSkipsReportedScopes(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := slog.New(client.SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	scoped := WithScope(context.Background())
	client.CaptureError(scoped, codedError{}, "tx.commit", SeverityError)
	logger.ErrorContext(scoped, "commit failed", "err", codedError{})
	logger.ErrorContext(context.Background(), "commit failed", "err", codedError{})
	_ = client.Close(context.Background())

	if server.Count() != 2 {
		t.Fatalf("reports = %d, want the capture and the unmarked record", server.Count())
	}
	if code := server.Requests()[1].Error()["code"]; code != "db.deadlock" {
		t.Errorf("a Coder's code must win over log.error, got %v", code)
	}
}

func TestSlogHandlerReportsRecordsWithoutAMessage(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := slog.New(client.SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	logger.Error("", "err", codedError{})
	logger.Error("")
	_ = client.Close(context.Background())

	if server.Count() != 2 {
		t.Fatalf("reports = %d, want both message-less records", server.Count())
	}
	if message := server.Requests()[0].Error()["message"]; message != "deadlock detected" {
		t.Errorf("a record with only an error must carry the error text, got %q", message)
	}
	if message := server.Requests()[1].Error()["message"]; message != emptyLogMessage {
		t.Errorf("a bare record must carry the placeholder, got %q", message)
	}
}

func TestSlogHandlerFindsAnErrorBoundWithWith(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := slog.New(client.SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	pathError := &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}

	jobLogger := logger.With("job", "sync", "err", pathError).WithGroup("run").With("attempt", 2)
	jobLogger.Error("sync failed")
	jobLogger.Error("sync failed again", "err", codedError{})
	_ = client.Close(context.Background())

	if server.Count() != 2 {
		t.Fatalf("reports = %d, want 2", server.Count())
	}
	if errorObject := server.Requests()[0].Error(); errorObject["type"] != "*fs.PathError" ||
		errorObject["message"] != "sync failed: open /x: file does not exist" {
		t.Errorf("an error bound with With must type the report: %v", errorObject)
	}
	if errorObject := server.Requests()[1].Error(); errorObject["type"] != "report.codedError" || errorObject["code"] != "db.deadlock" {
		t.Errorf("the record's own error must win over the bound one: %v", errorObject)
	}
}
