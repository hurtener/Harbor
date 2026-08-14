package projectorworker_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/projectorworker"
)

// TestWorker_UnsupportedTypesAdvanceCursor pins the cursor semantics
// for canonical event types the rollup extractor deliberately maps to
// NO measure (runtime errors and other non-cost/non-task events): they
// must advance the durable watermark — never mint a row and never be
// re-read — so the projection stays caught up with the canonical stream
// without inventing measures.
func TestWorker_UnsupportedTypesAdvanceCursor(t *testing.T) {
	ctx := context.Background()
	a := tq("tenant-a", "user-1", "session-a")
	store := newMemStore(t)
	src := &stubSource{events: seq(
		unsupportedEvent(base.Add(time.Minute), a),
		unsupportedEvent(base.Add(2*time.Minute), a),
	)}

	w, err := projectorworker.New(src, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	q, err := w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 2 || q.State != rollups.StateCurrent {
		t.Fatalf("quality = %+v; want watermark 2, current", q)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureLLMCompletions); got != 0 {
		t.Fatalf("unsupported events minted completions = %d; want 0", got)
	}
	if got := measureSum(t, store, rollups.Filter{}, rollups.MeasureTasksCompleted); got != 0 {
		t.Fatalf("unsupported events minted task counts = %d; want 0", got)
	}
	if _, err := w.Advance(ctx); err != nil {
		t.Fatalf("re-read after catch-up: %v", err)
	}
	q, err = w.Quality(ctx)
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.Watermark != 2 {
		t.Fatalf("watermark after re-read = %d; want 2 (unsupported events never re-applied)", q.Watermark)
	}
}
