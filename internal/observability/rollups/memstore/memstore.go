// Package memstore is the indexed in-memory implementation of the
// rollups.Store interface — the reference driver every Store-backed
// consumer and the conformancetest suite exercise. It is constructed
// directly (no registration): the production driver aggregator home is a
// wiring concern for the phase that wires the projector into the runtime.
//
// Indexing: the Store maintains a bucket index (populated fixed-UTC minute
// bucket starts → row keys, kept sorted for range scans) and a per-dimension
// index (dimension → value → row keys). A Query resolves its candidate rows
// from these indexes — the bounded window plus the filter axes — and never
// snapshots or full-scans the row table, matching the indexed access SQLite /
// Postgres drivers will use (WHERE bucket_start BETWEEN … AND … AND tenant =
// …). The scan is therefore proportional to the query, not to the store size.
//
// Concurrency: a *Store is a compiled artifact — immutable after
// construction — and safe for concurrent use by N goroutines against a
// single shared instance. Writes (ApplyBatch, FenceSession, Rebuild) are
// serialised under a write lock; queries resolve candidates under a read
// lock and compute the response outside it, so concurrent readers do not
// block each other and never observe a torn write.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// Store is the in-memory rollups.Store. rows is keyed by the full rollups.Key
// (bucket start on the fixed UTC minute grid + authoritative dimension
// values); byBucket and byDim are the query indexes over it; fenced holds the
// PERMANENTLY erased session triples (keyed by the comparable SessionTriple —
// never a NUL-joined string); checkpoint is the last applied local durable
// sequence. All guarded by mu; scannedKeys is atomic (test instrumentation).
type Store struct {
	mu           sync.RWMutex
	rows         map[rollups.Key]rollups.MeasureSet
	byBucket     map[int64]map[rollups.Key]struct{}                        // minute bucket start nano → keys
	bucketStarts []int64                                                   // sorted populated bucket-start nanos (index range scans)
	byDim        map[rollups.Dimension]map[string]map[rollups.Key]struct{} // dimension → value → keys
	fenced       map[rollups.SessionTriple]struct{}
	checkpoint   uint64
	oldest       time.Time
	newest       time.Time
	closed       bool

	// scannedKeys counts the candidate rows a Query resolved through the
	// indexes (test instrumentation for the index-proportionality pin).
	scannedKeys atomic.Int64
}

// New returns a fresh, empty Store.
func New() *Store {
	return &Store{
		rows:     map[rollups.Key]rollups.MeasureSet{},
		byBucket: map[int64]map[rollups.Key]struct{}{},
		byDim:    map[rollups.Dimension]map[string]map[rollups.Key]struct{}{},
	}
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
		s.indexAddLocked(d.Key)
		s.floorRetentionLocked(d.Key.BucketStart)
	}
	s.checkpoint = batch.Checkpoint
	return nil
}

// Query implements rollups.Store. The query is re-validated (the wrapped
// ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels flow through),
// the candidate rows are resolved through the bucket + dimension indexes
// under the read lock (proportional to the bounded window and filter — never
// a full-table scan), and the grouping, sort, and pagination run outside it
// so readers never block writers. The response is deterministic for a stable
// store: same query + same cursor ⇒ same rows, and pages never skip or
// repeat a row (see rollups.Query).
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
	// Candidate resolution via the indexes. Fenced rows cannot be
	// candidates by construction: FenceSession deletes them from rows AND
	// the indexes under one lock hold, and ApplyBatch refuses fenced
	// deltas, so no fenced row ever lives in the index.
	candidates := s.candidatesLocked(q)
	rows := make(map[rollups.Key]rollups.MeasureSet, len(candidates))
	for k := range candidates {
		if v, ok := s.rows[k]; ok {
			rows[k] = v
		}
	}
	scanned := int64(len(candidates))
	s.mu.RUnlock()
	s.scannedKeys.Add(scanned)

	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if len(rows) == 0 {
		return rollups.Result{}, nil
	}
	return s.aggregate(ctx, q, rows)
}

// candidatesLocked resolves the query's candidate rows from the bucket index
// (populated minute buckets whose start falls in the half-open [From, To)
// window) intersected with every non-empty filter axis via the dimension
// index. Caller holds at least a read lock.
func (s *Store) candidatesLocked(q rollups.Query) map[rollups.Key]struct{} {
	fromNano := q.From.UnixNano()
	toNano := q.To.UnixNano()

	start := sort.Search(len(s.bucketStarts), func(i int) bool { return s.bucketStarts[i] >= fromNano })
	cand := make(map[rollups.Key]struct{})
	for i := start; i < len(s.bucketStarts); i++ {
		b := s.bucketStarts[i]
		if b >= toNano {
			break
		}
		for k := range s.byBucket[b] {
			cand[k] = struct{}{}
		}
	}

	s.intersectFilterLocked(cand, rollups.DimensionTenant, q.Filter.TenantIDs)
	s.intersectFilterLocked(cand, rollups.DimensionUser, q.Filter.UserIDs)
	s.intersectFilterLocked(cand, rollups.DimensionSession, q.Filter.SessionIDs)
	s.intersectFilterLocked(cand, rollups.DimensionModel, q.Filter.Models)
	return cand
}

// intersectFilterLocked narrows cand to the rows matching one filter axis
// (set semantics: the union of the axis' listed values). An empty values
// slice matches everything and is a no-op. Caller holds at least a read lock.
func (s *Store) intersectFilterLocked(cand map[rollups.Key]struct{}, d rollups.Dimension, values []string) {
	if len(values) == 0 {
		return
	}
	allowed := make(map[rollups.Key]struct{})
	idx := s.byDim[d]
	for _, v := range values {
		for k := range idx[v] {
			allowed[k] = struct{}{}
		}
	}
	for k := range cand {
		if _, ok := allowed[k]; !ok {
			delete(cand, k)
		}
	}
}

// aggregate groups, sorts, and pages the filtered candidate rows. Runs
// outside the store lock.
func (s *Store) aggregate(ctx context.Context, q rollups.Query, rows map[rollups.Key]rollups.MeasureSet) (rollups.Result, error) {
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
	for k, v := range rows {
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

	out := make([]rollups.Row, 0, len(groups))
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return rollups.Result{}, err
		}
		measures := make(map[rollups.Measure]rollups.MeasureValue, len(q.Measures))
		for _, m := range q.Measures {
			measures[m] = g.sum.Get(m)
		}
		out = append(out, rollups.Row{
			BucketStart: g.bucketStart,
			Dimensions:  g.values,
			Measures:    measures,
		})
	}

	sort.Slice(out, func(i, j int) bool { return rowLess(out[i], out[j], q) })

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
	page := make([]rollups.Row, 0, q.Limit+1)
	for _, r := range out {
		if len(page) > q.Limit {
			break
		}
		if q.Cursor != "" && !rowAfter(r, q, cursor) {
			continue
		}
		page = append(page, r)
	}
	if len(page) <= q.Limit {
		return rollups.Result{Rows: page}, nil
	}
	last := page[q.Limit-1]
	next := rollups.PageCursor{
		BucketNano: last.BucketStart.UnixNano(),
		Group:      last.Dimensions,
	}
	if q.Sort == rollups.SortKeyMeasureAsc || q.Sort == rollups.SortKeyMeasureDesc {
		next.MeasureVal = last.Measures[q.SortMeasure].N
	}
	cursorStr, err := rollups.EncodeCursor(next)
	if err != nil {
		return rollups.Result{}, err
	}
	return rollups.Result{Rows: page[:q.Limit], NextCursor: cursorStr}, nil
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
// the grouped dimension values (canonical order). Deterministic. Measure
// comparisons use the exact integer MeasureValue.N — never float.
func rowLess(a, b rollups.Row, q rollups.Query) bool {
	switch q.Sort {
	case rollups.SortKeyBucketDesc:
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.After(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureAsc:
		av := a.Measures[q.SortMeasure].N
		bv := b.Measures[q.SortMeasure].N
		if av != bv {
			return av < bv
		}
		if a.BucketStart != b.BucketStart {
			return a.BucketStart.Before(b.BucketStart)
		}
		return a.Dimensions.Less(b.Dimensions)
	case rollups.SortKeyMeasureDesc:
		av := a.Measures[q.SortMeasure].N
		bv := b.Measures[q.SortMeasure].N
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
		v := r.Measures[q.SortMeasure].N
		return v > c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	case rollups.SortKeyMeasureDesc:
		v := r.Measures[q.SortMeasure].N
		return v < c.MeasureVal || (v == c.MeasureVal && (bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)))
	default: // SortKeyBucketAsc
		return bNano > c.BucketNano || (bNano == c.BucketNano && groupAfter)
	}
}

// FenceSession implements rollups.Store: it erases every row for the
// session triple and fences the triple PERMANENTLY so no future ApplyBatch
// can create rows for it (the erasure is never resurrected by a late event
// or by Rebuild). There is no unfence operation.
func (s *Store) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rollups.ErrClosed
	}
	t := rollups.TripleOf(id)
	for k := range s.rows {
		if t.Matches(k) {
			delete(s.rows, k)
			s.indexRemoveLocked(k)
		}
	}
	if s.fenced == nil {
		s.fenced = map[rollups.SessionTriple]struct{}{}
	}
	s.fenced[t] = struct{}{}
	s.recomputeRetentionLocked()
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
	_, ok := s.fenced[rollups.TripleOf(id)]
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

// Rebuild implements rollups.Store: clears rows and the checkpoint so the
// projector reprocesses the full log. Erasure fences are PERMANENT and are
// deliberately NOT cleared — rebuilding projection rows or the checkpoint
// cannot authorize the resurrection of an erased session.
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
	s.byBucket = map[int64]map[rollups.Key]struct{}{}
	s.bucketStarts = nil
	s.byDim = map[rollups.Dimension]map[string]map[rollups.Key]struct{}{}
	s.checkpoint = 0
	s.oldest = time.Time{}
	s.newest = time.Time{}
	// s.fenced is intentionally left untouched: erasure fences are
	// permanent.
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
	s.byBucket = nil
	s.bucketStarts = nil
	s.byDim = nil
	s.fenced = nil
	return nil
}

// isFencedLocked reports whether the row's session triple is fenced.
// Caller holds mu.
func (s *Store) isFencedLocked(k rollups.Key) bool {
	if s.fenced == nil {
		return false
	}
	_, ok := s.fenced[rollups.SessionTriple{TenantID: k.TenantID, UserID: k.UserID, SessionID: k.SessionID}]
	return ok
}

// indexAddLocked inserts the key into the bucket and dimension indexes.
// Caller holds mu.
func (s *Store) indexAddLocked(k rollups.Key) {
	b := k.BucketStart.UnixNano()
	set := s.byBucket[b]
	if set == nil {
		set = make(map[rollups.Key]struct{})
		s.byBucket[b] = set
		s.insertBucketLocked(b)
	}
	set[k] = struct{}{}

	for _, d := range rollups.AllDimensions {
		v := k.DimensionValue(d)
		vs := s.byDim[d]
		if vs == nil {
			vs = make(map[string]map[rollups.Key]struct{})
			s.byDim[d] = vs
		}
		kset := vs[v]
		if kset == nil {
			kset = make(map[rollups.Key]struct{})
			vs[v] = kset
		}
		kset[k] = struct{}{}
	}
}

// indexRemoveLocked removes the key from the bucket and dimension indexes.
// Caller holds mu.
func (s *Store) indexRemoveLocked(k rollups.Key) {
	b := k.BucketStart.UnixNano()
	if set := s.byBucket[b]; set != nil {
		delete(set, k)
		if len(set) == 0 {
			delete(s.byBucket, b)
			s.removeBucketLocked(b)
		}
	}
	for _, d := range rollups.AllDimensions {
		v := k.DimensionValue(d)
		if vs := s.byDim[d]; vs != nil {
			if kset := vs[v]; kset != nil {
				delete(kset, k)
				if len(kset) == 0 {
					delete(vs, v)
				}
			}
		}
	}
}

// insertBucketLocked inserts a bucket-start nano into the sorted
// bucketStarts slice (no-op when already present). Caller holds mu.
func (s *Store) insertBucketLocked(b int64) {
	i := sort.Search(len(s.bucketStarts), func(i int) bool { return s.bucketStarts[i] >= b })
	if i < len(s.bucketStarts) && s.bucketStarts[i] == b {
		return
	}
	s.bucketStarts = append(s.bucketStarts, 0)
	copy(s.bucketStarts[i+1:], s.bucketStarts[i:])
	s.bucketStarts[i] = b
}

// removeBucketLocked removes a bucket-start nano from the sorted
// bucketStarts slice (no-op when absent). Caller holds mu.
func (s *Store) removeBucketLocked(b int64) {
	i := sort.Search(len(s.bucketStarts), func(i int) bool { return s.bucketStarts[i] >= b })
	if i < len(s.bucketStarts) && s.bucketStarts[i] == b {
		s.bucketStarts = append(s.bucketStarts[:i], s.bucketStarts[i+1:]...)
	}
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
