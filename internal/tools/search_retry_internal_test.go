package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
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
