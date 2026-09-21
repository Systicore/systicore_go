// Package grpclog is the gRPC adapter of logging: the same request-scoped
// logger and request-id propagation as logging.Middleware, as unary
// interceptors. Separate package so HTTP-only services never compile the
// gRPC dependency.
package grpclog

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Systicore/systicore_go/logging"
)

// UnaryServerInterceptor is logging.Middleware for gRPC: a request-scoped logger in
// the handler context (rpc, request id, peer) plus one line per call with
// the status code. The request id comes from the x-request-id metadata the
// calling service forwarded, or is minted here.
func UnaryServerInterceptor(root zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id := incomingRequestID(ctx)
		l := root.With().
			Str("request_id", id).
			Str("rpc", info.FullMethod).
			Logger()
		ctx = l.WithContext(logging.WithRequestID(ctx, id))

		start := time.Now()
		resp, err := handler(ctx, req)

		evt := l.Info()
		if err != nil {
			evt = l.Warn().Err(err)
		}
		evt.Str("code", status.Code(err).String()).Dur("latency", time.Since(start)).Msg("rpc")
		return resp, err
	}
}

// UnaryClientInterceptor forwards the current request id to the callee so
// one id spans a whole reports → projects → core chain in the logs.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if id := logging.RequestID(ctx); id != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, logging.RequestIDKey, id)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func incomingRequestID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(logging.RequestIDKey); len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return uuid.NewString()
}
