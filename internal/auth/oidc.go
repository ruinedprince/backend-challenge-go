// Package auth validates bearer tokens at the API boundary.
package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Principal struct {
	ProviderID string
	Internal   bool
}

type Verifier struct {
	issuer           string
	internalClientID string
	mu               sync.Mutex
	verify           *oidc.IDTokenVerifier
}

func NewVerifier(issuer, internalClientID string) *Verifier {
	return &Verifier{issuer: issuer, internalClientID: internalClientID}
}

// Authenticate verifies issuer, signature and expiration. Keycloak service
// account access tokens identify the calling client in the azp claim.
func (v *Verifier) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	parts := strings.Fields(authorization)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return Principal{}, fmt.Errorf("bearer token is required")
	}
	verifier, err := v.getVerifier(ctx)
	if err != nil {
		return Principal{}, err
	}
	token, err := verifier.Verify(ctx, parts[1])
	if err != nil {
		return Principal{}, fmt.Errorf("verify access token: %w", err)
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err := token.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("read access token claims: %w", err)
	}
	if claims.AuthorizedParty == "" {
		return Principal{}, fmt.Errorf("access token has no authorized party")
	}
	return Principal{ProviderID: claims.AuthorizedParty, Internal: claims.AuthorizedParty == v.internalClientID}, nil
}

func (v *Verifier) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verify != nil {
		return v.verify, nil
	}
	provider, err := oidc.NewProvider(ctx, v.issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer: %w", err)
	}
	// Keycloak's client-credentials token commonly has audience "account".
	// We authenticate the calling client through the signed azp claim instead.
	v.verify = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return v.verify, nil
}
