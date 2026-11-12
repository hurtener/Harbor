package projection_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func int64p(v int64) *int64 { return &v }

func TestActiveRunLimits(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	runtimeDefault := planner.RunLimits{MaxDuration: time.Hour, MaxTokens: 9_000_000}
	got, err := projection.ActiveRunLimits(ctx, reg, projAgent, projID(), runtimeDefault)
	if err != nil || got != runtimeDefault {
		t.Fatalf("absent section must inherit the runtime default: %+v %v", got, err)
	}
	cases := []struct {
		section agentcfg.RunLimitsSection
		want    planner.RunLimits
	}{
		{agentcfg.RunLimitsSection{MaxDurationSeconds: int64p(1200)}, planner.RunLimits{MaxDuration: 20 * time.Minute}},
		{agentcfg.RunLimitsSection{MaxTokens: int64p(500_000)}, planner.RunLimits{MaxTokens: 500_000}},
		{agentcfg.RunLimitsSection{MaxDurationSeconds: int64p(60), MaxTokens: int64p(10)}, planner.RunLimits{MaxDuration: time.Minute, MaxTokens: 10}},
		{agentcfg.RunLimitsSection{}, planner.RunLimits{}}, // present but empty: explicitly unlimited
	}
	for _, c := range cases {
		section := c.section
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{RunLimits: &section}, agentcfg.SetOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := projection.ActiveRunLimits(ctx, reg, projAgent, projID(), runtimeDefault)
		if err != nil || got != c.want {
			t.Fatalf("section %+v: got %+v %v, want %+v", c.section, got, err, c.want)
		}
	}
}

func TestActiveRunLimitsConcurrentIsolation(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id := projID()
			id.TenantID = fmt.Sprintf("limits-tenant-%d", i)
			want := int64(i + 1)
			if _, err := reg.SetRevision(ctx, id, projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{RunLimits: &agentcfg.RunLimitsSection{MaxTokens: &want}}, agentcfg.SetOptions{}); err != nil {
				t.Error(err)
				return
			}
			got, err := projection.ActiveRunLimits(ctx, reg, projAgent, id, planner.RunLimits{})
			if err != nil || got.MaxTokens != want {
				t.Errorf("tenant %d: %+v %v", i, got, err)
			}
		})
	}
	wg.Wait()
}
