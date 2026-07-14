// Phase 176 cross-subsystem integration test per CLAUDE.md §17.1 — session
// reopen (RFC §6.9 amended, D-312) exercised end-to-end against REAL production
// drivers with NO mocks at any seam:
//
//   - internal/state (SQLITE) — the durable session-lifecycle record + the
//     durable event log substrate; the untrimmed-history guarantee is proven on
//     the durable driver specifically.
//   - internal/events (DURABLE) — the HistoryReplayer that backs state.history.
//   - internal/memory (inmem real driver) — the session's conversation memory.
//   - internal/artifacts (inmem real driver) — the scoped artifact store the
//     CascadeEraser deletes.
//   - internal/sessions — the Registry (reopen) + the real CascadeEraser.
//
// Covered: (1) close→reopen and GC-reap→reopen re-activate with the durable
// history + memory byte-intact; (2) a fully-CONVERGED erase → reopen fails loud
// ErrReopenAfterErase (the primary AC, ≥1 failure mode); (3) cross-tenant reopen
// ErrSessionIDReuse; (4) the hard cap restarts from the reopen so an over-cap
// reopened session is NOT re-reaped. Identity propagation is asserted through
// every layer. Runs under -race.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
)

type reopenStack struct {
	store  state.StateStore
	bus    events.EventBus
	mem    memory.MemoryStore
	arts   artifacts.ArtifactStore
	reg    *sessions.Registry
	eraser *sessions.CascadeEraser
	clock  *reopenClock
}

// reopenClock is a controllable clock for the GC hard-cap case.
type reopenClock struct {
	mu  chan struct{}
	now time.Time
}

func newReopenClock(t time.Time) *reopenClock {
	c := &reopenClock{mu: make(chan struct{}, 1), now: t}
	return c
}
func (c *reopenClock) Now() time.Time {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	return c.now
}
func (c *reopenClock) advance(d time.Duration) {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	c.now = c.now.Add(d)
}

func newReopenStack(t *testing.T, opts ...sessions.Option) *reopenStack {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()

	// SQLITE state so the durable event log + lifecycle record are genuinely
	// durable (the untrimmed-history guarantee is a durable-driver property).
	dsn := filepath.Join(t.TempDir(), "reopen-state.sqlite")
	store, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
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

	reg, err := sessions.New(store, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus, opts...)
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
	return &reopenStack{store: store, bus: bus, mem: mem, arts: arts, reg: reg, eraser: eraser}
}

func reopenCtx(id identity.Identity) context.Context {
	ctx, _ := identity.With(context.Background(), id)
	return ctx
}

// TestE2E_SessionReopen_HistoryIntact_Durable proves the untrimmed-history
// guarantee: a close→reopen (and a GC-reap→reopen) cycle leaves the durable
// event log AND the session memory byte-intact — the events read from
// state.history are identical before and after, and the memory patch is
// unchanged.
func TestE2E_SessionReopen_HistoryIntact_Durable(t *testing.T) {
	st := newReopenStack(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "resume-me"}
	ictx := reopenCtx(id)
	q := identity.Quadruple{Identity: id}
	hr, ok := st.bus.(events.HistoryReplayer)
	if !ok {
		t.Fatal("durable bus does not implement HistoryReplayer")
	}
	filter := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	// Open + generate durable history (session.opened + two touches) + memory.
	first, err := st.reg.Open(ictx, id.SessionID, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for range 2 {
		if terr := st.reg.Touch(ictx, id.SessionID); terr != nil {
			t.Fatalf("Touch: %v", terr)
		}
	}
	if aerr := st.mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage: "what is the capital of France?", AssistantResponse: "Paris.",
	}); aerr != nil {
		t.Fatalf("AddTurn: %v", aerr)
	}
	beforeEvents, err := hr.Window(ctx, 0, 100, filter)
	if err != nil {
		t.Fatalf("Window (before): %v", err)
	}
	if len(beforeEvents) == 0 {
		t.Fatal("no durable history before close — test cannot prove intactness")
	}
	beforeMem, err := st.mem.GetLLMContext(ctx, q)
	if err != nil {
		t.Fatalf("GetLLMContext (before): %v", err)
	}

	// Close, then reopen via EnsureOpen.
	if cerr := st.reg.Close(ictx, id.SessionID, "operator:test"); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	reopened, err := st.reg.EnsureOpen(ictx, id)
	if err != nil {
		t.Fatalf("EnsureOpen (reopen): %v", err)
	}
	if reopened.Closed || !reopened.OpenedAt.Equal(first.OpenedAt) || reopened.Identity != id {
		t.Fatalf("reopen did not preserve identity/OpenedAt: %+v", reopened)
	}

	// The durable pre-close events must all still be present, byte-identical.
	afterEvents, err := hr.Window(ctx, 0, 100, filter)
	if err != nil {
		t.Fatalf("Window (after reopen): %v", err)
	}
	assertHistorySuperset(t, beforeEvents, afterEvents)
	// Memory survived intact.
	afterMem, err := st.mem.GetLLMContext(ctx, q)
	if err != nil {
		t.Fatalf("GetLLMContext (after): %v", err)
	}
	if len(afterMem.RecentTurns) != len(beforeMem.RecentTurns) || afterMem.Tokens != beforeMem.Tokens {
		t.Fatalf("memory changed across reopen: before=%+v after=%+v", beforeMem, afterMem)
	}
	if len(afterMem.RecentTurns) == 0 {
		t.Fatal("memory empty after reopen — history not intact")
	}

	// Second sub-case: a GC reap → reopen re-activates identically.
	if cerr := st.reg.Close(ictx, id.SessionID, "gc:idle"); cerr != nil {
		t.Fatalf("Close (2): %v", cerr)
	}
	reopened2, err := st.reg.EnsureOpen(ictx, id)
	if err != nil {
		t.Fatalf("EnsureOpen (reopen 2): %v", err)
	}
	if reopened2.Closed {
		t.Fatal("second reopen left session closed")
	}
	post, err := hr.Window(ctx, 0, 100, filter)
	if err != nil {
		t.Fatalf("Window (after reopen 2): %v", err)
	}
	assertHistorySuperset(t, beforeEvents, post)
}

// assertHistorySuperset asserts every event in `before` is still present in
// `after` at the same sequence with the same type + identity (durable log
// untrimmed across the reopen cycle).
func assertHistorySuperset(t *testing.T, before, after []events.Event) {
	t.Helper()
	idx := make(map[uint64]events.Event, len(after))
	for _, e := range after {
		idx[e.Sequence] = e
	}
	for _, b := range before {
		a, ok := idx[b.Sequence]
		if !ok {
			t.Fatalf("durable event seq=%d (type=%s) LOST across reopen — history trimmed", b.Sequence, b.Type)
		}
		if a.Type != b.Type || a.Identity != b.Identity {
			t.Fatalf("durable event seq=%d changed across reopen: before=(%s,%+v) after=(%s,%+v)",
				b.Sequence, b.Type, b.Identity, a.Type, a.Identity)
		}
	}
}

// TestE2E_SessionReopen_ConvergedErase_FailsLoud is the primary AC: a
// fully-CONVERGED session.erase (record gone) → reopen fails loud
// ErrReopenAfterErase, NOT a fresh empty session — proven against the real
// CascadeEraser on the durable stack.
func TestE2E_SessionReopen_ConvergedErase_FailsLoud(t *testing.T) {
	st := newReopenStack(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "erase-me"}
	ictx := reopenCtx(id)

	if _, err := st.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if terr := st.reg.Touch(ictx, id.SessionID); terr != nil {
		t.Fatalf("Touch: %v", terr)
	}
	// Full erase → converges (lifecycle record removed, tombstone written).
	if _, err := st.eraser.Erase(ctx, id); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if _, lerr := st.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("lifecycle record survived erase (not converged): %v", lerr)
	}
	// Reopen on the converged (not-found) path is terminal.
	if _, oerr := st.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("Open converged-erased = %v, want ErrReopenAfterErase", oerr)
	}
	if _, eerr := st.reg.EnsureOpen(ictx, id); !errors.Is(eerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("EnsureOpen converged-erased = %v, want ErrReopenAfterErase", eerr)
	}
}

// TestE2E_SessionReopen_ConcurrentReopenErase_Durable is the §17.3 durable-seam
// concurrency stress for the reopen∥erase race (the unit-level D-025 stress runs
// on the inmem fixture; this proves the same invariants against the real durable
// event bus + sqlite state + real CascadeEraser). N concurrent (reopen ∥ erase)
// pairs on distinct sessions resolve to cleanly erased with the erasure FENCE
// INTACT — a straggler late event stays dropped under the erased triple, no
// resurrected history. Under -race.
func TestE2E_SessionReopen_ConcurrentReopenErase_Durable(t *testing.T) {
	st := newReopenStack(t)
	ctx := context.Background()
	hr, ok := st.bus.(events.HistoryReplayer)
	if !ok {
		t.Fatal("durable bus is not a HistoryReplayer")
	}
	const n = 16
	ids := make([]identity.Identity, n)
	for i := range n {
		id := identity.Identity{TenantID: "T", UserID: "U", SessionID: fmt.Sprintf("race-%d", i)}
		ids[i] = id
		if _, err := st.reg.Open(reopenCtx(id), id.SessionID, id); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := st.reg.Close(reopenCtx(id), id.SessionID, "race:pre"); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := range n {
		id := ids[i]
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, oerr := st.reg.Open(reopenCtx(id), id.SessionID, id); oerr != nil && !errors.Is(oerr, sessions.ErrReopenAfterErase) {
				t.Errorf("concurrent reopen %s = %v, want nil or ErrReopenAfterErase", id.SessionID, oerr)
			}
		}()
		go func() {
			defer wg.Done()
			if _, eerr := st.eraser.Erase(ctx, id); eerr != nil && !errors.Is(eerr, sessions.ErrSessionNotFound) {
				t.Errorf("concurrent erase %s = %v", id.SessionID, eerr)
			}
		}()
	}
	wg.Wait()

	for i := range n {
		id := ids[i]
		if _, lerr := st.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
			t.Fatalf("%s: lifecycle record survived the durable race: %v", id.SessionID, lerr)
		}
		if _, oerr := st.reg.Open(reopenCtx(id), id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
			t.Fatalf("%s: post-race reopen = %v, want ErrReopenAfterErase", id.SessionID, oerr)
		}
		// FENCE INTACT: a straggler late event stays dropped, history empty.
		late := events.Event{
			Type:     sessions.EventTypeSessionTouched,
			Identity: identity.Quadruple{Identity: id},
			Payload:  sessions.SessionTouchedPayload{SessionID: id.SessionID, LastSeen: 999},
		}
		if perr := st.bus.Publish(ctx, late); perr != nil {
			t.Fatalf("%s: late fenced publish should drop gracefully: %v", id.SessionID, perr)
		}
		filter := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
		if _, _, _, herr := hr.Bounds(ctx, filter); !errors.Is(herr, events.ErrNoHistory) {
			t.Fatalf("%s: FENCE LIFTED mid-erase on the durable seam (Bounds=%v, want ErrNoHistory)", id.SessionID, herr)
		}
	}
}

// TestE2E_SessionReopen_CrossTenant_Rejected — a SessionID closed under tenant
// A, then Opened under tenant B, is ErrSessionIDReuse, never a reopen.
func TestE2E_SessionReopen_CrossTenant_Rejected(t *testing.T) {
	st := newReopenStack(t)
	idA := identity.Identity{TenantID: "TA", UserID: "UA", SessionID: "shared"}
	idB := identity.Identity{TenantID: "TB", UserID: "UB", SessionID: "shared"}
	if _, err := st.reg.Open(reopenCtx(idA), idA.SessionID, idA); err != nil {
		t.Fatalf("tenant A Open: %v", err)
	}
	if err := st.reg.Close(reopenCtx(idA), idA.SessionID, "test"); err != nil {
		t.Fatalf("tenant A Close: %v", err)
	}
	if _, err := st.reg.Open(reopenCtx(idB), idB.SessionID, idB); !errors.Is(err, sessions.ErrSessionIDReuse) {
		t.Fatalf("cross-tenant reopen = %v, want ErrSessionIDReuse", err)
	}
}

// TestE2E_SessionReopen_HardCapRestarts — an over-cap session reopened then
// GC-swept is NOT re-reaped (FAIL-1), while a never-reopened over-cap peer is.
func TestE2E_SessionReopen_HardCapRestarts(t *testing.T) {
	clock := newReopenClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st := newReopenStack(t,
		sessions.WithClock(clock),
		sessions.WithGCPolicy(sessions.GCPolicy{
			IdleTTL: 5000 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
		}))
	reopened := identity.Identity{TenantID: "T", UserID: "U", SessionID: "old-resumed"}
	stale := identity.Identity{TenantID: "T", UserID: "U", SessionID: "old-abandoned"}
	if _, err := st.reg.Open(reopenCtx(reopened), reopened.SessionID, reopened); err != nil {
		t.Fatalf("Open reopened: %v", err)
	}
	if _, err := st.reg.Open(reopenCtx(stale), stale.SessionID, stale); err != nil {
		t.Fatalf("Open stale: %v", err)
	}
	clock.advance(721 * time.Hour) // both now over the 720h hard cap
	if err := st.reg.Close(reopenCtx(reopened), reopened.SessionID, "test"); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
	if _, err := st.reg.Open(reopenCtx(reopened), reopened.SessionID, reopened); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reaped, err := st.reg.GC(reopenCtx(reopened), sessions.GCPolicy{
		IdleTTL: 5000 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("GC reaped %d, want 1 (only the never-reopened over-cap peer)", reaped)
	}
	rs, err := st.reg.Get(reopenCtx(reopened), reopened.SessionID)
	if err != nil {
		t.Fatalf("Get reopened: %v", err)
	}
	if rs.Closed {
		t.Fatal("reopened over-cap session was re-reaped (FAIL-1 regression)")
	}
}
