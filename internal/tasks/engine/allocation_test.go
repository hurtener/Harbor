package engine_test

import (
	"context"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestEngine_AllocationImmutableAndInherited(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	e, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(t.Context(), id)
	a := &llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000}
	req := tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "work", IdempotencyKey: "same", InferenceAllocation: a}
	root, err := e.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	a.MaxTotalTokens = 2000
	if _, err = e.Spawn(ctx, req); !errors.Is(err, tasks.ErrIdempotencyConflict) {
		t.Fatalf("mutated retry: %v", err)
	}
	childReq := tasks.SpawnRequest{Identity: req.Identity, Kind: tasks.KindBackground, Query: "helper", ParentTaskID: &root.ID}
	child, err := e.Spawn(ctx, childReq)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := e.Get(ctx, child.ID)
	if err != nil || ct.InferenceAllocation == nil || ct.InferenceAllocation.MaxTotalTokens != 1000 || ct.AllocationTaskID != string(root.ID) {
		t.Fatalf("child %+v %v", ct, err)
	}
	childReq.ParentTaskID = &child.ID
	grand, err := e.Spawn(ctx, childReq)
	if err != nil {
		t.Fatal(err)
	}
	gt, _ := e.Get(ctx, grand.ID)
	if gt.AllocationTaskID != string(root.ID) {
		t.Fatalf("grandchild root %s", gt.AllocationTaskID)
	}
	childReq.InferenceAllocation = a
	if _, err = e.Spawn(ctx, childReq); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatalf("child replacement %v", err)
	}
}

func TestEngine_MonetaryAllocationRequiresOperatorCatalogAndInherits(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	tariff := pricing.Tariff{EndpointBinding: "provider_default", Provider: "openai", Model: "fixture-v1", ModelVersion: "fixture-v1", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(0)), CacheWriteMicroUSDPerMillion: new(int64(0)), ReasoningMicroUSDPerMillion: new(int64(0)), RequestMicroUSD: new(int64(0)), AncillaryMicroUSD: new(int64(0))}
	catalog, err := pricing.New([]pricing.Manifest{{ID: "operator-fixture", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{tariff}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := catalog.References()[0]
	a := &llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000, MaxCostMicroUSD: new(int64(100)), PricingManifestID: ref.ID, PricingManifestRevision: ref.Revision, PricingManifestSHA256: ref.SHA256}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(t.Context(), id)
	req := tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "work", IdempotencyKey: "root", InferenceAllocation: a}
	unconfigured, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unconfigured.Close(context.Background()) }()
	if _, err = unconfigured.Spawn(ctx, req); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatal("caller-provided reference conferred authority", err)
	}
	e, err := engine.New(bus, auditpatterns.New(), &memBackend{}, engine.WithPricingCatalog(catalog))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	root, err := e.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.InferenceAllocation = llm.CloneInferenceAllocation(a)
	replay, err := e.Spawn(ctx, req)
	if err != nil || replay.ID != root.ID {
		t.Fatal("equal cost pointer replay", replay, err)
	}
	childReq := tasks.SpawnRequest{Identity: req.Identity, Kind: tasks.KindBackground, Query: "child", ParentTaskID: &root.ID, InferenceAllocation: llm.CloneInferenceAllocation(a)}
	childCtx := llm.WithInferenceAllocation(ctx, a)
	child, err := e.Spawn(childCtx, childReq)
	if err != nil {
		t.Fatal("deep-value parent comparison", err)
	}
	got, err := e.Get(ctx, child.ID)
	if err != nil || !llm.EqualInferenceAllocation(got.InferenceAllocation, a) || got.AllocationTaskID != string(root.ID) {
		t.Fatal(got, err)
	}
	*got.InferenceAllocation.MaxCostMicroUSD = 999
	persisted, err := e.Get(ctx, child.ID)
	if err != nil || *persisted.InferenceAllocation.MaxCostMicroUSD != 100 {
		t.Fatal("caller mutated cap", persisted, err)
	}
	childReq.InferenceAllocation = got.InferenceAllocation
	if _, err = e.Spawn(ctx, childReq); !errors.Is(err, llm.ErrAllocationInvalid) {
		t.Fatal("child changed money", err)
	}
	req.InferenceAllocation = llm.CloneInferenceAllocation(a)
	*req.InferenceAllocation.MaxCostMicroUSD = 101
	if _, err = e.Spawn(ctx, req); !errors.Is(err, tasks.ErrIdempotencyConflict) {
		t.Fatal("changed accepted money", err)
	}
}
