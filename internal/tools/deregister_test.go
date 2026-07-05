package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// deregister_test.go — coverage for CatalogSourceDeregisterer: the catalog's
// per-source removal the run-start reconcile uses to prune a detached MCP
// server's tools so the next run's projected catalog excludes them.

func regTool(t *testing.T, cat tools.ToolCatalog, name string, source tools.ToolSourceID) {
	t.Helper()
	err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}
}

func TestCatalog_DeregisterSource_RemovesOnlyThatSource(t *testing.T) {
	cat := tools.NewCatalog()
	regTool(t, cat, "srv_a", "srv")
	regTool(t, cat, "srv_b", "srv")
	regTool(t, cat, "other_c", "other")

	dc, ok := cat.(tools.CatalogSourceDeregisterer)
	if !ok {
		t.Fatal("NewCatalog does not implement CatalogSourceDeregisterer")
	}
	removed := dc.DeregisterSource("srv")
	if removed != 2 {
		t.Fatalf("DeregisterSource(srv) removed %d, want 2", removed)
	}
	if _, ok := cat.Resolve("srv_a"); ok {
		t.Error("srv_a still resolvable after deregister")
	}
	if _, ok := cat.Resolve("srv_b"); ok {
		t.Error("srv_b still resolvable after deregister")
	}
	if _, ok := cat.Resolve("other_c"); !ok {
		t.Error("unrelated source's tool was wrongly removed")
	}
	// The projected List no longer carries the deregistered source's tools.
	for _, tl := range cat.List(tools.CatalogFilter{}) {
		if tl.Source == "srv" {
			t.Errorf("List still returns a srv tool: %q", tl.Name)
		}
	}
}

func TestCatalog_DeregisterSource_Idempotent(t *testing.T) {
	cat := tools.NewCatalog()
	regTool(t, cat, "srv_a", "srv")
	dc := cat.(tools.CatalogSourceDeregisterer)
	if got := dc.DeregisterSource("srv"); got != 1 {
		t.Fatalf("first deregister removed %d, want 1", got)
	}
	if got := dc.DeregisterSource("srv"); got != 0 {
		t.Fatalf("second deregister removed %d, want 0 (idempotent)", got)
	}
	if got := dc.DeregisterSource("never-registered"); got != 0 {
		t.Fatalf("deregister of unknown source removed %d, want 0", got)
	}
}
