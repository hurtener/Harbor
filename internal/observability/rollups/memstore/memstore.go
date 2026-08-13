// Package memstore is the indexed in-memory implementation of the
// rollups.Store interface — the reference driver every Store-backed
// consumer and the conformancetest suite exercise. It is constructed
// directly (no registration): the production driver aggregator home is a
// wiring concern for the phase that wires the projector into the runtime.
//
// Indexing: the Store maintains a bucket index (populated fixed-UTC minute
// bucket starts → row keys, kept sorted for range scans) and a per-dimension
// index (dimension → value → row keys). A Query resolves its candidate rows
// from these indexes — seeding on whichever of the bounded window and the
// filter axes is the cheaper index scan, then verifying the window and the
// remaining axes per row — and never snapshots or full-scans the row table,
// matching the indexed access SQLite / Postgres drivers will use (WHERE
// bucket_start BETWEEN … AND … AND tenant = …). The scan is therefore
// proportional to the query, not to the store size.
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

	// scannedKeys counts the row index entries a Query ACTUALLY visited
	// while resolving candidates — every entry examined, kept or rejected
	// (test instrumentation for the index-proportionality pin). The
	// bucket-start range navigation is index metadata, not a row entry,
	// and is not counted.
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
// rollups.ErrSessionFenced. A delta whose accumulation would overflow a
// measure's exact int64 representation also rejects the WHOLE batch with
// rollups.ErrMeasureOverflow: every delta's merge is verified against a
// working copy BEFORE any row is touched, so a refused batch never leaves
// partial rows or wrapped-negative counters and the checkpoint does not
// advance.
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
	// Pre-check pass: accumulate every delta into a working copy of its
	// row (folding same-key deltas within the batch together) and verify
	// every measure fits the exact int64 range BEFORE any write. A batch
	// whose accumulation would overflow fails loudly with
	// rollups.ErrMeasureOverflow and applies NOTHING.
	pending := make(map[rollups.Key]rollups.MeasureSet, len(batch.Deltas))
	for _, d := range batch.Deltas {
		r := s.rows[d.Key]
		if prev, ok := pending[d.Key]; ok {
			r = prev
		}
		if err := r.Add(d.Add); err != nil {
			return fmt.Errorf("rollups: ApplyBatch checkpoint=%d: %w", batch.Checkpoint, err)
		}
		pending[d.Key] = r
	}
	for _, d := range batch.Deltas {
		s.rows[d.Key] = pending[d.Key]
		s.indexAddLocked(d.Key)
		s.floorRetentionLocked(d.Key.BucketStart)
	}
	s.checkpoint = batch.Checkpoint
	return nil
}

// Query implements rollups.Store. The query is re-validated (the wrapped
// ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels flow through),
// the candidate rows are resolved through the bucket + dimension indexes
// under the read lock (seeding on the cheapest index scan among the bounded
// window and the filter axes, then verifying the rest per row — never a
// full-table scan), and the grouping, sort, and pagination run outside it
// so readers never block writers. The response is deterministic for a stable
// store: same query + same cursor ⇒ same rows, and pages never skip or
// repeat a row (see rollups.Query). A group whose measure sums would
// overflow fails loudly with rollups.ErrMeasureOverflow.
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
	// deltas, so no fenced row ever lives in the index. The visited count
	// is every index entry the resolution examined (kept or rejected) —
	// the honest scan cost, recorded on scannedKeys.
	candidates, visited := s.candidatesLocked(q)
	rows := make(map[rollups.Key]rollups.MeasureSet, len(candidates))
	for k := range candidates {
		if v, ok := s.rows[k]; ok {
			rows[k] = v
		}
	}
	s.mu.RUnlock()
	s.scannedKeys.Add(visited)

	if err := ctx.Err(); err != nil {
		return rollups.Result{}, err
	}
	if len(rows) == 0 {
		return rollups.Result{}, nil
	}
	return s.aggregate(ctx, q, rows)
}

// candidatesLocked resolves the query's candidate rows from the CHEAPEST
// applicable index seed and then verifies the remaining constraints as
// direct per-row checks. The seed is either the bounded bucket range
// (every populated minute bucket whose start falls in the half-open
// [From, To) window) or one exact filter axis (every row in that axis'
// index entries for the filter's distinct values), whichever the populated
// indexes make cheaper. The verification pass applies the time window and
// every remaining exact filter axis as a direct equality check on each
// visited row — it never builds a full-retention allowed set, and it never
// scans canonical events. The returned count is the number of row index
// entries ACTUALLY visited (including entries the direct checks reject),
// which is what the scannedKeys instrumentation records — the honest cost
// of the scan, not the size of the result. Caller holds at least a read
// lock.
func (s *Store) candidatesLocked(q rollups.Query) (map[rollups.Key]struct{}, int64) {
	fromNano := q.From.UnixNano()
	toNano := q.To.UnixNano()

	// Distinct filter values per non-empty axis (set semantics: a
	// duplicate value is visited once, so the scan cost and the result
	// are identical to the deduplicated filter).
	type axisValues struct {
		dim    rollups.Dimension
		values []string
	}
	var axes []axisValues
	for _, d := range rollups.AllDimensions {
		if vals := distinctStrings(filterValues(q.Filter, d)); len(vals) > 0 {
			axes = append(axes, axisValues{dim: d, values: vals})
		}
	}

	// Seed cost of the bounded bucket range: the rows in the populated
	// minute buckets inside [From, To). The bucket-start range scan is
	// index navigation (like a SQL range scan), not a row visit; only the
	// row entries themselves are counted.
	bucketStart := sort.Search(len(s.bucketStarts), func(i int) bool { return s.bucketStarts[i] >= fromNano })
	bucketCost := int64(0)
	for i := bucketStart; i < len(s.bucketStarts); i++ {
		if s.bucketStarts[i] >= toNano {
			break
		}
		bucketCost += int64(len(s.byBucket[s.bucketStarts[i]]))
	}

	// Seed cost of each non-empty filter axis: the rows in that axis'
	// index entries for the filter's distinct values. len() on the index
	// maps is O(1), so choosing the seed never scans the entries.
	bestAxis := -1
	bestCost := bucketCost
	for ai, a := range axes {
		idx := s.byDim[a.dim]
		cost := int64(0)
		for _, v := range a.values {
			cost += int64(len(idx[v]))
		}
		if cost < bestCost {
			bestAxis = ai
			bestCost = cost
		}
	}

	cand := make(map[rollups.Key]struct{})
	var visited int64
	if bestAxis < 0 {
		// Bucket-window seed: every row in the bounded window; each
		// filter axis is a direct equality check on the row.
		for i := bucketStart; i < len(s.bucketStarts); i++ {
			b := s.bucketStarts[i]
			if b >= toNano {
				break
			}
			for k := range s.byBucket[b] {
				visited++
				if q.Filter.Matches(k) {
					cand[k] = struct{}{}
				}
			}
		}
		return cand, visited
	}

	// Exact-axis seed: every row in the chosen axis' index entries for
	// the filter's distinct values (the axis index spans the full
	// retention horizon); the time window and every other axis are direct
	// per-row checks.
	a := axes[bestAxis]
	idx := s.byDim[a.dim]
	for _, v := range a.values {
		for k := range idx[v] {
			visited++
			b := k.BucketStart.UnixNano()
			if b >= fromNano && b < toNano && q.Filter.Matches(k) {
				cand[k] = struct{}{}
			}
		}
	}
	return cand, visited
}

// filterValues returns the filter's values for one closed dimension (the
// axis' set semantics; an empty slice matches every value on the axis).
func filterValues(f rollups.Filter, d rollups.Dimension) []string {
	switch d {
	case rollups.DimensionTenant:
		return f.TenantIDs
	case rollups.DimensionUser:
		return f.UserIDs
	case rollups.DimensionSession:
		return f.SessionIDs
	case rollups.DimensionModel:
		return f.Models
	default:
		return nil
	}
}

// distinctStrings returns vals with adjacent-preserving deduplication (set
// semantics: order is preserved, duplicates collapse to one occurrence).
func distinctStrings(vals []string) []string {
	if len(vals) < 2 {
		return vals
	}
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
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
		// Group aggregation is the same checked accumulation as writes: a
		// group whose sum would overflow fails loudly with
		// rollups.ErrMeasureOverflow instead of returning a wrapped or
		// clamped total.
		if err := g.sum.Add(v); err != nil {
			return rollups.Result{}, fmt.Errorf("rollups: aggregate: %w", err)
		}
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
	// there is a next page. The cursor's shape binding (version +
	// fingerprint) was verified by q.Validate() before the candidate scan;
	// the group-shape re-check here guards a hand-crafted cursor with a
	// correct fingerprint but an unrelated Group map.
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
		// Bind the cursor to the producing query's canonical shape so a
		// reuse under a differently-shaped query fails loudly.
		ShapeVersion: rollups.CursorShapeVersion,
		Fingerprint:  rollups.QueryShapeFingerprint(q),
		BucketNano:   last.BucketStart.UnixNano(),
		Group:        last.Dimensions,
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
// rather than silently mis-paginating. The full shape binding (version +
// fingerprint) is enforced by Query.Validate; this is the structural
// defence against a fabricated position.
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
