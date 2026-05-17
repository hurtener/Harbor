// phase_d103_planner_registry_test.go — D-103 / issue #126 integration
// test. Pins the production boot path through the `internal/planner`
// driver registry: a config with `planner.driver: react` boots the
// dev stack and produces a working planner; an unknown driver name is
// rejected pre-boot by the config validator.
//
// The test uses `devstack.Assemble` (D-094 source-of-truth invariant)
// so the planner construction the helper performs mirrors
// `cmd/harbor/cmd_dev.go::bootDevStack` exactly. A future regression in
// either path would fail here.
//
// Real drivers everywhere (CLAUDE.md §17.3): real audit Redactor, real
// EventBus (inmem), real TaskRegistry (inprocess driver), real
// pauseresume Coordinator, real steering Registry, real RunLoop, real
// planner registry, real ReAct planner. The mock LLM is the only stub
// — it's an explicit dev-only escape hatch (§13 amendment).

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestPlannerRegistry_BootsWithReactDriver pins the happy path: a
// minimal config with `planner.driver: react` reaches the devstack
// assembly's planner construction and produces a non-nil RunLoop
// (which holds the constructed planner indirectly via the per-task
// driver).
func TestPlannerRegistry_BootsWithReactDriver(t *testing.T) {
	t.Parallel()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Planner = config.PlannerConfig{Driver: "react"}

	llmSnap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &llmSnap,
	})
	defer stack.Close()

	// The RunLoop is constructed only when the registry resolution
	// succeeded; a nil RunLoop after a non-error Assemble would mean
	// the planner construction was silently skipped — a §13 violation.
	if stack.RunLoop == nil {
		t.Fatal("devstack: RunLoop is nil after Assemble with planner.driver=react — the registry path did not construct the planner")
	}
	if stack.RunLoopDriver == nil {
		t.Fatal("devstack: RunLoopDriver is nil — the planner consumer (per-task driver) did not wire")
	}
}

// TestPlannerRegistry_RejectsUnknownDriverAtValidate pins §13 fail-loud
// at the pre-boot stage. An operator typoing the driver name gets a
// clear error from the config validator BEFORE the binary attempts to
// boot — the same surface `harbor validate` exercises.
func TestPlannerRegistry_RejectsUnknownDriverAtValidate(t *testing.T) {
	t.Parallel()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Planner = config.PlannerConfig{Driver: "definitely-not-a-real-driver"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil for unknown planner driver, want loud rejection")
	} else if !strings.Contains(err.Error(), "react") {
		t.Fatalf("Validate err = %q, want it to list allowed planner drivers", err.Error())
	}
}

// TestPlannerRegistry_DirectResolve_ReactReachable exercises the
// registry surface directly. The react driver MUST self-register so
// `planner.Resolve` finds it without any test-side setup beyond the
// blank import that lives in `wave11_test.go`. A direct call from the
// integration test catches the regression where the driver's `init()`
// fails to fire (e.g. a future move that drops the blank import).
func TestPlannerRegistry_DirectResolve_ReactReachable(t *testing.T) {
	t.Parallel()

	got, err := planner.Resolve(context.Background(),
		planner.PlannerConfig{Driver: "react"},
		planner.FactoryDeps{LLM: integrationDummyLLM{}})
	if err != nil {
		t.Fatalf("planner.Resolve(react): %v", err)
	}
	if got == nil {
		t.Fatal("planner.Resolve returned nil Planner")
	}
}

// TestPlannerRegistry_FactoryRejectsNilLLM — the factory MUST fail
// closed when no LLM client is supplied (§13). Silent fallback to a
// stub is forbidden.
func TestPlannerRegistry_FactoryRejectsNilLLM(t *testing.T) {
	t.Parallel()

	_, err := planner.Resolve(context.Background(),
		planner.PlannerConfig{Driver: "react"},
		planner.FactoryDeps{LLM: nil})
	if err == nil {
		t.Fatal("planner.Resolve(LLM=nil) returned nil error, want fail-loud")
	}
}

// integrationDummyLLM is a no-op `llm.LLMClient` used by the registry
// dispatch test. Lives in `_test.go` only; never reachable from
// production code (§13).
type integrationDummyLLM struct{}

func (integrationDummyLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{}, nil
}

func (integrationDummyLLM) Close(_ context.Context) error { return nil }
