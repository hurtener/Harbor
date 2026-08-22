package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

var errRepairResponseLoss = errors.New("repair test: response lost after commit")

type repairResponseLossStore struct {
	state.StateStore
	once atomic.Bool
}

func (s *repairResponseLossStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	err := s.StateStore.SaveBatchIf(ctx, expectations, writes)
	if err == nil && s.once.CompareAndSwap(false, true) {
		return errors.Join(state.ErrCommitOutcomeUnknown, errRepairResponseLoss)
	}
	return err
}

type repairEntryLoadMutatorStore struct {
	state.StateStore
	mutate func(state.StateRecord) state.StateRecord
}

func (s *repairEntryLoadMutatorStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	rec, err := s.StateStore.Load(ctx, id, kind)
	if err == nil && strings.HasPrefix(kind, kindEntryPrefix) {
		rec = s.mutate(rec)
	}
	return rec, err
}

type repairCASRaceStore struct {
	state.StateStore
	identity identity.Quadruple
	once     atomic.Bool
}

func (s *repairCASRaceStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if s.once.CompareAndSwap(false, true) {
		head, err := s.Load(ctx, s.identity, kindHead)
		if err != nil {
			return err
		}
		if err := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: head.Identity, Kind: head.Kind, Bytes: append([]byte(nil), head.Bytes...)}); err != nil {
			return err
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

type repairBatchCall struct {
	expectations int
	writes       int
}

type repairBatchMetrics struct {
	mu    sync.Mutex
	calls []repairBatchCall
}

func (m *repairBatchMetrics) recordBatch(expectations, writes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, repairBatchCall{expectations: expectations, writes: writes})
}

func (m *repairBatchMetrics) batchCalls() []repairBatchCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]repairBatchCall(nil), m.calls...)
}

type repairBatchConflictStore struct {
	state.StateStore
	repairBatchMetrics
	identity identity.Quadruple
	once     atomic.Bool
}

func (s *repairBatchConflictStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.recordBatch(len(expectations), len(writes))
	if s.once.CompareAndSwap(false, true) {
		head, err := s.Load(ctx, s.identity, kindHead)
		if err != nil {
			return err
		}
		if err := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: head.Identity, Kind: kindHead, Bytes: append([]byte(nil), head.Bytes...)}); err != nil {
			return err
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

type repairBatchCancelStore struct {
	state.StateStore
	repairBatchMetrics
	cancel context.CancelFunc
	once   atomic.Bool
}

func (s *repairBatchCancelStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.recordBatch(len(expectations), len(writes))
	if s.once.CompareAndSwap(false, true) {
		s.cancel()
		return ctx.Err()
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

type repairBatchUnknownStore struct {
	state.StateStore
	repairBatchMetrics
	once atomic.Bool
}

func (s *repairBatchUnknownStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.recordBatch(len(expectations), len(writes))
	err := s.StateStore.SaveBatchIf(ctx, expectations, writes)
	if err == nil && s.once.CompareAndSwap(false, true) {
		return errors.Join(state.ErrCommitOutcomeUnknown, errRepairResponseLoss)
	}
	return err
}

type repairCancelAfterListStore struct {
	state.StateStore
	cancel context.CancelFunc
}

func (s *repairCancelAfterListStore) ListKindBounded(ctx context.Context, scope state.ListScope, prefix string, limit int) ([]state.StateRecord, error) {
	recs, err := s.StateStore.ListKindBounded(ctx, scope, prefix, limit)
	s.cancel()
	return recs, err
}

func repairIdentity(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "repair-tenant", UserID: "repair-user", SessionID: session}}
}

func repairEvent(id identity.Quadruple, seq uint64, run string) events.Event {
	return events.Event{
		Type:       events.EventTypeRuntimeWarning,
		Identity:   identity.Quadruple{Identity: id.Identity, RunID: run},
		OccurredAt: time.Unix(1_700_000_000+int64(seq), 0).UTC(),
		Sequence:   seq,
		Payload:    events.BusDroppedPayload{FromSeq: seq, ToSeq: seq, DroppedCount: 0, SubscriberID: seq},
	}
}

func newRepairInmem(t *testing.T) state.StateStore {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("open in-memory StateStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func seedRepairHead(t *testing.T, store state.StateStore, session string, sequences []uint64, metadataReady bool) identity.Quadruple {
	t.Helper()
	id := repairIdentity(session)
	seen := make(map[uint64]bool)
	metadata := make([]eventMetadataRecord, 0, len(sequences))
	for _, seq := range sequences {
		if seen[seq] {
			continue
		}
		seen[seq] = true
		ev := repairEvent(id, seq, "run-"+session)
		body, err := encodeEvent(ev)
		if err != nil {
			t.Fatalf("encode entry seq=%d: %v", seq, err)
		}
		if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindEntryPrefix + seqToken(seq), Bytes: body}); err != nil {
			t.Fatalf("save entry seq=%d: %v", seq, err)
		}
	}
	if metadataReady {
		for _, seq := range sequences {
			metadata = append(metadata, mustRepairMetadata(t, repairEvent(id, seq, "run-"+session)))
		}
	}
	head := headRecord{Sequences: append([]uint64(nil), sequences...), Metadata: metadata, MetadataReady: metadataReady}
	if metadataReady {
		head.MetadataValidatedCount = len(sequences)
		head.MetadataIntegrityChecksum = metadataIntegrityChecksum(head.Sequences, head.Metadata)
	}
	headBytes, err := encodeHead(head)
	if err != nil {
		t.Fatalf("encode head: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headBytes}); err != nil {
		t.Fatalf("save head: %v", err)
	}
	return id
}

func mustRepairMetadata(t *testing.T, ev events.Event) eventMetadataRecord {
	t.Helper()
	m, err := metadataRecordFromEvent(ev)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	return m
}

func TestLegacyRepair_AdjacentDuplicates_ApplyAndIdempotentReplay(t *testing.T) {
	store := newRepairInmem(t)
	id := seedRepairHead(t, store, "adjacent", []uint64{21246, 21247, 21247, 23491, 23492, 23492, 23493}, false)

	inspect, err := InspectLegacyHeads(context.Background(), store)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspect.AffectedHeadCount != 1 || inspect.DuplicateSequenceCount != 2 || inspect.RedundantReferenceCount != 2 {
		t.Fatalf("inspect summary = %+v", inspect)
	}
	if len(inspect.Heads) != 1 || len(inspect.Heads[0].Duplicates) != 2 {
		t.Fatalf("inspect duplicates = %+v", inspect.Heads)
	}
	if inspect.Heads[0].HeadIdentityHash == "" || inspect.Heads[0].HeadIdentityHash == id.SessionID {
		t.Fatalf("inspect leaked or omitted identity hash: %+v", inspect.Heads[0])
	}

	first, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if first.AffectedHeadCount != 1 || first.DuplicateSequenceCount != 2 || first.RedundantReferenceCount != 2 {
		t.Fatalf("apply summary = %+v", first)
	}
	head, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load repaired head: %v", err)
	}
	decoded, err := decodeHead(head.Bytes)
	if err != nil {
		t.Fatalf("decode repaired head: %v", err)
	}
	wantSeq := []uint64{21246, 21247, 23491, 23492, 23493}
	if !reflect.DeepEqual(decoded.Sequences, wantSeq) {
		t.Fatalf("repaired sequences = %v, want %v", decoded.Sequences, wantSeq)
	}
	entry, err := store.Load(context.Background(), id, kindEntryPrefix+seqToken(21247))
	if err != nil {
		t.Fatalf("immutable entry missing: %v", err)
	}
	if got, err := decodeEvent(entry.Bytes); err != nil || got.Identity.RunID != "run-adjacent" {
		t.Fatalf("immutable entry changed: event=%+v err=%v", got, err)
	}

	second, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("response-loss replay changed receipt report:\nfirst=%+v\nsecond=%+v", first, second)
	}
	verified, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.AffectedHeadCount != 0 || verified.DuplicateSequenceCount != 0 {
		t.Fatalf("verify = %+v", verified)
	}
}

func TestLegacyRepair_ResponseLoss_ReplaysPersistedReceiptByteIdentically(t *testing.T) {
	base := newRepairInmem(t)
	id := seedRepairHead(t, base, "response-loss", []uint64{10, 11, 11, 12}, false)
	store := &repairResponseLossStore{StateStore: base}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, ErrLegacyRepairRefused) || !errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("first response-loss apply error=%v, want refusal plus ErrCommitOutcomeUnknown", err)
	}
	receiptKind := legacyRepairReceiptKind(id)
	firstReceipt, err := base.Load(context.Background(), id, receiptKind)
	if err != nil {
		t.Fatalf("receipt after committed response loss: %v", err)
	}
	firstHead, err := base.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("head after committed response loss: %v", err)
	}
	firstReport, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("response-loss replay: %v", err)
	}
	secondReceipt, err := base.Load(context.Background(), id, receiptKind)
	if err != nil {
		t.Fatalf("receipt after replay: %v", err)
	}
	secondHead, err := base.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("head after replay: %v", err)
	}
	if !bytes.Equal(firstReceipt.Bytes, secondReceipt.Bytes) || !bytes.Equal(firstHead.Bytes, secondHead.Bytes) {
		t.Fatal("response-loss replay changed persisted receipt or canonical head bytes")
	}
	secondReport, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("second response-loss replay: %v", err)
	}
	if !reflect.DeepEqual(firstReport, secondReport) {
		t.Fatalf("response-loss reports differ:\nfirst=%+v\nsecond=%+v", firstReport, secondReport)
	}
}

func TestLegacyRepair_DurableBootReplaysEachImmutableEventOnceAndAdoptsAuthority(t *testing.T) {
	store := newRepairInmem(t)
	id := seedRepairHead(t, store, "boot-replay", []uint64{100, 100, 101, 102, 102, 103}, false)

	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); err != nil {
		t.Fatalf("repair legacy head: %v", err)
	}

	opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable boot after repair: %v", err)
	}
	b := opened.(*bus)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if b.nextSeq != 103 {
		t.Fatalf("boot sequence floor = %d, want repaired head max 103", b.nextSeq)
	}
	authority, err := store.Load(context.Background(), sequenceAuthorityIdentity, kindSequenceAuthority)
	if err != nil {
		t.Fatalf("load adopted sequence authority: %v", err)
	}
	var adopted sequenceAuthorityRecord
	if err := json.Unmarshal(authority.Bytes, &adopted); err != nil {
		t.Fatalf("decode adopted sequence authority: %v", err)
	}
	if adopted.Sequence != 103 {
		t.Fatalf("adopted sequence authority = %d, want 103", adopted.Sequence)
	}

	filter := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	replayed, err := opened.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filter)
	if err != nil {
		t.Fatalf("replay after repair and boot: %v", err)
	}
	if len(replayed) != 4 {
		t.Fatalf("replayed %d immutable events, want exactly 4: %+v", len(replayed), replayed)
	}
	for i, ev := range replayed {
		want := uint64(100 + i)
		if ev.Sequence != want {
			t.Fatalf("replayed event[%d] sequence = %d, want %d", i, ev.Sequence, want)
		}
		if ev.Identity.TenantID != id.TenantID || ev.Identity.UserID != id.UserID || ev.Identity.SessionID != id.SessionID {
			t.Fatalf("replayed event[%d] identity = %+v, want session identity", i, ev.Identity)
		}
	}

	if err := opened.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  repairEvent(id, 104, "run-boot-replay").Payload,
	}); err != nil {
		t.Fatalf("publish after repaired boot: %v", err)
	}
	after, err := opened.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID, Sequence: 103}, filter)
	if err != nil {
		t.Fatalf("replay post-recovery event: %v", err)
	}
	if len(after) != 1 || after[0].Sequence != 104 {
		t.Fatalf("post-recovery replay = %+v, want exactly sequence 104", after)
	}
}

func TestLegacyRepair_RejectsReceiptWithStaleAppliedGeneration(t *testing.T) {
	store := newRepairInmem(t)
	id := seedRepairHead(t, store, "stale-receipt-generation", []uint64{10, 11, 11, 12}, false)
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	receiptKind := legacyRepairReceiptKind(id)
	receiptRecord, err := store.Load(context.Background(), id, receiptKind)
	if err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	var receipt LegacyRepairReceipt
	if err := json.Unmarshal(receiptRecord.Bytes, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	previousReceiptID := receiptRecord.ID
	receiptRecord.ID = state.NewEventID()
	receipt.ReceiptID = string(receiptRecord.ID)
	receipt.AppliedGeneration = "stale-generation"
	receiptRecord.Bytes, err = json.Marshal(receipt)
	if err != nil {
		t.Fatalf("encode forged receipt: %v", err)
	}
	replacement := state.NewInternalRecord(receiptRecord.ID, receiptRecord.Identity, receiptRecord.Kind, receiptRecord.Bytes)
	if err := store.SaveBatchIf(context.Background(),
		[]state.SlotExpectation{state.InternalSlotExpectation(id, receiptKind, previousReceiptID)},
		[]state.StateRecord{replacement}); err != nil {
		t.Fatalf("save forged receipt: %v", err)
	}
	headBefore, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load head before replay: %v", err)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, ErrLegacyRepairRefused) {
		t.Fatalf("forged receipt replay error=%v, want refusal", err)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify}); !errors.Is(err, ErrLegacyRepairRefused) {
		t.Fatalf("forged receipt verify error=%v, want refusal", err)
	}
	headAfter, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load head after replay: %v", err)
	}
	if headBefore.ID != headAfter.ID || !bytes.Equal(headBefore.Bytes, headAfter.Bytes) {
		t.Fatal("forged receipt replay changed canonical head")
	}
}

func TestLegacyRepair_RejectsReceiptWhenImmutableEvidenceChanges(t *testing.T) {
	store := newRepairInmem(t)
	id := seedRepairHead(t, store, "stale-receipt-entry", []uint64{10, 11, 11, 12}, false)
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	record, err := store.Load(context.Background(), id, kindEntryPrefix+seqToken(11))
	if err != nil {
		t.Fatalf("load immutable entry: %v", err)
	}
	record.Bytes = append(record.Bytes, '\n')
	record.ID = state.NewEventID()
	if err := store.Save(context.Background(), record); err != nil {
		t.Fatalf("rewrite immutable entry fixture: %v", err)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify}); !errors.Is(err, ErrLegacyRepairRefused) {
		t.Fatalf("stale entry verify error=%v, want refusal", err)
	}
}

func TestLegacyRepair_CASRace_RereadsAndRepairsCurrentGeneration(t *testing.T) {
	base := newRepairInmem(t)
	id := seedRepairHead(t, base, "cas-race", []uint64{20, 20, 21}, false)
	store := &repairCASRaceStore{StateStore: base, identity: id}
	report, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("CAS-raced apply: %v", err)
	}
	if report.AffectedHeadCount != 1 || report.RedundantReferenceCount != 1 {
		t.Fatalf("CAS-raced report = %+v", report)
	}
	head, err := base.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load CAS-raced head: %v", err)
	}
	decoded, err := decodeHead(head.Bytes)
	if err != nil || !reflect.DeepEqual(decoded.Sequences, []uint64{20, 21}) {
		t.Fatalf("CAS-raced repaired head=%+v err=%v", decoded, err)
	}
}

func assertCanonicalRepairHead(t *testing.T, store state.StateStore, id identity.Quadruple, want []uint64) {
	t.Helper()
	head, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load repaired head: %v", err)
	}
	decoded, err := decodeHead(head.Bytes)
	if err != nil {
		t.Fatalf("decode repaired head: %v", err)
	}
	if !reflect.DeepEqual(decoded.Sequences, want) {
		t.Fatalf("repaired sequences = %v, want %v", decoded.Sequences, want)
	}
	if _, found, err := loadLegacyRepairReceipt(context.Background(), store, head, LegacyRepairToolVersion); err != nil || !found {
		t.Fatalf("repaired head receipt found=%v err=%v", found, err)
	}
}

func TestLegacyRepair_MultiHeadApply_IsAtomicAcrossConflictCancellationAndUnknownAck(t *testing.T) {
	seedTwo := func(t *testing.T) (state.StateStore, identity.Quadruple, identity.Quadruple) {
		t.Helper()
		base := newRepairInmem(t)
		first := seedRepairHead(t, base, "batch-first", []uint64{10, 10, 11}, false)
		second := seedRepairHead(t, base, "batch-second", []uint64{20, 20, 21}, false)
		return base, first, second
	}

	t.Run("generation conflict retries the complete batch", func(t *testing.T) {
		base, first, second := seedTwo(t)
		store := &repairBatchConflictStore{StateStore: base, identity: first}
		report, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
		if err != nil {
			t.Fatalf("generation-conflict batch apply: %v", err)
		}
		if report.AffectedHeadCount != 2 || report.RedundantReferenceCount != 2 {
			t.Fatalf("generation-conflict report = %+v", report)
		}
		calls := store.batchCalls()
		if !reflect.DeepEqual(calls, []repairBatchCall{{expectations: 4, writes: 4}, {expectations: 4, writes: 4}}) {
			t.Fatalf("generation-conflict batch calls = %+v, want two complete 4-slot batches", calls)
		}
		assertCanonicalRepairHead(t, base, first, []uint64{10, 11})
		assertCanonicalRepairHead(t, base, second, []uint64{20, 21})
	})

	t.Run("cancellation leaves every head and receipt untouched", func(t *testing.T) {
		base, first, second := seedTwo(t)
		beforeFirst, err := base.Load(context.Background(), first, kindHead)
		if err != nil {
			t.Fatalf("load first head before cancellation: %v", err)
		}
		beforeSecond, err := base.Load(context.Background(), second, kindHead)
		if err != nil {
			t.Fatalf("load second head before cancellation: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &repairBatchCancelStore{StateStore: base, cancel: cancel}
		if _, err := RepairLegacyHeads(ctx, store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled batch apply error=%v, want context.Canceled", err)
		}
		calls := store.batchCalls()
		if !reflect.DeepEqual(calls, []repairBatchCall{{expectations: 4, writes: 4}}) {
			t.Fatalf("cancelled batch calls = %+v, want one complete attempted batch", calls)
		}
		afterFirst, err := base.Load(context.Background(), first, kindHead)
		if err != nil {
			t.Fatalf("load first head after cancellation: %v", err)
		}
		afterSecond, err := base.Load(context.Background(), second, kindHead)
		if err != nil {
			t.Fatalf("load second head after cancellation: %v", err)
		}
		if beforeFirst.ID != afterFirst.ID || !bytes.Equal(beforeFirst.Bytes, afterFirst.Bytes) || beforeSecond.ID != afterSecond.ID || !bytes.Equal(beforeSecond.Bytes, afterSecond.Bytes) {
			t.Fatal("cancellation left a partial head mutation")
		}
		for _, id := range []identity.Quadruple{first, second} {
			if _, err := base.Load(context.Background(), id, legacyRepairReceiptKind(id)); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("cancellation left receipt for %s: %v", id.SessionID, err)
			}
		}
	})

	t.Run("unknown acknowledgement commits or replays the complete batch", func(t *testing.T) {
		base, first, second := seedTwo(t)
		store := &repairBatchUnknownStore{StateStore: base}
		if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, ErrLegacyRepairRefused) || !errors.Is(err, state.ErrCommitOutcomeUnknown) {
			t.Fatalf("unknown-ack batch apply error=%v, want refusal plus ErrCommitOutcomeUnknown", err)
		}
		calls := store.batchCalls()
		if !reflect.DeepEqual(calls, []repairBatchCall{{expectations: 4, writes: 4}}) {
			t.Fatalf("unknown-ack batch calls = %+v, want one complete batch", calls)
		}
		assertCanonicalRepairHead(t, base, first, []uint64{10, 11})
		assertCanonicalRepairHead(t, base, second, []uint64{20, 21})
		replayed, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
		if err != nil {
			t.Fatalf("unknown-ack batch replay: %v", err)
		}
		if replayed.AffectedHeadCount != 2 || replayed.RedundantReferenceCount != 2 {
			t.Fatalf("unknown-ack replay report = %+v", replayed)
		}
		if calls := store.batchCalls(); !reflect.DeepEqual(calls, []repairBatchCall{{expectations: 4, writes: 4}}) {
			t.Fatalf("unknown-ack replay unexpectedly wrote another batch: %+v", calls)
		}
	})
}

func TestLegacyRepair_CancellationAfterBoundedScanBegins(t *testing.T) {
	base := newRepairInmem(t)
	seedRepairHead(t, base, "cancel-after-list", []uint64{30, 30}, false)
	ctx, cancel := context.WithCancel(context.Background())
	store := &repairCancelAfterListStore{StateStore: base, cancel: cancel}
	if _, err := InspectLegacyHeads(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation error=%v, want context.Canceled", err)
	}
}

func TestLegacyRepair_MetadataReady_RecomputesIntegrity(t *testing.T) {
	store := newRepairInmem(t)
	id := seedRepairHead(t, store, "metadata", []uint64{1, 2, 2, 3}, true)
	before, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}
	old, err := decodeHead(before.Bytes)
	if err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if !headMetadataReady(old) {
		t.Fatal("fixture is not metadata-ready")
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); err != nil {
		t.Fatalf("apply metadata-ready: %v", err)
	}
	after, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	got, err := decodeHead(after.Bytes)
	if err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if !headMetadataReady(got) || len(got.Sequences) != len(got.Metadata) || len(got.Sequences) != 3 {
		t.Fatalf("repaired metadata readiness = %+v", got)
	}
	if got.Metadata[1].RunID != "run-metadata" {
		t.Fatalf("payload RunID was not preserved: %+v", got.Metadata)
	}
}

func TestLegacyRepair_RefusesAmbiguousOrCorruptHeads(t *testing.T) {
	tests := []struct {
		name      string
		sequences []uint64
		mutate    func(t *testing.T, store state.StateStore, id identity.Quadruple)
	}{
		{name: "non-adjacent", sequences: []uint64{1, 2, 1}},
		{name: "descending", sequences: []uint64{1, 3, 2}},
		{name: "missing-entry", sequences: []uint64{4, 4}, mutate: func(t *testing.T, store state.StateStore, id identity.Quadruple) {
			if err := store.Delete(context.Background(), id, kindEntryPrefix+seqToken(4)); err != nil {
				t.Fatalf("delete fixture entry: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newRepairInmem(t)
			id := seedRepairHead(t, store, tt.name, tt.sequences, false)
			if tt.mutate != nil {
				tt.mutate(t, store, id)
			}
			if _, err := InspectLegacyHeads(context.Background(), store); !errors.Is(err, ErrLegacyRepairRefused) {
				t.Fatalf("inspect error = %v, want ErrLegacyRepairRefused", err)
			}
			if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, ErrLegacyRepairRefused) {
				t.Fatalf("apply error = %v, want ErrLegacyRepairRefused", err)
			}
		})
	}
}

func rewriteRepairEntry(t *testing.T, store state.StateStore, id identity.Quadruple, mutate func(*events.Event), raw []byte) {
	t.Helper()
	const sequence = uint64(40)
	kind := kindEntryPrefix + seqToken(sequence)
	rec, err := store.Load(context.Background(), id, kind)
	if err != nil {
		t.Fatalf("load entry %d: %v", sequence, err)
	}
	if raw != nil {
		rec.Bytes = raw
	} else {
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			t.Fatalf("decode entry %d: %v", sequence, err)
		}
		mutate(&ev)
		rec.Bytes, err = encodeEvent(ev)
		if err != nil {
			t.Fatalf("encode entry %d: %v", sequence, err)
		}
	}
	rec.ID = state.NewEventID()
	if err := store.Save(context.Background(), rec); err != nil {
		t.Fatalf("rewrite entry %d: %v", sequence, err)
	}
}

func assertRepairRefusalPreservesHead(t *testing.T, base, store state.StateStore, id identity.Quadruple) {
	t.Helper()
	before, err := base.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load adversarial head before apply: %v", err)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true}); !errors.Is(err, ErrLegacyRepairRefused) {
		t.Fatalf("adversarial apply error=%v, want ErrLegacyRepairRefused", err)
	}
	after, err := base.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load adversarial head after apply: %v", err)
	}
	if before.ID != after.ID || !bytes.Equal(before.Bytes, after.Bytes) {
		t.Fatalf("adversarial refusal changed head: before=%s after=%s", before.ID, after.ID)
	}
	if _, err := base.Load(context.Background(), id, legacyRepairReceiptKind(id)); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("adversarial refusal left receipt: %v", err)
	}
}

func TestLegacyRepair_RefusesEntryAndMetadataAmbiguityWithoutWrites(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore
		metadata bool
	}{
		{name: "wrong entry storage identity", mutate: func(_ *testing.T, base state.StateStore, _ identity.Quadruple) state.StateStore {
			return &repairEntryLoadMutatorStore{StateStore: base, mutate: func(rec state.StateRecord) state.StateRecord {
				rec.Identity.RunID = "wrong-storage-run"
				return rec
			}}
		}},
		{name: "wrong entry kind", mutate: func(_ *testing.T, base state.StateStore, _ identity.Quadruple) state.StateStore {
			return &repairEntryLoadMutatorStore{StateStore: base, mutate: func(rec state.StateRecord) state.StateRecord {
				rec.Kind += ":wrong"
				return rec
			}}
		}},
		{name: "payload tenant mismatch", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, func(ev *events.Event) { ev.Identity.TenantID = "wrong-tenant" }, nil)
			return base
		}},
		{name: "payload user mismatch", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, func(ev *events.Event) { ev.Identity.UserID = "wrong-user" }, nil)
			return base
		}},
		{name: "payload session mismatch", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, func(ev *events.Event) { ev.Identity.SessionID = "wrong-session" }, nil)
			return base
		}},
		{name: "payload sequence mismatch", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, func(ev *events.Event) { ev.Sequence = 999 }, nil)
			return base
		}},
		{name: "malformed payload JSON", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, nil, []byte(`{"malformed"`))
			return base
		}},
		{name: "invalid payload event type", mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			rewriteRepairEntry(t, base, id, func(ev *events.Event) { ev.Type = events.EventType("unknown.invalid") }, nil)
			return base
		}},
		{name: "conflicting duplicate metadata", metadata: true, mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			head, err := base.Load(context.Background(), id, kindHead)
			if err != nil {
				t.Fatalf("load metadata head: %v", err)
			}
			decoded, err := decodeHead(head.Bytes)
			if err != nil {
				t.Fatalf("decode metadata head: %v", err)
			}
			decoded.Metadata[1].RunID = "conflicting-run"
			decoded.MetadataIntegrityChecksum = metadataIntegrityChecksum(decoded.Sequences, decoded.Metadata)
			head.Bytes, err = encodeHead(decoded)
			if err != nil {
				t.Fatalf("encode conflicting metadata head: %v", err)
			}
			head.ID = state.NewEventID()
			if err := base.Save(context.Background(), head); err != nil {
				t.Fatalf("save conflicting metadata head: %v", err)
			}
			return base
		}},
		{name: "conflicting nonduplicate metadata", metadata: true, mutate: func(t *testing.T, base state.StateStore, id identity.Quadruple) state.StateStore {
			head, err := base.Load(context.Background(), id, kindHead)
			if err != nil {
				t.Fatalf("load metadata head: %v", err)
			}
			decoded, err := decodeHead(head.Bytes)
			if err != nil {
				t.Fatalf("decode metadata head: %v", err)
			}
			// Sequence 41 occurs once, so a structurally valid but forged
			// projection here would evade duplicate-only validation.
			decoded.Metadata[2].RunID = "conflicting-nonduplicate-run"
			decoded.MetadataIntegrityChecksum = metadataIntegrityChecksum(decoded.Sequences, decoded.Metadata)
			head.Bytes, err = encodeHead(decoded)
			if err != nil {
				t.Fatalf("encode conflicting nonduplicate metadata head: %v", err)
			}
			head.ID = state.NewEventID()
			if err := base.Save(context.Background(), head); err != nil {
				t.Fatalf("save conflicting nonduplicate metadata head: %v", err)
			}
			return base
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := newRepairInmem(t)
			seqs := []uint64{40, 40, 41}
			if tt.metadata {
				seqs = []uint64{40, 40, 41}
			}
			id := seedRepairHead(t, base, tt.name, seqs, tt.metadata)
			store := tt.mutate(t, base, id)
			assertRepairRefusalPreservesHead(t, base, store, id)
		})
	}
}

func TestLegacyRepair_RequiresDrainAcknowledgementAndHonorsCancellation(t *testing.T) {
	store := newRepairInmem(t)
	seedRepairHead(t, store, "ack", []uint64{1, 1}, false)
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply}); !errors.Is(err, ErrLegacyRepairRefused) {
		t.Fatalf("missing acknowledgement error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectLegacyHeads(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inspect error = %v", err)
	}
}

func TestLegacyRepair_ConcurrentApplyHasOneReceipt(t *testing.T) {
	store := newRepairInmem(t)
	seedRepairHead(t, store, "concurrent", []uint64{1, 1, 2, 2}, false)
	const workers = 32
	results := make(chan LegacyRepairReport, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
			if err != nil {
				errs <- err
				return
			}
			results <- report
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		current, loadErr := store.Load(context.Background(), repairIdentity("concurrent"), kindHead)
		if loadErr == nil {
			decoded, decodeErr := decodeHead(current.Bytes)
			t.Logf("concurrent refusal diagnostics: head=%s hash=%s sequences=%v decode=%v", current.ID, sha256Hex(current.Bytes), decoded.Sequences, decodeErr)
		}
		t.Fatalf("concurrent apply: %v", err)
	}
	var first LegacyRepairReport
	for report := range results {
		if first.ToolVersion == "" {
			first = report
			continue
		}
		if !reflect.DeepEqual(first, report) {
			t.Fatalf("concurrent reports differ:\nfirst=%+v\nnext=%+v", first, report)
		}
	}
	if first.AffectedHeadCount != 1 || first.RedundantReferenceCount != 2 {
		t.Fatalf("concurrent report = %+v", first)
	}
}

func TestLegacyRepair_ScansFleetScaleWithoutPayloadMaterialization(t *testing.T) {
	store := newRepairInmem(t)
	for i := range 4500 {
		session := "scale-" + string(rune('a'+(i/26)%26)) + "-" + padRepairNumber(i)
		id := repairIdentity(session)
		sequences := []uint64(nil)
		if i < 89 {
			seq := uint64(i + 1)
			ev := repairEvent(id, seq, "scale-run")
			body, err := encodeEvent(ev)
			if err != nil {
				t.Fatalf("encode scale entry: %v", err)
			}
			if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindEntryPrefix + seqToken(seq), Bytes: body}); err != nil {
				t.Fatalf("save scale entry: %v", err)
			}
			sequences = []uint64{seq, seq}
		}
		headBytes, err := encodeHead(headRecord{Sequences: sequences})
		if err != nil {
			t.Fatalf("encode scale head: %v", err)
		}
		if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headBytes}); err != nil {
			t.Fatalf("save scale head: %v", err)
		}
	}
	report, err := InspectLegacyHeads(context.Background(), store)
	if err != nil {
		t.Fatalf("scale inspect: %v", err)
	}
	if report.HeadsScanned != 4500 || report.AffectedHeadCount != 89 || report.DuplicateSequenceCount != 89 || report.RedundantReferenceCount != 89 {
		t.Fatalf("scale report = %+v", report)
	}
}

func padRepairNumber(n int) string {
	return string([]byte{'0' + byte((n/1000)%10), '0' + byte((n/100)%10), '0' + byte((n/10)%10), '0' + byte(n%10)})
}

func TestLegacyRepair_ApplyDSNRejectsPooledEndpoints(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@example:6432/harbor",
		"postgres://u:p@example:6432/harbor?direct=true",
		"host=example port=6432 dbname=harbor",
		"host=example\tport\t=\t6432 dbname=harbor",
		"postgres://u:p@example/harbor?pool_mode=transaction",
		"postgres://u:p@pgbouncer.example:5432/harbor",
		"postgres://u:p@PGBOUNCER.example:5432/harbor",
		"host=PgBouncer port=5432 dbname=harbor",
		"host = pgbouncer port = 5432 dbname=harbor",
		"host=pooler-pgbouncer.internal port=5432 dbname=harbor",
	} {
		if err := ValidateLegacyRepairApplyDSN(dsn); err == nil {
			t.Errorf("ValidateLegacyRepairApplyDSN(%q) accepted pooled endpoint", dsn)
		}
	}
	if err := ValidateLegacyRepairApplyDSN("postgres://u:p@example:5432/harbor"); err != nil {
		t.Fatalf("direct DSN rejected: %v", err)
	}
}
