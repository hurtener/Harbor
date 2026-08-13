package memstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// TestQuery_IndexProportionalScan pins the indexed-query contract: a Query
// resolves its candidate rows through the bucket + dimension indexes
// (proportional to the bounded window and filter) and never snapshots or
// full-scans the row table. This is the parity pin for the indexed access
// SQLite / Postgres drivers will use (WHERE bucket_start BETWEEN … AND …
// AND tenant = …).
func TestQuery_IndexProportionalScan(t *testing.T) {
	s := New()
	ctx := context.Background()
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	// 100 minute buckets × 100 rows (25 per tenant) = 10_000 rows total.
	var deltas []rollups.Delta
	var seq uint64
	for b := 0; b < 100; b++ {
		bstart := base.Add(time.Duration(b) * time.Minute)
		for i := 0; i < 100; i++ {
			seq++
			tenant := fmt.Sprintf("tenant-%02d", i%4)
			deltas = append(deltas, rollups.Delta{
				Key: rollups.Key{
					BucketStart: bstart,
					TenantID:    tenant,
					UserID:      fmt.Sprintf("u%03d", i),
					SessionID:   fmt.Sprintf("s%03d", i),
					Model:       "model-a",
				},
				Add: rollups.MeasureSet{LLMCompletions: 1},
			})
		}
	}
	if err := s.ApplyBatch(ctx, rollups.Batch{Checkpoint: seq, Deltas: deltas}); err != nil {
		t.Fatalf("seed ApplyBatch: %v", err)
	}

	// A narrow window + tenant filter must scan exactly the 25 rows of
	// that tenant in that ONE minute bucket — not the 10_000-row table.
	before := s.scannedKeys.Load()
	res, err := s.Query(ctx, rollups.Query{
		From:     base,
		To:       base.Add(time.Minute),
		Bucket:   rollups.BucketMinute,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-00"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	scanned := s.scannedKeys.Load() - before
	if scanned != 25 {
		t.Fatalf("filtered query scanned %d rows; want exactly 25 (one tenant in one minute bucket), never a full scan", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 25 {
		t.Fatalf("filtered result = %+v; want 1 row with 25 completions", res.Rows)
	}

	// A window-only query (no filter) over one bucket scans exactly that
	// bucket's 100 rows — the window, not the table.
	before = s.scannedKeys.Load()
	res, err = s.Query(ctx, rollups.Query{
		From:     base.Add(50 * time.Minute),
		To:       base.Add(51 * time.Minute),
		Bucket:   rollups.BucketMinute,
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("window query: %v", err)
	}
	scanned = s.scannedKeys.Load() - before
	if scanned != 100 {
		t.Fatalf("window query scanned %d rows; want exactly 100 (one minute bucket)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 100 {
		t.Fatalf("window result = %+v; want 1 row with 100 completions", res.Rows)
	}

	// A coarsened (hour) query over the same minute storage must resolve
	// through the minute index: one hour bucket aggregates its 60 minute
	// buckets → 6_000 rows scanned, still not a snapshot of anything
	// outside the window.
	before = s.scannedKeys.Load()
	res, err = s.Query(ctx, rollups.Query{
		From:     base,
		To:       base.Add(time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("hour query: %v", err)
	}
	scanned = s.scannedKeys.Load() - before
	if scanned != 6_000 {
		t.Fatalf("hour query scanned %d rows; want 6_000 (60 minute buckets × 100 rows)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 6_000 {
		t.Fatalf("hour result = %+v; want 1 row with 6_000 completions", res.Rows)
	}
}
