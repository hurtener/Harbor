package rollups

import (
	"errors"
	"testing"
	"time"
)

func TestCursor_RoundTrip(t *testing.T) {
	c := PageCursor{
		BucketNano: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC).UnixNano(),
		MeasureVal: 42.5,
		Group:      DimensionValues{DimensionTenant: "tenant-a", DimensionModel: "model-x"},
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

func TestCursor_Malformed(t *testing.T) {
	for _, bad := range []string{"", "not-base64!!", "aGVsbG8"} {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("DecodeCursor(%q) err = %v; want ErrBadCursor", bad, err)
		}
	}
}

func TestQueryValidate(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	valid := Query{
		From:     from,
		To:       from.Add(time.Hour),
		Bucket:   BucketHour,
		Measures: []Measure{MeasureLLMCostUSD},
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
	q.SortMeasure = MeasureLLMCostUSD
	if err := q.Validate(); err != nil {
		t.Fatalf("measure sort with SortMeasure must pass: %v", err)
	}

	// The window budget fails loudly for a window spanning too many
	// buckets at the requested size.
	q = valid
	q.From = from.Add(-time.Duration(MaxBuckets) * time.Hour)
	if err := q.Validate(); !errors.Is(err, ErrQueryBudget) {
		t.Fatalf("over-budget window: err=%v; want ErrQueryBudget", err)
	}

	// A valid wide window passes at a coarser bucket.
	q = valid
	q.From = from.Add(-time.Duration(MaxBuckets) * time.Hour)
	q.Bucket = BucketDay
	if err := q.Validate(); err != nil {
		t.Fatalf("coarsened window must pass: %v", err)
	}
}
