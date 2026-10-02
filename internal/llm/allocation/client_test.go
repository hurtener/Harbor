package allocation_test

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	artifactmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type provider struct {
	calls   atomic.Int64
	unknown bool
	failure error
}

func (p *provider) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	p.calls.Add(1)
	return llm.CompleteResponse{Content: "ok", Usage: llm.Usage{ReportPresent: !p.unknown, PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}}, p.failure
}
func (*provider) Close(context.Context) error { return nil }
func (*provider) ProviderAttemptBound(context.Context, llm.CompleteRequest) (int, error) {
	return 1, nil
}
func TestAllocation_ProviderEdgeRefusesBeforeTransportAndRetainsUnknown(t *testing.T) {
	st, _ := inmem.New(config.StateConfig{})
	defer func() { _ = st.Close(context.Background()) }()
	for _, unknown := range []bool{false, true} {
		p := &provider{unknown: unknown}
		name := "allocation-" + string(state.NewEventID())
		llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return p, nil })
		client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "model", ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 1000}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, allocationDeps(t, allocation.New(st)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := identity.WithRun(t.Context(), identity.Identity{TenantID: "t", UserID: "u", SessionID: name}, "task")
		if err != nil {
			t.Fatal(err)
		}
		a := &llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 1107}
		ctx = llm.WithInferenceAllocation(ctx, a)
		max := 100
		req := llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
		if _, err = client.Complete(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err = client.Complete(ctx, req)
		if unknown {
			if !errors.Is(err, llm.ErrAllocationExhausted) || p.calls.Load() != 1 {
				t.Fatalf("unknown err=%v calls=%d", err, p.calls.Load())
			}
		} else if err != nil || p.calls.Load() != 2 {
			t.Fatalf("known err=%v calls=%d", err, p.calls.Load())
		}
		if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) {
			t.Fatalf("exhausted: %v", err)
		}
		_ = client.Close(context.Background())
	}
}

func TestAllocation_MonetaryAndUnboundedRequestsRefused(t *testing.T) {
	money := int64(1)
	if err := llm.ValidateInferenceAllocation(&llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 100, MaxCostMicroUSD: &money}); !errors.Is(err, llm.ErrAllocationPricingUnavailable) {
		t.Fatalf("cost %v", err)
	}

	for _, a := range []llm.InferenceAllocation{{}, {AllocationID: "a", MaxTotalTokens: 1}, {AllocationID: "a", Revision: 1, MaxTotalTokens: -1}} {
		if err := llm.ValidateInferenceAllocation(&a); !errors.Is(err, llm.ErrAllocationInvalid) {
			t.Fatalf("invalid %+v: %v", a, err)
		}
	}
}

func TestAllocation_PartialUsageOnCancelledStreamStaysHeld(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	p := &provider{failure: context.Canceled}
	name := "allocation-partial-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return p, nil })
	client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "model", ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 1000}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, allocationDeps(t, mgr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	ctx, _ := identity.WithRun(t.Context(), q.Identity, q.RunID)
	a := llm.InferenceAllocation{AllocationID: "a", Revision: 1, MaxTotalTokens: 1100}
	ctx = llm.WithInferenceAllocation(ctx, &a)
	max := 100
	req := llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = client.Complete(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("call %v", err)
	}
	snap, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || snap.SettledTokens != 0 || snap.UnknownTokens != 1100 {
		t.Fatalf("partial refund %+v %v", snap, err)
	}
}

func allocationDeps(t *testing.T, mgr llm.AllocationStore) llm.Deps {
	t.Helper()
	art, err := artifactmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventmem.New(config.EventsConfig{MaxSubscribersPerSession: 16, SubscriberBufferSize: 256, IdleTimeout: time.Minute, DropWindow: time.Second}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()); _ = art.Close(context.Background()) })
	return llm.Deps{Allocations: mgr, Artifacts: art, Bus: bus}
}

type overflowingUsageProvider struct{ calls atomic.Int64 }

func (p *overflowingUsageProvider) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	p.calls.Add(1)
	return llm.CompleteResponse{Content: "untrusted usage", Usage: llm.Usage{ReportPresent: true, PromptTokens: int(^uint(0) >> 1), CompletionTokens: 100, TotalTokens: 1}}, nil
}
func (*overflowingUsageProvider) Close(context.Context) error { return nil }
func (*overflowingUsageProvider) ProviderAttemptBound(context.Context, llm.CompleteRequest) (int, error) {
	return 1, nil
}

func TestAllocation_UsageOverflowRetainsLiabilityAndLatchesBreach(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("64-bit usage overflow fixture; 32-bit provider counters cannot exceed the accounting range")
	}
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	p := &overflowingUsageProvider{}
	name := "overflow-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return p, nil })
	mgr := allocation.New(st)
	client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "model", ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 1000}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, allocationDeps(t, mgr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "overflow", UserID: "u", SessionID: "s"}, RunID: "task"}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	a := llm.InferenceAllocation{AllocationID: "overflow", Revision: 1, MaxTotalTokens: 10000}
	ctx = llm.WithInferenceAllocation(ctx, &a)
	maxTokens := 100
	req := llm.CompleteRequest{Model: "model", MaxTokens: &maxTokens, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationBoundViolated) {
		t.Fatalf("overflow result=%v", err)
	}
	got, err := mgr.Snapshot(ctx, q, a)
	if err != nil || !got.BoundBreached || got.SettledTokens != 0 || got.ReservedTokens != 1100 || got.UnknownTokens != 1100 {
		t.Fatalf("overflow accounting=%+v err=%v", got, err)
	}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) || p.calls.Load() != 1 {
		t.Fatalf("latched retry=%v calls=%d", err, p.calls.Load())
	}
	if err = mgr.ReportBoundViolation(ctx, q, a, got.Receipts[0].AttemptID); err != nil {
		t.Fatalf("exact breach replay=%v", err)
	}
}
