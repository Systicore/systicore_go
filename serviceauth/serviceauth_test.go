package serviceauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testIssuer = "https://auth.systicore.test"

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func testValidator() *Validator {
	return NewValidator(func(*jwt.Token) (any, error) { return &testKey.PublicKey, nil }, testIssuer)
}

// serviceToken is a client_credentials token as the authenticator mints
// it (oauth.service.go); edit tweaks the claims.
func serviceToken(t *testing.T, edit func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":        "client:projects",
		"iss":        testIssuer,
		"aud":        []string{"projects"},
		"scope":      "svc.push.send svc.health.read",
		"exp":        time.Now().Add(15 * time.Minute).Unix(),
		"iat":        time.Now().Unix(),
		"client_id":  "projects",
		"grant_type": "client_credentials",
	}
	if edit != nil {
		edit(claims)
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidate(t *testing.T) {
	caller, err := testValidator().Validate(serviceToken(t, nil))
	if err != nil || caller.ClientID != "projects" || len(caller.Scopes) != 2 || caller.Scopes[0] != "svc.push.send" {
		t.Fatalf("valid token: %+v, %v", caller, err)
	}

	userToken := func(c jwt.MapClaims) { c["sub"] = "42"; delete(c, "grant_type") }
	rejected := map[string]struct {
		v   *Validator
		raw string
		is  error
	}{
		"user token":             {testValidator(), serviceToken(t, userToken), ErrNotServiceToken},
		"user sub, service type": {testValidator(), serviceToken(t, func(c jwt.MapClaims) { c["sub"] = "42" }), ErrNotServiceToken},
		"empty client id":        {testValidator(), serviceToken(t, func(c jwt.MapClaims) { c["sub"] = "client:" }), ErrNotServiceToken},
		"foreign issuer":         {testValidator(), serviceToken(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.test" }), jwt.ErrTokenInvalidIssuer},
		"expired":                {testValidator(), serviceToken(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), jwt.ErrTokenExpired},
		"no keys":                {NewValidator(nil, testIssuer), serviceToken(t, nil), ErrNoKeys},
		"no issuer pinned":       {NewValidator(testValidator().keyfunc, ""), serviceToken(t, nil), ErrNoIssuer},
	}
	for name, tc := range rejected {
		if _, err := tc.v.Validate(tc.raw); !errors.Is(err, tc.is) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.is)
		}
	}
}

func TestRequire(t *testing.T) {
	const (
		missing      = `{"status":"unauthorized","message":"missing or malformed Authorization header (expected 'Bearer \u003ctoken\u003e')"}`
		invalid      = `{"status":"unauthorized","message":"invalid or expired service token"}`
		noScope      = `{"status":"forbidden","message":"missing required scope: svc.push.send"}`
		notOnTheList = `{"status":"forbidden","message":"client is not allowed to call this service"}`
	)
	strict := Options{Validator: testValidator(), AllowedClients: []string{"projects"}}
	dev := strict
	dev.DevBypass = true

	cases := []struct {
		name    string
		o       Options
		auth    string
		cookie  bool
		code    int
		body    string
		allowed string
	}{
		{"valid", strict, "Bearer " + serviceToken(t, nil), false, 200, "", "projects"},
		{"no header", strict, "", false, 401, missing, ""},
		{"wrong scheme", strict, "Basic dTpw", false, 401, missing, ""},
		// A browser session never passes as a service.
		{"session cookie only", strict, "", true, 401, missing, ""},
		{"forged", strict, "Bearer not.a.jwt", false, 401, invalid, ""},
		{"user token", strict, "Bearer " + serviceToken(t, func(c jwt.MapClaims) { c["sub"] = "42"; delete(c, "grant_type") }), false, 401, invalid, ""},
		{"missing scope", strict, "Bearer " + serviceToken(t, func(c jwt.MapClaims) { c["scope"] = "svc.health.read" }), false, 403, noScope, ""},
		{"not on the allowlist", strict, "Bearer " + serviceToken(t, func(c jwt.MapClaims) { c["sub"] = "client:reports" }), false, 403, notOnTheList, ""},
		{"empty allowlist refuses all", Options{Validator: testValidator()}, "Bearer " + serviceToken(t, nil), false, 403, notOnTheList, ""},
		{"dev bypass without a credential", dev, "", false, 200, "", DevBypassClientID},
		{"dev bypass still checks a sent token", dev, "Bearer forged", false, 401, invalid, ""},
	}
	for _, tc := range cases {
		var got Caller
		h := Require(tc.o, "svc.push.send")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, _ = CallerFrom(r.Context())
		}))
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/push", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		if tc.cookie {
			r.AddCookie(&http.Cookie{Name: "systicore_session", Value: serviceToken(t, nil)})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tc.code || rec.Body.String() != tc.body || got.ClientID != tc.allowed {
			t.Errorf("%s: %d %s caller=%q, want %d %s %q", tc.name, rec.Code, rec.Body.String(), got.ClientID, tc.code, tc.body, tc.allowed)
		}
	}
}

// tokenEndpoint is a fake authenticator /api/v1/oauth/token.
func tokenEndpoint(t *testing.T, expiresIn int, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/oauth/token" || r.ParseForm() != nil ||
			r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != "projects" ||
			r.PostForm.Get("client_secret") != "s3cret" || r.PostForm.Get("scope") != "svc.push.send svc.issues.write" {
			t.Errorf("unexpected token request %s %v", r.URL.Path, r.PostForm)
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"access_token":"tok-` + time.Now().Format("150405.000000") + `","token_type":"Bearer","expires_in":` + strconv.Itoa(expiresIn) + `}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"invalid_scope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newSource(url string) *TokenSource {
	return NewTokenSource(ClientOptions{IssuerURL: url + "/", ClientID: "projects", ClientSecret: "s3cret", Scopes: []string{"svc.push.send", "svc.issues.write"}})
}

func TestTokenSourceCachesUntilShortlyBeforeExpiry(t *testing.T) {
	srv, calls := tokenEndpoint(t, 900, http.StatusOK)
	src := newSource(srv.URL)
	clock := time.Now()
	src.now = func() time.Time { return clock }

	first, err := src.Token(context.Background())
	if err != nil || first == "" {
		t.Fatalf("first token: %q %v", first, err)
	}
	clock = clock.Add(800 * time.Second) // still more than a minute left
	if again, _ := src.Token(context.Background()); again != first || calls.Load() != 1 {
		t.Errorf("within the lifetime: %q (%d requests), want the cached %q", again, calls.Load(), first)
	}
	clock = clock.Add(50 * time.Second) // inside the last minute
	if _, err := src.Token(context.Background()); err != nil || calls.Load() != 2 {
		t.Errorf("near expiry: %v, %d requests, want a fresh mint", err, calls.Load())
	}
}

func TestTokenSourceSharesOneMintAndNeverCachesShortTokens(t *testing.T) {
	srv, calls := tokenEndpoint(t, 30, http.StatusOK) // shorter than the slack
	src := newSource(srv.URL)
	for range 3 {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("a token shorter-lived than the slack is used once: %d requests, want 3", calls.Load())
	}

	srv2, calls2 := tokenEndpoint(t, 900, http.StatusOK)
	src2 := newSource(srv2.URL)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = src2.Token(context.Background()) }()
	}
	wg.Wait()
	if calls2.Load() != 1 {
		t.Errorf("16 concurrent callers made %d token requests, want 1", calls2.Load())
	}
}

func TestTokenSourceErrorsAndDevBypass(t *testing.T) {
	srv, _ := tokenEndpoint(t, 0, http.StatusBadRequest)
	if _, err := newSource(srv.URL).Token(context.Background()); err == nil {
		t.Error("a refused token request must be an error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"expires_in":900}`))
	}))
	defer empty.Close()
	if _, err := newSource(empty.URL).Token(context.Background()); err == nil {
		t.Error("an answer without access_token must be an error")
	}

	none := NewTokenSource(ClientOptions{IssuerURL: "http://127.0.0.1:1"}) // nothing listens there
	if token, err := none.Token(context.Background()); token != "" || err != nil {
		t.Errorf("no client id is the dev bypass: %q %v", token, err)
	}
}
