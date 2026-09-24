package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestPlannerView_ResolveRechecksScopesWithoutHidingDeferred(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()
	for _, tool := range []Tool{{Name: "deferred", Loading: LoadingDeferred}, {Name: "scoped", Loading: LoadingDeferred, AuthScopes: []string{"required"}}} {
		if err := cat.Register(ToolDescriptor{Tool: tool, Invoke: func(context.Context, json.RawMessage) (ToolResult, error) { return ToolResult{}, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	denied := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	if _, ok := denied.Resolve("scoped"); ok {
		t.Fatal("name resolution bypasses current scopes")
	}
	if _, ok := denied.Resolve("deferred"); !ok {
		t.Fatal("deferred loading mistaken for revoked authority")
	}
	allowed := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s", GrantedScopes: []string{"required"}})
	if _, ok := allowed.Resolve("scoped"); !ok {
		t.Fatal("authorized tool disappeared")
	}
}

func TestPlannerView_ConcurrentScopedNameResolution(t *testing.T) {
	t.Parallel()
	cat := plannerViewTestCatalog(t)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			filter := CatalogFilter{TenantID: "t", UserID: fmt.Sprint(i), SessionID: "s"}
			if i%2 == 0 {
				filter.GrantedScopes = []string{"scope:a"}
			}
			v := NewPlannerView(cat, filter)
			if _, ok := v.Resolve("scoped_tool"); ok != (i%2 == 0) {
				t.Error("scope leaked through shared catalog resolution")
			}
		}()
	}
	wg.Wait()
}
