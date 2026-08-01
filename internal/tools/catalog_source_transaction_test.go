package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type sourceSyncRecorder struct {
	mu    sync.Mutex
	names []string
}

func (*sourceSyncRecorder) Search(context.Context, string, []string, int) ([]Tool, error) {
	return nil, nil
}
func (r *sourceSyncRecorder) Sync(_ context.Context, tools []Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tool := range tools {
		r.names = append(r.names, tool.Name)
	}
	return nil
}
func (*sourceSyncRecorder) Close() error { return nil }

func TestCatalogStageSource_RollbackNeverIndexesStagedDescriptors(t *testing.T) {
	cache := &sourceSyncRecorder{}
	cat := NewCatalog(WithSearchCache(cache))
	d := ToolDescriptor{
		Tool:   Tool{Name: "staged_echo", Source: "staged", Transport: TransportMCP},
		Invoke: func(context.Context, json.RawMessage) (ToolResult, error) { return ToolResult{}, nil },
	}
	swap, err := cat.StageSource("staged", []ToolDescriptor{d}, false)
	if err != nil {
		t.Fatalf("StageSource: %v", err)
	}
	if err := swap.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.names) != 0 {
		t.Fatalf("rolled-back descriptors entered search index: %v", cache.names)
	}
}
