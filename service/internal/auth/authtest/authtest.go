// SPDX-License-Identifier: Apache-2.0

// Package authtest is a test OIDC issuer: discovery, a JWKS and signed tokens.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer serves OIDC discovery and its JWKS; discovery fails while Down.
type Issuer struct {
	*httptest.Server
	Key  *rsa.PrivateKey
	Down atomic.Bool
}

// NewIssuer starts an issuer, closed when the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	iss := &Issuer{Key: NewKey(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if iss.Down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"issuer": iss.URL, "jwks_uri": iss.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &iss.Key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	iss.Server = httptest.NewServer(mux)
	t.Cleanup(iss.Close)
	return iss
}

// Claims are the issuer's claims for audience with roles, valid for an hour.
func (iss *Issuer) Claims(audience string, roles ...string) map[string]any {
	now := time.Now()
	c := map[string]any{"iss": iss.URL, "aud": audience, "sub": "user-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	if roles != nil {
		c["roles"] = roles
	}
	return c
}

// Token signs claims with the issuer's key.
func (iss *Issuer) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	return Sign(t, iss.Key, claims)
}

// NewKey returns a fresh RSA key.
func NewKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Sign returns claims as a JWT signed by key under the issuer's key id.
func Sign(t testing.TB, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
