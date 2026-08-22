package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
)

// TestRegistry_Interleave_CloseVsSetTitle_ClosedSurvives pins the
// lost-update fix on the whole-record write family: a SetTitle racing a
// Close must NEVER re-persist Closed=false after Close returned nil.
// Before the serialized read-modify-write path (mutateSession), SetTitle
// loaded outside the registry lock and could save a stale Closed=false
// record AFTER Close's save — resurrecting the closed session as a
// GC-invisible zombie (Close had already dropped it from openSessions),
// violating the pinned reopen-after-close invariant. The race
// reproduced in ~half of pre-fix iterations; each iteration here races
// one Close against one SetTitle on a fresh session and asserts the
// terminal state.
func TestRegistry_Interleave_CloseVsSetTitle_ClosedSurvives(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)

	const iterations = 50
	for i := range iterations {
		id := ident("t1", "u1", fmt.Sprintf("s-interleave-%d", i))
		if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
			t.Fatalf("iter %d: Open: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := reg.Close(ctxFor(id), id.SessionID, "interleave"); err != nil {
				t.Errorf("iter %d: Close: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			// SetTitle on a CLOSED session is allowed, so BOTH orders
			// succeed — the assertion is about the terminal record, not
			// either call's error.
			if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "interleaved title"); err != nil {
				t.Errorf("iter %d: SetTitle: %v", i, err)
			}
		}()
		wg.Wait()

		// Closed survived: Close returned nil, so the persisted record
		// MUST carry Closed=true regardless of interleaving.
		snap, err := reg.Inspect(ctxFor(id), id.SessionID)
		if err != nil {
			t.Fatalf("iter %d: Inspect: %v", i, err)
		}
		if !snap.Closed {
			t.Fatalf("iter %d: session resurrected — Closed=false after Close returned nil (lost update)", i)
		}
		// The session never reappears open: Touch (open-only) is refused,
		// and the open-only listing does not show it.
		if err := reg.Touch(ctxFor(id), id.SessionID); !errors.Is(err, sessions.ErrSessionClosed) {
			t.Fatalf("iter %d: Touch on closed session = %v, want ErrSessionClosed", i, err)
		}
		snaps, err := reg.ListSnapshots(ctxFor(id), sessions.SessionListFilter{
			TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID},
			SessionIDs: []string{id.SessionID}, IncludeClosed: false,
		})
		if err != nil {
			t.Fatalf("iter %d: ListSnapshots: %v", i, err)
		}
		if len(snaps) != 0 {
			t.Fatalf("iter %d: closed session reappears in the open-only listing: %+v", i, snaps)
		}
	}
}

// TestRegistry_Interleave_EraseVsSetTitle_NoResurrection pins the
// re-persist-after-erasure fix: a SetTitle racing the erasure cascade's
// irreversible StateStore.DeleteScope must never save the
// session.lifecycle record back after the clear. The eraser's clear now
// runs through Registry.deleteScopeSerialized (the same r.mu the
// whole-record writers hold across load→save), so the writer either
// completes fully before the clear (its write is removed with
// everything else) or loads after it and observes ErrSessionNotFound.
// Each iteration races one Erase against one SetTitle on a fresh
// session and asserts the record is durably gone — not_found stays
// not_found.
func TestRegistry_Interleave_EraseVsSetTitle_NoResurrection(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()

	const iterations = 20
	for i := range iterations {
		id := ident("t1", "u1", fmt.Sprintf("s-erase-interleave-%d", i))
		ictx := ctxFor(id)
		if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
			t.Fatalf("iter %d: Open: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := f.eraser.Erase(ctx, id); err != nil {
				t.Errorf("iter %d: Erase: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			// Valid outcomes: nil (the rename fully completed before the
			// clear — its write is removed with everything else) or
			// ErrSessionNotFound (it loaded after the clear). Anything
			// else is a bug.
			err := f.reg.SetTitle(ctx, id.SessionID, id, "rename during erase")
			if err != nil && !errors.Is(err, sessions.ErrSessionNotFound) {
				t.Errorf("iter %d: SetTitle = %v, want nil or ErrSessionNotFound", i, err)
			}
		}()
		wg.Wait()

		// The record is durably gone — the rename can NOT have
		// re-persisted it after DeleteScope.
		if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("iter %d: session.lifecycle record re-persisted after erasure: err=%v", i, err)
		}
		// not_found stays not_found: a follow-up SetTitle is refused.
		if err := f.reg.SetTitle(ctx, id.SessionID, id, "post-erasure rename"); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("iter %d: post-erasure SetTitle = %v, want ErrSessionNotFound", i, err)
		}
		if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("iter %d: post-erasure SetTitle re-persisted the record: err=%v", i, err)
		}
	}
}

// catalogSaveGate wraps a real StateStore and, while armed, parks the FIRST
// `session.catalog` Save it sees until released — a deterministic interleave
// gate for the catalog lost-update. The gate disarms itself on capture, so
// every subsequent catalog save (e.g. a concurrent Open's add) passes
// through untouched. All other kinds always pass through.
type catalogSaveGate struct {
	state.StateStore
	mu       sync.Mutex
	armed    bool
	captured chan struct{} // closed once when the armed save parks
	release  chan struct{} // closed by the test to release the parked save
}

func newCatalogSaveGate(inner state.StateStore) *catalogSaveGate {
	return &catalogSaveGate{StateStore: inner}
}

// arm primes the gate: the NEXT `session.catalog` Save parks until release.
// Returns the captured/release channel pair for this arming.
func (g *catalogSaveGate) arm() (captured <-chan struct{}, release chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.captured = make(chan struct{})
	g.release = make(chan struct{})
	return g.captured, g.release
}

func (g *catalogSaveGate) Save(ctx context.Context, r state.StateRecord) error {
	if r.Kind == "session.catalog" {
		g.mu.Lock()
		if g.armed {
			g.armed = false
			captured, release := g.captured, g.release
			g.mu.Unlock()
			close(captured)
			<-release
		} else {
			g.mu.Unlock()
		}
	}
	return g.StateStore.Save(ctx, r)
}

func (g *catalogSaveGate) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	return g.StateStore.SaveIf(ctx, expectations, next)
}

// TestRegistry_Interleave_EraseVsOpen_NoResurrection pins the catalog
// lost-update fix DETERMINISTICALLY: the per-(tenant, user) discovery
// catalog's read-modify-write must serialize an Open's add against an
// erasure's remove. The lost-update window (between the eraser's catalog
// Load and its Save) is nanoseconds on the inmem driver, so a bare
// go-Erase/go-Open race virtually never lands in it — a stochastic
// interleave passes even against the UN-serialized pre-fix code and would
// rubber-stamp a future revert. Instead, a gated StateStore parks the
// eraser's catalog Save after its Load has returned, drives Open(C) into
// exactly that window, then releases:
//
//   - PRE-fix (catalog RMW outside r.mu): the eraser parks WITHOUT holding
//     r.mu, so Open(C) completes its add ({keep, erase, C}); the released
//     eraser then saves its stale difference ({keep}) — C's catalog entry
//     is lost-updated away, and a fresh registry never re-discovers C.
//     FAILS in one iteration (A/B-verified against the pre-fix shape).
//   - POST-fix (mutateCatalog under r.mu; the remove folded into
//     clearErased's critical section): the eraser parks while HOLDING
//     r.mu, so Open(C) blocks on the registry lock instead of racing —
//     the bounded wait expires, the gate is released either way (the
//     deadlock-avoidance branch), the eraser completes, and Open(C)'s add
//     then lands on the post-remove catalog. A fresh registry re-discovers
//     keep AND C; the erased session stays gone.
func TestRegistry_Interleave_EraseVsOpen_NoResurrection(t *testing.T) {
	ctx := context.Background()

	// Build the erasure wiring over the GATED store (real drivers on every
	// other seam — the gate is a pass-through except for the one armed save).
	red := auditpatterns.New()
	inner, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(ctx) })
	gate := newCatalogSaveGate(inner)

	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		LegacyWritersDrained:     true,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, red, gate)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: gate, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctx) })
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctx) })
	skillStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = skillStore.Close(ctx) })
	reg, err := sessions.New(gate, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(ctx) })
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: gate, Memory: mem, Artifacts: arts, Skills: skillStore, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	const tenant, user = "t1", "u1"
	keep := ident(tenant, user, "s-keep")
	erase := ident(tenant, user, "s-erase")
	open := ident(tenant, user, "s-open")
	if _, err := reg.Open(ctxFor(keep), keep.SessionID, keep); err != nil {
		t.Fatalf("Open keep: %v", err)
	}
	if _, err := reg.Open(ctxFor(erase), erase.SessionID, erase); err != nil {
		t.Fatalf("Open erase: %v", err)
	}

	// Arm the gate, then start the erasure: its clearErased loads the catalog
	// {keep, erase}, computes the difference, and its Save parks on the gate.
	captured, release := gate.arm()
	eraseDone := make(chan error, 1)
	go func() { _, eerr := eraser.Erase(ctx, erase); eraseDone <- eerr }()
	select {
	case <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the eraser's catalog save to reach the gate")
	}

	// The eraser's stale catalog write is parked. Drive Open(C) into the
	// window. Post-fix, Open blocks on r.mu (the eraser holds it across the
	// parked save), so wait bounded and release the gate EITHER WAY — the
	// deadlock-avoidance branch; pre-fix, Open completes inside the window.
	openDone := make(chan error, 1)
	go func() { _, oerr := reg.Open(ctxFor(open), open.SessionID, open); openDone <- oerr }()
	var openErr error
	openCompletedInWindow := false
	select {
	case openErr = <-openDone:
		openCompletedInWindow = true
	case <-time.After(300 * time.Millisecond):
		// Open is blocked on the registry lock (the post-fix serialization).
	}
	close(release)
	if !openCompletedInWindow {
		select {
		case openErr = <-openDone:
		case <-time.After(10 * time.Second):
			t.Fatal("Open never completed after the gate release")
		}
	}
	if openErr != nil {
		t.Fatalf("Open (racing erase): %v", openErr)
	}
	select {
	case eerr := <-eraseDone:
		if eerr != nil {
			t.Fatalf("Erase: %v", eerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Erase never completed after the gate release")
	}

	// A FRESH registry over the SAME store re-hydrates the in-memory index
	// ONLY from the persisted catalog — so it re-discovers exactly the
	// sessions the catalog still records. Both survivors (keep + open) MUST
	// be there; the erased one MUST NOT.
	fresh, err := sessions.New(inner, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("fresh registry: %v", err)
	}
	snaps, err := fresh.ListSnapshots(ctx, sessions.SessionListFilter{
		TenantIDs: []string{tenant}, UserIDs: []string{user}, IncludeClosed: true,
	})
	_ = fresh.CloseRegistry(ctx)
	if err != nil {
		t.Fatalf("fresh ListSnapshots: %v", err)
	}
	got := make(map[string]bool, len(snaps))
	for _, s := range snaps {
		got[s.ID] = true
	}
	if !got[keep.SessionID] {
		t.Fatalf("kept session %q not re-discovered by a fresh registry (catalog lost-update)", keep.SessionID)
	}
	if !got[open.SessionID] {
		t.Fatalf("session %q opened inside the erasure's RMW window was NOT re-discovered by a fresh registry — its catalog entry was lost-updated away", open.SessionID)
	}
	if got[erase.SessionID] {
		t.Fatalf("erased session %q re-discovered by a fresh registry (catalog not cleaned)", erase.SessionID)
	}
}
