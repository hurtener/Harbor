package serve

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	artifactmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

type allocatedProvider struct{}

func (*allocatedProvider) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "done", Name: "_finish", Args: json.RawMessage(`{"answer":"ok"}`)}}, Usage: llm.Usage{ReportPresent: true, PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}}, nil
}
func (*allocatedProvider) Close(context.Context) error { return nil }
func (*allocatedProvider) ProviderAttemptBound(context.Context, llm.CompleteRequest) (int, error) {
	return 1, nil
}
func TestAllocation_AcceptedTaskReachesDurableProviderAccounting(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	reg, err := tasks.Open(t.Context(), tasks.Dependencies{Store: st, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(context.Background()) }()
	mgr := allocation.New(st)
	art, err := artifactmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = art.Close(context.Background()) }()
	name := "allocation-runtime-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return &allocatedProvider{}, nil })
	max := 100
	client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "model", ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 4096, DefaultMaxTokens: &max}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, llm.Deps{Allocations: mgr, Artifacts: art, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New(pauseresume.WithBus(bus)), steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{SessionMemory: config.MemoryConfig{Strategy: "none"}, Bus: bus, RunLoop: rl, Planner: react.New(client), Tasks: reg})
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.WithVerified(t.Context(), id)
	a := llm.InferenceAllocation{AllocationID: "funded", Revision: 1, MaxTotalTokens: 4196}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "test", InferenceAllocation: &a})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := reg.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == tasks.StatusComplete {
			got, err := mgr.Snapshot(ctx, identity.Quadruple{Identity: id, RunID: string(h.ID)}, a)
			if err != nil || !got.Closed || got.SettledTokens != 7 || got.AttemptCount != 1 {
				t.Fatalf("accounting %+v %v", got, err)
			}
			lateCtx := llm.WithInferenceAllocationTask(ctx, &a, string(h.ID))
			if _, err = client.Complete(lateCtx, llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("late helper")}}}}); !errors.Is(err, llm.ErrAllocationClosed) {
				t.Fatalf("late helper escaped finality: %v", err)
			}
			return
		}
		if task.Status == tasks.StatusFailed {
			t.Fatalf("task failed %+v", task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish")
}

func (*allocatedProvider) MonetaryTarget(_ context.Context, req llm.CompleteRequest, p llm.ModelProfile) (llm.MonetaryTarget, error) {
	return llm.MonetaryTarget{EndpointBinding: "provider_default", Provider: "openai", Model: req.Model, InputTokens: int64(p.ContextWindowTokens), OutputTokens: int64(*req.MaxTokens)}, nil
}

func TestMonetaryAllocation_AcceptedTaskReachesDurableProviderAccounting(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	tariff := pricing.Tariff{EndpointBinding: "provider_default", Provider: "openai", Model: "model", ModelVersion: "model", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(1)), CacheReadMicroUSDPerMillion: new(int64(0)), CacheWriteMicroUSDPerMillion: new(int64(0)), ReasoningMicroUSDPerMillion: new(int64(0)), RequestMicroUSD: new(int64(1)), AncillaryMicroUSD: new(int64(1))}
	catalog, err := pricing.New([]pricing.Manifest{{ID: "runtime-fixture", Revision: 1, Currency: "USD", Tariffs: []pricing.Tariff{tariff}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := catalog.References()[0]
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	reg, err := tasks.Open(t.Context(), tasks.Dependencies{Store: st, Bus: bus, Redactor: red, PricingCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(context.Background()) }()
	mgr := allocation.New(st)
	art, err := artifactmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = art.Close(context.Background()) }()
	name := "allocation-runtime-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return &allocatedProvider{}, nil })
	max := 100
	client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "model", ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 4096, DefaultMaxTokens: &max}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, llm.Deps{Allocations: mgr, Artifacts: art, Bus: bus, PricingCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New(pauseresume.WithBus(bus)), steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{SessionMemory: config.MemoryConfig{Strategy: "none"}, Bus: bus, RunLoop: rl, Planner: react.New(client), Tasks: reg})
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.WithVerified(t.Context(), id)
	a := llm.InferenceAllocation{AllocationID: "funded", Revision: 1, MaxTotalTokens: 4196, MaxCostMicroUSD: new(int64(4)), PricingManifestID: ref.ID, PricingManifestRevision: ref.Revision, PricingManifestSHA256: ref.SHA256}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "test", InferenceAllocation: &a})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := reg.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == tasks.StatusComplete {
			got, err := mgr.Snapshot(ctx, identity.Quadruple{Identity: id, RunID: string(h.ID)}, a)
			if err != nil || !got.Closed || got.SettledTokens != 7 || got.AttemptCount != 1 || got.ChargedCostMicroUSD != 4 || got.Guarantee != "tokens_and_cost_micro_usd" {
				t.Fatalf("accounting %+v %v", got, err)
			}
			lateCtx := llm.WithInferenceAllocationTask(ctx, &a, string(h.ID))
			if _, err = client.Complete(lateCtx, llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("late helper")}}}}); !errors.Is(err, llm.ErrAllocationClosed) {
				t.Fatalf("late helper escaped finality: %v", err)
			}
			return
		}
		if task.Status == tasks.StatusFailed {
			t.Fatalf("task failed %+v", task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish")
}
