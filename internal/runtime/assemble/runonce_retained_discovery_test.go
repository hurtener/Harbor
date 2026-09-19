package assemble_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools"
)

type retainedDiscoveryClient struct {
	mu       sync.Mutex
	requests map[string][]llm.CompleteRequest
}

func (c *retainedDiscoveryClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[q.RunID] = append(c.requests[q.RunID], req)
	if q.RunID == "first" && len(c.requests[q.RunID]) == 1 {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "search", Name: "tool_search", Args: json.RawMessage(`{"query":"document"}`)}}}, nil
	}
	return llm.CompleteResponse{Content: "done"}, nil
}
func (*retainedDiscoveryClient) Close(context.Context) error { return nil }

func TestRunOnce_RetainedDiscoveryRefreshAndRevocation(t *testing.T) {
	s := runnableStack(t)
	defer func() { _ = s.Close(context.Background()) }()
	client := &retainedDiscoveryClient{requests: map[string][]llm.CompleteRequest{}}
	s.Planner = react.New(client)
	var searches, edits atomic.Int64
	// Script the discovery result at the tool boundary; all dispatch, retained
	// storage, restoration and request construction use production components.
	search := tools.ToolDescriptor{Tool: tools.Tool{Name: "tool_search", Loading: tools.LoadingAlways, Transport: tools.TransportInProcess, ArgsSchema: json.RawMessage(`{"type":"object"}`)}}
	search.Invoke = func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		searches.Add(1)
		return tools.ToolResult{Value: map[string]any{"tools": []any{map[string]any{"name": "edit_document", "description": "STALE-DESCRIPTION"}}, "count": 1}}, nil
	}
	replacer, ok := s.Catalog.(tools.CatalogReplacer)
	if !ok {
		t.Fatal("catalog replacement missing")
	}
	if err := s.Catalog.Register(search); err != nil {
		t.Fatal(err)
	}
	edit := tools.ToolDescriptor{Tool: tools.Tool{Name: "edit_document", Description: "OLD", Loading: tools.LoadingDeferred, Transport: tools.TransportInProcess, ArgsSchema: json.RawMessage(`{"type":"object"}`)}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		edits.Add(1)
		return tools.ToolResult{Value: "saved"}, nil
	}}
	if err := s.Catalog.Register(edit); err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "discovery"}
	run := func(name string) {
		t.Helper()
		if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID(name), assemble.WithRetainedContext(4)); err != nil {
			t.Fatal(err)
		}
	}
	run("first")
	edit.Tool.Description = "CURRENT-DESCRIPTION"
	edit.Tool.ArgsSchema = json.RawMessage(`{"type":"object","properties":{"current_version":{"type":"integer"}}}`)
	if err := replacer.Replace([]tools.ToolDescriptor{edit}); err != nil {
		t.Fatal(err)
	}
	run("second")
	client.mu.Lock()
	second := client.requests["second"][0]
	client.mu.Unlock()
	found := false
	for _, decl := range second.Tools {
		if decl.Name == "edit_document" {
			found = true
			if decl.Description != "CURRENT-DESCRIPTION" || !strings.Contains(string(decl.Schema), "current_version") {
				t.Fatal("restored stale tool declaration")
			}
		}
	}
	if !found {
		t.Fatal("next run forgot discovered deferred tool")
	}
	edit.Tool.AuthScopes = []string{"now-required"}
	if err := replacer.Replace([]tools.ToolDescriptor{edit}); err != nil {
		t.Fatal(err)
	}
	run("third")
	client.mu.Lock()
	third := client.requests["third"][0]
	client.mu.Unlock()
	for _, decl := range third.Tools {
		if decl.Name == "edit_document" {
			t.Fatal("retained discovery bypassed revoked scope")
		}
	}
	if searches.Load() != 1 || edits.Load() != 0 {
		t.Fatal("restoration executed historical tools")
	}
}
