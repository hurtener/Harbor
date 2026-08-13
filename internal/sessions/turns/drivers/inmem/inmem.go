// Package inmem is Harbor's V1 in-memory turns.Store driver — the
// reference driver the conformance suite
// (`internal/sessions/turns/conformancetest`) gates. Every later
// durable driver (SQLite, Postgres) inherits the same suite verbatim.
//
// Internal model:
//
//   - A primary map keyed on the exact identity triple
//     `(tenant, user, session)` plus the turn id holds the retained
//     rows. The turn id keys rows WITHIN the triple; run id and agent
//     id are row DATA, never storage axes — the isolation boundary is
//     and stays the triple (§6).
//   - A per-session ordered TAIL index (`sessionState.tail`) holds the
//     retained turn ids, oldest first (ascending immutable sequence).
//     ListTurns walks that index from the newest end — it never scans
//     the raw history, the global map, or any other session's rows —
//     so paging is bounded by the session's retention bound and by the
//     page limit, and an append during a walk only ever extends the
//     tail (a newer row can never satisfy an already-issued keyset
//     cursor).
//   - A single `sync.RWMutex` guards both maps plus every session's
//     sequence counter, checkpoint, projection snapshot generation,
//     erasure fence, and truncation flag. The driver does no I/O, so
//     contention is bounded by Go's map throughput; a finer-grained
//     lock structure would be premature.
//   - Every row is deep-copied on every write and every read boundary
//     (answer reference, per-measure usage values, attachment / app /
//     reasoning / activity slices) so caller memory never reaches (or
//     escapes) durable state and a caller mutating a returned row can
//     never corrupt the stored row (the concurrent-reuse gate, D-025).
//   - The erasure FENCE is store-local and durable-in-driver: it is
//     retained for the life of the store (never removed by
//     DeleteScope, never cleared by Close) so an erased session stays
//     fenced against replay resurrection while this store lives.
//   - `Close(ctx)` flips an atomic flag; subsequent calls fail with
//     ErrStoreClosed. There are no driver-owned goroutines to join, so
//     Close is fast and the goroutine-leak gate passes trivially.
package inmem

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// Option configures a new in-memory Store. Options apply at
// construction only; a Store is immutable thereafter (D-025).
type Option func(*options)

type options struct {
	retention int
}

// WithRetention bounds how many of the newest turn rows a session
// retains: beyond the bound the oldest rows are evicted and the
// session's truncation flag is set (retention eviction is explicit,
// never silent). A value <= 0 selects the documented default
// (turns.MaxRetainedTurns).
func WithRetention(n int) Option {
	return func(o *options) { o.retention = n }
}

// New constructs a fresh, empty in-memory turns.Store with the
// documented default retention bound unless configured otherwise.
func New(opts ...Option) (turns.Store, error) {
	o := options{retention: turns.MaxRetainedTurns}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.retention <= 0 {
		o.retention = turns.MaxRetainedTurns
	}
	return &driver{
		retain: o.retention,
		rows:   map[rowKey]turns.TurnRow{},
		states: map[sessionKey]*sessionState{},
	}, nil
}

// rowKey is the exact storage identity of one turn row: the full
// (tenant, user, session) triple plus the turn id. Struct-typed (never
// string-concatenated) so tenant / user / session ids containing
// delimiters cannot collide.
type rowKey struct {
	tenant  string
	user    string
	session string
	turnID  string
}

func rowKeyOf(id identity.Identity, turnID turns.TurnID) rowKey {
	return rowKey{
		tenant:  id.TenantID,
		user:    id.UserID,
		session: id.SessionID,
		turnID:  string(turnID),
	}
}

// sessionKey is the exact per-session scope: the full (tenant, user,
// session) triple. Sequence counters, checkpoints, the projection
// snapshot generation, the erasure fence, and the truncation flag are
// all session-scoped.
type sessionKey struct {
	tenant  string
	user    string
	session string
}

func sessionKeyOf(id identity.Identity) sessionKey {
	return sessionKey{tenant: id.TenantID, user: id.UserID, session: id.SessionID}
}

// sessionState is the per-session bookkeeping the driver keeps
// alongside the rows: the atomic sequence counter, the monotonic
// checkpoint, the projection snapshot generation, the store-local
// erasure fence, the sticky truncation flag, and the ordered TAIL
// index of retained turn ids (oldest first).
type sessionState struct {
	// seq is the number of turns appended so far; the next minted
	// per-session sequence is seq+1 (1-based, never reused, unique
	// within the session). int64 so a minted sequence is assigned to
	// the row's turns.Seq (int64) without a narrowing conversion.
	seq int64
	// checkpoint is the session's last-applied runtime event sequence
	// (monotonic, idempotent; 0 = none ever saved).
	checkpoint uint64
	// snapshot is the session's projection snapshot generation
	// (as-of retention generation). It starts at 0 and advances on
	// DeleteScope so a cursor minted before an erase can never be
	// confused with one minted after.
	snapshot uint64
	// fenced is the STORE-LOCAL erasure fence. Set by FenceSession,
	// never cleared by DeleteScope or Close: an erased session stays
	// fenced for the life of the store (no replay resurrection).
	fenced bool
	// truncated reports whether the session's retained window ever hit
	// its bound (retention eviction is explicit, never silent). Reset
	// by DeleteScope, which erases this projection's own records.
	truncated bool
	// tail is the ordered index of retained turn ids, oldest first
	// (ascending immutable sequence). tail[i] is the (i+1)-th oldest
	// retained row; eviction drops the front, appends extend the back.
	// The authoritative sequence of each entry lives on the row itself
	// (this index is a navigation aid, not a duplicate store).
	tail []turns.TurnID
}

type driver struct {
	mu     sync.RWMutex
	retain int
	rows   map[rowKey]turns.TurnRow
	states map[sessionKey]*sessionState
	closed atomic.Bool
}

// Durable reports whether the backing store survives a process
// restart. The in-memory driver returns false: after a restart its
// projection is EMPTY — rows, checkpoints, AND erasure fences gone
// (explicit loss, never a silent claim of durability) — and the
// runtime rebuilds it by reconciling from sequence zero, gated on the
// runtime's durable erasure probe so an erased session is never
// rebuilt merely because this store restarted.
func (d *driver) Durable() bool { return false }

func (d *driver) closedErr() error {
	if d.closed.Load() {
		return turns.ErrStoreClosed
	}
	return nil
}

func (d *driver) identityErr(id identity.Identity) error {
	if err := identity.Validate(id); err != nil {
		return turns.ErrIdentityRequired
	}
	return nil
}

// stateLocked returns the session's state, creating it when absent.
// Caller holds d.mu for writing.
func (d *driver) stateLocked(id identity.Identity) *sessionState {
	k := sessionKeyOf(id)
	st, ok := d.states[k]
	if !ok {
		st = &sessionState{}
		d.states[k] = st
	}
	return st
}

// stateRead returns the session's state, or nil when the session was
// never touched. Caller holds d.mu for reading (or writing).
func (d *driver) stateRead(id identity.Identity) *sessionState {
	return d.states[sessionKeyOf(id)]
}

// AppendTurnIf creates the mutable row for id / TurnID, minting the
// next immutable per-session sequence atomically with the write.
// Idempotent on the turn id: an existing row is returned unchanged (a
// replay of an already-applied append is a no-op, never an error).
// Refused with ErrErasureFenced when the session is fenced and
// ErrStoreClosed after Close. The driver may evict the session's
// oldest rows beyond its retention bound, setting the session's
// truncation flag.
func (d *driver) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	if row.TurnID == "" {
		return turns.TurnRow{}, fmt.Errorf("%w: empty turn id", turns.ErrInvalidInput)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	// The store-local erasure fence is checked in the same serialized
	// section as the write (a real driver checks it in the same
	// transaction as its row write).
	st := d.stateLocked(id)
	if st.fenced {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}
	k := rowKeyOf(id, row.TurnID)
	if existing, ok := d.rows[k]; ok {
		return cloneRow(existing), nil // idempotent replay no-op
	}
	st.seq++
	row.Sequence = turns.Seq(st.seq)
	row.TieBreaker = row.TurnID
	row.Version = 1
	// Deep-copy on the write boundary: caller memory never becomes
	// durable state.
	d.rows[k] = cloneRow(row)
	st.tail = append(st.tail, row.TurnID)
	// Retention: evict the oldest rows past the bound; the eviction is
	// surfaced as the session's explicit truncation flag.
	if over := len(st.tail) - d.retain; over > 0 {
		for _, tid := range st.tail[:over] {
			delete(d.rows, rowKeyOf(id, tid))
		}
		st.tail = append([]turns.TurnID(nil), st.tail[over:]...)
		st.truncated = true
	}
	return cloneRow(row), nil
}

// mutate is the shared UpdateTurnIf / SealTurnIf conditional-write
// path: fence check, load, guard (sealed / stale version), replace
// with the immutable ordering keys preserved, deep-copied both into
// the store and out. Caller holds d.mu.
func (d *driver) mutate(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow, sealed bool) (turns.TurnRow, error) {
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	st := d.stateLocked(id)
	if st.fenced {
		return turns.TurnRow{}, turns.ErrErasureFenced
	}
	k := rowKeyOf(id, turnID)
	current, ok := d.rows[k]
	if !ok {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	if current.Sealed {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnSealed, turnID)
	}
	if current.Version != expectedVersion {
		return turns.TurnRow{}, fmt.Errorf("%w: stored version %d, expected %d",
			turns.ErrStaleVersion, current.Version, expectedVersion)
	}
	next := cloneRow(row)
	next.TurnID = turnID
	next.Sequence = current.Sequence // immutable ordering key
	next.TieBreaker = current.TieBreaker
	next.Sealed = sealed
	next.Version = current.Version + 1
	d.rows[k] = cloneRow(next)
	return cloneRow(next), nil
}

// UpdateTurnIf atomically replaces a MUTABLE row at an expected
// version. Refused with ErrStaleVersion on a version mismatch,
// ErrTurnSealed when the stored row is already sealed, ErrTurnNotFound
// when the row is not retained, ErrErasureFenced when the session is
// fenced. On success the returned row carries Version + 1.
func (d *driver) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mutate(ctx, id, turnID, expectedVersion, row, false)
}

// SealTurnIf atomically replaces a MUTABLE row with its SEALED
// terminal form. Same refusals as UpdateTurnIf; a sealed row is
// immutable thereafter. On success the returned row carries
// Sealed == true and Version + 1.
func (d *driver) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mutate(ctx, id, turnID, expectedVersion, row, true)
}

// FenceSession marks id's session as ERASURE-FENCED in the driver's
// own store, atomically with respect to the row writes it guards.
// After FenceSession, AppendTurnIf / UpdateTurnIf / SealTurnIf /
// SaveCheckpoint refuse with ErrErasureFenced, and the fence is NEVER
// removed by DeleteScope (nor by Close) — an erased session stays
// fenced for the life of the store (no resurrection). Idempotent:
// fencing an already fenced session is a no-op.
func (d *driver) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := d.closedErr(); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stateLocked(id).fenced = true // idempotent
	return nil
}

// GetTurn reads one retained row; ErrTurnNotFound when the turn was
// never created, was evicted past the retention bound, or was erased.
// Identity scoping is the store's job: a turn under a different
// (tenant, user) is not addressable from this triple. The returned row
// is a deep copy — mutating it never corrupts the stored row.
func (d *driver) GetTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	if err := d.closedErr(); err != nil {
		return turns.TurnRow{}, err
	}
	if err := d.identityErr(id); err != nil {
		return turns.TurnRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return turns.TurnRow{}, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	row, ok := d.rows[rowKeyOf(id, turnID)]
	if !ok {
		return turns.TurnRow{}, fmt.Errorf("%w: %q", turns.ErrTurnNotFound, turnID)
	}
	return cloneRow(row), nil
}

// ListTurns returns one newest-first keyset page of at most limit
// retained rows strictly older than before (nil before = the newest
// page), ordered by (Sequence DESC, TurnID DESC). The page walks the
// session's ordered TAIL index from the newest end — never the raw
// history, never the global map, never another session's rows — so a
// page is bounded by the retention bound and the page limit, and an
// append during a walk only extends the tail (a newer row can never
// satisfy an already-issued cursor). next is non-nil iff older rows
// remain; info carries the page's snapshot binding, completeness, and
// the exact older-row remaining count, computed from the bounded
// retained window (never a scan of the raw history).
//
// The cursor is BOUND to (session, projection snapshot, authoritative
// boundary row): a foreign-session cursor fails with
// ErrCursorForeignSession, a stale-snapshot cursor with
// ErrCursorSnapshotStale, a cursor whose boundary row is no longer
// retained with ErrCursorExpired, and a forged / altered cursor that
// names a retained boundary row but carries a sequence that does not
// equal the stored row's immutable sequence with ErrInvalidCursor —
// each a distinct domain error, and none ever silently re-keysets.
func (d *driver) ListTurns(ctx context.Context, id identity.Identity, before *turns.Cursor, limit int) ([]turns.TurnRow, *turns.Cursor, turns.ListPageInfo, error) {
	var zero turns.ListPageInfo
	if err := d.closedErr(); err != nil {
		return nil, nil, zero, err
	}
	if err := d.identityErr(id); err != nil {
		return nil, nil, zero, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, zero, err
	}
	if limit < 1 {
		return nil, nil, zero, fmt.Errorf("%w: limit %d", turns.ErrInvalidInput, limit)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	st := d.stateRead(id)
	var snapshot uint64
	if st != nil {
		snapshot = st.snapshot
	}
	// Opaque-cursor BINDING, enforced against the authoritative
	// boundary row at list time.
	if before != nil {
		if before.SessionID != id.SessionID {
			return nil, nil, zero, fmt.Errorf("%w: cursor names session %q, request is %q",
				turns.ErrCursorForeignSession, before.SessionID, id.SessionID)
		}
		if before.Snapshot != snapshot {
			return nil, nil, zero, fmt.Errorf("%w: cursor snapshot %d, current %d",
				turns.ErrCursorSnapshotStale, before.Snapshot, snapshot)
		}
		boundary, ok := d.rows[rowKeyOf(id, before.TurnID)]
		if !ok {
			return nil, nil, zero, fmt.Errorf("%w: boundary row %q is no longer retained",
				turns.ErrCursorExpired, before.TurnID)
		}
		// The cursor is BOUND to the AUTHORITATIVE boundary row: a
		// forged / altered cursor that names a retained row but carries
		// a sequence that does not equal the stored row's immutable
		// sequence is refused with ErrInvalidCursor — it would otherwise
		// silently skip or repeat rows.
		if boundary.Sequence != before.Seq {
			return nil, nil, zero, fmt.Errorf("%w: cursor sequence %d does not match the stored boundary row %q (sequence %d) — forged or altered cursor",
				turns.ErrInvalidCursor, before.Seq, before.TurnID, boundary.Sequence)
		}
	}
	// Indexed tail walk: collect the session's retained rows strictly
	// older than the cursor, newest first. Bounded by the session's
	// retention bound — never a scan of the raw history or of any
	// other session's rows.
	tail := stTail(st)
	candidates := make([]turns.TurnRow, 0, len(tail))
	for i := len(tail) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		row, ok := d.rows[rowKeyOf(id, tail[i])]
		if !ok {
			// The tail index and the row map are updated under one
			// lock; a retained tail entry without a row is a driver
			// invariant violation — surface it loudly.
			return nil, nil, zero, fmt.Errorf("turns inmem: tail index entry %q has no retained row", tail[i])
		}
		if before != nil && !olderThan(before, row) {
			continue
		}
		candidates = append(candidates, cloneRow(row))
	}
	var truncated bool
	if st != nil {
		truncated = st.truncated
	}
	info := turns.ListPageInfo{Snapshot: snapshot, Truncated: truncated}
	if len(candidates) <= limit {
		info.Remaining = 0
		info.CountExact = true
		return candidates, nil, info, nil
	}
	page := candidates[:limit]
	last := page[len(page)-1]
	info.Remaining = len(candidates) - limit // exact: older retained rows beyond this page
	info.CountExact = true
	next := &turns.Cursor{SessionID: id.SessionID, Snapshot: snapshot, Seq: last.Sequence, TurnID: last.TurnID}
	return page, next, info, nil
}

// stTail returns the session's tail index, or nil when the session was
// never touched.
func stTail(st *sessionState) []turns.TurnID {
	if st == nil {
		return nil
	}
	return st.tail
}

// olderThan reports whether r is strictly older than the cursor's
// boundary in the newest-first keyset order (Sequence DESC, TurnID
// DESC): (Seq < c.Seq) || (Seq == c.Seq && TurnID < c.TurnID). The
// full tie-breaker predicate is kept (defensively) so the filter stays
// total even against a driver that minted duplicate sequences — this
// driver mints unique per-session sequences, so the tie arm matches
// only the boundary row itself, which is excluded by the strict
// ordering.
func olderThan(c *turns.Cursor, r turns.TurnRow) bool {
	if r.Sequence != c.Seq {
		return r.Sequence < c.Seq
	}
	return r.TurnID < c.TurnID
}

// LoadCheckpoint returns the session's last-applied runtime event
// sequence; 0 when none was ever saved (a fresh store, an erased
// session — the erasure cleared it — or this in-memory store after a
// restart). Reads are not fenced: a fenced session's checkpoint is
// still readable (it reads 0 after erasure).
func (d *driver) LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error) {
	if err := d.closedErr(); err != nil {
		return 0, err
	}
	if err := d.identityErr(id); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if st := d.stateRead(id); st != nil {
		return st.checkpoint, nil
	}
	return 0, nil
}

// SaveCheckpoint records the session's last-applied runtime event
// sequence. MONOTONIC and IDEMPOTENT: a sequence at or below the
// stored checkpoint is a no-op (never a regression), so a reconcile
// retry cannot rewind the checkpoint. Refused with ErrErasureFenced
// when the session is fenced — a rebuild must not advance the
// checkpoint of an erased session (no resurrection).
func (d *driver) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	if err := d.closedErr(); err != nil {
		return err
	}
	if err := d.identityErr(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stateLocked(id)
	if st.fenced {
		return turns.ErrErasureFenced
	}
	if seq > st.checkpoint {
		st.checkpoint = seq // monotonic: never regress
	}
	return nil
}

// DeleteScope removes every retained turn row and the checkpoint under
// id (the erasure cascade's projection leg). Idempotent: an absent
// scope returns (0, nil). The erasure FENCE is NOT removed — the
// caller (Projector.Erase) sets it via FenceSession before calling
// DeleteScope, and this method deliberately never clears it, so an
// erased session stays fenced for the life of the store. This method
// only clears this projection's own records (rows, checkpoint,
// sequence counter, truncation marker) and advances the projection
// snapshot generation so any cursor minted before the erase is
// rejected as stale.
func (d *driver) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if err := d.closedErr(); err != nil {
		return 0, err
	}
	if err := d.identityErr(id); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stateRead(id)
	if st == nil {
		return 0, nil // idempotent absent scope
	}
	deleted := 0
	for _, tid := range st.tail {
		delete(d.rows, rowKeyOf(id, tid))
		deleted++
	}
	st.tail = nil
	if st.checkpoint != 0 {
		st.checkpoint = 0
		deleted++
	}
	if st.seq != 0 {
		st.seq = 0
		deleted++
	}
	if st.truncated {
		st.truncated = false
		deleted++
	}
	// Advance the projection SNAPSHOT generation so a cursor minted
	// before the erase is rejected as stale. Not counted as a deleted
	// record.
	st.snapshot++
	return deleted, nil
}

// Close releases driver resources; subsequent calls fail with
// ErrStoreClosed. Idempotent. The in-memory driver spawns no
// goroutines, so there is nothing to join. The erasure fences are
// retained for the life of the store object (they simply become
// unreachable with it).
func (d *driver) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.closed.Store(true)
	return nil
}

// cloneRow returns a deep copy of a turn row so the driver never lets
// caller memory reach (or escape) durable state: every slice and every
// optional pointer-backed mutable field (Answer.Ref, UsageMeasure.Value)
// is copied. This is the store contract's deep-copy obligation, pinned
// by the conformance suite's Row_DeepCopy_NoAliasing subtest.
func cloneRow(r turns.TurnRow) turns.TurnRow {
	out := r
	if out.Answer.Ref != nil {
		ref := *out.Answer.Ref
		out.Answer.Ref = &ref
	}
	out.Inputs = append([]turns.Attachment(nil), out.Inputs...)
	out.Outputs = append([]turns.Attachment(nil), out.Outputs...)
	out.Apps = append([]turns.AppRef(nil), out.Apps...)
	out.Reasoning.Steps = append([]turns.ReasoningStep(nil), out.Reasoning.Steps...)
	out.Activity.Rows = append([]turns.ActivityRow(nil), out.Activity.Rows...)
	measures := []*turns.UsageMeasure{
		&out.Usage.PromptTokens, &out.Usage.CompletionTokens, &out.Usage.ReasoningTokens,
		&out.Usage.CacheReadTokens, &out.Usage.CacheWriteTokens, &out.Usage.TotalTokens,
		&out.Usage.CostMicroUSD, &out.Usage.LatencyNS,
	}
	for _, m := range measures {
		if m.Value != nil {
			v := *m.Value
			m.Value = &v
		}
	}
	return out
}

// Compile-time assertion that driver satisfies turns.Store.
var _ turns.Store = (*driver)(nil)
