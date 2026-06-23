// Wave-end cross-subsystem integration test (CLAUDE.md §17.5 + §17.7
// step 5) for the runtime-hardening + observability band (the phases
// that ship runtime ctx/retention hardening, the observability
// foundation, the persistence/driver-registry dedup, and the memory
// context budget):
//
//   - runtime retention / ctx hardening — long-lived components are
//     cancellable and joined on shutdown; no goroutine leak after Close.
//   - runtime observability foundation — telemetry.MetricsRegistry +
//     RegisterRuntimeGauges (governance-cache + events-dropped gauges),
//     read back through Snapshot the same way the Protocol
//     metrics.snapshot surface reads them.
//   - the memory context budget + the D-241 refinement — a rolling
//     summary that exceeds the heavy byte threshold is exempt from the
//     LLM-edge ErrContextLeak (it is conversation context), while an
//     offloadable RoleTool observation that exceeds it still trips.
//
// Each phase ships its own focused test; this aggregator proves the
// surfaces COMPOSE under concurrency without cross-run bleed or leaks.
//
// # Per CLAUDE.md §17.3
//
//  1. Real drivers everywhere on the seam — real audit redactor, real
//     inmem EventBus, real SQLite StateStore, real rolling_summary
//     MemoryStore, the real governance enforcement Subsystem, the real
//     LLM-edge safety pass, and the real telemetry MetricsRegistry. The
//     mock LLM *provider* underneath the safety pass is acceptable: the
//     seam under test is memory/telemetry/governance composition, not
//     the LLM driver. The EchoSummarizer is the deterministic,
//     network-free Summarizer the rolling_summary executor consumes —
//     it is the production seam shape, not a mock at the boundary.
//  2. Identity propagation — every session carries a distinct
//     (tenant,user,session,run) quadruple through ctx + the explicit
//     storage-method identity argument; the concurrency test asserts no
//     cross-session bleed across the shared MemoryStore.
//  3. ≥1 failure mode — TestE2E_WaveRuntimeHardening_FailLoudModes proves
//     missing-identity rejection (memory + LLM edge) and a closed-store
//     mid-operation both fail loud with typed errors, never silently.
//  4. `-race` is the CI gate.
//
// The runtime gauges this stack can source without an engine.Engine
// node-graph host are the governance-cache + events-dropped gauges (the
// same two assemble.Assemble wires for a planner/RunLoop-shaped stack).
// The active-runs / engine-capacity gauges need an engine host; that
// limitation is called out at the gauge-observability assertion.
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

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory/strategy"
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

// whardSeam bundles the wave's wired subsystems with real drivers on
// every seam, exposing the bus, stores, the governance-wrapped safety
// LLM client, the telemetry registry, and the governance Subsystem so
// the composition assertions can reach them. closeAll tears the stack
// down in reverse dependency order, joining the bus + metrics-bridge
// goroutines — it is idempotent (the t.Cleanup call and an explicit
// retention-test call are both safe).
type whardSeam struct {
	bus      events.EventBus
	state    state.StateStore
	mem      memory.MemoryStore
	client   llm.LLMClient // governance-wrapped safety client
	metrics  *telemetry.MetricsRegistry
	gov      governance.Subsystem
	closeAll func()
}

// newWhardSeam wires the wave's composed stack: real SQLite StateStore,
// real inmem EventBus (audit-redactor-paired), real rolling_summary
// MemoryStore (EchoSummarizer), the real governance enforcement
// Subsystem wrapping the real LLM-edge safety pass (mock provider), and
// the real telemetry MetricsRegistry with the runtime gauges registered
// EXACTLY as assemble.Assemble registers them (governance.CacheLen +
// the bus DroppedCounter). A bus->metrics bridge goroutine runs so the
// retention test has a real long-lived goroutine to join.
// whardBudgetTokens / whardWindowTokens are the fixed per-session token
// budget and model context-window the wave seam runs with — constant
// across every caller, so they are package constants, not newWhardSeam
// parameters (unparam).
const (
	whardBudgetTokens = 8 * 1024
	whardWindowTokens = 1 << 20
)

func newWhardSeam(t *testing.T) *whardSeam {
	t.Helper()
	ctx := context.Background()
	var closers []func()

	red, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}

	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	closers = append(closers, func() { _ = bus.Close(ctx) })

	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: dbPath})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	closers = append(closers, func() { _ = store.Close(ctx) })

	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyRollingSummary,
		BudgetTokens: whardBudgetTokens,
	}, memory.Deps{State: store, Bus: bus, Summarizer: strategy.EchoSummarizer{}},
		inmem.Options{Summarizer: strategy.EchoSummarizer{}})
	if err != nil {
		t.Fatalf("memory inmem.New: %v", err)
	}
	closers = append(closers, func() { _ = mem.Close(ctx) })

	artStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}

	safety, err := llm.Open(ctx, llm.ConfigSnapshot{
		Driver:               "mock",
		Provider:             "mock",
		Model:                "m",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: budgetHeavyThreshold,
		ModelProfiles: map[string]llm.ModelProfile{
			"m": {ContextWindowTokens: whardWindowTokens, TokenEstimator: "chars_div_4"},
		},
	}, llm.Deps{Artifacts: artStore, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	closers = append(closers, func() { _ = safety.Close(ctx) })

	// Real governance enforcement Subsystem: a single generous tier so
	// PreCall (rate cache) + PostCall (cost cache) populate one cache
	// entry each per distinct identity, exactly as the production
	// governance band does. Wrapping the safety client is the production
	// composition (governance.Wrap over the llm.Open result).
	gov, err := governance.NewSubsystemFromConfig(governance.Config{
		DefaultTier: "std",
		IdentityTiers: map[string]governance.TierConfig{
			"std": {
				BudgetCeilingUSD: 1_000_000,
				RateLimit:        governance.RateLimitConfig{Capacity: 1_000_000},
			},
		},
	}, store, bus)
	if err != nil {
		t.Fatalf("governance.NewSubsystemFromConfig: %v", err)
	}
	client := governance.Wrap(safety, gov)

	// Real telemetry MetricsRegistry. A ManualReader is injected (the
	// documented test-only seam) so Snapshot is observable without a
	// metric-exporter driver per test binary — the same shape the
	// devstack injects.
	metrics, metricsShutdown, err := telemetry.NewMetricsRegistry(
		config.TelemetryConfig{ServiceName: "harbor-wave-hardening-test"},
		telemetry.WithMetricReader(sdkmetric.NewManualReader()),
	)
	if err != nil {
		t.Fatalf("telemetry.NewMetricsRegistry: %v", err)
	}
	closers = append(closers, func() { _ = metricsShutdown(ctx) })

	// Source the runtime gauges identically to assemble.Assemble: the
	// governance-cache gauge via governance.CacheLen, the events-dropped
	// gauge via the bus DroppedCounter capability. The engine gauges
	// (active runs, capacity entries) are left nil — this stack hosts no
	// engine.Engine node-graph, and the gauge seam skips a nil callback.
	var eventsDropped func() int64
	if dc, ok := bus.(events.DroppedCounter); ok {
		eventsDropped = dc.DroppedTotal
	}
	if err := metrics.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		GovernanceCacheEntries: func() int64 { return int64(governance.CacheLen(gov)) },
		EventsDropped:          eventsDropped,
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges: %v", err)
	}

	// Bus -> metrics bridge: a real long-lived goroutine (the
	// metrics-as-derivation wiring). Stopping it joins the drain
	// goroutine — the retention test relies on this to prove the
	// join-on-shutdown contract.
	bridgeStop, err := telemetry.BridgeBusToMetrics(ctx, bus, metrics, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("BridgeBusToMetrics: %v", err)
	}
	closers = append(closers, bridgeStop)

	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
		})
	}
	t.Cleanup(closeAll)

	return &whardSeam{
		bus: bus, state: store, mem: mem, client: client,
		metrics: metrics, gov: gov, closeAll: closeAll,
	}
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

// TestE2E_WaveRuntimeHardening_ConcurrentSessionIsolation is the core
// concurrency proof: N distinct sessions drive many turns each into ONE
// shared rolling_summary MemoryStore until compaction triggers, all
// under -race. It asserts (a) per-session budget enforcement — every
// assembled GetLLMContext patch stays within budget_tokens — and (b) no
// cross-session bleed — session i's marker never appears in session j's
// assembled context.
func TestE2E_WaveRuntimeHardening_ConcurrentSessionIsolation(t *testing.T) {
	const (
		N             = 12
		turnsPerSess  = 40
		userFillChars = 2000
	)
	seam := newWhardSeam(t)

	var (
		wg    sync.WaitGroup
		fails atomic.Int64
	)
	// Each goroutine's assembled patch text, captured for the post-join
	// cross-session bleed sweep (kept off the hot path to avoid a shared
	// lock skewing the -race timing).
	patches := make([]string, N)

	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			ctx, q := whardSessionID(t, i)
			marker := whardMarker(i)
			for turn := range turnsPerSess {
				ct := memory.ConversationTurn{
					UserMessage: fmt.Sprintf("%s turn-%02d ", marker, turn) +
						strings.Repeat("u", userFillChars),
					AssistantResponse: strings.Repeat("a", userFillChars),
				}
				if err := seam.mem.AddTurn(ctx, q, ct); err != nil {
					t.Errorf("[sess %d] AddTurn %d: %v", i, turn, err)
					fails.Add(1)
					return
				}
			}
			patch, err := seam.mem.GetLLMContext(ctx, q)
			if err != nil {
				t.Errorf("[sess %d] GetLLMContext: %v", i, err)
				fails.Add(1)
				return
			}
			// (a) per-session budget enforcement.
			if patch.Tokens > whardBudgetTokens {
				t.Errorf("[sess %d] patch.Tokens=%d exceeds budget=%d — compaction did not hold the per-session budget",
					i, patch.Tokens, whardBudgetTokens)
				fails.Add(1)
				return
			}
			// Own marker must survive into the assembled context.
			joined := patch.Summary
			for _, rt := range patch.RecentTurns {
				joined += "\n" + rt.UserMessage + "\n" + rt.AssistantResponse
			}
			if !strings.Contains(joined, marker) {
				t.Errorf("[sess %d] own marker %q missing from assembled context", i, marker)
				fails.Add(1)
				return
			}
			patches[i] = joined
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d/%d concurrent sessions failed before the bleed sweep", fails.Load(), N)
	}

	// (b) no cross-session bleed: session i's assembled context must not
	// carry any OTHER session's marker.
	for i := range N {
		for j := range N {
			if i == j {
				continue
			}
			if strings.Contains(patches[i], whardMarker(j)) {
				t.Errorf("cross-session bleed: session %d's context carries session %d's marker %q",
					i, j, whardMarker(j))
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
		turn := memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("turn-%02d-", i) + strings.Repeat("u", 2000),
			AssistantResponse: strings.Repeat("a", 2000),
		}
		if err := seam.mem.AddTurn(ctx, id, turn); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	patch, err := seam.mem.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Tokens > budget {
		t.Fatalf("assembled context not within budget: Tokens=%d, budget=%d", patch.Tokens, budget)
	}
	// Precondition: the summary alone must exceed the heavy byte
	// threshold so the exemption (not a happens-to-be-small summary) is
	// what carries the request through.
	if len(patch.Summary) <= budgetHeavyThreshold {
		t.Fatalf("test precondition: summary (%d bytes) must exceed the heavy threshold (%d) to exercise the D-241 exemption",
			len(patch.Summary), budgetHeavyThreshold)
	}

	// D-241 exempt side: the heavy conversation summary passes the edge.
	if _, err := seam.client.Complete(ctx, patchToRequest(patch)); err != nil {
		t.Fatalf("conversation summary (%d bytes > %d threshold) tripped the safety edge — D-241 exemption broken: %v",
			len(patch.Summary), budgetHeavyThreshold, err)
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
	_, err = seam.client.Complete(ctx, leakReq)
	if !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("heavy RoleTool observation: err=%v, want ErrContextLeak", err)
	}
}

// TestE2E_WaveRuntimeHardening_RuntimeGaugesObservable proves the
// observability foundation composes with the live runtime: after driving
// the concurrent memory load (which fans bus events into the registry's
// counter) and a per-identity governance-wrapped LLM load, the runtime
// gauges are observable via Snapshot — the governance-cache gauge tracks
// governance.CacheLen and the events-dropped gauge is at baseline (no
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
				if err := seam.mem.AddTurn(ctx, q, ct); err != nil {
					t.Errorf("[gauge sess %d] AddTurn: %v", i, err)
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

	wantGovEntries := int64(governance.CacheLen(seam.gov))
	if wantGovEntries != int64(2*distinctIdentities) {
		t.Fatalf("governance cache = %d, want %d (2 per identity) — governance enforcement did not run on the wrapped client",
			wantGovEntries, 2*distinctIdentities)
	}

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
		t.Errorf("%s gauge = %v, want %d (must track governance.CacheLen)", whardGaugeGovCache, govVal, wantGovEntries)
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
				if err := seam.mem.AddTurn(ctx, q, ct); err != nil {
					t.Errorf("[retention sess %d] AddTurn: %v", i, err)
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
	err := seam.mem.AddTurn(context.Background(), badID,
		memory.ConversationTurn{UserMessage: "x", AssistantResponse: "y"})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Errorf("AddTurn with incomplete identity: err=%v, want ErrIdentityRequired", err)
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
	if err := seam.mem.AddTurn(ctx, q,
		memory.ConversationTurn{UserMessage: "pre-close", AssistantResponse: "ok"}); err != nil {
		t.Fatalf("pre-close AddTurn: %v", err)
	}
	seam.closeAll()
	err = seam.mem.AddTurn(ctx, q,
		memory.ConversationTurn{UserMessage: "post-close", AssistantResponse: "no"})
	if !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("AddTurn after store Close: err=%v, want ErrStoreClosed", err)
	}
}
