// Package memstore is the indexed in-memory implementation of the
// rollups.Store interface — the reference driver every Store-backed
// consumer and the conformancetest suite exercise. It is constructed
// directly (no registration): the production driver aggregator home is a
// wiring concern for the phase that wires the projector into the runtime.
//
// Concurrency: a *Store is a compiled artifact — immutable after
// construction — and safe for concurrent use by N goroutines against a
// single shared instance. Writes (ApplyBatch, FenceSession, Rebuild) are
// serialised under a write lock; queries snapshot the row map under a read
// lock and compute the response outside it, so concurrent readers do not
// block each other and never observe a torn write.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// Store is the in-memory rollups.Store. rows is keyed by the full rollups.Key
// (bucket start on the fixed UTC hour grid + authoritative dimension values);
// fenced holds the erased session triples; checkpoint is the last applied
// local durable sequence. all guarded by mu.
type Store struct {
	mu         sync.RWMutex
	rows       map[rollups.Key]rollups.MeasureSet
	fenced     map[string]struct{}
	checkpoint uint64
	oldest     time.Time
	newest     time.Time
	closed     bool
}

// New returns a fresh, empty Store.
func New() *Store {
	return &Store{rows: map[rollups.Key]rollups.MeasureSet{}}
}

// ApplyBatch implements rollups.Store. The batch's deltas and the
// checkpoint move are atomic (one lock hold): a crash between applying
// deltas and checkpointing is impossible, and a batch whose Checkpoint
// does not advance the stored checkpoint is a no-op (idempotent replay —
// every event at or below the stored checkpoint is already applied). A
// delta for a fenced triple rejects the WHOLE batch with
// rollups.ErrSessionFenced.
func (s *Store) ApplyBatch(ctx context.Context, batch rollups.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rollups.ErrClosed
	}
	if batch.Checkpoint <= s.checkpoint {
		// Idempotent replay: the batch covers nothing newer than the
		// stored checkpoint, and deltas + checkpoint are atomic, so every
		// event it covers was already applied. No-op.
		return nil
	}
	for _, d := range batch.Deltas {
		if s.isFencedLocked(d.Key) {
			return rollups.ErrSessionFenced
		}
	}
	for _, d := range batch.Deltas {
		r := s.rows[d.Key]
		r.Add(d.Add)
		s.rows[d.Key] = r
		s.floorRetentionLocked(d.Key.BucketStart)
	}
	s.checkpoint = batch.Checkpoint
	return nil
}

// Query implements rollups.Store. The query is re-validated (the wrapped
// ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels flow through),
// the row map is snapshotted under the read lock, and the grouping, sort,
// and pagination run outside it so readers never block writers. The
// response is deterministic for a stable store: same query + same cursor ⇒
// same rows, and pages never skip or repeat a row (see rollups.Query).
func (s *Store) Query(ctx context.Context, q rollups.Query) (rollups.Result, error) {
	if err := q.Validate(); err != nil {
		return rollups.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return rollups.Result{}, rollups.ErrClosed
	}
	// Snapshot the surviving rows (fenced triples are invisible even if a
	// row slipped in before its fence — defence in depth on top of the
	// fence-time delete). A stored row (BucketHour granularity) is included
	// when its own bucket start falls in the half-open [From, To) window;
	// it is then coarsened to the query's Bucket for grouping/labelling.
	snapshot := make(map[rollups.Key]rollups.MeasureSet, len(s.rows))
	for k, v := range s.rows {
		if s.isFencedLocked(k) {
			continue
		}
		if k.BucketStart.Before(q.From) || !k.BucketStart.Before(q.To) {
			continue
		}
		if !q.Filter.Matches(k) {
			continue
		}
		snapshot[k] = v
	}
	s.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if len(snapshot) == 0 {
		return rollups.Result{}, nil
	}
	return s.aggregateSnapshot(ctx, q, snapshot)
}

// aggregateSnapshot groups, sorts, and pages the filtered snapshot. Runs
// outside the store lock.
func (s *Store) aggregateSnapshot(ctx context.Context, q rollups.Query, snapshot map[rollups.Key]rollups.MeasureSet) (rollups.Result, error) {
	// groupKey is the comparable grouping key: the coarsened bucket start
	// plus one fixed slot per AllDimensions member. Slots beyond the
	// query's GroupBy set are unused (the empty string), so distinct
	// groups never collide.
	type groupKey struct {
		bucketNano int64
		dims       [len(rollups.AllDimensions)]string
	}
	type group struct {
		bucketStart time.Time
		values      rollups.DimensionValues
		sum         rollups.MeasureSet
	}

	groups := make(map[groupKey]*group)
	for k, v := range snapshot {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		b := rollups.BucketStart(k.BucketStart, q.Bucket)
		var gk groupKey
		gk.bucketNano = b.UnixNano()
		values := make(rollups.DimensionValues, len(q.GroupBy))
		for _, d := range q.GroupBy {
			gk.dims[dimensionSlot(d)] = k.DimensionValue(d)
			values[d] = gk.dims[dimensionSlot(d)]
		}
		g, ok := groups[gk]
		if !ok {
			g = &group{bucketStart: b, values: values}
			groups[gk] = g
		}
		g.sum.Add(v)
	}

	if len(groups) == 0 {
		return rollups.Result{}, nil
	}

	rows := make([]rollups.Row, 0, len(groups))
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		measures := make(map[rollups.Measure]float64, len(q.Measures))
		for _, m := range q.Measures {
			measures[m] = g.sum.Get(m)
		}
		rows = append(rows, rollups.Row{
			BucketStart: g.bucketStart,
			Dimensions:  g.values,
			Measures:    measures,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rowLess(rows[i], rows[j], q) })

	// Keyset pagination: skip everything up to and including the cursor
	// position, then emit at most Limit rows; a Limit+1-th row means
	// there is a next page.
	var cursor rollups.PageCursor
	if q.Cursor != "" {
		decoded, err := rollups.DecodeCursor(q.Cursor)
		if err != nil {
			return rollups.Result{}, err
		}
		if !cursorShapeMatches(decoded.Group, q.GroupBy) {
			return rollups.Result{}, fmt.Errorf("%w: cursor group shape does not match the query's GroupBy", rollups.ErrBadCursor)
		}
		cursor = decoded
	}
	out := make([]rollups.Row, 0, q.Limit+1)
	for _, r := range rows {
		if len(out) > q.Limit {
			break
		}
		if q.Cursor != "" && !rowAfter(r, q, cursor) {
			continue
		}
		out = append(out, r)
	}
	if len(out) <= q.Limit {
		return rollups.Result{Rows: out}, nil
	}
	last := out[q.Limit-1]
	next := rollups.PageCursor{
		BucketNano: last.BucketStart.UnixNano(),
		Group:      last.Dimensions,
	}
	if q.Sort == rollups.SortKeyMeasureAsc || q.Sort == rollups.SortKeyMeasureDesc {
		next.MeasureVal = last.Measures[q.SortMeasure]
	}
	cursorStr, err := rollups.EncodeCursor(next)
	if err != nil {
		return rollups.Result{}, err
	}
	return rollups.Result{Rows: out[:q.Limit], NextCursor: cursorStr}, nil
}

// cursorShapeMatches reports whether the cursor's group values carry
// exactly the query's GroupBy dimensions — a cursor produced by a query
// with a different GroupBy (or hand-crafted) must be rejected loudly
// rather than silently mis-paginating.
func cursorShapeMatches(group rollups.DimensionValues, groupBy []rollups.Dimension) bool {
	if len(group) != len(groupBy) {
		return false
	}
	for _, d := range groupBy {
		if _, ok := group[d]; !ok {
			return false
		}
	}
	return true
}

// dimensionSlot maps a closed dimension to its AllDimensions slot.
func dimensionSlot(d rollups.Dimension) int {
	for i, cd := range rollups.AllDimensions {
		if d == cd {
			return i
		}
	}
	return 0 // unreachable for validated GroupBy members
}

// rowLess is the query's total order: primary key, then bucket start, then
// the grouped dimension values (canonical order). Deterministic.
func rowLess(a, b rollups.Row, q rollups.Query) bool {
	switch q.Sort {
	case rollups.SortKeyBucketDesc:
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.After(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureAsc:
		av := a.Measures[q.SortMeasure]
		bv := b.Measures[q.SortMeasure]
		if av != bv {
			return av < bv
		}
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureDesc:
		av := a.Measures[q.SortMeasure]
		bv := b.Measures[q.SortMeasure]
		if av != bv {
			return av > bv
		}
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	default: // SortKeyBucketAsc
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	}
}

// rowAfter reports whether the row sorts strictly after the cursor
// position — the keyset "next page starts here" predicate. Uses the same
// total order as rowLess.
func rowAfter(r rollups.Row, q rollups.Query, c rollups.PageCursor) bool {
	bNano := r.BucketStart.UnixNano()
	groupAfter := c.Group.Less(r.Dimensions)
	switch q.Sort {
	case rollups.SortKeyBucketDesc:
		return bNano < c.BucketNano || (bNano == c.BucketNano && groupAfter)
	case rollups.SortKeyMeasureAsc:
		v := r.Measures[q.SortMeasure]
		return v > c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	case rollups.SortKeyMeasureDesc:
		v := r.Measures[q.SortMeasure]
		return v < c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	default: // SortKeyBucketAsc
		return bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)
	}
}

// FenceSession implements rollups.Store: it erases every row for the
// session triple and fences the triple so no future ApplyBatch can create
// rows for it (the erasure is never resurrected by a late event).
func (s *Store) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rollups.ErrClosed
	}
	for k := range s.rows {
		if k.TenantID == id.TenantID && k.UserID == id.UserID && k.SessionID == id.SessionID {
			delete(s.rows, k)
		}
	}
	if s.fenced == nil {
		s.fenced = map[string]struct{}{}
	}
	s.fenced[tripleKey(id)] = struct{}{}
	s.recomputeRetentionLocked()
	return nil
}

// UnfenceSession implements rollups.Store. Idempotent.
func (s *Store) UnfenceSession(ctx context.Context, id identity.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rollups.ErrClosed
	}
	delete(s.fenced, tripleKey(id))
	return nil
}

// IsFenced implements rollups.Store.
func (s *Store) IsFenced(ctx context.Context, id identity.Identity) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false, rollups.ErrClosed
	}
	_, ok := s.fenced[tripleKey(id)]
	return ok, nil
}

// Checkpoint implements rollups.Store.
func (s *Store) Checkpoint(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, rollups.ErrClosed
	}
	return s.checkpoint, nil
}

// Retention implements rollups.Store.
func (s *Store) Retention(ctx context.Context) (time.Time, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return time.Time{}, time.Time{}, rollups.ErrClosed
	}
	return s.oldest, s.newest, nil
}

// Rebuild implements rollups.Store: clears rows, fences, checkpoint, and
// retention so the projector reprocesses the full log.
func (s *Store) Rebuild(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rollups.ErrClosed
	}
	s.rows = map[rollups.Key]rollups.MeasureSet{}
	s.fenced = nil
	s.checkpoint = 0
	s.oldest = time.Time{}
	s.newest = time.Time{}
	return nil
}

// Close implements rollups.Store. Idempotent.
func (s *Store) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.rows = nil
	s.fenced = nil
	return nil
}

// isFencedLocked reports whether the row's session triple is fenced.
// Caller holds mu.
func (s *Store) isFencedLocked(k rollups.Key) bool {
	if s.fenced == nil {
		return false
	}
	_, ok := s.fenced[tripleKey(identity.Identity{
		TenantID:  k.TenantID,
		UserID:    k.UserID,
		SessionID: k.SessionID,
	})]
	return ok
}

// floorRetentionLocked widens the retained horizon to include the bucket.
// Caller holds mu.
func (s *Store) floorRetentionLocked(bucketStart time.Time) {
	if s.oldest.IsZero() || bucketStart.Before(s.oldest) {
		s.oldest = bucketStart
	}
	if s.newest.IsZero() || bucketStart.After(s.newest) {
		s.newest = bucketStart
	}
}

// recomputeRetentionLocked re-derives the retained horizon from the rows.
// Caller holds mu; used after a fence delete.
func (s *Store) recomputeRetentionLocked() {
	s.oldest = time.Time{}
	s.newest = time.Time{}
	for k := range s.rows {
		s.floorRetentionLocked(k.BucketStart)
	}
}

// tripleKey renders a session triple as the fenced-set key. The NUL
// separator can never appear in a tenant/user/session id, so distinct
// triples never collide.
func tripleKey(id identity.Identity) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
}
