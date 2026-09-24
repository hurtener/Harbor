package projection_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func TestActiveMemoryBudget(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	budget, err := projection.ActiveMemoryBudget(ctx, reg, projAgent, projID(), 64000, true)
	if err != nil || budget != 64000 {
		t.Fatalf("YAML inheritance: %d %v", budget, err)
	}
	for _, override := range []int{0, 32000, -1} {
		_, err = reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Memory: &agentcfg.MemorySection{BudgetTokens: override}}, agentcfg.SetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := projection.ActiveMemoryBudget(ctx, reg, projAgent, projID(), 64000, true)
		if override < 0 {
			if err == nil {
				t.Fatal("invalid stored value accepted")
			}
			continue
		}
		if err != nil || got != override {
			t.Fatalf("override %d: %d %v", override, got, err)
		}
		if _, err := projection.ActiveMemoryBudget(ctx, reg, projAgent, projID(), 64000, false); err == nil {
			t.Fatal("unwired compactor accepted override")
		}
	}
	if budget != 64000 {
		t.Fatal("later revision changed frozen run value")
	}
}

func TestActiveMemoryBudgetConcurrentIsolation(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id := projID()
			id.TenantID = fmt.Sprintf("memory-tenant-%d", i)
			want := i + 1
			_, err := reg.SetRevision(ctx, id, projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Memory: &agentcfg.MemorySection{BudgetTokens: want}}, agentcfg.SetOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			got, err := projection.ActiveMemoryBudget(ctx, reg, projAgent, id, 64000, true)
			if err != nil || got != want {
				t.Errorf("tenant %d: %d %v", i, got, err)
			}
		})
	}
	wg.Wait()
}
