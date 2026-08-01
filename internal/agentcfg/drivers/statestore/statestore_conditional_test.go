package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
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

	// The cross-process residual test needs a REAL durable store shared by two
	// registries; the in-memory driver cannot express "one store, two Runtimes".
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// Phase 221 (D-366) — the expected-revision precondition on the durable
// agent-config spine.
//
// The four conformance rows in internal/agentcfg/conformance pin the CONTRACT
// every driver inherits. The tests here pin what only this driver can be asked
// about: the EVALUATION ORDER inside its read-modify-write, that a refusal
// touches neither the store nor the bus, and — the one the upstream ask
// actually demands — that two writers RACING one shared registry produce
// exactly one winner.

const condAgent = "agent-cond"

func condPayload(names ...string) agentcfg.ConfigPayload {
	return agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: names}}
}

// newBus builds a real in-memory EventBus (no fake — the refusal assertions
// need a bus that actually fans out).
func newBus(t *testing.T) events.EventBus {
	t.Helper()
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
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newRegistryOnBus builds a registry over a fresh in-memory StateStore and the
// supplied bus, so a test can subscribe before any write happens.
func newRegistryOnBus(t *testing.T, bus events.EventBus) agentcfg.Registry {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg
}

// newSharedStore builds a REAL file-backed SQLite StateStore in t.TempDir().
// The cross-process residual is about two Runtimes sharing one durable store,
// so the test uses the durable driver rather than a map.
func newSharedStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := statesqlite.New(config.StateConfig{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "agentcfg-shared.db"),
	})
	if err != nil {
		t.Fatalf("state sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// newRegistryOnStore builds a registry over an EXISTING StateStore, so two
// independently-constructed registries can share one store — the two-Runtime
// shape the cross-process residual is about.
func newRegistryOnStore(t *testing.T, st state.StateStore) agentcfg.Registry {
	t.Helper()
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: st, Bus: newBus(t)})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// TestSetRevision_ConditionalWrite_MatchingHashProceeds — the token matches
// the active revision's content hash, so the write lands and advances the
// chain exactly as an unconditional write would.
func TestSetRevision_ConditionalWrite_MatchingHashProceeds(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	base, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	next, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"),
		agentcfg.SetOptions{ExpectedContentHash: base.ContentHash})
	if err != nil {
		t.Fatalf("matching token must proceed: %v", err)
	}
	if next.ParentRevisionID != base.RevisionID {
		t.Fatalf("parent = %q, want %q", next.ParentRevisionID, base.RevisionID)
	}
}

// TestSetRevision_ConditionalWrite_MismatchRefused — the base moved, so the
// write is refused with the sentinel rather than silently reverting the
// writer that moved it.
func TestSetRevision_ConditionalWrite_MismatchRefused(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	stale, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("interleaved writer: %v", err)
	}

	_, err = reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "c"),
		agentcfg.SetOptions{ExpectedContentHash: stale.ContentHash})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale token: got %v, want ErrRevisionConflict", err)
	}
}

// TestSetRevision_ConditionalWrite_NoActiveRevisionRefused — a token supplied
// against an empty slot is a conflict, not a silent first write.
func TestSetRevision_ConditionalWrite_NoActiveRevisionRefused(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"),
		agentcfg.SetOptions{ExpectedContentHash: "0000000000000000000000000000000000000000000000000000000000000000"})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("token against empty slot: got %v, want ErrRevisionConflict", err)
	}
	if _, ok, aerr := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent); aerr != nil || ok {
		t.Fatalf("refused first write created an active revision: ok=%v err=%v", ok, aerr)
	}
}

// TestSetRevision_ConditionalWrite_RefusalPersistsNothing — after a refusal:
// the chain has not grown, the active pointer has not moved, and a subscriber
// on a REAL in-memory bus received NO agent.config.revised.
//
// The bus assertion is the one a store-only check cannot make: a driver that
// emitted the event before deciding would leave the store clean and still tell
// every projection the config had changed.
func TestSetRevision_ConditionalWrite_RefusalPersistsNothing(t *testing.T) {
	ctx := context.Background()

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
	reg := newRegistryOnBus(t, bus)
	q := agentQuad(condAgent)

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{agentcfg.EventTypeConfigRevised},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	stale, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("stale base: %v", err)
	}
	current, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("interleaved writer: %v", err)
	}
	// Drain the two legitimate events so anything left on the channel after
	// the refusal is attributable to the refusal.
	for i := range 2 {
		select {
		case <-sub.Events():
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d setup events, timed out waiting for #%d", 2, i+1)
		}
	}

	before, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "c"),
		agentcfg.SetOptions{ExpectedContentHash: stale.ContentHash}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale token: got %v, want ErrRevisionConflict", err)
	}

	after, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("refusal persisted a revision: %d -> %d", len(before), len(after))
	}
	active, ok, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active after refusal: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != current.RevisionID {
		t.Fatalf("refusal moved the active pointer: %q, want %q", active.RevisionID, current.RevisionID)
	}
	// No event. A publish that raced the assertion would arrive within the
	// bus's synchronous fan-out, so a short bounded wait is sufficient and is
	// not a synchronisation sleep.
	select {
	case ev := <-sub.Events():
		t.Fatalf("refusal published %s (revision %v) — nothing must be emitted", ev.Type, ev.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSetRevision_ConditionalWrite_PreconditionPrecedesIdempotentReset — THE
// ORDERING PIN.
//
// A caller holds a STALE token and submits a payload that happens to equal the
// CURRENT content. If the driver ran the idempotent-re-set short-circuit
// first, it would return the active revision and a `200`, telling the caller
// its write landed on the base it read — when the base had moved. That is a
// success that lies, and it is the §5 silent-degradation shape.
//
// This test is the ONLY guard that catches a transposition of the two blocks:
// every grep-for-presence assertion stays green through it, and so does every
// other conditional-write test, because the mismatch cases all submit content
// that DIFFERS from the current content.
func TestSetRevision_ConditionalWrite_PreconditionPrecedesIdempotentReset(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	stale, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("stale base: %v", err)
	}
	// A second writer moves the base. The content it lands is the content the
	// stale-token caller is about to submit.
	current, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("interleaved writer: %v", err)
	}
	if current.ContentHash == stale.ContentHash {
		t.Fatalf("fixture broken: the interleaved write did not move the content hash")
	}

	// Same payload as the CURRENT content — the idempotent re-set would answer
	// success here. The precondition must win.
	rev, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"),
		agentcfg.SetOptions{ExpectedContentHash: stale.ContentHash})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("a stale token with current-equal content answered %v (revision %q) — "+
			"the precondition is being evaluated AFTER the idempotent re-set short-circuit, "+
			"which converts a stale base into a misleading success", err, rev.RevisionID)
	}
}

// TestRollback_ConditionalWrite_MatchMismatchAndNoActive — the same three
// outcomes on the pointer-move door.
func TestRollback_ConditionalWrite_MatchMismatchAndNoActive(t *testing.T) {
	ctx := context.Background()
	q := agentQuad(condAgent)

	t.Run("NoActiveRevisionRefused", func(t *testing.T) {
		reg := newRegistry(t)
		_, err := reg.Rollback(ctx, q, condAgent, "01ZZZNONEXISTENT", agentcfg.ConfigScopeAgent,
			agentcfg.SetOptions{ExpectedContentHash: "deadbeef"})
		// The missing TARGET is resolved first and fails loud; the point is
		// that a token never converts a missing target into a success.
		if err == nil {
			t.Fatalf("rollback to a nonexistent revision must fail")
		}
	})

	t.Run("MatchingHashProceeds", func(t *testing.T) {
		reg := newRegistry(t)
		first, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		second, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		// The caller read `second` and rolls back to `first` under that base.
		if _, err := reg.Rollback(ctx, q, condAgent, first.RevisionID, agentcfg.ConfigScopeAgent,
			agentcfg.SetOptions{ExpectedContentHash: second.ContentHash}); err != nil {
			t.Fatalf("matching token on rollback must proceed: %v", err)
		}
		active, ok, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
		if err != nil || !ok || active.RevisionID != first.RevisionID {
			t.Fatalf("rollback did not repoint: %q want %q (ok=%v err=%v)", active.RevisionID, first.RevisionID, ok, err)
		}
	})

	t.Run("MismatchRefusedAndPointerUnmoved", func(t *testing.T) {
		reg := newRegistry(t)
		first, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a"), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		second, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("a", "b"), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		// The caller decided to roll back while looking at `first`, but a
		// third write landed after that decision.
		if _, err := reg.Rollback(ctx, q, condAgent, first.RevisionID, agentcfg.ConfigScopeAgent,
			agentcfg.SetOptions{ExpectedContentHash: first.ContentHash}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
			t.Fatalf("stale token on rollback: got %v, want ErrRevisionConflict", err)
		}
		active, ok, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
		if err != nil || !ok || active.RevisionID != second.RevisionID {
			t.Fatalf("refused rollback moved the pointer: %q want %q (ok=%v err=%v)",
				active.RevisionID, second.RevisionID, ok, err)
		}
	})
}

// TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins — THE RACE.
//
// N=128 goroutines all read the same active revision, all submit a conditional
// write carrying that SAME content hash, against ONE shared registry, under
// -race. Exactly one must be accepted and 127 refused.
//
// A test that serialised the writers would prove nothing — it would pass
// against a driver with no precondition at all, because each serialised writer
// would observe the base it expected. The writers here are released together
// from a single barrier and contend for the same slot.
//
// N.B. the driver's read-modify-write is NOT internally locked; the
// agent-config Service's per-owner striped mutex is what makes it atomic in
// production. This test therefore takes that same lock around each writer, so
// what it exercises is the production configuration rather than a shape no
// caller uses. The companion test in the protocol package drives the real
// Service and takes the real lock.
func TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	base, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("base"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("base: %v", err)
	}

	const n = 128
	var (
		ownerLock sync.Mutex // stands in for the Service's per-owner write lock
		start     = make(chan struct{})
		wg        sync.WaitGroup
		mu        sync.Mutex
		wins      int
		conflicts int
		others    []error
	)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			ownerLock.Lock()
			_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent,
				condPayload(fmt.Sprintf("writer-%d", i)),
				agentcfg.SetOptions{ExpectedContentHash: base.ContentHash})
			ownerLock.Unlock()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, agentcfg.ErrRevisionConflict):
				conflicts++
			default:
				others = append(others, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (the lost-update guarantee)", wins)
	}
	if conflicts != n-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, n-1)
	}

	// The winner's content is intact and the chain grew by exactly one.
	revs, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("revision chain = %d, want 2 (the base plus the single winner)", len(revs))
	}
	active, ok, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if active.ParentRevisionID != base.RevisionID {
		t.Fatalf("winner's parent = %q, want the base %q", active.ParentRevisionID, base.RevisionID)
	}
	names := active.Payload.SkillNames()
	if len(names) != 1 {
		t.Fatalf("winner's content was clobbered: %v", names)
	}
}

// TestSetRevision_ConcurrentUnconditionalWriters_AllSucceed — the §10 twin.
// The same N=128 with EMPTY tokens still behaves as it always has: every write
// is accepted and the last one wins. The feature did not tighten the
// unconditional path.
func TestSetRevision_ConcurrentUnconditionalWriters_AllSucceed(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	q := agentQuad(condAgent)

	const n = 128
	var (
		ownerLock sync.Mutex
		start     = make(chan struct{})
		wg        sync.WaitGroup
		mu        sync.Mutex
		failures  []error
	)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			ownerLock.Lock()
			_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent,
				condPayload(fmt.Sprintf("writer-%d", i)), agentcfg.SetOptions{})
			ownerLock.Unlock()
			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d unconditional writes were refused (the empty token must never refuse): %v",
			len(failures), failures[0])
	}
	revs, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != n {
		t.Fatalf("revision chain = %d, want %d (every unconditional write lands)", len(revs), n)
	}
}

// TestConditionalWrite_ConcurrentReuse_NoCrossOwnerBleed — the concurrent-reuse
// contract (D-025) for the conditional path: N=128 invocations across two
// tenants and two agents against ONE shared Registry, asserting no cross-owner
// bleed (each owner's conditional write sees only its own base), no
// cancellation cross-talk, and goroutines back to baseline after teardown.
func TestConditionalWrite_ConcurrentReuse_NoCrossOwnerBleed(t *testing.T) {
	ctx := context.Background()
	baseline := runtime.NumGoroutine()
	reg := newRegistry(t)

	type owner struct {
		q     identity.Quadruple
		agent string
		lock  sync.Mutex
		base  agentcfg.Revision
	}
	owners := []*owner{
		{q: identity.Quadruple{Identity: identity.Identity{TenantID: "t-a", UserID: reservedUser, SessionID: "agent-1"}}, agent: "agent-1"},
		{q: identity.Quadruple{Identity: identity.Identity{TenantID: "t-a", UserID: reservedUser, SessionID: "agent-2"}}, agent: "agent-2"},
		{q: identity.Quadruple{Identity: identity.Identity{TenantID: "t-b", UserID: reservedUser, SessionID: "agent-1"}}, agent: "agent-1"},
		{q: identity.Quadruple{Identity: identity.Identity{TenantID: "t-b", UserID: reservedUser, SessionID: "agent-2"}}, agent: "agent-2"},
	}
	for _, o := range owners {
		rev, err := reg.SetRevision(ctx, o.q, o.agent, agentcfg.ConfigScopeAgent,
			condPayload("base-"+o.q.TenantID+"-"+o.agent), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("seed %s/%s: %v", o.q.TenantID, o.agent, err)
		}
		o.base = rev
	}

	// One writer per owner cancels its own ctx mid-flight; the others must be
	// unaffected (no cancellation cross-talk).
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	const perOwner = 32 // 4 owners x 32 = 128 concurrent invocations
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  = map[string]int{}
		other []error
	)
	start := make(chan struct{})
	for _, o := range owners {
		for i := range perOwner {
			wg.Add(1)
			go func(o *owner, i int) {
				defer wg.Done()
				<-start
				useCtx := ctx
				if i == 0 {
					useCtx = cancelCtx
				}
				o.lock.Lock()
				_, err := reg.SetRevision(useCtx, o.q, o.agent, agentcfg.ConfigScopeAgent,
					condPayload(fmt.Sprintf("%s-%s-%d", o.q.TenantID, o.agent, i)),
					agentcfg.SetOptions{ExpectedContentHash: o.base.ContentHash})
				o.lock.Unlock()
				key := o.q.TenantID + "/" + o.agent
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins[key]++
				case errors.Is(err, agentcfg.ErrRevisionConflict), errors.Is(err, context.Canceled):
				default:
					other = append(other, err)
				}
			}(o, i)
		}
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	// Every owner resolved independently: exactly one winner each, so a
	// cancelled writer in one owner never consumed another owner's token, and
	// no owner's base was visible to another.
	for _, o := range owners {
		key := o.q.TenantID + "/" + o.agent
		if wins[key] != 1 {
			t.Fatalf("owner %s had %d winners, want exactly 1 — cross-owner bleed or a lost update", key, wins[key])
		}
		active, ok, err := reg.Active(ctx, o.q, o.agent, agentcfg.ConfigScopeAgent)
		if err != nil || !ok {
			t.Fatalf("Active %s: ok=%v err=%v", key, ok, err)
		}
		names := active.Payload.SkillNames()
		if len(names) != 1 {
			t.Fatalf("owner %s content = %v, want one name", key, names)
		}
		// The content is this owner's, never another's.
		if got := names[0]; len(got) < len(o.q.TenantID) || got[:len(o.q.TenantID)] != o.q.TenantID {
			t.Fatalf("owner %s read another owner's content: %q", key, got)
		}
	}

	// Goroutines back to baseline after the registry closes.
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if n := runtime.NumGoroutine(); n > baseline+2 {
		t.Fatalf("goroutines = %d, baseline %d — leak after close", n, baseline)
	}
}

// TestConditionalWrite_CrossProcessBoundIsDocumented — THE RESIDUAL, PINNED AS
// ABSENT.
//
// The precondition's atomicity comes from the agent-config service's per-owner
// write lock, which is a PROCESS-LOCAL mutex. Two independently-constructed
// registries over ONE shared StateStore are the cross-process shape, and the
// lost update STILL OCCURS there: each registry reads the active revision,
// both see the expected hash, and both write — the second overwriting the
// first, because the StateStore has no compare-and-swap and `saveActive` mints
// a fresh event id on every write.
//
// This test asserts the failure, not the fix. It exists so the gap is a
// recorded property with a guard on it rather than something a later reader
// assumes was handled: if a future change ever DID make the write
// cross-process safe, this test fails and must be replaced by its positive
// twin. The real fix is a conditional-write primitive on the StateStore
// interface across the persistence triad — a separate change with its own
// conformance rows.
func TestConditionalWrite_CrossProcessBoundIsDocumented(t *testing.T) {
	ctx := context.Background()
	q := agentQuad(condAgent)

	// One REAL StateStore, wrapped in a gate that can suspend ONE writer
	// between its precondition read and its save. The gate adds no behaviour —
	// every call is delegated to the real driver — it only chooses when a call
	// returns, which is what makes the interleaving deterministic instead of
	// hoping the scheduler produces it.
	gate := newSaveGate(newSharedStore(t))
	regA := newRegistryOnStore(t, gate)
	regB := newRegistryOnStore(t, gate)

	base, err := regA.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("base"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	token := agentcfg.SetOptions{ExpectedContentHash: base.ContentHash}

	// Writer B (the second "Runtime") starts a conditional write. Its
	// precondition reads the base and PASSES; the gate then suspends it just
	// before its first save.
	gate.arm()
	bDone := make(chan agentcfg.Revision, 1)
	bErr := make(chan error, 1)
	go func() {
		rev, err := regB.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("from-b"), token)
		if err != nil {
			bErr <- err
			return
		}
		bDone <- rev
	}()
	select {
	case <-gate.suspended:
	case err := <-bErr:
		t.Fatalf("writer B failed before reaching its save: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer B never reached its save — the gate did not arm")
	}

	// Writer A (the first "Runtime") completes an entire conditional write
	// under the SAME token. Its precondition passes — the base has not moved
	// yet, because B is suspended before its save.
	revA, err := regA.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("from-a"), token)
	if err != nil {
		t.Fatalf("writer A must be accepted (its base had not moved): %v", err)
	}

	// Release B. Its precondition was validated against a base that A has
	// since replaced, but the check already happened: there is no lock
	// spanning the two registries and no store-level CAS to catch it.
	gate.release()
	var revB agentcfg.Revision
	select {
	case revB = <-bDone:
	case err := <-bErr:
		t.Fatalf("writer B was refused (%v) — a cross-process guarantee now exists that "+
			"agentcfg.SetOptions, the generated Protocol reference and the operator skill all "+
			"state is ABSENT. Update those texts and replace this test with its positive twin.", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer B never completed")
	}

	// THE LOST UPDATE, OBSERVED. Both writers were told they succeeded, and
	// exactly one of the two revisions is active — A's write was silently
	// discarded with no error to anyone.
	active, ok, err := regA.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != revB.RevisionID {
		t.Fatalf("expected writer B's revision %q to be active (A's update lost); got %q",
			revB.RevisionID, active.RevisionID)
	}
	if active.RevisionID == revA.RevisionID {
		t.Fatalf("writer A's revision survived — the cross-process lost update no longer reproduces")
	}
	// Both writes carried the SAME expected base, and both were accepted. That
	// is the definition of the guarantee being absent across processes.
	if revA.ParentRevisionID != base.RevisionID || revB.ParentRevisionID != base.RevisionID {
		t.Fatalf("both writers should have descended from the same base %q: A=%q B=%q",
			base.RevisionID, revA.ParentRevisionID, revB.ParentRevisionID)
	}
}

// saveGate wraps a real StateStore and can suspend exactly one Save call, so a
// test can interleave two registries' read-modify-writes deterministically. It
// re-implements nothing: every method delegates.
type saveGate struct {
	state.StateStore
	armed     chan struct{} // buffered(1); a token means "suspend the next Save"
	suspended chan struct{}
	proceed   chan struct{}
}

func newSaveGate(inner state.StateStore) *saveGate {
	return &saveGate{
		StateStore: inner,
		armed:      make(chan struct{}, 1),
		suspended:  make(chan struct{}, 1),
		proceed:    make(chan struct{}),
	}
}

// arm makes the NEXT Save block until release is called.
func (g *saveGate) arm() { g.armed <- struct{}{} }

// release lets the suspended Save proceed.
func (g *saveGate) release() { close(g.proceed) }

func (g *saveGate) Save(ctx context.Context, r state.StateRecord) error {
	select {
	case <-g.armed:
		g.suspended <- struct{}{}
		<-g.proceed
	default:
	}
	return g.StateStore.Save(ctx, r)
}
