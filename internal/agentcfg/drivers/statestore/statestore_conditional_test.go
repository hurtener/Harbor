package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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

// newSharedStores builds two independent SQLite driver handles over one file.
// Separate handles (and therefore separate database/sql pools) model the
// persistence boundary two Runtime processes actually cross.
func newSharedStores(t *testing.T) (state.StateStore, state.StateStore) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "agentcfg-shared.db")
	left, err := statesqlite.New(config.StateConfig{
		Driver: "sqlite",
		DSN:    dsn,
	})
	if err != nil {
		t.Fatalf("open left state sqlite: %v", err)
	}
	t.Cleanup(func() { _ = left.Close(context.Background()) })
	right, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open right state sqlite: %v", err)
	}
	t.Cleanup(func() { _ = right.Close(context.Background()) })
	return left, right
}

// newSharedStore remains the single-handle fixture for fault-injection tests
// that do not model an inter-process boundary.
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

// TestSignedOAuthMCPActivationFence_HidesCandidateAndRejectsForeignWriters
// proves the signed-capability fence at the real Registry boundary. The candidate pointer
// is physically durable before publication, yet Active exposes only the prior
// authority and every unmarked writer fails closed across that interval.
func TestSignedOAuthMCPActivationFence_HidesCandidateAndRejectsForeignWriters(t *testing.T) {
	ctx := context.Background()
	reg, st := newRegistryWithStore(t)
	q := agentQuad(condAgent)
	prior, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("prior"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed prior: %v", err)
	}
	pair := &agentcfg.SignedOAuthMCPPair{ProviderName: "provider", Broker: "broker", Audience: "aud", CapabilityRevision: "v1", URLDigest: "digest", OwnerAgentID: condAgent, AuthorityOperationKind: "operation-kind"}
	candidatePayload := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"candidate"}}, SignedOAuthMCPPair: pair}
	candidateHash, err := agentcfg.ContentHash(agentcfg.NormalizePayload(candidatePayload))
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(st)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fences.Begin(ctx, tenantT, condAgent, pair.AuthorityOperationKind, "fingerprint", candidateHash, prior.RevisionID)
	if err != nil {
		t.Fatalf("begin fence: %v", err)
	}
	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("foreign"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("foreign writer = %v, want ErrSignedCapabilityPending", err)
	}
	candidate, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), q, condAgent, agentcfg.ConfigScopeAgent, candidatePayload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("owning candidate write: %v", err)
	}
	visible, set, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !set || visible.RevisionID != prior.RevisionID {
		t.Fatalf("pending Active = (%q, %v, %v), want prior %q", visible.RevisionID, set, err, prior.RevisionID)
	}
	if _, err := fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, candidate.RevisionID); err != nil {
		t.Fatalf("commit fence: %v", err)
	}
	visible, set, err = reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !set || visible.RevisionID != candidate.RevisionID {
		t.Fatalf("committed Active = (%q, %v, %v), want candidate %q", visible.RevisionID, set, err, candidate.RevisionID)
	}
}

func TestSignedOAuthMCPPair_GenericWritesCarryForwardAndRollbackCannotMutate(t *testing.T) {
	ctx := context.Background()
	reg, _ := newRegistryWithStore(t)
	q := agentQuad(condAgent)
	prior, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("prior"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pair := &agentcfg.SignedOAuthMCPPair{
		ProviderName: "provider", Broker: "broker", Audience: "audience", CapabilityRevision: "v1",
		URLDigest: "url-digest", SinkDigest: "sink-digest", Sink: "https://mcp.example.test:8443",
		Connection:   agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://mcp.example.test:8443/mcp", ToolAllowlist: []string{"read"}},
		OwnerAgentID: condAgent, OwnerUserID: q.UserID, OwnerSessionID: q.SessionID, AuthorityOperationKind: "pair-operation",
	}
	pairPayload := condPayload("paired")
	pairPayload.SignedOAuthMCPPair = pair
	pairRevision, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), q, condAgent, agentcfg.ConfigScopeAgent, pairPayload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed pair: %v", err)
	}
	carried, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("generic-edit"), agentcfg.SetOptions{})
	if err != nil || !reflect.DeepEqual(carried.Payload.SignedOAuthMCPPair, pair) {
		t.Fatalf("generic omission did not carry immutable pair: pair=%+v err=%v", carried.Payload.SignedOAuthMCPPair, err)
	}
	altered := carried.Payload
	mutated := *pair
	mutated.CapabilityRevision = "v2"
	altered.SignedOAuthMCPPair = &mutated
	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, altered, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("generic pair alteration = %v, want replay refusal", err)
	}
	if _, err := reg.Rollback(ctx, q, condAgent, prior.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("generic rollback removed pair: %v", err)
	}
	removedPayload := carried.Payload
	removedPayload.SignedOAuthMCPPair = nil
	removed, err := reg.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), q, condAgent, agentcfg.ConfigScopeAgent, removedPayload, agentcfg.SetOptions{})
	if err != nil || removed.Payload.SignedOAuthMCPPair != nil {
		t.Fatalf("paired removal: pair=%+v err=%v", removed.Payload.SignedOAuthMCPPair, err)
	}
	if _, err := reg.Rollback(ctx, q, condAgent, pairRevision.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("generic rollback resurrected pair: %v", err)
	}
}

// TestSignedOAuthMCPActivationFence_SQLiteTwoRuntimeRecovery pins the restart
// contract against two independent real SQLite handles. The second runtime
// cannot observe or mutate the physically-landed candidate until the first
// operation writes the exact committed fence receipt.
func TestSignedOAuthMCPActivationFence_SQLiteTwoRuntimeRecovery(t *testing.T) {
	ctx := context.Background()
	leftStore, rightStore := newSharedStores(t)
	left := newRegistryOnStore(t, leftStore)
	right := newRegistryOnStore(t, rightStore)
	q := agentQuad("agent-fence-sqlite")
	prior, err := left.SetRevision(ctx, q, "agent-fence-sqlite", agentcfg.ConfigScopeAgent, condPayload("prior"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed prior: %v", err)
	}
	pair := &agentcfg.SignedOAuthMCPPair{ProviderName: "provider", Broker: "broker", Audience: "aud", CapabilityRevision: "v1", URLDigest: "digest", OwnerAgentID: "agent-fence-sqlite", AuthorityOperationKind: "sqlite-operation"}
	payload := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"candidate"}}, SignedOAuthMCPPair: pair}
	hash, err := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
	if err != nil {
		t.Fatal(err)
	}
	fences, err := agentcfg.NewSignedOAuthMCPActivationFenceStore(leftStore)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fences.Begin(ctx, tenantT, "agent-fence-sqlite", pair.AuthorityOperationKind, "fingerprint", hash, prior.RevisionID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	candidate, err := left.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), q, "agent-fence-sqlite", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	visible, set, err := right.Active(ctx, q, "agent-fence-sqlite", agentcfg.ConfigScopeAgent)
	if err != nil || !set || visible.RevisionID != prior.RevisionID {
		t.Fatalf("restart Active = (%q, %v, %v), want prior %q", visible.RevisionID, set, err, prior.RevisionID)
	}
	if _, err := right.SetRevision(ctx, q, "agent-fence-sqlite", agentcfg.ConfigScopeAgent, condPayload("foreign"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("foreign restart writer = %v, want pending", err)
	}
	if _, err := fences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, candidate.RevisionID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	visible, set, err = right.Active(ctx, q, "agent-fence-sqlite", agentcfg.ConfigScopeAgent)
	if err != nil || !set || visible.RevisionID != candidate.RevisionID {
		t.Fatalf("committed restart Active = (%q, %v, %v), want candidate %q", visible.RevisionID, set, err, candidate.RevisionID)
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

// TestConditionalWrite_SharedSQLiteTwoRegistries_OneWinner proves the durable
// StateStore predicate rather than the process-local protocol lock decides the
// race. The two registries use independent SQLite driver handles and model
// independent Runtime processes.
func TestConditionalWrite_SharedSQLiteTwoRegistries_OneWinner(t *testing.T) {
	ctx := context.Background()
	q := agentQuad(condAgent)

	// Separate REAL StateStore handles. The gate wraps B's handle and can suspend ONE writer
	// between its precondition read and its save. The gate adds no behaviour —
	// every call is delegated to the real driver — it only chooses when a call
	// returns, which is what makes the interleaving deterministic instead of
	// hoping the scheduler produces it.
	left, right := newSharedStores(t)
	regA := newRegistryOnStore(t, left)
	gate := newSaveGate(right)
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

	// Release B. Its original content-hash check passed, but SaveIf checks the
	// exact active-pointer generation again at the durable linearization point.
	gate.release()
	select {
	case revB := <-bDone:
		t.Fatalf("writer B unexpectedly succeeded with revision %q", revB.RevisionID)
	case err := <-bErr:
		if !errors.Is(err, agentcfg.ErrRevisionConflict) {
			t.Fatalf("writer B error = %v, want ErrRevisionConflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer B never completed")
	}

	// A remains active and B's rejected candidate was never published.
	active, ok, err := regA.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if active.RevisionID != revA.RevisionID {
		t.Fatalf("active revision = %q, want winning writer A %q", active.RevisionID, revA.RevisionID)
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

// saveIfBarrierStore stops the first conditional active-pointer publication
// after SetRevision has durably saved its immutable candidate. It exposes the
// StateStore CAS linearization window without reimplementing persistence.
type saveIfBarrierStore struct {
	state.StateStore
	entered     chan struct{}
	proceed     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newSaveIfBarrierStore(inner state.StateStore) *saveIfBarrierStore {
	return &saveIfBarrierStore{StateStore: inner, entered: make(chan struct{}, 1), proceed: make(chan struct{})}
}

func (s *saveIfBarrierStore) release() { s.releaseOnce.Do(func() { close(s.proceed) }) }

func (s *saveIfBarrierStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.enteredOnce.Do(func() { s.entered <- struct{}{} })
	<-s.proceed
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func newSaveGate(inner state.StateStore) *saveGate {
	return &saveGate{
		StateStore: inner,
		armed:      make(chan struct{}, 1),
		suspended:  make(chan struct{}, 1),
		proceed:    make(chan struct{}),
	}
}

type saveIfFaultStore struct {
	state.StateStore
	saveIfErr error
	deleteErr error
}

func (s *saveIfFaultStore) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return s.saveIfErr
}

func (s *saveIfFaultStore) Delete(ctx context.Context, q identity.Quadruple, kind string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.StateStore.Delete(ctx, q, kind)
}

type expectationRecordingStore struct {
	state.StateStore
	expectationCounts []int
}

type loadFaultStore struct {
	state.StateStore
	err error
}

func (s *loadFaultStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, s.err
}

func (s *expectationRecordingStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.expectationCounts = append(s.expectationCounts, len(expectations))
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestSetRevision_SaveIfConditionFailureRemovesCandidate(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	store := &saveIfFaultStore{StateStore: base, saveIfErr: state.ErrConditionFailed}
	reg := newRegistryOnStore(t, store)
	q := agentQuad(condAgent)

	_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("SetRevision = %v, want ErrRevisionConflict", err)
	}
	revs, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("condition failure retained candidate revisions: %+v", revs)
	}
}

func TestSetRevision_LifecycleTerminalOrCorruptNeverOverwritten(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		bytes []byte
		want  error
	}{
		{name: "terminal", bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`), want: agentcfg.ErrAgentRetired},
		{name: "corrupt", bytes: []byte(`{"schema":1,"revision_id":"active"}`), want: agentcfg.ErrStateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSharedStore(t)
			reg := newRegistryOnStore(t, st)
			q := agentQuad(condAgent)
			userQ := identity.Quadruple{Identity: identity.Identity{TenantID: q.TenantID, UserID: "user", SessionID: "session"}}
			if err := st.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: agentQuad(condAgent), Kind: "agentcfg.active", Bytes: tc.bytes}); err != nil {
				t.Fatalf("seed %s lifecycle: %v", tc.name, err)
			}
			if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{}); !errors.Is(err, tc.want) {
				t.Fatalf("SetRevision after %s = %v, want %v", tc.name, err, tc.want)
			}
			if _, err := reg.SetRevision(ctx, userQ, condAgent, agentcfg.ConfigScopeUser, condPayload("candidate"), agentcfg.SetOptions{}); !errors.Is(err, tc.want) {
				t.Fatalf("user SetRevision after %s = %v, want %v", tc.name, err, tc.want)
			}
			if _, _, err := reg.Active(ctx, q, condAgent, agentcfg.ConfigScopeAgent); !errors.Is(err, tc.want) {
				t.Fatalf("agent Active after %s = %v, want %v", tc.name, err, tc.want)
			}
			if _, _, err := reg.Active(ctx, userQ, condAgent, agentcfg.ConfigScopeUser); !errors.Is(err, tc.want) {
				t.Fatalf("user Active after %s = %v, want %v", tc.name, err, tc.want)
			}
			after, err := st.Load(ctx, agentQuad(condAgent), "agentcfg.active")
			if err != nil {
				t.Fatalf("load %s lifecycle: %v", tc.name, err)
			}
			if string(after.Bytes) != string(tc.bytes) {
				t.Fatalf("SetRevision overwrote %s lifecycle: got %s want %s", tc.name, after.Bytes, tc.bytes)
			}
			revs, err := reg.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
			if err != nil {
				t.Fatalf("list revisions: %v", err)
			}
			if len(revs) != 0 {
				t.Fatalf("refused %s write retained revisions: %+v", tc.name, revs)
			}
		})
	}
}

func TestSetRevision_FirstWriteCASCannotOverwriteConcurrentTerminalOrCorruptLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{name: "terminal", bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)},
		{name: "corrupt", bytes: []byte(`{"schema":1,"revision_id":"active"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := newSharedStores(t)
			gate := newSaveIfBarrierStore(left)
			t.Cleanup(gate.release)
			writer := newRegistryOnStore(t, gate)
			observer := newRegistryOnStore(t, right)
			q := agentQuad(condAgent)

			result := make(chan error, 1)
			go func() {
				_, err := writer.SetRevision(context.Background(), q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision})
				result <- err
			}()
			<-gate.entered // lifecycleExpectation saw absent and candidate revision is durable.

			// Phase 234 will publish this through its retirement CAS verb. A
			// separate StateStore handle models that independent runtime at the
			// exact active-slot generation boundary today.
			if err := right.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: agentQuad(condAgent), Kind: "agentcfg.active", Bytes: tc.bytes}); err != nil {
				t.Fatalf("publish concurrent %s lifecycle: %v", tc.name, err)
			}
			gate.release()
			if err := <-result; !errors.Is(err, agentcfg.ErrRevisionConflict) {
				t.Fatalf("first-write after concurrent %s = %v, want ErrRevisionConflict", tc.name, err)
			}
			after, err := right.Load(ctx, agentQuad(condAgent), "agentcfg.active")
			if err != nil {
				t.Fatalf("load concurrent %s lifecycle: %v", tc.name, err)
			}
			if string(after.Bytes) != string(tc.bytes) {
				t.Fatalf("first-write overwrote concurrent %s: got %s want %s", tc.name, after.Bytes, tc.bytes)
			}
			revs, err := observer.ListRevisions(ctx, q, condAgent, agentcfg.ConfigScopeAgent, 0)
			if err != nil {
				t.Fatalf("list candidate revisions: %v", err)
			}
			if len(revs) != 0 {
				t.Fatalf("conflicted first-write retained %d candidate revision(s): %+v", len(revs), revs)
			}
		})
	}
}

func TestRollback_LifecycleTerminalOrCorruptNeverOverwritten(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		bytes []byte
		want  error
	}{
		{name: "terminal", bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`), want: agentcfg.ErrAgentRetired},
		{name: "corrupt", bytes: []byte(`{"schema":1,"revision_id":"active"}`), want: agentcfg.ErrStateUnavailable},
	} {
		for _, sc := range []struct {
			name  string
			scope agentcfg.ConfigScope
		}{{name: "agent", scope: agentcfg.ConfigScopeAgent}, {name: "user", scope: agentcfg.ConfigScopeUser}} {
			t.Run(tc.name+"/"+sc.name, func(t *testing.T) {
				st := newSharedStore(t)
				reg := newRegistryOnStore(t, st)
				agentQ := agentQuad(condAgent)
				userQ := identity.Quadruple{Identity: identity.Identity{TenantID: agentQ.TenantID, UserID: "rollback-user", SessionID: "rollback-session"}}
				base, err := reg.SetRevision(ctx, agentQ, condAgent, agentcfg.ConfigScopeAgent, condPayload("agent-base"), agentcfg.SetOptions{})
				if err != nil {
					t.Fatalf("seed agent revision: %v", err)
				}
				id, target := agentQ, base.RevisionID
				if sc.scope == agentcfg.ConfigScopeUser {
					userBase, setErr := reg.SetRevision(ctx, userQ, condAgent, agentcfg.ConfigScopeUser, condPayload("user-base"), agentcfg.SetOptions{})
					if setErr != nil {
						t.Fatalf("seed user revision: %v", setErr)
					}
					id, target = userQ, userBase.RevisionID
				}
				if err := st.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: agentQuad(condAgent), Kind: "agentcfg.active", Bytes: tc.bytes}); err != nil {
					t.Fatalf("seed %s lifecycle: %v", tc.name, err)
				}
				if _, err := reg.Rollback(ctx, id, condAgent, target, sc.scope, agentcfg.SetOptions{}); !errors.Is(err, tc.want) {
					t.Fatalf("Rollback after %s = %v, want %v", tc.name, err, tc.want)
				}
				after, err := st.Load(ctx, agentQuad(condAgent), "agentcfg.active")
				if err != nil {
					t.Fatalf("load %s lifecycle: %v", tc.name, err)
				}
				if string(after.Bytes) != string(tc.bytes) {
					t.Fatalf("Rollback overwrote %s lifecycle: got %s want %s", tc.name, after.Bytes, tc.bytes)
				}
			})
		}
	}
}

func TestSetRevision_SaveIfConditionFailureReportsCleanupFailure(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	store := &saveIfFaultStore{StateStore: base, saveIfErr: state.ErrConditionFailed, deleteErr: errors.New("delete unavailable")}
	reg := newRegistryOnStore(t, store)
	q := agentQuad(condAgent)

	_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{})
	if !errors.Is(err, agentcfg.ErrRevisionConflict) || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("SetRevision = %v, want revision conflict with cleanup failure", err)
	}
}

func TestSetRevision_SaveIfStorageFailureIsStateUnavailable(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	store := &saveIfFaultStore{StateStore: base, saveIfErr: errors.New("conditional write unavailable")}
	reg := newRegistryOnStore(t, store)
	q := agentQuad(condAgent)

	_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{})
	if !errors.Is(err, agentcfg.ErrStateUnavailable) {
		t.Fatalf("SetRevision = %v, want ErrStateUnavailable", err)
	}
}

func TestSetRevision_ActiveExpectationLoadFailureIsStateUnavailable(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	reg := newRegistryOnStore(t, &loadFaultStore{StateStore: base, err: errors.New("state read unavailable")})
	q := agentQuad(condAgent)

	_, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("candidate"), agentcfg.SetOptions{})
	if !errors.Is(err, agentcfg.ErrStateUnavailable) {
		t.Fatalf("SetRevision = %v, want ErrStateUnavailable", err)
	}
}

func TestRollback_SaveIfConditionFailureIsRevisionConflict(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	regular := newRegistryOnStore(t, base)
	q := agentQuad(condAgent)
	first, err := regular.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("one"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := regular.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("two"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	faulted := newRegistryOnStore(t, &saveIfFaultStore{StateStore: base, saveIfErr: state.ErrConditionFailed})
	if _, err := faulted.Rollback(ctx, q, condAgent, first.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("Rollback = %v, want ErrRevisionConflict", err)
	}
}

func TestSetRevision_UserScopeConditionsBothPointers(t *testing.T) {
	ctx := context.Background()
	base := newSharedStore(t)
	store := &expectationRecordingStore{StateStore: base}
	reg := newRegistryOnStore(t, store)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-user-conditions", UserID: "user-conditions", SessionID: "session-conditions"}}
	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeAgent, condPayload("agent"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("agent SetRevision: %v", err)
	}
	if _, err := reg.SetRevision(ctx, q, condAgent, agentcfg.ConfigScopeUser, condPayload("user"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("user SetRevision: %v", err)
	}
	if got, want := store.expectationCounts, []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveIf expectation counts = %v, want %v", got, want)
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

func (g *saveGate) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	select {
	case <-g.armed:
		g.suspended <- struct{}{}
		<-g.proceed
	default:
	}
	return g.StateStore.SaveIf(ctx, expectations, next)
}
