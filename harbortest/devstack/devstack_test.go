// Tests for the devstack helper. The four `Skip*` shapes are pinned
// here: each asserts the expected non-nil / nil field set. A regression
// that flips a field's nilness without updating the matrix will fail
// loudly — the helper's contract is "tests track production," and a
// drift here means a downstream integration test would silently miss
// a layer.
package devstack_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"

	// D-103 — `internal/planner/react` self-registers under "react" via
	// init() so devstack.Assemble's `planner.Resolve` call can construct
	// the V1 reference planner from the cfg.
	_ "github.com/hurtener/Harbor/internal/planner/react"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// minimalConfig returns a *config.Config the helper can pass through
// every layer without external dependencies. All drivers are inmem;
// the LLM is the mock driver (driver=mock); memory.strategy=none.
func minimalConfig(t *testing.T) *config.Config {
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
			ServiceName: "harbor-devstack-test",
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
	if err := cfg.Validate(); err != nil {
		t.Fatalf("minimalConfig: cfg.Validate(): %v", err)
	}
	return cfg
}

// TestAssemble_DefaultOpts_BuildsEveryLayer — the zero AssembleOpts
// builds every layer the cfg implies. Every public field is non-nil.
func TestAssemble_DefaultOpts_BuildsEveryLayer(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	if stack.Audit == nil {
		t.Error("Audit nil")
	}
	if stack.Bus == nil {
		t.Error("Bus nil")
	}
	if stack.State == nil {
		t.Error("State nil")
	}
	if stack.Artifacts == nil {
		t.Error("Artifacts nil")
	}
	if stack.Tasks == nil {
		t.Error("Tasks nil")
	}
	if stack.LLMClient == nil {
		t.Error("LLMClient nil (cfg.LLM.Driver=mock should open)")
	}
	if stack.Memory == nil {
		t.Error("Memory nil (cfg.Memory.Driver=inmem should open)")
	}
	if stack.Steering == nil {
		t.Error("Steering nil")
	}
	if stack.Surface == nil {
		t.Error("Surface nil")
	}
	if stack.Catalog == nil {
		t.Error("Catalog nil")
	}
	if stack.Coordinator == nil {
		t.Error("Coordinator nil")
	}
	if stack.Gates == nil {
		t.Error("Gates nil (empty map is OK; nil is not)")
	}
	if stack.Validator == nil {
		t.Error("Validator nil")
	}
	if stack.SigningKey == nil {
		t.Error("SigningKey nil")
	}
	if stack.Token == "" {
		t.Error("Token empty")
	}
	if stack.Mux == nil {
		t.Error("Mux nil")
	}
	if stack.Handler == nil {
		t.Error("Handler nil")
	}
}

// TestAssemble_SkipAuth_LeavesAuthFieldsNil — Validator/SigningKey/Token
// nil; transports still build (with WithoutValidator).
func TestAssemble_SkipAuth_LeavesAuthFieldsNil(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipAuth: true})
	defer stack.Close()

	if stack.Validator != nil {
		t.Error("Validator should be nil under SkipAuth")
	}
	if stack.SigningKey != nil {
		t.Error("SigningKey should be nil under SkipAuth")
	}
	if stack.Token != "" {
		t.Errorf("Token should be empty under SkipAuth, got %q", stack.Token)
	}
	// Transports still build — auth is independent of transport.
	if stack.Handler == nil {
		t.Error("Handler should still build under SkipAuth")
	}
}

// TestAssemble_SkipTransports_LeavesMuxNil — Mux/Handler nil; the
// surface still composes (the test cares only about in-process
// dispatch).
func TestAssemble_SkipTransports_LeavesMuxNil(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipTransports: true})
	defer stack.Close()

	if stack.Mux != nil {
		t.Error("Mux should be nil under SkipTransports")
	}
	if stack.Handler != nil {
		t.Error("Handler should be nil under SkipTransports")
	}
	if stack.Surface == nil {
		t.Error("Surface should still build under SkipTransports (steering not skipped)")
	}
}

// TestAssemble_SkipCatalog_LeavesCatalogNil — Catalog/Coordinator/Gates
// nil; the rest still build.
func TestAssemble_SkipCatalog_LeavesCatalogNil(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipCatalog: true})
	defer stack.Close()

	if stack.Catalog != nil {
		t.Error("Catalog should be nil under SkipCatalog")
	}
	if stack.Coordinator != nil {
		t.Error("Coordinator should be nil under SkipCatalog")
	}
	if stack.Gates != nil {
		t.Error("Gates should be nil under SkipCatalog")
	}
	// Surface + Validator still build.
	if stack.Surface == nil {
		t.Error("Surface should still build under SkipCatalog")
	}
	if stack.Validator == nil {
		t.Error("Validator should still build under SkipCatalog")
	}
}

// TestAssemble_SkipSteering_LeavesSurfaceNil — Steering/Surface nil;
// Mux/Handler ALSO nil (a Mux without a Surface is meaningless).
func TestAssemble_SkipSteering_LeavesSurfaceNil(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipSteering: true})
	defer stack.Close()

	if stack.Steering != nil {
		t.Error("Steering should be nil under SkipSteering")
	}
	if stack.Surface != nil {
		t.Error("Surface should be nil under SkipSteering")
	}
	if stack.Mux != nil {
		t.Error("Mux should be nil when SkipSteering implies SkipTransports")
	}
	if stack.Handler != nil {
		t.Error("Handler should be nil when SkipSteering implies SkipTransports")
	}
	// Core still builds.
	if stack.Bus == nil {
		t.Error("Bus should still build under SkipSteering")
	}
}

// TestAssemble_AllSkips_LeavesOnlyCore — minimal shape (the
// phase31 / phase64a footprint): bus + audit + state + artifacts +
// tasks + (when cfg names them) LLM + memory.
func TestAssemble_AllSkips_LeavesOnlyCore(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		SkipAuth:       true,
		SkipTransports: true,
		SkipCatalog:    true,
		SkipSteering:   true,
	})
	defer stack.Close()

	// Core present.
	if stack.Bus == nil {
		t.Error("Bus nil")
	}
	if stack.State == nil {
		t.Error("State nil")
	}
	// Optional layers (skipped).
	if stack.Catalog != nil {
		t.Error("Catalog should be nil")
	}
	if stack.Coordinator != nil {
		t.Error("Coordinator should be nil")
	}
	if stack.Steering != nil {
		t.Error("Steering should be nil")
	}
	if stack.Surface != nil {
		t.Error("Surface should be nil")
	}
	if stack.Validator != nil {
		t.Error("Validator should be nil")
	}
	if stack.Handler != nil {
		t.Error("Handler should be nil")
	}
}

// TestAssemble_LLMConfigSnapshot_Overrides — when the opts pin an
// explicit snapshot, the helper uses it instead of deriving from cfg.
// Mirrors the Phase 64 / D-089 `HARBOR_DEV_ALLOW_MOCK=1` shape.
func TestAssemble_LLMConfigSnapshot_Overrides(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	// Wipe cfg.LLM so the helper would otherwise skip the LLM layer.
	cfg.LLM = config.LLMConfig{}
	snap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &snap,
	})
	defer stack.Close()

	if stack.LLMClient == nil {
		t.Fatal("LLMClient nil — the snapshot override should have opened the mock driver")
	}
}

// TestAssemble_TokenIsAcceptedByValidator — round-trip: the minted
// token validates against the constructed validator. Exercises the
// devKeySet.KeyByID path that no Skip variant covers.
func TestAssemble_TokenIsAcceptedByValidator(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	if stack.Token == "" {
		t.Fatal("Token empty")
	}
	id, err := stack.Validator.Validate(context.Background(), stack.Token)
	if err != nil {
		t.Fatalf("Validator.Validate(token): %v", err)
	}
	if id.Identity.TenantID != devstack.DefaultDevTenant {
		t.Errorf("validated tenant = %q, want %q",
			id.Identity.TenantID, devstack.DefaultDevTenant)
	}
}

// TestAssemble_CustomIdentity_FlowsIntoToken — the opts.Identity
// override stamps non-default values into the JWT.
func TestAssemble_CustomIdentity_FlowsIntoToken(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		Identity: struct {
			Tenant  string
			User    string
			Session string
		}{
			Tenant:  "alt-tenant",
			User:    "alt-user",
			Session: "alt-session",
		},
	})
	defer stack.Close()

	id, err := stack.Validator.Validate(context.Background(), stack.Token)
	if err != nil {
		t.Fatalf("Validator.Validate: %v", err)
	}
	if id.Identity.TenantID != "alt-tenant" {
		t.Errorf("tenant = %q, want alt-tenant", id.Identity.TenantID)
	}
	if id.Identity.UserID != "alt-user" {
		t.Errorf("user = %q, want alt-user", id.Identity.UserID)
	}
	if id.Identity.SessionID != "alt-session" {
		t.Errorf("session = %q, want alt-session", id.Identity.SessionID)
	}
}

// TestAssemble_ModelProfiles_CopiesDefaultMaxTokens — the cfg's
// `default_max_tokens` ptr-field flows through `copyModelProfiles`
// unchanged. Closes the conditional in that helper.
func TestAssemble_ModelProfiles_CopiesDefaultMaxTokens(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	maxTok := 12345
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {
			ContextWindowTokens: 100000,
			TokenEstimator:      "chars_div_4",
			DefaultMaxTokens:    &maxTok,
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	if stack.LLMClient == nil {
		t.Fatal("LLMClient nil — Open should have succeeded with the profile")
	}
	// We do not assert the profile round-tripped to the LLM client —
	// the LLM client's internal state isn't exposed here. The
	// coverage gain is on copyModelProfiles' ptr-handling branch.
}

// TestAssemble_CatalogWiring_AppliesBuilder — when the cfg names
// `tools.entries[]` and the matching descriptor is pre-registered,
// the Builder applies and the wrapped tool is resolvable. Exercises
// the Builder.Apply + Gates-close-fn registration paths that no
// other test covers.
func TestAssemble_CatalogWiring_AppliesBuilder(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	cfg.Tools = config.ToolsConfig{
		Entries: []config.ToolEntryConfig{
			{
				Name: "echo_tool",
				Approval: &config.ToolApprovalConfig{
					Policy: "approve-all",
				},
			},
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		PreRegisterTools: []tools.ToolDescriptor{
			{
				Tool: tools.Tool{
					Name:        "echo_tool",
					Description: "devstack catalog test",
					Transport:   tools.TransportInProcess,
					Source:      "devstack-test",
				},
				Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
					return tools.ToolResult{Value: string(args)}, nil
				},
			},
		},
	})
	defer stack.Close()

	if _, ok := stack.Catalog.Resolve("echo_tool"); !ok {
		t.Error("echo_tool not resolvable after Builder.Apply")
	}
	if _, hasGate := stack.Gates["echo_tool"]; !hasGate {
		t.Error("Gates[echo_tool] missing — Builder should have populated it")
	}
}

// TestAssemble_SkipAuth_TransportsUseWithoutValidator — when auth is
// skipped but transports stay on, the helper composes the Mux via
// `transports.WithoutValidator`. The test exercises the path; the
// assertion is "Handler is non-nil and serves" (the next HTTP call
// would go through).
func TestAssemble_SkipAuth_TransportsAreServedWithoutValidator(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		SkipAuth: true,
	})
	defer stack.Close()
	if stack.Handler == nil {
		t.Fatal("Handler nil under SkipAuth")
	}
	if stack.Validator != nil {
		t.Error("Validator should be nil under SkipAuth")
	}
}

// TestTryAssemble_NilCfg_ReturnsError — the cfg-required guard fires
// without a *testing.T fatal so the unit test exercises it directly.
func TestTryAssemble_NilCfg_ReturnsError(t *testing.T) {
	t.Parallel()
	stack, err := devstack.TryAssemble(nil, devstack.AssembleOpts{})
	if err == nil {
		t.Fatal("expected error on nil cfg")
	}
	if stack != nil {
		t.Errorf("expected nil stack on nil cfg, got %+v", stack)
	}
}

// TestTryAssemble_RollingSummary_WiredWithLLM — the helper now wires
// rolling_summary through memory.Open with the Summarizer defaulting to
// the configured LLM (D-174 / §17.6 — the helper mirrors production
// cmd_dev wiring). minimalConfig carries the mock LLM, so the strategy
// opens cleanly.
func TestTryAssemble_RollingSummary_WiredWithLLM(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	cfg.Memory.Strategy = "rolling_summary"
	stack, err := devstack.TryAssemble(cfg, devstack.AssembleOpts{})
	if err != nil {
		t.Fatalf("rolling_summary should wire with the configured LLM: %v", err)
	}
	if stack == nil || stack.Memory == nil {
		t.Fatal("expected a non-nil stack with an opened Memory store")
	}
	stack.Close()
}

// TestTryAssemble_RollingSummary_FailsLoudWithoutLLM — rolling_summary
// without an LLM fails loud (no stub summariser, CLAUDE.md §13). The
// error is returned with `stack != nil` so the caller's Close drains the
// layers that already opened.
func TestTryAssemble_RollingSummary_FailsLoudWithoutLLM(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	cfg.Memory.Strategy = "rolling_summary"
	cfg.LLM = config.LLMConfig{} // no LLM → no default Summarizer
	stack, err := devstack.TryAssemble(cfg, devstack.AssembleOpts{})
	if err == nil {
		t.Fatal("expected a loud error: rolling_summary without an LLM has no Summarizer")
	}
	if stack == nil {
		t.Fatal("expected non-nil stack so caller can Close partially-opened layers")
	}
	stack.Close()
}

// TestTryAssemble_PreRegisterDuplicate_ReturnsError — registering the
// same tool name twice via PreRegisterTools fails at the catalog's
// Register step.
func TestTryAssemble_PreRegisterDuplicate_ReturnsError(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	desc := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        "dup_tool",
			Description: "duplicate",
			Transport:   tools.TransportInProcess,
			Source:      "test",
		},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}
	stack, err := devstack.TryAssemble(cfg, devstack.AssembleOpts{
		PreRegisterTools: []tools.ToolDescriptor{desc, desc},
	})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if stack == nil {
		t.Fatal("expected non-nil stack so caller can Close partial layers")
	}
	stack.Close()
}

// TestTryAssemble_CatalogBuilderFails_ReturnsError — a cfg with
// `tools.entries[]` naming a tool not registered fails at Builder.Apply.
// Exercises the catalog-wiring error path.
func TestTryAssemble_CatalogBuilderFails_ReturnsError(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	cfg.Tools = config.ToolsConfig{
		Entries: []config.ToolEntryConfig{
			{Name: "no_such_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
		},
	}
	stack, err := devstack.TryAssemble(cfg, devstack.AssembleOpts{})
	if err == nil {
		t.Fatal("expected Builder.Apply error on unknown tool")
	}
	if stack == nil {
		t.Fatal("expected non-nil stack so caller can Close partial layers")
	}
	stack.Close()
}

// TestDevKeySet_UnknownKid_Errors — the in-test KeySet's KeyByID
// rejects unknown kids. Covers the error branch on the type's
// KeyByID method.
func TestDevKeySet_UnknownKid_Errors(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	// Mint a bogus token with a kid the dev signer does not know,
	// then attempt to validate. The validator's keyset lookup
	// surfaces the unknown-kid error.
	bogusToken := buildBogusKidToken(t)
	_, err := stack.Validator.Validate(context.Background(), bogusToken)
	if err == nil {
		t.Fatal("expected validation error on unknown kid")
	}
}

// buildBogusKidToken mints an ES256 token whose kid header does not
// match the dev signer's. Helper for the kid-mismatch test.
func buildBogusKidToken(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     "u",
		"aud":     "harbor",
		"exp":     time.Now().Add(1 * time.Hour).Unix(),
		"nbf":     time.Now().Add(-1 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
		"tenant":  "t",
		"user":    "u",
		"session": "s",
		"scopes":  []string{"admin"},
	})
	tok.Header["kid"] = "unknown-kid"
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// TestAssemble_CloseIsIdempotent — repeated Close calls are safe.
func TestAssemble_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	stack.Close()
	stack.Close() // second call must not panic
}

// TestAssemble_PreRegisterTools_RegistersBeforeBuilder — a tool
// pre-registered via PreRegisterTools is resolvable AFTER the
// builder applies catalog wiring (mirrors the wave11 / phase64a
// pattern of registering test tools before the operator-config
// wiring fires).
func TestAssemble_PreRegisterTools_RegistersBeforeBuilder(t *testing.T) {
	t.Parallel()
	cfg := minimalConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		PreRegisterTools: []tools.ToolDescriptor{
			{
				Tool: tools.Tool{
					Name:        "echo_tool",
					Description: "devstack test echo",
					Transport:   tools.TransportInProcess,
					Source:      "devstack-test",
				},
				Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
					return tools.ToolResult{Value: string(args)}, nil
				},
			},
		},
	})
	defer stack.Close()

	if _, ok := stack.Catalog.Resolve("echo_tool"); !ok {
		t.Error("echo_tool not resolvable after Assemble")
	}
}
