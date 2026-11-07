package durable

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// hookStore runs a hook before the Nth chunk pre-write batch or before the
// header flip, simulating an erasure that lands mid-migration.
type hookStore struct {
	state.StateStore
	batches   atomic.Int32
	atBatch   int32
	onBatch   func()
	onFlip    func()
	flipFired atomic.Bool
}

func (s *hookStore) SaveBatchIf(ctx context.Context, exps []state.SlotExpectation, writes []state.StateRecord) error {
	if len(writes) > 0 && strings.HasPrefix(writes[0].Kind, kindChunkPrefix) {
		if s.batches.Add(1) == s.atBatch && s.onBatch != nil {
			s.onBatch()
		}
	}
	return s.StateStore.SaveBatchIf(ctx, exps, writes)
}

func (s *hookStore) SaveIf(ctx context.Context, exps []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == kindHead && isIndexV2(next.Bytes) && s.onFlip != nil && s.flipFired.CompareAndSwap(false, true) {
		s.onFlip()
	}
	return s.StateStore.SaveIf(ctx, exps, next)
}

// TestMigration_ErasureRacingMigrationLeavesNoResidue (review P1-1): an
// erasure landing between the head load and a pre-write, between pre-write
// batches, or before the flip leaves no events.durable.* record behind.
func TestMigration_ErasureRacingMigrationLeavesNoResidue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		atBatch   int32
		atFlip    bool
		fenceOnly bool
	}{
		{name: "before_first_prewrite", atBatch: 1},
		{name: "between_prewrite_batches", atBatch: 2},
		{name: "before_flip", atFlip: true},
		{name: "fence_only_before_flip", atFlip: true, fenceOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newInmemTestStore(t)
			id := sid("erased-mid-migration")
			seedLegacySession(t, inner, seedSpec{id: id, n: 40 * indexChunkRows, at: indexEpoch, bodies: true})
			store := &hookStore{StateStore: inner, atBatch: tc.atBatch}
			b := openIndexBus(t, store, 0)
			erase := func() {
				mustf(t, b.Fence(context.Background(), id.Identity), "Fence")
				if !tc.fenceOnly {
					if _, err := inner.DeleteScope(context.Background(), id.Identity); err != nil {
						t.Errorf("DeleteScope: %v", err)
					}
				}
			}
			if tc.atFlip {
				store.onFlip = erase
			} else {
				store.onBatch = erase
			}
			flipped, err := b.migrateSession(context.Background(), id)
			if err != nil || flipped {
				t.Fatalf("migrate during erasure = %v, %v; want a clean no-op", flipped, err)
			}
			prefix := "events.durable."
			if tc.fenceOnly {
				prefix = kindChunkPrefix // the legacy head and bodies await DeleteScope
			}
			if left := listKinds(t, inner, id, prefix); len(left) != 0 {
				t.Fatalf("erasure race left %d records (%v...)", len(left), left[0])
			}
		})
	}
}

// TestProjection_RaceWithTrimKeepsRetainedEvents (review P1-2): a chunk
// deleted mid-walk must not drop the retained rows already collected, and a
// projector that checkpoints on a Current page must not skip any.
func TestProjection_RaceWithTrimKeepsRetainedEvents(t *testing.T) {
	for _, limit := range []int{1000, 100} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			store := newInmemTestStore(t)
			writer := retentionBus(t, store)
			id := sid("raced")
			publishN(t, writer, id, 512, events.EventTypeAdminScopeUsed, oldAt())
			publishN(t, writer, id, 300, events.EventTypeRuntimeWarning, recentAt())
			reader := retentionBus(t, store)
			p := newPauseOnce()
			reader.hookAfterChunkLoad = func(_ identity.Quadruple, chunk uint64) {
				if chunk == 2 {
					p.hit()
				}
			}
			// A projector: page from its checkpoint; on a Current page it
			// advances the checkpoint to the watermark.
			seen := map[uint64]bool{}
			var checkpoint uint64
			var first events.ProjectionPage
			done := make(chan error, 1)
			go func() {
				for i := range 50 {
					page, err := reader.Page(context.Background(), checkpoint, limit)
					if err != nil {
						done <- err
						return
					}
					if i == 0 {
						first = page
					}
					for _, ev := range page.Events {
						seen[ev.Sequence] = true
					}
					checkpoint = page.Next
					if page.Quality == events.ProjectionCurrent {
						checkpoint = page.Watermark
						break
					}
				}
				done <- nil
			}()
			select {
			case <-p.reached:
			case <-time.After(10 * time.Second):
				t.Fatal("projector never reached the pause point")
			}
			runPass(t, writer)
			close(p.release)
			mustf(t, <-done, "projector")
			if !first.RetentionGap {
				t.Fatal("raced page did not report RetentionGap")
			}
			if limit >= 300 && len(first.Events) != 300 {
				t.Fatalf("raced page served %d events, want all 300 retained", len(first.Events))
			}
			if len(seen) != 300 {
				t.Fatalf("projector applied %d retained events before checkpointing past them, want 300", len(seen))
			}
		})
	}
}

func deleteAuthority(t *testing.T, store state.StateStore) {
	t.Helper()
	rec, err := store.Load(context.Background(), sequenceAuthorityIdentity, kindSequenceAuthority)
	mustf(t, err, "load authority")
	if ok, err := store.DeleteIf(context.Background(), state.InternalSlotExpectation(sequenceAuthorityIdentity, kindSequenceAuthority, rec.ID)); err != nil || !ok {
		t.Fatalf("delete authority: %v %v", ok, err)
	}
}

// TestBoot_CorruptHeaderQuarantinesOnlyThatSession (review P1-3).
func TestBoot_CorruptHeaderQuarantinesOnlyThatSession(t *testing.T) {
	store := newInmemTestStore(t)
	first := openIndexBus(t, store, 0)
	bad, good := sid("bad"), sid("good")
	publishN(t, first, bad, 600, events.EventTypeRuntimeWarning, indexEpoch)
	publishN(t, first, good, 600, events.EventTypeRuntimeWarning, indexEpoch)
	mustf(t, first.Close(context.Background()), "close")
	corruptHeader(t, store, bad, func(h *indexHeader) { h.LastSeq++ }, false)

	b := openIndexBus(t, store, 0) // boot succeeds: authority exists
	if _, err := b.Replay(context.Background(), events.Cursor{SessionID: bad.SessionID}, filterFor(bad)); !errors.Is(err, ErrIndexIntegrity) {
		t.Fatalf("quarantined session Replay = %v, want ErrIndexIntegrity", err)
	}
	if err := b.Publish(context.Background(), testEvent(bad, events.EventTypeRuntimeWarning, indexEpoch)); !errors.Is(err, ErrIndexIntegrity) {
		t.Fatalf("quarantined session Publish = %v, want ErrIndexIntegrity", err)
	}
	mustf(t, b.Publish(context.Background(), testEvent(good, events.EventTypeRuntimeWarning, indexEpoch)), "healthy publish")
	page, err := b.Page(context.Background(), 0, 2000)
	if err != nil || len(page.Events) != 601 || !page.RetentionGap {
		t.Fatalf("projection page with a quarantined session: %d events gap=%v err=%v", len(page.Events), page.RetentionGap, err)
	}
	if page.Quality == events.ProjectionCurrent {
		t.Fatal("a page that skipped a corrupt session claimed Current; a projector would checkpoint past its events")
	}
	admin, err := b.ListWindow(context.Background(), events.EventListQuery{Admin: true, Limit: 2000})
	if err != nil || len(admin.Events) != 601 || !admin.Truncated {
		t.Fatalf("admin list with a quarantined session: %d events truncated=%v err=%v", len(admin.Events), admin.Truncated, err)
	}
	if b.corruptSessionSkips.Load() < 2 || b.indexIntegrityFailures.Load() == 0 {
		t.Fatalf("skips=%d integrity=%d", b.corruptSessionSkips.Load(), b.indexIntegrityFailures.Load())
	}

	// Without sequence authority the head floor is the only source: boot
	// must fail rather than guess.
	deleteAuthority(t, store)
	if _, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store, withMaintenance(func(m *maintenance) { m.disabled = true })); !errors.Is(err, ErrIndexIntegrity) {
		t.Fatalf("boot without authority over a corrupt header = %v, want ErrIndexIntegrity", err)
	}
}

// TestRebuildSessionIndex_RecoversQuarantinedSession (review P1-3 recovery).
func TestRebuildSessionIndex_RecoversQuarantinedSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trimmed bool
		corrupt func(t *testing.T, store state.StateStore, id identity.Quadruple)
	}{
		{name: "header", corrupt: func(t *testing.T, store state.StateStore, id identity.Quadruple) {
			corruptHeader(t, store, id, func(h *indexHeader) { h.RetainedCount-- }, true)
		}},
		{name: "chunk", corrupt: func(t *testing.T, store state.StateStore, id identity.Quadruple) {
			corruptChunk(t, store, id, 1, func(d map[string]any) { d["first_seq"] = addNumber(d["first_seq"], 1) })
		}},
		{name: "header_after_trim", trimmed: true, corrupt: func(t *testing.T, store state.StateStore, id identity.Quadruple) {
			corruptHeader(t, store, id, func(h *indexHeader) { h.Tail[0].Type = events.EventTypeRuntimeError }, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newInmemTestStore(t)
			b := retentionBus(t, store)
			id := sid("rebuild")
			if tc.trimmed {
				publishN(t, b, id, 512, events.EventTypeAdminScopeUsed, oldAt())
			}
			publishN(t, b, id, 700, events.EventTypeRuntimeWarning, recentAt())
			if tc.trimmed {
				runPass(t, b)
				runPass(t, b)
			}
			before := snapshotReaders(t, b, id, sessionIndexSeqs(t, store, id)[100])
			tc.corrupt(t, store, id)
			if _, err := b.Window(context.Background(), 0, 2000, filterFor(id)); !errors.Is(err, ErrIndexIntegrity) {
				t.Fatalf("corruption not detected: %v", err)
			}
			report, err := RebuildSessionIndex(context.Background(), store, id, RebuildSessionIndexOptions{})
			mustf(t, err, "inspect")
			if report.Applied || report.Retained != 700 {
				t.Fatalf("inspect report %+v", report)
			}
			if _, err := RebuildSessionIndex(context.Background(), store, id, RebuildSessionIndexOptions{Apply: true}); !errors.Is(err, ErrRebuildRefused) {
				t.Fatalf("apply without writer-drained ack = %v", err)
			}
			report, err = RebuildSessionIndex(context.Background(), store, id, RebuildSessionIndexOptions{Apply: true, WriterDrained: true})
			mustf(t, err, "apply")
			if !report.Applied {
				t.Fatalf("apply report %+v", report)
			}
			after := snapshotReaders(t, b, id, sessionIndexSeqs(t, store, id)[100])
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rebuilt session reads differ from before the corruption")
			}
			mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, recentAt())), "publish after rebuild")
		})
	}
}

// peerSweepStore lets a peer sweeper trim and delete between this sweeper's
// header read and its first chunk load.
type peerSweepStore struct {
	state.StateStore
	once sync.Once
	peer func()
}

func (s *peerSweepStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if kind == chunkKind(0) {
		s.once.Do(s.peer)
	}
	return s.StateStore.Load(ctx, id, kind)
}

// TestRetention_PeerSweeperRaceIsNotAnIntegrityFailure (review P2-1).
func TestRetention_PeerSweeperRaceIsNotAnIntegrityFailure(t *testing.T) {
	inner := newInmemTestStore(t)
	peer := retentionBus(t, inner)
	id := sid("shared")
	publishN(t, peer, id, 600, events.EventTypeAdminScopeUsed, oldAt())
	publishN(t, peer, id, 10, events.EventTypeRuntimeWarning, recentAt())
	store := &peerSweepStore{StateStore: inner, peer: func() { runPass(t, peer) }}
	b := retentionBus(t, store)
	runPass(t, b)
	if got := b.indexIntegrityFailures.Load(); got != 0 {
		t.Fatalf("a peer's concurrent trim raised %d integrity failures", got)
	}
	if hdr, _ := loadHeaderV2(t, inner, id); hdr.Trimmed.Count != 512 {
		t.Fatalf("trimmed %d, want 512", hdr.Trimmed.Count)
	}
}

// TestRetention_LargeBacklogDoesNotStarveOtherSessions (review P2-2).
func TestRetention_LargeBacklogDoesNotStarveOtherSessions(t *testing.T) {
	store := newInmemTestStore(t)
	b := retentionBus(t, store, withMaintenance(func(m *maintenance) { m.deleteBudget = 600 }))
	big := sid("aaa-big")
	publishN(t, b, big, 8*indexChunkRows, events.EventTypeAdminScopeUsed, oldAt())
	publishN(t, b, big, 5, events.EventTypeRuntimeWarning, recentAt())
	var small []identity.Quadruple
	for i := range 30 {
		id := sid(fmt.Sprintf("zzz-small-%02d", i))
		small = append(small, id)
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeAdminScopeUsed, oldAt())), "publish")
	}
	runPass(t, b)
	for _, id := range small {
		hdr, _ := loadHeaderV2(t, store, id)
		if hdr.RetainedCount != 0 || hdr.Trimmed.CleanupPending {
			t.Fatalf("small session %s starved behind the large backlog: %+v", id.SessionID, hdr.Trimmed)
		}
	}
	if hdr, _ := loadHeaderV2(t, store, big); hdr.Trimmed.Count != 8*indexChunkRows || !hdr.Trimmed.CleanupPending {
		t.Fatalf("large session: %+v", hdr.Trimmed)
	}
	for range 10 {
		runPass(t, b)
	}
	if left := listKinds(t, store, big, kindChunkPrefix); len(left) != 0 {
		t.Fatalf("large backlog never drained: %d chunks", len(left))
	}
}

// TestRetention_CleanupProvesExhaustionBeyondListingBound (Sol P2-4): more
// trimmed chunks than one bounded listing returns are all removed, by
// explicit index, before the cleanup marker clears.
func TestRetention_CleanupProvesExhaustionBeyondListingBound(t *testing.T) {
	store := newInmemTestStore(t)
	id := sid("deep")
	seedV2Session(t, store, seedSpec{id: id, n: 70 * indexChunkRows, at: oldAt(), bodies: true,
		typeOf: func(int) events.EventType { return events.EventTypeAdminScopeUsed }})
	b := retentionBus(t, store, withMaintenance(func(m *maintenance) {
		m.deleteBudget = 1 << 20
		m.deleteSlice = 1 << 20
	}))
	mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, recentAt())), "recent row")
	for range 3 {
		runPass(t, b)
	}
	hdr, _ := loadHeaderV2(t, store, id)
	if hdr.Trimmed.Count != 70*indexChunkRows || hdr.Trimmed.CleanupPending {
		t.Fatalf("trim/cleanup state %+v", hdr.Trimmed)
	}
	if left := listKinds(t, store, id, kindChunkPrefix); len(left) != 0 {
		t.Fatalf("%d trimmed chunk records remain", len(left))
	}
	if left := listKinds(t, store, id, kindEntryPrefix); len(left) != 1 {
		t.Fatalf("%d entries remain, want only the recent one", len(left))
	}
}

// TestMigration_NoNudgeForSessionsInBackoff (review P2-3).
func TestMigration_NoNudgeForSessionsInBackoff(t *testing.T) {
	store := newInmemTestStore(t)
	id := sid("stuck")
	seedLegacySession(t, store, seedSpec{id: id, n: inlineMigrationMaxRows + 1, at: indexEpoch, bodies: true})
	b := openIndexBus(t, store, 0)
	b.maint.stuck.Store(sessionMapKey(id), struct{}{})
	mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch)), "publish")
	select {
	case <-b.maint.nudge:
		t.Fatal("a session in migration backoff nudged the migrator")
	default:
	}
}

// liveWriterStore publishes through a live bus just before the rebuild's
// header flip, so the flip's CAS fails.
type liveWriterStore struct {
	state.StateStore
	once    sync.Once
	publish func()
}

func (s *liveWriterStore) SaveIf(ctx context.Context, exps []state.SlotExpectation, next state.StateRecord) error {
	if next.Kind == kindHead {
		s.once.Do(s.publish)
	}
	return s.StateStore.SaveIf(ctx, exps, next)
}

// TestRebuildSessionIndex_RefusalsAndLiveWriterRollback (verification N2/N3).
func TestRebuildSessionIndex_RefusalsAndLiveWriterRollback(t *testing.T) {
	inner := newInmemTestStore(t)
	b := openIndexBus(t, inner, 0)
	id := sid("healthy")
	publishN(t, b, id, 700, events.EventTypeRuntimeWarning, indexEpoch)
	drained := RebuildSessionIndexOptions{Apply: true, WriterDrained: true}
	if _, err := RebuildSessionIndex(context.Background(), inner, id, drained); !errors.Is(err, ErrRebuildRefused) {
		t.Fatalf("rebuild of a verifying index without force = %v, want refused", err)
	}

	// A live writer publishes during a forced rebuild: the flip's CAS fails,
	// the new slots roll back and the session stays healthy.
	before := sessionIndexSeqs(t, inner, id)
	hdrBefore, _ := loadHeaderV2(t, inner, id)
	store := &liveWriterStore{StateStore: inner, publish: func() {
		mustf(t, b.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch)), "live publish")
	}}
	forced := drained
	forced.Force = true
	if _, err := RebuildSessionIndex(context.Background(), store, id, forced); !errors.Is(err, ErrRebuildRefused) {
		t.Fatalf("rebuild racing a live writer = %v, want refused", err)
	}
	after := sessionIndexSeqs(t, inner, id)
	if len(after) != len(before)+1 {
		t.Fatalf("session after the failed rebuild has %d rows, want %d", len(after), len(before)+1)
	}
	hdrAfter, _ := loadHeaderV2(t, inner, id)
	for _, kind := range listKinds(t, inner, id, kindChunkPrefix) {
		index, _ := parseChunkKind(kind)
		if index < hdrAfter.FirstChunk || index >= hdrAfter.NextChunk {
			t.Fatalf("failed rebuild left unreferenced chunk slot %d (index [%d,%d))", index, hdrAfter.FirstChunk, hdrAfter.NextChunk)
		}
	}
	if hdrAfter.FirstChunk != hdrBefore.FirstChunk {
		t.Fatal("failed rebuild changed the live index")
	}
	if got, err := b.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id)); err != nil || len(got) != 701 {
		t.Fatalf("session unhealthy after the failed rebuild: %d, %v", len(got), err)
	}

	// A forced rebuild without a live writer succeeds and preserves reads.
	snap := snapshotReaders(t, b, id, after[100])
	if _, err := RebuildSessionIndex(context.Background(), inner, id, forced); err != nil {
		t.Fatalf("forced rebuild: %v", err)
	}
	if !reflect.DeepEqual(snap, snapshotReaders(t, b, id, after[100])) {
		t.Fatal("forced rebuild changed reads")
	}
	if left := listKinds(t, inner, id, kindChunkPrefix); len(left) != 2 {
		t.Fatalf("old chunk slots not removed after rebuild: %d", len(left))
	}

	// A fenced session is refused even with force.
	mustf(t, b.Fence(context.Background(), id.Identity), "Fence")
	if _, err := RebuildSessionIndex(context.Background(), inner, id, forced); !errors.Is(err, ErrRebuildRefused) {
		t.Fatalf("rebuild of a fenced session = %v, want refused", err)
	}
}

// TestRetention_NonInternalAuditRowsAreNeverTrimmed (verification latent
// P1-2): only bus-internal audit notices qualify, so a trim can never remove a
// row a canonical reader still needs.
func TestRetention_NonInternalAuditRowsAreNeverTrimmed(t *testing.T) {
	cutoff := retentionNow.UnixNano()
	internal := indexRow{Seq: 1, Type: events.EventTypeAdminScopeUsed, Internal: true, OccurredAt: 1}
	canonical := indexRow{Seq: 2, Type: events.EventType("audit.hypothetical_canonical"), Internal: false, OccurredAt: 1}
	if !rowQualifies(internal, cutoff) || rowQualifies(canonical, cutoff) {
		t.Fatal("retention must trim only bus-internal audit.* rows")
	}
	for _, typ := range events.EventTypes() {
		if strings.HasPrefix(string(typ), "audit.") && !events.IsBusInternalNotice(typ) {
			t.Errorf("registered audit type %q is not a bus-internal notice; audit retention will keep it (update D-493 if it should be trimmable)", typ)
		}
	}
}

// TestMigration_NudgeRespectsBootQuietPeriod: a publish-path nudge never runs
// the migrator before the initial boot delay.
func TestMigration_NudgeRespectsBootQuietPeriod(t *testing.T) {
	store := newInmemTestStore(t)
	id := sid("early")
	seedLegacySession(t, store, seedSpec{id: id, n: inlineMigrationMaxRows + 1, at: indexEpoch, bodies: true})
	opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store, WithLogger(discardTestLogger()), withMaintenance(func(m *maintenance) {
		m.initialDelay = time.Hour
	}))
	mustf(t, err, "New")
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	mustf(t, opened.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch)), "publish")
	time.Sleep(300 * time.Millisecond)
	if isV2Head(t, store, id) {
		t.Fatal("a nudge ran the migrator inside the boot quiet period")
	}
}
