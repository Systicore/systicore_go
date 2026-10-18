package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCache records what ResolvePermissions stores.
type fakeCache struct {
	mu     sync.Mutex
	values map[string]string
	ttls   map[string]time.Duration
}

func newFakeCache() *fakeCache {
	return &fakeCache{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (c *fakeCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.values[key]; ok {
		return v, nil
	}
	return "", errors.New("miss")
}

func (c *fakeCache) Set(_ context.Context, key string, value interface{}, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = value.(string)
	c.ttls[key] = ttl
	return nil
}

// countingSource answers perms and counts the calls.
type countingSource struct {
	calls atomic.Int32
	perms []string
	err   error
}

func (s *countingSource) Permissions(context.Context, Principal, Credential) ([]string, error) {
	s.calls.Add(1)
	return s.perms, s.err
}

// asUser builds a request Authenticate has already run on.
func asUser(p Principal) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := withCredential(WithPrincipal(r.Context(), p), Credential{Token: "t", Source: TokenHeader})
	return r.WithContext(ctx)
}

func TestResolvePermissionsCachesPerUserWithinTheTokenLife(t *testing.T) {
	cache := newFakeCache()
	source := &countingSource{perms: []string{"systicore.reports.read"}}
	var seen []string
	h := ResolvePermissions(PermissionOptions{Source: source, Cache: cache})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, _ = PermissionsFrom(r.Context()) }))

	user := Principal{UserID: 42, TokenExp: time.Now().Add(15 * time.Minute)}
	h.ServeHTTP(httptest.NewRecorder(), asUser(user))
	h.ServeHTTP(httptest.NewRecorder(), asUser(user))

	if source.calls.Load() != 1 {
		t.Errorf("source called %d times, want 1: the second request reads the cache", source.calls.Load())
	}
	if len(seen) != 1 || seen[0] != "systicore.reports.read" {
		t.Errorf("permissions = %v", seen)
	}
	if ttl := cache.ttls["user_perms:42"]; ttl != 5*time.Minute {
		t.Errorf("ttl = %s, want the 5 minute cap", ttl)
	}

	// A token about to expire: the set is used but not cached.
	short := Principal{UserID: 43, TokenExp: time.Now().Add(20 * time.Second)}
	h.ServeHTTP(httptest.NewRecorder(), asUser(short))
	if _, cached := cache.values["user_perms:43"]; cached {
		t.Error("a set must not be cached past the token's expiry minus the buffer")
	}
}

func TestResolvePermissionsWithoutCacheAsksEveryTime(t *testing.T) {
	source := &countingSource{perms: []string{}}
	h := ResolvePermissions(PermissionOptions{Source: source})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	user := Principal{UserID: 1, TokenExp: time.Now().Add(time.Hour)}
	for range 3 {
		h.ServeHTTP(httptest.NewRecorder(), asUser(user))
	}
	if source.calls.Load() != 3 {
		t.Errorf("source called %d times, want 3", source.calls.Load())
	}
}

func TestResolvePermissionsSharesOneLookup(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	source := PermissionSourceFunc(func(context.Context, Principal, Credential) ([]string, error) {
		calls.Add(1)
		<-release
		return []string{"p"}, nil
	})
	h := ResolvePermissions(PermissionOptions{Source: source})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	user := Principal{UserID: 9, TokenExp: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(httptest.NewRecorder(), asUser(user)) }()
	}
	time.Sleep(50 * time.Millisecond) // let them all queue on the one flight
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("%d lookups for 8 concurrent requests of one user, want 1", n)
	}
}

func TestPermissionOutageAnswers503NotForbidden(t *testing.T) {
	source := &countingSource{err: errors.New("authenticator down")}
	chain := ResolvePermissions(PermissionOptions{Source: source, Cache: newFakeCache()})(
		RequirePermission("systicore.reports.read")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("handler reached")
		})))
	rec, _ := serve(chain, asUser(Principal{UserID: 42, TokenExp: time.Now().Add(time.Hour)}))
	if rec.Code != http.StatusServiceUnavailable ||
		rec.Body.String() != `{"status":"error","message":"Permission service temporarily unavailable. Please retry shortly."}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestResolvePermissionsEdges(t *testing.T) {
	source := &countingSource{perms: []string{"x"}}
	var seen []string
	var ok bool
	h := ResolvePermissions(PermissionOptions{Source: source})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, ok = PermissionsFrom(r.Context())
	}))

	// Mounted without Authenticate in front.
	rec, _ := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"internal error"}` {
		t.Errorf("no principal: %d %s", rec.Code, rec.Body.String())
	}

	// The dev user keeps the empty set Authenticate seeded.
	r := asUser(devUser())
	r = r.WithContext(WithPermissions(r.Context(), []string{}))
	serve(h, r)
	if source.calls.Load() != 0 || !ok || len(seen) != 0 {
		t.Errorf("dev fallback: %d lookups, permissions %v", source.calls.Load(), seen)
	}
}

func TestRequirePermission(t *testing.T) {
	reached := false
	h := RequirePermission("systicore.reports.update")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	run := func(ctx context.Context) (int, string) {
		reached = false
		rec, _ := serve(h, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		return rec.Code, rec.Body.String()
	}
	base := context.Background()

	if code, _ := run(WithPermissions(base, []string{"a", "systicore.reports.update"})); code != http.StatusOK || !reached {
		t.Errorf("held: %d", code)
	}
	if code, body := run(WithPermissions(base, []string{"systicore.reports.read"})); code != http.StatusForbidden ||
		body != `{"status":"error","message":"missing permission: systicore.reports.update"}` || reached {
		t.Errorf("missing: %d %s", code, body)
	}
	if code, body := run(base); code != http.StatusInternalServerError || body != `{"status":"error","message":"no permissions found"}` {
		t.Errorf("never resolved: %d %s", code, body)
	}

	dev := DevelopmentPermission("anything")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	reached = false
	serve(dev, httptest.NewRequest(http.MethodGet, "/", nil))
	if !reached {
		t.Error("DevelopmentPermission must let the request through")
	}
}

func TestUserInfoForwardsTheCredentialAsItCame(t *testing.T) {
	var gotAuth, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		if c, err := r.Cookie("systicore_session"); err == nil {
			gotCookie = c.Value
		}
		_, _ = w.Write([]byte(`{"status":"ok","data":{"id":42,"permissions":["systicore.reports.read"]}}`))
	}))
	defer upstream.Close()
	source := UserInfo(upstream.URL+"/", nil)

	perms, err := source.Permissions(context.Background(), Principal{}, Credential{Token: "hdr", Source: TokenHeader})
	if err != nil || len(perms) != 1 || perms[0] != "systicore.reports.read" || gotAuth != "Bearer hdr" || gotCookie != "" {
		t.Errorf("header: %v %v auth=%q cookie=%q", perms, err, gotAuth, gotCookie)
	}

	gotAuth, gotCookie = "", ""
	_, err = source.Permissions(context.Background(), Principal{}, Credential{Token: "ck", Source: TokenCookie, CookieName: "systicore_session"})
	if err != nil || gotCookie != "ck" || gotAuth != "" {
		t.Errorf("cookie: %v auth=%q cookie=%q", err, gotAuth, gotCookie)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer failing.Close()
	if _, err := UserInfo(failing.URL, nil).Permissions(context.Background(), Principal{}, Credential{Token: "x"}); err == nil {
		t.Error("a non-200 userinfo answer must be an error")
	}
}

func TestCacheTTL(t *testing.T) {
	now := time.Now()
	cases := []struct {
		exp  time.Time
		want func(time.Duration) bool
	}{
		{now.Add(time.Hour), func(d time.Duration) bool { return d == 5*time.Minute }},
		{now.Add(2 * time.Minute), func(d time.Duration) bool { return d > 80*time.Second && d <= 90*time.Second }},
		{now.Add(20 * time.Second), func(d time.Duration) bool { return d == 0 }},
		{time.Time{}, func(d time.Duration) bool { return d == 0 }},
	}
	for _, tc := range cases {
		if got := cacheTTL(tc.exp, 5*time.Minute, 30*time.Second); !tc.want(got) {
			t.Errorf("cacheTTL(exp in %s) = %s", time.Until(tc.exp).Round(time.Second), got)
		}
	}
}
