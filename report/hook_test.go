package report

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/report/reporttest"
)

func TestHookReportsErrorLevelsOnly(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := zerolog.New(&bytes.Buffer{}).Hook(client.Hook())

	logger.Info().Msg("fine")
	logger.Warn().Msg("meh")
	logger.Log().Msg("no level")
	logger.Error().Msg("payment failed")
	_ = client.Close(context.Background())

	if server.Count() != 1 {
		t.Fatalf("the hook must forward error+ lines only, sent=%d", server.Count())
	}
	errorObject := server.Requests()[0].Error()
	if errorObject["code"] != "log.error" || errorObject["message"] != "payment failed" || errorObject["severity"] != "error" {
		t.Errorf("error line = %v", errorObject)
	}
	trace := errorObject["trace"].(string)
	if !strings.HasPrefix(trace, "github.com/Systicore/systicore_go/report.TestHookReportsErrorLevelsOnly\n") {
		t.Errorf("the trace must start at the logging call site, not inside zerolog:\n%s", trace)
	}
}

func TestHookReportsALineWithoutAMessage(t *testing.T) {
	// log.Error().Err(err).Send() is a common idiom; the hook cannot see
	// the error, but the line must not vanish.
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := zerolog.New(&bytes.Buffer{}).Hook(client.Hook())

	logger.Error().Err(errors.New("dial tcp: connection refused")).Send()
	_ = client.Close(context.Background())

	if server.Count() != 1 {
		t.Fatalf("a message-less error line must be reported, sent=%d", server.Count())
	}
	errorObject := server.Requests()[0].Error()
	if errorObject["code"] != "log.error" || errorObject["message"] != emptyLogMessage {
		t.Errorf("message-less line = %v", errorObject)
	}
	if trace := errorObject["trace"].(string); !strings.HasPrefix(trace, "github.com/Systicore/systicore_go/report.TestHookReportsALineWithoutAMessage\n") {
		t.Errorf("the call site groups the line, so the trace must start there:\n%s", trace)
	}
}

func TestHookDeliversFatalAndPanicLinesBeforeReturning(t *testing.T) {
	// zerolog exits right after the hooks of a Fatal line and panics after
	// a Panic line: a report still sitting in the queue would be lost.
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := zerolog.New(&bytes.Buffer{}).Hook(client.Hook())

	deliveredAtExit := -1
	previousExit := zerolog.FatalExitFunc
	zerolog.FatalExitFunc = func() { deliveredAtExit = server.Count() }
	defer func() { zerolog.FatalExitFunc = previousExit }()
	logger.Fatal().Msg("cannot open db")
	if deliveredAtExit != 1 {
		t.Errorf("the fatal report must be delivered before zerolog exits, delivered=%d", deliveredAtExit)
	}

	deliveredAtRecover := -1
	func() {
		defer func() {
			_ = recover()
			deliveredAtRecover = server.Count()
		}()
		logger.Panic().Msg("ledger corrupted")
	}()
	if deliveredAtRecover != 2 {
		t.Errorf("the panic report must be delivered before zerolog panics, delivered=%d", deliveredAtRecover)
	}

	for index, wantCode := range []string{"log.fatal", "log.panic"} {
		if errorObject := server.Requests()[index].Error(); errorObject["code"] != wantCode || errorObject["severity"] != "critical" {
			t.Errorf("report %d = %v, want %s / critical", index, errorObject, wantCode)
		}
	}
	if trace := server.Requests()[0].Error()["trace"].(string); !strings.HasPrefix(trace, "github.com/Systicore/systicore_go/report.TestHookDeliversFatalAndPanicLinesBeforeReturning\n") {
		t.Errorf("the trace must start at the logging call site:\n%s", trace)
	}
}

func TestHookSkipsEventsOfAReportedScope(t *testing.T) {
	server := reporttest.NewServer(t)
	client, _ := newTestClient(t, server)
	logger := zerolog.New(&bytes.Buffer{}).Hook(client.Hook())

	// CaptureError at the error site, then the usual log line: one report.
	jobContext := WithScope(logging.WithRequestID(context.Background(), "job-1"))
	err := errors.New("push failed")
	client.CaptureError(jobContext, err, "push.deliver", SeverityError)
	logger.Error().Ctx(jobContext).Err(err).Msg("push failed")

	// A logger bound to the ctx behaves the same.
	boundLogger := logger.With().Ctx(jobContext).Logger()
	boundLogger.Error().Msg("still the same job")

	// MarkReported keeps a single line out without a scope.
	logger.Error().Ctx(MarkReported(context.Background())).Msg("handled elsewhere")

	// An unmarked ctx is reported, with its request id.
	logger.Error().Ctx(logging.WithRequestID(context.Background(), "req-7")).Msg("unexpected")
	_ = client.Close(context.Background())

	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("reports = %d, want the CaptureError and the unmarked line only", len(requests))
	}
	if requests[0].Error()["action"] != "push.deliver" {
		t.Errorf("first report must be the explicit capture: %v", requests[0].Error())
	}
	if requests[1].Error()["message"] != "unexpected" || requests[1].Context()["requestId"] != "req-7" {
		t.Errorf("unmarked line = %v / %v", requests[1].Error(), requests[1].Context())
	}
}
