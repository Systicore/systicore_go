package authn

import (
	"context"
	"slices"
	"time"
)

// Principal is the authenticated user, as Authenticate established it.
type Principal struct {
	UserID   int64
	Email    string
	Username string
	Name     string
	// TokenExp is the access token's expiry; the permission cache never
	// outlives it. Zero when unknown.
	TokenExp time.Time
	// TokenPermissions is the access token's permissions claim. An OAuth
	// client's token carries it, scoped to that client's namespaces;
	// ResolvePermissions then takes it as the caller's whole permission
	// set. First-party session tokens never carry it (Present false).
	TokenPermissions PermissionsClaim
	// DevFallback marks the development user Authenticate installs for a
	// request without a token (AuthOptions.DevFallback).
	DevFallback bool
}

type principalKey struct{}

type permissionsKey struct{}

// permissionsUnavailableKey marks a request whose permission lookup
// failed, so RequirePermission answers 503 instead of a false 403.
type permissionsUnavailableKey struct{}

// WithPrincipal returns ctx carrying p. Authenticate calls it; tests of
// handlers behind it can too.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller; ok is false on a request Authenticate
// did not run on (a public route, or a middleware-order mistake).
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// UserIDFrom is the caller's id, 0 when there is none.
func UserIDFrom(ctx context.Context) int64 {
	p, _ := PrincipalFrom(ctx)
	return p.UserID
}

// WithPermissions returns ctx carrying the caller's permission set.
// ResolvePermissions calls it; tests of handlers behind it can too.
func WithPermissions(ctx context.Context, permissions []string) context.Context {
	return context.WithValue(ctx, permissionsKey{}, permissions)
}

// PermissionsFrom returns the caller's permission set; ok is false when
// ResolvePermissions did not run on the request.
func PermissionsFrom(ctx context.Context) ([]string, bool) {
	p, ok := ctx.Value(permissionsKey{}).([]string)
	return p, ok
}

// HasPermission reports whether the caller holds permission — for a
// handler whose answer depends on it beyond a route-level check.
func HasPermission(ctx context.Context, permission string) bool {
	permissions, _ := PermissionsFrom(ctx)
	return slices.Contains(permissions, permission)
}
