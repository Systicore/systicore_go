package httpx

import (
	"net/http"
	"strconv"
)

// SecurityHeadersOptions are the headers a service sets differently from
// the defaults. The zero value sends the defaults of Fiber helmet.
type SecurityHeadersOptions struct {
	// CrossOriginResourcePolicy; "" sends "same-origin". A Systicore API
	// answers an SPA on another subdomain and sends "cross-origin".
	CrossOriginResourcePolicy string
	// FrameOptions is X-Frame-Options; "" sends "SAMEORIGIN".
	FrameOptions string
	// ContentSecurityPolicy, when set, is sent as is.
	ContentSecurityPolicy string
	// HSTSMaxAge above 0 sends Strict-Transport-Security with
	// includeSubDomains, plus preload when HSTSPreload is set. It goes out
	// on every response: a browser ignores it over plain HTTP (RFC 6797
	// section 8.1), so whether TLS ended at a proxy does not matter.
	HSTSMaxAge  int
	HSTSPreload bool
}

// SecurityHeaders sets the security headers on every response, before the
// handler runs. The list is the Fiber helmet defaults written out, so a
// change is a visible diff and not a flipped default.
func SecurityHeaders(o SecurityHeadersOptions) Middleware {
	corp := o.CrossOriginResourcePolicy
	if corp == "" {
		corp = "same-origin"
	}
	frame := o.FrameOptions
	if frame == "" {
		frame = "SAMEORIGIN"
	}
	hsts := ""
	if o.HSTSMaxAge > 0 {
		hsts = "max-age=" + strconv.Itoa(o.HSTSMaxAge) + "; includeSubDomains"
		if o.HSTSPreload {
			hsts += "; preload"
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-XSS-Protection", "0")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", frame)
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Embedder-Policy", "require-corp")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", corp)
			h.Set("Origin-Agent-Cluster", "?1")
			h.Set("X-DNS-Prefetch-Control", "off")
			h.Set("X-Download-Options", "noopen")
			h.Set("X-Permitted-Cross-Domain-Policies", "none")
			if o.ContentSecurityPolicy != "" {
				h.Set("Content-Security-Policy", o.ContentSecurityPolicy)
			}
			if hsts != "" {
				h.Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}
