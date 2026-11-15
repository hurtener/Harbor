package config_test

import (
	"os"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// D-513: protocol.steering_requires_agent_reach and
// protocol.require_execution_proof are opt-in booleans, default false,
// settable from YAML or the environment, and reject a non-boolean value.
func TestProtocolSecuritySettings(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("default off", func(t *testing.T) {
		cfg, err := config.LoadFromBytes(t.Context(), fixture)
		if err != nil {
			t.Fatalf("LoadFromBytes: %v", err)
		}
		if cfg.Protocol.SteeringRequiresAgentReach || cfg.Protocol.RequireExecutionProof {
			t.Fatalf("defaults = %+v, want both off", cfg.Protocol)
		}
	})

	t.Run("yaml on", func(t *testing.T) {
		data := append(append([]byte{}, fixture...), []byte("\nprotocol:\n  steering_requires_agent_reach: true\n  require_execution_proof: true\n")...)
		cfg, err := config.LoadFromBytes(t.Context(), data)
		if err != nil {
			t.Fatalf("LoadFromBytes: %v", err)
		}
		if !cfg.Protocol.SteeringRequiresAgentReach || !cfg.Protocol.RequireExecutionProof {
			t.Fatalf("protocol = %+v, want both on", cfg.Protocol)
		}
	})

	t.Run("yaml non-boolean rejected", func(t *testing.T) {
		for _, key := range []string{"steering_requires_agent_reach", "require_execution_proof"} {
			data := append(append([]byte{}, fixture...), []byte("\nprotocol:\n  "+key+": sometimes\n")...)
			if _, err := config.LoadFromBytes(t.Context(), data); err == nil {
				t.Fatalf("%s: non-boolean accepted", key)
			}
		}
	})

	t.Run("env on", func(t *testing.T) {
		t.Setenv("HARBOR_PROTOCOL_STEERING_REQUIRES_AGENT_REACH", "true")
		t.Setenv("HARBOR_PROTOCOL_REQUIRE_EXECUTION_PROOF", "true")
		cfg, err := config.LoadFromBytes(t.Context(), fixture)
		if err != nil {
			t.Fatalf("LoadFromBytes: %v", err)
		}
		if !cfg.Protocol.SteeringRequiresAgentReach || !cfg.Protocol.RequireExecutionProof {
			t.Fatalf("protocol = %+v, want both on from env", cfg.Protocol)
		}
	})

	t.Run("env non-boolean rejected", func(t *testing.T) {
		t.Setenv("HARBOR_PROTOCOL_REQUIRE_EXECUTION_PROOF", "sometimes")
		if _, err := config.LoadFromBytes(t.Context(), fixture); err == nil {
			t.Fatal("non-boolean env override accepted")
		}
	})
}
