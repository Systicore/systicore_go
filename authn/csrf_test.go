package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeSameSite(t *testing.T) {
	for in, want := range map[string]http.SameSite{
		"Strict": http.SameSiteStrictMode, "STRICT": http.SameSiteStrictMode,
		"None": http.SameSiteNoneMode, " none ": http.SameSiteNoneMode,
		"Lax": http.SameSiteLaxMode, "": http.SameSiteLaxMode, "laxx": http.SameSiteLaxMode,
	} {
		if got := NormalizeSameSite(in); got != want {
			t.Errorf("NormalizeSameSite(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCSRFIssuesTheCookieOnSafeRequests(t *testing.T) {
	h := CSRF(CSRFOptions{Secure: true, SameSite: "none", Domain: ".systicore.hu"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want one csrf cookie, got %v", cookies)
	}
	c := cookies[0]
	if c.Name != CSRFCookieName || len(c.Value) != 64 || c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteNoneMode || c.Domain != "systicore.hu" || c.Path != "/" {
		t.Errorf("cookie = %+v", c)
	}

	// A client that has one keeps it: the backends on the shared domain
	// check the same value.
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodHead, "/", nil)
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "existing"})
	h.ServeHTTP(rec, r)
	if len(rec.Result().Cookies()) != 0 {
		t.Error("an existing csrf cookie must not be replaced")
	}
}

func TestCSRFGuardsStateChangingRequests(t *testing.T) {
	cases := []struct {
		name          string
		cookie        string
		header        string
		authorization string
		code          int
		body          string
	}{
		{"neither", "", "", "", 403, `{"error":"csrf token missing (need both csrf_token cookie and X-CSRF-Token header)"}`},
		{"cookie only", "abc", "", "", 403, `{"error":"csrf token missing (need both csrf_token cookie and X-CSRF-Token header)"}`},
		{"mismatch", "abc", "abd", "", 403, `{"error":"csrf token mismatch"}`},
		{"match", "abc", "abc", "", 200, ""},
		{"bearer skips the check", "", "", "Bearer t", 200, ""},
		{"bearer in any case skips too", "", "", "bearer t", 200, ""},
		// Only a real Bearer header proves the request is not a browser's.
		{"other scheme does not skip", "", "", "Basic dTpw", 403, `{"error":"csrf token missing (need both csrf_token cookie and X-CSRF-Token header)"}`},
	}
	for _, tc := range cases {
		h := CSRF(CSRFOptions{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := httptest.NewRequest(http.MethodPost, "/api/v1/report", nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
		}
		if tc.header != "" {
			r.Header.Set(CSRFHeaderName, tc.header)
		}
		if tc.authorization != "" {
			r.Header.Set("Authorization", tc.authorization)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tc.code || rec.Body.String() != tc.body {
			t.Errorf("%s: %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
}
