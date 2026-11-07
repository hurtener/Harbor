package tools_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/searchcache"
)

// search_reconcile_test.go — the catalog keeps its search index a mirror of
// the current catalog: detached sources, dropped tools and rolled-back
// publications leave no rows behind.

func indexedCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	sc, err := searchcache.New(searchcache.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("searchcache.New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return tools.NewCatalog(tools.WithSearchCache(sc))
}

func descriptor(name string, source tools.ToolSourceID, desc string) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Source: source, Description: desc, Transport: tools.TransportMCP},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}
}

func indexed(cat tools.ToolCatalog, query string) map[string]bool {
	out := map[string]bool{}
	for _, t := range cat.Search(context.Background(), tools.ToolSearchRequest{
		Query: query, Limit: 50, Visible: func(string) bool { return true },
	}) {
		out[t.Name] = true
	}
	return out
}

func TestCatalogSearch_NilVisibleAdmitsNothing(t *testing.T) {
	cat := indexedCatalog(t)
	if err := cat.Register(descriptor("srv_widget", "srv", "widget maker")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := cat.Search(context.Background(), tools.ToolSearchRequest{Query: "widget", Limit: 10}); len(got) != 0 {
		t.Fatalf("Search without a visibility predicate returned %v", got)
	}
}

func TestCatalogSearch_DeregisterSourcePrunesIndex(t *testing.T) {
	cat := indexedCatalog(t)
	for _, d := range []tools.ToolDescriptor{
		descriptor("gone_widget", "gone", "widget maker"),
		descriptor("kept_widget", "kept", "widget maker"),
	} {
		if err := cat.Register(d); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	dereg, ok := cat.(tools.CatalogSourceDeregisterer)
	if !ok {
		t.Fatal("catalog does not implement CatalogSourceDeregisterer")
	}
	if n := dereg.DeregisterSource("gone"); n != 1 {
		t.Fatalf("DeregisterSource removed %d, want 1", n)
	}
	got := indexed(cat, "widget")
	if got["gone_widget"] || !got["kept_widget"] {
		t.Fatalf("index after detach = %v, want only kept_widget", got)
	}
}

func TestCatalogSearch_SourcePublicationDropsRemovedTools(t *testing.T) {
	cat := indexedCatalog(t)
	first, err := cat.StageSource("srv", []tools.ToolDescriptor{
		descriptor("srv_alpha", "srv", "widget alpha"),
		descriptor("srv_beta", "srv", "widget beta"),
	}, false)
	if err != nil {
		t.Fatalf("StageSource: %v", err)
	}
	first.Commit()
	if got := indexed(cat, "widget"); !got["srv_alpha"] || !got["srv_beta"] {
		t.Fatalf("index after first publication = %v", got)
	}
	// The server's tool set changes: beta disappears, alpha's text changes.
	second, err := cat.StageSource("srv", []tools.ToolDescriptor{
		descriptor("srv_alpha", "srv", "gadget alpha"),
	}, true)
	if err != nil {
		t.Fatalf("StageSource replace: %v", err)
	}
	second.Commit()
	if got := indexed(cat, "widget"); len(got) != 0 {
		t.Fatalf("stale rows after republication: %v", got)
	}
	if got := indexed(cat, "gadget"); !got["srv_alpha"] {
		t.Fatalf("updated row missing: %v", got)
	}
}

func TestCatalogSearch_RollbackRestoresPriorIndex(t *testing.T) {
	cat := indexedCatalog(t)
	first, err := cat.StageSource("srv", []tools.ToolDescriptor{descriptor("srv_alpha", "srv", "widget alpha")}, false)
	if err != nil {
		t.Fatalf("StageSource: %v", err)
	}
	first.Commit()
	second, err := cat.StageSource("srv", []tools.ToolDescriptor{descriptor("srv_gamma", "srv", "widget gamma")}, true)
	if err != nil {
		t.Fatalf("StageSource replace: %v", err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got := indexed(cat, "widget")
	if !got["srv_alpha"] || got["srv_gamma"] {
		t.Fatalf("index after rollback = %v, want only srv_alpha", got)
	}
}

// TestCatalogSearch_ConcurrentReconcileConverges races registrations,
// publications and detaches under -race; the index must end equal to the
// catalog.
func TestCatalogSearch_ConcurrentReconcileConverges(t *testing.T) {
	cat := indexedCatalog(t)
	dereg := cat.(tools.CatalogSourceDeregisterer)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := tools.ToolSourceID("src" + string(rune('a'+i)))
			for round := range 5 {
				swap, err := cat.StageSource(src, []tools.ToolDescriptor{descriptor(string(src)+"_widget", src, "widget")}, round > 0)
				if err != nil {
					t.Errorf("StageSource: %v", err)
					return
				}
				swap.Commit()
			}
			if i%2 == 0 {
				dereg.DeregisterSource(src)
			}
		}(i)
	}
	wg.Wait()
	got := indexed(cat, "widget")
	want := map[string]bool{}
	for _, tl := range cat.List(tools.CatalogFilter{LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred}}) {
		want[tl.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("index %v != catalog %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("index %v missing %s", got, name)
		}
	}
}
