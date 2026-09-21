package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rootTo builds a root logger that writes JSON lines into buf.
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

func TestNewWritesFileAndFallsBackOnBadLevel(t *testing.T) {
	dir := t.TempDir()
	root, closeFn, err := New(Options{Service: "svc", Level: "nope", File: &FileOptions{Dir: dir, MaxSizeMB: 1}})
	if err != nil {
		t.Fatal(err)
	}
	root.Info().Msg("hello")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "svc-app.log"))
	if len(matches) != 1 {
		t.Fatalf("log file not created in %s", dir)
	}
	if root.GetLevel() != zerolog.InfoLevel {
		t.Errorf("level = %s, want info fallback", root.GetLevel())
	}
	// Outside any request, zerolog.Ctx yields the root logger, not a Nop.
	if zerolog.Ctx(context.Background()).GetLevel() == zerolog.Disabled {
		t.Error("DefaultContextLogger must be the root logger")
	}
}

func TestMiddlewareScopesLoggerAndLogsAccessLine(t *testing.T) {
	var buf bytes.Buffer
	r := chi.NewRouter()
	r.Use(middleware.RequestID, Middleware(rootTo(&buf)))
	r.Get("/reports/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Domain code: only zerolog.Ctx, nothing from this package.
		zerolog.Ctx(r.Context()).Warn().Msg("from handler")
		Enrich(r.Context(), func(c zerolog.Context) zerolog.Context { return c.Int64("user_id", 42) })
		zerolog.Ctx(r.Context()).Info().Msg("after enrich")
		if RequestID(r.Context()) == "" {
			t.Error("request id must be in the context")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/reports/7", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := lines(t, &buf)
	if len(got) != 3 {
		t.Fatalf("want 3 lines (handler, enriched, access), got %d: %s", len(got), buf.String())
	}
	handlerLine, enriched, access := got[0], got[1], got[2]
	if handlerLine["message"] != "from handler" || handlerLine["method"] != "GET" || handlerLine["path"] != "/reports/7" || handlerLine["ip"] != "10.0.0.9:1234" || handlerLine["request_id"] == "" {
		t.Errorf("handler line lacks request fields: %v", handlerLine)
	}
	if _, present := handlerLine["user_id"]; present {
		t.Error("user_id must not exist before Enrich")
	}
	if enriched["user_id"] != float64(42) {
		t.Errorf("Enrich must add user_id in place: %v", enriched)
	}
	if access["message"] != "request" || access["status"] != float64(201) || access["bytes"] != float64(2) || access["service"] != "test" {
		t.Errorf("access line wrong: %v", access)
	}
	if access["request_id"] != handlerLine["request_id"] {
		t.Error("all lines of one request share the request id")
	}
}

func TestGRPCInterceptorsPropagateRequestID(t *testing.T) {
	var buf bytes.Buffer
	server := UnaryServerInterceptor(rootTo(&buf))
	info := &grpc.UnaryServerInfo{FullMethod: "/systicore.projects.v1.ReportSourcesService/Resolve"}

	// Incoming call that carries an id from the caller.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(RequestIDKey, "req-123"))
	var seen string
	_, err := server(ctx, nil, info, func(ctx context.Context, _ any) (any, error) {
		seen = RequestID(ctx)
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
		if RequestID(ctx) == "" {
			t.Error("a request id must be minted when none arrives")
		}
		return nil, nil
	})

	// Client side: the id in ctx becomes outgoing metadata.
	var forwarded string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		if v := md.Get(RequestIDKey); len(v) > 0 {
			forwarded = v[0]
		}
		return nil
	}
	_ = UnaryClientInterceptor()(WithRequestID(context.Background(), "req-123"), "/x", nil, nil, nil, invoker)
	if forwarded != "req-123" {
		t.Errorf("client interceptor forwarded %q, want req-123", forwarded)
	}
}
