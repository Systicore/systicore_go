package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/Systicore/systicore_go/httpx"
)

// PermissionSource resolves the permission set of an authenticated user.
// UserInfo asks the authenticator over HTTP; the authenticator itself
// plugs in its own store with PermissionSourceFunc.
type PermissionSource interface {
	Permissions(ctx context.Context, user Principal, credential Credential) ([]string, error)
}

// PermissionSourceFunc adapts a function to PermissionSource.
type PermissionSourceFunc func(ctx context.Context, user Principal, credential Credential) ([]string, error)

// Permissions calls f.
func (f PermissionSourceFunc) Permissions(ctx context.Context, user Principal, credential Credential) ([]string, error) {
	return f(ctx, user, credential)
}

// UserInfo resolves permissions from the authenticator's
// GET {authURL}/api/v1/user, presenting the caller's credential the way
// it came: as a Bearer header, or as the session cookie under its name.
// A nil client gets a 5-second timeout.
func UserInfo(authURL string, client *http.Client) PermissionSource {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	endpoint := strings.TrimRight(authURL, "/") + "/api/v1/user"
	return PermissionSourceFunc(func(ctx context.Context, _ Principal, c Credential) ([]string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("authn: build userinfo request: %w", err)
		}
		if c.Source == TokenCookie {
			req.AddCookie(&http.Cookie{Name: c.CookieName, Value: c.Token})
		} else {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("authn: userinfo: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("authn: userinfo answered %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("authn: read userinfo: %w", err)
		}
		var envelope struct {
			Data struct {
				Permissions []string `json:"permissions"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("authn: parse userinfo: %w", err)
		}
		if envelope.Data.Permissions == nil {
			return []string{}, nil
		}
		return envelope.Data.Permissions, nil
	})
}

// Cache is the part of a service's cache ResolvePermissions uses; the
// Systicore backends' cache.ICache satisfies it as is.
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
}

// PermissionOptions configure ResolvePermissions.
type PermissionOptions struct {
	Source PermissionSource
	// Cache, when set, keeps each user's set under user_perms:<id> for
	// MaxTTL, but never past the token's expiry minus Buffer, so a set
	// does not outlive the session it was read for. Nil asks Source on
	// every request — right for the authenticator, whose store is cached
	// already and invalidated on every RBAC change.
	Cache  Cache
	MaxTTL time.Duration // 0 means 5 minutes
	Buffer time.Duration // 0 means 30 seconds
}

const permissionsCachePrefix = "user_perms:"

// ResolvePermissions loads the caller's permission set into the context,
// for RequirePermission and HasPermission. Mount it after Authenticate.
// Concurrent requests of one user share a single lookup.
//
// A lookup that fails leaves the set empty and marks the request, and
// RequirePermission then answers 503: an outage of the authenticator is
// no reason to tell the user they lack a permission.
func ResolvePermissions(o PermissionOptions) httpx.Middleware {
	maxTTL, buffer := o.MaxTTL, o.Buffer
	if maxTTL == 0 {
		maxTTL = 5 * time.Minute
	}
	if buffer == 0 {
		buffer = 30 * time.Second
	}
	var flights singleflight.Group

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := logger(r)
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				l.Error().Str("middleware", "authn.ResolvePermissions").Msg("principal missing from context; mount Authenticate first")
				authError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if p.DevFallback { // Authenticate already seeded the empty set
				next.ServeHTTP(w, r)
				return
			}
			credential, _ := credentialFrom(r.Context())
			key := permissionsCachePrefix + strconv.FormatInt(p.UserID, 10)

			permissions, cached := cachedPermissions(r.Context(), o.Cache, key)
			if !cached {
				// The lookup outlives a caller that gives up, so the others
				// sharing it are not failed by that one's cancellation.
				lookupCtx := context.WithoutCancel(r.Context())
				v, err, _ := flights.Do(key, func() (any, error) {
					if again, ok := cachedPermissions(lookupCtx, o.Cache, key); ok {
						return again, nil
					}
					fetched, err := o.Source.Permissions(lookupCtx, p, credential)
					if err != nil {
						return nil, err
					}
					storePermissions(lookupCtx, o.Cache, key, fetched, cacheTTL(p.TokenExp, maxTTL, buffer))
					return fetched, nil
				})
				if err != nil {
					l.Error().Err(err).Str("middleware", "authn.ResolvePermissions").Msg("permission lookup failed; RequirePermission answers 503")
					ctx := context.WithValue(WithPermissions(r.Context(), []string{}), permissionsUnavailableKey{}, true)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				permissions = v.([]string)
			}
			next.ServeHTTP(w, r.WithContext(WithPermissions(r.Context(), permissions)))
		})
	}
}

func cachedPermissions(ctx context.Context, c Cache, key string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	raw, err := c.Get(ctx, key)
	if err != nil || raw == "" {
		return nil, false
	}
	var permissions []string
	if json.Unmarshal([]byte(raw), &permissions) != nil {
		return nil, false
	}
	return permissions, true
}

func storePermissions(ctx context.Context, c Cache, key string, permissions []string, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	raw, err := json.Marshal(permissions)
	if err != nil {
		return
	}
	if err := c.Set(ctx, key, string(raw), ttl); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("middleware", "authn.ResolvePermissions").Msg("failed to cache permissions")
	}
}

// cacheTTL is how long a permission set may be cached: maxTTL, but never
// past the token's expiry minus buffer; 0, do not cache, when that point
// has passed or the expiry is unknown.
func cacheTTL(tokenExp time.Time, maxTTL, buffer time.Duration) time.Duration {
	if tokenExp.IsZero() {
		return 0
	}
	remaining := time.Until(tokenExp) - buffer
	if remaining <= 0 {
		return 0
	}
	return min(remaining, maxTTL)
}

// RequirePermission lets the request through only when the caller holds
// permission; otherwise 403 {"status":"error","message":"missing
// permission: <permission>"}. Mount it after ResolvePermissions. When the
// lookup failed it answers 503, and 500 when ResolvePermissions never ran.
func RequirePermission(permission string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := logger(r)
			if unavailable, _ := r.Context().Value(permissionsUnavailableKey{}).(bool); unavailable {
				l.Warn().Str("required", permission).Msg("permissions unavailable; refusing rather than answering a false 403")
				httpx.Error(w, http.StatusServiceUnavailable, "Permission service temporarily unavailable. Please retry shortly.")
				return
			}
			permissions, ok := PermissionsFrom(r.Context())
			if !ok {
				l.Error().Str("required", permission).Msg("no permissions in context; mount ResolvePermissions first")
				httpx.Error(w, http.StatusInternalServerError, "no permissions found")
				return
			}
			for _, held := range permissions {
				if held == permission {
					next.ServeHTTP(w, r)
					return
				}
			}
			l.Warn().Str("required", permission).Msg("missing permission")
			httpx.Error(w, http.StatusForbidden, "missing permission: "+permission)
		})
	}
}

// DevelopmentPermission grants permission to everyone and logs that it
// did. Development only, in place of RequirePermission.
func DevelopmentPermission(permission string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger(r).Warn().Str("permission", permission).Msg("development mode: permission granted without a check")
			next.ServeHTTP(w, r)
		})
	}
}
