package durable_test

import (
	"context"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestMonetaryAllocation_DurableAcceptedReference(t *testing.T) {
	st := newStore(t)
	defer func() { _ = st.Close(context.Background()) }()
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	tariff := pricing.Tariff{EndpointBinding: "provider_default", Provider: "openai", Model: "fixture-v1", ModelVersion: "fixture-v1", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(0)), CacheWriteMicroUSDPerMillion: new(int64(0)), ReasoningMicroUSDPerMillion: new(int64(0)), RequestMicroUSD: new(int64(0)), AncillaryMicroUSD: new(int64(0))}
	catalog, err := pricing.New([]pricing.Manifest{{ID: "durable-fixture", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{tariff}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := catalog.References()[0]
	deps := tasks.Dependencies{Store: st, Bus: bus, Redactor: auditpatterns.New(), PricingCatalog: catalog}
	reg, err := tasks.OpenDriver("durable", deps)
	if err != nil {
		t.Fatal(err)
	}
	a := &llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000, MaxCostMicroUSD: new(int64(100)), PricingManifestID: ref.ID, PricingManifestRevision: ref.Revision, PricingManifestSHA256: ref.SHA256}
	q := quadA()
	ctx := ctxFor(t, q.Identity)
	req := tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, Query: "work", IdempotencyKey: "funded", InferenceAllocation: a}
	root, err := reg.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg, err = tasks.OpenDriver("durable", deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(context.Background()) }()
	got, err := reg.Get(ctx, root.ID)
	if err != nil || !llm.EqualInferenceAllocation(got.InferenceAllocation, a) {
		t.Fatal(got, err)
	}
	again, err := reg.Spawn(ctx, req)
	if err != nil || again.ID != root.ID {
		t.Fatal("restart changed acceptance", again, err)
	}
	child, err := reg.Spawn(llm.WithInferenceAllocation(ctx, got.InferenceAllocation), tasks.SpawnRequest{Identity: q, Kind: tasks.KindBackground, Query: "helper", ParentTaskID: &root.ID})
	if err != nil {
		t.Fatal(err)
	}
	cg, err := reg.Get(ctx, child.ID)
	if err != nil || cg.AllocationTaskID != string(root.ID) || !llm.EqualInferenceAllocation(cg.InferenceAllocation, a) {
		t.Fatal(cg, err)
	}
}
