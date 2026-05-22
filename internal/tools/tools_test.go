package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestCatalog_Register_Resolve(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "x", Transport: tools.TransportInProcess},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := cat.Resolve("x")
	if !ok {
		t.Fatalf("Resolve(x): not found")
	}
	if got.Tool.Name != "x" {
		t.Errorf("expected Name=x, got %q", got.Tool.Name)
	}
	_, ok = cat.Resolve("nope")
	if ok {
		t.Errorf("expected miss")
	}
}

func TestCatalog_Register_EmptyName_Rejects(t *testing.T) {
	cat := tools.NewCatalog()
	err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: ""},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	})
	if err == nil {
		t.Fatalf("expected duplicate-name error, got nil")
	}
	if !errors.Is(err, tools.ErrToolDuplicateName) {
		t.Fatalf("expected ErrToolDuplicateName, got: %v", err)
	}
}

func TestCatalog_Register_NilInvoke_Rejects(t *testing.T) {
	cat := tools.NewCatalog()
	err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "x"}})
	if err == nil {
		t.Fatalf("expected nil-Invoke error, got nil")
	}
}

func TestCatalog_Register_Duplicate_Rejects(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "x"},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := cat.Register(d)
	if err == nil {
		t.Fatalf("expected duplicate, got nil")
	}
	if !errors.Is(err, tools.ErrToolDuplicateName) {
		t.Fatalf("expected ErrToolDuplicateName, got: %v", err)
	}
}

func TestCatalogFilter_HasFullTriple(t *testing.T) {
	cases := []struct {
		name string
		f    tools.CatalogFilter
		want bool
	}{
		{"empty", tools.CatalogFilter{}, false},
		{"tenant-only", tools.CatalogFilter{TenantID: "t"}, false},
		{"two-of-three", tools.CatalogFilter{TenantID: "t", UserID: "u"}, false},
		{"full", tools.CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"}, true},
	}
	for _, c := range cases {
		if got := c.f.HasFullTriple(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCatalog_List_DeterministicOrder(t *testing.T) {
	cat := tools.NewCatalog()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Loading: tools.LoadingAlways},
			Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		})
		if err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	list := cat.List(tools.CatalogFilter{})
	if len(list) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "mu" || list[2].Name != "zeta" {
		t.Errorf("expected sorted order, got %v", []string{list[0].Name, list[1].Name, list[2].Name})
	}
}

func TestCatalog_List_NameRegex(t *testing.T) {
	cat := tools.NewCatalog()
	for _, name := range []string{"http.get", "http.post", "rpc.call"} {
		err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Loading: tools.LoadingAlways},
			Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		})
		if err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	list := cat.List(tools.CatalogFilter{NameRegex: regexp.MustCompile(`^http\.`)})
	if len(list) != 2 {
		t.Fatalf("expected 2, got %v", names(list))
	}
}

func TestWithCatalog_Roundtrips(t *testing.T) {
	cat := tools.NewCatalog()
	ctx := tools.WithCatalog(context.Background(), cat)
	got, ok := tools.Catalog(ctx)
	if !ok {
		t.Fatalf("expected catalog in ctx")
	}
	if got != cat {
		t.Fatalf("expected same catalog, got different reference")
	}
}

func TestMustCatalog_PanicsOnMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	tools.MustCatalog(context.Background())
}

func names(ts []tools.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
