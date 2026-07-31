package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
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

// isActivePointerKind reports whether kind is one of the two active-pointer
// record kinds ("agentcfg.active" / "agentcfg.user.active"). The revision
// kinds ("agentcfg.revision.<id>" / "agentcfg.user.revision.<id>") never end
// in ".active", so the suffix test cleanly separates the write that PUBLISHES
// a revision from the write that PERSISTS it.
func isActivePointerKind(kind string) bool {
	return strings.HasSuffix(kind, ".active")
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
}

func (f *faultStore) Save(ctx context.Context, r state.StateRecord) error {
	if isActivePointerKind(r.Kind) {
		if f.cancelOnPointerWrite != nil {
			f.cancelOnPointerWrite()
		}
		return errPointerWrite
	}
	return f.StateStore.Save(ctx, r)
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

// newRegistryOver builds a Registry over the supplied StateStore.
func newRegistryOver(t *testing.T, wrap func(state.StateStore) state.StateStore) agentcfg.Registry {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
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
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{
		State: wrap(st),
		Bus:   bus,
	})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg
}

// newFaultyRegistry builds a Registry over a store armed to fail the
// active-pointer write. It is the conformance suite's FaultFactory for this
// driver.
func newFaultyRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	return newRegistryOver(t, func(st state.StateStore) state.StateStore {
		return &faultStore{StateStore: st}
	})
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

// countingDeleteStore counts Delete calls without altering behaviour.
type countingDeleteStore struct {
	state.StateStore
	deletes *int
}

func (c *countingDeleteStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	*c.deletes++
	return c.StateStore.Delete(ctx, id, kind)
}
