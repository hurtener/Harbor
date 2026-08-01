package builtin

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

type collisionSearchCache struct{}

func (collisionSearchCache) Search(context.Context, string, []string, int) ([]tools.Tool, error) {
	return []tools.Tool{
		{Name: "clock.now", Description: "declared winner"},
		{Name: "clock_now", Description: "dropped raw collider"},
	}, nil
}
func (collisionSearchCache) Sync(context.Context, []tools.Tool) error { return nil }
func (collisionSearchCache) Close() error                             { return nil }

type fixedSearchCache struct{ results []tools.Tool }

func (c fixedSearchCache) Search(context.Context, string, []string, int) ([]tools.Tool, error) {
	return append([]tools.Tool(nil), c.results...), nil
}
func (fixedSearchCache) Sync(context.Context, []tools.Tool) error { return nil }
func (fixedSearchCache) Close() error                             { return nil }

func collisionCatalog(t *testing.T, winner, dropped *atomic.Int64) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog(tools.WithSearchCache(collisionSearchCache{}))
	for _, fixture := range []struct {
		name        string
		description string
		counter     *atomic.Int64
	}{
		{name: "clock.now", description: "declared winner", counter: winner},
		{name: "clock_now", description: "dropped raw collider", counter: dropped},
	} {
		f := fixture
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: f.name, Description: f.description},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				f.counter.Add(1)
				return tools.ToolResult{Value: f.name}, nil
			},
		}); err != nil {
			t.Fatalf("register %s: %v", f.name, err)
		}
	}
	return cat
}

func TestBuiltins_ModelAuthoredDeclaredNameNeverDispatchesRawCollider(t *testing.T) {
	var winner, dropped atomic.Int64
	cat := collisionCatalog(t, &winner, &dropped)
	ctx := declarativeTestCtx(t)

	t.Run("tool_search emits only the winner's declared name", func(t *testing.T) {
		search, err := toolSearch(ctx, cat, ToolSearchArgs{Query: "clock"})
		if err != nil {
			t.Fatalf("toolSearch: %v", err)
		}
		if search.Count != 1 || len(search.Tools) != 1 || search.Tools[0].Name != "clock_now" || search.Tools[0].Description != "declared winner" {
			t.Fatalf("toolSearch taught a vocabulary that does not match declarations: %+v", search)
		}
	})

	t.Run("tool_get resolves the declared winner", func(t *testing.T) {
		got, err := toolGet(ctx, cat, ToolGetArgs{Name: "clock_now"})
		if err != nil || !got.Found || got.Description != "declared winner" || got.Name != "clock_now" {
			t.Fatalf("toolGet(clock_now) = %+v, %v", got, err)
		}
	})

	t.Run("declarative_action dispatches the declared winner", func(t *testing.T) {
		out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{Tool: "clock_now", Args: json.RawMessage(`{}`)})
		if err != nil || !out.Dispatched {
			t.Fatalf("declarativeAction(clock_now) = %+v, %v", out, err)
		}
		if winner.Load() != 1 || dropped.Load() != 0 {
			t.Fatalf("dispatch counts winner=%d dropped=%d; model read one tool but another ran", winner.Load(), dropped.Load())
		}
	})

	t.Run("raw catalog key is not an alternate model vocabulary", func(t *testing.T) {
		raw, err := declarativeAction(ctx, cat, DeclarativeActionArgs{Tool: "clock.now", Args: json.RawMessage(`{}`)})
		if err != nil || raw.Dispatched {
			t.Fatalf("raw catalog key was accepted as a model-authored alias: %+v, %v", raw, err)
		}
	})
}

func TestDeclarativeAction_DeclaredProjection_ConcurrentReuse(t *testing.T) {
	const n = 128
	var winner, dropped atomic.Int64
	cat := collisionCatalog(t, &winner, &dropped)
	ctx := declarativeTestCtx(t)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{Tool: "clock_now", Args: json.RawMessage(`{}`)})
			if err != nil || !out.Dispatched {
				t.Errorf("declarativeAction = %+v, %v", out, err)
			}
		}()
	}
	wg.Wait()
	if winner.Load() != n || dropped.Load() != 0 {
		t.Fatalf("dispatch counts winner=%d dropped=%d, want %d/0", winner.Load(), dropped.Load(), n)
	}
}

func TestBuiltins_ReservedNameColliderIsNeverAdvertisedOrCallable(t *testing.T) {
	var invoked atomic.Int64
	collider := tools.Tool{Name: "_spawn.task", Description: "catalog collider"}
	cat := tools.NewCatalog(tools.WithSearchCache(fixedSearchCache{results: []tools.Tool{collider}}))
	if err := cat.Register(tools.ToolDescriptor{
		Tool: collider,
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			invoked.Add(1)
			return tools.ToolResult{Value: "wrong"}, nil
		},
	}); err != nil {
		t.Fatalf("register collider: %v", err)
	}
	ctx := declarativeTestCtx(t)

	search, err := toolSearch(ctx, cat, ToolSearchArgs{Query: "spawn"})
	if err != nil || search.Count != 0 || len(search.Tools) != 0 {
		t.Fatalf("tool_search advertised reserved collider: %+v, %v", search, err)
	}
	got, err := toolGet(ctx, cat, ToolGetArgs{Name: "_spawn_task"})
	if err != nil || got.Found {
		t.Fatalf("tool_get resolved reserved collider: %+v, %v", got, err)
	}
	action, err := declarativeAction(ctx, cat, DeclarativeActionArgs{Tool: "_spawn_task", Args: json.RawMessage(`{}`)})
	if err != nil || action.Dispatched {
		t.Fatalf("declarative_action dispatched reserved collider: %+v, %v", action, err)
	}
	if invoked.Load() != 0 {
		t.Fatalf("reserved collider invoked %d times, want 0", invoked.Load())
	}
}

func TestBuiltins_DeclaredProjectionEnforcesGrantedScopes(t *testing.T) {
	var invoked atomic.Int64
	scoped := tools.Tool{Name: "weather.read", Description: "scoped weather", AuthScopes: []string{"weather:read"}}
	cat := tools.NewCatalog(tools.WithSearchCache(fixedSearchCache{results: []tools.Tool{scoped}}))
	if err := cat.Register(tools.ToolDescriptor{
		Tool: scoped,
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			invoked.Add(1)
			return tools.ToolResult{Value: "sunny"}, nil
		},
	}); err != nil {
		t.Fatalf("register scoped tool: %v", err)
	}
	ctx := declarativeTestCtx(t)

	search, err := toolSearchWithScopes(ctx, cat, ToolSearchArgs{Query: "weather"}, nil)
	if err != nil || search.Count != 0 {
		t.Fatalf("unauthorized tool_search = %+v, %v", search, err)
	}
	got, err := toolGetWithScopes(ctx, cat, ToolGetArgs{Name: "weather_read"}, nil)
	if err != nil || got.Found {
		t.Fatalf("unauthorized tool_get = %+v, %v", got, err)
	}
	action, err := declarativeActionWithScopes(ctx, cat, DeclarativeActionArgs{Tool: "weather_read", Args: json.RawMessage(`{}`)}, nil)
	if err != nil || action.Dispatched || invoked.Load() != 0 {
		t.Fatalf("unauthorized declarative_action = %+v, %v; invokes=%d", action, err, invoked.Load())
	}

	granted := []string{"weather:read"}
	got, err = toolGetWithScopes(ctx, cat, ToolGetArgs{Name: "weather_read"}, granted)
	if err != nil || !got.Found {
		t.Fatalf("authorized tool_get = %+v, %v", got, err)
	}
	action, err = declarativeActionWithScopes(ctx, cat, DeclarativeActionArgs{Tool: "weather_read", Args: json.RawMessage(`{}`)}, granted)
	if err != nil || !action.Dispatched || invoked.Load() != 1 {
		t.Fatalf("authorized declarative_action = %+v, %v; invokes=%d", action, err, invoked.Load())
	}
}

func TestRegisteredToolGet_CapturesGrantedScopesImmutably(t *testing.T) {
	scoped := tools.Tool{Name: "weather.read", Description: "scoped weather", AuthScopes: []string{"weather:read"}}
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: scoped,
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: "sunny"}, nil
		},
	}); err != nil {
		t.Fatalf("register scoped tool: %v", err)
	}
	granted := []string{"weather:read"}
	if err := RegisterWith(RegistryContext{Catalog: cat, GrantedScopes: granted}, []string{"tool_get"}); err != nil {
		t.Fatalf("RegisterWith(tool_get): %v", err)
	}
	granted[0] = "mutated:after-registration"

	desc, ok := cat.Resolve("tool_get")
	if !ok {
		t.Fatal("tool_get not registered")
	}
	result, err := desc.Invoke(declarativeTestCtx(t), json.RawMessage(`{"name":"weather_read"}`))
	if err != nil {
		t.Fatalf("tool_get Invoke: %v", err)
	}
	out, ok := result.Value.(ToolGetOut)
	if !ok || !out.Found {
		t.Fatalf("tool_get result = %#v (%T), want Found=true", result.Value, result.Value)
	}
}
