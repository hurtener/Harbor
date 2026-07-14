// Phase 174 cross-subsystem integration test per CLAUDE.md §17.1 — the
// sessions-projection counter enrichment (HA-22 / D-309) exercised
// end-to-end against REAL drivers at every seam, no mocks:
//
//   - internal/sessions            — the real StateStore-backed Registry.
//   - internal/sessions/protocol   — ListerProjector + CounterEnricher +
//     Service (the net-new aggregation).
//   - internal/events/drivers/inmem — the real event substrate the cost /
//     tokens / events counters read from.
//   - internal/tasks/drivers/inprocess — the real task registry the
//     tasks_count / has_failed_task read from.
//   - internal/runtime/pauseresume — the real pause coordinator the
//     has_pending_intervention reads from.
//
// It proves: a populated counter reaches the wire shape; cross-session
// isolation (session A's cost never bleeds into session B's row); a failure
// mode (filter.agent_ids over an unpopulated binding fails loud); and an
// N≥10 concurrency stress with no cross-talk. Runs under -race.
package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

type sessionEnrichDeps struct {
	svc     *sessionsprotocol.Service
	bus     events.EventBus
	tasks   tasks.TaskRegistry
	cleanup func()
}

func newSessionEnrichDeps(t *testing.T) *sessionEnrichDeps {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     128,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	pauses := pauseresume.New()
	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	enricher, err := sessionsprotocol.NewCounterEnricher(sessionsprotocol.CounterEnricherDeps{
		Bus:    bus,
		Tasks:  taskReg,
		Pauses: pauses,
	})
	if err != nil {
		t.Fatalf("NewCounterEnricher: %v", err)
	}
	projector, err := sessionsprotocol.NewListerProjector(reg, sessionsprotocol.WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Open one session under each of two tenants.
	openSession(t, reg, identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"})
	openSession(t, reg, identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"})

	return &sessionEnrichDeps{
		svc:   svc,
		bus:   bus,
		tasks: taskReg,
		cleanup: func() {
			_ = reg.CloseRegistry(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func openSession(t *testing.T, reg *sessions.Registry, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("open session %q: %v", id.SessionID, err)
	}
}

func publishSessionCost(t *testing.T, bus events.EventBus, id identity.Identity, dollars float64, tokens int) {
	t.Helper()
	if err := bus.Publish(context.Background(), events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: time.Now(),
		Payload: llm.CostRecordedPayload{
			Identity: identity.Quadruple{Identity: id},
			Model:    "test-model",
			Cost:     llm.Cost{TotalCost: dollars, Currency: "USD"},
			Usage:    llm.Usage{TotalTokens: tokens},
		},
	}); err != nil {
		t.Fatalf("publish cost: %v", err)
	}
}

func spawnFailedTask(t *testing.T, reg tasks.TaskRegistry, id identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        tasks.KindBackground,
		Description: "integration failing task",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "boom", Message: "failed"}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
}

func aIdentity() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "tA", User: "uA", Session: "sA"}
}
func bIdentity() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "tB", User: "uB", Session: "sB"}
}

func TestE2E_SessionEnrichment_PopulatedCounterReachesWire(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	publishSessionCost(t, deps.bus, idA, 3.00, 1200) // 300c
	publishSessionCost(t, deps.bus, idA, 1.50, 600)  // 150c
	spawnFailedTask(t, deps.tasks, idA)

	resp, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{Identity: aIdentity()}, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("A list returned %d rows, want 1", len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.TotalCostCents != 450 {
		t.Errorf("TotalCostCents = %d, want 450 (3.00 + 1.50) — the enricher must sum real cost", row.TotalCostCents)
	}
	if row.TotalTokens != 1800 {
		t.Errorf("TotalTokens = %d, want 1800", row.TotalTokens)
	}
	if !row.HasFailedTask {
		t.Error("HasFailedTask = false, want true — the failed task must reach the wire")
	}
	if row.TasksCount != 1 {
		t.Errorf("TasksCount = %d, want 1", row.TasksCount)
	}

	// "Reaches the wire": the marshalled row carries the populated counter.
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if !strings.Contains(string(raw), `"total_cost_cents":450`) {
		t.Errorf("wire row missing populated total_cost_cents: %s", raw)
	}

	// cost_above_cents narrows truthfully.
	above := int64(400)
	narrowed, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: aIdentity(),
		Filter:   prototypes.SessionFilter{CostAboveCents: &above},
	}, false)
	if err != nil {
		t.Fatalf("List cost_above: %v", err)
	}
	if len(narrowed.Rows) != 1 {
		t.Fatalf("cost_above=400 returned %d rows, want 1 (sA at 450)", len(narrowed.Rows))
	}
}

func TestE2E_SessionEnrichment_CrossSessionIsolation(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}
	// Distinct cost on each session — each row must reflect ONLY its own.
	publishSessionCost(t, deps.bus, idA, 9.99, 9000) // 999c on A
	publishSessionCost(t, deps.bus, idB, 1.00, 100)  // 100c on B

	respA, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{Identity: aIdentity()}, false)
	if err != nil {
		t.Fatalf("List A: %v", err)
	}
	respB, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{Identity: bIdentity()}, false)
	if err != nil {
		t.Fatalf("List B: %v", err)
	}
	if len(respA.Rows) != 1 || len(respB.Rows) != 1 {
		t.Fatalf("A/B list returned %d/%d rows, want 1/1", len(respA.Rows), len(respB.Rows))
	}
	// Each session sees ONLY its own cost — no cross-session bleed
	// (CLAUDE.md §6). If A's 999c leaked into B (or vice versa) these fail.
	if respA.Rows[0].TotalCostCents != 999 {
		t.Errorf("session A TotalCostCents = %d, want 999 (its own only)", respA.Rows[0].TotalCostCents)
	}
	if respB.Rows[0].TotalCostCents != 100 {
		t.Errorf("session B TotalCostCents = %d, want 100 (its own only) — A's 999c must not bleed across the isolation boundary", respB.Rows[0].TotalCostCents)
	}
}

func TestE2E_SessionEnrichment_AgentIDsFilter_FailsLoud(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	_, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: aIdentity(),
		Filter:   prototypes.SessionFilter{AgentIDs: []string{"agent-x"}},
	}, false)
	if err == nil {
		t.Fatal("filter.agent_ids over an unpopulated binding must fail loud (D-309 WARN-4), got nil error")
	}
	// A plain query still succeeds (never a whole-query reject).
	if _, qerr := deps.svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: aIdentity(),
		Filter:   prototypes.SessionFilter{Query: "sA"},
	}, false); qerr != nil {
		t.Fatalf("plain query must still succeed (WARN-4): %v", qerr)
	}
}

func TestE2E_SessionEnrichment_ConcurrentList_NoCrossTalk(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}
	publishSessionCost(t, deps.bus, idA, 2.00, 200) // 200c
	publishSessionCost(t, deps.bus, idB, 7.00, 700) // 700c

	const N = 40
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan string, N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			ident := aIdentity()
			wantTenant := "tA"
			wantCost := int64(200)
			if n%2 == 1 {
				ident = bIdentity()
				wantTenant = "tB"
				wantCost = 700
			}
			resp, err := deps.svc.List(context.Background(), prototypes.SessionsListRequest{Identity: ident}, false)
			if err != nil {
				errCh <- "list error: " + err.Error()
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != wantTenant {
					errCh <- "tenant bleed: got " + r.TenantID + " want " + wantTenant
					return
				}
				if r.TotalCostCents != wantCost {
					errCh <- "cost bleed on " + wantTenant
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}
