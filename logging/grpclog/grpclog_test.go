package grpclog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Systicore/systicore_go/logging"
)

func rootTo(buf *bytes.Buffer) zerolog.Logger {
	return zerolog.New(buf).With().Str("service", "test").Logger()
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not JSON: %q", ln)
		}
		out = append(out, m)
	}
	return out
}

func TestGRPCInterceptorsPropagateRequestID(t *testing.T) {
	var buf bytes.Buffer
	server := UnaryServerInterceptor(rootTo(&buf))
	info := &grpc.UnaryServerInfo{FullMethod: "/systicore.projects.v1.ReportSourcesService/Resolve"}

	// Incoming call that carries an id from the caller.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(logging.RequestIDKey, "req-123"))
	var seen string
	_, err := server(ctx, nil, info, func(ctx context.Context, _ any) (any, error) {
		seen = logging.RequestID(ctx)
		zerolog.Ctx(ctx).Info().Msg("in rpc")
		return nil, status.Error(codes.NotFound, "no such source")
	})
	if err == nil || seen != "req-123" {
		t.Fatalf("handler ctx request id = %q, err = %v", seen, err)
	}
	got := lines(t, &buf)
	if len(got) != 2 || got[0]["request_id"] != "req-123" || got[0]["rpc"] != info.FullMethod {
		t.Fatalf("rpc-scoped logger wrong: %v", got)
	}
	if got[1]["message"] != "rpc" || got[1]["code"] != "NotFound" || got[1]["level"] != "warn" {
		t.Errorf("call line wrong: %v", got[1])
	}

	// Without an incoming id one is minted.
	buf.Reset()
	_, _ = server(context.Background(), nil, info, func(ctx context.Context, _ any) (any, error) {
		if logging.RequestID(ctx) == "" {
			t.Error("a request id must be minted when none arrives")
		}
		return nil, nil
	})

	// Client side: the id in ctx becomes outgoing metadata.
	var forwarded string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		if v := md.Get(logging.RequestIDKey); len(v) > 0 {
			forwarded = v[0]
		}
		return nil
	}
	_ = UnaryClientInterceptor()(logging.WithRequestID(context.Background(), "req-123"), "/x", nil, nil, nil, invoker)
	if forwarded != "req-123" {
		t.Errorf("client interceptor forwarded %q, want req-123", forwarded)
	}
}
