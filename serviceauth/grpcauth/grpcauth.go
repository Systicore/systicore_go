// Package grpcauth is serviceauth for gRPC: the server interceptors that
// admit a service per RPC, and the per-RPC credentials a client sends its
// token with. A package of its own so HTTP-only services never compile
// gRPC.
//
// Transport follows systicore_proto: plaintext HTTP/2 on the Docker
// internal network, the token in the "authorization: Bearer" metadata.
package grpcauth

import (
	"context"
	"slices"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/serviceauth"
)

// ServerOptions configure the server interceptors.
type ServerOptions struct {
	serviceauth.Options
	// Scopes maps each full method name, e.g.
	// "/systicore.core.v1.PushService/Send", to the scope it needs. A method
	// in neither Scopes nor Open is refused: an RPC added without deciding
	// who may call it stays closed.
	Scopes map[string]string
	// Open are the methods callable without a token: the health check,
	// grpc_health_v1.Health_Check_FullMethodName.
	Open []string
}

// UnaryServerInterceptor admits a call only from a service holding the
// method's scope and on the allowlist, and puts it into the context
// (serviceauth.CallerFrom). Chain it after grpclog's interceptor, so a
// refusal is logged with the request fields.
func UnaryServerInterceptor(o ServerOptions) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := o.admit(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is the same check for streaming methods. The
// Systicore RPCs are unary today; chain it anyway, or a streaming method
// registered later would run unauthenticated.
func StreamServerInterceptor(o ServerOptions) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := o.admit(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &stream{ServerStream: ss, ctx: ctx})
	}
}

type stream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stream) Context() context.Context { return s.ctx }

func (o ServerOptions) admit(ctx context.Context, method string) (context.Context, error) {
	if slices.Contains(o.Open, method) {
		return ctx, nil
	}
	l := zerolog.Ctx(ctx)
	scope, ok := o.Scopes[method]
	if !ok {
		l.Error().Str("rpc", method).Msg("no scope configured for this method; refusing")
		return ctx, status.Error(codes.PermissionDenied, "method is not open to service callers")
	}

	token, present := bearer(ctx)
	caller, outcome, err := o.Check(token, present, scope)
	switch outcome {
	case serviceauth.Unauthenticated:
		l.Warn().Err(err).Str("rpc", method).Msg("service authentication failed")
		return ctx, status.Error(codes.Unauthenticated, "missing, malformed or invalid service token")
	case serviceauth.MissingScope:
		l.Warn().Err(err).Str("rpc", method).Str("client_id", caller.ClientID).Str("required_scope", scope).Msg("service token missing required scope")
		return ctx, status.Error(codes.PermissionDenied, "missing required scope: "+scope)
	case serviceauth.NotAllowed:
		l.Warn().Err(err).Str("rpc", method).Str("client_id", caller.ClientID).Msg("service client not on the allowlist")
		return ctx, status.Error(codes.PermissionDenied, "client is not allowed to call this service")
	}

	ctx = serviceauth.WithCaller(ctx, caller)
	logging.Enrich(ctx, func(c zerolog.Context) zerolog.Context { return c.Str("client_id", caller.ClientID) })
	return ctx, nil
}

// bearer reads the token from the authorization metadata. present is true
// when any authorization value was sent, a malformed one included.
func bearer(ctx context.Context) (token string, present bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", false
	}
	scheme, rest, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", true
	}
	return strings.TrimSpace(rest), true
}

// TokenSource is what Credentials needs; *serviceauth.TokenSource is one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Credentials sends the service token with every RPC:
//
//	grpc.NewClient(addr,
//		grpc.WithTransportCredentials(insecure.NewCredentials()),
//		grpc.WithPerRPCCredentials(grpcauth.Credentials(tokens)),
//		grpc.WithChainUnaryInterceptor(grpclog.UnaryClientInterceptor()))
//
// A token that cannot be had fails the call with UNAVAILABLE, which the
// outbox callers retry. An empty token — the development bypass — sends no
// authorization at all.
func Credentials(tokens TokenSource) credentials.PerRPCCredentials {
	return perRPC{tokens: tokens}
}

type perRPC struct{ tokens TokenSource }

func (p perRPC) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "service token: %v", err)
	}
	if token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

// RequireTransportSecurity is false: the services talk plaintext HTTP/2 on
// the internal Docker network, never through Traefik.
func (perRPC) RequireTransportSecurity() bool { return false }
