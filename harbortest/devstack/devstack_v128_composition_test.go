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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/llm/mock" // the dev-only mock LLM driver (outside the prod aggregator)
	_ "github.com/hurtener/Harbor/internal/planner/react"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/skills"
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

// TestTryAssemble_AdmissionDisabled_ConfiguredKEK_WiresImportService is
// the DEVSTACK consumer-independent-sealer regression: an explicitly
// configured VALID `tools.oauth_token_kek_env` with the HA-56 flag OFF
// and NO OAuth broker resolves the ONE shared sealer, so the HA-61
// user-skill-import service is wired (the import_validate route answers
// non-501). Sealer availability never enables the render-admission
// surface itself — WireRenderAdmission(Enabled: false, ...) still
// returns the nil pair — but the shared KEK is NOT a render-admission
// toggle: HA-61 import needs it with the surface disabled.
func TestTryAssemble_AdmissionDisabled_ConfiguredKEK_WiresImportService(t *testing.T) {
	t.Setenv(v128KEKEnv, v128DummyKEKHex)
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Tools.OAuthTokenKEKEnv = v128KEKEnv
		c.Skills = config.SkillsConfig{
			Driver: "localdb",
			DSN:    filepath.Join(t.TempDir(), "skills.sqlite"),
		}
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble (configured KEK, admission disabled): %v", err)
	}
	defer stack.Close()

	// The import route is WIRED: a non-501 answer proves the shared
	// sealer resolved for HA-61 even though render admission is disabled
	// and no OAuth broker is present. (The artifact is deliberately
	// missing — the assertion is that the service seam answers, not that
	// the review succeeds.)
	body := `{"identity":{"tenant":"dev","user":"dev","session":"dev"},"agent_id":"harbor-dev-agent","artifact_id":"art-missing"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/user/skills/import_validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+stack.Token)
	rec := httptest.NewRecorder()
	stack.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotImplemented {
		t.Fatalf("import_validate answered 501 with a configured valid shared KEK (admission disabled, no broker) — the shared sealer must resolve for HA-61 import")
	}
}

// TestTryAssemble_NoBootPacks_PreviewAvailable is the DEVSTACK P1
// regression: with NO boot declarations the read-only composition
// preview stays available (never 501) — an independently persisted
// active revision composes as provenance "revision" through the empty
// immutable boot contribution. Boot config removal neither erases the
// durable revision nor weakens the boot+revision collision defense.
func TestTryAssemble_NoBootPacks_PreviewAvailable(t *testing.T) {
	cfg := devstackV128Config(t, nil)
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble (no boot packs): %v", err)
	}
	defer stack.Close()

	// Seed an independently persisted agent-scope active revision (the
	// canonical reserved tenant-agent control-plane slot) with one pack
	// item — the exact durable state that must appear in the preview.
	scopeQ, err := agentcfg.AgentScope(DefaultDevTenant, stack.AgentConfigID)
	if err != nil {
		t.Fatalf("AgentScope: %v", err)
	}
	ctx := context.Background()
	if _, err := stack.AgentConfig.SetRevision(ctx, scopeQ, stack.AgentConfigID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "runbook", Title: "Runbook", Trigger: "when asked about the runbook",
			Steps: []string{"do the thing", "verify the thing"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision (agent scope): %v", err)
	}

	// The composition preview route must be LIVE without any boot
	// declaration and must surface the persisted revision as provenance
	// "revision". Driven through a REAL httptest server (the same shape
	// the integration suite uses) so the auth middleware + transport
	// chain is exactly the wire path.
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()
	bodyBytes, err := json.Marshal(prototypes.AgentConfigCompositionPreviewRequest{
		Identity: prototypes.IdentityScope{
			Tenant:  "dev",
			User:    "dev",
			Session: "dev",
		},
		AgentID: "harbor-dev-agent",
	})
	if err != nil {
		t.Fatalf("marshal preview request: %v", err)
	}
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/v1/agent_config/composition/preview",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		t.Fatalf("build preview request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+stack.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preview request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preview body: %v", err)
	}
	respBody := string(respBytes)
	if resp.StatusCode == http.StatusNotImplemented {
		t.Fatalf("composition/preview answered 501 with no boot packs declared — the preview must stay available with an empty immutable boot contribution")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("composition/preview status = %d body=%s, want 200", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, `"outcome":"available"`) {
		t.Fatalf("composition/preview outcome not available: %s", respBody)
	}
	if !strings.Contains(respBody, `"source":"revision"`) {
		t.Fatalf("composition/preview items do not carry revision provenance: %s", respBody)
	}
	if !strings.Contains(respBody, `"name":"runbook"`) {
		t.Fatalf("composition/preview items do not include the persisted runbook pack: %s", respBody)
	}
}

// TestTryAssemble_NoBootPacks_MutationPathDoesNotPanic is the P0
// regression for the no-boot ownership/mutation path: with NO boot
// declarations the mux must carry an ACTUAL-nil BootOwnership — never a
// typed-nil `*bootpacks.Index` inside a non-nil interface — so a pack
// mutation through the real wire stays fully mutable and completes with
// 200 instead of panicking on the guard's first OwnsName call.
func TestTryAssemble_NoBootPacks_MutationPathDoesNotPanic(t *testing.T) {
	cfg := devstackV128Config(t, nil)
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble (no boot packs): %v", err)
	}
	defer stack.Close()

	body := `{"identity":{"tenant":"dev","user":"dev","session":"dev"},"agent_id":"harbor-dev-agent","skill":{"name":"free-pack","trigger":"trigger","steps":["step"]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/agent_packs/upsert", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+stack.Token)
	rec := httptest.NewRecorder()
	stack.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-boot pack upsert status = %d body=%s, want 200 (guards inert — no panic on a nil boot owner)", rec.Code, rec.Body.String())
	}
}
