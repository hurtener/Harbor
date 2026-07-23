// build_wire_test.go — the DEV-GATED wire-carried OAuth provider build
// (HA-32 / D-340), in-package coverage of the incomplete-descriptor guard. The
// end-to-end build + derived-downstream-sink + SSRF-refusal coverage lives in
// test/integration/phase199_wire_oauth_test.go (it needs the real tokenexchange
// driver, which cannot be imported in-package without a cycle).
package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// wireBuilder builds a ProviderBuilder with a seeded crypto chain (one dummy
// broker declared so NewProviderBuilder builds the shared KEK → sealer → token
// store a wire provider shares). The broker itself is never resolved by a wire
// descriptor.
func wireBuilder(t *testing.T) *ProviderBuilder {
	t.Helper()
	t.Setenv("HARBOR_WIRE_TEST_KEK", dummyKEKHex)
	cfg := config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_WIRE_TEST_KEK",
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{
			{Name: "seed", TokenURL: "https://broker/token", AllowedDownstreamHosts: []string{"x"}, AuthTokenEnv: "HARBOR_WIRE_TEST_KEK"},
		},
	}
	b, err := NewProviderBuilder(context.Background(), cfg, bpDeps(t))
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	return b
}

func TestBuildWire_IncompleteDescriptorFailsLoud(t *testing.T) {
	b := wireBuilder(t)
	cases := map[string]WireProviderDescriptor{
		"no token_url":  {Name: "w", RemoteURL: "https://c/x", RemoteAuthTokenEnv: "E"},
		"no remote url": {Name: "w", TokenURL: "https://b/token", RemoteAuthTokenEnv: "E"},
		"no auth env":   {Name: "w", TokenURL: "https://b/token", RemoteURL: "https://c/x"},
	}
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := b.BuildWire(context.Background(), desc)
			if !errors.Is(err, ErrWireDescriptorIncomplete) {
				t.Fatalf("want ErrWireDescriptorIncomplete, got %v", err)
			}
		})
	}
}
