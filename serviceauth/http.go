package serviceauth

import (
	"net/http"

	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/authn"
	"github.com/Systicore/systicore_go/httpx"
	"github.com/Systicore/systicore_go/logging"
)

// answer is the {"status","message"} envelope the /internal/v1 routes of
// the Systicore backends have always answered a refusal with.
type answer struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Require admits only a service holding scope on the allowlist, and puts
// it into the context (CallerFrom). The token is read from the
// Authorization header only: a service is no browser, and honouring the
// session cookie here would let a user session pass as a service.
//
// 401 for a missing, malformed or invalid token, 403 for a valid one
// without the scope or off the allowlist. Mount it on the /internal/v1
// routes, outside the CSRF guard: service callers carry no CSRF cookie.
func Require(o Options, scope string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, source := authn.ExtractToken(r, "")
			if source != authn.TokenHeader {
				token = ""
			}
			caller, outcome, err := o.Check(token, source != authn.TokenNone, scope)

			l := zerolog.Ctx(r.Context())
			switch outcome {
			case Unauthenticated:
				l.Warn().Err(err).Str("middleware", "serviceauth").Str("token_source", source.String()).Msg("service authentication failed")
				message := "invalid or expired service token"
				if token == "" {
					message = "missing or malformed Authorization header (expected 'Bearer <token>')"
				}
				httpx.JSON(w, http.StatusUnauthorized, answer{Status: "unauthorized", Message: message})
				return
			case MissingScope:
				l.Warn().Err(err).Str("middleware", "serviceauth").Str("client_id", caller.ClientID).Str("required_scope", scope).Msg("service token missing required scope")
				httpx.JSON(w, http.StatusForbidden, answer{Status: "forbidden", Message: "missing required scope: " + scope})
				return
			case NotAllowed:
				l.Warn().Err(err).Str("middleware", "serviceauth").Str("client_id", caller.ClientID).Msg("service client not on the allowlist")
				httpx.JSON(w, http.StatusForbidden, answer{Status: "forbidden", Message: "client is not allowed to call this service"})
				return
			}

			ctx := WithCaller(r.Context(), caller)
			logging.Enrich(ctx, func(c zerolog.Context) zerolog.Context { return c.Str("client_id", caller.ClientID) })
			l.Debug().Str("middleware", "serviceauth").Str("client_id", caller.ClientID).Msg("service client authenticated")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
