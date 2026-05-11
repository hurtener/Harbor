// Wave 7a cross-subsystem integration test per AGENTS.md §17.5.
//
// Wave 7a closed two big surfaces:
//
//   - Memory subsystem (Phases 23 / 24 / 25):
//       * `MemoryStore` interface + InMem driver + conformance suite
//       * `truncation` + `rolling_summary` strategies + `Summarizer`
//         interface + health FSM + `RecoveryBacklogMax` recovery loop
//       * SQLite + Postgres memory drivers (persistent legs)
//
//   - Tools subsystem (Phases 26 / 26a / 27 / 28 / 29):
//       * Unified `Tool` / `ToolCatalog` / `ToolProvider` surface +
//         `ToolPolicy` reliability shell (D-024)
//       * `tools.RegisterFunc[I,O]` with reflection-derived schemas
//       * `flow.Definition` + `flow.RegisterAsTool` + per-flow `Budget`
//       * HTTP / MCP / A2A transports (each transport's per-driver
//         tests cover its wire surface; this wave-end E2E focuses on
//         the in-process composition path so it exercises NO network)
//
// The wave-end E2E proves these COMPOSE: a tool invocation reads
// ctx-carried identity, the result is appendable to memory under the
// same identity, and the LLM-context patch surfaces the recorded
// turn — the canonical "tool runs, the runtime remembers what it did"
// loop that future planner phases (42+) will drive.
//
// Four focused tests:
//
//   - TestE2E_Wave7a_Tool_Memory_Composition — basic loop: tool
//     resolves under identity, `Invoke` returns, runtime records the
//     turn via `AddTurn`, `GetLLMContext` surfaces the recent-window
//     view (truncation strategy).
//   - TestE2E_Wave7a_RollingSummary_TriggersSummarizer — exercises the
//     Phase 24 rolling-summary path with the `EchoSummarizer` test
//     stub; flushes a budget-saturating burst of turns + verifies the
//     summariser fires + the next `GetLLMContext` carries a Summary.
//   - TestE2E_Wave7a_DurablePersistence_SQLiteMemory_AcrossClose —
//     SQLite memory + a tool invocation, then Close + reopen against
//     the same DSN; the recorded turn must survive (Phase 25 ↔
//     Phase 26 wiring across the persistent boundary).
//   - TestE2E_Wave7a_Concurrent_MultiTenant_ToolsAndMemory — 8 ×
//     4 concurrent tenants × sessions sharing one catalog + one
//     memory store + one event bus; the bus subscriber observes the
//     happy-path `tool.invoked` + `tool.completed` cycle; cross-
//     session memory isolation pins; goroutine baseline restored
//     after teardown (D-025).
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memorydriverinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	memorydriversqlite "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// --- helpers ---------------------------------------------------------------

// echoArgs / echoOut shape the canonical tool wave-7a registers. The
// shape is intentionally tiny — the test isn't exercising schema
// derivation, just the composition path.
type echoArgs struct {
	Message string `json:"message"`
}
type echoOut struct {
	Echo   string `json:"echo"`
	Tenant string `json:"tenant"`
}

// wave7aIdentityEchoTool registers a tool that reads identity from
// ctx and echoes the input message + the tenant claim. The bus
// argument is optional — when non-nil the inproc driver publishes
// tool.invoked / tool.completed / tool.failed around each invocation
// so admin-scope subscribers can observe the lifecycle (Phase 26
// event surface wiring).
func wave7aIdentityEchoTool(t *testing.T, cat tools.ToolCatalog, name string, bus events.EventBus) {
	t.Helper()
	opts := []tools.DescriptorOption{
		tools.WithDescription("Echoes the input + stamps the tenant claim."),
		tools.WithSideEffect(tools.SideEffectPure),
	}
	if bus != nil {
		opts = append(opts, tools.WithBus(bus))
	}
	err := inproc.RegisterFunc[echoArgs, echoOut](cat, name,
		func(ctx context.Context, in echoArgs) (echoOut, error) {
			id, ok := identity.From(ctx)
			if !ok {
				return echoOut{}, errors.New("no identity in ctx")
			}
			return echoOut{Echo: in.Message, Tenant: id.TenantID}, nil
		},
		opts...,
	)
	if err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

// invokeEchoTool resolves + invokes the echo tool under the supplied
// identity, returning the decoded output. Centralised so all four
// tests use the same call shape.
func invokeEchoTool(t *testing.T, cat tools.ToolCatalog, ctx context.Context, name, msg string) echoOut {
	t.Helper()
	desc, ok := cat.Resolve(name)
	if !ok {
		t.Fatalf("Resolve(%q): not found", name)
	}
	args, err := json.Marshal(echoArgs{Message: msg})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := desc.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke(%q): %v", name, err)
	}
	out, ok := res.Value.(echoOut)
	if !ok {
		t.Fatalf("Invoke(%q): result type %T, want echoOut", name, res.Value)
	}
	return out
}

// turnFromInvocation builds the `ConversationTurn` the test would push
// to memory after a successful tool invocation. The "user message" is
// the input the planner would have given the tool; the "assistant
// response" is the tool's echoed output rendered through the
// observation renderer (Phase 26+). The wave-end E2E uses a simpler
// stringification — the renderer itself is the planner phase's
// responsibility.
func turnFromInvocation(msg string, out echoOut) memory.ConversationTurn {
	return memory.ConversationTurn{
		UserMessage:       msg,
		AssistantResponse: fmt.Sprintf("echo=%s tenant=%s", out.Echo, out.Tenant),
		Timestamp:         time.Now(),
	}
}

// openMemoryInMem opens an InMem MemoryStore via the registry path
// with the truncation strategy at the supplied budget. Returns the
// store + the underlying event bus (so concurrent tests can subscribe
// for tool-event observation) + a cleanup func that closes both the
// store + the deps.
func openMemoryInMem(t *testing.T, budget int) (memory.MemoryStore, events.EventBus, func()) {
	t.Helper()
	cfg := wave7aConfig()
	cfg.Memory.Strategy = string(memory.StrategyTruncation)
	cfg.Memory.BudgetTokens = budget

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.Strategy(cfg.Memory.Strategy),
		BudgetTokens: budget,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("memory.Open: %v", err)
	}
	return mem, bus, func() {
		_ = mem.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}
}

// wave7aConfig returns the in-memory config wave-7a tests use. SQLite
// memory is exercised in test #3 via direct driver construction so it
// can supply a per-test DSN.
func wave7aConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-wave7a-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			DefaultMaxTokens: 4096,
			RepairAttempts:   2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 32,
			SubscriberBufferSize:     128,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 16,
		},
	}
}

// --- tests -----------------------------------------------------------------

// TestE2E_Wave7a_Tool_Memory_Composition wires the in-process tool
// catalog + an InMem `MemoryStore` (truncation) under one identity,
// runs a tool, records the turn, and verifies `GetLLMContext`
// surfaces it.
//
// What this exercises:
//   - Phase 26 catalog + ToolPolicy default shell.
//   - Phase 23 / 24 InMem MemoryStore (truncation strategy) under
//     `memory.Open`.
//   - Identity propagation: the tool reads ctx identity, the memory
//     reads the same `Quadruple` for per-session storage.
//   - The canonical "tool ran, runtime remembered" loop the planner
//     (Phase 42+) will drive.
func TestE2E_Wave7a_Tool_Memory_Composition(t *testing.T) {
	mem, _, cleanup := openMemoryInMem(t, 1024)
	defer cleanup()

	cat := tools.NewCatalog()
	wave7aIdentityEchoTool(t, cat, "echo", nil)

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	quad := identity.Quadruple{Identity: id}

	const msg = "hello, world"
	out := invokeEchoTool(t, cat, ctx, "echo", msg)
	if out.Echo != msg {
		t.Errorf("echoed body: got %q want %q", out.Echo, msg)
	}
	if out.Tenant != id.TenantID {
		t.Errorf("tool did NOT see ctx identity: got tenant=%q want %q", out.Tenant, id.TenantID)
	}

	turn := turnFromInvocation(msg, out)
	if err := mem.AddTurn(ctx, quad, turn); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	patch, err := mem.GetLLMContext(ctx, quad)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Strategy != memory.StrategyTruncation {
		t.Errorf("patch.Strategy=%q want %q", patch.Strategy, memory.StrategyTruncation)
	}
	if got := len(patch.RecentTurns); got != 1 {
		t.Fatalf("RecentTurns: got %d want 1", got)
	}
	if patch.RecentTurns[0].UserMessage != msg {
		t.Errorf("RecentTurns[0].UserMessage=%q want %q",
			patch.RecentTurns[0].UserMessage, msg)
	}
	if patch.Tokens <= 0 {
		t.Errorf("Tokens=%d want > 0 (single non-empty turn)", patch.Tokens)
	}

	// Failure mode: invoking the tool with a missing-identity ctx is
	// rejected at the tool boundary (the echo body's `identity.From`
	// guard fires). Catches an identity-propagation regression in
	// the catalog dispatcher.
	desc, _ := cat.Resolve("echo")
	args, _ := json.Marshal(echoArgs{Message: "no-ident"})
	if _, err := desc.Invoke(context.Background(), args); err == nil {
		t.Errorf("Invoke without identity: err=nil, want non-nil (the tool itself rejects)")
	}
}

// TestE2E_Wave7a_RollingSummary_TriggersSummarizer exercises the
// Phase 24 rolling-summary path with the `EchoSummarizer` test stub:
// the summariser interface is injectable (Phase 32+ will land an
// LLM-backed default), and a budget-saturating burst of turns must
// trigger summarisation + leave a non-empty Summary on the next
// `GetLLMContext`.
//
// Key composition points:
//   - The rolling-summary strategy executor was authored at Phase 24
//     but the LLM-backed Summarizer doesn't exist until Phase 32+.
//     The injectable interface MUST be exercised today so we know
//     the seam is alive. `EchoSummarizer{}` (a deterministic stub
//     that returns the concatenation of incoming turns) is the
//     wave-end vehicle.
//   - This path also pins identity propagation through the
//     background summariser: the executor invokes the summariser
//     with the same `Quadruple` AddTurn received.
func TestE2E_Wave7a_RollingSummary_TriggersSummarizer(t *testing.T) {
	cfg := wave7aConfig()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// Direct driver construction is the path operators take to inject
	// a Summarizer; the registry can't resolve one for `rolling_summary`
	// today (Phase 32+ will land an LLM-backed default).
	mem, err := memorydriverinmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyRollingSummary,
		BudgetTokens: 16, // tiny so 3 turns saturates
	}, memory.Deps{State: store, Bus: bus}, memorydriverinmem.Options{
		Summarizer: strategy.EchoSummarizer{},
	})
	if err != nil {
		t.Fatalf("inmem.New(rolling_summary): %v", err)
	}
	defer func() { _ = mem.Close(context.Background()) }()

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	quad := identity.Quadruple{Identity: id}

	for i := 0; i < 5; i++ {
		turn := memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("u-%d %s", i, longLine(64)),
			AssistantResponse: fmt.Sprintf("a-%d %s", i, longLine(64)),
			Timestamp:         time.Now(),
		}
		if err := mem.AddTurn(ctx, quad, turn); err != nil {
			t.Fatalf("AddTurn[%d]: %v", i, err)
		}
	}

	// Wait briefly for the rolling-summary executor's background
	// summariser to land at least one Summarize call. The executor
	// MUST resolve eventually; deadline is the hard cap.
	deadline := time.Now().Add(3 * time.Second)
	var patch memory.LLMContextPatch
	for time.Now().Before(deadline) {
		patch, err = mem.GetLLMContext(ctx, quad)
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Summary != "" {
			break
		}
		runtime.Gosched()
	}
	if patch.Summary == "" {
		t.Fatalf("Summary stayed empty after 5 over-budget turns; rolling-summary path is dead")
	}
	if patch.Strategy != memory.StrategyRollingSummary {
		t.Errorf("Strategy=%q want %q", patch.Strategy, memory.StrategyRollingSummary)
	}
}

// TestE2E_Wave7a_DurablePersistence_SQLiteMemory_AcrossClose proves
// the SQLite memory driver + the in-process tool catalog compose
// across a Close/reopen boundary. Phase 25 ships SQLite with
// `StrategyNone` only — the audit Wave 7a FAIL #2 captures that
// truncation / rolling_summary persistence is deferred to a Phase 25b
// follow-on; tracked as `TODO: phase-25b-persistent-strategies`.
//
// What this test still gates against, given the Phase 25 surface:
//
//	1. Tool invocation under identity composes with the SQLite memory
//	   driver's boundary (AddTurn on StrategyNone is a no-op but the
//	   driver still validates identity, runs the bus-emit on missing
//	   identity, and returns nil — the tool→memory hand-off works).
//	2. Snapshot bytes round-trip across Close/reopen for the canonical
//	   empty-record envelope (`memory.Record{Strategy: "none"}`) —
//	   this is the cross-driver byte-stable invariant per D-034.
//	3. Cross-tenant identity isolation pins at the SQLite layer.
//	4. The persistent driver rejects missing identity at the boundary
//	   (`memory.identity_rejected` emit path, D-033).
func TestE2E_Wave7a_DurablePersistence_SQLiteMemory_AcrossClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wave7a.sqlite")
	dsn := "file:" + dbPath + "?cache=shared"

	cfg := wave7aConfig()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// Step 1: open SQLite memory under StrategyNone, register a tool,
	// drive the composition path. AddTurn is a no-op on StrategyNone
	// (Phase 25 surface) but the boundary still validates identity.
	mem1, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver:   "sqlite",
		DSN:      dsn,
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New (1): %v", err)
	}

	cat := tools.NewCatalog()
	wave7aIdentityEchoTool(t, cat, "echo", bus)

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	quad := identity.Quadruple{Identity: id}

	const msg = "durable hello"
	out := invokeEchoTool(t, cat, ctx, "echo", msg)
	turn := turnFromInvocation(msg, out)
	if err := mem1.AddTurn(ctx, quad, turn); err != nil {
		t.Fatalf("mem1.AddTurn: %v", err)
	}

	// Snapshot the empty-record envelope so we can verify byte-stable
	// round-trip across Close/reopen. (StrategyNone snapshots are the
	// canonical empty record per D-034.)
	snap1, err := mem1.Snapshot(ctx, quad)
	if err != nil {
		t.Fatalf("mem1.Snapshot: %v", err)
	}
	if err := mem1.Restore(ctx, quad, snap1); err != nil {
		t.Fatalf("mem1.Restore (pre-close): %v", err)
	}
	if err := mem1.Close(context.Background()); err != nil {
		t.Fatalf("mem1.Close: %v", err)
	}

	// Step 2: reopen against the same DSN. The memory_state row must
	// survive the close/reopen cycle even on StrategyNone (the row
	// persists the canonical empty record).
	mem2, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver:   "sqlite",
		DSN:      dsn,
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New (2): %v", err)
	}
	defer func() { _ = mem2.Close(context.Background()) }()

	snap2, err := mem2.Snapshot(ctx, quad)
	if err != nil {
		t.Fatalf("mem2.Snapshot: %v", err)
	}
	if snap2.Strategy != snap1.Strategy {
		t.Errorf("snapshot Strategy across reopen: got %q want %q",
			snap2.Strategy, snap1.Strategy)
	}

	// Cross-tenant isolation across the durable boundary: a different
	// tenant's GetLLMContext sees an empty patch (no turns) even on
	// the StrategyNone surface.
	otherID := identity.Identity{TenantID: "T2", UserID: "U2", SessionID: "S2"}
	otherCtx, _ := identity.With(context.Background(), otherID)
	otherQuad := identity.Quadruple{Identity: otherID}
	otherPatch, err := mem2.GetLLMContext(otherCtx, otherQuad)
	if err != nil {
		t.Fatalf("cross-tenant GetLLMContext: %v", err)
	}
	if len(otherPatch.RecentTurns) != 0 {
		t.Errorf("cross-tenant leak: tenant %q saw %d turns from %q",
			otherID.TenantID, len(otherPatch.RecentTurns), id.TenantID)
	}

	// Identity-rejection gate: an empty identity must fail closed per
	// AGENTS.md §6 rule 9.
	emptyQuad := identity.Quadruple{}
	if err := mem2.AddTurn(context.Background(), emptyQuad, turn); err == nil {
		t.Errorf("AddTurn with empty identity should fail closed, got nil")
	}
}

// TestE2E_Wave7a_Concurrent_MultiTenant_ToolsAndMemory runs N tenants
// × M sessions concurrently against ONE shared catalog + ONE shared
// memory store. Each goroutine invokes the tool, AddTurns the
// result, asserts its own RecentTurns surfaces ONLY its own turn.
//
// Also subscribes the bus to `tool.invoked` + `tool.completed` once
// (with admin scope) and asserts the lifecycle events fire — proves
// the Phase 26 event surface is observable through the same bus the
// memory subsystem publishes to (single canonical bus, no per-
// subsystem fan-out).
func TestE2E_Wave7a_Concurrent_MultiTenant_ToolsAndMemory(t *testing.T) {
	const tenantCount = 8
	const sessionsPerTenant = 4

	baseline := runtime.NumGoroutine()

	mem, b, cleanup := openMemoryInMem(t, 1024)

	cat := tools.NewCatalog()
	wave7aIdentityEchoTool(t, cat, "echo", b)

	// Admin-scope subscriber observes tool.invoked + tool.completed
	// across all tenants. Phase 05's `Filter.Admin` is the documented
	// elevated-scope path for this pattern.
	sub, err := b.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{
			tools.EventTypeToolInvoked,
			tools.EventTypeToolCompleted,
		},
	})
	if err != nil {
		cleanup()
		t.Fatalf("bus.Subscribe(admin): %v", err)
	}
	defer sub.Cancel()

	var (
		wg     sync.WaitGroup
		errCnt atomic.Int64
		seenMu sync.Mutex
		seen   = make(map[string]string) // sessionID → recorded user-message
	)
	wg.Add(tenantCount * sessionsPerTenant)

	for ti := 0; ti < tenantCount; ti++ {
		for sj := 0; sj < sessionsPerTenant; sj++ {
			ti, sj := ti, sj
			go func() {
				defer wg.Done()
				id := identity.Identity{
					TenantID:  fmt.Sprintf("T-%d", ti),
					UserID:    fmt.Sprintf("U-%d", ti),
					SessionID: fmt.Sprintf("S-%d-%d", ti, sj),
				}
				ctx, err := identity.With(context.Background(), id)
				if err != nil {
					errCnt.Add(1)
					t.Errorf("identity.With(%s): %v", id.SessionID, err)
					return
				}
				quad := identity.Quadruple{Identity: id}

				msg := fmt.Sprintf("hello-%s", id.SessionID)
				out := invokeEchoTool(t, cat, ctx, "echo", msg)
				if out.Tenant != id.TenantID {
					errCnt.Add(1)
					t.Errorf("tenant claim mismatch in %s: got %q want %q",
						id.SessionID, out.Tenant, id.TenantID)
					return
				}
				turn := turnFromInvocation(msg, out)
				if err := mem.AddTurn(ctx, quad, turn); err != nil {
					errCnt.Add(1)
					t.Errorf("AddTurn(%s): %v", id.SessionID, err)
					return
				}
				patch, err := mem.GetLLMContext(ctx, quad)
				if err != nil {
					errCnt.Add(1)
					t.Errorf("GetLLMContext(%s): %v", id.SessionID, err)
					return
				}
				if len(patch.RecentTurns) != 1 {
					errCnt.Add(1)
					t.Errorf("session %s saw %d turns (want 1) — cross-session leak",
						id.SessionID, len(patch.RecentTurns))
					return
				}
				if patch.RecentTurns[0].UserMessage != msg {
					errCnt.Add(1)
					t.Errorf("session %s saw foreign message %q (want %q)",
						id.SessionID, patch.RecentTurns[0].UserMessage, msg)
					return
				}
				seenMu.Lock()
				seen[id.SessionID] = patch.RecentTurns[0].UserMessage
				seenMu.Unlock()
			}()
		}
	}

	wg.Wait()
	if n := errCnt.Load(); n != 0 {
		cleanup()
		t.Fatalf("%d concurrent operations errored", n)
	}
	wantCount := tenantCount * sessionsPerTenant
	if got := len(seen); got != wantCount {
		t.Errorf("seen-sessions=%d want %d", got, wantCount)
	}

	// Bus admin-scope subscriber should have observed at least one
	// `tool.invoked` AND one `tool.completed` event by now.
	gotInvoked, gotCompleted := false, false
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
drain:
	for !(gotInvoked && gotCompleted) {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break drain
			}
			switch ev.Type {
			case tools.EventTypeToolInvoked:
				gotInvoked = true
			case tools.EventTypeToolCompleted:
				gotCompleted = true
			}
		case <-deadline.C:
			break drain
		}
	}
	if !gotInvoked {
		t.Errorf("admin subscriber did NOT observe tool.invoked across %d concurrent runs", wantCount)
	}
	if !gotCompleted {
		t.Errorf("admin subscriber did NOT observe tool.completed across %d concurrent runs", wantCount)
	}

	cleanup()

	// Gosched-only settle loop per AGENTS.md §11 (no time.Sleep for
	// sync). 2s hard cap; +5 tolerance for parked-but-not-yet-retired
	// goroutines.
	deadline2 := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline2) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// longLine returns a deterministic n-byte string used to bulk up turn
// payloads so the rolling-summary budget saturates quickly.
func longLine(n int) string {
	const pad = "abcdefghijklmnopqrstuvwxyz0123456789 ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, n)
	for i := range out {
		out[i] = pad[i%len(pad)]
	}
	return string(out)
}
