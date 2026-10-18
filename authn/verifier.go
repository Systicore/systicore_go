package authn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
)

// ErrNoKeys is what Verify returns when the verifier has no signing keys:
// a development server that booted without its JWKS answers 401 instead
// of panicking.
var ErrNoKeys = errors.New("authn: no signing keys loaded")

// ErrNoIssuer is what Verify returns when no issuer is pinned: an
// unpinned verifier would accept a token of any issuer whose key it
// happens to hold.
var ErrNoIssuer = errors.New("authn: no issuer pinned")

// Claims are the access-token claims the Systicore authenticator issues
// (domain/oauth/services/oauth.service.go). The username travels as
// preferred_username and only with the profile scope; a client
// credentials token has sub "client:<id>" and a scope instead of a user.
type Claims struct {
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Scope             string `json:"scope"`
	ClientID          string `json:"client_id"`
	// Permissions is the permissions claim the authenticator puts into
	// the access tokens of an OAuth client that has permission prefixes
	// configured. First-party session tokens never carry it.
	Permissions PermissionsClaim `json:"permissions,omitzero"`
	jwt.RegisteredClaims
}

// PermissionsClaim is the token's permissions claim: the user's
// permissions within the issuing client's namespaces, as full permission
// UIDs ("systicore.reports.read"). Present tells "no claim" (resolve
// through the PermissionSource) from "an empty claim" (the user holds
// none of the client's permissions; nothing else is asked).
type PermissionsClaim struct {
	Present bool
	Values  []string
}

// ErrMalformedPermissions is why a token whose permissions claim is not
// a JSON array of non-empty strings fails verification. Such a token is
// invalid as a whole (401), not "a token without permissions": a claim
// the authenticator did not write that way is not one to guess about.
var ErrMalformedPermissions = errors.New("authn: permissions claim is not an array of non-empty strings")

// UnmarshalJSON accepts an array of non-empty strings, and nothing else:
// null, a string, an object, a number or a null or empty element is
// ErrMalformedPermissions.
func (p *PermissionsClaim) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) || json.Unmarshal(b, &raw) != nil {
		return ErrMalformedPermissions
	}
	values := make([]string, 0, len(raw))
	for _, element := range raw {
		var v string
		if bytes.Equal(bytes.TrimSpace(element), []byte("null")) || json.Unmarshal(element, &v) != nil || v == "" {
			return ErrMalformedPermissions
		}
		values = append(values, v)
	}
	*p = PermissionsClaim{Present: true, Values: values}
	return nil
}

// IsZero reports an absent claim, so omitzero leaves it out of a token.
func (p PermissionsClaim) IsZero() bool { return !p.Present }

// MarshalJSON writes the claim as the array it came as; an absent one,
// which omitzero leaves out of Claims, as null.
func (p PermissionsClaim) MarshalJSON() ([]byte, error) {
	if !p.Present {
		return []byte("null"), nil
	}
	return json.Marshal(p.Values)
}

// VerifierOptions configure a Verifier.
type VerifierOptions struct {
	// Keyfunc resolves the signing key: FetchJWKS on a resource server, the
	// authenticator's own public key on the authenticator. Nil fails every
	// token with ErrNoKeys.
	Keyfunc jwt.Keyfunc
	// Issuer is the iss every token must carry, AUTH_ISSUER_URL. Required.
	Issuer string
	// Audience, when set, must be among the token's aud. The authenticator
	// puts the OAuth client id there, so a service pins it only when it
	// accepts the tokens of one client.
	Audience string
}

// Verifier checks access tokens: RS256 or ES256 signature, the pinned
// issuer, the audience when one is set, and an expiry in the future.
type Verifier struct {
	keyfunc jwt.Keyfunc
	options []jwt.ParserOption
	issuer  string
}

// NewVerifier builds a Verifier. It never fails; a missing Keyfunc or
// Issuer surfaces on every Verify, so a server that booted without them
// rejects tokens rather than refusing to start in development.
func NewVerifier(o VerifierOptions) *Verifier {
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(o.Issuer),
		jwt.WithExpirationRequired(),
	}
	if o.Audience != "" {
		options = append(options, jwt.WithAudience(o.Audience))
	}
	return &Verifier{keyfunc: o.Keyfunc, options: options, issuer: o.Issuer}
}

// Verify returns the claims of a valid token, or why it is not one.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	if v == nil || v.keyfunc == nil {
		return nil, ErrNoKeys
	}
	if v.issuer == "" {
		return nil, ErrNoIssuer
	}
	claims := &Claims{}
	if _, err := jwt.ParseWithClaims(raw, claims, v.keyfunc, v.options...); err != nil {
		return nil, err
	}
	return claims, nil
}

// FetchJWKS loads a JWK Set — the authenticator serves it at
// {AUTH_ISSUER_URL}/api/v1/oauth/jwks — and keeps it fresh: hourly, and
// at most every five minutes when a token names a key id it lacks, which
// is how a rotated key arrives.
//
// ctx is the server's lifetime: it ends the refresh, so pass the context
// main cancels on shutdown. A boot timeout there would stop the refresh
// seconds after start and keep a withdrawn key trusted until the next
// restart. The first fetch, and each refresh, is bounded by timeout
// instead. Refresh failures are logged on zerolog.Ctx(ctx).
func FetchJWKS(ctx context.Context, url string, timeout time.Duration) (jwt.Keyfunc, error) {
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{url}, keyfunc.Override{
		HTTPTimeout: timeout,
		RefreshErrorHandlerFunc: func(u string) func(context.Context, error) {
			return func(ctx context.Context, err error) {
				zerolog.Ctx(ctx).Warn().Err(err).Str("url", u).Msg("JWKS refresh failed; keeping the keys already loaded")
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("authn: load JWKS from %s: %w", url, err)
	}
	return k.Keyfunc, nil
}
