package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fishman/neocloud-catalog/service/internal/auth/authtest"
)

const testAudience = "catalog-service"

// An ICP token for the audience authenticates; only the admin role writes.
func TestOIDCPrincipal(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := New(nil, OIDCConfig{IssuerURL: iss.URL, Audience: testAudience})
	for name, c := range map[string]struct {
		roles     []string
		wantWrite bool
	}{
		"admin":     {[]string{"viewer", "admin"}, true},
		"no admin":  {[]string{"viewer"}, false},
		"no roles":  {nil, false},
		"admin-ish": {[]string{"Admin", "administrator"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := a.Authenticate(context.Background(), iss.Token(t, iss.Claims(testAudience, c.roles...)))
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if p.Subject != "user-1" || p.CanWrite() != c.wantWrite {
				t.Errorf("principal %+v: subject user-1 and CanWrite %v wanted", p, c.wantWrite)
			}
		})
	}
}

func TestOIDCRejects(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := New(nil, OIDCConfig{IssuerURL: iss.URL, Audience: testAudience})
	other := authtest.NewKey(t)
	for name, mutate := range map[string]func(map[string]any) string{
		"wrong issuer":   func(c map[string]any) string { c["iss"] = "https://evil.example.com"; return iss.Token(t, c) },
		"wrong audience": func(c map[string]any) string { c["aud"] = "neocloud-api"; return iss.Token(t, c) },
		"expired":        func(c map[string]any) string { c["exp"] = time.Now().Add(-time.Hour).Unix(); return iss.Token(t, c) },
		"bad signature":  func(c map[string]any) string { return authtest.Sign(t, other, c) },
		"not a jwt":      func(map[string]any) string { return "opaque-secret" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Authenticate(context.Background(), mutate(iss.Claims(testAudience, RoleAdmin))); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

// The service starts while the IdP is down and picks it up once it is back.
func TestOIDCDiscoveryRetried(t *testing.T) {
	iss := authtest.NewIssuer(t)
	iss.Down.Store(true)
	a := New(nil, OIDCConfig{IssuerURL: iss.URL, Audience: testAudience})
	tok := iss.Token(t, iss.Claims(testAudience))
	if _, err := a.Authenticate(context.Background(), tok); err == nil {
		t.Fatal("authenticated without discovery")
	}
	iss.Down.Store(false)
	if _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

type stubBackend struct{ calls int }

func (s *stubBackend) Authenticate(context.Context, string) (Principal, error) {
	s.calls++
	return Principal{Subject: "key"}, nil
}

// API keys never reach the issuer, and without an issuer nothing but an API
// key authenticates.
func TestDispatch(t *testing.T) {
	iss := authtest.NewIssuer(t)
	jwt := iss.Token(t, iss.Claims(testAudience))

	keys := &stubBackend{}
	a := New(nil, OIDCConfig{IssuerURL: iss.URL, Audience: testAudience}).(*dispatcher)
	a.apikey = keys
	if p, err := a.Authenticate(context.Background(), KeyPrefix+"ops_abc"); err != nil || p.Subject != "key" {
		t.Fatalf("api key: %+v, %v", p, err)
	}
	if p, err := a.Authenticate(context.Background(), jwt); err != nil || p.Subject != "user-1" {
		t.Fatalf("jwt: %+v, %v", p, err)
	}
	if keys.calls != 1 {
		t.Fatalf("api key backend calls = %d, want 1", keys.calls)
	}

	keys = &stubBackend{}
	a = New(nil, OIDCConfig{}).(*dispatcher)
	a.apikey = keys
	if _, err := a.Authenticate(context.Background(), jwt); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("jwt without issuer: err = %v, want ErrUnauthorized", err)
	}
	if _, err := a.Authenticate(context.Background(), KeyPrefix+"ops_abc"); err != nil || keys.calls != 1 {
		t.Fatalf("api key without issuer: %v (calls %d)", err, keys.calls)
	}
}

// A key is neocatalog_<prefix>_<secret> with 32 random bytes of secret; only
// its SHA-256 is kept.
func TestGenerateKey(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		plain, hash, err := GenerateKey("ops")
		if err != nil {
			t.Fatal(err)
		}
		secret, ok := strings.CutPrefix(plain, KeyPrefix+"ops_")
		if !ok || len(secret) != 43 || seen[plain] {
			t.Fatalf("key %q: want %sops_ and 43 fresh base64url characters", plain, KeyPrefix)
		}
		seen[plain] = true
		if sum := sha256.Sum256([]byte(plain)); hash != hex.EncodeToString(sum[:]) {
			t.Fatalf("hash %s is not the key's SHA-256", hash)
		}
	}
	if _, _, err := GenerateKey("a_b"); err == nil {
		t.Error("a prefix with an underscore was accepted")
	}
}

// A hanging issuer fails discovery after the client's timeout instead of
// holding every OIDC request.
func TestOIDCDiscoveryTimesOut(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	a := New(nil, OIDCConfig{IssuerURL: srv.URL, Audience: testAudience}).(*dispatcher)
	a.oidc.(*oidcBackend).client.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := a.Authenticate(context.Background(), "a.b.c")
	if err == nil || errors.Is(err, ErrUnauthorized) || time.Since(start) > 5*time.Second {
		t.Fatalf("authenticate against a hanging issuer: %v after %s, want a discovery error within the timeout", err, time.Since(start))
	}
}
