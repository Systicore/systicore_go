package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/Systicore/systicore_go/httpx"
)

const (
	// CSRFCookieName is the cookie carrying the double-submit token. It is
	// readable by JavaScript on purpose: the web client copies it into
	// CSRFHeaderName, which a cross-site page can neither read nor set.
	CSRFCookieName = "csrf_token"
	// CSRFHeaderName is the header the web client echoes the cookie into.
	CSRFHeaderName = "X-CSRF-Token"
)

// CSRFOptions are the attributes of the CSRF cookie. They must match the
// session cookie's, or cross-site flows lose the CSRF cookie and every
// state-changing request gets a 403.
type CSRFOptions struct {
	// Secure marks the cookie HTTPS-only; browsers require it with
	// SameSite=None.
	Secure bool
	// SameSite is "Lax" (the default), "Strict" or "None", in any case.
	SameSite string
	// Domain is the cookie domain; "" makes it host-only. With the web
	// client on systicore.hu and the API on a subdomain it must be
	// ".systicore.hu", or the client cannot read the cookie to echo it.
	Domain string
}

// NormalizeSameSite maps a SameSite setting written in any case to its
// net/http mode; anything unknown, "" included, is Lax. Read once at
// start, so a mistyped environment value never reaches a Set-Cookie
// header, where some browsers drop the cookie over it.
func NormalizeSameSite(value string) http.SameSite {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

// CSRF is the double-submit cookie guard. A safe request (GET, HEAD,
// OPTIONS) gets a csrf_token cookie when it has none; a state-changing one
// must carry the cookie and the same value in X-CSRF-Token, compared in
// constant time, or it is refused with 403 before reaching the handler.
//
// A request with a Bearer token skips the check: the browser never
// attaches an Authorization header on its own, so a cross-site page cannot
// forge one, and mobile, CLI and service clients have no cookie to echo.
//
// Mount it before Authenticate, so a forged request gets the 403 and not
// a 401.
func CSRF(o CSRFOptions) httpx.Middleware {
	sameSite := NormalizeSameSite(o.SameSite)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookieToken := ""
			if c, err := r.Cookie(CSRFCookieName); err == nil {
				cookieToken = c.Value
			}

			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				// Issue a token only when there is none: the backends on
				// the shared cookie domain then all check the same value.
				if cookieToken == "" {
					token, err := newCSRFToken()
					if err != nil {
						authError(w, http.StatusInternalServerError, "failed to issue csrf token")
						return
					}
					http.SetCookie(w, &http.Cookie{
						Name:     CSRFCookieName,
						Value:    token,
						Path:     "/",
						HttpOnly: false, // the web client must read it
						Secure:   o.Secure,
						SameSite: sameSite,
						Domain:   o.Domain,
					})
				}
				next.ServeHTTP(w, r)
				return
			}

			if _, source := ExtractToken(r, ""); source == TokenHeader {
				next.ServeHTTP(w, r)
				return
			}

			headerToken := r.Header.Get(CSRFHeaderName)
			if cookieToken == "" || headerToken == "" {
				authError(w, http.StatusForbidden, "csrf token missing (need both "+CSRFCookieName+" cookie and "+CSRFHeaderName+" header)")
				return
			}
			if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(headerToken)) != 1 {
				authError(w, http.StatusForbidden, "csrf token mismatch")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// newCSRFToken is 32 random bytes, hex-encoded.
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
