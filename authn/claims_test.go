package authn

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// claimChain is a service's chain, Authenticate → ResolvePermissions →
// RequirePermission(required), with a source and a cache that already
// holds user 42's full first-party set, so a test sees whether either is
// consulted.
func claimChain(t *testing.T, required string, o PermissionOptions) (http.Handler, *countingSource, *fakeCache, *[]string) {
	t.Helper()
	source := &countingSource{perms: []string{"systicore.reports.read", "systicore.core.users.read"}}
	cache := newFakeCache()
	cache.values["user_perms:42"] = `["systicore.reports.read","systicore.core.users.read"]`
	o.Source, o.Cache = source, cache
	var seen []string
	h := Authenticate(AuthOptions{Verifier: testVerifier("")})(
		ResolvePermissions(o)(
			RequirePermission(required)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen, _ = PermissionsFrom(r.Context())
			}))))
	return h, source, cache, &seen
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func mobileToken(t *testing.T, permissions any) string {
	return sign(t, func(c jwt.MapClaims) {
		c["client_id"] = "systicore_mobile"
		c["aud"] = []string{"systicore_mobile"}
		c["permissions"] = permissions
	})
}

func TestTokenPermissionsClaimIsTheWholeSet(t *testing.T) {
	h, source, cache, seen := claimChain(t, "systicore.reports.read", PermissionOptions{})
	cache.values = map[string]string{} // nothing to read either way

	rec, _ := serve(h, bearer(mobileToken(t, []string{"systicore.reports.read", "systicore.reports.update"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("held via claim: %d %s", rec.Code, rec.Body.String())
	}
	if n := source.calls.Load(); n != 0 {
		t.Errorf("source called %d times; a token with the claim needs no userinfo call", n)
	}
	if len(cache.values) != 0 {
		t.Errorf("the claim's set was cached: %v", cache.values)
	}
	if strings.Join(*seen, ",") != "systicore.reports.read,systicore.reports.update" {
		t.Errorf("permissions = %v", *seen)
	}
}

// The claim lists only the client's namespaces. A route outside them is
// refused, even though the user holds the permission first-party and the
// cache has that set under the same user id.
func TestTokenPermissionsClaimDeniesOutsideTheClientsNamespaces(t *testing.T) {
	h, source, _, _ := claimChain(t, "systicore.core.users.read", PermissionOptions{})
	rec, _ := serve(h, bearer(mobileToken(t, []string{"systicore.reports.read"})))
	if rec.Code != http.StatusForbidden ||
		rec.Body.String() != `{"status":"error","message":"missing permission: systicore.core.users.read"}` {
		t.Errorf("outside the namespaces: %d %s", rec.Code, rec.Body.String())
	}
	if n := source.calls.Load(); n != 0 {
		t.Errorf("source called %d times", n)
	}
}

func TestEmptyTokenPermissionsClaimDoesNotFallBack(t *testing.T) {
	var seen []string
	var ok bool
	source := &countingSource{perms: []string{"systicore.reports.read"}}
	h := Authenticate(AuthOptions{Verifier: testVerifier("")})(
		ResolvePermissions(PermissionOptions{Source: source, Cache: newFakeCache()})(
			http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, ok = PermissionsFrom(r.Context()) })))
	serve(h, bearer(mobileToken(t, []string{})))
	if !ok || seen == nil || len(seen) != 0 || source.calls.Load() != 0 {
		t.Errorf("empty claim: permissions %v (ok=%v), %d lookups; want the empty set and none", seen, ok, source.calls.Load())
	}

	chain, source2, _, _ := claimChain(t, "systicore.reports.read", PermissionOptions{})
	if rec, _ := serve(chain, bearer(mobileToken(t, []string{}))); rec.Code != http.StatusForbidden || source2.calls.Load() != 0 {
		t.Errorf("empty claim at RequirePermission: %d, %d lookups", rec.Code, source2.calls.Load())
	}
}

func TestNoTokenPermissionsClaimKeepsTheUserInfoPath(t *testing.T) {
	h, source, cache, _ := claimChain(t, "systicore.core.users.read", PermissionOptions{})
	cache.values = map[string]string{}
	for range 2 {
		if rec, _ := serve(h, bearer(sign(t, nil))); rec.Code != http.StatusOK {
			t.Fatalf("session token: %d %s", rec.Code, rec.Body.String())
		}
	}
	if n := source.calls.Load(); n != 1 {
		t.Errorf("source called %d times, want 1 (then the cache)", n)
	}
	if _, ok := cache.values["user_perms:42"]; !ok {
		t.Error("the userinfo set was not cached")
	}
}

func TestIgnoreTokenPermissionsAsksTheSource(t *testing.T) {
	h, source, cache, _ := claimChain(t, "systicore.core.users.read", PermissionOptions{IgnoreTokenPermissions: true})
	cache.values = map[string]string{}
	if rec, _ := serve(h, bearer(mobileToken(t, []string{"systicore.reports.read"}))); rec.Code != http.StatusOK {
		t.Errorf("IgnoreTokenPermissions: %d %s", rec.Code, rec.Body.String())
	}
	if source.calls.Load() != 1 {
		t.Errorf("source called %d times, want 1", source.calls.Load())
	}
}

// A claim that is not an array of non-empty strings makes the token
// invalid: 401, never a guess at what was meant and never the userinfo
// path.
func TestMalformedTokenPermissionsClaimIsAnInvalidToken(t *testing.T) {
	malformed := map[string]any{
		"string":        "systicore.reports.read",
		"number":        1,
		"object":        map[string]any{"systicore.reports.read": true},
		"null":          nil,
		"number inside": []any{"systicore.reports.read", 1},
		"null inside":   []any{"systicore.reports.read", nil},
		"empty string":  []any{""},
		"nested array":  []any{[]string{"systicore.reports.read"}},
	}
	for name, claim := range malformed {
		token := mobileToken(t, claim)
		if _, err := testVerifier("").Verify(token); !errors.Is(err, ErrMalformedPermissions) {
			t.Errorf("%s: Verify err = %v, want ErrMalformedPermissions", name, err)
		}
		h, source, _, _ := claimChain(t, "systicore.reports.read", PermissionOptions{})
		rec, _ := serve(h, bearer(token))
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":"invalid or expired token"}` || source.calls.Load() != 0 {
			t.Errorf("%s: %d %s, %d lookups", name, rec.Code, rec.Body.String(), source.calls.Load())
		}
	}
}

func TestVerifierReadsThePermissionsClaim(t *testing.T) {
	claims, err := testVerifier("").Verify(sign(t, nil))
	if err != nil || claims.Permissions.Present || claims.Permissions.Values != nil {
		t.Errorf("no claim: %+v %v", claims.Permissions, err)
	}
	claims, err = testVerifier("").Verify(mobileToken(t, []string{}))
	if err != nil || !claims.Permissions.Present || claims.Permissions.Values == nil || len(claims.Permissions.Values) != 0 {
		t.Errorf("empty claim: %+v %v", claims.Permissions, err)
	}

	// Claims written back leave an absent claim out and keep a present one.
	raw, _ := json.Marshal(Claims{})
	if strings.Contains(string(raw), "permissions") {
		t.Errorf("absent claim marshalled: %s", raw)
	}
	raw, _ = json.Marshal(Claims{Permissions: PermissionsClaim{Present: true, Values: []string{}}})
	if !strings.Contains(string(raw), `"permissions":[]`) {
		t.Errorf("empty claim marshalled: %s", raw)
	}
}

func TestTokenPermissionsAreClonedIntoTheContext(t *testing.T) {
	p := Principal{UserID: 42, TokenPermissions: PermissionsClaim{Present: true, Values: make([]string, 1, 4)}}
	p.TokenPermissions.Values[0] = "a"
	h := ResolvePermissions(PermissionOptions{Source: &countingSource{}})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		set, _ := PermissionsFrom(r.Context())
		set[0] = "changed"
	}))
	serve(h, asUser(p))
	if p.TokenPermissions.Values[0] != "a" {
		t.Error("a handler's write reached the Principal's set")
	}
}
