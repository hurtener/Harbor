package allocation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type pricedProvider struct{ provider }

func (*pricedProvider) MonetaryTarget(_ context.Context, req llm.CompleteRequest, p llm.ModelProfile) (llm.MonetaryTarget, error) {
	return llm.MonetaryTarget{EndpointBinding: "provider_default", Provider: "openai", Model: req.Model, InputTokens: int64(p.ContextWindowTokens), OutputTokens: int64(*req.MaxTokens)}, nil
}

func TestMonetaryAllocation_ProviderEdgeChargesOnlyTrustedCapacity(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "unknown"}[unknown], func(t *testing.T) {
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			a, c := monetaryAllocation(t, 4)
			p := &pricedProvider{provider: provider{unknown: unknown}}
			name := "priced-" + string(state.NewEventID())
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return p, nil })
			mgr := allocation.New(st)
			deps := allocationDeps(t, mgr)
			deps.PricingCatalog = c
			client, err := llm.Open(t.Context(), llm.ConfigSnapshot{Driver: name, Model: "fixture-v1", ModelProfiles: map[string]llm.ModelProfile{"fixture-v1": {ContextWindowTokens: 1000}, "alias": {ContextWindowTokens: 1000}}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true}, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: name}, RunID: "task"}
			ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
			if err != nil {
				t.Fatal(err)
			}
			ctx = llm.WithInferenceAllocation(ctx, &a)
			req := llm.CompleteRequest{Model: "alias", MaxTokens: new(100), Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
			if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationPricingUnavailable) || p.calls.Load() != 0 {
				t.Fatal("alias reached transport", err, p.calls.Load())
			}
			req.Model = "fixture-v1"
			if _, err = client.Complete(ctx, req); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) || p.calls.Load() != 1 {
				t.Fatal("money cap bypassed", err, p.calls.Load())
			}
			snap, err := mgr.Snapshot(ctx, q, a)
			if err != nil {
				t.Fatal(err)
			}
			if unknown {
				if snap.UnknownCostMicroUSD != 4 || snap.ChargedCostMicroUSD != 0 {
					t.Fatal(snap)
				}
			} else if snap.ChargedCostMicroUSD != 4 || snap.ReservedCostMicroUSD != 0 {
				t.Fatal(snap)
			}
			if snap.Guarantee != "tokens_and_cost_micro_usd" || snap.PricingManifestSHA256 != a.PricingManifestSHA256 {
				t.Fatal(snap)
			}
		})
	}
}
