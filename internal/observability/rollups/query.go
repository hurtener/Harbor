package rollups

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Result budgets — a query that would exceed them fails loudly with
// ErrQueryBudget (never a silently truncated response).
const (
	// MaxBuckets bounds the number of buckets one query may span. A
	// window at BucketMinute covering more than ~2.8 days, BucketHour
	// covering more than ~5.7 months, or BucketDay covering ~11 years
	// must be narrowed or coarsened.
	MaxBuckets = 4096
	// MaxRowsPerQuery bounds one query page (the deterministic pagination
	// budget). Larger result sets are read page by page via NextCursor.
	MaxRowsPerQuery = 10_000
)

// Filter constrains a query over the closed dimensions. Each slice has set
// semantics: an empty slice matches every value on that axis; a non-empty
// slice matches exactly the listed values. All axes are ANDed.
type Filter struct {
	// TenantIDs restricts to rows of these tenants ("" matches all).
	TenantIDs []string
	// UserIDs restricts to rows of these users ("" matches all).
	UserIDs []string
	// SessionIDs restricts to rows of these sessions ("" matches all).
	SessionIDs []string
	// Models restricts to rows with these model values. An empty Models
	// slice matches BOTH un-attributed (model "") and attributed rows;
	// to see only model-attributed rows, name the models explicitly.
	Models []string
}

// Matches reports whether the row key satisfies the filter. A fenced
// (erased) triple is handled by the store before this predicate runs.
func (f Filter) Matches(k Key) bool {
	if len(f.TenantIDs) > 0 && !containsString(f.TenantIDs, k.TenantID) {
		return false
	}
	if len(f.UserIDs) > 0 && !containsString(f.UserIDs, k.UserID) {
		return false
	}
	if len(f.SessionIDs) > 0 && !containsString(f.SessionIDs, k.SessionID) {
		return false
	}
	if len(f.Models) > 0 && !containsString(f.Models, k.Model) {
		return false
	}
	return true
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// SortKey is the closed set of query sort keys. Every sort is total: the
// primary key, then (bucket start, then the grouped dimension values in
// canonical order) as deterministic tie-breakers, so pagination never
// skips or repeats a row on a stable store.
type SortKey string

const (
	// SortKeyBucketAsc sorts chronologically, oldest bucket first.
	SortKeyBucketAsc SortKey = "bucket_asc"
	// SortKeyBucketDesc sorts newest bucket first.
	SortKeyBucketDesc SortKey = "bucket_desc"
	// SortKeyMeasureAsc sorts by the query's SortMeasure sum, ascending.
	SortKeyMeasureAsc SortKey = "measure_asc"
	// SortKeyMeasureDesc sorts by the query's SortMeasure sum, descending.
	SortKeyMeasureDesc SortKey = "measure_desc"
)

// AllSortKeys is the closed sort set.
var AllSortKeys = [...]SortKey{
	SortKeyBucketAsc,
	SortKeyBucketDesc,
	SortKeyMeasureAsc,
	SortKeyMeasureDesc,
}

// Query is the typed rollup read. The window is mandatory; every other
// field is closed / enumerated and validated by Validate. A Query is
// immutable in the intended usage — Validate does not mutate it.
type Query struct {
	// From / To bound the bucket window (half-open [From, To), both
	// normalised to UTC AND aligned to the Bucket grid: each must equal
	// its own BucketStart, so a window never includes a partial bucket —
	// an unaligned edge is rejected with ErrQueryInvalid). Mandatory:
	// From must precede To, and the window may span at most MaxBuckets at
	// the requested Bucket size.
	From time.Time
	To   time.Time
	// Bucket is the (closed) query bucket size. Rows are stored at
	// StoreGranularity (the minute grid) and coarsened to Bucket at read
	// time.
	Bucket BucketSize
	// GroupBy is the closed dimension subset the rows are grouped by (may
	// be empty — then one row per bucket aggregates the whole window).
	GroupBy []Dimension
	// Filter constrains the rows before grouping (closed axes).
	Filter Filter
	// Measures selects the measures each result row carries (mandatory,
	// non-empty, closed, deduplicated).
	Measures []Measure
	// Sort is the closed sort key (default: SortKeyBucketAsc when empty).
	Sort SortKey
	// SortMeasure names the measure used by SortKeyMeasureAsc/Desc; must
	// be a closed measure when a measure sort is requested.
	SortMeasure Measure
	// Limit bounds the page size (1..MaxRowsPerQuery, mandatory).
	Limit int
	// Cursor is the opaque deterministic pagination cursor returned by a
	// previous page ("" = the first page). A stale or malformed cursor is
	// rejected with ErrBadCursor — a query never silently restarts at the
	// beginning.
	Cursor string
}

// Validate checks every closed/structural invariant and the result budgets.
// Returns wrapped ErrQueryInvalid / ErrQueryBudget / ErrBadCursor sentinels.
func (q Query) Validate() error {
	if !q.To.After(q.From) {
		return fmt.Errorf("%w: window [%s, %s) is empty or reversed", ErrQueryInvalid,
			q.From.Format(time.RFC3339Nano), q.To.Format(time.RFC3339Nano))
	}
	if err := q.Bucket.Validate(); err != nil {
		return err
	}
	if err := validateWindowAligned(q.From, q.To, q.Bucket); err != nil {
		return err
	}
	if err := ValidateDimensions(q.GroupBy); err != nil {
		return err
	}
	if err := ValidateMeasures(q.Measures); err != nil {
		return err
	}
	if q.Sort == "" {
		q.Sort = SortKeyBucketAsc
	}
	if !isClosedSortKey(q.Sort) {
		return fmt.Errorf("%w: Sort=%q (allowed: %v)", ErrQueryInvalid, q.Sort, allSortKeys())
	}
	if (q.Sort == SortKeyMeasureAsc || q.Sort == SortKeyMeasureDesc) && q.SortMeasure.Validate() != nil {
		return fmt.Errorf("%w: SortKeyMeasure* requires a closed SortMeasure, got %q", ErrQueryInvalid, q.SortMeasure)
	}
	if q.Limit <= 0 {
		return fmt.Errorf("%w: Limit=%d must be > 0", ErrQueryInvalid, q.Limit)
	}
	if q.Limit > MaxRowsPerQuery {
		return fmt.Errorf("%w: Limit=%d exceeds MaxRowsPerQuery=%d", ErrQueryBudget, q.Limit, MaxRowsPerQuery)
	}
	// The cursor is bound to the canonical shape that produced it: a
	// cursor from a differently-shaped query (or a hand-crafted one) is
	// rejected here, before any paging.
	if err := q.validateCursorShape(); err != nil {
		return err
	}
	span, err := bucketSpan(q.From, q.To, q.Bucket)
	if err != nil {
		return err
	}
	if span > MaxBuckets {
		return fmt.Errorf("%w: window [%s, %s) spans %d %s buckets, exceeding MaxBuckets=%d (narrow the window or coarsen the bucket)",
			ErrQueryBudget, q.From.Format(time.RFC3339Nano), q.To.Format(time.RFC3339Nano), span, q.Bucket, MaxBuckets)
	}
	return nil
}

// validateWindowAligned requires From and To to sit exactly on the Bucket's
// fixed UTC grid. Every closed size is a multiple of the minute, so a window
// aligned to the hour or day grid is also aligned to the minute storage
// base — a misaligned edge would silently count a partial bucket, so it is
// rejected loudly with ErrQueryInvalid instead.
func validateWindowAligned(from, to time.Time, bucket BucketSize) error {
	if !from.Equal(BucketStart(from, bucket)) {
		return fmt.Errorf("%w: window From %s is not aligned to the %s grid (it must fall exactly on a fixed-UTC bucket boundary)",
			ErrQueryInvalid, from.Format(time.RFC3339Nano), bucket)
	}
	if !to.Equal(BucketStart(to, bucket)) {
		return fmt.Errorf("%w: window To %s is not aligned to the %s grid (it must fall exactly on a fixed-UTC bucket boundary)",
			ErrQueryInvalid, to.Format(time.RFC3339Nano), bucket)
	}
	return nil
}

// validateCursorShape rejects a cursor that was not produced by a query of
// this exact canonical shape. The PageCursor carries the shape version and
// fingerprint of its producer; either mismatch is ErrBadCursor — a query
// never silently restarts at an arbitrary position.
func (q Query) validateCursorShape() error {
	if q.Cursor == "" {
		return nil
	}
	c, err := DecodeCursor(q.Cursor)
	if err != nil {
		return err
	}
	if c.ShapeVersion != CursorShapeVersion {
		return fmt.Errorf("%w: cursor was produced by a different shape version (%d != %d); restart from the first page",
			ErrBadCursor, c.ShapeVersion, CursorShapeVersion)
	}
	if c.Fingerprint != QueryShapeFingerprint(q) {
		return fmt.Errorf("%w: cursor was produced by a query of a different shape; restart from the first page", ErrBadCursor)
	}
	return nil
}

func isClosedSortKey(s SortKey) bool {
	for _, k := range AllSortKeys {
		if s == k {
			return true
		}
	}
	return false
}

func allSortKeys() []string {
	out := make([]string, 0, len(AllSortKeys))
	for _, k := range AllSortKeys {
		out = append(out, string(k))
	}
	return out
}

// MeasureValue is the exact, wire-ready value of one measure for one row.
// Every measure accumulates in integer form only (see measure.go): counts,
// tokens, latency ms, and cost micro-units. N is the exact accumulated
// integer — counters above 2^53 stay exact because nothing is ever
// normalised to float64. Scale is the measure's fixed decimal denominator
// (1 for integer measures; CostScaleMicros for cost), so a consumer formats
// decimal USD exactly as N / Scale at the edge. A MeasureValue is
// JSON-safe and comparable on N for a fixed measure.
type MeasureValue struct {
	// N is the exact accumulated integer.
	N int64
	// Scale is the decimal denominator of the measure's unit; constant
	// per measure.
	Scale uint32
}

// Row is one grouped result row.
type Row struct {
	// BucketStart is the bucket the row aggregates (coarsened to the
	// query's Bucket size, UTC).
	BucketStart time.Time
	// Dimensions carries the query's GroupBy dimension values (empty when
	// GroupBy was empty — the row aggregates the whole window).
	Dimensions DimensionValues
	// Measures carries the query's requested measures and their exact
	// integer sums / folds.
	Measures map[Measure]MeasureValue
}

// Result is one query page.
type Result struct {
	// Rows is the page, in the query's total order (nil when empty).
	Rows []Row
	// NextCursor is the opaque cursor for the next page ("" when this is
	// the last page).
	NextCursor string
}

// CursorShapeVersion is the version of the cursor shape-binding contract: a
// PageCursor carries the version + fingerprint of the query that produced
// it, and a query whose canonical shape differs is rejected with ErrBadCursor
// before any paging. Bump the version when the canonical shape changes (a
// new filter axis, sort key, measure, dimension, or a changed window
// normalisation) — cursors from the previous contract then fail loudly with
// ErrBadCursor instead of silently mis-paginating.
const CursorShapeVersion = 1

// maxCursorBytes bounds the decoded cursor payload. Cursors are small (a
// fingerprint plus one row position); anything larger is rejected as
// malformed before decoding allocates unbounded memory.
const maxCursorBytes = 4096

// PageCursor is the opaque deterministic pagination position: the primary
// sort value plus the exact bucket start and grouped dimension values of
// the last row of the page. The next page starts strictly after it. Encode
// via EncodeCursor; the encoded string is what a caller passes back as
// Query.Cursor.
//
// A cursor is BOUND to the canonical shape of the query that produced it:
// the shape version and fingerprint (see QueryShapeFingerprint) ride along,
// and any query whose shape differs is rejected with ErrBadCursor — a
// cursor is never silently re-purposed across a different window, bucket,
// filter, grouping, measure set, or sort.
type PageCursor struct {
	// ShapeVersion is CursorShapeVersion of the producing query. A cursor
	// with a different version is rejected with ErrBadCursor.
	ShapeVersion int
	// Fingerprint is the deterministic fingerprint of the producing
	// query's canonical shape (see QueryShapeFingerprint). A query whose
	// fingerprint differs is rejected with ErrBadCursor before paging.
	Fingerprint string
	// BucketNano is the row's bucket start in unix nanoseconds (exact
	// int64 — never float-converted).
	BucketNano int64
	// MeasureVal is the row's SortMeasure sum in its exact integer form
	// (the MeasureValue.N — same scale as SortMeasure, so comparison is
	// exact; never float-converted).
	MeasureVal int64
	// Group is the row's grouped dimension values.
	Group DimensionValues
}

// QueryShapeFingerprint returns a deterministic fingerprint of the query's
// canonical shape: the normalized (UTC) From/To instants, the Bucket, the
// sorted + deduplicated filter sets (one axis per fixed slot), the GroupBy
// dimensions in their given order, the sorted requested Measures, and the
// effective Sort (empty defaults to SortKeyBucketAsc) with SortMeasure.
// Limit and Cursor are deliberately EXCLUDED — a caller may page the same
// shape with a different page size, and the cursor position never changes
// the shape.
//
// Two queries that differ in any shape field produce different
// fingerprints; two queries that differ only in Limit, Cursor, the order of
// a filter axis' values, or the order of Measures produce the same
// fingerprint. The caller validates the query first; this is a pure
// normalisation.
func QueryShapeFingerprint(q Query) string {
	h := sha256.New()
	var b [8]byte
	writeUint64 := func(v uint64) {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	writeString := func(s string) {
		writeUint64(uint64(len(s)))
		h.Write([]byte(s))
	}
	writeSet := func(vals []string) {
		sorted := append([]string(nil), vals...)
		sort.Strings(sorted)
		// Set semantics: collapse adjacent duplicates after sorting.
		uniq := sorted[:0]
		for _, v := range sorted {
			if len(uniq) == 0 || uniq[len(uniq)-1] != v {
				uniq = append(uniq, v)
			}
		}
		writeUint64(uint64(len(uniq)))
		for _, v := range uniq {
			writeString(v)
		}
	}

	// Version first: a shape-contract change produces a different
	// fingerprint even when no field below changed.
	writeUint64(uint64(CursorShapeVersion))
	// The window as exact UTC instants (never wall-clock locations).
	writeUint64(uint64(q.From.UTC().UnixNano()))
	writeUint64(uint64(q.To.UTC().UnixNano()))
	writeString(string(q.Bucket))
	writeSet(q.Filter.TenantIDs)
	writeSet(q.Filter.UserIDs)
	writeSet(q.Filter.SessionIDs)
	writeSet(q.Filter.Models)
	// GroupBy in the query's given order: the grouping keys differ when
	// the order differs, so order is part of the shape.
	writeUint64(uint64(len(q.GroupBy)))
	for _, d := range q.GroupBy {
		writeString(string(d))
	}
	// Measures: the requested SET (order-insensitive).
	measures := append([]Measure(nil), q.Measures...)
	sort.Slice(measures, func(i, j int) bool { return measures[i] < measures[j] })
	writeUint64(uint64(len(measures)))
	for _, m := range measures {
		writeString(string(m))
	}
	// The effective sort (default bucket_asc) and its measure.
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = SortKeyBucketAsc
	}
	writeString(string(sortKey))
	writeString(string(q.SortMeasure))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// EncodeCursor renders the cursor as its opaque, deterministic string form
// (base64url of the JSON encoding — encoding/json orders map keys, so the
// bytes are stable for equal cursors).
func EncodeCursor(c PageCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("rollups: cursor encode: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses an opaque cursor. Decoding is STRICT and bounded: an
// empty or over-long cursor, an unknown field, a mistyped field, or any
// trailing data after the JSON value fails with a wrapped ErrBadCursor — a
// query never silently restarts at the beginning.
func DecodeCursor(s string) (PageCursor, error) {
	if s == "" {
		return PageCursor{}, fmt.Errorf("%w: empty cursor", ErrBadCursor)
	}
	if len(s) > base64.RawURLEncoding.EncodedLen(maxCursorBytes) {
		return PageCursor{}, fmt.Errorf("%w: cursor exceeds the %d-byte bound", ErrBadCursor, maxCursorBytes)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return PageCursor{}, fmt.Errorf("%w: malformed cursor", ErrBadCursor)
	}
	if len(raw) > maxCursorBytes {
		return PageCursor{}, fmt.Errorf("%w: cursor exceeds the %d-byte bound", ErrBadCursor, maxCursorBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c PageCursor
	if err := dec.Decode(&c); err != nil {
		return PageCursor{}, fmt.Errorf("%w: malformed cursor: %v", ErrBadCursor, err)
	}
	if dec.More() {
		return PageCursor{}, fmt.Errorf("%w: malformed cursor: trailing data", ErrBadCursor)
	}
	return c, nil
}
