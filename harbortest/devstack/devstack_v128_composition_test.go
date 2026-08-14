package devstack

// devstack_v128_composition_test.go — the HA-56/61/64/65/66 composition
// regressions at the DEVSTACK seam (the exact wiring serve.Boot and the
// kit share, CLAUDE.md §17.6):
//
//   - the render-admission surface is wired ONLY when the operator
//     explicitly opted in — an OAuth broker's shared sealer alone never
//     enables it (the P1 opt-in fix), and
//   - an ENABLED surface with an unresolvable
//     `tools.oauth_token_kek_env` fails the devstack readiness LOUD
//     (never a silent fallback to the disabled surface).

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/llm/mock" // the dev-only mock LLM driver (outside the prod aggregator)
	_ "github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// v128KEKEnv is the test-dummy env slot naming the shared KEK (a
// documented dummy, never a real secret — CLAUDE.md §7 rule 2).
const v128KEKEnv = "HARBOR_DEVSTACK_V128_TEST_KEK"

// v128DummyKEKHex is a documented-dummy 32-byte hex KEK.
const v128DummyKEKHex = "0202020202020202020202020202020202020202020202020202020202020202"

// devstackV128Config returns a devstack-assemblable minimal config (the
// same shape the external suite's minimalConfig uses: all inmem drivers,
// the mock LLM) with the v1.28 knobs applied by the mutator.
func devstackV128Config(t *testing.T, mut func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:0",
			ShutdownGracePeriod: 1 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"ES256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "text",
			LogLevel:    "error",
			ServiceName: "harbor-devstack-v128-test",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
			ModelProfiles: map[string]config.LLMModelProfileConfig{
				"mock/echo": {
					ContextWindowTokens: 100000,
					TokenEstimator:      "chars_div_4",
				},
			},
		},
		Governance: config.GovernanceConfig{RepairAttempts: 1},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              2 * time.Second,
			DropWindow:               50 * time.Millisecond,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       24 * time.Hour,
			SweepInterval: 5 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    1 * time.Minute,
			ContinuationHopLimit: 4,
		},
		Distributed: config.DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 8,
		},
	}
	if mut != nil {
		mut(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("devstackV128Config: Validate: %v", err)
	}
	return cfg
}

// TestTryAssemble_AdmissionEnabled_MissingKEK_FailsLoud is the DEVSTACK
// readiness regression: an ENABLED render-admission surface
// (`tools.mcp_app_render_admission.enabled: true`) with an unresolvable
// `tools.oauth_token_kek_env` fails the devstack composition LOUD,
// naming the surface — even with no OAuth provider or credential
// broker declared. The kit must never silently fall back to the
// disabled surface.
func TestTryAssemble_AdmissionEnabled_MissingKEK_FailsLoud(t *testing.T) {
	// The env is deliberately UNKNOWN — resolveTokenKEK reads it as
	// unset/empty and fails loud.
	t.Setenv(v128KEKEnv, "")
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Tools.OAuthTokenKEKEnv = v128KEKEnv
		c.Tools.MCPAppRenderAdmission.Enabled = true
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err == nil {
		stack.Close()
		t.Fatal("TryAssemble with an enabled render-admission surface and an unresolvable KEK must fail loud")
	}
	if !strings.Contains(err.Error(), "tools.mcp_app_render_admission.enabled") {
		t.Fatalf("enabled-surface failure %q does not name tools.mcp_app_render_admission.enabled", err)
	}
}

// TestTryAssemble_AdmissionEnabled_InvalidKEK_FailsLoud is the same
// readiness regression for an INVALID (non-hex / wrong-length) KEK
// value.
func TestTryAssemble_AdmissionEnabled_InvalidKEK_FailsLoud(t *testing.T) {
	t.Setenv(v128KEKEnv, "not-hex!!")
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Tools.OAuthTokenKEKEnv = v128KEKEnv
		c.Tools.MCPAppRenderAdmission.Enabled = true
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err == nil {
		stack.Close()
		t.Fatal("TryAssemble with an enabled render-admission surface and an invalid KEK must fail loud")
	}
	if !strings.Contains(err.Error(), "tools.mcp_app_render_admission.enabled") {
		t.Fatalf("invalid-KEK failure %q does not name tools.mcp_app_render_admission.enabled", err)
	}
}

// TestTryAssemble_AdmissionDisabled_WithBroker_StaysUnwired is the
// DEVSTACK opt-in regression: an OAuth credential broker (whose shared
// sealer ResolveSharedKEKSealer reuses) configured WITHOUT the
// `tools.mcp_app_render_admission.enabled` flag assembles cleanly, and
// the exact WireRenderAdmission call the devstack makes — with the
// broker's resolved sealer and Enabled=false — returns the nil pair.
// Sealer availability is NOT feature enablement: ordinary OAuth/broker
// configuration alone must never wire the surface.
func TestTryAssemble_AdmissionDisabled_WithBroker_StaysUnwired(t *testing.T) {
	t.Setenv(v128KEKEnv, v128DummyKEKHex)
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Tools.OAuthTokenKEKEnv = v128KEKEnv
		c.Tools.OAuthCredentialBrokers = []config.ToolOAuthCredentialBrokerConfig{{
			Name:                   "broker-1",
			TokenURL:               "https://broker.example.com/exchange",
			AllowedDownstreamHosts: []string{"https://mcp.example.com"},
			AuthTokenEnv:           "HARBOR_DEVSTACK_V128_BROKER_TOKEN",
		}}
		// The flag is OFF (the zero value).
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		stack.Close()
		t.Fatalf("TryAssemble with a broker and the admission flag OFF must assemble cleanly: %v", err)
	}
	stack.Close()

	// The devstack's exact constructor seam: the broker's shared sealer
	// resolves, but WireRenderAdmission(Enabled: false, ...) returns the
	// nil pair — the P1 opt-in regression, pinned at the devstack seam.
	sealer, err := toolauth.NewSealerFromEnv(v128KEKEnv)
	if err != nil {
		t.Fatalf("NewSealerFromEnv: %v", err)
	}
	if sealer == nil {
		t.Fatal("fixture: shared sealer must resolve from the valid KEK env")
	}
	authz, gate, err := serve.WireRenderAdmission(serve.RenderAdmissionAuthorityDeps{
		Enabled: false,
		Sealer:  sealer,
	})
	if err != nil {
		t.Fatalf("WireRenderAdmission(disabled, broker sealer): %v", err)
	}
	if authz != nil || gate != nil {
		t.Fatalf("WireRenderAdmission(disabled, broker sealer) = (%v, %v), want (nil, nil) — a broker sealer alone must never wire the render-admission surface", authz, gate)
	}
}
