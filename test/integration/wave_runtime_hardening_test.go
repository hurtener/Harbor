// Cross-subsystem acceptance through the production cumulative runtime,
// SQLite persistence, governed model client and runtime metrics.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// Canonical runtime-gauge wire names (telemetry consts are unexported;
// pinned here so a rename is caught by this composition test too — the
// metrics.snapshot reader depends on these names).
const (
	whardGaugeGovCache      = "harbor_runtime_governance_cache_entries"
	whardGaugeEventsDropped = "harbor_runtime_events_dropped"
)

// One assembled stack owns execution, inspection, governance and metrics.
type whardSeam struct {
	stack    *assemble.Stack
	bus      events.EventBus
	state    state.StateStore
	mem      memory.MemoryStore
	client   llm.LLMClient
	metrics  *telemetry.MetricsRegistry
	driver   *budgetDriver
	closeAll func()
}

const (
	whardBudgetTokens = 8 * 1024
	whardWindowTokens = 1 << 20
)

func newWhardSeam(t *testing.T) *whardSeam {
	t.Helper()
	driver := &budgetDriver{decisions: map[string]budgetDecision{}}
	name := "hardening-cumulative-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := config.Defaults()
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")}
	cfg.Memory.BudgetTokens = whardBudgetTokens
	cfg.LLM.Driver = name
	cfg.LLM.Model = "m"
	cfg.Governance.DefaultTier = "std"
	cfg.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{"std": {
		BudgetCeilingUSD: 1000000, RateLimit: config.GovernanceRateLimitConfig{Capacity: 1000000},
	}}
	snapshot := llm.ConfigSnapshot{Driver: name, Model: "m", ContextWindowReserve: .05, HeavyOutputThreshold: budgetHeavyThreshold,
		ModelProfiles:      map[string]llm.ModelProfile{"m": {ContextWindowTokens: whardWindowTokens, TokenEstimator: "chars_div_4"}},
		DisableCorrections: true, DisableRetry: true, DisableDowngrade: true,
	}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot, MetricsOptions: []telemetry.MetricsOption{telemetry.WithMetricReader(sdkmetric.NewManualReader())}})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatal(err)
	}
	if !stack.GovernanceEnforcementActive {
		_ = stack.Close(context.Background())
		t.Fatal("governance not wired")
	}
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			if err := stack.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	t.Cleanup(closeAll)
	return &whardSeam{stack: stack, bus: stack.Bus, state: stack.State, mem: stack.Memory, client: stack.LLM, metrics: stack.Metrics, driver: driver, closeAll: closeAll}
}
func (s *whardSeam) run(ctx context.Context, q identity.Quadruple, query string) (budgetDecision, error) {
	run := q.RunID + "-" + string(state.NewEventID())
	_, err := s.stack.RunOnce(ctx, query, q.Identity, assemble.WithRunID(run))
	return s.driver.decision(run), err
}

// whardSessionID builds a distinct (tenant,user,session,run) quadruple
// for session index i, plus a ctx carrying that identity — identity must
// propagate through ctx AND the explicit storage argument.
func whardSessionID(t *testing.T, i int) (context.Context, identity.Quadruple) {
	t.Helper()
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%03d", i),
			UserID:    fmt.Sprintf("user-%03d", i),
			SessionID: fmt.Sprintf("sess-%03d", i),
		},
		RunID: fmt.Sprintf("run-%03d", i),
	}
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun(%d): %v", i, err)
	}
	return ctx, q
}

// whardMarker is the per-session uniqueness marker embedded in every
// turn so cross-session bleed is detectable. Fixed-width so no marker is
// a substring-prefix of another (MARKER000 is not a prefix of MARKER010).
func whardMarker(i int) string { return fmt.Sprintf("MARKER%03d", i) }

// All 12 sessions execute 40 actual runs against one SQLite-backed runtime.
// The initial marker must survive compaction and never cross an identity scope.
func TestE2E_WaveRuntimeHardening_ConcurrentSessionIsolation(t *testing.T) {
	const N = 12
	const turnsPerSess = 40
	seam := newWhardSeam(t)
	var wg sync.WaitGroup
	var fails atomic.Int64
	bodies := make([]string, N)
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			ctx, q := whardSessionID(t, i)
			for turn := range turnsPerSess {
				query := fmt.Sprintf("turn-%02d ", turn) + strings.Repeat("u", 2000)
				if turn == 0 {
					query = whardMarker(i) + " " + query
				}
				decision, err := seam.run(ctx, q, query)
				if err != nil {
					t.Errorf("session %d turn %d: %v", i, turn, err)
					fails.Add(1)
					return
				}
				if decision.tokens > whardBudgetTokens || !strings.Contains(decision.body, whardMarker(i)) {
					t.Errorf("session %d turn %d: request exceeded budget (%d) or lost initial marker", i, turn, decision.tokens)
					fails.Add(1)
					return
				}
				bodies[i] = decision.body
			}
			view, err := seam.mem.Inspect(ctx, q)
			if err != nil || view.Summary == "" || !strings.Contains(view.Summary, whardMarker(i)) {
				t.Errorf("session %d has no committed marker checkpoint: %+v, %v", i, view, err)
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d session failures", fails.Load())
	}
	for i := range N {
		for j := range N {
			if i != j && strings.Contains(bodies[i], whardMarker(j)) {
				t.Errorf("session %d received session %d's marker", i, j)
			}
		}
	}
}

// TestE2E_WaveRuntimeHardening_BudgetExemptionVsLeak proves the memory
// context budget + the D-241 refinement end-to-end against the SAME
// safety-wrapped client: a rolled-up conversation summary that exceeds
// the heavy byte threshold passes the LLM-edge safety pass (conversation
// text is byte-exempt; governed by the token-window guard instead),
// while an offloadable RoleTool observation that exceeds the same
// threshold trips ErrContextLeak.
func TestE2E_WaveRuntimeHardening_BudgetExemptionVsLeak(t *testing.T) {
	const budget = 32 * 1024
	// Reuse the focused budget seam (memory_llm_budget_test.go) for the
	// summary-assembly half: same rolling_summary + safety-edge wiring.
	seam := newBudgetSeam(t, budget, 128*1024)
	ctx := budgetIdentity(t)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1",
	}}

	for i := range 60 {
		budgetTurn(t, seam, id.Identity, fmt.Sprintf("exemption-%d", i), fmt.Sprintf("turn-%02d-", i)+strings.Repeat("u", 2000))
	}
	// The maintenance summary itself is bounded. The byte exemption also
	// applies to larger legitimate conversation text received at the LLM edge.
	text := "Historical conversation: " + strings.Repeat("c", budgetHeavyThreshold+1024)
	if _, err := seam.client.Complete(ctx, llm.CompleteRequest{Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleSystem, Content: llm.Content{Text: &text}}}}); err != nil {
		t.Fatalf("conversation byte exemption: %v", err)
	}

	// Offloadable side: a RoleTool observation above the heavy threshold
	// MUST trip ErrContextLeak (it should have been an ArtifactStub). The
	// RoleTool message is correctly paired to a preceding assistant
	// tool_call so it clears the OpenAI-spec pairing check (step 0) and
	// the heavy-content leak check (step 2) is what fires.
	callID := "call_leak"
	heavyToolText := strings.Repeat("x", budgetHeavyThreshold+1024)
	leakReq := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCallStructured{
				{ID: callID, Name: "fetch", Args: json.RawMessage(`{}`)},
			}},
			{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &heavyToolText}},
		},
	}
	_, err := seam.client.Complete(ctx, leakReq)
	if !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("heavy RoleTool observation: err=%v, want ErrContextLeak", err)
	}
}

// TestE2E_WaveRuntimeHardening_RuntimeGaugesObservable proves the
// observability foundation composes with the live runtime: after driving
// the concurrent memory load (which fans bus events into the registry's
// counter) and a per-identity governance-wrapped LLM load, the runtime
// gauges are observable via Snapshot — the governance-cache gauge tracks
// the configured identity cohort and the events-dropped gauge is at baseline (no
// backpressure drops under a bounded, drained load; the retention
// guarantee read through the gauge surface).
//
// Engine-gauge limitation: harbor_runtime_active_runs and
// harbor_runtime_engine_capacity_entries are NOT exercised here — they
// require an engine.Engine node-graph host, which a planner/RunLoop-shaped
// stack (the shape assemble.Assemble builds for the binary) does not run.
// assemble leaves those two callbacks nil for the same reason; the gauge
// seam skips a nil callback. This test asserts only the two gauges that
// are live without an engine host.
func TestE2E_WaveRuntimeHardening_RuntimeGaugesObservable(t *testing.T) {
	const distinctIdentities = 6
	seam := newWhardSeam(t)

	// Drive a modest concurrent memory load so the bus carries real
	// memory.* events to the metrics bridge.
	var wg sync.WaitGroup
	wg.Add(distinctIdentities)
	for i := range distinctIdentities {
		go func() {
			defer wg.Done()
			ctx, q := whardSessionID(t, i)
			for turn := range 20 {
				ct := memory.ConversationTurn{
					UserMessage:       fmt.Sprintf("g-turn-%02d ", turn) + strings.Repeat("u", 1500),
					AssistantResponse: strings.Repeat("a", 1500),
				}
				if _, err := seam.run(ctx, q, ct.UserMessage); err != nil {
					t.Errorf("[gauge sess %d] Put: %v", i, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Drive one governance-wrapped Complete per distinct identity so the
	// governance caches populate (PreCall -> rate key, PostCall -> cost
	// key => 2 entries per identity).
	for i := range distinctIdentities {
		ctx, _ := whardSessionID(t, i)
		txt := "ping " + fmt.Sprintf("%03d", i)
		if _, err := seam.client.Complete(ctx, llm.CompleteRequest{
			Model:    "m",
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
		}); err != nil {
			t.Fatalf("[gauge sess %d] governance-wrapped Complete: %v", i, err)
		}
	}

	wantGovEntries := int64(2 * distinctIdentities)

	snap, err := seam.metrics.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("metrics.Snapshot: %v", err)
	}
	gauges := map[string]float64{}
	for _, g := range snap.Gauges {
		gauges[g.Name] = g.Value
		if len(g.Labels) != 0 {
			t.Errorf("runtime gauge %q carries labels %v — runtime gauges are unlabelled by construction (cardinality firewall)",
				g.Name, g.Labels)
		}
	}

	govVal, ok := gauges[whardGaugeGovCache]
	if !ok {
		t.Fatalf("snapshot missing %q gauge; gauges=%v", whardGaugeGovCache, gauges)
	}
	if int64(govVal) != wantGovEntries {
		t.Errorf("%s gauge = %v, want %d (must track the configured identity cohort)", whardGaugeGovCache, govVal, wantGovEntries)
	}

	dropped, ok := gauges[whardGaugeEventsDropped]
	if !ok {
		t.Fatalf("snapshot missing %q gauge; gauges=%v", whardGaugeEventsDropped, gauges)
	}
	if dropped != 0 {
		t.Errorf("%s gauge = %v, want 0 — the bounded, drained load should not drop events (retention guarantee)", whardGaugeEventsDropped, dropped)
	}

	// Sanity: the bus DroppedCounter and the gauge agree.
	if dc, ok := seam.bus.(events.DroppedCounter); ok {
		if dc.DroppedTotal() != int64(dropped) {
			t.Errorf("events-dropped gauge (%v) disagrees with bus.DroppedTotal (%d)", dropped, dc.DroppedTotal())
		}
	}
}

// TestE2E_WaveRuntimeHardening_RetentionNoGoroutineLeak proves the
// runtime retention contract: the long-lived components the stack starts
// (the EventBus, the bus->metrics bridge goroutine) are cancellable and
// joined on Close, so goroutines return to the pre-construction baseline
// after teardown. Not parallel: runtime.NumGoroutine is process-global.
func TestE2E_WaveRuntimeHardening_RetentionNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	seam := newWhardSeam(t)

	// Drive a load so any per-operation goroutines spin up and must be
	// joined.
	var wg sync.WaitGroup
	const sessions = 8
	wg.Add(sessions)
	for i := range sessions {
		go func() {
			defer wg.Done()
			ctx, q := whardSessionID(t, i)
			for turn := range 15 {
				ct := memory.ConversationTurn{
					UserMessage:       fmt.Sprintf("r-turn-%02d ", turn) + strings.Repeat("u", 1200),
					AssistantResponse: strings.Repeat("a", 1200),
				}
				if _, err := seam.run(ctx, q, ct.UserMessage); err != nil {
					t.Errorf("[retention sess %d] Put: %v", i, err)
					return
				}
			}
			txt := "ping"
			if _, err := seam.client.Complete(ctx, llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
			}); err != nil {
				t.Errorf("[retention sess %d] Complete: %v", i, err)
			}
		}()
	}
	wg.Wait()

	// Tear everything down (joins the bus + bridge goroutines).
	seam.closeAll()

	// Eventually-style bounded wait — no fixed sleep as a sync primitive.
	// Small slack absorbs the runtime's own scheduler/GC goroutines plus
	// any other package-level test goroutines that may coexist when the
	// full suite runs.
	const slack = 4
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > baseline+slack {
		time.Sleep(20 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > slack {
		t.Errorf("goroutine count rose by %d after stack teardown (baseline=%d, after=%d) — retention/join-on-shutdown contract violated",
			delta, baseline, runtime.NumGoroutine())
	}
}

// TestE2E_WaveRuntimeHardening_FailLoudModes is the §17.3 failure-mode
// assertion at the wave-composition boundary: identity is mandatory and a
// closed store fails loud with a typed error — never a silent degradation
// (§13). It covers three fail-loud paths across the composed seam.
func TestE2E_WaveRuntimeHardening_FailLoudModes(t *testing.T) {
	seam := newWhardSeam(t)

	// 1) Memory store fails closed on an incomplete identity (no session).
	badID := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}
	_, err := seam.mem.Put(context.Background(), badID,
		memory.ConversationTurn{UserMessage: "x", AssistantResponse: "y"})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Errorf("Put with incomplete identity: err=%v, want ErrIdentityRequired", err)
	}

	// 2) LLM edge fails closed when ctx carries no identity. In the
	// composed stack the governance enforcement wrapper fronts the safety
	// pass, so the no-identity rejection surfaces as governance's typed
	// ErrIdentityRequired (the unwrapped safety pass would itself return
	// llm.ErrIdentityMissing) — either way the composed edge fails loud,
	// never silently completes against a missing identity.
	txt := "hello"
	_, err = seam.client.Complete(context.Background(), llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
	})
	if !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("Complete with no identity: err=%v, want governance.ErrIdentityRequired", err)
	}

	// 3) Closed store mid-operation fails loud (no silent no-op). Close
	// the seam, then a memory operation must report ErrStoreClosed.
	ctx, q := whardSessionID(t, 0)
	if _, err := seam.mem.Put(ctx, q,
		memory.ConversationTurn{UserMessage: "pre-close", AssistantResponse: "ok"}); err != nil {
		t.Fatalf("pre-close Put: %v", err)
	}
	seam.closeAll()
	_, err = seam.mem.Put(ctx, q,
		memory.ConversationTurn{UserMessage: "post-close", AssistantResponse: "no"})
	if !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Put after store Close: err=%v, want ErrStoreClosed", err)
	}
}
