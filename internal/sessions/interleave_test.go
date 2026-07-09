package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
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
		if err := reg.Touch(ctxFor(id), id.SessionID); !errors.Is(err, sessions.ErrReopenAfterClose) {
			t.Fatalf("iter %d: Touch on closed session = %v, want ErrReopenAfterClose", i, err)
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

// TestRegistry_Interleave_EraseVsOpen_NoResurrection pins the catalog
// lost-update fix (FIX-1): the per-(tenant, user) discovery catalog's
// read-modify-write must serialize an Open's add against an erasure's
// remove. Before folding removeFromCatalog into clearErased's r.mu section
// (and routing both catalog writers through the serialized mutateCatalog
// path), the confirmed race was: the eraser loads the catalog {keep, erase},
// an Open(new) completes its own add ({keep, erase, new}), the eraser saves
// its stale difference ({keep}); the freshly-opened session's record survived
// but its catalog entry was gone — invisible to sessions.list and never
// re-discovered by a fresh process after a restart (the StateStore has no
// List). Each iteration races one Erase against one Open of a sibling and
// asserts a FRESH registry over the SAME store re-discovers every surviving
// session (the kept one AND the newly-opened one) while the erased one stays
// gone.
func TestRegistry_Interleave_EraseVsOpen_NoResurrection(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()

	const iterations = 20
	for i := range iterations {
		const tenant, user = "t1", "u1"
		keep := ident(tenant, user, fmt.Sprintf("s-keep-%d", i))
		erase := ident(tenant, user, fmt.Sprintf("s-erase-%d", i))
		open := ident(tenant, user, fmt.Sprintf("s-open-%d", i))
		if _, err := f.reg.Open(ctxFor(keep), keep.SessionID, keep); err != nil {
			t.Fatalf("iter %d: Open keep: %v", i, err)
		}
		if _, err := f.reg.Open(ctxFor(erase), erase.SessionID, erase); err != nil {
			t.Fatalf("iter %d: Open erase: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := f.eraser.Erase(ctx, erase); err != nil {
				t.Errorf("iter %d: Erase: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := f.reg.Open(ctxFor(open), open.SessionID, open); err != nil {
				t.Errorf("iter %d: Open (racing erase): %v", i, err)
			}
		}()
		wg.Wait()

		// A FRESH registry over the SAME store re-hydrates the in-memory index
		// ONLY from the persisted catalog — so it re-discovers exactly the
		// sessions the catalog still records. Both survivors (keep + open) MUST
		// be there; the erased one MUST NOT.
		fresh, err := sessions.New(f.store, config.SessionsConfig{
			IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
		}, f.bus)
		if err != nil {
			t.Fatalf("iter %d: fresh registry: %v", i, err)
		}
		snaps, err := fresh.ListSnapshots(ctx, sessions.SessionListFilter{
			TenantIDs: []string{tenant}, UserIDs: []string{user}, IncludeClosed: true,
		})
		_ = fresh.CloseRegistry(ctx)
		if err != nil {
			t.Fatalf("iter %d: fresh ListSnapshots: %v", i, err)
		}
		got := make(map[string]bool, len(snaps))
		for _, s := range snaps {
			got[s.ID] = true
		}
		if !got[keep.SessionID] {
			t.Fatalf("iter %d: kept session %q not re-discovered by a fresh registry (catalog lost-update)", i, keep.SessionID)
		}
		if !got[open.SessionID] {
			t.Fatalf("iter %d: session %q opened while racing an erase was NOT re-discovered by a fresh registry — its catalog entry was lost-updated away", i, open.SessionID)
		}
		if got[erase.SessionID] {
			t.Fatalf("iter %d: erased session %q re-discovered by a fresh registry (catalog not cleaned)", i, erase.SessionID)
		}
	}
}
