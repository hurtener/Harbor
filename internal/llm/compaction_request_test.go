package llm

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestCompactionRequest_BoundsAndIsolation(t *testing.T) {
	t.Parallel()
	parentOutput, configuredOutput := 800, 1200
	cfg := ConfigSnapshot{ContextWindowReserve: .05, ModelProfiles: map[string]ModelProfile{
		"large": {ContextWindowTokens: 8192, DefaultMaxTokens: &configuredOutput},
		"small": {ContextWindowTokens: 4096},
	}}
	parent := CompleteRequest{Model: "large", MaxTokens: &parentOutput, ReasoningEffort: ReasoningLow, ReasoningEffortExplicit: true}
	ctx := withCompactionRequest(t.Context(), parent, cfg)
	for _, model := range []string{"", "large", "small"} {
		output := 2048
		req, bounds, err := PrepareCompactionRequest(ctx, CompleteRequest{Model: model, MaxTokens: &output})
		if err != nil {
			t.Fatal(err)
		}
		wantModel, wantOutput, wantLimit := "large", 800, 6982
		if model == "small" {
			wantModel, wantOutput, wantLimit = "small", 2048, 1843
		}
		if req.Model != wantModel || *req.MaxTokens != wantOutput || bounds.InputLimit != wantLimit {
			t.Fatalf("model/output/limit = %s/%d/%d", req.Model, *req.MaxTokens, bounds.InputLimit)
		}
		if model != "small" && (req.ReasoningEffort != ReasoningLow || !req.ReasoningEffortExplicit) {
			t.Fatal("lost explicit reasoning")
		}
		*req.MaxTokens = 1
		if bounds.Profile.DefaultMaxTokens != nil {
			*bounds.Profile.DefaultMaxTokens = 2
		}
	}
	if parentOutput != 800 || configuredOutput != 1200 {
		t.Fatal("mutated parent controls or shared profile")
	}
	_, _, err := PrepareCompactionRequest(ctx, CompleteRequest{Model: "unknown"})
	if !errors.Is(err, ErrUnsupportedModel) {
		t.Fatalf("unknown model: %v", err)
	}
}

func TestCompactionRequest_DynamicProfileAndBoundModel(t *testing.T) {
	t.Parallel()
	maxOutput := 600
	parent := withTrustedModelProfile(CompleteRequest{Model: "dynamic"}, "dynamic", ModelProfile{ContextWindowTokens: 4096, DefaultMaxTokens: &maxOutput})
	parent.ExternalGrant = &ExternalGrant{} // Only a binding flag here; this helper never verifies or grants authority.
	ctx := withCompactionRequest(t.Context(), parent, ConfigSnapshot{ContextWindowReserve: .05})
	output := 2048
	req, bounds, err := PrepareCompactionRequest(ctx, CompleteRequest{MaxTokens: &output})
	if err != nil || req.Model != "dynamic" || *req.MaxTokens != 600 || bounds.InputLimit != 3291 {
		t.Fatalf("dynamic profile: %+v %+v %v", req, bounds, err)
	}
	if _, _, err := PrepareCompactionRequest(ctx, CompleteRequest{Model: "other", MaxTokens: &output}); !errors.Is(err, ErrProviderRouteInvalid) {
		t.Fatalf("bound model override: %v", err)
	}
	// Metadata copies cannot mutate the original private technical descriptor.
	*bounds.Profile.DefaultMaxTokens = 17
	if *parent.modelProfile.DefaultMaxTokens != 600 {
		t.Fatal("dynamic descriptor aliased")
	}
}

func TestCompactionRequest_InvalidCapacityAndStandalone(t *testing.T) {
	t.Parallel()
	for _, output := range []int{-1, 0, 950, 1000} {
		ctx := withCompactionRequest(t.Context(), CompleteRequest{Model: "m"}, ConfigSnapshot{ContextWindowReserve: .05, ModelProfiles: map[string]ModelProfile{"m": {ContextWindowTokens: 1000}}})
		_, _, err := PrepareCompactionRequest(ctx, CompleteRequest{MaxTokens: &output})
		if err == nil {
			t.Errorf("invalid output capacity accepted: %d", output)
		}
	}
	output := 700
	req := CompleteRequest{Model: "custom", MaxTokens: &output}
	got, bounds, err := PrepareCompactionRequest(context.Background(), req)
	if err != nil || bounds.InputLimit != 0 || !reflect.DeepEqual(req, got) {
		t.Fatal("standalone caller changed")
	}
}

func TestCompactionRequest_ConcurrentPerRunBounds(t *testing.T) {
	t.Parallel()
	cfg := ConfigSnapshot{ContextWindowReserve: .05, ModelProfiles: map[string]ModelProfile{"m": {ContextWindowTokens: 4096}}}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parentLimit := 100 + i
			ctx := withCompactionRequest(t.Context(), CompleteRequest{Model: "m", MaxTokens: &parentLimit}, cfg)
			output := 2048
			req, budget, err := PrepareCompactionRequest(ctx, CompleteRequest{MaxTokens: &output})
			if err != nil || *req.MaxTokens != parentLimit || budget.InputLimit != 3891-parentLimit {
				t.Errorf("scope %d: output/capacity %v/%v/%v", i, req.MaxTokens, budget, err)
			}
		}()
	}
	wg.Wait()
}
