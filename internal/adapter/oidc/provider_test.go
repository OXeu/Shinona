package oidc

import (
	"errors"
	"testing"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
)

func TestProductionDiscoveryRejectsLocalIssuer(t *testing.T) {
	_, err := New(t.Context(), "https://127.0.0.1:9", "fixture", "secret", "https://example.com/auth/oauth/callback")
	if !errors.Is(err, httpclient.ErrPrivateEndpoint) {
		t.Fatalf("local issuer returned %v", err)
	}
}
