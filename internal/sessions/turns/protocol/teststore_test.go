package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// This file provides the test-only in-memory turns.Store backing the
// REAL *turns.Projector in the service tests: the Service is exercised
// through the production domain core (cursor binding, page building,
// not-found semantics) rather than a hand-rolled read double.

// scopeKey is the storage key of one identity triple.
type scopeKey struct {
	tenant, user, session string
}

// memStore is a minimal, concurrency-safe, JSON-backed in-memory
// turns.Store implementing the Store contract's read surface (and
// enough of the write surface for the projector's Append path). Rows
// are deep-copied through JSON on every boundary so concurrent readers
// can never alias durable state.
type memStore struct {
	mu         sync.Mutex
	closed     bool
	durable    bool
	rows       map[scopeKey]map[turns.TurnID][]byte
	seq        map[scopeKey]int64
	snapshot   map[scopeKey]uint64
	truncated  map[scopeKey]bool
	checkpoint map[scopeKey]uint64
	fenced     map[scopeKey]bool
}

func newMemStore(durable bool) *memStore {
	return &memStore{
		durable:    durable,
		rows:       make(map[scopeKey]map[turns.TurnID][]byte),
		seq:        make(map[scopeKey]int64),
		snapshot:   make(map[scopeKey]uint64),
		truncated:  make(map[scopeKey]bool),
		checkpoint: make(map[scopeKey]uint64),
		fenced:     make(map[scopeKey]bool),
	}
}

func (s *memStore) sk(id identity.Identity) scopeKey {
	return scopeKey{id.TenantID, id.UserID, id.SessionID}
}

func (s *memStore) closedErr() error {
	if s.closed {
		return turns.ErrStoreClosed
	}
	return nil
}

func (s *memStore) Durable() bool { return s.durable }

func (s *memStore) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// deepCopyBytes round-trips a row through JSON — the structural deep
// copy the Store contract requires at every boundary.
func deepCopyBytes(b []byte) (turns.TurnRow, error) {
	var row turns.TurnRow
	if err := json.Unmarshal(b, &row); err != nil {
		return turns.TurnRow{}, fmt.Errorf("memstore: row decode: %w", err)
	}
	return row, nil
}

func (s *memStore) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	if s.fenced[s.sk(id)] {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}
	k := s.sk(id)
	if s.rows[k] == nil {
		s.rows[k] = make(map[turns.TurnID][]byte)
	}
	if existing, ok := s.rows[k][row.TurnID]; ok {
		return deepCopyBytes(existing) // idempotent replay: no-op
	}
	row.Sequence = turns.Seq(s.seq[k])
	s.seq[k]++
	row.TieBreaker = row.TurnID
	raw, err := json.Marshal(row)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("memstore: row encode: %w", err)
	}
	s.rows[k][row.TurnID] = raw
	return deepCopyBytes(raw)
}

// UpdateTurnIf implements the Store contract minimally: it refuses
// sealed rows, refuses stale expected versions, and bumps Version.
// The service under test never writes, but the projector interface
// requires the full surface.
func (s *memStore) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	if s.fenced[s.sk(id)] {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}
	k := s.sk(id)
	existing, ok := s.rows[k][turnID]
	if !ok {
		return turns.TurnRow{}, turns.ErrTurnNotFound
	}
	current, err := deepCopyBytes(existing)
	if err != nil {
		return turns.TurnRow{}, err
	}
	if current.Sealed {
		return turns.TurnRow{}, turns.ErrTurnSealed
	}
	if current.Version != expectedVersion {
		return turns.TurnRow{}, turns.ErrStaleVersion
	}
	row.Sequence = current.Sequence
	row.TieBreaker = current.TieBreaker
	row.Version = current.Version + 1
	raw, err := json.Marshal(row)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("memstore: row encode: %w", err)
	}
	s.rows[k][turnID] = raw
	return deepCopyBytes(raw)
}

// SealTurnIf replaces a mutable row with its sealed terminal form.
func (s *memStore) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	row.Sealed = true
	return s.UpdateTurnIf(ctx, id, turnID, expectedVersion, row)
}

func (s *memStore) FenceSession(ctx context.Context, id identity.Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	s.fenced[s.sk(id)] = true
	return nil
}

func (s *memStore) GetTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	raw, ok := s.rows[s.sk(id)][turnID]
	if !ok {
		return turns.TurnRow{}, turns.ErrTurnNotFound
	}
	return deepCopyBytes(raw)
}

func (s *memStore) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, turns.ListPageInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return nil, nil, turns.ListPageInfo{}, err
	}
	if err := identity.Validate(id); err != nil {
		return nil, nil, turns.ListPageInfo{}, turns.ErrIdentityRequired
	}
	if limit < 1 {
		return nil, nil, turns.ListPageInfo{}, turns.ErrInvalidInput
	}
	k := s.sk(id)
	snapshot := s.snapshot[k]

	// Opaque-cursor BINDING (mirrors the conformance contract): the
	// cursor is only valid for this session, against this projection
	// snapshot, with a retained boundary row whose immutable sequence
	// matches the cursor's.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, turns.ListPageInfo{}, fmt.Errorf("%w: cursor names session %q, request is %q",
				turns.ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, turns.ListPageInfo{}, fmt.Errorf("%w: cursor snapshot %d, current %d",
				turns.ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		raw, ok := s.rows[k][before.TurnID]
		if !ok {
			return nil, nil, turns.ListPageInfo{}, fmt.Errorf("%w: boundary row %q is no longer retained",
				turns.ErrCursorExpired, before.TurnID)
		}
		boundary, err := deepCopyBytes(raw)
		if err != nil {
			return nil, nil, turns.ListPageInfo{}, err
		}
		if boundary.Sequence != before.Seq {
			return nil, nil, turns.ListPageInfo{}, fmt.Errorf("%w: cursor sequence %d does not match the stored boundary row %q (sequence %d) — forged or altered cursor",
				turns.ErrInvalidCursor, before.Seq, before.TurnID, boundary.Sequence)
		}
	}

	rows := make([]turns.TurnRow, 0, len(s.rows[k]))
	for _, raw := range s.rows[k] {
		row, err := deepCopyBytes(raw)
		if err != nil {
			return nil, nil, turns.ListPageInfo{}, err
		}
		rows = append(rows, row)
	}
	// Newest first by the immutable keys.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Sequence != rows[j].Sequence {
			return rows[i].Sequence > rows[j].Sequence
		}
		return rows[i].TurnID > rows[j].TurnID
	})
	var candidates []turns.TurnRow
	for _, r := range rows {
		if before != nil && !olderThan(before, r) {
			continue
		}
		candidates = append(candidates, r)
	}

	info := turns.ListPageInfo{Snapshot: snapshot, Truncated: s.truncated[k]}
	if len(candidates) <= limit {
		info.Remaining = 0
		info.CountExact = true
		return candidates, nil, info, nil
	}
	page := candidates[:limit]
	last := page[len(page)-1]
	info.Remaining = len(candidates) - limit
	info.CountExact = true
	next := &turns.Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	return page, next, info, nil
}

// olderThan reports whether r is strictly older than c in the
// newest-first keyset order (Sequence DESC, TurnID DESC).
func olderThan(c *turns.Cursor, r turns.TurnRow) bool {
	if r.Sequence != c.Seq {
		return r.Sequence < c.Seq
	}
	return r.TurnID < c.TurnID
}

func (s *memStore) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return 0, err
	}
	if err := identity.Validate(id); err != nil {
		return 0, turns.ErrIdentityRequired
	}
	return s.checkpoint[s.sk(id)], nil
}

func (s *memStore) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	k := s.sk(id)
	if s.fenced[k] {
		return turns.ErrErasureFenced
	}
	if seq <= s.checkpoint[k] {
		return nil // monotonic idempotent: never regress
	}
	s.checkpoint[k] = seq
	return nil
}

func (s *memStore) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return 0, err
	}
	if err := identity.Validate(id); err != nil {
		return 0, turns.ErrIdentityRequired
	}
	k := s.sk(id)
	n := len(s.rows[k])
	delete(s.rows, k)
	delete(s.checkpoint, k)
	s.snapshot[k]++ // erasure advances the projection snapshot generation
	delete(s.truncated, k)
	// The FENCE is deliberately NOT removed: an erased session stays
	// fenced (no resurrection).
	return n, nil
}

// ---- test-only seeding surface (not part of turns.Store) ----

// seedRow stores a row VERBATIM under id — the test fixture path that
// bypasses the projector's write validation so tests can plant exact
// rows (terminal / paused / agent-bound / etc.). When row.Sequence is
// zero the next per-session sequence is minted, mirroring
// AppendTurnIf. Returns a deep copy.
func (s *memStore) seedRow(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := identity.Validate(id); err != nil {
		return turns.TurnRow{}, turns.ErrIdentityRequired
	}
	k := s.sk(id)
	if s.rows[k] == nil {
		s.rows[k] = make(map[turns.TurnID][]byte)
	}
	row.SessionID = id.SessionID
	if row.Sequence == 0 {
		row.Sequence = turns.Seq(s.seq[k])
		s.seq[k]++
	}
	if row.TieBreaker == "" {
		row.TieBreaker = row.TurnID
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return turns.TurnRow{}, fmt.Errorf("memstore: row encode: %w", err)
	}
	s.rows[k][row.TurnID] = raw
	return deepCopyBytes(raw)
}

// setTruncated flags the session's retained window as truncated
// (retention eviction) — the honest partial-page fixture.
func (s *memStore) setTruncated(id identity.Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncated[s.sk(id)] = true
}

// erase simulates the projection erasure cascade: fence + delete.
func (s *memStore) erase(ctx context.Context, id identity.Identity) error {
	if err := s.FenceSession(ctx, id); err != nil {
		return err
	}
	_, err := s.DeleteScope(ctx, id)
	return err
}

// mustProjector builds a production *turns.Projector over the
// in-memory store for service tests.
func mustProjector(t *testing.T, st *memStore) *turns.Projector {
	t.Helper()
	p, err := turns.New(st)
	if err != nil {
		t.Fatalf("turns.New: %v", err)
	}
	return p
}

// mustRow is the minimal assertion-aware seed helper: it plants one
// fixture row (with a fresh sequence) and returns the stored copy.
func mustSeedRow(t *testing.T, st *memStore, id identity.Identity, row turns.TurnRow) turns.TurnRow {
	t.Helper()
	stored, err := st.seedRow(context.Background(), id, row)
	if err != nil {
		t.Fatalf("seedRow: %v", err)
	}
	return stored
}

// errIs is a tiny helper for table assertions that use errors.Is.
func errIs(err, target error) bool { return errors.Is(err, target) }
