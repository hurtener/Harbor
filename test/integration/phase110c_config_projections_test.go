// phase110c_config_projections_test.go — Phase 110c (D-196)
// integration coverage: the exported FromConfig projections + the
// `internal/drivers/prod` aggregator, exercised through the REAL
// devstack assembly (CLAUDE.md §17.3 — real drivers everywhere on the
// seam).
//
// What this pins:
//
//  1. B3 regression gate — a devstack-assembled stack resolves
//     ExtraGuidance / ReasoningReplay / MaxToolExamplesPerTool /
//     ParallelToolCalls IDENTICALLY to production: both call sites now
//     consume the ONE exported `planner.ConfigFromOperator`, and a
//     capture planner driver proves the resolved config that reaches
//     `planner.Resolve` carries every field (the devstack copy used to
//     silently drop four).
//  2. Aggregator wrapper seating — a devstack-composed LLM client
//     seats the production corrections/downgrade/retry/governance
//     wrapper chain (observable: `llm.Open`'s "wrapper hook not
//     seated" warning never fires now that devstack imports
//     `internal/drivers/prod`).
//  3. Failure mode — a config with an unknown driver name fails
//     loudly THROUGH the projections, and the factory error names the
//     registered drivers (§13 fail-loud).
//  4. Identity propagation — the projection-opened stores scope by the
//     identity quadruple: cross-identity isolation + the
//     missing-identity rejection, under a concurrent stress.

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
)

// capture110cPlanner is the minimal Planner the capture factory
// returns. It is NOT the subsystem under test (the seam under test is
// the config→projection→registry wiring); the test never drives a
// task through it.
type capture110cPlanner struct{}

func (capture110cPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishGoal, Payload: "phase110c capture planner"}, nil
}

// capture110cState records the PlannerConfig the registry factory was
// resolved with. Registries are write-once per process; the factory is
// registered once via sync.Once and the captured value is read under
// the mutex.
var capture110cState struct {
	once sync.Once
	mu   sync.Mutex
	cfg  planner.PlannerConfig
	hits int
}

func registerCapture110cDriver() {
	capture110cState.once.Do(func() {
		planner.MustRegister("capture-110c", func(cfg planner.PlannerConfig, _ planner.FactoryDeps) (planner.Planner, error) {
			capture110cState.mu.Lock()
			defer capture110cState.mu.Unlock()
			capture110cState.cfg = cfg
			capture110cState.hits++
			return capture110cPlanner{}, nil
		})
	})
}

// TestE2E_Phase110c_DevstackPlannerProjection_B3RegressionGate — the
// audit-B3 wiring gate: the planner config that reaches
// `planner.Resolve` through a devstack assembly carries EVERY
// operator-set field, and equals the exported projection's output
// field-for-field (the production call site consumes the same
// function, so devstack↔production parity holds by construction; this
// pins that devstack actually routes cfg.Planner through it instead of
// a private copy).
func TestE2E_Phase110c_DevstackPlannerProjection_B3RegressionGate(t *testing.T) {
	registerCapture110cDriver()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	parallel := false
	cfg.Planner = config.PlannerConfig{
		Driver:                 "capture-110c",
		MaxSteps:               7,
		ExtraGuidance:          "always cite sources",
		ReasoningReplay:        "text",
		MaxToolExamplesPerTool: 3,
		ParallelToolCalls:      &parallel,
		Extra:                  map[string]string{"k": "v"},
	}

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
	if stack.RunLoop == nil || stack.RunLoopDriver == nil {
		t.Fatal("devstack: RunLoop/RunLoopDriver nil — the planner construction was skipped")
	}

	capture110cState.mu.Lock()
	got := capture110cState.cfg
	capture110cState.mu.Unlock()

	// The B3 four — the fields the pre-110c devstack copy silently
	// dropped while its own comment claimed field-for-field parity.
	if got.ExtraGuidance != "always cite sources" {
		t.Errorf("ExtraGuidance = %q — the B3 drop regressed", got.ExtraGuidance)
	}
	if got.ReasoningReplay != planner.ReasoningReplayMode("text") {
		t.Errorf("ReasoningReplay = %q — the B3 drop regressed", got.ReasoningReplay)
	}
	if got.MaxToolExamplesPerTool != 3 {
		t.Errorf("MaxToolExamplesPerTool = %d — the B3 drop regressed", got.MaxToolExamplesPerTool)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls != false {
		t.Errorf("ParallelToolCalls = %v — the B3 drop regressed", got.ParallelToolCalls)
	}
	if got.Driver != "capture-110c" || got.MaxSteps != 7 || got.Extra["k"] != "v" {
		t.Errorf("base fields not carried: %+v", got)
	}

	// Production-parity assertion: the resolved config equals the ONE
	// exported projection's output (the same call production's
	// bootDevStack makes).
	want := planner.ConfigFromOperator(cfg.Planner)
	if got.Driver != want.Driver || got.MaxSteps != want.MaxSteps ||
		got.ExtraGuidance != want.ExtraGuidance || got.ReasoningReplay != want.ReasoningReplay ||
		got.MaxToolExamplesPerTool != want.MaxToolExamplesPerTool {
		t.Errorf("devstack-resolved config %+v != planner.ConfigFromOperator output %+v", got, want)
	}
	if (got.ParallelToolCalls == nil) != (want.ParallelToolCalls == nil) ||
		(got.ParallelToolCalls != nil && *got.ParallelToolCalls != *want.ParallelToolCalls) {
		t.Errorf("ParallelToolCalls mismatch: devstack %v vs projection %v", got.ParallelToolCalls, want.ParallelToolCalls)
	}
}

// phase110cLogCapture is a concurrency-safe slog.Handler that records
// every record's message + attrs as one line.
type phase110cLogCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *phase110cLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *phase110cLogCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.String())
		return true
	})
	c.mu.Lock()
	c.lines = append(c.lines, b.String())
	c.mu.Unlock()
	return nil
}
func (c *phase110cLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *phase110cLogCapture) WithGroup(string) slog.Handler      { return c }

func (c *phase110cLogCapture) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestE2E_Phase110c_AggregatorSeatsProductionLLMWrapperChain — the
// devstack now imports `internal/drivers/prod`, so the LLM client it
// composes seats the full production wrapper set. Observable contract:
// `llm.Open` warns "wrapper hook not seated — composing client WITHOUT
// this production layer" (via the default slog logger) for every
// missing hook; with the aggregator imported that warning MUST NOT
// fire. NOT t.Parallel: it swaps the process-global default logger.
func TestE2E_Phase110c_AggregatorSeatsProductionLLMWrapperChain(t *testing.T) {
	capture := &phase110cLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
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
	if stack.LLMClient == nil {
		t.Fatal("LLMClient nil after Assemble")
	}
	if capture.contains("wrapper hook not seated") {
		t.Error("llm.Open warned about an unseated wrapper hook — the internal/drivers/prod aggregator did not seat the production corrections/downgrade/retry/governance chain (SDK friction audit §7 regression)")
	}
}

// TestE2E_Phase110c_UnknownDriverFailsLoudThroughProjection — the §13
// failure mode: an unknown driver name flows through the exported
// projection into the real factory and fails loudly, with the error
// naming the registered drivers so the misconfiguration is obvious.
func TestE2E_Phase110c_UnknownDriverFailsLoudThroughProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("llm", func(t *testing.T) {
		t.Parallel()
		// Real deps on the seam: real artifact store + real bus.
		stores := openPhase83fStores(t)
		artStore := openPhase110cArtifacts(t)
		snap := llm.SnapshotFromConfig(config.LLMConfig{
			Driver:               "definitely-not-a-driver",
			Provider:             "openrouter",
			Model:                "anthropic/claude-sonnet-4",
			APIKey:               "env.FAKE_TEST_KEY", // documented dummy fixture value
			ContextWindowReserve: 0.05,
			ModelProfiles: map[string]config.LLMModelProfileConfig{
				"anthropic/claude-sonnet-4": {ContextWindowTokens: 200000},
			},
		}, config.ArtifactsConfig{HeavyOutputThresholdBytes: 32 * 1024})
		_, err := llm.Open(ctx, snap, llm.Deps{Artifacts: artStore, Bus: stores.bus})
		if err == nil {
			t.Fatal("llm.Open accepted an unknown driver projected from config")
		}
		if !errors.Is(err, llm.ErrUnknownDriver) {
			t.Errorf("err = %v, want errors.Is(_, llm.ErrUnknownDriver)", err)
		}
		if !strings.Contains(err.Error(), "registered:") {
			t.Errorf("err = %q, want it to name the registered drivers", err.Error())
		}
	})

	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		stores := openPhase83fStores(t)
		_, err := memory.Open(ctx, memory.SnapshotFromConfig(config.MemoryConfig{
			Driver:   "definitely-not-a-driver",
			Strategy: "none",
		}), memory.Deps{State: stores.state, Bus: stores.bus})
		if err == nil {
			t.Fatal("memory.Open accepted an unknown driver projected from config")
		}
		if !strings.Contains(err.Error(), "definitely-not-a-driver") {
			t.Errorf("err = %q, want it to name the offending driver", err.Error())
		}
	})
}

// TestE2E_Phase110c_IdentityIsolation_ThroughProjectionOpenedStores —
// identity propagation through the assembled stack's stores: the
// memory + skills stores devstack opens via the exported projections
// scope every read/write by the identity quadruple. Covers
// cross-identity isolation under a concurrent stress (N=12) plus the
// identity-mandatory rejection.
func TestE2E_Phase110c_IdentityIsolation_ThroughProjectionOpenedStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Drive BOTH projection-opened stores from cfg: memory
	// (memory.SnapshotFromConfig) + skills (skills.SnapshotFromConfig).
	cfg.Memory = config.MemoryConfig{Driver: "inmem", Strategy: "truncation", BudgetTokens: 4096}
	cfg.Skills = config.SkillsConfig{Driver: "localdb", DSN: filepath.Join(t.TempDir(), "skills.db")}

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
	if stack.Memory == nil {
		t.Fatal("stack.Memory nil — memory.SnapshotFromConfig open path did not run")
	}
	if stack.Skills == nil {
		t.Fatal("stack.Skills nil — skills.SnapshotFromConfig open path did not run (production mirror, Phase 110c)")
	}

	quad := func(i int) identity.Quadruple {
		return identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%2), // two tenants
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			},
			RunID: fmt.Sprintf("run-%d", i),
		}
	}

	// Concurrent writers: N=12 identities each write their own turn.
	const n = 12
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turn := memory.ConversationTurn{
				UserMessage:       fmt.Sprintf("question-%d", i),
				AssistantResponse: fmt.Sprintf("answer-%d", i),
			}
			if err := stack.Memory.AddTurn(ctx, quad(i), turn); err != nil {
				errCh <- fmt.Errorf("AddTurn(%d): %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Each identity reads back ONLY its own content.
	for i := range n {
		patch, gErr := stack.Memory.GetLLMContext(ctx, quad(i))
		if gErr != nil {
			t.Fatalf("GetLLMContext(%d): %v", i, gErr)
		}
		if len(patch.RecentTurns) != 1 {
			t.Fatalf("identity %d sees %d turns, want exactly its own 1 (cross-identity bleed)", i, len(patch.RecentTurns))
		}
		if patch.RecentTurns[0].UserMessage != fmt.Sprintf("question-%d", i) {
			t.Errorf("identity %d sees %q — cross-identity bleed", i, patch.RecentTurns[0].UserMessage)
		}
	}

	// Identity-mandatory: a quadruple with an empty session is
	// rejected loudly (CLAUDE.md §6 rule 9 — fail closed).
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}
	if err := stack.Memory.AddTurn(ctx, bad, memory.ConversationTurn{UserMessage: "x"}); err == nil {
		t.Error("AddTurn accepted an identity with an empty session — identity must be mandatory")
	} else if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Errorf("err = %v, want errors.Is(_, memory.ErrIdentityRequired)", err)
	}
}

// openPhase110cArtifacts opens a real inmem artifact store for the
// llm failure-mode subtest.
func openPhase110cArtifacts(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}
