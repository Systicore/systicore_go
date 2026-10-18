package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPOptions say which TCP peers are trusted proxies (Traefik on the
// Docker network): their X-Forwarded-For is honoured, anybody else's is
// ignored. The fields mirror Fiber's TrustProxyConfig, so a service keeps
// its SERVER_PROXY_* settings across the move to chi.
type ClientIPOptions struct {
	// Enabled false ignores X-Forwarded-For entirely.
	Enabled        bool
	TrustLoopback  bool
	TrustPrivate   bool
	TrustLinkLocal bool
	// TrustedPrefixes are further proxy networks (SERVER_PROXY_TRUST_CIDRS),
	// e.g. a load balancer outside the private ranges.
	TrustedPrefixes []netip.Prefix
}

func (o ClientIPOptions) trusts(address netip.Addr) bool {
	address = address.Unmap()
	if (o.TrustLoopback && address.IsLoopback()) ||
		(o.TrustPrivate && address.IsPrivate()) ||
		(o.TrustLinkLocal && (address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast())) {
		return true
	}
	for _, prefix := range o.TrustedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// ClientIP rewrites r.RemoteAddr to the bare client IP, without a port, so
// everything downstream — the access log, rate-limit keys, audit metadata
// — sees one canonical value.
//
// Behind a trusted proxy the value comes from X-Forwarded-For, read from
// the nearest hop outwards: trusted proxies are skipped and the first
// address that is not one is the client. Entries left of it were written
// by the client itself and are ignored, so a forged X-Forwarded-For
// cannot choose the address per-IP limits key on.
//
// Mount it first: everything else keys off RemoteAddr.
func ClientIP(o ClientIPOptions) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer := r.RemoteAddr
			if host, _, err := net.SplitHostPort(peer); err == nil {
				peer = host
			}
			client := peer
			if o.Enabled {
				if peerAddress, err := netip.ParseAddr(peer); err == nil && o.trusts(peerAddress) {
					if forwarded, ok := forwardedClient(r.Header.Get("X-Forwarded-For"), o); ok {
						client = forwarded.String()
					}
				}
			}
			r.RemoteAddr = client
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedClient walks X-Forwarded-For from the rightmost (nearest) hop
// and returns the first address that is not a trusted proxy; when every
// hop is trusted, the leftmost. ok is false for an empty header or a hop
// that is not an IP address — the caller then keeps the TCP peer rather
// than guess past a malformed entry.
func forwardedClient(header string, o ClientIPOptions) (netip.Addr, bool) {
	if strings.TrimSpace(header) == "" {
		return netip.Addr{}, false
	}
	hops := strings.Split(header, ",")
	var outermost netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		address, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, false
		}
		outermost = address.Unmap()
		if !o.trusts(outermost) {
			return outermost, true
		}
	}
	return outermost, true
}

// IP is the client address of r without a port: what ClientIP left in
// RemoteAddr, or the host part of the "ip:port" the server fills in when
// ClientIP is not mounted.
func IP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
