package grpcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Systicore/systicore_go/serviceauth"
)

const (
	issuer = "https://auth.systicore.test"
	send   = "/systicore.core.v1.PushService/Send"
)

var key = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func token(t *testing.T, sub, scope string) string {
	t.Helper()
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": sub, "iss": issuer, "scope": scope, "grant_type": "client_credentials",
		"exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func options(devBypass bool) ServerOptions {
	return ServerOptions{
		Options: serviceauth.Options{
			Validator:      serviceauth.NewValidator(func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, issuer),
			AllowedClients: []string{"projects"},
			DevBypass:      devBypass,
		},
		Scopes: map[string]string{send: "svc.push.send"},
		Open:   []string{grpc_health_v1.Health_Check_FullMethodName},
	}
}

func withAuth(value string) context.Context {
	ctx := context.Background()
	if value == "" {
		return ctx
	}
	return metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", value))
}

func TestUnaryServerInterceptor(t *testing.T) {
	cases := []struct {
		name   string
		o      ServerOptions
		method string
		auth   string
		code   codes.Code
		caller string
	}{
		{"valid", options(false), send, "Bearer " + token(t, "client:projects", "svc.push.send"), codes.OK, "projects"},
		{"scheme in any case", options(false), send, "bearer " + token(t, "client:projects", "svc.push.send"), codes.OK, "projects"},
		{"health check is open", options(false), grpc_health_v1.Health_Check_FullMethodName, "", codes.OK, ""},
		// An RPC nobody gave a scope stays closed, even to a valid caller.
		{"unconfigured method", options(false), "/systicore.core.v1.PushService/Other", "Bearer " + token(t, "client:projects", "svc.push.send"), codes.PermissionDenied, ""},
		// The dev bypass admits before any scope is looked at, so only the
		// explicit refusal keeps an unconfigured method closed there.
		{"unconfigured method under dev bypass", options(true), "/systicore.core.v1.PushService/Other", "", codes.PermissionDenied, ""},
		{"no token", options(false), send, "", codes.Unauthenticated, ""},
		{"malformed", options(false), send, "Basic dTpw", codes.Unauthenticated, ""},
		{"user token", options(false), send, "Bearer " + token(t, "42", "svc.push.send"), codes.Unauthenticated, ""},
		{"missing scope", options(false), send, "Bearer " + token(t, "client:projects", "svc.health.read"), codes.PermissionDenied, ""},
		{"not on the allowlist", options(false), send, "Bearer " + token(t, "client:reports", "svc.push.send"), codes.PermissionDenied, ""},
		{"dev bypass without a credential", options(true), send, "", codes.OK, serviceauth.DevBypassClientID},
		{"dev bypass still checks a sent token", options(true), send, "Bearer forged", codes.Unauthenticated, ""},
	}
	for _, tc := range cases {
		var got serviceauth.Caller
		handler := func(ctx context.Context, _ any) (any, error) {
			got, _ = serviceauth.CallerFrom(ctx)
			return "ok", nil
		}
		_, err := UnaryServerInterceptor(tc.o)(withAuth(tc.auth), nil, &grpc.UnaryServerInfo{FullMethod: tc.method}, handler)
		if status.Code(err) != tc.code || got.ClientID != tc.caller {
			t.Errorf("%s: code %s caller %q, want %s %q (%v)", tc.name, status.Code(err), got.ClientID, tc.code, tc.caller, err)
		}
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeStream) Context() context.Context { return s.ctx }

func TestStreamServerInterceptor(t *testing.T) {
	var got serviceauth.Caller
	handler := func(_ any, ss grpc.ServerStream) error {
		got, _ = serviceauth.CallerFrom(ss.Context())
		return nil
	}
	interceptor := StreamServerInterceptor(options(false))

	ok := fakeStream{ctx: withAuth("Bearer " + token(t, "client:projects", "svc.push.send"))}
	if err := interceptor(nil, ok, &grpc.StreamServerInfo{FullMethod: send}, handler); err != nil || got.ClientID != "projects" {
		t.Errorf("valid stream: %v, caller %q", err, got.ClientID)
	}
	anonymous := fakeStream{ctx: context.Background()}
	if err := interceptor(nil, anonymous, &grpc.StreamServerInfo{FullMethod: send}, handler); status.Code(err) != codes.Unauthenticated {
		t.Errorf("anonymous stream: %v", err)
	}
}

type fixedSource struct {
	token string
	err   error
}

func (s fixedSource) Token(context.Context) (string, error) { return s.token, s.err }

func TestCredentials(t *testing.T) {
	md, err := Credentials(fixedSource{token: "abc"}).GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer abc" {
		t.Errorf("token: %v %v", md, err)
	}
	md, err = Credentials(fixedSource{}).GetRequestMetadata(context.Background())
	if err != nil || len(md) != 0 {
		t.Errorf("dev bypass sends nothing: %v %v", md, err)
	}
	_, err = Credentials(fixedSource{err: errors.New("authenticator down")}).GetRequestMetadata(context.Background())
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a token that cannot be had must be UNAVAILABLE, so outbox callers retry: %v", err)
	}
	if Credentials(fixedSource{}).RequireTransportSecurity() {
		t.Error("the internal network is plaintext")
	}
}
