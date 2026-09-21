package logging

import (
	"context"

	"github.com/rs/zerolog"
)

// RequestIDKey is the metadata / header name the request id travels under
// between services.
const RequestIDKey = "x-request-id"

type requestIDCtxKey struct{}

// WithRequestID returns ctx carrying id, so a downstream gRPC call can
// forward it (see UnaryClientInterceptor).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// RequestID returns the id set by Middleware / UnaryServerInterceptor, or
// "" outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDCtxKey{}).(string)
	return id
}

// FromContext is zerolog.Ctx for callers who prefer a value: the
// request-scoped logger when inside a request, the root logger otherwise.
func FromContext(ctx context.Context) zerolog.Logger {
	return *zerolog.Ctx(ctx)
}

// Enrich adds fields to the logger already in ctx, in place — every later
// layer of the same request sees them. Typical use: the auth middleware
// stamping user_id once the token is validated.
func Enrich(ctx context.Context, fn func(zerolog.Context) zerolog.Context) {
	zerolog.Ctx(ctx).UpdateContext(fn)
}
