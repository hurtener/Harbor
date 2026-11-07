package tools

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// flakyIndex fails its first failures writes, then records state.
type flakyIndex struct {
	mu       sync.Mutex
	failures int
	rows     map[string]bool
}

func (f *flakyIndex) fail() bool {
	if f.failures > 0 {
		f.failures--
		return true
	}
	return false
}

func (f *flakyIndex) Search(_ context.Context, req ToolSearchRequest) ([]Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Tool
	for name := range f.rows {
		if req.Visible(name) {
			out = append(out, Tool{Name: name})
		}
	}
	return out, nil
}

func (f *flakyIndex) Sync(_ context.Context, in []Tool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail() {
		return errors.New("index unavailable")
	}
	for _, t := range in {
		f.rows[t.Name] = true
	}
	return nil
}

func (f *flakyIndex) Remove(_ context.Context, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail() {
		return errors.New("index unavailable")
	}
	for _, n := range names {
		delete(f.rows, n)
	}
	return nil
}

func (*flakyIndex) Close() error { return nil }

func (f *flakyIndex) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[name]
}

// TestCatalogSearch_FailedIndexWritesAreRetried: a failed upsert or removal
// is not a silent permanent gap — the next Search (or reconcile) retries it.
func TestCatalogSearch_FailedIndexWritesAreRetried(t *testing.T) {
	idx := &flakyIndex{failures: 1, rows: map[string]bool{}}
	cat := NewCatalog(WithSearchCache(idx)).(*catalog)
	d := ToolDescriptor{
		Tool:   Tool{Name: "srv_tool", Source: "srv", Transport: TransportMCP},
		Invoke: func(context.Context, json.RawMessage) (ToolResult, error) { return ToolResult{}, nil },
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if idx.has("srv_tool") {
		t.Fatal("precondition: the first index write should have failed")
	}
	all := func(string) bool { return true }
	if got := cat.Search(context.Background(), ToolSearchRequest{Query: "x", Limit: 5, Visible: all}); len(got) != 1 {
		t.Fatalf("Search after retry = %v, want the retried row", got)
	}
	// A failed removal is retried too.
	idx.mu.Lock()
	idx.failures = 1
	idx.mu.Unlock()
	if n := cat.DeregisterSource("srv"); n != 1 {
		t.Fatalf("DeregisterSource = %d", n)
	}
	if !idx.has("srv_tool") {
		t.Fatal("precondition: the removal should have failed")
	}
	cat.Search(context.Background(), ToolSearchRequest{Query: "x", Limit: 5, Visible: all})
	if idx.has("srv_tool") {
		t.Fatal("failed removal was never retried; stale row persists")
	}
	if cat.searchDirtyN.Load() != 0 {
		t.Fatalf("dirty set not drained: %d", cat.searchDirtyN.Load())
	}
}

func TestCatalogSearch_RetrySetIsBounded(t *testing.T) {
	cat := NewCatalog(WithSearchCache(&flakyIndex{rows: map[string]bool{}})).(*catalog)
	names := make([]string, maxSearchDirty+100)
	for i := range names {
		names[i] = "n" + string(rune('a'+i%26)) + string(rune(i))
	}
	cat.searchMu.Lock()
	cat.markSearchDirty(names)
	cat.searchMu.Unlock()
	if got := len(cat.searchDirty); got > maxSearchDirty {
		t.Fatalf("dirty set grew to %d, cap %d", got, maxSearchDirty)
	}
}

// stallingIndex fails its first Sync (leaving the name dirty), then stalls
// every later Sync until its ctx ends.
type stallingIndex struct {
	flakyIndex
	mu2     sync.Mutex
	stalled bool
}

func (s *stallingIndex) Sync(ctx context.Context, in []Tool) error {
	s.mu2.Lock()
	first := !s.stalled
	s.stalled = true
	s.mu2.Unlock()
	if first {
		return errors.New("index unavailable")
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestCatalogSearch_RetryIsBoundedByCallerCtx: the dirty-retry a Search
// performs honours the search's own ctx — a short-deadline search returns
// at its deadline, not after the reconcile's internal 5 s bound — and the
// name stays dirty for the next attempt.
func TestCatalogSearch_RetryIsBoundedByCallerCtx(t *testing.T) {
	idx := &stallingIndex{flakyIndex: flakyIndex{rows: map[string]bool{}}}
	cat := NewCatalog(WithSearchCache(idx)).(*catalog)
	if err := cat.Register(ToolDescriptor{
		Tool:   Tool{Name: "srv_tool"},
		Invoke: func(context.Context, json.RawMessage) (ToolResult, error) { return ToolResult{}, nil },
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if cat.searchDirtyN.Load() != 1 {
		t.Fatalf("precondition: name not dirty (%d)", cat.searchDirtyN.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	cat.Search(ctx, ToolSearchRequest{Query: "x", Limit: 5, Visible: func(string) bool { return true }})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Search blocked %v past its own 100ms deadline", elapsed)
	}
	if cat.searchDirtyN.Load() != 1 {
		t.Fatalf("a cancelled retry dropped the dirty name (%d)", cat.searchDirtyN.Load())
	}
}

// TestCatalogSearch_RetrySetAllocationIsBounded: a huge failed batch must
// not size the retry map past maxSearchDirty.
func TestCatalogSearch_RetrySetAllocationIsBounded(t *testing.T) {
	cat := NewCatalog(WithSearchCache(&flakyIndex{rows: map[string]bool{}})).(*catalog)
	names := make([]string, 1<<20)
	for i := range names {
		names[i] = "n" + strconv.Itoa(i)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	cat.searchMu.Lock()
	cat.markSearchDirty(names)
	cat.searchMu.Unlock()
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("marking a %d-name batch dirty allocated %d bytes; the retry set is capped at %d names", len(names), grew, maxSearchDirty)
	}
}
