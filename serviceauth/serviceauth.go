// Package serviceauth is service-to-service authentication between the
// Systicore backends: client_credentials tokens the authenticator issues
// with sub "client:<id>" and a scope.
//
// The calling side gets its token from a TokenSource, cached and minted
// once for a burst of callers. The serving side checks it with Require on
// an HTTP route, or with the interceptor in serviceauth/grpcauth on a gRPC
// server: a valid service token of the pinned issuer, the scope the route
// or RPC needs, and a client id on the service's allowlist. An empty
// allowlist refuses everybody.
//
// The gRPC parts live in grpcauth so a service that only speaks HTTP never
// compiles gRPC, as with logging/grpclog.
package serviceauth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// ErrNotServiceToken is what Validate returns for a correctly signed token
// that is not a client_credentials token: a user token in particular. A
// stolen user session must never pass as a service.
var ErrNotServiceToken = errors.New("serviceauth: not a client_credentials service token")

// ErrNoKeys and ErrNoIssuer are what Validate returns when the validator
// lacks signing keys or a pinned issuer: every token is refused then.
var (
	ErrNoKeys   = errors.New("serviceauth: no signing keys loaded")
	ErrNoIssuer = errors.New("serviceauth: no issuer pinned")
)

// DevBypassClientID is the caller of a credential-less request under
// Options.DevBypass.
const DevBypassClientID = "dev-bypass"

// Caller is the authenticated service.
type Caller struct {
	ClientID string
	Scopes   []string
}

type callerKey struct{}

// WithCaller returns ctx carrying c. Require and the gRPC interceptor call
// it; tests of handlers behind them can too.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the calling service; ok is false when no service
// authentication ran on the request.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Validator checks service tokens: RS256 or ES256 signature, the pinned
// issuer, an expiry in the future, grant_type client_credentials and a
// sub of "client:<id>".
//
// The audience is deliberately not checked: the authenticator puts the
// calling client's own id there, not the service it calls, so one token
// is presented to several backends. What a token may do is bounded by
// the scope and the allowlist instead.
type Validator struct {
	keyfunc jwt.Keyfunc
	issuer  string
}

// NewValidator builds a Validator over the same keys as user tokens —
// authn.FetchJWKS — and AUTH_ISSUER_URL. A nil keyfunc or an empty issuer
// fails every token rather than the boot.
func NewValidator(keyfunc jwt.Keyfunc, issuer string) *Validator {
	return &Validator{keyfunc: keyfunc, issuer: issuer}
}

type serviceClaims struct {
	GrantType string `json:"grant_type"`
	Scope     string `json:"scope"`
	jwt.RegisteredClaims
}

// Validate returns the service a token identifies, or why it does not.
func (v *Validator) Validate(raw string) (Caller, error) {
	if v == nil || v.keyfunc == nil {
		return Caller{}, ErrNoKeys
	}
	if v.issuer == "" {
		return Caller{}, ErrNoIssuer
	}
	claims := &serviceClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, v.keyfunc,
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Caller{}, err
	}
	if claims.GrantType != "client_credentials" {
		return Caller{}, fmt.Errorf("%w: grant_type %q", ErrNotServiceToken, claims.GrantType)
	}
	clientID, ok := strings.CutPrefix(claims.Subject, "client:")
	if !ok || clientID == "" {
		return Caller{}, fmt.Errorf("%w: sub %q", ErrNotServiceToken, claims.Subject)
	}
	return Caller{ClientID: clientID, Scopes: strings.Fields(claims.Scope)}, nil
}

// Options configure the serving side, HTTP and gRPC alike.
type Options struct {
	Validator *Validator
	// AllowedClients are the client ids that may call this service
	// (SERVICE_ALLOWED_CLIENT_IDS). Empty refuses every client: the list
	// must be configured per deployment.
	AllowedClients []string
	// DevBypass lets a request with no credential at all through as
	// DevBypassClientID, so a local pipeline runs without an authenticator
	// minting tokens; a credential that is sent is still checked fully.
	// Development only.
	DevBypass bool
}

// Outcome of checking a request, shared by the HTTP and gRPC front ends.
type Outcome int

const (
	Allowed         Outcome = iota
	Unauthenticated         // no, malformed or invalid credential: 401 / UNAUTHENTICATED
	MissingScope            // valid, but without the scope: 403 / PERMISSION_DENIED
	NotAllowed              // valid, but not on the allowlist: 403 / PERMISSION_DENIED
)

// Check decides one request: token is the bearer token ("" when none was
// sent), present whether any credential was sent at all — a malformed one
// counts — and scope what the route or RPC needs. On Allowed the Caller
// is returned; otherwise err says why, for the log.
func (o Options) Check(token string, present bool, scope string) (Caller, Outcome, error) {
	if !present && o.DevBypass {
		return Caller{ClientID: DevBypassClientID}, Allowed, nil
	}
	if token == "" {
		return Caller{}, Unauthenticated, errors.New("serviceauth: missing or malformed bearer token")
	}
	caller, err := o.Validator.Validate(token)
	if err != nil {
		return Caller{}, Unauthenticated, err
	}
	if !slices.Contains(caller.Scopes, scope) {
		return caller, MissingScope, fmt.Errorf("serviceauth: client %q lacks scope %q", caller.ClientID, scope)
	}
	if !slices.Contains(o.AllowedClients, caller.ClientID) {
		return caller, NotAllowed, fmt.Errorf("serviceauth: client %q is not on the allowlist", caller.ClientID)
	}
	return caller, Allowed, nil
}
