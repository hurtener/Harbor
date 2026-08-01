package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// errPointerWrite is the injected store failure. It stands in for any store
// error arriving between the driver's two writes (a dropped connection, a
// disk-full SQLite, a context that went away).
var errPointerWrite = errors.New("injected: active-pointer write failed")

// errDeleteRefused is the injected failure of the COMPENSATION itself — the
// second-order case where the store is down hard enough to refuse the
// cleanup too.
var errDeleteRefused = errors.New("injected: delete refused")

// errPointerRead is the injected failure of the active-pointer READ the
// compensation performs to decide whether the write it is compensating
// actually landed.
var errPointerRead = errors.New("injected: active-pointer read failed")

var (
	errRevisionWrite = errors.New("injected: revision-record write failed")
	errRevisionRead  = errors.New("injected: revision-record read failed")
)

// isActivePointerKind reports whether kind is one of the two active-pointer
// record kinds ("agentcfg.active" / "agentcfg.user.active"). The revision
// kinds ("agentcfg.revision.<id>" / "agentcfg.user.revision.<id>") never end
// in ".active", so the suffix test cleanly separates the write that PUBLISHES
// a revision from the write that PERSISTS it.
func isActivePointerKind(kind string) bool {
	return strings.HasSuffix(kind, ".active")
}

func isRevisionKind(kind string) bool {
	return strings.Contains(kind, ".revision.")
}

// faultStore wraps a real StateStore and fails the Save that publishes a
// revision as the active one, leaving the revision record itself written.
// This is the driver-specific arming the shared conformance FaultFactory
// describes: only this package knows which record kinds are which.
//
// It is a fault INJECTOR, not a re-implementation of the store — every other
// method delegates to the embedded production driver, so the seam under test
// is still the real one.
type faultStore struct {
	state.StateStore
	failDelete bool
	// cancelOnPointerWrite, when set, is invoked just before the pointer
	// write fails: the real-world interleaving where the write fails BECAUSE
	// the caller's context went away.
	cancelOnPointerWrite context.CancelFunc
	// requireLiveDeleteCtx makes the compensating delete assert it arrived on
	// a context that is still alive.
	requireLiveDeleteCtx bool
	// commitPointerThenFail models the failure mode that is far more common in
	// production than a write that did not happen: the pointer write COMMITS
	// and the caller still receives an error — a deadline firing after commit,
	// a dropped ack, a proxy timeout, a connection reset while the response
	// was in flight. The store's answer says "failed"; the disk says
	// otherwise, and the disk is right.
	commitPointerThenFail bool
	// blindAfterPointerFailure makes every subsequent active-pointer READ
	// fail, so the compensation cannot tell whether the pointer landed. This
	// is the store-is-down-hard case, and it is the one where "delete on an
	// unknown answer" and "retain on an unknown answer" diverge.
	blindAfterPointerFailure bool
	// pointerFailed latches once the pointer write has failed. Reads of the
	// active kind are only blinded AFTER that moment, so the read that opens
	// SetRevision's read-modify-write still succeeds and the fault is confined
	// to the compensation.
	pointerFailed atomic.Bool
	// Revision-record fault modes exercise the FIRST Save, whose return is
	// equally ambiguous: commit-then-error, no-commit, and unreadable-after-
	// commit must not be collapsed into one outcome.
	failRevisionWrite         bool
	commitRevisionThenFail    bool
	blindAfterRevisionFailure bool
	revisionFailed            atomic.Bool
}

func (f *faultStore) Save(ctx context.Context, r state.StateRecord) error {
	if isRevisionKind(r.Kind) && (f.failRevisionWrite || f.commitRevisionThenFail) {
		if f.commitRevisionThenFail {
			if err := f.StateStore.Save(ctx, r); err != nil {
				return err
			}
		}
		f.revisionFailed.Store(true)
		return errRevisionWrite
	}
	if isActivePointerKind(r.Kind) {
		if f.commitPointerThenFail {
			// Commit first, THEN report failure — the durable state and the
			// returned error disagree, which is the whole point.
			if err := f.StateStore.Save(ctx, r); err != nil {
				return err
			}
		}
		if f.cancelOnPointerWrite != nil {
			f.cancelOnPointerWrite()
		}
		f.pointerFailed.Store(true)
		return errPointerWrite
	}
	return f.StateStore.Save(ctx, r)
}

func (f *faultStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if f.blindAfterRevisionFailure && isRevisionKind(kind) && f.revisionFailed.Load() {
		return state.StateRecord{}, errRevisionRead
	}
	if f.blindAfterPointerFailure && isActivePointerKind(kind) && f.pointerFailed.Load() {
		return state.StateRecord{}, errPointerRead
	}
	return f.StateStore.Load(ctx, id, kind)
}

func TestSetRevision_RevisionSaveCommittedThenErrored_DeletesExactUnreferencedRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T) state.StateStore
	}{
		{name: "inmem", make: newInmemStore},
		{name: "sqlite", make: newSharedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistryOverStore(t, tc.make, func(st state.StateStore) state.StateStore {
				return &faultStore{StateStore: st, commitRevisionThenFail: true}
			})
			id := atomicityID()
			_, err := r.SetRevision(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
			if !errors.Is(err, errRevisionWrite) {
				t.Fatalf("SetRevision error = %v, want injected revision-write cause", err)
			}
			revs, listErr := r.ListRevisions(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, 0)
			if listErr != nil {
				t.Fatalf("ListRevisions: %v", listErr)
			}
			if len(revs) != 0 {
				t.Fatalf("commit-then-error left %d unreferenced revision record(s): %+v", len(revs), revs)
			}
			if _, ok, activeErr := r.Active(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent); activeErr != nil || ok {
				t.Fatalf("first record-save failure changed active state: ok=%v err=%v", ok, activeErr)
			}
		})
	}
}

func TestSetRevision_RevisionSaveAbsentOrUnreadable_DistinguishesCleanupOutcome(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
			return &faultStore{StateStore: st, failRevisionWrite: true}
		})
		id := atomicityID()
		_, err := r.SetRevision(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
		if !errors.Is(err, errRevisionWrite) {
			t.Fatalf("SetRevision error = %v, want injected cause", err)
		}
		revs, _ := r.ListRevisions(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, 0)
		if len(revs) != 0 {
			t.Fatalf("a non-committed write left records: %+v", revs)
		}
	})
	t.Run("unreadable-retained", func(t *testing.T) {
		r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
			return &faultStore{StateStore: st, commitRevisionThenFail: true, blindAfterRevisionFailure: true}
		})
		id := atomicityID()
		_, err := r.SetRevision(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
		if !errors.Is(err, errRevisionWrite) || !errors.Is(err, errRevisionRead) || !strings.Contains(err.Error(), "retained") {
			t.Fatalf("unreadable exact record was not retained and reported: %v", err)
		}
		revs, listErr := r.ListRevisions(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, 0)
		if listErr != nil || len(revs) != 1 {
			t.Fatalf("unreadable record retention: len=%d err=%v", len(revs), listErr)
		}
	})
}

func (f *faultStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	if f.failDelete {
		return errDeleteRefused
	}
	if f.requireLiveDeleteCtx {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("compensating delete arrived on a dead context: %w", err)
		}
	}
	return f.StateStore.Delete(ctx, id, kind)
}

// newInmemStore / newRegistryOver build a Registry over a fresh in-memory
// StateStore, optionally wrapped by a fault injector.
func newInmemStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func newRegistryOver(t *testing.T, wrap func(state.StateStore) state.StateStore) agentcfg.Registry {
	t.Helper()
	return newRegistryOverStore(t, newInmemStore, wrap)
}

// newRegistryOverStore builds a Registry over a store built by mkStore and
// optionally wrapped by a fault injector. The store constructor is a parameter
// so the SAME fault arming runs over more than one state driver: the atomicity
// and compensation invariants are owed across the persistence triad, and a
// row armed over one driver only asserts one driver's behaviour.
func newRegistryOverStore(t *testing.T, mkStore func(*testing.T) state.StateStore, wrap func(state.StateStore) state.StateStore) agentcfg.Registry {
	t.Helper()
	st := mkStore(t)
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	if wrap != nil {
		st = wrap(st)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{
		State: st,
		Bus:   bus,
	})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return reg
}

// newFaultyRegistry builds a Registry over a store armed to fail the
// active-pointer write. It is the conformance suite's FaultFactory for this
// driver.
func newFaultyRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOver(t, armPointerWriteFailure)
}

// armPointerWriteFailure / armCommittedPointerWriteFailure are the two fault
// arms the shared suite requires, factored out so they can be applied over any
// state driver.
func armPointerWriteFailure(st state.StateStore) state.StateStore {
	return &faultStore{StateStore: st}
}

func armCommittedPointerWriteFailure(st state.StateStore) state.StateStore {
	return &faultStore{StateStore: st, commitPointerThenFail: true}
}

func atomicityID() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "t", UserID: "admin-1", SessionID: "s",
	}}
}

// TestSetRevision_PointerWriteFailure_SurfacesTheStoreError pins that the
// compensation does NOT mask the failure: the caller still receives the
// store's own error, wrapped in the driver's ErrStateUnavailable sentinel.
// A compensation that swallowed the cause would leave the caller unable to
// branch on why its write failed.
func TestSetRevision_PointerWriteFailure_SurfacesTheStoreError(t *testing.T) {
	ctx := context.Background()
	r := newFaultyRegistry(t)
	_, err := r.SetRevision(ctx, atomicityID(), "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
	if err == nil {
		t.Fatal("SetRevision succeeded although the active-pointer write was armed to fail")
	}
	if !errors.Is(err, agentcfg.ErrStateUnavailable) {
		t.Fatalf("error does not wrap ErrStateUnavailable: %v", err)
	}
	if !errors.Is(err, errPointerWrite) {
		t.Fatalf("error does not carry the injected store cause: %v", err)
	}
}

// TestSetRevision_PointerWriteFailure_RemovesTheOrphanRevision is the
// driver-level twin of the shared conformance row: the revision record the
// failed write persisted is GONE, so history carries no revision that nothing
// references.
//
// Mutation that turns this red: return the store error directly from
// SetRevision instead of routing it through compensateOrphanRevision.
func TestSetRevision_PointerWriteFailure_RemovesTheOrphanRevision(t *testing.T) {
	ctx := context.Background()
	r := newFaultyRegistry(t)
	id := atomicityID()
	if _, err := r.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision succeeded although the active-pointer write was armed to fail")
	}
	revs, err := r.ListRevisions(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("the failed write left %d revision(s) behind: %+v", len(revs), revs)
	}
}

// TestSetRevision_PointerWriteFailure_CompensatesOnACancelledContext is the
// reason the compensation runs on a context.WithoutCancel. A cancelled or
// timed-out caller context is the MOST likely production reason for the
// pointer write to fail, so a compensation issued on the caller's own context
// would fail on exactly the occasions it exists for — the orphan would
// survive precisely the common case.
//
// The store cancels the caller's context at the moment the pointer write
// fails (the real interleaving) and then asserts the compensating delete
// arrived on a LIVE context.
//
// Mutation that turns this red: pass ctx straight to state.Delete instead of
// context.WithoutCancel(ctx).
func TestSetRevision_PointerWriteFailure_CompensatesOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
		return &faultStore{
			StateStore:           st,
			cancelOnPointerWrite: cancel,
			requireLiveDeleteCtx: true,
		}
	})
	id := atomicityID()
	if _, err := r.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision succeeded although the active-pointer write was armed to fail")
	}
	// Read back on a live context — the residue question is about the store,
	// not about the caller's dead context.
	revs, err := r.ListRevisions(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("a cancelled-context failure left %d revision(s) behind — the compensation inherited the dead context: %+v", len(revs), revs)
	}
}

// TestSetRevision_CompensationFailure_IsReportedNotSwallowed covers the
// second-order case: the store refuses the compensating delete too. The
// residue is then real, and the driver says so — the returned error names the
// unreferenced record AND still carries the original cause, so a caller
// branching on ErrStateUnavailable is unaffected while an operator reading
// the message learns a record was left behind.
//
// Mutation that turns this red: ignore the delete error and return `cause`.
func TestSetRevision_CompensationFailure_IsReportedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
		return &faultStore{StateStore: st, failDelete: true}
	})
	_, err := r.SetRevision(ctx, atomicityID(), "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
	if err == nil {
		t.Fatal("SetRevision succeeded although the active-pointer write was armed to fail")
	}
	if !errors.Is(err, errPointerWrite) {
		t.Fatalf("the original cause was dropped: %v", err)
	}
	if !errors.Is(err, errDeleteRefused) {
		t.Fatalf("the compensation failure was swallowed rather than reported: %v", err)
	}
	if !strings.Contains(err.Error(), "unreferenced") {
		t.Fatalf("the error does not name the residue in words an operator can act on: %v", err)
	}
}

// TestSetRevision_SuccessfulWritePathIssuesNoDelete pins that the
// compensation is confined to the failure path: an ordinary successful write
// must not delete anything. A compensation that fired unconditionally would
// destroy the revision it just wrote.
func TestSetRevision_SuccessfulWritePathIssuesNoDelete(t *testing.T) {
	ctx := context.Background()
	var deletes int
	r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
		return &countingDeleteStore{StateStore: st, deletes: &deletes}
	})
	id := atomicityID()
	if _, err := r.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	if deletes != 0 {
		t.Fatalf("a successful write issued %d delete(s) — the compensation is not confined to the failure path", deletes)
	}
	revs, err := r.ListRevisions(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("history holds %d revision(s) after one successful write, want 1", len(revs))
	}
}

// newCommittedFaultRegistry builds a Registry over a store whose pointer write
// COMMITS and then reports failure. It is the conformance suite's
// CommittedFaultFactory for this driver.
func newCommittedFaultRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOver(t, armCommittedPointerWriteFailure)
}

// The SQLite arm of the same three factories. The atomicity and compensation
// invariants are owed across the persistence triad, so the fault is armed over
// the durable driver too rather than over the in-memory one alone: SQLite has
// real transactions, a WAL and a file, so "the record survived" and "the record
// was removed" are answered by the disk rather than by a map.
func newSQLiteRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOverStore(t, newSharedStore, nil)
}

func newSQLiteFaultyRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOverStore(t, newSharedStore, armPointerWriteFailure)
}

func newSQLiteCommittedFaultRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOverStore(t, newSharedStore, armCommittedPointerWriteFailure)
}

// TestSetRevision_PointerWriteCommittedThenErrored_KeepsTheRevision is the
// regression this file exists for.
//
// The compensation was written for the case where the pointer write did not
// happen. The commoner production case is the one where it DID: a deadline
// firing after commit, a dropped ack, a proxy timeout, a reset connection.
// The store answers "failed"; the record is durably on disk naming the very
// revision the compensation is about to delete.
//
// An unconditional delete there does not tidy an orphan — it manufactures a
// DANGLING pointer, and the agent is then unrecoverable: every door reads
// through Active(), which fails loud on a pointer that names nothing, so no
// later write can repair it. That is strictly worse than the operator-visible
// list_revisions artifact the compensation was introduced to remove.
//
// Mutation that turns this red: drop the pointer re-read and delete
// unconditionally (the shape this fixes).
func TestSetRevision_PointerWriteCommittedThenErrored_KeepsTheRevision(t *testing.T) {
	ctx := context.Background()
	r := newCommittedFaultRegistry(t)
	id := atomicityID()

	if _, err := r.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision reported success although the store returned an error")
	}

	// The agent is USABLE: the pointer landed, so it names a revision that
	// must still be there.
	rev, ok, err := r.Active(ctx, id, "agent-x", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("Active is broken after a committed-then-errored pointer write — the compensation deleted the revision the pointer names: %v", err)
	}
	if !ok {
		t.Fatal("Active reports no config although the pointer write committed")
	}
	if rev.RevisionID == "" {
		t.Fatal("Active returned an empty revision id")
	}
	revs, err := r.ListRevisions(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("history holds %d revision(s), want the 1 the durable pointer names: %+v", len(revs), revs)
	}
}

// TestSetRevision_PointerWriteCommittedThenErrored_LeavesTheAgentWritable is
// the half that makes the defect UNRECOVERABLE rather than merely wrong: every
// door does a read-modify-write through the active pointer, so once the pointer
// dangles no subsequent healthy write can repair it. This runs a second,
// completely healthy registry over the same store and requires it to succeed.
func TestSetRevision_PointerWriteCommittedThenErrored_LeavesTheAgentWritable(t *testing.T) {
	ctx := context.Background()
	shared := newSharedStore(t)

	// Registry one: the store commits the pointer and then errors.
	broken := newRegistryOnStore(t, &faultStore{StateStore: shared, commitPointerThenFail: true})
	id := atomicityID()
	if _, err := broken.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision reported success although the store returned an error")
	}

	// Registry two: an entirely healthy Runtime over the same durable store.
	healthy := newRegistryOnStore(t, shared)
	if _, err := healthy.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1", "s2"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("a subsequent HEALTHY write cannot repair the agent — the config is unrecoverable: %v", err)
	}
	rev, ok, err := r2Active(ctx, healthy, id)
	if err != nil || !ok {
		t.Fatalf("Active after the healthy write: ok=%v err=%v", ok, err)
	}
	if len(rev.Payload.SkillNames()) != 2 {
		t.Fatalf("the healthy write did not land: skills=%v", rev.Payload.SkillNames())
	}
}

func r2Active(ctx context.Context, r agentcfg.Registry, id identity.Quadruple) (agentcfg.Revision, bool, error) {
	return r.Active(ctx, id, "agent-x", agentcfg.ConfigScopeAgent)
}

// TestSetRevision_PointerWriteCommittedThenErrored_StillAnnouncesTheRevision
// closes the silent half of the same defect. Retaining the record because the
// pointer names it means the config DID change; suppressing the revised event
// because the call is about to return an error would leave every observer's
// view stale behind a change that really happened. The caller's error and the
// bus must tell the same story.
//
// The caller's context is CANCELLED at the moment the pointer write fails —
// the same interleaving the compensation's un-cancellable context exists for,
// and the likeliest way to reach this branch at all. An announcement issued on
// the caller's own context would be dropped on exactly those occasions.
//
// Mutations that turn this red: drop the emitRevised call from the landed
// branch of SetRevision's compensation; or issue it on the caller's ctx
// instead of a context.WithoutCancel.
func TestSetRevision_PointerWriteCommittedThenErrored_StillAnnouncesTheRevision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := newBus(t)
	id := atomicityID()
	// The revised event carries the AUTHOR's identity (the caller's triple),
	// not the synthetic slot the records are keyed under.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{agentcfg.EventTypeConfigRevised},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{
		State: &faultStore{
			StateStore:            newInmemStore(t),
			commitPointerThenFail: true,
			cancelOnPointerWrite:  cancel,
		},
		Bus: &liveCtxBus{EventBus: bus},
	})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	if _, err := reg.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{}); err == nil {
		t.Fatal("SetRevision reported success although the store returned an error")
	}
	// Read back on a LIVE context — the caller's own is cancelled by now, and
	// the question here is about the store, not about the caller's context.
	active, ok, err := reg.Active(context.Background(), id, "agent-x", agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	select {
	case ev := <-sub.Events():
		p, isRevised := ev.Payload.(agentcfg.ConfigRevisedPayload)
		if !isRevised {
			t.Fatalf("payload type = %T, want ConfigRevisedPayload", ev.Payload)
		}
		if p.RevisionID != active.RevisionID {
			t.Fatalf("announced revision %q, active revision %q — the bus and the store disagree", p.RevisionID, active.RevisionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the config changed durably and NOTHING was announced — an observer's view stays stale behind a change that really happened")
	}
}

// TestSetRevision_CompensationCannotReadThePointer_RetainsTheRevision pins the
// deliberate choice on the UNKNOWN answer.
//
// The compensation asks one question — does the active pointer name the
// revision I just wrote? A store that refuses the read gives no answer, and
// "absent" and "cannot tell" are then indistinguishable. Deleting on a
// cannot-tell reintroduces exactly the defect above, on the population where it
// is most likely (a store sick enough to fail the write is sick enough to fail
// the read). So the record is RETAINED and the residue is REPORTED: the caller's
// error names it and an Error-level log carries it. The cost of retaining is an
// operator-visible list_revisions row; the cost of deleting is an unrecoverable
// agent. They are not comparable.
//
// Mutation that turns this red: delete when the pointer read fails.
func TestSetRevision_CompensationCannotReadThePointer_RetainsTheRevision(t *testing.T) {
	ctx := context.Background()
	r := newRegistryOver(t, func(st state.StateStore) state.StateStore {
		return &faultStore{StateStore: st, commitPointerThenFail: true, blindAfterPointerFailure: true}
	})
	id := atomicityID()
	_, err := r.SetRevision(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, skills("s1"), agentcfg.SetOptions{})
	if err == nil {
		t.Fatal("SetRevision reported success although the store returned an error")
	}
	if !errors.Is(err, errPointerWrite) {
		t.Fatalf("the original cause was dropped: %v", err)
	}
	if !errors.Is(err, errPointerRead) {
		t.Fatalf("the unreadable pointer was swallowed rather than reported: %v", err)
	}
	if !strings.Contains(err.Error(), "retained") {
		t.Fatalf("the error does not say the record was retained, so an operator cannot act on it: %v", err)
	}
	// The record survived the cannot-tell. Read it back through a store that
	// is no longer blinded.
	revs, err := r.ListRevisions(ctx, id, "agent-x", agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("the revision was deleted on an UNKNOWN answer — %d revision(s) remain, want 1", len(revs))
	}
}

// liveCtxBus refuses a Publish that arrives on a dead context, and delegates
// everything else to the real bus.
//
// It exists because the in-memory bus does NOT consult the context on Publish,
// so a test that merely cancels the caller and waits for the event stays green
// whether the announcement is issued on a live context or a dead one — a guard
// whose pass value is also its can't-tell value. A durable bus WOULD refuse the
// dead context, so this wrapper makes the in-memory arm answer the same
// question the durable one would, and makes the WithoutCancel load-bearing
// under mutation.
type liveCtxBus struct {
	events.EventBus
}

func (b *liveCtxBus) Publish(ctx context.Context, ev events.Event) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish arrived on a dead context: %w", err)
	}
	return b.EventBus.Publish(ctx, ev)
}

// countingDeleteStore counts Delete calls without altering behaviour.
type countingDeleteStore struct {
	state.StateStore
	deletes *int
}

func (c *countingDeleteStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	*c.deletes++
	return c.StateStore.Delete(ctx, id, kind)
}
