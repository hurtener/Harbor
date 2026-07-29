// validate_core_test.go — Phase 110c (D-196): the exported Defaults()
// baseline, the headless ValidateCore profile, and the re-homed
// planner-knob default resolvers.

package config_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// defaultsForCore returns config.Defaults() with ONLY the documented
// required-for-core fields set (the LLM provider trio) — the exact
// recipe the Defaults() godoc names for a headless consumer.
func defaultsForCore() *config.Config {
	cfg := config.Defaults()
	cfg.LLM.Provider = "openrouter"
	cfg.LLM.Model = "anthropic/claude-sonnet-4"
	cfg.LLM.APIKey = "env.FAKE_TEST_KEY" // documented dummy fixture value
	return cfg
}

// TestDefaults_BaselineGolden pins the documented non-security
// defaults to the values the loader-private defaults() produced before
// the Phase 110c rename — the `Load` behaviour-unchanged golden.
func TestDefaults_BaselineGolden(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()

	if cfg.Server.BindAddr != "127.0.0.1:8080" || cfg.Server.ShutdownGracePeriod != 30*time.Second {
		t.Errorf("Server defaults = %+v", cfg.Server)
	}
	if cfg.Telemetry.LogFormat != "json" || cfg.Telemetry.LogLevel != "info" || cfg.Telemetry.ServiceName != "harbor" {
		t.Errorf("Telemetry defaults = %+v", cfg.Telemetry)
	}
	if cfg.State.Driver != "inmem" {
		t.Errorf("State.Driver = %q, want inmem", cfg.State.Driver)
	}
	if cfg.LLM.Driver != "bifrost" || cfg.LLM.Timeout != 60*time.Second || cfg.LLM.ContextWindowReserve != 0.05 {
		t.Errorf("LLM defaults = %+v", cfg.LLM)
	}
	if cfg.LLM.Corrections.Enabled == nil || !*cfg.LLM.Corrections.Enabled {
		t.Error("LLM.Corrections.Enabled default must be *true")
	}
	if cfg.Governance.RepairAttempts != 3 {
		t.Errorf("Governance.RepairAttempts = %d, want 3", cfg.Governance.RepairAttempts)
	}
	if cfg.Events.Driver != "inmem" || cfg.Events.SubscriberBufferSize != 256 || cfg.Events.ReplayBufferSize != 10000 {
		t.Errorf("Events defaults = %+v", cfg.Events)
	}
	if cfg.Sessions.IdleTTL != 24*time.Hour || cfg.Sessions.HardCap != 720*time.Hour || cfg.Sessions.SweepInterval != 15*time.Minute {
		t.Errorf("Sessions defaults = %+v", cfg.Sessions)
	}
	// Retargeted onto the named constant (phase 213): the seeded default
	// IS the LLM-context arm, so a literal here would have to be re-typed
	// every time that arm moves and would silently pass if someone
	// re-pointed Defaults() at the pinned Console bound instead.
	if cfg.Artifacts.Driver != "inmem" || cfg.Artifacts.HeavyOutputThresholdBytes != config.DefaultHeavyOutputThresholdBytes {
		t.Errorf("Artifacts defaults = %+v", cfg.Artifacts)
	}
	if config.DefaultHeavyOutputThresholdBytes != 128*1024 {
		t.Errorf("DefaultHeavyOutputThresholdBytes = %d, want 131072 (the raised LLM-context arm)",
			config.DefaultHeavyOutputThresholdBytes)
	}
	if cfg.Tasks.Driver != "inprocess" || cfg.Tasks.RetainTurnTimeout != 5*time.Minute || cfg.Tasks.ContinuationHopLimit != 8 {
		t.Errorf("Tasks defaults = %+v", cfg.Tasks)
	}
	if cfg.Memory.Driver != "inmem" || cfg.Memory.Strategy != "none" || cfg.Memory.RecoveryBacklogMax != 16 {
		t.Errorf("Memory defaults = %+v", cfg.Memory)
	}
	if cfg.Planner.Driver != "react" {
		t.Errorf("Planner.Driver = %q, want react", cfg.Planner.Driver)
	}
	// Security-relevant fields stay intentionally absent (fail-loud
	// posture unchanged).
	if len(cfg.Identity.JWTAlgorithms) != 0 || cfg.Identity.Issuer != "" {
		t.Errorf("Identity must stay zero in Defaults(): %+v", cfg.Identity)
	}
}

// TestDefaults_ReturnsFreshInstance — each call returns an independent
// *Config; mutating one never bleeds into the next (the loader relies
// on this when layering yaml on top).
func TestDefaults_ReturnsFreshInstance(t *testing.T) {
	t.Parallel()
	a := config.Defaults()
	a.Server.BindAddr = "10.0.0.1:1"
	a.CLI.DevHotReload.WatchRoots[0] = "mutated"
	b := config.Defaults()
	if b.Server.BindAddr != "127.0.0.1:8080" {
		t.Error("Defaults() instances share scalar state")
	}
	if b.CLI.DevHotReload.WatchRoots[0] != ".harbor/agents" {
		t.Error("Defaults() instances share slice backing storage")
	}
}

// TestValidateCore_AcceptsJWTLessHandBuilt — the headline 110c pair: a
// hand-built Defaults()-based config with only the required-for-core
// LLM trio set passes ValidateCore (no JWT ceremony demanded)…
func TestValidateCore_AcceptsJWTLessHandBuilt(t *testing.T) {
	t.Parallel()
	cfg := defaultsForCore()
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore rejected a JWT-less hand-built config: %v", err)
	}
}

// TestValidate_StillRejectsJWTLessHandBuilt — …and the SAME config
// still FAILS the full-binary Validate, naming the identity section.
// Full Validate semantics are unchanged by the profile split.
func TestValidate_StillRejectsJWTLessHandBuilt(t *testing.T) {
	t.Parallel()
	cfg := defaultsForCore()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a JWT-less config — the full-binary identity ceremony was lost")
	}
	if !strings.Contains(err.Error(), "identity.jwt_algorithms") {
		t.Errorf("Validate err = %q, want it to name identity.jwt_algorithms", err.Error())
	}
}

// TestValidateCore_IsSubtractiveOnly — ValidateCore still enforces
// every non-identity section: a core-shaped config with a broken
// non-identity field fails ValidateCore with the same error Validate
// reports.
func TestValidateCore_IsSubtractiveOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantPath string
	}{
		{"server stays core", func(c *config.Config) { c.Server.BindAddr = "" }, "server.bind_addr"},
		{"llm stays core", func(c *config.Config) { c.LLM.Provider = "" }, "llm.provider"},
		{"state stays core", func(c *config.Config) { c.State.Driver = "sqlite"; c.State.DSN = "" }, "state.dsn"},
		{"planner stays core", func(c *config.Config) { c.Planner.MaxSteps = -1 }, "planner.max_steps"},
		{"memory stays core", func(c *config.Config) { c.Memory.Driver = "not-a-driver" }, "memory.driver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultsForCore()
			tc.mutate(cfg)
			err := cfg.ValidateCore()
			if err == nil {
				t.Fatalf("ValidateCore accepted a config with a broken %s", tc.wantPath)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("ValidateCore err = %q, want it to name %s", err.Error(), tc.wantPath)
			}
		})
	}
}

// TestValidateCore_PassesWhenFullValidatePasses — a config that passes
// the full Validate always passes ValidateCore (core ⊂ full).
func TestValidateCore_PassesWhenFullValidatePasses(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load(valid_minimal): %v", err)
	}
	if err := cfg.ValidateCore(); err != nil {
		t.Errorf("ValidateCore rejected a config the full Validate accepted: %v", err)
	}
}

// TestSkillsContextMaxResolved — the re-homed zero→default resolver
// (Phase 110c; formerly run-loop literals in cmd + devstack).
func TestSkillsContextMaxResolved(t *testing.T) {
	t.Parallel()
	if got := (config.PlannerConfig{}).SkillsContextMaxResolved(); got != config.DefaultSkillsContextMax {
		t.Errorf("zero SkillsContextMax resolved to %d, want %d", got, config.DefaultSkillsContextMax)
	}
	if got := (config.PlannerConfig{SkillsContextMax: 9}).SkillsContextMaxResolved(); got != 9 {
		t.Errorf("explicit SkillsContextMax resolved to %d, want 9", got)
	}
	if config.DefaultSkillsContextMax != 5 {
		t.Errorf("DefaultSkillsContextMax = %d, want 5 (the pre-110c run-loop literal)", config.DefaultSkillsContextMax)
	}
}

// TestSpawnDepthCap_SingleSourcedDefault — the deduped spawn-depth
// default (Phase 110c): the resolver and the exported constant agree.
func TestSpawnDepthCap_SingleSourcedDefault(t *testing.T) {
	t.Parallel()
	if got := (config.PlannerConfig{}).SpawnDepthCap(); got != config.DefaultSpawnDepthCap {
		t.Errorf("zero AbsoluteMaxSpawnDepth resolved to %d, want %d", got, config.DefaultSpawnDepthCap)
	}
	if got := (config.PlannerConfig{AbsoluteMaxSpawnDepth: 7}).SpawnDepthCap(); got != 7 {
		t.Errorf("explicit AbsoluteMaxSpawnDepth resolved to %d, want 7", got)
	}
	if config.DefaultSpawnDepthCap != 4 {
		t.Errorf("DefaultSpawnDepthCap = %d, want 4 (D-170's documented default)", config.DefaultSpawnDepthCap)
	}
}

// TestBatchSpawnCap_SingleSourcedDefault — the batch-spawn breadth cap
// resolver and the exported constant agree; the default is the
// documented conservative value.
func TestBatchSpawnCap_SingleSourcedDefault(t *testing.T) {
	t.Parallel()
	if got := (config.PlannerConfig{}).BatchSpawnCap(); got != config.DefaultMaxBatchSpawns {
		t.Errorf("zero MaxBatchSpawns resolved to %d, want %d", got, config.DefaultMaxBatchSpawns)
	}
	if got := (config.PlannerConfig{MaxBatchSpawns: 12}).BatchSpawnCap(); got != 12 {
		t.Errorf("explicit MaxBatchSpawns resolved to %d, want 12", got)
	}
	if config.DefaultMaxBatchSpawns != 5 {
		t.Errorf("DefaultMaxBatchSpawns = %d, want 5 (the documented conservative default)", config.DefaultMaxBatchSpawns)
	}
}
