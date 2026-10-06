package serviceauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// expirySlack is taken off a token's lifetime when it is cached, so a call
// never sets out with a token that expires on the way.
const expirySlack = 60 * time.Second

// ClientOptions configure a TokenSource.
type ClientOptions struct {
	// IssuerURL is the authenticator, AUTH_ISSUER_URL; the token endpoint
	// is {IssuerURL}/api/v1/oauth/token.
	IssuerURL    string
	ClientID     string // SVC_OAUTH_CLIENT_ID; "" is the development bypass
	ClientSecret string // SVC_OAUTH_CLIENT_SECRET
	// Scopes are requested together. The authenticator refuses the whole
	// token when one of them is not granted to the client, so a scope added
	// here must be granted (oauth_client_scopes) before the build ships.
	Scopes []string
	// HTTPClient makes the token requests; nil is a client with a
	// 10-second timeout. Pass one whose transport counts the requests to
	// keep them in the service's metrics.
	HTTPClient *http.Client
}

// TokenSource mints and caches the client_credentials token a service
// presents to the others. Safe for concurrent use.
type TokenSource struct {
	o        ClientOptions
	endpoint string
	now      func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time

	flight singleflight.Group
}

// NewTokenSource builds a TokenSource.
func NewTokenSource(o ClientOptions) *TokenSource {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &TokenSource{
		o:        o,
		endpoint: strings.TrimRight(o.IssuerURL, "/") + "/api/v1/oauth/token",
		now:      time.Now,
	}
}

// Token returns a valid access token: the cached one, or a fresh one when
// it is missing or within a minute of expiry. Concurrent callers share one
// token request; one of them giving up does not fail the others.
//
// With no ClientID it returns "" and no error — the development bypass:
// the caller sends no Authorization header, which a server with DevBypass
// admits. In production an empty client id must be refused at boot.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if s.o.ClientID == "" {
		return "", nil
	}
	if token, ok := s.cached(); ok {
		return token, nil
	}
	v, err, _ := s.flight.Do("token", func() (any, error) {
		if token, ok := s.cached(); ok {
			return token, nil
		}
		token, lifetime, err := s.mint(context.WithoutCancel(ctx))
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		// A token shorter-lived than the slack is used once, not cached.
		s.token, s.expiry = token, s.now().Add(lifetime-expirySlack)
		s.mu.Unlock()
		return token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (s *TokenSource) cached() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expiry) {
		return s.token, true
	}
	return "", false
}

// mint is the client_credentials exchange.
func (s *TokenSource) mint(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.o.ClientID},
		"client_secret": {s.o.ClientSecret},
		"scope":         {strings.Join(s.o.Scopes, " ")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("serviceauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.o.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("serviceauth: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusOK {
		// A snippet only: it is an auth endpoint, keep logs bounded.
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return "", 0, fmt.Errorf("serviceauth: token endpoint answered %d: %s", resp.StatusCode, snippet)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", 0, fmt.Errorf("serviceauth: decode token response: %w", err)
	}
	if answer.AccessToken == "" {
		return "", 0, errors.New("serviceauth: token endpoint answered no access_token")
	}
	return answer.AccessToken, time.Duration(answer.ExpiresIn) * time.Second, nil
}
