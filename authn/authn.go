// Package authn authenticates requests to a Systicore service on chi
// (or any router taking func(http.Handler) http.Handler) against the
// Systicore authenticator: access-token verification (JWKS or a local
// key), the caller as a Principal in the context, permission resolution
// with a cache, permission checks and the double-submit CSRF guard.
//
// It is the code the four Systicore backends each kept a copy of, with
// the answers they all gave — {"error":"invalid or expired token"} and
// the rest are byte for byte what the web client keys on. Two things are
// stricter than some of the copies were: every token must carry the
// pinned issuer, and a permission lookup that fails answers 503 rather
// than a 403 that blames the user for an outage.
//
// Like httpx it imports no web framework and no service config, and the
// domain layer never imports it: handlers read the caller through
// PrincipalFrom, services get plain values.
package authn

import (
	"context"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/httpx"
)

// TokenSource says where ExtractToken found the access token, or why it
// found none.
type TokenSource int

const (
	TokenNone      TokenSource = iota // neither header nor cookie
	TokenHeader                       // a valid "Bearer <token>" header
	TokenCookie                       // the session cookie
	TokenMalformed                    // an Authorization header that is not "Bearer <token>"
)

func (s TokenSource) String() string {
	switch s {
	case TokenHeader:
		return "header"
	case TokenCookie:
		return "cookie"
	case TokenMalformed:
		return "header_malformed"
	default:
		return "none"
	}
}

// ExtractToken reads the access token from the Authorization header and,
// when there is none, from the cookieName cookie ("" skips the cookie).
//
// An Authorization header that is present but not "Bearer <token>" is
// TokenMalformed and does not fall back to the cookie: the client sent
// explicit credentials, and a mistyped header must not quietly turn into
// whatever session the browser holds.
func ExtractToken(r *http.Request, cookieName string) (string, TokenSource) {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, token, ok := strings.Cut(header, " ")
		if ok && strings.EqualFold(scheme, "bearer") {
			if token = strings.TrimSpace(token); token != "" {
				return token, TokenHeader
			}
		}
		return "", TokenMalformed
	}
	if cookieName != "" {
		if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
			return cookie.Value, TokenCookie
		}
	}
	return "", TokenNone
}

// Credential is how the caller authenticated: the token, where it came
// from and, for a cookie, under which name — so a PermissionSource can
// present it to the authenticator the same way.
type Credential struct {
	Token      string
	Source     TokenSource
	CookieName string
}

type credentialKey struct{}

func withCredential(ctx context.Context, c Credential) context.Context {
	return context.WithValue(ctx, credentialKey{}, c)
}

func credentialFrom(ctx context.Context) (Credential, bool) {
	c, ok := ctx.Value(credentialKey{}).(Credential)
	return c, ok
}

// authError writes the {"error": ...} body the auth layer of every
// Systicore service answers with, distinct from the handlers' envelope:
// the web client keys its "session expired" handling on it.
func authError(w http.ResponseWriter, status int, message string) {
	httpx.JSON(w, status, map[string]string{"error": message})
}

func logger(r *http.Request) *zerolog.Logger { return zerolog.Ctx(r.Context()) }
