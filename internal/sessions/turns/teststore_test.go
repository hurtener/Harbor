package turns

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"

	// Test-scoped driver carve-out (CLAUDE.md §13): the in-memory
	// StateStore driver backs the in-test Store; production code in
	// this lane ships no driver, and production registration happens
	// only in the internal/drivers/prod aggregator.
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// This file implements the in-test Store used by every test in this
// package: a StateStore-backed typed wrapper over the in-memory
// state.StateStore driver — the exact §4.4 shape a future production
// driver lane takes. It lives in a _test.go file on purpose: the
// package ships NO concrete persistent driver, and a test-grade Store
// is never a production default (CLAUDE.md §13).

const (
	turnRowKindPrefix      = "turn.row."
	turnSeqKind            = "turn.seq"
	turnCheckpointKind     = "turn.checkpoint"
	turnRetentionKind      = "turn.retention"
	turnFenceKind          = "turn.fence"
	turnSnapshotKind       = "turn.snapshot"
	testStoreRetentionTiny = 5 // for retention/truncation tests
)

// testStore is the in-test Store. durable simulates a restart-
// surviving backing store (a real durable driver reports true; the
// default in-memory test shape reports false — explicit restart loss).
type testStore struct {
	st      state.StateStore
	retain  int
	durable bool
	mu      sync.Mutex // serializes the load-modify-write sequences below
	closed  atomic.Bool
}

// newTestStore builds a fresh test Store over a fresh in-memory
// state.StateStore. retention <= 0 means the documented default
// (MaxRetainedTurns); durable=true simulates a restart-surviving
// backing store.
func newTestStore(retention int, durable bool) (*testStore, error) {
	st, err := openTestStateStore()
	if err != nil {
		return nil, err
	}
	if retention <= 0 {
		retention = MaxRetainedTurns
	}
	return &testStore{st: st, retain: retention, durable: durable}, nil
}

func openTestStateStore() (state.StateStore, error) {
	// The inmem driver registers itself via init(); Open by name is the
	// canonical construction path (production callers use state.Open).
	return state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
}

func (s *testStore) Durable() bool { return s.durable }

func (s *testStore) closedErr() error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	return nil
}

// fencedErr reports ErrErasureFenced when the session's STORE-LOCAL
// erasure fence record is present. Caller holds s.mu (FenceSession
// writes the record under the same lock, so the check and any write
// are serialized — a real driver must do the same check in the same
// transaction as its write).
func (s *testStore) fencedErr(ctx context.Context, id identity.Identity) error {
	q := identity.Quadruple{Identity: id}
	_, err := s.st.Load(ctx, q, turnFenceKind)
	if err == nil {
		return ErrErasureFenced
	}
	if !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("turns: fence load: %w", err)
	}
	return nil
}

// rowKey returns the StateStore slot for one turn row.
func rowKey(id identity.Identity, turnID TurnID) (identity.Quadruple, string) {
	return identity.Quadruple{Identity: id}, turnRowKindPrefix + string(turnID)
}

func (s *testStore) loadRow(ctx context.Context, id identity.Identity, turnID TurnID) (TurnRow, state.StateRecord, error) {
	q, kind := rowKey(id, turnID)
	rec, err := s.st.Load(ctx, q, kind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return TurnRow{}, state.StateRecord{}, ErrTurnNotFound
		}
		return TurnRow{}, state.StateRecord{}, err
	}
	var row TurnRow
	if err := json.Unmarshal(rec.Bytes, &row); err != nil {
		return TurnRow{}, state.StateRecord{}, fmt.Errorf("turns: row %q decode: %w", turnID, err)
	}
	return row, rec, nil
}

func (s *testStore) AppendTurnIf(ctx context.Context, id identity.Identity, row TurnRow) (TurnRow, error) {
	if err := s.closedErr(); err != nil {
		return TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return TurnRow{}, ErrIdentityRequired
	}
	if row.TurnID == "" {
		return TurnRow{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Store-local erasure fence first, always: an erased session
	//    admits no turn write.
	if err := s.fencedErr(ctx, id); err != nil {
		return TurnRow{}, err
	}
	// 2. Idempotent append: an existing row returns unchanged (a
	//    replay no-op — never an error).
	if existing, _, err := s.loadRow(ctx, id, row.TurnID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrTurnNotFound) {
		return TurnRow{}, err
	}
	// 3. Mint the next immutable per-session sequence atomically (CAS
	//    loop over the seq slot).
	seq, err := s.mintSeq(ctx, id)
	if err != nil {
		return TurnRow{}, err
	}
	row.Sequence = seq
	row.TieBreaker = row.TurnID
	row.Version = 1
	// 4. Create the row atomically with the fence (the guard against a
	//    racing erasure between the fence check and the write: the row
	//    slot must be absent AND the local fence slot must be absent).
	q, kind := rowKey(id, row.TurnID)
	bytes, err := json.Marshal(row)
	if err != nil {
		return TurnRow{}, fmt.Errorf("turns: row %q encode: %w", row.TurnID, err)
	}
	fq := identity.Quadruple{Identity: id}
	eid := state.NewEventID()
	err = s.st.SaveIf(ctx,
		[]state.SlotExpectation{
			{Identity: q, Kind: kind, ExpectedEventID: ""},           // row slot absent
			{Identity: fq, Kind: turnFenceKind, ExpectedEventID: ""}, // local fence absent
		},
		state.StateRecord{ID: eid, Identity: q, Kind: kind, Bytes: bytes},
	)
	if errors.Is(err, state.ErrConditionFailed) {
		// A racing append won the slot; the idempotent path returns it.
		existing, _, lerr := s.loadRow(ctx, id, row.TurnID)
		if lerr != nil {
			return TurnRow{}, fmt.Errorf("turns: append %q race: %w", row.TurnID, lerr)
		}
		return existing, nil
	}
	if err != nil {
		return TurnRow{}, err
	}
	// 5. Retention: evict the oldest rows past the bound; the eviction
	//    is surfaced as the session's explicit truncation flag.
	if err := s.enforceRetention(ctx, id); err != nil {
		return TurnRow{}, err
	}
	return row, nil
}

// mintSeq atomically increments the session's sequence counter and
// returns the new value. Caller holds s.mu.
func (s *testStore) mintSeq(ctx context.Context, id identity.Identity) (Seq, error) {
	q := identity.Quadruple{Identity: id}
	for {
		cur, err := s.loadSeq(ctx, id)
		if err != nil {
			return 0, err
		}
		next := cur + 1
		bytes := make([]byte, 8)
		binary.BigEndian.PutUint64(bytes, next)
		eid := state.NewEventID()
		expect := []state.SlotExpectation{}
		if cur == 0 {
			expect = append(expect, state.SlotExpectation{Identity: q, Kind: turnSeqKind, ExpectedEventID: ""})
		} else {
			rec, rerr := s.st.Load(ctx, q, turnSeqKind)
			if rerr != nil {
				return 0, rerr
			}
			expect = append(expect, state.SlotExpectation{Identity: q, Kind: turnSeqKind, ExpectedEventID: rec.ID})
		}
		err = s.st.SaveIf(ctx, expect, state.StateRecord{ID: eid, Identity: q, Kind: turnSeqKind, Bytes: bytes})
		if err == nil {
			return Seq(next), nil
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return 0, err
		}
		// Concurrent mint — reload and retry.
	}
}

func (s *testStore) loadSeq(ctx context.Context, id identity.Identity) (uint64, error) {
	q := identity.Quadruple{Identity: id}
	rec, err := s.st.Load(ctx, q, turnSeqKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if len(rec.Bytes) != 8 {
		return 0, fmt.Errorf("turns: seq record malformed")
	}
	return binary.BigEndian.Uint64(rec.Bytes), nil
}

// enforceRetention evicts the session's oldest rows past s.retain and
// sets the explicit truncation flag. Caller holds s.mu.
func (s *testStore) enforceRetention(ctx context.Context, id identity.Identity) error {
	rows, err := s.enumerate(ctx, id)
	if err != nil {
		return err
	}
	if len(rows) <= s.retain {
		return nil
	}
	evict := len(rows) - s.retain
	sort.Slice(rows, func(i, j int) bool { // oldest first
		if rows[i].Sequence != rows[j].Sequence {
			return rows[i].Sequence < rows[j].Sequence
		}
		return rows[i].TurnID < rows[j].TurnID
	})
	for _, row := range rows[:evict] {
		q, kind := rowKey(id, row.TurnID)
		if err := s.st.Delete(ctx, q, kind); err != nil {
			return err
		}
	}
	return s.setTruncated(ctx, id, true, evict)
}

func (s *testStore) setTruncated(ctx context.Context, id identity.Identity, truncated bool, evicted int) error {
	q := identity.Quadruple{Identity: id}
	var cur truncationMarker
	if rec, err := s.st.Load(ctx, q, turnRetentionKind); err == nil {
		_ = json.Unmarshal(rec.Bytes, &cur)
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	cur.Truncated = cur.Truncated || truncated
	cur.Evicted += evicted
	bytes, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	eid := state.NewEventID()
	return s.st.Save(ctx, state.StateRecord{ID: eid, Identity: q, Kind: turnRetentionKind, Bytes: bytes})
}

func (s *testStore) loadTruncated(ctx context.Context, id identity.Identity) (bool, error) {
	q := identity.Quadruple{Identity: id}
	rec, err := s.st.Load(ctx, q, turnRetentionKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	var cur truncationMarker
	if err := json.Unmarshal(rec.Bytes, &cur); err != nil {
		return false, err
	}
	return cur.Truncated, nil
}

type truncationMarker struct {
	Truncated bool
	Evicted   int
}

// mutate applies the UpdateTurnIf / SealTurnIf conditional-write
// pattern: local-fence check, load, guard, conditional save, and a
// bounded retry that re-evaluates the guard when a concurrent writer
// wins.
func (s *testStore) mutate(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow, sealed bool) (TurnRow, error) {
	q, kind := rowKey(id, turnID)
	fq := identity.Quadruple{Identity: id}
	for range 3 {
		// The store-local erasure fence is checked in the same
		// serialized section as the write (a real driver checks it in
		// the same transaction).
		if err := s.fencedErr(ctx, id); err != nil {
			return TurnRow{}, err
		}
		current, rec, err := s.loadRow(ctx, id, turnID)
		if err != nil {
			return TurnRow{}, err
		}
		if current.Sealed {
			return TurnRow{}, fmt.Errorf("%w: %q", ErrTurnSealed, turnID)
		}
		if current.Version != expectedVersion {
			return TurnRow{}, fmt.Errorf("%w: stored version %d, expected %d", ErrStaleVersion, current.Version, expectedVersion)
		}
		next := row
		next.TurnID = turnID
		next.Sequence = current.Sequence // immutable
		next.TieBreaker = current.TieBreaker
		next.Sealed = sealed
		next.Version = current.Version + 1
		bytes, err := json.Marshal(next)
		if err != nil {
			return TurnRow{}, err
		}
		eid := state.NewEventID()
		err = s.st.SaveIf(ctx,
			[]state.SlotExpectation{
				{Identity: q, Kind: kind, ExpectedEventID: rec.ID},
				{Identity: fq, Kind: turnFenceKind, ExpectedEventID: ""},
			},
			state.StateRecord{ID: eid, Identity: q, Kind: kind, Bytes: bytes},
		)
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return TurnRow{}, err
		}
		// A concurrent write or a racing erasure — re-evaluate the
		// guard on the next attempt.
	}
	return TurnRow{}, fmt.Errorf("turns: conditional write did not converge")
}

func (s *testStore) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow) (TurnRow, error) {
	if err := s.closedErr(); err != nil {
		return TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return TurnRow{}, ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(ctx, id, turnID, expectedVersion, row, false)
}

func (s *testStore) SealTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow) (TurnRow, error) {
	if err := s.closedErr(); err != nil {
		return TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return TurnRow{}, ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(ctx, id, turnID, expectedVersion, row, true)
}

// FenceSession marks id's session as erasure-fenced by writing the
// store-local durable fence record. Idempotent: an already-fenced
// session stays fenced (a no-op, never an error). DeleteScope never
// removes this record — an erased session stays fenced.
func (s *testStore) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := s.closedErr(); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := identity.Quadruple{Identity: id}
	if _, err := s.st.Load(ctx, q, turnFenceKind); err == nil {
		return nil // already fenced: idempotent no-op
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	eid := state.NewEventID()
	return s.st.Save(ctx, state.StateRecord{ID: eid, Identity: q, Kind: turnFenceKind, Bytes: []byte(`{}`)})
}

func (s *testStore) GetTurn(ctx context.Context, id identity.Identity, turnID TurnID) (TurnRow, error) {
	if err := s.closedErr(); err != nil {
		return TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return TurnRow{}, ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, _, err := s.loadRow(ctx, id, turnID)
	return row, err
}

func (s *testStore) ListTurns(ctx context.Context, id identity.Identity, before *Cursor, limit int) ([]TurnRow, *Cursor, ListPageInfo, error) {
	var zero ListPageInfo
	if err := s.closedErr(); err != nil {
		return nil, nil, zero, err
	}
	if err := identity.Validate(id); err != nil {
		return nil, nil, zero, ErrIdentityRequired
	}
	if limit < 1 {
		return nil, nil, zero, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot, err := s.loadSnapshotGen(ctx, id)
	if err != nil {
		return nil, nil, zero, err
	}
	// Opaque-cursor BINDING: the cursor is only valid for this session,
	// against this projection snapshot, with a retained boundary row
	// whose immutable sequence matches the cursor's.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, zero, fmt.Errorf("%w: cursor names session %q, request is %q",
				ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, zero, fmt.Errorf("%w: cursor snapshot %d, current %d",
				ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		boundary, _, err := s.loadRow(ctx, id, before.TurnID)
		if err != nil {
			if errors.Is(err, ErrTurnNotFound) {
				return nil, nil, zero, fmt.Errorf("%w: boundary row %q is no longer retained",
					ErrCursorExpired, before.TurnID)
			}
			return nil, nil, zero, err
		}
		// The cursor is BOUND to the AUTHORITATIVE boundary row: a
		// forged / altered cursor that names a retained row but carries
		// a sequence that does not equal the stored row's immutable
		// sequence is refused with ErrInvalidCursor — the keyset filter
		// would otherwise page from a sequence no stored row owns,
		// silently skipping or repeating rows.
		if boundary.Sequence != before.Seq {
			return nil, nil, zero, fmt.Errorf("%w: cursor sequence %d does not match the stored boundary row %q (sequence %d) — forged or altered cursor",
				ErrInvalidCursor, before.Seq, before.TurnID, boundary.Sequence)
		}
	}

	rows, err := s.enumerate(ctx, id)
	if err != nil {
		return nil, nil, zero, err
	}
	sort.Slice(rows, func(i, j int) bool { // newest first
		if rows[i].Sequence != rows[j].Sequence {
			return rows[i].Sequence > rows[j].Sequence
		}
		return rows[i].TurnID > rows[j].TurnID
	})
	var candidates []TurnRow
	for _, r := range rows {
		if before != nil && !after(before, r) {
			continue
		}
		candidates = append(candidates, r)
	}
	truncated, err := s.loadTruncated(ctx, id)
	if err != nil {
		return nil, nil, zero, err
	}
	info := ListPageInfo{Snapshot: snapshot, Truncated: truncated}
	if len(candidates) <= limit {
		info.Remaining = 0
		info.CountExact = true
		return candidates, nil, info, nil
	}
	page := candidates[:limit]
	last := page[len(page)-1]
	info.Remaining = len(candidates) - limit // exact: the older retained rows beyond this page
	info.CountExact = true
	next := &Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	return page, next, info, nil
}

// loadSnapshotGen returns the session's projection snapshot generation
// (0 when never advanced — a fresh session; the initial generation).
// Caller holds s.mu.
func (s *testStore) loadSnapshotGen(ctx context.Context, id identity.Identity) (uint64, error) {
	q := identity.Quadruple{Identity: id}
	rec, err := s.st.Load(ctx, q, turnSnapshotKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if len(rec.Bytes) != 8 {
		return 0, fmt.Errorf("turns: snapshot record malformed")
	}
	return binary.BigEndian.Uint64(rec.Bytes), nil
}

// bumpSnapshotGen advances the session's projection snapshot
// generation by one (used by DeleteScope so a cursor minted before an
// erase can never be confused with one minted after). Caller holds
// s.mu.
func (s *testStore) bumpSnapshotGen(ctx context.Context, id identity.Identity) error {
	q := identity.Quadruple{Identity: id}
	cur, err := s.loadSnapshotGen(ctx, id)
	if err != nil {
		return err
	}
	bytes := make([]byte, 8)
	binary.BigEndian.PutUint64(bytes, cur+1)
	eid := state.NewEventID()
	return s.st.Save(ctx, state.StateRecord{ID: eid, Identity: q, Kind: turnSnapshotKind, Bytes: bytes})
}

// after reports whether r is strictly older than c in the newest-first
// keyset order (Sequence DESC, TurnID DESC).
func after(c *Cursor, r TurnRow) bool {
	if r.Sequence != c.Seq {
		return r.Sequence < c.Seq
	}
	return r.TurnID < c.TurnID
}

func (s *testStore) enumerate(ctx context.Context, id identity.Identity) ([]TurnRow, error) {
	q := identity.Quadruple{Identity: id}
	recs, err := s.st.ListKindForIdentity(ctx, q, turnRowKindPrefix)
	if err != nil {
		return nil, err
	}
	rows := make([]TurnRow, 0, len(recs))
	for _, rec := range recs {
		var row TurnRow
		if err := json.Unmarshal(rec.Bytes, &row); err != nil {
			return nil, fmt.Errorf("turns: row decode: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *testStore) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if err := s.closedErr(); err != nil {
		return 0, err
	}
	if err := identity.Validate(id); err != nil {
		return 0, ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := identity.Quadruple{Identity: id}
	rec, err := s.st.Load(ctx, q, turnCheckpointKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if len(rec.Bytes) != 8 {
		return 0, fmt.Errorf("turns: checkpoint record malformed")
	}
	return binary.BigEndian.Uint64(rec.Bytes), nil
}

func (s *testStore) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if err := s.closedErr(); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A fenced (erased) session must never advance its checkpoint — no
	// resurrection after replay / restart.
	if err := s.fencedErr(ctx, id); err != nil {
		return err
	}
	q := identity.Quadruple{Identity: id}
	for {
		cur, err := s.loadCheckpointRaw(ctx, id)
		if err != nil {
			return err
		}
		if seq <= cur {
			return nil // monotonic idempotent: never regress
		}
		bytes := make([]byte, 8)
		binary.BigEndian.PutUint64(bytes, seq)
		eid := state.NewEventID()
		expect := []state.SlotExpectation{
			{Identity: q, Kind: turnFenceKind, ExpectedEventID: ""},
		}
		if cur == 0 {
			expect = append(expect, state.SlotExpectation{Identity: q, Kind: turnCheckpointKind, ExpectedEventID: ""})
		} else {
			rec, rerr := s.st.Load(ctx, q, turnCheckpointKind)
			if rerr != nil {
				return rerr
			}
			expect = append(expect, state.SlotExpectation{Identity: q, Kind: turnCheckpointKind, ExpectedEventID: rec.ID})
		}
		err = s.st.SaveIf(ctx, expect, state.StateRecord{ID: eid, Identity: q, Kind: turnCheckpointKind, Bytes: bytes})
		if err == nil {
			return nil
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return err
		}
		// Concurrent save or a racing erasure — reload and retry
		// (converges to the max).
	}
}

func (s *testStore) loadCheckpointRaw(ctx context.Context, id identity.Identity) (uint64, error) {
	q := identity.Quadruple{Identity: id}
	rec, err := s.st.Load(ctx, q, turnCheckpointKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if len(rec.Bytes) != 8 {
		return 0, fmt.Errorf("turns: checkpoint record malformed")
	}
	return binary.BigEndian.Uint64(rec.Bytes), nil
}

func (s *testStore) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if err := s.closedErr(); err != nil {
		return 0, err
	}
	if err := identity.Validate(id); err != nil {
		return 0, ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Delete ONLY this projection's owned records (rows, checkpoint,
	// sequence, retention marker). The erasure FENCE record
	// (turnFenceKind) is deliberately NEVER deleted: the erasure
	// cascade sets it via FenceSession BEFORE calling DeleteScope, and
	// the fence must survive the erasure so an erased session stays
	// fenced (no resurrection after replay / restart). A real driver
	// keeps the fence in its own table for the same reason — this
	// models exactly that shape instead of delegating to the
	// kind-agnostic StateStore cascade, which would wipe the fence.
	q := identity.Quadruple{Identity: id}
	deleted := 0
	recs, err := s.st.ListKindForIdentity(ctx, q, turnRowKindPrefix)
	if err != nil {
		return 0, err
	}
	for _, rec := range recs {
		if err := s.st.Delete(ctx, q, rec.Kind); err != nil {
			return 0, err
		}
		deleted++
	}
	for _, kind := range []string{turnCheckpointKind, turnSeqKind, turnRetentionKind} {
		if _, err := s.st.Load(ctx, q, kind); err == nil {
			if err := s.st.Delete(ctx, q, kind); err != nil {
				return 0, err
			}
			deleted++
		} else if !errors.Is(err, state.ErrNotFound) {
			return 0, err
		}
	}
	// Advance the projection SNAPSHOT generation (as-of retention
	// generation) so any cursor minted before the erase is rejected as
	// stale — a pre-erase cursor must never page the post-erase (or
	// rebuilt) projection. Not counted as a deleted record.
	if err := s.bumpSnapshotGen(ctx, id); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *testStore) Close(ctx context.Context) error {
	s.closed.Store(true)
	return s.st.Close(ctx)
}
