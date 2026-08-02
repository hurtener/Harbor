package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestBuildSignedCapability_RequestedScopeOutsideBootCeilingRejected(t *testing.T) {
	builder := &ProviderBuilder{brokers: map[string]config.ToolOAuthCredentialBrokerConfig{
		"broker": {Name: "broker", ScopeCeiling: []string{"read"}},
	}}
	binding := SignedCapabilityExchangeBinding{
		TenantID: "tenant", UserID: "user", SessionID: "session", AgentID: "agent",
		ProviderName: "provider", CapabilityRevision: "revision", PairFingerprint: "pair",
		URLDigest: "url-digest", SinkDigest: "sink-digest", Audience: "audience",
		Resource: "https://mcp.example.test",
	}

	provider, err := builder.BuildSignedCapability(context.Background(), "broker", binding, []string{"read", "admin"})
	if provider != nil {
		t.Fatal("provider was built after a signed request exceeded the boot scope ceiling")
	}
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("BuildSignedCapability = %v, want ErrConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), `scope "admin" outside the boot ceiling`) {
		t.Fatalf("BuildSignedCapability error omits refused scope: %v", err)
	}
}
