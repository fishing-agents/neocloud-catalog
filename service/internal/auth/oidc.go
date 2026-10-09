package auth

import (
	"context"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

type oidcBackend struct {
	issuer, audience string

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

// verifierFor discovers the issuer on first use and caches the result. A failed
// discovery is not cached, so the service starts while the IdP is down and
// recovers on a later request. go-oidc caches the JWKS and refetches it on
// unknown keys.
func (o *oidcBackend) verifierFor(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier == nil {
		p, err := oidc.NewProvider(ctx, o.issuer)
		if err != nil {
			return nil, fmt.Errorf("auth: oidc discovery: %w", err)
		}
		o.verifier = p.Verifier(&oidc.Config{ClientID: o.audience})
	}
	return o.verifier, nil
}

// Authenticate verifies the token's signature, issuer, audience and expiry.
func (o *oidcBackend) Authenticate(ctx context.Context, token string) (Principal, error) {
	v, err := o.verifierFor(ctx)
	if err != nil {
		return Principal{}, err
	}
	t, err := v.Verify(ctx, token)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var c struct {
		Roles []string `json:"roles"`
	}
	if err := t.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return Principal{Subject: t.Subject, Roles: c.Roles}, nil
}
