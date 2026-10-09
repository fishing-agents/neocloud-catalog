// Package auth authenticates bearer tokens as neocloud-api does. Tokens with
// the API-key prefix go to the apikey backend (opaque random keys, SHA-256 at
// rest, minted by `catalog-service auth issue`); any other token is an OIDC
// JWT from the configured issuer (ICP), or unauthorized without one. Tokens
// are never logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/model"
)

var ErrUnauthorized = errors.New("auth: unauthorized")

// KeyPrefix starts every API key; it routes a token to the apikey backend.
const KeyPrefix = "neocatalog_"

// RoleAdmin in an OIDC token's roles claim lets it write. API keys carry it:
// only operators mint them.
const RoleAdmin = "admin"

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	Roles   []string
}

// CanWrite reports whether p may change the catalog.
func (p Principal) CanWrite() bool { return slices.Contains(p.Roles, RoleAdmin) }

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// OIDCConfig configures the OIDC backend; an empty IssuerURL disables it.
type OIDCConfig struct {
	IssuerURL string
	Audience  string
}

func New(db *gorm.DB, oc OIDCConfig) Authenticator {
	d := &dispatcher{apikey: &apikeyBackend{db: db}}
	if oc.IssuerURL != "" {
		d.oidc = &oidcBackend{issuer: oc.IssuerURL, audience: oc.Audience}
	}
	return d
}

type dispatcher struct {
	apikey Authenticator
	oidc   Authenticator
}

func (d *dispatcher) Authenticate(ctx context.Context, token string) (Principal, error) {
	switch {
	case strings.HasPrefix(token, KeyPrefix):
		return d.apikey.Authenticate(ctx, token)
	case d.oidc != nil:
		return d.oidc.Authenticate(ctx, token)
	}
	return Principal{}, ErrUnauthorized
}

type apikeyBackend struct{ db *gorm.DB }

func (a *apikeyBackend) Authenticate(ctx context.Context, token string) (Principal, error) {
	h := sha256.Sum256([]byte(token))
	hashHex := hex.EncodeToString(h[:])
	var keys []model.APIKey
	if err := a.db.WithContext(ctx).Where("key_hash = ?", hashHex).Limit(1).Find(&keys).Error; err != nil {
		return Principal{}, err
	}
	if len(keys) == 0 || subtle.ConstantTimeCompare([]byte(hashHex), []byte(keys[0].KeyHash)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	if k := keys[0]; k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return Principal{}, ErrUnauthorized
	}
	return Principal{Subject: keys[0].Name, Roles: []string{RoleAdmin}}, nil
}

var prefixRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// GenerateKey mints neocatalog_<prefix>_<secret> with 32 random bytes of
// secret and returns it with its SHA-256, the only part stored. The prefix
// labels the key; it holds no underscore, so the format stays parseable.
func GenerateKey(prefix string) (plaintext, hashHex string, err error) {
	if !prefixRE.MatchString(prefix) {
		return "", "", fmt.Errorf("key prefix %q: want lower-case letters, digits and dashes", prefix)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plaintext = KeyPrefix + prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(plaintext))
	return plaintext, hex.EncodeToString(h[:]), nil
}

// Issue mints an API key named name, stores its hash and returns the key,
// which exists nowhere else, and its row. A nil expiresAt never expires.
func Issue(ctx context.Context, db *gorm.DB, name, prefix string, expiresAt *time.Time) (string, model.APIKey, error) {
	plaintext, hashHex, err := GenerateKey(prefix)
	if err != nil {
		return "", model.APIKey{}, err
	}
	k := model.APIKey{Name: name, Prefix: prefix, KeyHash: hashHex, ExpiresAt: expiresAt}
	if err := db.WithContext(ctx).Create(&k).Error; err != nil {
		return "", model.APIKey{}, err
	}
	return plaintext, k, nil
}
