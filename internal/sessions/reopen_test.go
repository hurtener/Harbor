package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

// TestRegistry_Reopen_GCClosed_Reactivates — a session reaped by the GC sweep
// (Closed=true, ClosedReason="gc:idle") is RE-ACTIVATED by EnsureOpen with the
// SAME OpenedAt and identity, a refreshed LastSeen, a stamped LastReopenedAt,
// and re-appears open in ListSnapshots (RFC §6.9 amended — D-312).
func TestRegistry_Reopen_GCClosed_Reactivates(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	reg, _ := testWiring(t, sessions.WithClock(clock))
	id := ident("t1", "u1", "conv-gc")

	first, err := reg.Open(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Advance past the idle TTL and sweep so the GC reaps it.
	clock.Advance(25 * time.Hour)
	reaped, err := reg.GC(ctxFor(id), sessions.GCPolicy{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("GC reaped %d, want 1", reaped)
	}

	clock.Advance(time.Hour)
	reopened, err := reg.EnsureOpen(ctxFor(id), id)
	if err != nil {
		t.Fatalf("EnsureOpen (reopen GC-closed): %v", err)
	}
	if reopened.Closed {
		t.Errorf("reopened session still Closed")
	}
	if !reopened.OpenedAt.Equal(first.OpenedAt) {
		t.Errorf("OpenedAt changed: first=%v reopened=%v", first.OpenedAt, reopened.OpenedAt)
	}
	if reopened.LastReopenedAt.IsZero() {
		t.Errorf("LastReopenedAt not stamped")
	}
	if !reopened.LastSeen.After(first.LastSeen) {
		t.Errorf("LastSeen not refreshed: first=%v reopened=%v", first.LastSeen, reopened.LastSeen)
	}
	// Re-appears open in the (open-only) listing.
	snaps, err := reg.ListSnapshots(ctxFor(id), sessions.SessionListFilter{
		TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID},
		SessionIDs: []string{id.SessionID},
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Closed {
		t.Fatalf("ListSnapshots after reopen = %+v, want one OPEN row", snaps)
	}
}

// TestRegistry_Reopen_ExplicitClose_SurfacesPriorReason — the prior
// ClosedReason is surfaced on the session.reopened event's PriorClosedReason
// and then cleared on the record.
func TestRegistry_Reopen_ExplicitClose_SurfacesPriorReason(t *testing.T) {
	t.Parallel()
	reg, bus := testWiring(t)
	id := ident("t1", "u1", "conv-explicit")

	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{sessions.EventTypeSessionReopened},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := reg.Close(ctxFor(id), id.SessionID, "operator:maintenance"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := reg.Open(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.ClosedReason != "" {
		t.Errorf("ClosedReason not cleared: %q", reopened.ClosedReason)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(sessions.SessionReopenedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want SessionReopenedPayload", ev.Payload)
		}
		if p.SessionID != id.SessionID {
			t.Errorf("SessionID = %q, want %q", p.SessionID, id.SessionID)
		}
		if p.PriorClosedReason != "operator:maintenance" {
			t.Errorf("PriorClosedReason = %q, want operator:maintenance", p.PriorClosedReason)
		}
		if p.ReopenedAt == 0 {
			t.Errorf("ReopenedAt not set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.reopened")
	}
}

// TestRegistry_Reopen_HardCapRestartsFromReopen pins FAIL-1: the GC hard cap
// is measured from max(OpenedAt, LastReopenedAt). A session opened before the
// hard cap, closed, and reopened is NOT re-reaped on the next sweep; a
// never-reopened over-cap session still hard-caps (no regression).
func TestRegistry_Reopen_HardCapRestartsFromReopen(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	// HardCap 720h (30d); short idle so the reopen's refreshed LastSeen keeps
	// idle from firing while we probe the hard cap specifically.
	reg, _ := testWiring(t, sessions.WithClock(clock),
		sessions.WithGCPolicy(sessions.GCPolicy{
			IdleTTL: 1000 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
		}))

	reopened := ident("t1", "u1", "conv-reopened")
	stale := ident("t1", "u1", "conv-stale")
	if _, err := reg.Open(ctxFor(reopened), reopened.SessionID, reopened); err != nil {
		t.Fatalf("Open reopened: %v", err)
	}
	if _, err := reg.Open(ctxFor(stale), stale.SessionID, stale); err != nil {
		t.Fatalf("Open stale: %v", err)
	}

	// Age BOTH past the hard cap. Close+reopen the first so its LastReopenedAt
	// is now (restarting its hard-cap clock); the second is never reopened.
	clock.Advance(721 * time.Hour)
	if err := reg.Close(ctxFor(reopened), reopened.SessionID, "test"); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
	if _, err := reg.Open(ctxFor(reopened), reopened.SessionID, reopened); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// Sweep now: the reopened session's hard-cap clock restarted at the reopen
	// (age 0), so it is NOT reaped; the never-reopened one is over-cap → reaped.
	reapedCount, err := reg.GC(ctxFor(reopened), sessions.GCPolicy{
		IdleTTL: 1000 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if reapedCount != 1 {
		t.Fatalf("GC reaped %d, want 1 (only the never-reopened over-cap session)", reapedCount)
	}
	rs, err := reg.Get(ctxFor(reopened), reopened.SessionID)
	if err != nil {
		t.Fatalf("Get reopened: %v", err)
	}
	if rs.Closed {
		t.Errorf("reopened session was re-reaped by the hard cap (FAIL-1 regression)")
	}
	ss, err := reg.Get(ctxFor(stale), stale.SessionID)
	if err != nil {
		t.Fatalf("Get stale: %v", err)
	}
	if !ss.Closed || ss.ClosedReason != "gc:hard_cap" {
		t.Errorf("never-reopened over-cap session not hard-capped: %+v", ss)
	}
}

// TestRegistry_Reopen_CrossTenant_Rejected pins that a SessionID closed under
// tenant A, then Opened under tenant B, returns ErrSessionIDReuse — NOT a
// reopen and NOT ErrReopenAfterErase.
func TestRegistry_Reopen_CrossTenant_Rejected(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	idA := ident("tA", "uA", "shared-sid")
	idB := ident("tB", "uB", "shared-sid")
	if _, err := reg.Open(ctxFor(idA), idA.SessionID, idA); err != nil {
		t.Fatalf("tenant A Open: %v", err)
	}
	if err := reg.Close(ctxFor(idA), idA.SessionID, "test"); err != nil {
		t.Fatalf("tenant A Close: %v", err)
	}
	_, err := reg.Open(ctxFor(idB), idB.SessionID, idB)
	if !errors.Is(err, sessions.ErrSessionIDReuse) {
		t.Fatalf("cross-tenant reopen = %v, want ErrSessionIDReuse", err)
	}
	if errors.Is(err, sessions.ErrReopenAfterErase) {
		t.Fatalf("cross-tenant reopen wrongly classified as ErrReopenAfterErase")
	}
}

// TestSessionReopenedPayload_ContentFree pins that the marshalled payload
// carries EXACTLY {SessionID, ReopenedAt, PriorClosedReason} and no user
// content (no title, no conversation body).
func TestSessionReopenedPayload_ContentFree(t *testing.T) {
	t.Parallel()
	p := sessions.SessionReopenedPayload{
		SessionID:         "s1",
		ReopenedAt:        123,
		PriorClosedReason: "gc:idle",
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]struct{}{"SessionID": {}, "ReopenedAt": {}, "PriorClosedReason": {}}
	for k := range m {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected field %q in reopened payload — content-free contract broken: %s", k, raw)
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q in reopened payload: %s", k, raw)
		}
	}
}

// faultyLoadStore wraps a StateStore and forces Load to return a non-NotFound
// error for any Kind carrying the erasure-observability prefix — the WARN-C
// fault-injection seam for isErased's fail-closed path. All other methods
// promote via embedding.
type faultyLoadStore struct {
	state.StateStore
	failPrefix string
	failErr    error
}

func (s *faultyLoadStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.failPrefix != "" && len(kind) >= len(s.failPrefix) && kind[:len(s.failPrefix)] == s.failPrefix {
		return state.StateRecord{}, s.failErr
	}
	return s.StateStore.Load(ctx, id, kind)
}

// TestRegistry_Reopen_IsErasedFailsClosed_WARNC pins WARN-C: a non-NotFound
// Load error on the erasure ledger/tombstone probe propagates out of Open —
// reopen mints NOTHING and never re-activates on an unverified erased-state
// read (fail-closed, mirroring loadLedger).
func TestRegistry_Reopen_IsErasedFailsClosed_WARNC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	red := auditpatterns.New()
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })
	inner, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(ctx) })

	boom := errors.New("forced non-NotFound load fault")
	// "session.erasure." covers both the pending-ledger and tombstone prefixes.
	faulty := &faultyLoadStore{StateStore: inner, failPrefix: "session.erasure.", failErr: boom}
	reg, err := sessions.New(faulty, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(ctx) })

	// A brand-new id: the session record Load returns ErrNotFound (reaching the
	// isErased gate on the fresh-create path), and the ledger/tombstone Load
	// returns the forced fault → Open must propagate it and mint nothing.
	id := ident("t1", "u1", "conv-faulty")
	_, oerr := reg.Open(ctxFor(id), id.SessionID, id)
	if !errors.Is(oerr, boom) {
		t.Fatalf("Open with faulty erasure-probe Load = %v, want the forced fault (fail-closed)", oerr)
	}
	// Nothing was minted: a subsequent Get (with a healthy read of the session
	// kind) still finds no record.
	if _, gerr := reg.Get(ctxFor(id), id.SessionID); !errors.Is(gerr, sessions.ErrSessionNotFound) {
		t.Fatalf("Get after fail-closed Open = %v, want ErrSessionNotFound (nothing minted)", gerr)
	}
}

// --- erasure-integrated reopen tests (real CascadeEraser via newErasureFixture) ---

// TestRegistry_Reopen_ConvergedErase_FailsLoud pins FAIL-2: a FULLY-CONVERGED
// session.erase removes the session.lifecycle record, so Open hits ErrNotFound
// on the fresh-create path — but the durable tombstone makes it fail loud with
// ErrReopenAfterErase rather than silently minting a fresh empty session.
func TestRegistry_Reopen_ConvergedErase_FailsLoud(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-conv", "u-conv", "s-conv")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Full, clean erase → converges (record gone, tombstone written).
	if _, err := f.eraser.Erase(ctx, id); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	// Sanity: the lifecycle record is gone (converged case, not in-flight).
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("session.lifecycle survived the erase: %v", lerr)
	}
	// Reopen on the fresh-create path is TERMINAL via the tombstone.
	if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("reopen converged-erased id = %v, want ErrReopenAfterErase", oerr)
	}
	// EnsureOpen surfaces it too (never a silent fresh session).
	if _, eerr := f.reg.EnsureOpen(ictx, id); !errors.Is(eerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("EnsureOpen converged-erased id = %v, want ErrReopenAfterErase", eerr)
	}
}

// erasureTombstoneTestKindPrefix mirrors the unexported
// sessions.erasureTombstoneKindPrefix literal (see erasureLedgerTestKindPrefix
// for the mirroring rationale — a drift over there fails this test loudly).
const erasureTombstoneTestKindPrefix = "session.erasure.tombstone."

// faultySaveStore wraps a StateStore and forces Save to fail for any Kind
// carrying failPrefix — the WARN-1 fault-injection seam for a tombstone-Save
// failure. All other methods (incl. the ledger Save under a different prefix)
// promote via embedding.
type faultySaveStore struct {
	state.StateStore
	failPrefix string
	failErr    error
}

func (s *faultySaveStore) Save(ctx context.Context, r state.StateRecord) error {
	if s.failPrefix != "" && len(r.Kind) >= len(s.failPrefix) && r.Kind[:len(s.failPrefix)] == s.failPrefix {
		return s.failErr
	}
	return s.StateStore.Save(ctx, r)
}

// TestCascadeEraser_TombstoneSaveFailure_LoudAndKeepsLedger_WARN1 pins the
// WARN-1 success-critical invariant: a failed tombstone Save fails the erasure
// LOUD (wrapped ErrErasureRecordFailed) and MUST NOT proceed to deleteLedger,
// so the pending ledger survives (isErased still fires via the ledger) and a
// re-invoke converges — a tombstone-fails + ledger-deleted interleave would
// otherwise open a converged-erasure resurrection gap.
func TestCascadeEraser_TombstoneSaveFailure_LoudAndKeepsLedger_WARN1(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-warn1", "u-warn1", "s-warn1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	boom := errors.New("forced tombstone save fault")
	faulty := &faultySaveStore{StateStore: f.store, failPrefix: erasureTombstoneTestKindPrefix, failErr: boom}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: faulty, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// The tombstone Save fails at the terminal step → erasure fails loud.
	if _, eerr := eraser.Erase(ctx, id); !errors.Is(eerr, sessions.ErrErasureRecordFailed) {
		t.Fatalf("erase with tombstone-save fault = %v, want ErrErasureRecordFailed", eerr)
	}
	// The pending ledger MUST survive (deleteLedger never ran).
	if _, lerr := f.store.Load(ctx, ledgerScopeForTest(id), erasureLedgerTestKindPrefix+id.SessionID); lerr != nil {
		t.Fatalf("WARN-1 violated: pending ledger deleted despite the tombstone-save failure: %v", lerr)
	}
	// isErased still fires via the surviving ledger → reopen fails loud.
	if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("reopen after tombstone-save fault = %v, want ErrReopenAfterErase (ledger present)", oerr)
	}
	// A re-invoke against a HEALTHY store converges (writes the tombstone, deletes the ledger).
	if _, rerr := f.eraser.Erase(ctx, id); rerr != nil {
		t.Fatalf("converging re-invoke on healthy store: %v", rerr)
	}
	if _, terr := f.store.Load(ctx, ledgerScopeForTest(id), erasureTombstoneTestKindPrefix+id.SessionID); terr != nil {
		t.Fatalf("tombstone missing after healthy convergence: %v", terr)
	}
}

// TestCascadeEraser_Tombstone_UnconditionalWhenAlreadyEmitted_WARNB pins
// WARN-B: the terminal tombstone Save runs on EVERY completeErasure invocation
// that reaches the terminal step — OUTSIDE the recordAlreadyEmitted emit-skip
// guard — and strictly BEFORE deleteLedger. The scenario reproduces a
// converging retry where a PRIOR attempt already emitted the record-of-fact
// but died before the tombstone write (event present, tombstone absent, ledger
// present): the re-invoke sees recordAlreadyEmitted==true, skips the emit, and
// MUST STILL write the tombstone (else deleteLedger would leave NEITHER ledger
// nor tombstone → a reopen-after-erase would silently resurrect).
func TestCascadeEraser_Tombstone_UnconditionalWhenAlreadyEmitted_WARNB(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-warnb", "u-warnb", "s-warnb")
	ictx := ctxFor(id)

	first, err := f.reg.Open(ictx, id.SessionID, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stamp := first.OpenedAt.UnixNano()

	// Clean erase: emits session.erased (stamp), writes the tombstone, deletes
	// the ledger. The record-of-fact event now durably exists.
	if _, eerr := f.eraser.Erase(ctx, id); eerr != nil {
		t.Fatalf("Erase: %v", eerr)
	}
	tombKind := erasureTombstoneTestKindPrefix + id.SessionID
	if _, terr := f.store.Load(ctx, ledgerScopeForTest(id), tombKind); terr != nil {
		t.Fatalf("tombstone missing after clean erase: %v", terr)
	}

	// Simulate the "prior attempt emitted but died before the tombstone write"
	// state: delete the tombstone, and re-seed a pending ledger (matching
	// lifecycle stamp) so the next Erase takes the converging path.
	if derr := f.store.Delete(ctx, ledgerScopeForTest(id), tombKind); derr != nil {
		t.Fatalf("delete tombstone: %v", derr)
	}
	ledgerJSON := []byte(fmt.Sprintf(`{"artifacts_deleted":0,"memory_purged":true,"state_records_deleted":0,"session_opened_at":%d}`, stamp))
	if serr := f.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: ledgerScopeForTest(id),
		Kind:     erasureLedgerTestKindPrefix + id.SessionID,
		Bytes:    ledgerJSON,
	}); serr != nil {
		t.Fatalf("re-seed ledger: %v", serr)
	}

	// Re-invoke: the session record is gone (converging path) and the ledger is
	// present, so completeErasure runs; recordAlreadyEmitted is TRUE (the event
	// still exists), so the emit is skipped — but the tombstone MUST be written.
	if _, rerr := f.eraser.Erase(ctx, id); rerr != nil {
		t.Fatalf("converging re-invoke: %v", rerr)
	}
	if _, terr := f.store.Load(ctx, ledgerScopeForTest(id), tombKind); terr != nil {
		t.Fatalf("WARN-B violated: tombstone NOT re-written on the emit-skip convergence: %v", terr)
	}
	// The ledger was deleted (write-happens-before-delete completed).
	if _, lerr := f.store.Load(ctx, ledgerScopeForTest(id), erasureLedgerTestKindPrefix+id.SessionID); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("ledger survived the converged re-invoke: %v", lerr)
	}
	// And reopen stays terminal via the re-written tombstone.
	if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("reopen after WARN-B convergence = %v, want ErrReopenAfterErase", oerr)
	}
}

// TestRegistry_Reopen_VsErase_Race pins the D-025 reopen-vs-erase race: N
// concurrent (reopen(S) || erase(S)) pairs on distinct sessions against one
// shared registry+eraser resolve to EXACTLY one of {re-activated,
// ErrReopenAfterErase} with no resurrected-data outcome, under -race. The
// harness includes the interleave where the erase CONVERGES (record gone,
// tombstone written) before the reopen's mint — asserting ErrReopenAfterErase
// there, never a fresh empty session.
func TestRegistry_Reopen_VsErase_Race(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	const n = 120

	// Distinct sessions, each opened then closed (so reopen has a closed record
	// to race against the erase).
	ids := make([]identity.Identity, n)
	for i := range n {
		id := ident("t-race", "u-race", fmt.Sprintf("s-race-%d", i))
		ids[i] = id
		if _, err := f.reg.Open(ctxFor(id), id.SessionID, id); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := f.reg.Close(ctxFor(id), id.SessionID, "race:pre"); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := range n {
		id := ids[i]
		ictx := ctxFor(id)
		wg.Add(2)
		// For a subset, run the erase FIRST to convergence, THEN the reopen, so
		// the reopen deterministically hits the converged (tombstone) not-found
		// path — a race-only harness could otherwise mask the FAIL-2 gap green.
		if i%3 == 0 {
			go func() {
				defer wg.Done()
				if _, err := f.eraser.Erase(ctx, id); err != nil {
					t.Errorf("converged erase %s: %v", id.SessionID, err)
				}
			}()
			go func() {
				defer wg.Done()
				// Small serialization: wait until the record is gone, then reopen.
				for {
					if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); errors.Is(lerr, state.ErrNotFound) {
						break
					}
				}
				if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
					t.Errorf("post-converge reopen %s = %v, want ErrReopenAfterErase", id.SessionID, oerr)
				}
			}()
			continue
		}
		// The rest run reopen || erase concurrently — either ordering is legal,
		// but reopen must be exactly {nil (re-activated), ErrReopenAfterErase}.
		go func() {
			defer wg.Done()
			_, oerr := f.reg.Open(ictx, id.SessionID, id)
			if oerr != nil && !errors.Is(oerr, sessions.ErrReopenAfterErase) {
				t.Errorf("concurrent reopen %s = %v, want nil or ErrReopenAfterErase", id.SessionID, oerr)
			}
		}()
		go func() {
			defer wg.Done()
			if _, eerr := f.eraser.Erase(ctx, id); eerr != nil && !errors.Is(eerr, sessions.ErrSessionNotFound) {
				t.Errorf("concurrent erase %s = %v", id.SessionID, eerr)
			}
		}()
	}
	wg.Wait()
}
