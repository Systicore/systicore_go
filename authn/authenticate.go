package authn

import (
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/httpx"
	"github.com/Systicore/systicore_go/logging"
)

// devUser is the caller of a token-less request under DevFallback.
func devUser() Principal {
	return Principal{
		UserID:      1,
		Email:       "dev@localhost",
		Username:    "dev",
		Name:        "Development User",
		TokenExp:    time.Now().Add(time.Hour),
		DevFallback: true,
	}
}

// AuthOptions configure Authenticate.
type AuthOptions struct {
	Verifier *Verifier
	// CookieName is the session cookie that may carry the access token
	// (AUTH_SESSION_COOKIE_NAME, typically systicore_session); "" reads
	// the Authorization header only.
	CookieName string
	// DevFallback runs a request that sends no token at all as the
	// development user (id 1, no permissions). A token that is sent is
	// still verified. Development only: main sets it from the
	// environment, never true in production.
	DevFallback bool
}

// Authenticate requires a valid access token and installs its user as the
// Principal, readable through PrincipalFrom; the request logger gets
// user_id for every later line. Without a valid token it answers 401.
// A token whose subject is not a user id — a client credentials token,
// sub "client:<id>" — is refused here: service routes use serviceauth.
func Authenticate(o AuthOptions) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := logger(r)
			token, source := ExtractToken(r, o.CookieName)
			switch source {
			case TokenMalformed:
				l.Warn().Str("middleware", "authn").Msg("malformed Authorization header")
				authError(w, http.StatusUnauthorized, "malformed Authorization header (expected 'Bearer <token>')")
				return
			case TokenNone:
				if o.DevFallback {
					l.Debug().Str("middleware", "authn").Msg("no token provided, using dev user fallback")
					serveAs(next, w, r.WithContext(WithPermissions(r.Context(), []string{})), devUser())
					return
				}
				l.Warn().Str("middleware", "authn").Msg("missing authorization (no header or cookie)")
				authError(w, http.StatusUnauthorized, "missing authorization (header or cookie)")
				return
			}

			claims, err := o.Verifier.Verify(token)
			if err != nil {
				l.Warn().Str("middleware", "authn").Str("token_source", source.String()).Err(err).Msg("token validation failed")
				authError(w, http.StatusUnauthorized, "invalid or expired token")
				return
			}
			userID, err := strconv.ParseInt(claims.Subject, 10, 64)
			if err != nil {
				l.Warn().Str("middleware", "authn").Str("sub", claims.Subject).Msg("token subject is not a user id")
				authError(w, http.StatusUnauthorized, "invalid user id in token")
				return
			}

			p := Principal{UserID: userID, Email: claims.Email, Username: claims.PreferredUsername, Name: claims.Name, TokenPermissions: claims.Permissions}
			if claims.ExpiresAt != nil {
				p.TokenExp = claims.ExpiresAt.Time
			}
			ctx := withCredential(r.Context(), Credential{Token: token, Source: source, CookieName: o.CookieName})
			l.Debug().Str("middleware", "authn").Int64("user_id", userID).Str("token_source", source.String()).Msg("authenticated")
			serveAs(next, w, r.WithContext(ctx), p)
		})
	}
}

// serveAs continues the chain with p as the caller, and stamps user_id on
// the request logger in place so every later line carries it.
func serveAs(next http.Handler, w http.ResponseWriter, r *http.Request, p Principal) {
	ctx := WithPrincipal(r.Context(), p)
	logging.Enrich(ctx, func(c zerolog.Context) zerolog.Context { return c.Int64("user_id", p.UserID) })
	next.ServeHTTP(w, r.WithContext(ctx))
}
