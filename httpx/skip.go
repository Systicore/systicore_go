package httpx

import (
	"net/http"
	"strings"
)

// Unless runs mw except on the requests skip reports true; those go
// straight to the next handler. E.g. no security headers on the Swagger
// UI:
//
//	r.Use(httpx.Unless(func(r *http.Request) bool {
//		return strings.HasPrefix(r.URL.Path, "/swagger")
//	}, httpx.SecurityHeaders(opts)))
func Unless(skip func(*http.Request) bool, mw Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

// SkipPaths runs mw except on the listed paths. A trailing slash is
// ignored on both sides, so "/x/" — which a sub-router mounted at "/x"
// also serves — is skipped along with "/x".
func SkipPaths(mw Middleware, paths ...string) Middleware {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		set[strings.TrimRight(path, "/")] = true
	}
	return Unless(func(r *http.Request) bool {
		return set[strings.TrimRight(r.URL.Path, "/")]
	}, mw)
}
