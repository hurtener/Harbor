// assemble_test.go — Phase 110d (D-197) coverage for the promoted
// assembly entry point: golden boot, the forced-failure table
// (partial-stack + close-what-opened + goroutine-baseline), and the
// Skip knobs.
package assemble_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	// Production driver aggregator (Phase 110c, D-196) — the single
	// sanctioned blank-import home; the same import the recipe tells a
	// headless embedder to add.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089): deliberately OUTSIDE the aggregator;
	// tests opt in explicitly.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// minimalCfg is the examples-shaped minimal config the golden boot
// uses (mirrors harbortest/devstack's fixture; mock LLM driver).
func minimalCfg(t *testing.T) *config.Config {
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
			ServiceName: "harbor-assemble-test",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
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
		t.Fatalf("minimalCfg: cfg.Validate(): %v", err)
	}
	return cfg
}

// settle gives detached driver goroutines a bounded window to drain
// before the goroutine-baseline comparison (no sleep-as-sync — the
// predicate is polled).
func settleGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine baseline not restored: started %d, now %d", baseline, runtime.NumGoroutine())
}

// TestAssemble_GoldenBoot_BuildsEveryLayer — the full fan-out on an
// examples-shaped config: every load-bearing field non-nil, Close
// clean + idempotent, goroutine baseline restored.
func TestAssemble_GoldenBoot_BuildsEveryLayer(t *testing.T) {
	baseline := runtime.NumGoroutine()
	stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stack.Redactor == nil || stack.State == nil || stack.Bus == nil ||
		stack.Metrics == nil || stack.Artifacts == nil || stack.Tasks == nil ||
		stack.Sessions == nil || stack.Agents == nil {
		t.Errorf("load-bearing core has nil fields: %+v", stack)
	}
	if stack.LLM == nil || stack.LLMSnapshot.Driver != "mock" {
		t.Errorf("LLM not opened from cfg: client=%v snapshot=%+v", stack.LLM, stack.LLMSnapshot)
	}
	if stack.Memory == nil {
		t.Errorf("memory.driver=inmem must open the memory store")
	}
	if stack.Catalog == nil || stack.Coordinator == nil || stack.Gates == nil ||
		stack.OAuthProviders == nil || stack.MCPRegistry == nil || stack.Executor == nil {
		t.Errorf("catalog band has nil fields")
	}
	if stack.Steering == nil || stack.Planner == nil || stack.RunLoop == nil {
		t.Errorf("steering band has nil fields (planner/runloop must build when an LLM is configured)")
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("second Close must be a no-op, got %v", err)
	}
	settleGoroutines(t, baseline)
}

// TestAssemble_TokenBudget_BuildsCompressionRunner — Phase 111e
// (D-202): a non-zero `planner.token_budget` makes the assembly
// construct the trajectory-compression runner (TrajectorySummariser
// over the configured LLM); zero leaves it nil (compression off).
func TestAssemble_TokenBudget_BuildsCompressionRunner(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Planner.TokenBudget = 2048
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate(token_budget): %v", err)
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() {
		if cerr := stack.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if stack.Compression == nil {
		t.Error("Stack.Compression is nil with planner.token_budget=2048 — the runner was not built")
	}

	// Zero budget: compression stays off.
	cfg2 := minimalCfg(t)
	stack2, err := assemble.Assemble(context.Background(), cfg2, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble(zero budget): %v", err)
	}
	defer func() {
		if cerr := stack2.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if stack2.Compression != nil {
		t.Error("Stack.Compression non-nil with planner.token_budget=0 — compression must default off")
	}
}

// TestAssemble_TokenBudget_WithoutLLM_FailsLoud — Phase 111e (D-202):
// a configured budget without an LLM is a misconfiguration surfaced
// loudly at assembly, never a silently-inert knob (CLAUDE.md §13).
func TestAssemble_TokenBudget_WithoutLLM_FailsLoud(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.LLM = config.LLMConfig{}
	cfg.Memory.Strategy = "none"
	cfg.Planner.TokenBudget = 2048
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }() // partial-stack drain on the failure path
	}
	if err == nil || !strings.Contains(err.Error(), "token_budget") {
		t.Fatalf("expected loud token_budget-requires-LLM error, got %v", err)
	}
}

// TestAssemble_NilConfig_FailsLoud — no silent default-config fallback.
func TestAssemble_NilConfig_FailsLoud(t *testing.T) {
	stack, err := assemble.Assemble(context.Background(), nil, assemble.Options{})
	if err == nil || !strings.Contains(err.Error(), "cfg is required") {
		t.Fatalf("expected cfg-required error, got %v", err)
	}
	if stack != nil {
		t.Errorf("expected nil stack on nil cfg")
	}
}

// TestAssemble_ForcedFailures_PartialStackClosesClean — the
// table-driven mid-assembly failure gate: each stage's error returns
// the PARTIAL stack, Close drains whatever opened, and the goroutine
// baseline is restored (acceptance: forced mid-assembly failure
// closes everything already opened).
func TestAssemble_ForcedFailures_PartialStackClosesClean(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(cfg *config.Config)
		wantErr string
	}{
		{
			name:    "state stage",
			mutate:  func(cfg *config.Config) { cfg.State.Driver = "no-such-state" },
			wantErr: "state:",
		},
		{
			name:    "events stage",
			mutate:  func(cfg *config.Config) { cfg.Events.Driver = "no-such-events" },
			wantErr: "events:",
		},
		{
			name:    "artifacts stage",
			mutate:  func(cfg *config.Config) { cfg.Artifacts.Driver = "no-such-artifacts" },
			wantErr: "artifacts:",
		},
		{
			name:    "llm stage",
			mutate:  func(cfg *config.Config) { cfg.LLM.Driver = "no-such-llm" },
			wantErr: "llm:",
		},
		{
			name:    "memory stage",
			mutate:  func(cfg *config.Config) { cfg.Memory.Driver = "no-such-memory" },
			wantErr: "memory:",
		},
		{
			name: "memory rolling_summary without llm",
			mutate: func(cfg *config.Config) {
				cfg.LLM = config.LLMConfig{}
				cfg.Memory.Strategy = "rolling_summary"
			},
			wantErr: "rolling_summary requires an LLM",
		},
		{
			name:    "skills stage",
			mutate:  func(cfg *config.Config) { cfg.Skills.Driver = "no-such-skills" },
			wantErr: "skills:",
		},
		{
			name:    "builtin stage",
			mutate:  func(cfg *config.Config) { cfg.Tools.BuiltIn = []string{"no_such_builtin"} },
			wantErr: "tools/builtin:",
		},
		{
			name: "catalog builder stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.Entries = []config.ToolEntryConfig{
					{Name: "no_such_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
				}
			},
			wantErr: "tools/catalog:",
		},
		{
			name: "oauth providers stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.OAuthTokenKEKEnv = "HARBOR_ASSEMBLE_TEST_KEK_UNSET"
				cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
					Name: "gh", Driver: "oauth2",
					ClientIDEnv: "HARBOR_ASSEMBLE_TEST_ID", ClientSecretEnv: "HARBOR_ASSEMBLE_TEST_SECRET",
				}}
			},
			wantErr: "tools/catalog:",
		},
		{
			name: "mcp attach stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.MCPServers = []config.MCPServerConfig{{
					Name:          "bad",
					TransportMode: "stdio",
					Command:       []string{"/nonexistent/harbor-110d-no-such-binary"},
				}}
			},
			wantErr: "mcp[bad]:",
		},
		{
			name:    "planner stage",
			mutate:  func(cfg *config.Config) { cfg.Planner.Driver = "no-such-planner" },
			wantErr: "planner:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			cfg := minimalCfg(t)
			tc.mutate(cfg)
			stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
			if err == nil {
				if stack != nil {
					_ = stack.Close(context.Background())
				}
				t.Fatalf("expected forced failure at %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v does not carry stage prefix %q", err, tc.wantErr)
			}
			if stack == nil {
				t.Fatalf("expected PARTIAL stack on error so the caller can drain")
			}
			if cErr := stack.Close(context.Background()); cErr != nil {
				t.Errorf("partial-stack Close: %v", cErr)
			}
			settleGoroutines(t, baseline)
		})
	}
}

// TestAssemble_SkipKnobs — SkipCatalog / SkipSteering / SkipRunLoop
// leave exactly their bands nil (the devstack test conveniences,
// expressed as options instead of forks).
func TestAssemble_SkipKnobs(t *testing.T) {
	t.Run("SkipCatalog", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipCatalog: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.Catalog != nil || stack.Coordinator != nil || stack.MCPRegistry != nil || stack.Executor != nil {
			t.Errorf("catalog band must be nil under SkipCatalog")
		}
		if stack.RunLoop != nil {
			t.Errorf("RunLoop requires the catalog band's Coordinator; must be nil under SkipCatalog")
		}
		if stack.Steering == nil || stack.Planner == nil {
			t.Errorf("steering registry + planner still build under SkipCatalog")
		}
	})
	t.Run("SkipSteering", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipSteering: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.Steering != nil || stack.Planner != nil || stack.RunLoop != nil {
			t.Errorf("steering band must be nil under SkipSteering")
		}
		if stack.Catalog == nil {
			t.Errorf("catalog band still builds under SkipSteering")
		}
	})
	t.Run("SkipRunLoop", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipRunLoop: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.RunLoop != nil {
			t.Errorf("RunLoop must be nil under SkipRunLoop")
		}
		if stack.Steering == nil || stack.Planner == nil {
			t.Errorf("steering registry + planner still build under SkipRunLoop")
		}
	})
	t.Run("no LLM means no planner and no runloop", func(t *testing.T) {
		cfg := minimalCfg(t)
		cfg.LLM = config.LLMConfig{}
		stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.LLM != nil || stack.Planner != nil || stack.RunLoop != nil {
			t.Errorf("LLM-less config must leave LLM/planner/RunLoop nil")
		}
		if stack.Steering == nil {
			t.Errorf("steering registry still builds without an LLM")
		}
	})
}

// stub110dProvider is a minimal toolauth.OAuthProvider for the
// injection-override test below (unit-test stub; the integration
// suite drives real providers).
type stub110dProvider struct{}

func (stub110dProvider) Token(context.Context, tools.ToolSourceID) (toolauth.Token, error) {
	return toolauth.Token{}, nil
}
func (stub110dProvider) InitiateFlow(context.Context, tools.ToolSourceID) (toolauth.FlowInitiation, error) {
	return toolauth.FlowInitiation{}, nil
}
func (stub110dProvider) CompleteFlow(context.Context, string, string) (toolauth.Token, error) {
	return toolauth.Token{}, nil
}
func (stub110dProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (stub110dProvider) Close(context.Context) error                      { return nil }

// TestAssemble_InjectedOAuthProvider_OverridesCfgDeclaration — an
// Options.OAuthProviders entry overrides the same-named cfg
// declaration: the overridden entry is NOT constructed (the KEK env —
// deliberately unset here — is never read) and the injected instance
// lands in Stack.OAuthProviders. This is the devstack injection seam
// the pre-110d kit relied on, now with explicit per-name semantics.
func TestAssemble_InjectedOAuthProvider_OverridesCfgDeclaration(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Tools.OAuthTokenKEKEnv = "HARBOR_ASSEMBLE_KEK_NEVER_SET"
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:            "stubbed",
		Driver:          "oauth2",
		ClientIDEnv:     "HARBOR_ASSEMBLE_ID_NEVER_SET",
		ClientSecretEnv: "HARBOR_ASSEMBLE_SECRET_NEVER_SET",
		AuthURL:         "https://example.com/authorize",
		TokenURL:        "https://example.com/token",
		RedirectURL:     "https://example.com/callback",
	}}
	injected := stub110dProvider{}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		OAuthProviders: map[string]toolauth.OAuthProvider{"stubbed": injected},
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("Assemble must not construct (or demand the KEK for) an injection-overridden provider: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	if got := stack.OAuthProviders["stubbed"]; got != toolauth.OAuthProvider(injected) {
		t.Errorf("injected provider must win: got %T", got)
	}
}
