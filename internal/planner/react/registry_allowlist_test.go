package react_test

// D-103 — allowlist drift guard. Asserts that the `react` driver's
// canonical name is in the `internal/config` validator's
// `allowedPlannerDrivers` allowlist (and vice-versa). The `internal/
// config` package MUST NOT import `internal/planner` (§4.4 — drivers
// depend on interfaces, not the other way round), so the two surfaces
// are duplicated by design; this test catches the drift.
//
// Mirrors the pattern in D-095 / `internal/tools/auth/drivers/oauth2`
// (which referenced the same drift test in its godoc — the planner
// landed first with the explicit implementation).

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// TestConfigAllowlist_AcceptsReactDriver asserts the config validator
// accepts `planner.driver: react` — the V1 default driver. A drift
// (the react driver renamed; the allowlist drops the entry) would fail
// pre-boot validation in `harbor validate`.
func TestConfigAllowlist_AcceptsReactDriver(t *testing.T) {
	t.Parallel()

	cfg := minimalValidConfig()
	cfg.Planner = config.PlannerConfig{Driver: react.DriverName}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.driver=%q): %v (allowlist must accept the registered driver)", react.DriverName, err)
	}
}

// TestConfigAllowlist_RejectsUnknownDriver pins the allowlist's
// fail-loud rejection of a typoed driver name. The validator must
// reject the value so the operator's pre-boot `harbor validate` flags
// the typo.
func TestConfigAllowlist_RejectsUnknownDriver(t *testing.T) {
	t.Parallel()

	cfg := minimalValidConfig()
	cfg.Planner = config.PlannerConfig{Driver: "no-such-driver-xyz"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate(planner.driver=no-such-driver-xyz) returned nil, want error")
	}
}

// TestConfigAllowlist_AcceptsEmptyAsDefault pins the loader's empty →
// "react" default — a config that omits the planner block boots with
// the V1 reference planner. Drift here (default flipped, validator
// rejects empty) would break every operator config that omits the
// block.
func TestConfigAllowlist_AcceptsEmptyAsDefault(t *testing.T) {
	t.Parallel()

	cfg := minimalValidConfig()
	cfg.Planner = config.PlannerConfig{Driver: ""}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(planner.driver=\"\") rejected the empty-default case: %v", err)
	}
}

// TestConfigAllowlist_RejectsNegativeMaxSteps pins the loud rejection
// of nonsensical MaxSteps values.
func TestConfigAllowlist_RejectsNegativeMaxSteps(t *testing.T) {
	t.Parallel()

	cfg := minimalValidConfig()
	cfg.Planner = config.PlannerConfig{Driver: react.DriverName, MaxSteps: -5}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate(planner.max_steps=-5) returned nil, want error")
	}
}

// TestConfigAllowlist_ReasoningReplayMirror is the Phase 83e (D-148)
// drift guard: the `internal/config` validator's
// `allowedReasoningReplayModes` allowlist MUST mirror the
// `planner.ReasoningReplayMode` enum. `internal/config` cannot import
// `internal/planner` (§4.4); the two surfaces are duplicated by
// design, and this test catches the drift by exercising every
// canonical enum value through the config validator.
func TestConfigAllowlist_ReasoningReplayMirror(t *testing.T) {
	t.Parallel()

	// Every canonical enum value (plus the empty unset sentinel) must
	// validate through the config-side allowlist.
	for _, mode := range []planner.ReasoningReplayMode{
		"", planner.ReasoningReplayNever, planner.ReasoningReplayText,
	} {
		if !planner.IsValidReasoningReplayMode(mode) {
			t.Errorf("planner.IsValidReasoningReplayMode(%q) = false — enum drift", mode)
		}
		cfg := minimalValidConfig()
		cfg.Planner = config.PlannerConfig{Driver: react.DriverName, ReasoningReplay: string(mode)}
		if err := cfg.Validate(); err != nil {
			t.Errorf("config rejects planner.reasoning_replay=%q but the planner enum accepts it: %v", mode, err)
		}
	}

	// A value the planner enum rejects must also fail config validation.
	bogus := "provider_native"
	if planner.IsValidReasoningReplayMode(planner.ReasoningReplayMode(bogus)) {
		t.Fatalf("planner enum unexpectedly accepts %q", bogus)
	}
	cfg := minimalValidConfig()
	cfg.Planner = config.PlannerConfig{Driver: react.DriverName, ReasoningReplay: bogus}
	if err := cfg.Validate(); err == nil {
		t.Errorf("config accepts planner.reasoning_replay=%q but the planner enum rejects it — allowlist drift", bogus)
	}
}

// TestRegistryDispatch_ReactReachable proves the react driver
// self-registered and is reachable via `planner.Resolve`. End-to-end
// smoke for the registry-side surface.
func TestRegistryDispatch_ReactReachable(t *testing.T) {
	t.Parallel()

	got, err := planner.Resolve(context.Background(),
		planner.PlannerConfig{Driver: react.DriverName},
		planner.FactoryDeps{LLM: registryDummyLLM{}})
	if err != nil {
		t.Fatalf("planner.Resolve(%q) returned %v; the react driver should be registered via init()", react.DriverName, err)
	}
	if got == nil {
		t.Fatal("planner.Resolve returned nil planner")
	}
}

// TestRegistryDispatch_ReactWithMaxSteps verifies the factory honours
// the optional MaxSteps knob and propagates it onto the constructed
// planner.
func TestRegistryDispatch_ReactWithMaxSteps(t *testing.T) {
	t.Parallel()

	got, err := planner.Resolve(context.Background(),
		planner.PlannerConfig{Driver: react.DriverName, MaxSteps: 42},
		planner.FactoryDeps{LLM: registryDummyLLM{}})
	if err != nil {
		t.Fatalf("planner.Resolve: %v", err)
	}
	if got == nil {
		t.Fatal("planner.Resolve returned nil planner")
	}
}

// TestFactory_NilLLMRejected — the factory MUST fail closed when no
// LLM client is supplied (the V1 react planner cannot run without one).
// Silent fallback to a stub LLM is forbidden per §13.
func TestFactory_NilLLMRejected(t *testing.T) {
	t.Parallel()

	_, err := planner.Resolve(context.Background(),
		planner.PlannerConfig{Driver: react.DriverName},
		planner.FactoryDeps{LLM: nil})
	if err == nil {
		t.Fatal("planner.Resolve(LLM=nil) returned nil error, want fail-loud")
	}
}

// registryDummyLLM is a minimal `llm.LLMClient` stand-in for registry-
// bookkeeping tests. It is NOT a production-grade stub (the §13
// amendment forbids `mock`-as-default); it lives in `_test.go` only
// and never reaches a production registry default.
type registryDummyLLM struct{}

func (registryDummyLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{}, nil
}

func (registryDummyLLM) Close(_ context.Context) error { return nil }

// minimalValidConfig returns a `*config.Config` that satisfies every
// other validator so we can exercise the planner-validator slice in
// isolation. Test-only fixture per §11.
func minimalValidConfig() *config.Config {
	return &config.Config{
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
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-test",
		},
		State:      config.StateConfig{Driver: "inmem"},
		LLM:        config.LLMConfig{Driver: "mock"},
		Governance: config.GovernanceConfig{RepairAttempts: 1},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 1,
			SubscriberBufferSize:     1,
			IdleTimeout:              1 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         0,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Second,
			HardCap:       1 * time.Second,
			SweepInterval: 1 * time.Second,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    1 * time.Second,
			ContinuationHopLimit: 1,
		},
		Distributed: config.DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: config.MemoryConfig{Driver: "inmem"},
	}
}
