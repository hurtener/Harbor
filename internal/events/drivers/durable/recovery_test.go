package durable_test

import (
	"context"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// kindHeadWire is the StateStore Kind the durable driver writes its
// per-session head record under. Duplicated here as the wire literal
// (the package constant is unexported) so the external test can pre-seed
// a corrupt head record and prove fail-loud recovery.
const kindHeadWire = "events.durable.head"

// replaySeqs replays a session and returns the assigned sequence
// numbers, in replay order.
func replaySeqs(t *testing.T, rp events.Replayer, id identity.Quadruple) []uint64 {
	t.Helper()
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay %q: %v", id.SessionID, err)
	}
	seqs := make([]uint64, len(got))
	for i, ev := range got {
		seqs[i] = ev.Sequence
	}
	return seqs
}

// restart closes bus1 and constructs a fresh durable bus over the SAME
// store — the Runtime-restart scenario. The new bus rehydrates its
// sequence counter from the persisted head records at construction.
func restart(t *testing.T, bus events.EventBus, store state.StateStore) (events.EventBus, events.Replayer) {
	t.Helper()
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	return newDurableBus(t, store)
}

// TestDurable_PublishAfterRestart_NoSequenceCollision is the core
// regression: the prior TestDurable_ReplayAcrossRestart_NoGaps only
// replayed PRE-restart events, so it never exercised a post-restart
// Publish. Here events are published AFTER the restart and must extend
// the sequence strictly past the pre-restart high-water mark — no
// collision, strictly monotonic, gap-free.
func TestDurable_PublishAfterRestart_NoSequenceCollision(t *testing.T) {
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	bus1, _ := newDurableBus(t, store)
	publishN(t, bus1, id, 8) // seq 1..8

	bus2, rp2 := restart(t, bus1, store)
	publishN(t, bus2, id, 5) // seq 9..13

	seqs := replaySeqs(t, rp2, id)
	if len(seqs) != 13 {
		t.Fatalf("expected 13 events after restart+republish, got %d (%v)", len(seqs), seqs)
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("sequence collision/gap: position %d has Sequence %d (want %d); full=%v", i, s, i+1, seqs)
		}
	}
}

// TestDurable_PublishAfterRestart_ReconnectAtHighWaterMark_NoSilentSkip
// proves the resumability contract: a client reconnecting with a cursor
// at the pre-restart high-water mark receives EVERY post-restart event,
// none silently skipped.
func TestDurable_PublishAfterRestart_ReconnectAtHighWaterMark_NoSilentSkip(t *testing.T) {
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	bus1, _ := newDurableBus(t, store)
	publishN(t, bus1, id, 8) // pre-restart max = 8

	bus2, rp2 := restart(t, bus1, store)
	publishN(t, bus2, id, 5) // post-restart seq 9..13

	got, err := rp2.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: 8}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay from high-water mark: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("reconnect at seq=8 must return the 5 post-restart events, got %d", len(got))
	}
	for i, ev := range got {
		if ev.Sequence != uint64(9+i) {
			t.Fatalf("silent skip: position %d has Sequence %d (want %d)", i, ev.Sequence, 9+i)
		}
	}
}

// TestDurable_TransientNoticeIsHighestPreRestartSeq_NoPostRestartSkip
// closes the SAME silent-skip class for transient notices: a notice that
// is the last thing emitted before restart must (a) carry no replay
// position (Sequence == 0) and not advance nextSeq, and (b) never cause a
// post-restart skip for a client replaying from the last PERSISTED
// sequence (the cursor it could actually hold).
func TestDurable_TransientNoticeIsHighestPreRestartSeq_NoPostRestartSkip(t *testing.T) {
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")

	bus1, rp1 := newDurableBus(t, store)

	sub, err := bus1.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	recv := func(what string) events.Event {
		select {
		case ev := <-sub.Events():
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return events.Event{}
		}
	}

	// e1 — persisted, seq 1.
	publishN(t, bus1, id, 1)
	if e1 := recv("e1"); e1.Sequence != 1 {
		t.Fatalf("e1 expected Sequence 1, got %d", e1.Sequence)
	}

	// Trigger a transient audit.admin_scope_used notice via an admin
	// Replay — it is the last thing emitted before restart.
	adminFilter := events.Filter{Admin: true, Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	if _, err := rp1.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, adminFilter); err != nil {
		t.Fatalf("admin replay (trigger notice): %v", err)
	}
	notice := recv("admin_scope_used notice")
	if notice.Type != events.EventTypeAdminScopeUsed {
		t.Fatalf("expected admin_scope_used notice, got type %q", notice.Type)
	}
	if notice.Sequence != 0 {
		t.Fatalf("transient notice must carry the non-replayable sentinel Sequence 0, got %d", notice.Sequence)
	}

	// e2 — persisted. If the notice had advanced nextSeq, this would be
	// seq 3; it must be seq 2 (lastPersisted + 1).
	publishN(t, bus1, id, 1)
	if e2 := recv("e2"); e2.Sequence != 2 {
		t.Fatalf("nextSeq was advanced by a transient notice: e2 got Sequence %d (want 2)", e2.Sequence)
	}

	// Restart. Recovery floors nextSeq at the max PERSISTED sequence (2);
	// the transient notice is absent from the head records.
	bus2, rp2 := restart(t, bus1, store)
	publishN(t, bus2, id, 1) // post-restart, must be seq 3

	// A client could only ever hold Last-Event-ID = 2 (the notice had no
	// id: line). Replay from 2 must return the post-restart event.
	got, err := rp2.Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: 2}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay from last persisted: %v", err)
	}
	if len(got) != 1 || got[0].Sequence != 3 {
		t.Fatalf("post-restart skip: expected exactly the seq=3 event, got %v", replaySeqsOf(got))
	}
}

func replaySeqsOf(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, ev := range evs {
		out[i] = ev.Sequence
	}
	return out
}

// TestDurable_RecoverNextSeq_GlobalAcrossSessions proves the recovered
// floor is the CROSS-session maximum, not a per-session value. sA holds
// seq 1..3, sB holds seq 4..8 (sequences are global). After restart a
// publish to sA must receive seq 9 — a per-session recovery would have
// (wrongly) given seq 4.
func TestDurable_RecoverNextSeq_GlobalAcrossSessions(t *testing.T) {
	store := newInmemStore(t)
	idA := quad("t1", "u1", "sA")
	idB := quad("t1", "u1", "sB")

	bus1, _ := newDurableBus(t, store)
	publishN(t, bus1, idA, 3) // seq 1,2,3
	publishN(t, bus1, idB, 5) // seq 4,5,6,7,8 (global max = 8)

	bus2, rp2 := restart(t, bus1, store)
	publishN(t, bus2, idA, 1)

	seqsA := replaySeqs(t, rp2, idA)
	last := seqsA[len(seqsA)-1]
	if last != 9 {
		t.Fatalf("expected post-restart sA publish to receive global-max+1 = 9, got %d (sA=%v)", last, seqsA)
	}
}

// TestDurable_RecoverNextSeq_EmptyLog_StartsAtZero: a fresh store ⇒ the
// first published sequence is 1 (recovery of an empty log is a no-op).
func TestDurable_RecoverNextSeq_EmptyLog_StartsAtZero(t *testing.T) {
	store := newInmemStore(t)
	id := quad("t1", "u1", "s1")
	bus, _ := newDurableBus(t, store)
	publishN(t, bus, id, 1)
	if seqs := replaySeqs(t, bus.(events.Replayer), id); len(seqs) != 1 || seqs[0] != 1 {
		t.Fatalf("empty-log recovery: first sequence must be 1, got %v", seqs)
	}
}

// TestDurable_RecoverNextSeq_FailsLoudOnScanError: a StateStore whose
// ListKind returns an error makes durable.New fail the boot — it never
// silently starts the counter at 0 (CLAUDE.md §13).
func TestDurable_RecoverNextSeq_FailsLoudOnScanError(t *testing.T) {
	sentinel := errSentinel("scan exploded")
	_, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), &listFailingStore{listErr: sentinel})
	if err == nil {
		t.Fatalf("expected durable.New to fail loudly on a ListKind scan error, got nil")
	}
	if !strings.Contains(err.Error(), "recover sequence counter") {
		t.Fatalf("expected a wrapped recovery error, got %v", err)
	}
}

// TestDurable_RecoverNextSeq_FailsLoudOnUndecodableHead: a corrupt
// persisted head record makes durable.New fail the boot with a decode
// error (no silent zero start).
func TestDurable_RecoverNextSeq_FailsLoudOnUndecodableHead(t *testing.T) {
	store := newInmemStore(t)
	if err := store.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: quad("t1", "u1", "s1"),
		Kind:     kindHeadWire,
		Bytes:    []byte("{ not valid json"),
	}); err != nil {
		t.Fatalf("seed corrupt head: %v", err)
	}
	_, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err == nil {
		t.Fatalf("expected durable.New to fail loudly on an undecodable head record, got nil")
	}
	if !strings.Contains(err.Error(), "decode head record") {
		t.Fatalf("expected a wrapped decode error, got %v", err)
	}
}

// TestDurable_BestEffort_SkipsRecovery: with no StateStore there is
// nothing to rehydrate; construction must succeed (it must not attempt a
// nil-store scan) and the first published sequence is 1. It also pins the
// "in any mode" half of the transient-notice contract: a notice carries
// Sequence == 0 and does NOT advance the counter even in best-effort mode
// (publishInternal has no mode branch).
func TestDurable_BestEffort_SkipsRecovery(t *testing.T) {
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), nil)
	if err != nil {
		t.Fatalf("best-effort durable.New must not attempt recovery: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := quad("t1", "u1", "s1")
	rp := bus.(events.Replayer)

	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	recv := func(what string) events.Event {
		select {
		case ev := <-sub.Events():
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return events.Event{}
		}
	}

	publishN(t, bus, id, 1)
	if e1 := recv("e1"); e1.Sequence != 1 {
		t.Fatalf("best-effort first sequence must be 1, got %d", e1.Sequence)
	}

	// Transient notice in best-effort mode: Sequence 0, no advance.
	adminFilter := events.Filter{Admin: true, Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	if _, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, adminFilter); err != nil {
		t.Fatalf("admin replay (trigger notice): %v", err)
	}
	if n := recv("admin_scope_used notice"); n.Sequence != 0 {
		t.Fatalf("best-effort transient notice must carry Sequence 0, got %d", n.Sequence)
	}

	// Next real publish still increments by exactly 1 (the notice did not
	// advance the counter).
	publishN(t, bus, id, 1)
	if e2 := recv("e2"); e2.Sequence != 2 {
		t.Fatalf("best-effort: transient notice advanced the counter — e2 got Sequence %d (want 2)", e2.Sequence)
	}

	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("best-effort Replay: %v", err)
	}
	if seqs := replaySeqsOf(got); len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("best-effort persisted-replay sequences must be [1 2], got %v", seqs)
	}
}

// TestDurable_RecoverNextSeq_AssertsMaintenanceScope pins that the
// recovery scan asserts ListScope{MaintenanceScoped: true}: a store double
// that rejects the zero scope (mirroring the real drivers'
// ErrMaintenanceScopeRequired fail-closed) must NOT fail the boot, proving
// the recovery sets the elevated claim explicitly.
func TestDurable_RecoverNextSeq_AssertsMaintenanceScope(t *testing.T) {
	rec := &scopeRecordingStore{}
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), rec)
	if err != nil {
		t.Fatalf("durable.New with a maintenance-scope-asserting store failed: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	if !rec.sawMaintenanceScope {
		t.Fatalf("recovery scan did not assert ListScope{MaintenanceScoped: true}")
	}
}

// ---------------------------------------------------------------------------
// Test doubles for fail-loud recovery
// ---------------------------------------------------------------------------

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// listFailingStore is a StateStore whose ListKind always fails. The
// existing failingStore.ListKind returns (nil, nil) — it would let
// recovery succeed against an empty log, so it cannot exercise the
// scan-error path.
type listFailingStore struct{ listErr error }

func (s *listFailingStore) Save(context.Context, state.StateRecord) error { return nil }
func (s *listFailingStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (s *listFailingStore) LoadByEventID(context.Context, state.EventID) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (s *listFailingStore) Delete(context.Context, identity.Quadruple, string) error { return nil }
func (s *listFailingStore) DeleteScope(context.Context, identity.Identity) (int, error) {
	return 0, nil
}
func (s *listFailingStore) ListKind(context.Context, state.ListScope, string) ([]state.StateRecord, error) {
	return nil, s.listErr
}
func (s *listFailingStore) ListKindForIdentity(context.Context, identity.Quadruple, string) ([]state.StateRecord, error) {
	return nil, s.listErr
}
func (s *listFailingStore) Close(context.Context) error { return nil }

// scopeRecordingStore records whether ListKind was called with the
// elevated maintenance scope, and rejects the zero scope exactly as the
// real drivers do (state.ErrMaintenanceScopeRequired) so a recovery that
// forgot the claim would fail the boot loudly.
type scopeRecordingStore struct{ sawMaintenanceScope bool }

func (s *scopeRecordingStore) Save(context.Context, state.StateRecord) error { return nil }
func (s *scopeRecordingStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (s *scopeRecordingStore) LoadByEventID(context.Context, state.EventID) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (s *scopeRecordingStore) Delete(context.Context, identity.Quadruple, string) error { return nil }
func (s *scopeRecordingStore) DeleteScope(context.Context, identity.Identity) (int, error) {
	return 0, nil
}
func (s *scopeRecordingStore) ListKind(_ context.Context, scope state.ListScope, _ string) ([]state.StateRecord, error) {
	if !scope.MaintenanceScoped {
		return nil, state.ErrMaintenanceScopeRequired
	}
	s.sawMaintenanceScope = true
	return nil, nil
}
func (s *scopeRecordingStore) ListKindForIdentity(context.Context, identity.Quadruple, string) ([]state.StateRecord, error) {
	return nil, nil
}
func (s *scopeRecordingStore) Close(context.Context) error { return nil }
