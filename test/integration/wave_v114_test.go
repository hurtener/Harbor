// Wave v1.14 boundary regression gate (§17.5 / §17.7 step 5).
//
// The v1.14 wave shipped four surfaces that each read the SAME sessions +
// events substrate, but the per-phase tests exercise them in isolation:
//
//   - Phase 174 (HA-22 / D-309) — sessions.list counter ENRICHMENT
//     (per-session cost / tokens / tasks folded from the event bus).
//   - Phase 175 (HA-23 / D-310) — the observed retention HORIZON on
//     runtime.health (the sessions surface horizon == the session's oldest
//     OpenedAt, tenant-scoped for an ordinary caller).
//   - Phase 176 (D-312) — session REOPEN (close→reopen re-activates a
//     session without trimming its history) + the converged-erase fence
//     (ErrReopenAfterErase).
//   - Phase 178 (HA-24 / D-314) — the tools production ANNOTATOR, whose
//     per-tool metrics window over the same event bus.
//
// None of the per-phase tests COMPOSE these against ONE shared substrate.
// This gate does: it stands up a single stack (real inmem StateStore, real
// inmem EventBus, real inprocess TaskRegistry, real sessions Registry +
// CascadeEraser, real tools Catalog + Annotator, and the real sessions /
// tools protocol Services + the runtime PostureSurface) and proves the
// four surfaces tell ONE consistent story about the same session:
//
//  1. A session's published cost / tool events reach the enrichment
//     counters AND the tools metrics; its OpenedAt is the retention horizon.
//  2. A close→reopen cycle (Phase 176) leaves ALL THREE reads byte-stable —
//     reopen preserves the substrate the other three surfaces observe.
//  3. Cross-tenant isolation holds across the whole composed surface — one
//     session's counters / horizon / metrics never bleed into another's.
//  4. A converged erase cascades across the surfaces AND fences reopen
//     loud (the ≥1 failure mode).
//  5. Under N≥16 concurrent goroutines mixing reopen ∥ enrichment ∥
//     retention ∥ tools reads against the shared stack, no cross-talk and
//     the invariants hold. -race is the gate.
//
// Real drivers at every seam (§17.3): no mocks at the boundary.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/annotate"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// waveV114Stack is the ONE shared composed stack. Every surface reads the
// same reg (sessions Registry, used as both the reopen target AND the
// SessionLister behind enrichment + retention) and the same bus (the
// event substrate behind enrichment cost, tools metrics, and the events
// retention reporter). Immutable after construction; safe for concurrent
// reuse by the goroutines in the concurrency leg.
type waveV114Stack struct {
	store    state.StateStore
	bus      events.EventBus
	reg      *sessions.Registry
	eraser   *sessions.CascadeEraser
	tasks    tasks.TaskRegistry
	sessSvc  *sessionsprotocol.Service
	toolsSvc *toolsprotocol.Service
	posture  *protocol.PostureSurface
}

const waveV114GHTool = "gh_tool"

func newWaveV114Stack(t *testing.T) *waveV114Stack {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()

	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open(inmem): %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     128,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, red)
	if err != nil {
		t.Fatalf("events.Open(inmem): %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })

	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 4000,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctx) })

	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctx) })

	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store: store, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(ctx) })

	reg, err := sessions.New(store, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(ctx) })

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: store, Memory: mem, Artifacts: arts, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// Enrichment (Phase 174): CounterEnricher + ListerProjector + Service,
	// all over the SHARED bus / reg / taskReg.
	enricher, err := sessionsprotocol.NewCounterEnricher(sessionsprotocol.CounterEnricherDeps{
		Bus: bus, Tasks: taskReg, Pauses: pauseresume.New(),
	})
	if err != nil {
		t.Fatalf("NewCounterEnricher: %v", err)
	}
	projector, err := sessionsprotocol.NewListerProjector(reg, sessionsprotocol.WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus), sessionsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("sessions NewService: %v", err)
	}

	// Tools annotator (Phase 178): one OAuth-source-tagged MCP tool whose
	// metrics window over the SHARED bus.
	toolsSvc := newToolsServiceV114(t, store, bus, red)

	// Retention posture (Phase 175): reuse the retention_horizon_fleet_test
	// surface builder over the SHARED bus / taskReg / reg (SessionLister).
	posture := retentionSurface(t, &retentionFleetStack{
		bus: bus, taskReg: taskReg, lister: reg,
	}, red)

	return &waveV114Stack{
		store: store, bus: bus, reg: reg, eraser: eraser, tasks: taskReg,
		sessSvc: sessSvc, toolsSvc: toolsSvc, posture: posture,
	}
}

// --- per-session driving helpers (each reads ONE surface for ONE id) -------

func (s *waveV114Stack) open(t *testing.T, id identity.Identity) time.Time {
	t.Helper()
	sess, err := s.reg.Open(reopenCtx(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("Open %s: %v", id.SessionID, err)
	}
	return sess.OpenedAt
}

func (s *waveV114Stack) publishCost(t *testing.T, id identity.Identity, dollars float64, tokens int) {
	t.Helper()
	if err := s.bus.Publish(reopenCtx(id), events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: time.Now().UTC(),
		Payload: llm.CostRecordedPayload{
			Identity: identity.Quadruple{Identity: id},
			Model:    "test-model",
			Cost:     llm.Cost{TotalCost: dollars, Currency: "USD"},
			Usage:    llm.Usage{TotalTokens: tokens},
		},
	}); err != nil {
		t.Fatalf("publish cost %s: %v", id.SessionID, err)
	}
}

func (s *waveV114Stack) publishToolDone(t *testing.T, id identity.Identity, tool string) {
	t.Helper()
	if err := s.bus.Publish(reopenCtx(id), events.Event{
		Type:       tools.EventTypeToolCompleted,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: time.Now().UTC().Add(-time.Minute),
		Payload:    tools.ToolCompletedPayload{ToolName: tool},
	}); err != nil {
		t.Fatalf("publish tool-completed %s: %v", id.SessionID, err)
	}
}

// enrichRow returns the single sessions.list row for id's (tenant, user),
// asserting exactly one row and no foreign-tenant bleed.
func (s *waveV114Stack) enrichRow(t *testing.T, id identity.Identity) prototypes.SessionRow {
	t.Helper()
	resp, err := s.sessSvc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("sessions.List %s: %v", id.SessionID, err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("sessions.List %s returned %d rows, want 1", id.SessionID, len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.TenantID != id.TenantID {
		t.Fatalf("sessions.List %s leaked foreign tenant %q", id.SessionID, row.TenantID)
	}
	return row
}

// sessionsHorizon returns the (tenant-scoped) sessions retention horizon
// observed on runtime.health for id.
func (s *waveV114Stack) sessionsHorizon(t *testing.T, id identity.Identity) prototypes.RetentionHorizon {
	t.Helper()
	m := healthRetention(t, s.posture, fleetIdentityCtx(t, id), id)
	h, ok := m["sessions"]
	if !ok {
		t.Fatalf("sessions retention horizon missing for %s: %+v", id.SessionID, m)
	}
	return h
}

func (s *waveV114Stack) toolInvocations(t *testing.T, id identity.Identity) int {
	t.Helper()
	m, err := s.toolsSvc.Metrics(context.Background(), prototypes.ToolMetricsRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		ID:       waveV114GHTool, Window: prototypes.ToolWindow1h,
	})
	if err != nil {
		t.Fatalf("tools.Metrics %s: %v", id.SessionID, err)
	}
	return int(m.Invocations)
}

// TestE2E_WaveV114_ComposedSurfaceStableAcrossReopen is the wave's
// cross-phase compose: a session's published substrate reaches all three
// read surfaces (enrichment cost/tokens, retention OpenedAt horizon, tools
// metrics), and a close→reopen cycle (Phase 176) leaves every read
// byte-stable — reopen does not disturb the substrate the other surfaces
// observe. A second session under a distinct tenant proves isolation.
func TestE2E_WaveV114_ComposedSurfaceStableAcrossReopen(t *testing.T) {
	s := newWaveV114Stack(t)
	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}

	openedA := s.open(t, idA)
	openedB := s.open(t, idB)

	// Session A substrate: $4.50 over two cost events, 1800 tokens, 3 tool runs.
	s.publishCost(t, idA, 3.00, 1200)
	s.publishCost(t, idA, 1.50, 600)
	for range 3 {
		s.publishToolDone(t, idA, waveV114GHTool)
	}
	// Session B substrate (distinct, smaller) — must never bleed into A.
	s.publishCost(t, idB, 1.00, 100)
	s.publishToolDone(t, idB, waveV114GHTool)

	assertA := func(phase string) {
		row := s.enrichRow(t, idA)
		if row.TotalCostCents != 450 || row.TotalTokens != 1800 {
			t.Fatalf("[%s] A enrichment = %dc / %d tok, want 450 / 1800", phase, row.TotalCostCents, row.TotalTokens)
		}
		h := s.sessionsHorizon(t, idA)
		if h.Scope != prototypes.RetentionScopeTenant {
			t.Fatalf("[%s] A sessions horizon scope = %q, want tenant", phase, h.Scope)
		}
		if h.OldestRetainedAt == nil || !h.OldestRetainedAt.Equal(openedA) {
			t.Fatalf("[%s] A retention horizon = %v, want session OpenedAt %v", phase, h.OldestRetainedAt, openedA)
		}
		if got := s.toolInvocations(t, idA); got != 3 {
			t.Fatalf("[%s] A tool invocations = %d, want 3", phase, got)
		}
	}

	// Baseline: all three surfaces agree on A's substrate.
	assertA("baseline")

	// Close → reopen A (Phase 176). OpenedAt preserved; history untrimmed.
	if err := s.reg.Close(reopenCtx(idA), idA.SessionID, "compose:test"); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	reopened, err := s.reg.EnsureOpen(reopenCtx(idA), idA)
	if err != nil {
		t.Fatalf("EnsureOpen A: %v", err)
	}
	if reopened.Closed || !reopened.OpenedAt.Equal(openedA) {
		t.Fatalf("reopen A did not preserve lifecycle: closed=%v OpenedAt=%v want %v",
			reopened.Closed, reopened.OpenedAt, openedA)
	}

	// After reopen: every surface reads IDENTICALLY — reopen trimmed nothing.
	assertA("post-reopen")

	// Isolation: B reflects ONLY its own substrate across all three surfaces.
	rowB := s.enrichRow(t, idB)
	if rowB.TotalCostCents != 100 || rowB.TotalTokens != 100 {
		t.Fatalf("B enrichment = %dc / %d tok, want 100 / 100 (A's 450c must not bleed)", rowB.TotalCostCents, rowB.TotalTokens)
	}
	hB := s.sessionsHorizon(t, idB)
	if hB.OldestRetainedAt == nil || !hB.OldestRetainedAt.Equal(openedB) {
		t.Fatalf("B retention horizon = %v, want B OpenedAt %v (A's horizon must not bleed)", hB.OldestRetainedAt, openedB)
	}
	if got := s.toolInvocations(t, idB); got != 1 {
		t.Fatalf("B tool invocations = %d, want 1 (A's 3 must not bleed)", got)
	}
}

// TestE2E_WaveV114_ConvergedEraseCascadesAndFencesReopen is the ≥1 failure
// mode: a converged CascadeEraser.Erase removes the session across the
// composed surface (enrichment stops listing it) AND fences reopen loud
// with ErrReopenAfterErase — never a silently-minted fresh empty session.
func TestE2E_WaveV114_ConvergedEraseCascadesAndFencesReopen(t *testing.T) {
	s := newWaveV114Stack(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "tE", UserID: "uE", SessionID: "erase-me"}

	s.open(t, id)
	s.publishCost(t, id, 2.00, 400)
	// Before erase the enrichment surface lists the session.
	if row := s.enrichRow(t, id); row.TotalCostCents != 200 {
		t.Fatalf("pre-erase enrichment = %dc, want 200", row.TotalCostCents)
	}

	if _, err := s.eraser.Erase(ctx, id); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	// Lifecycle record converged-gone.
	if _, lerr := s.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("lifecycle survived erase (not converged): %v", lerr)
	}
	// Enrichment surface no longer lists the erased session.
	resp, err := s.sessSvc.List(ctx, prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
	}, false)
	if err != nil {
		t.Fatalf("post-erase List: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("post-erase enrichment still lists %d rows, want 0 (cascade incomplete)", len(resp.Rows))
	}
	// Reopen is fenced loud on both entry points.
	if _, oerr := s.reg.Open(reopenCtx(id), id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("Open converged-erased = %v, want ErrReopenAfterErase", oerr)
	}
	if _, eerr := s.reg.EnsureOpen(reopenCtx(id), id); !errors.Is(eerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("EnsureOpen converged-erased = %v, want ErrReopenAfterErase", eerr)
	}
}

// TestE2E_WaveV114_ComposeConcurrency runs N=16 concurrent goroutines
// (4 sessions × {reopener, enrichment reader, retention reader, tools
// reader}) against the ONE shared stack under -race. reopen ∥ enrichment ∥
// retention ∥ tools reads all touch the shared sessions + events surface
// concurrently; every read must reflect ONLY its own session — no
// cross-talk under contention.
func TestE2E_WaveV114_ComposeConcurrency(t *testing.T) {
	s := newWaveV114Stack(t)

	const sessionsN = 4
	type expect struct {
		id       identity.Identity
		opened   time.Time
		cents    int64
		tokens   int64
		toolRuns int
	}
	exp := make([]expect, sessionsN)
	for i := range sessionsN {
		id := identity.Identity{
			TenantID:  "t-cc" + string(rune('A'+i)),
			UserID:    "u-cc",
			SessionID: "s-cc" + string(rune('A'+i)),
		}
		opened := s.open(t, id)
		// Distinct per-session substrate so any bleed is detectable.
		dollars := float64(i + 1) // $1, $2, $3, $4
		tokens := (i + 1) * 100
		s.publishCost(t, id, dollars, tokens)
		runs := i + 2 // 2..5 tool runs
		for range runs {
			s.publishToolDone(t, id, waveV114GHTool)
		}
		exp[i] = expect{id: id, opened: opened, cents: int64((i + 1) * 100), tokens: int64(tokens), toolRuns: runs}
	}

	const iterations = 5
	var wg sync.WaitGroup
	errs := make(chan string, sessionsN*4*iterations)

	for i := range sessionsN {
		e := exp[i]
		// Reopener: close→reopen its own session in a loop; OpenedAt must hold.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if err := s.reg.Close(reopenCtx(e.id), e.id.SessionID, "cc:test"); err != nil {
					errs <- "close " + e.id.SessionID + ": " + err.Error()
					return
				}
				st, err := s.reg.EnsureOpen(reopenCtx(e.id), e.id)
				if err != nil {
					errs <- "reopen " + e.id.SessionID + ": " + err.Error()
					return
				}
				if st.Closed || !st.OpenedAt.Equal(e.opened) {
					errs <- "reopen " + e.id.SessionID + " lost OpenedAt"
					return
				}
			}
		}()
		// Enrichment reader: cost/tokens must stay its own throughout.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				resp, err := s.sessSvc.List(context.Background(), prototypes.SessionsListRequest{
					Identity: prototypes.IdentityScope{Tenant: e.id.TenantID, User: e.id.UserID, Session: e.id.SessionID},
				}, false)
				if err != nil {
					errs <- "enrich " + e.id.SessionID + ": " + err.Error()
					return
				}
				for _, r := range resp.Rows {
					if r.TenantID != e.id.TenantID {
						errs <- "enrich tenant bleed on " + e.id.SessionID
						return
					}
					if r.TotalCostCents != e.cents || r.TotalTokens != e.tokens {
						errs <- "enrich cost bleed on " + e.id.SessionID
						return
					}
				}
			}
		}()
		// Retention reader: sessions horizon == its own OpenedAt throughout.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				h := s.sessionsHorizon(t, e.id)
				if h.OldestRetainedAt == nil || !h.OldestRetainedAt.Equal(e.opened) {
					errs <- "retention horizon bleed on " + e.id.SessionID
					return
				}
				if h.Scope == prototypes.RetentionScopeRuntime {
					errs <- "ordinary caller saw runtime-wide horizon on " + e.id.SessionID
					return
				}
			}
		}()
		// Tools reader: metrics reflect its own invocation count throughout.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if got := s.toolInvocations(t, e.id); got != e.toolRuns {
					errs <- "tools metrics bleed on " + e.id.SessionID
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal("compose concurrency failure: " + e)
	}
}

// --- local tools-protocol surface (Phase 178 annotator, minimal deps) ------

// newToolsServiceV114 builds the tools-protocol Service the compose drives:
// exactly one catalog tool + the production Annotator wired over the SHARED
// bus + state, with no OAuth ceremony (the metrics compose does not need a
// live binding — the tool-completed events on the shared bus are the whole
// substrate under test).
func newToolsServiceV114(t *testing.T, st state.StateStore, bus events.EventBus, red audit.Redactor) *toolsprotocol.Service {
	t.Helper()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: waveV114GHTool, Description: waveV114GHTool,
			Source: "gh", Transport: tools.TransportMCP,
			SideEffects: tools.SideEffectExternal, Loading: tools.LoadingAlways,
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
	}); err != nil {
		t.Fatalf("register %s: %v", waveV114GHTool, err)
	}
	policyStore, err := approval.NewStatePolicyStore(st)
	if err != nil {
		t.Fatalf("NewStatePolicyStore: %v", err)
	}
	annotator, err := annotate.NewAnnotator(annotate.Deps{
		Catalog: cat, Approval: policyStore, Events: bus, HeavyThresholdBytes: 32 * 1024,
	})
	if err != nil {
		t.Fatalf("NewAnnotator: %v", err)
	}
	proj, err := toolsprotocol.NewCatalogProjector(cat, toolsprotocol.WithAnnotator(annotator))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj, toolsprotocol.WithBus(bus), toolsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("tools NewService: %v", err)
	}
	return svc
}
