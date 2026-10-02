package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	taskprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

func TestAllocation_TaskProjectionIsScopedAndContentFree(t *testing.T) {
	_, reg, _ := newListService(t)
	id := idFor("tenant", "owner", "session")
	ctx, _ := identity.With(t.Context(), id)
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	a := llm.InferenceAllocation{AllocationID: "opaque-funding", Revision: 1, MaxTotalTokens: 100}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "private query", InferenceAllocation: &a})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: id, RunID: string(h.ID)}
	if err = mgr.Reserve(ctx, q, a, "attempt", 40); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Settle(ctx, q, a, "attempt", nil, false); err != nil {
		t.Fatal(err)
	}
	p, err := taskprotocol.NewRegistryProjector(reg, taskprotocol.WithAllocations(mgr))
	if err != nil {
		t.Fatal(err)
	}
	detail, err := p.GetTask(ctx, id, string(h.ID))
	if err != nil {
		t.Fatal(err)
	}
	got := detail.InferenceAllocation
	if got == nil || got.ReservedTokens != 40 || got.UnknownTokens != 40 || got.Guarantee != "tokens" || got.PricingStatus != "unavailable" || len(got.Receipts) != 1 {
		t.Fatalf("snapshot %+v", got)
	}
	if err = mgr.Close(ctx, q, a); err != nil {
		t.Fatal(err)
	}
	detail, err = p.GetTask(ctx, id, string(h.ID))
	if err != nil || !detail.InferenceAllocation.Closed || detail.InferenceAllocation.UnknownTokens != 40 {
		t.Fatalf("closed projection: %+v %v", detail, err)
	}
	foreign := id
	foreign.UserID = "foreign"
	if _, err = p.GetTask(ctx, foreign, string(h.ID)); !errors.Is(err, taskprotocol.ErrTaskNotFound) {
		t.Fatalf("foreign access %v", err)
	}
}
