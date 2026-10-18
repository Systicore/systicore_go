package authn

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

const testIssuer = "https://auth.systicore.test"

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func testKeyfunc(*jwt.Token) (any, error) { return &testKey.PublicKey, nil }

func testVerifier(audience string) *Verifier {
	return NewVerifier(VerifierOptions{Keyfunc: testKeyfunc, Issuer: testIssuer, Audience: audience})
}

// sign issues a token the way the authenticator does (MapClaims, RS256);
// edit tweaks the claims before signing.
func sign(t *testing.T, edit func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":                "42",
		"iss":                testIssuer,
		"aud":                []string{"systicore_web"},
		"exp":                time.Now().Add(15 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
		"email":              "ada@example.com",
		"preferred_username": "ada",
		"name":               "Ada Lovelace",
		"client_id":          "systicore_web",
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

func TestExtractToken(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		cookie     string
		cookieName string
		token      string
		source     TokenSource
	}{
		{"bearer header", "Bearer abc", "", "s", "abc", TokenHeader},
		{"scheme in any case", "bEaReR abc", "", "s", "abc", TokenHeader},
		{"cookie", "", "xyz", "s", "xyz", TokenCookie},
		{"header wins over cookie", "Bearer abc", "xyz", "s", "abc", TokenHeader},
		// A client that sent explicit credentials does not get the
		// browser's session instead.
		{"malformed header does not fall back", "Basic abc", "xyz", "s", "", TokenMalformed},
		{"empty bearer is malformed", "Bearer   ", "", "s", "", TokenMalformed},
		{"no cookie name ignores cookies", "", "xyz", "", "", TokenNone},
		{"nothing", "", "", "s", "", TokenNone},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "s", Value: tc.cookie})
		}
		token, source := ExtractToken(r, tc.cookieName)
		if token != tc.token || source != tc.source {
			t.Errorf("%s: got %q/%s, want %q/%s", tc.name, token, source, tc.token, tc.source)
		}
	}
}

func TestVerifier(t *testing.T) {
	claims, err := testVerifier("").Verify(sign(t, nil))
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if claims.Subject != "42" || claims.PreferredUsername != "ada" || claims.Email != "ada@example.com" || claims.ClientID != "systicore_web" {
		t.Errorf("claims = %+v", claims)
	}

	hs256, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "42", "iss": testIssuer, "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte("shared-secret"))

	rejected := map[string]struct {
		verifier *Verifier
		raw      string
		is       error
	}{
		"foreign issuer":      {testVerifier(""), sign(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.test" }), jwt.ErrTokenInvalidIssuer},
		"no expiry":           {testVerifier(""), sign(t, func(c jwt.MapClaims) { delete(c, "exp") }), jwt.ErrTokenRequiredClaimMissing},
		"expired":             {testVerifier(""), sign(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), jwt.ErrTokenExpired},
		"pinned audience":     {testVerifier("systicore_mobile"), sign(t, nil), jwt.ErrTokenInvalidAudience},
		"HS256 alg confusion": {testVerifier(""), hs256, jwt.ErrTokenSignatureInvalid},
		"no keys":             {NewVerifier(VerifierOptions{Issuer: testIssuer}), sign(t, nil), ErrNoKeys},
		"no issuer pinned":    {NewVerifier(VerifierOptions{Keyfunc: testKeyfunc}), sign(t, nil), ErrNoIssuer},
	}
	for name, tc := range rejected {
		if _, err := tc.verifier.Verify(tc.raw); !errors.Is(err, tc.is) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.is)
		}
	}
	if _, err := testVerifier("systicore_web").Verify(sign(t, nil)); err != nil {
		t.Errorf("matching audience: %v", err)
	}
}

// serve runs h behind logging.Middleware, so the request logger exists as
// in a service, and returns the answer and the log.
func serve(h http.Handler, r *http.Request) (*httptest.ResponseRecorder, string) {
	var log bytes.Buffer
	rec := httptest.NewRecorder()
	logging.Middleware(zerolog.New(&log))(h).ServeHTTP(rec, r)
	return rec, log.String()
}

func TestAuthenticateRefusals(t *testing.T) {
	never := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") })
	auth := Authenticate(AuthOptions{Verifier: testVerifier(""), CookieName: "systicore_session"})
	cases := map[string]struct {
		header string
		body   string
	}{
		"no token": {"", `{"error":"missing authorization (header or cookie)"}`},
		// json.Marshal escapes < and > — so did Fiber's encoder, so these
		// are the bytes the services have always sent.
		"malformed":    {"Token abc", `{"error":"malformed Authorization header (expected 'Bearer \u003ctoken\u003e')"}`},
		"invalid":      {"Bearer not.a.jwt", `{"error":"invalid or expired token"}`},
		"client token": {"Bearer " + sign(t, func(c jwt.MapClaims) { c["sub"] = "client:projects" }), `{"error":"invalid user id in token"}`},
	}
	for name, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		rec, _ := serve(auth(never), r)
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != tc.body {
			t.Errorf("%s: %d %s, want 401 %s", name, rec.Code, rec.Body.String(), tc.body)
		}
	}
}

func TestAuthenticateInstallsPrincipal(t *testing.T) {
	for _, viaCookie := range []bool{false, true} {
		var got Principal
		var cred Credential
		h := Authenticate(AuthOptions{Verifier: testVerifier(""), CookieName: "systicore_session"})(
			http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, _ = PrincipalFrom(r.Context())
				cred, _ = credentialFrom(r.Context())
				zerolog.Ctx(r.Context()).Info().Msg("in handler")
			}))
		token := sign(t, nil)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if viaCookie {
			r.AddCookie(&http.Cookie{Name: "systicore_session", Value: token})
		} else {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec, log := serve(h, r)

		if rec.Code != http.StatusOK {
			t.Fatalf("cookie=%v: status %d %s", viaCookie, rec.Code, rec.Body.String())
		}
		if got.UserID != 42 || got.Username != "ada" || got.Email != "ada@example.com" || got.Name != "Ada Lovelace" || got.TokenExp.IsZero() || got.DevFallback {
			t.Errorf("cookie=%v: principal = %+v", viaCookie, got)
		}
		wantSource := TokenHeader
		if viaCookie {
			wantSource = TokenCookie
		}
		if cred.Token != token || cred.Source != wantSource || cred.CookieName != "systicore_session" {
			t.Errorf("cookie=%v: credential = %+v", viaCookie, cred)
		}
		if !strings.Contains(log, `"user_id":42,"message":"in handler"`) {
			t.Errorf("cookie=%v: the handler's log line lacks user_id: %s", viaCookie, log)
		}
	}
}

func TestAuthenticateDevFallback(t *testing.T) {
	var got Principal
	var permissions []string
	h := Authenticate(AuthOptions{Verifier: testVerifier(""), DevFallback: true})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, _ = PrincipalFrom(r.Context())
			permissions, _ = PermissionsFrom(r.Context())
		}))

	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))
	if !got.DevFallback || got.UserID != 1 || permissions == nil || len(permissions) != 0 {
		t.Errorf("no token: principal %+v, permissions %v", got, permissions)
	}

	// A token that is sent is still checked.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer forged")
	if rec, _ := serve(h, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token under DevFallback: %d", rec.Code)
	}
}

func TestPrincipalHelpers(t *testing.T) {
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	if UserIDFrom(ctx) != 0 || HasPermission(ctx, "x") {
		t.Error("an anonymous context has no user and no permission")
	}
	ctx = WithPermissions(WithPrincipal(ctx, Principal{UserID: 7}), []string{"systicore.reports.read"})
	if UserIDFrom(ctx) != 7 || !HasPermission(ctx, "systicore.reports.read") || HasPermission(ctx, "systicore.reports.update") {
		t.Error("helpers disagree with the context")
	}
}
