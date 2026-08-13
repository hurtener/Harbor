package rollups

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	// normalised to UTC). Mandatory: From must precede To, and the window
	// may span at most MaxBuckets at the requested Bucket size.
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
// Returns wrapped ErrQueryInvalid / ErrQueryBudget sentinels.
func (q Query) Validate() error {
	if !q.To.After(q.From) {
		return fmt.Errorf("%w: window [%s, %s) is empty or reversed", ErrQueryInvalid,
			q.From.Format(time.RFC3339Nano), q.To.Format(time.RFC3339Nano))
	}
	if err := q.Bucket.Validate(); err != nil {
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
	if q.Cursor != "" {
		if _, err := DecodeCursor(q.Cursor); err != nil {
			return err
		}
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

// PageCursor is the opaque deterministic pagination position: the primary
// sort value plus the exact bucket start and grouped dimension values of
// the last row of the page. The next page starts strictly after it. Encode
// via EncodeCursor; the encoded string is what a caller passes back as
// Query.Cursor.
type PageCursor struct {
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

// DecodeCursor parses an opaque cursor. A malformed cursor fails with a
// wrapped ErrBadCursor — a query never silently restarts at the beginning.
func DecodeCursor(s string) (PageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return PageCursor{}, fmt.Errorf("%w: malformed cursor", ErrBadCursor)
	}
	var c PageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return PageCursor{}, fmt.Errorf("%w: malformed cursor: %v", ErrBadCursor, err)
	}
	return c, nil
}
