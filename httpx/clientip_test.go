package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	loadBalancer := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	cases := []struct {
		name   string
		opts   ClientIPOptions
		remote string
		xff    string
		want   string
	}{
		{"direct, port stripped", ClientIPOptions{}, "203.0.113.5:4321", "", "203.0.113.5"},
		{"proxy disabled ignores xff", ClientIPOptions{}, "127.0.0.1:1", "198.51.100.1", "127.0.0.1"},
		{"loopback proxy trusted", ClientIPOptions{Enabled: true, TrustLoopback: true}, "127.0.0.1:1", "198.51.100.1", "198.51.100.1"},
		{"private proxy trusted", ClientIPOptions{Enabled: true, TrustPrivate: true}, "172.18.0.3:1", "198.51.100.1", "198.51.100.1"},
		{"private peer not trusted when only loopback", ClientIPOptions{Enabled: true, TrustLoopback: true}, "172.18.0.3:1", "198.51.100.1", "172.18.0.3"},
		{"public peer never trusted", ClientIPOptions{Enabled: true, TrustLoopback: true, TrustPrivate: true}, "203.0.113.5:1", "198.51.100.1", "203.0.113.5"},
		{"garbage xff falls back to peer", ClientIPOptions{Enabled: true, TrustLoopback: true}, "127.0.0.1:1", "not-an-ip", "127.0.0.1"},
		{"v4-mapped v6 unmapped", ClientIPOptions{Enabled: true, TrustLoopback: true}, "[::1]:1", "::ffff:198.51.100.1", "198.51.100.1"},
		// The proxy appends the address it saw; whatever the client wrote
		// in front of it must not become the client IP.
		{"spoofed leftmost entry ignored", ClientIPOptions{Enabled: true, TrustPrivate: true}, "172.18.0.3:1", "6.6.6.6, 198.51.100.1", "198.51.100.1"},
		{"trusted hops skipped", ClientIPOptions{Enabled: true, TrustPrivate: true}, "172.18.0.3:1", "198.51.100.1, 10.0.0.2", "198.51.100.1"},
		{"untrusted inner hop is the client", ClientIPOptions{Enabled: true, TrustLoopback: true}, "127.0.0.1:1", "198.51.100.1, 10.0.0.2", "10.0.0.2"},
		{"every hop trusted: leftmost", ClientIPOptions{Enabled: true, TrustPrivate: true}, "10.0.0.1:1", "10.0.0.9, 10.0.0.8", "10.0.0.9"},
		{"cidr peer trusted", ClientIPOptions{Enabled: true, TrustedPrefixes: loadBalancer}, "203.0.113.5:1", "198.51.100.1", "198.51.100.1"},
		{"cidr hop skipped", ClientIPOptions{Enabled: true, TrustPrivate: true, TrustedPrefixes: loadBalancer}, "172.18.0.3:1", "198.51.100.1, 203.0.113.7", "198.51.100.1"},
		{"link-local peer trusted", ClientIPOptions{Enabled: true, TrustLinkLocal: true}, "169.254.10.1:1", "198.51.100.1", "198.51.100.1"},
		{"link-local peer untrusted by default", ClientIPOptions{Enabled: true, TrustPrivate: true}, "169.254.10.1:1", "198.51.100.1", "169.254.10.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := ClientIP(tc.opts)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Fatalf("RemoteAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIP(t *testing.T) {
	for remote, want := range map[string]string{
		"198.51.100.1:443": "198.51.100.1", // the server's own ip:port form
		"[2001:db8::1]:80": "2001:db8::1",
		"198.51.100.1":     "198.51.100.1", // already bare: ClientIP ran
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if got := IP(r); got != want {
			t.Errorf("IP(%q) = %q, want %q", remote, got, want)
		}
	}
}
