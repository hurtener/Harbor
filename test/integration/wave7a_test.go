// Cross-subsystem tool events, identity isolation and cumulative memory.
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

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
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
// ctx and echoes the input message + the tenant claim. Tool-lifecycle
// events (tool.invoked / tool.completed / tool.failed) are emitted by
// the catalog's universal descriptor-wrap shell when the catalog was
// constructed with a bus (tools.WithCatalogBus) — NOT per-registration —
// so admin-scope subscribers observe the lifecycle for every registered
// tool regardless of transport. Callers that want to observe the
// lifecycle build the catalog with tools.WithCatalogBus(bus).
func wave7aIdentityEchoTool(t *testing.T, cat tools.ToolCatalog, name string) {
	t.Helper()
	err := inproc.RegisterFunc[echoArgs, echoOut](cat, name,
		func(ctx context.Context, in echoArgs) (echoOut, error) {
			id, ok := identity.From(ctx)
			if !ok {
				return echoOut{}, errors.New("no identity in ctx")
			}
			return echoOut{Echo: in.Message, Tenant: id.TenantID}, nil
		},
		tools.WithDescription("Echoes the input + stamps the tenant claim."),
		tools.WithSideEffect(tools.SideEffectPure),
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
func wave7aNoteQuery(t *testing.T, item memory.Item) string {
	t.Helper()
	var note struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(item.Value, &note); err != nil {
		t.Fatal(err)
	}
	return note.Query
}

func turnFromInvocation(msg string, out echoOut) memory.ConversationTurn {
	return memory.ConversationTurn{
		UserMessage:       msg,
		AssistantResponse: fmt.Sprintf("echo=%s tenant=%s", out.Echo, out.Tenant),
	}
}

// openMemoryInMem opens an InMem MemoryStore via the registry path
// with the cumulative strategy at the supplied budget. Returns the
// store + the underlying event bus (so concurrent tests can subscribe
// for tool-event observation) + a cleanup func that closes both the
// store + the deps.
func openMemoryInMem(t *testing.T, budget int) (memory.MemoryStore, events.EventBus, func()) {
	t.Helper()
	cfg := wave7aConfig()
	cfg.Memory.Strategy = string(memory.StrategyRollingSummary)
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
	}, memory.Deps{State: store, Bus: bus, Redactor: red})
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
			RepairAttempts: 2,
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
			Driver:   "inmem",
			Strategy: "none",
		},
	}
}

// --- tests -----------------------------------------------------------------

// TestE2E_Wave7a_Tool_Memory_Composition wires the in-process tool
// catalog + an InMem `MemoryStore` (cumulative) under one identity,
// runs a tool, records the turn, and verifies `Inspect`
// surfaces it.
//
// What this exercises:
//   - Phase 26 catalog + ToolPolicy default shell.
//   - Phase 23 / 24 InMem MemoryStore (cumulative strategy) under
//     `memory.Open`.
//   - Identity propagation: the tool reads ctx identity, the memory
//     reads the same `Quadruple` for per-session storage.
//   - The canonical "tool ran, runtime remembered" loop the planner
//     (Phase 42+) will drive.
func TestE2E_Wave7a_Tool_Memory_Composition(t *testing.T) {
	mem, _, cleanup := openMemoryInMem(t, 1024)
	defer cleanup()

	cat := tools.NewCatalog()
	wave7aIdentityEchoTool(t, cat, "echo")

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
	if _, err := mem.Put(ctx, quad, turn); err != nil {
		t.Fatalf("Put: %v", err)
	}

	patch, err := mem.Inspect(ctx, quad)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if patch.Strategy != memory.StrategyRollingSummary {
		t.Errorf("patch.Strategy=%q want %q", patch.Strategy, memory.StrategyRollingSummary)
	}
	if got := len(patch.Items); got != 1 {
		t.Fatalf("RecentTurns: got %d want 1", got)
	}
	if wave7aNoteQuery(t, patch.Items[0]) != msg {
		t.Errorf("RecentTurns[0].UserMessage=%q want %q",
			wave7aNoteQuery(t, patch.Items[0]), msg)
	}
	if patch.EstimatedTokens <= 0 {
		t.Errorf("Tokens=%d want > 0 (single non-empty turn)", patch.EstimatedTokens)
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

// The production compactor must run between user turns and carry its prior
// checkpoint through subsequent actual decision requests.
func TestE2E_Wave7a_RollingSummary_TriggersSummarizer(t *testing.T) {
	seam := newBudgetSeam(t, 10000, 100000)
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	for i := range 25 {
		query := fmt.Sprintf("edit-%d %s", i, longLine(64))
		if i == 0 {
			query = budgetConstraint + " " + query
		}
		decision := budgetTurn(t, seam, id, fmt.Sprintf("wave7a-%d", i), query)
		if !strings.Contains(decision.body, budgetConstraint) {
			t.Fatalf("turn %d lost early context", i)
		}
	}
	view, err := seam.mem.Inspect(t.Context(), identity.Quadruple{Identity: id})
	if err != nil || view.Summary == "" || seam.driver.summaryCount() == 0 {
		t.Fatalf("compactor not exercised: %+v, %v", view, err)
	}
}

// Tool-derived administrative notes survive a real SQLite close/reopen; they
// are not an alternate transcript or authority to execute the tool again.
func TestE2E_Wave7a_DurablePersistence_SQLiteMemory_AcrossClose(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "wave7a.sqlite")
	mem, bus, closeAll := phase24Memory(t, "sqlite", dsn, 20)
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	wave7aIdentityEchoTool(t, cat, "echo")
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: id}
	const msg = "durable hello"
	out := invokeEchoTool(t, cat, ctx, "echo", msg)
	key, err := mem.Put(ctx, q, turnFromInvocation(msg, out))
	if err != nil {
		t.Fatal(err)
	}
	before, err := mem.Inspect(ctx, q)
	if err != nil || len(before.Items) != 1 {
		t.Fatalf("before close: %+v, %v", before, err)
	}
	closeAll()
	reopened, _, _ := phase24Memory(t, "sqlite", dsn, 20)
	after, err := reopened.Inspect(ctx, q)
	if err != nil || len(after.Items) != 1 {
		t.Fatalf("reopen: %+v, %v", after, err)
	}
	if after.Items[0].Key != key || string(after.Items[0].Value) != string(before.Items[0].Value) || !after.Items[0].ExpiresAt.Equal(before.Items[0].ExpiresAt) {
		t.Fatal("reopen changed the note or renewed retention")
	}
	other := identity.Quadruple{Identity: identity.Identity{TenantID: "T2", UserID: "U2", SessionID: "S2"}}
	view, err := reopened.Inspect(t.Context(), other)
	if err != nil || len(view.Items) != 0 {
		t.Fatalf("cross-tenant note read: %+v, %v", view, err)
	}
	if _, err := reopened.Put(t.Context(), identity.Quadruple{}, turnFromInvocation(msg, out)); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("missing identity: %v", err)
	}
}

// TestE2E_Wave7a_Concurrent_MultiTenant_ToolsAndMemory runs N tenants
// × M sessions concurrently against ONE shared catalog + ONE shared
// memory store. Each goroutine invokes the tool, Puts the
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

	cat := tools.NewCatalog(tools.WithCatalogBus(b))
	wave7aIdentityEchoTool(t, cat, "echo")

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

	for ti := range tenantCount {
		for sj := range sessionsPerTenant {
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
				if _, err := mem.Put(ctx, quad, turn); err != nil {
					errCnt.Add(1)
					t.Errorf("Put(%s): %v", id.SessionID, err)
					return
				}
				patch, err := mem.Inspect(ctx, quad)
				if err != nil {
					errCnt.Add(1)
					t.Errorf("Inspect(%s): %v", id.SessionID, err)
					return
				}
				if len(patch.Items) != 1 {
					errCnt.Add(1)
					t.Errorf("session %s saw %d turns (want 1) — cross-session leak",
						id.SessionID, len(patch.Items))
					return
				}
				if wave7aNoteQuery(t, patch.Items[0]) != msg {
					errCnt.Add(1)
					t.Errorf("session %s saw foreign message %q (want %q)",
						id.SessionID, wave7aNoteQuery(t, patch.Items[0]), msg)
					return
				}
				seenMu.Lock()
				seen[id.SessionID] = wave7aNoteQuery(t, patch.Items[0])
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
	for !gotInvoked || !gotCompleted {
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
