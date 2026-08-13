package memstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// TestQuery_IndexProportionalScan pins the indexed-query contract: a Query
// resolves its candidate rows through the bucket + dimension indexes — the
// cheapest applicable seed plus direct per-row checks — and never snapshots
// or full-scans the row table. This is the parity pin for the indexed access
// SQLite / Postgres drivers will use (WHERE bucket_start BETWEEN … AND …
// AND tenant = …). scannedKeys counts every index entry actually visited,
// including entries the direct checks reject.
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

	// A narrow window + tenant filter. The bucket-window seed holds 100
	// entries (the whole minute bucket); the tenant axis for tenant-00
	// holds 25 × 100 buckets = 2_500 entries. The window is cheaper, so
	// the query seeds on it and direct-checks the tenant axis: exactly
	// 100 index entries are visited (25 kept, 75 rejected) — never the
	// 10_000-row table and never a full-retention tenant scan.
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
	if scanned != 100 {
		t.Fatalf("filtered query visited %d index entries; want exactly 100 (the one-minute bucket window: 25 kept + 75 rejected), never a full scan", scanned)
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
		t.Fatalf("window query visited %d index entries; want exactly 100 (one minute bucket)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 100 {
		t.Fatalf("window result = %+v; want 1 row with 100 completions", res.Rows)
	}

	// A coarsened (hour) query over the same minute storage must resolve
	// through the minute index: one hour bucket aggregates its 60 minute
	// buckets → 6_000 rows visited, still not a snapshot of anything
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
		t.Fatalf("hour query visited %d index entries; want 6_000 (60 minute buckets × 100 rows)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 6_000 {
		t.Fatalf("hour result = %+v; want 1 row with 6_000 completions", res.Rows)
	}
}

// TestQuery_Adversarial_NarrowWindowHugeSameTenantHistory pins the
// cheapest-seed choice the OTHER way: a one-minute window against a tenant
// whose history spans the whole retention horizon must seed on the bounded
// bucket range (100 entries) — never on the tenant axis, which holds
// 25 × 2_000 = 50_000 entries across the full horizon. The reported visited
// count must equal the real index work: 100 entries examined (25 kept,
// 75 rejected).
func TestQuery_Adversarial_NarrowWindowHugeSameTenantHistory(t *testing.T) {
	s := New()
	ctx := context.Background()
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	// 2_000 minute buckets × 100 rows (4 tenants × 25 rows) = 200_000
	// rows. tenant-00 alone spans the FULL retention horizon: 25 rows ×
	// 2_000 buckets = 50_000 index entries on the tenant axis.
	const buckets = 2_000
	var deltas []rollups.Delta
	var seq uint64
	for b := 0; b < buckets; b++ {
		bstart := base.Add(time.Duration(b) * time.Minute)
		for i := 0; i < 100; i++ {
			seq++
			deltas = append(deltas, rollups.Delta{
				Key: rollups.Key{
					BucketStart: bstart,
					TenantID:    fmt.Sprintf("tenant-%02d", i%4),
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

	// The one-minute window holds 100 entries; the tenant axis holds
	// 50_000. The bucket-window seed is the cheapest — the query must
	// visit exactly those 100 entries, never the full-retention tenant
	// axis.
	before := s.scannedKeys.Load()
	res, err := s.Query(ctx, rollups.Query{
		From:     base.Add(500 * time.Minute),
		To:       base.Add(501 * time.Minute),
		Bucket:   rollups.BucketMinute,
		Filter:   rollups.Filter{TenantIDs: []string{"tenant-00"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	scanned := s.scannedKeys.Load() - before
	if scanned != 100 {
		t.Fatalf("visited %d index entries; want 100 (the one-minute bucket window — the cheaper seed — not the 50_000-entry tenant axis)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 25 {
		t.Fatalf("result = %+v; want 1 row with 25 completions", res.Rows)
	}
}

// TestQuery_Adversarial_NarrowSelectiveDimensionLargeWindow pins the
// cheapest-seed choice: a large window whose seed would visit ~200_000
// entries, against a user axis holding exactly ONE entry, must seed on the
// user axis and direct-check the window. The reported visited count must
// equal the real index work: exactly 1 entry.
func TestQuery_Adversarial_NarrowSelectiveDimensionLargeWindow(t *testing.T) {
	s := New()
	ctx := context.Background()
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	// 2_000 minute buckets × 100 rows, except user u001 exists in exactly
	// ONE row (bucket 0): its user-axis index holds 1 entry, while the
	// full window holds ~198_001 entries.
	const buckets = 2_000
	var deltas []rollups.Delta
	var seq uint64
	for b := 0; b < buckets; b++ {
		bstart := base.Add(time.Duration(b) * time.Minute)
		for i := 0; i < 100; i++ {
			if i == 1 && b > 0 {
				continue // u001 only in bucket 0
			}
			seq++
			deltas = append(deltas, rollups.Delta{
				Key: rollups.Key{
					BucketStart: bstart,
					TenantID:    fmt.Sprintf("tenant-%02d", i%4),
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

	// The full 2_000-bucket window with a u001 filter: the window seed
	// would visit ~198_001 entries; the user axis for u001 holds exactly
	// 1. The axis seed is the cheapest — the query must visit exactly
	// that 1 entry and verify the window + other axes directly.
	before := s.scannedKeys.Load()
	res, err := s.Query(ctx, rollups.Query{
		From:     base,
		To:       base.Add(buckets * time.Minute),
		Bucket:   rollups.BucketMinute,
		Filter:   rollups.Filter{UserIDs: []string{"u001"}},
		Measures: []rollups.Measure{rollups.MeasureLLMCompletions},
		Sort:     rollups.SortKeyBucketAsc,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	scanned := s.scannedKeys.Load() - before
	if scanned != 1 {
		t.Fatalf("visited %d index entries; want 1 (the user axis for u001 — the cheaper seed — not the ~198_001-entry window)", scanned)
	}
	if len(res.Rows) != 1 || res.Rows[0].Measures[rollups.MeasureLLMCompletions].N != 1 {
		t.Fatalf("result = %+v; want 1 row with 1 completion", res.Rows)
	}
}
