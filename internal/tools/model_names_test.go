package tools_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestModelToolNameProjection_CollisionWinnerIsOnlyReachableTool(t *testing.T) {
	p := tools.NewModelToolNameProjection([]string{"clock.now", "clock_now"}, nil)
	if got, ok := p.ResolveDeclared("clock_now"); !ok || got != "clock.now" {
		t.Fatalf("ResolveDeclared(clock_now) = %q, %v; want clock.now, true", got, ok)
	}
	if _, ok := p.DeclaredName("clock_now"); ok {
		t.Fatal("dropped raw collider remained model-visible")
	}
	collisions := p.Collisions()
	if len(collisions) != 1 || collisions[0].DeclaredTool != "clock.now" || collisions[0].DroppedTool != "clock_now" {
		t.Fatalf("collisions = %+v, want clock.now winner and clock_now dropped", collisions)
	}
	if _, ok := p.ResolveDeclared("clock.now"); ok {
		t.Fatal("model-authored raw catalog key was accepted as an alternate vocabulary")
	}
}

func TestModelToolNameProjection_ReservedNameCannotResolveCatalogCollider(t *testing.T) {
	p := tools.NewModelToolNameProjection([]string{"_spawn.task"}, []string{"_spawn_task"})
	if _, ok := p.ResolveDeclared("_spawn_task"); ok {
		t.Fatal("reserved declaration resolved to a dropped catalog collider")
	}
	if got := p.Collisions(); len(got) != 1 || got[0].DeclaredTool != "_spawn_task" || got[0].DroppedTool != "_spawn.task" {
		t.Fatalf("reserved collision = %+v", got)
	}
}

func TestModelToolNameProjection_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const n = 128
	names := make([]string, 0, n)
	for i := range n {
		names = append(names, fmt.Sprintf("source.with.a.long.shared.prefix.%03d_fetch_record", i))
	}
	p := tools.NewModelToolNameProjection(names, nil)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			declaration, ok := p.DeclaredName(names[i])
			if !ok {
				t.Errorf("%d: catalog name dropped", i)
			} else if got, found := p.ResolveDeclared(declaration); !found || got != names[i] {
				t.Errorf("%d: round trip = %q, %v; want %q", i, got, found, names[i])
			}
			wg.Done()
		}()
	}
	wg.Wait()
}
