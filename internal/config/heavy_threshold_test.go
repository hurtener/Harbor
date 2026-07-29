// heavy_threshold_test.go — phase 213 (D-358): the heavy-content
// threshold split by purpose. The LLM-context arm
// (DefaultHeavyOutputThresholdBytes) rose to 128 KiB; the Console-facing
// inline-payload bound (DefaultConsoleInlinePayloadBytes) pinned at
// 32 KiB. These tests pin the resolution rules and — critically — pin
// the two constants APART, so a future "re-unification" fails a test
// instead of silently re-coupling the matrix.

package config_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// heavyFixtureYAML is the smallest valid config the threshold tests
// append an `artifacts:` block to.
const heavyFixtureYAML = `server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: harbor-test
state:
  driver: inmem
llm:
  provider: openrouter
  model: m
  api_key: k
  timeout: 30s
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 64
  idle_timeout: 60s
  drop_window: 1s
`

// TestHeavyThreshold_ConstantsAnswerDifferentQuestions is the matrix's
// mutation witness at the constant layer: the two bounds are separate
// questions that currently have different answers, and an author who
// "single-sources" them back together fails here rather than silently
// re-widening every Console-facing Protocol reply.
func TestHeavyThreshold_ConstantsAnswerDifferentQuestions(t *testing.T) {
	t.Parallel()
	if config.DefaultHeavyOutputThresholdBytes != 128*1024 {
		t.Errorf("DefaultHeavyOutputThresholdBytes = %d, want 131072",
			config.DefaultHeavyOutputThresholdBytes)
	}
	if config.DefaultConsoleInlinePayloadBytes != 32*1024 {
		t.Errorf("DefaultConsoleInlinePayloadBytes = %d, want 32768",
			config.DefaultConsoleInlinePayloadBytes)
	}
	if config.DefaultConsoleInlinePayloadBytes == config.DefaultHeavyOutputThresholdBytes {
		t.Fatal("the Console inline bound and the LLM-context heavy-output threshold " +
			"are the same value — they answer different questions and must not be re-coupled")
	}
}

// TestHeavyThreshold_UnsetResolvesToRaisedDefault — a configuration
// that names no `artifacts` block at all resolves the LLM-context arm
// to the raised default.
func TestHeavyThreshold_UnsetResolvesToRaisedDefault(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFromBytes(context.Background(), []byte(heavyFixtureYAML))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if got := cfg.Artifacts.HeavyOutputThresholdBytes; got != config.DefaultHeavyOutputThresholdBytes {
		t.Errorf("unset heavy_output_threshold_bytes resolved to %d, want %d",
			got, config.DefaultHeavyOutputThresholdBytes)
	}
}

// TestHeavyThreshold_OperatorValueWins — an existing deployment that
// pinned the OLD default keeps it. The operator's explicit value wins
// (CLAUDE.md §10): the raise is a default change, not a policy change.
func TestHeavyThreshold_OperatorValueWins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{"pins the old default", "  heavy_output_threshold_bytes: 32768\n", 32 * 1024},
		{"raises past the new default", "  heavy_output_threshold_bytes: 262144\n", 256 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.LoadFromBytes(context.Background(),
				[]byte(heavyFixtureYAML+"artifacts:\n  driver: inmem\n"+tc.yaml))
			if err != nil {
				t.Fatalf("LoadFromBytes: %v", err)
			}
			if got := cfg.Artifacts.HeavyOutputThresholdBytes; got != tc.want {
				t.Errorf("heavy_output_threshold_bytes = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestHeavyThreshold_NegativeRefusedByName — zero stays "unset", a
// negative value is refused by field name rather than silently
// reinterpreted (fail loudly, CLAUDE.md §5).
func TestHeavyThreshold_NegativeRefusedByName(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.APIKey = "openrouter", "m", "env.FAKE_TEST_KEY"

	cfg.Artifacts.HeavyOutputThresholdBytes = 0
	if err := cfg.ValidateCore(); err != nil {
		t.Errorf("zero (unset) must validate, got %v", err)
	}

	cfg.Artifacts.HeavyOutputThresholdBytes = -1
	err := cfg.ValidateCore()
	if err == nil {
		t.Fatal("a negative heavy_output_threshold_bytes must be refused")
	}
	if !strings.Contains(err.Error(), "artifacts.heavy_output_threshold_bytes") {
		t.Errorf("refusal does not name the field: %v", err)
	}
}
