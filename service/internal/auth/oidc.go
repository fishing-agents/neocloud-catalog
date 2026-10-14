// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// oidcTimeout bounds issuer requests: discovery holds the backend's lock, so a
// hanging issuer would queue every token.
const oidcTimeout = 10 * time.Second

type oidcBackend struct {
	issuer, audience string
	client           *http.Client

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

// verifierFor discovers the issuer on first use. Failures are not cached, so
// the service starts while the IdP is down and recovers later.
func (o *oidcBackend) verifierFor(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier == nil {
		// The key set keeps this context's client for its later JWKS fetches.
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, o.client), o.issuer)
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
