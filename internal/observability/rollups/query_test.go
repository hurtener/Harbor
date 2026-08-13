package rollups

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCursor_RoundTrip(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	producer := Query{
		From:     from,
		To:       from.Add(3 * time.Hour),
		Bucket:   BucketHour,
		GroupBy:  []Dimension{DimensionTenant, DimensionModel},
		Filter:   Filter{TenantIDs: []string{"tenant-b", "tenant-a"}},
		Measures: []Measure{MeasureLLMCostMicros},
		Sort:     SortKeyBucketAsc,
		Limit:    2,
	}
	c := PageCursor{
		ShapeVersion: CursorShapeVersion,
		Fingerprint:  QueryShapeFingerprint(producer),
		BucketNano:   time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC).UnixNano(),
		MeasureVal:   42_500_000, // exact int64 micro-units — never float
		Group:        DimensionValues{DimensionTenant: "tenant-a", DimensionModel: "model-x"},
	}
	enc, err := EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if enc == "" {
		t.Fatal("empty cursor encoding")
	}
	dec, err := DecodeCursor(enc)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if dec.ShapeVersion != c.ShapeVersion || dec.Fingerprint != c.Fingerprint {
		t.Fatalf("decoded shape binding = %d/%q; want %d/%q", dec.ShapeVersion, dec.Fingerprint, c.ShapeVersion, c.Fingerprint)
	}
	if dec.BucketNano != c.BucketNano || dec.MeasureVal != c.MeasureVal {
		t.Fatalf("decoded cursor = %+v; want %+v", dec, c)
	}
	if dec.Group[DimensionTenant] != "tenant-a" || dec.Group[DimensionModel] != "model-x" {
		t.Fatalf("decoded group = %+v", dec.Group)
	}
	// Determinism: equal cursors encode to identical bytes.
	enc2, err := EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor 2: %v", err)
	}
	if enc != enc2 {
		t.Fatalf("cursor encoding not deterministic: %q vs %q", enc, enc2)
	}
}

// TestCursor_ShapeFingerprint pins the fingerprint contract: shape fields
// change it, set-order does not, and Limit/Cursor are excluded.
func TestCursor_ShapeFingerprint(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	base := Query{
		From:     from,
		To:       from.Add(3 * time.Hour),
		Bucket:   BucketHour,
		GroupBy:  []Dimension{DimensionSession},
		Filter:   Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"s1", "s2"}},
		Measures: []Measure{MeasureLLMCostMicros},
		Sort:     SortKeyBucketAsc,
		Limit:    2,
		Cursor:   "stale-position", // excluded: cursor never changes the shape
	}
	fp := QueryShapeFingerprint(base)

	// Limit and Cursor are excluded.
	relimit := base
	relimit.Limit = 100
	if got := QueryShapeFingerprint(relimit); got != fp {
		t.Fatal("Limit must be excluded from the shape fingerprint")
	}
	// Filter set order and measure set order are not part of the shape.
	reorder := base
	reorder.Filter = Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"s2", "s1"}}
	reorder.Measures = []Measure{MeasureLLMCostMicros}
	if got := QueryShapeFingerprint(reorder); got != fp {
		t.Fatal("filter/measure set order must not change the shape fingerprint")
	}
	// The empty Sort defaults to bucket_asc — same shape as explicit.
	implicitSort := base
	implicitSort.Sort = ""
	if got := QueryShapeFingerprint(implicitSort); got != fp {
		t.Fatal("empty Sort must normalise to the default bucket_asc fingerprint")
	}
	// A non-UTC instant of the SAME instant normalises to the same shape.
	otherLoc := base
	otherLoc.From = base.From.In(time.FixedZone("x", 3*60*60))
	if got := QueryShapeFingerprint(otherLoc); got != fp {
		t.Fatal("From/To must be normalised to UTC instants for the fingerprint")
	}

	// Every shape field changes the fingerprint.
	mutations := map[string]func(*Query){
		"window": func(q *Query) { q.From = q.From.Add(time.Hour); q.To = q.To.Add(time.Hour) },
		"bucket": func(q *Query) { q.Bucket = BucketMinute },
		"filter tenant": func(q *Query) {
			q.Filter = Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"s1", "s2"}, Models: []string{"m"}}
		},
		"filter user": func(q *Query) {
			q.Filter = Filter{TenantIDs: []string{"tenant-a"}, UserIDs: []string{"u1"}, SessionIDs: []string{"s1", "s2"}}
		},
		"filter session": func(q *Query) {
			q.Filter = Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"s1", "s2", "s3"}}
		},
		"filter model": func(q *Query) {
			q.Filter = Filter{TenantIDs: []string{"tenant-a"}, SessionIDs: []string{"s1", "s2"}, Models: []string{"m2"}}
		},
		"group by order": func(q *Query) { q.GroupBy = []Dimension{DimensionTenant, DimensionSession} },
		"measures": func(q *Query) {
			q.Measures = []Measure{MeasureLLMCostMicros, MeasureLLMCompletions}
		},
		"sort":         func(q *Query) { q.Sort = SortKeyBucketDesc },
		"sort measure": func(q *Query) { q.Sort = SortKeyMeasureAsc; q.SortMeasure = MeasureLLMCompletions },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			q := base
			mutate(&q)
			if got := QueryShapeFingerprint(q); got == fp {
				t.Fatalf("changing %s must change the shape fingerprint", name)
			}
		})
	}
}

// TestCursor_MeasureValExact pins the >2^53 guarantee on the pagination
// cursor: the sort value is an exact int64, so a measure sort over huge
// counters never loses the low bits through a float64 cursor.
func TestCursor_MeasureValExact(t *testing.T) {
	big := int64(1<<53) + 1
	c := PageCursor{BucketNano: 1, MeasureVal: big}
	enc, err := EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	dec, err := DecodeCursor(enc)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if dec.MeasureVal != big {
		t.Fatalf("cursor measure val = %d; want %d (exact — float64 would lose the low bit)", dec.MeasureVal, big)
	}
}

func TestCursor_Malformed(t *testing.T) {
	for _, bad := range []string{"", "not-base64!!", "aGVsbG8"} {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("DecodeCursor(%q) err = %v; want ErrBadCursor", bad, err)
		}
	}
}

// TestCursor_StrictDecode pins the strict bounded decode contract: unknown
// fields, mistyped fields, trailing data, and over-long payloads are all
// rejected with ErrBadCursor — never silently ignored or half-parsed.
func TestCursor_StrictDecode(t *testing.T) {
	enc := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	known := `{"ShapeVersion":1,"Fingerprint":"fp","BucketNano":1,"MeasureVal":0,"Group":{}}`
	cases := []string{
		enc(`{"BucketNano":1,"Evil":true}`),                                     // unknown field
		enc(`{"ShapeVersion":1,"BucketNano":1,"Extra":[],"Group":{}}`),          // unknown field
		enc(known + `trailing`),                                                 // trailing garbage after the value
		enc(known + `{"BucketNano":2}`),                                         // second top-level value
		enc(`{"BucketNano":"1"}`),                                               // mistyped field
		enc(`{"BucketNano":1,"Group":{}`),                                       // truncated JSON
		strings.Repeat("A", base64.RawURLEncoding.EncodedLen(maxCursorBytes)+1), // over-long
	}
	for i, bad := range cases {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("strict decode case %d err = %v; want ErrBadCursor", i, err)
		}
	}
	// A valid cursor still decodes (the bound does not reject legitimate
	// payloads).
	good, err := EncodeCursor(PageCursor{ShapeVersion: CursorShapeVersion, Fingerprint: "fp", BucketNano: 1, Group: DimensionValues{}})
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if _, err := DecodeCursor(good); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
}

// TestQueryValidate_CursorShapeBinding pins the cursor's shape binding at
// the Validate gate: a cursor produced by a query of a different shape
// version or fingerprint is rejected with ErrBadCursor before any paging.
func TestQueryValidate_CursorShapeBinding(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	base := Query{
		From:     from,
		To:       from.Add(3 * time.Hour),
		Bucket:   BucketHour,
		GroupBy:  []Dimension{DimensionSession},
		Measures: []Measure{MeasureLLMCostMicros},
		Sort:     SortKeyBucketAsc,
		Limit:    2,
	}
	produced := PageCursor{
		ShapeVersion: CursorShapeVersion,
		Fingerprint:  QueryShapeFingerprint(base),
		BucketNano:   from.UnixNano(),
		Group:        DimensionValues{DimensionSession: "session-1"},
	}
	enc, err := EncodeCursor(produced)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}

	// The same shape validates.
	same := base
	same.Cursor = enc
	if err := same.Validate(); err != nil {
		t.Fatalf("same-shape cursor must validate: %v", err)
	}

	// A stale shape version is rejected.
	stale := produced
	stale.ShapeVersion = CursorShapeVersion + 1
	staleEnc, err := EncodeCursor(stale)
	if err != nil {
		t.Fatalf("EncodeCursor(stale): %v", err)
	}
	q := base
	q.Cursor = staleEnc
	if err := q.Validate(); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("stale-version cursor err = %v; want ErrBadCursor", err)
	}

	// A fingerprint from a different shape (different bucket) is rejected.
	other := produced
	otherBucket := base
	otherBucket.Bucket = BucketMinute
	other.Fingerprint = QueryShapeFingerprint(otherBucket)
	otherEnc, err := EncodeCursor(other)
	if err != nil {
		t.Fatalf("EncodeCursor(other): %v", err)
	}
	q = base
	q.Cursor = otherEnc
	if err := q.Validate(); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("foreign-shape cursor err = %v; want ErrBadCursor", err)
	}

	// A pre-binding cursor (no version/fingerprint) is rejected: old
	// cursors fail loudly rather than silently mis-paginate.
	legacy, err := EncodeCursor(PageCursor{BucketNano: from.UnixNano(), Group: DimensionValues{DimensionSession: "s"}})
	if err != nil {
		t.Fatalf("EncodeCursor(legacy): %v", err)
	}
	q = base
	q.Cursor = legacy
	if err := q.Validate(); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("legacy cursor err = %v; want ErrBadCursor", err)
	}
}

func TestQueryValidate(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	valid := Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   BucketMinute,
		Measures: []Measure{MeasureLLMCostMicros},
		Sort:     SortKeyBucketAsc,
		Limit:    100,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid query must pass: %v", err)
	}

	// Empty Sort defaults to bucket_asc.
	q := valid
	q.Sort = ""
	if err := q.Validate(); err != nil {
		t.Fatalf("empty-sort query must pass: %v", err)
	}

	// A measure sort requires a closed SortMeasure.
	q = valid
	q.Sort = SortKeyMeasureDesc
	if err := q.Validate(); !errors.Is(err, ErrQueryInvalid) {
		t.Fatalf("measure sort without SortMeasure: err=%v; want ErrQueryInvalid", err)
	}
	q.SortMeasure = MeasureLLMCostMicros
	if err := q.Validate(); err != nil {
		t.Fatalf("measure sort with SortMeasure must pass: %v", err)
	}

	// The window budget fails loudly for a window spanning too many
	// buckets at the requested size.
	q = valid
	q.From = from.Add(-time.Duration(MaxBuckets) * time.Minute)
	if err := q.Validate(); !errors.Is(err, ErrQueryBudget) {
		t.Fatalf("over-budget window: err=%v; want ErrQueryBudget", err)
	}

	// A valid wide window passes at a coarser bucket (both edges stay on
	// the hour grid).
	q = valid
	q.From = from.Add(-1000 * time.Hour)
	q.Bucket = BucketHour
	if err := q.Validate(); err != nil {
		t.Fatalf("coarsened window must pass: %v", err)
	}

	// A misaligned edge fails loudly with ErrQueryInvalid — a window never
	// silently counts a partial bucket.
	q = valid
	q.From = from.Add(time.Second)
	if err := q.Validate(); !errors.Is(err, ErrQueryInvalid) {
		t.Fatalf("unaligned From: err=%v; want ErrQueryInvalid", err)
	}
	q = valid
	q.To = from.Add(time.Hour).Add(-time.Nanosecond)
	if err := q.Validate(); !errors.Is(err, ErrQueryInvalid) {
		t.Fatalf("unaligned To: err=%v; want ErrQueryInvalid", err)
	}
}

// TestQueryValidate_WindowAligned pins the fail-loud window contract: From
// and To must fall exactly on the requested bucket's fixed-UTC grid, so 1ns
// and 1s misalignments are rejected for minute, hour, and day, and exact
// boundaries pass.
func TestQueryValidate_WindowAligned(t *testing.T) {
	minute := time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC)
	hour := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	day := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		from   time.Time
		to     time.Time
		bucket BucketSize
		wantOK bool
	}{
		{"minute exact", minute, minute.Add(time.Minute), BucketMinute, true},
		{"minute 1ns from", minute.Add(time.Nanosecond), minute.Add(time.Minute), BucketMinute, false},
		{"minute 1s from", minute.Add(time.Second), minute.Add(time.Minute), BucketMinute, false},
		{"minute 1ns to", minute, minute.Add(time.Minute).Add(-time.Nanosecond), BucketMinute, false},
		{"minute 1s to", minute, minute.Add(time.Minute).Add(-time.Second), BucketMinute, false},
		{"hour exact", hour, hour.Add(time.Hour), BucketHour, true},
		{"hour 1ns from", hour.Add(time.Nanosecond), hour.Add(time.Hour), BucketHour, false},
		{"hour 1s from", hour.Add(time.Second), hour.Add(time.Hour), BucketHour, false},
		{"hour 1ns to", hour, hour.Add(time.Hour).Add(-time.Nanosecond), BucketHour, false},
		{"hour 1s to", hour, hour.Add(time.Hour).Add(-time.Second), BucketHour, false},
		{"day exact", day, day.Add(24 * time.Hour), BucketDay, true},
		{"day 1ns from", day.Add(time.Nanosecond), day.Add(24 * time.Hour), BucketDay, false},
		{"day 1s from", day.Add(time.Second), day.Add(24 * time.Hour), BucketDay, false},
		{"day 1ns to", day, day.Add(24 * time.Hour).Add(-time.Nanosecond), BucketDay, false},
		{"day 1s to", day, day.Add(24 * time.Hour).Add(-time.Second), BucketDay, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := Query{
				From:     tc.from,
				To:       tc.to,
				Bucket:   tc.bucket,
				Measures: []Measure{MeasureLLMCostMicros},
				Limit:    10,
			}
			err := q.Validate()
			if tc.wantOK && err != nil {
				t.Fatalf("aligned window must validate: %v", err)
			}
			if !tc.wantOK && !errors.Is(err, ErrQueryInvalid) {
				t.Fatalf("misaligned window err = %v; want ErrQueryInvalid", err)
			}
		})
	}
}

// TestMeasureValue_WireReady pins the typed integer/decimal representation:
// the value is JSON-safe (encoding/json keeps int64 N and Scale exact) and
// never float-normalised.
func TestMeasureValue_WireReady(t *testing.T) {
	big := int64(1<<53) + 1
	v := MeasureValue{N: big, Scale: CostScaleMicros}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back MeasureValue
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.N != big {
		t.Fatalf("wire round-trip N = %d; want %d (exact)", back.N, big)
	}
	if back.Scale != CostScaleMicros {
		t.Fatalf("wire round-trip Scale = %d; want %d", back.Scale, CostScaleMicros)
	}
}
