package summarizer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem" // driver conformance: the summariser retains clipped exchanges in a real store
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

func openInmemArtifacts(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// routedCapacity builds a routed maintenance context whose resolved model
// allowance is window tokens, so the summariser's real input bound applies.
func routedCapacity(t *testing.T, window int) (context.Context, planner.RunContext, summarizer.TrajectoryOption) {
	t.Helper()
	route := llm.ProviderRoute{RouteID: "maintenance", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "compact"}
	rc := trajRC("run-capacity")
	ctx, err := identity.WithRun(t.Context(), rc.Quadruple.Identity, rc.Quadruple.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, llm.TrustedProviderRouteContext{RuntimeID: "runtime", Route: route,
		EffectiveAgentID: "agent", TaskID: "task", Purpose: llm.ProviderRoutePurposeRun})
	resolver := trajectoryRouteResolver{selectFn: func(_ context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
		return llm.SelectedProviderRoute{Provider: "openai", Model: "compact-model", KeyName: "fixture-only-key-label",
			RouteID: req.RouteID, RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID,
			ProviderConnectionGeneration: req.ProviderConnectionGeneration, CredentialAssetGeneration: req.CredentialAssetGeneration,
			ModelSelector: req.ModelSelector, ExpiresAt: time.Now().Add(time.Minute),
			ModelProfile: &llm.ProviderModelProfile{ContextWindowTokens: window, MaxOutputTokens: 2048}}, nil
	}}
	return ctx, rc, summarizer.WithTrajectoryProviderRoute(route, llm.ProviderRouteConfig{Resolver: resolver}, .05)
}

func assertPartsBelow(t *testing.T, calls []recordedCall, threshold int) {
	t.Helper()
	for i, call := range calls {
		for j, part := range call.req.Messages[1].Content.Parts {
			if len(part.Text) >= threshold {
				t.Fatalf("call %d part %d is %d bytes, at/over the %d heavy threshold", i, j, len(part.Text), threshold)
			}
		}
	}
}

// The aggregate request is bounded by the summarizer model's allowance, not
// by the heavy-content byte cap: 60 exchanges totalling ~180 KiB fit in one
// call against a large window, each exchange in its own part.
func TestTrajectoryCapacity_ModelAllowanceBoundsWholeRequest(t *testing.T) {
	ctx, rc, routeOpt := routedCapacity(t, 1_000_000)
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, routeOpt)
	if err != nil {
		t.Fatal(err)
	}
	tr := budgetTrajectory(60)
	if _, err := s.Summarise(ctx, rc, tr); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	calls := client.seenCalls()
	if len(calls) != 1 {
		t.Fatalf("calls=%d, want one request bounded by the model allowance", len(calls))
	}
	if got := len(calls[0].req.Messages[1].Content.Parts); got != 61 {
		t.Fatalf("parts=%d, want header plus one part per step", got)
	}
	if len(userPayload(calls[0].req)) <= llm.DefaultHeavyOutputThreshold {
		t.Fatal("aggregate payload stayed under the byte cap; the test does not prove the model bound")
	}
	assertPartsBelow(t, calls, llm.DefaultHeavyOutputThreshold)
}

// A small model allowance still splits chronologically into several calls.
func TestTrajectoryCapacity_SmallAllowanceSplitsChronologically(t *testing.T) {
	ctx, rc, routeOpt := routedCapacity(t, 16_000)
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, routeOpt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Summarise(ctx, rc, budgetTrajectory(30)); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	calls := client.seenCalls()
	if len(calls) < 2 {
		t.Fatalf("calls=%d, want chronological split", len(calls))
	}
	for i, call := range calls {
		if llm.EstimateRequestTokens(call.req, llm.ModelProfile{ContextWindowTokens: 16_000}) >= 16_000-2048 {
			t.Fatalf("call %d exceeds the model input allowance", i)
		}
	}
}

// The incident shape: one exchange between the offload threshold and the
// heavy ceiling (and one far above it) is clipped to a deterministic preview
// plus a retained reference instead of failing maintenance.
func TestTrajectoryCapacity_OversizedExchangeClipsWithRetainedReference(t *testing.T) {
	ctx, rc, routeOpt := routedCapacity(t, 1_000_000)
	store := openInmemArtifacts(t)
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, routeOpt, summarizer.WithTrajectoryArtifactStore(store))
	if err != nil {
		t.Fatal(err)
	}
	tr := trajFixture()
	tr.Steps = []planner.Step{
		{Action: planner.CallTool{Tool: "web_search"}, LLMObservation: strings.Repeat("a", 126*1024) + "TAIL-A"},
		{Action: planner.CallTool{Tool: "knowledge_read"}, LLMObservation: strings.Repeat("b", 700*1024) + "TAIL-B"},
		{Action: planner.CallTool{Tool: "small"}, LLMObservation: "fine"},
	}
	first, err := s.Summarise(ctx, rc, tr)
	if err != nil || first == nil {
		t.Fatalf("oversized exchange failed maintenance: %v", err)
	}
	calls := client.seenCalls()
	assertPartsBelow(t, calls, llm.DefaultHeavyOutputThreshold)
	payload := userPayload(calls[0].req)
	if strings.Count(payload, "clipped for maintenance") != 2 || !strings.Contains(payload, "Step 3 action:") {
		t.Fatalf("expected two clipped exchanges and the small one verbatim")
	}
	if strings.Contains(payload, "TAIL-A") || strings.Contains(payload, "TAIL-B") {
		t.Fatal("clipped preview carried the tail; the request was not bounded")
	}
	refs, err := store.List(ctx, artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	if err != nil || len(refs) != 2 {
		t.Fatalf("retained refs=%d err=%v, want two", len(refs), err)
	}
	for _, ref := range refs {
		if !strings.Contains(payload, ref.ID) || ref.Source["source"] != "tool" || ref.Source["tool"] == "" {
			t.Fatalf("retained ref %+v is not referenced or lacks tool provenance", ref)
		}
		body, found, err := store.Get(ctx, artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "s1"}, ref.ID)
		if err != nil || !found || (!strings.Contains(string(body), "TAIL-A") && !strings.Contains(string(body), "TAIL-B")) {
			t.Fatal("retained evidence is not the exact rendered exchange")
		}
	}
	// Determinism: a second attempt renders byte-identical parts and reuses
	// the same content-addressed references.
	if _, err := s.Summarise(ctx, rc, tr); err != nil {
		t.Fatal(err)
	}
	again := client.seenCalls()
	if userPayload(again[len(again)-1].req) != payload {
		t.Fatal("clipping is not deterministic")
	}
	if refs2, _ := store.List(ctx, artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "s1"}); len(refs2) != 2 {
		t.Fatal("repeated maintenance duplicated retained evidence")
	}
}

// Without a store the preview states that the remainder was not retained;
// maintenance still succeeds.
func TestTrajectoryCapacity_OversizedExchangeWithoutStoreStillSummarises(t *testing.T) {
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryHeavyOutputThreshold(16384))
	if err != nil {
		t.Fatal(err)
	}
	tr := trajFixture()
	tr.Steps[0].LLMObservation = strings.Repeat("x", 40*1024)
	got, err := s.Summarise(context.Background(), trajRC("no-store"), tr)
	if err != nil || got == nil {
		t.Fatalf("summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if !strings.Contains(payload, "was not retained") || !strings.Contains(payload, "Step 2 action:") {
		t.Fatal("missing explicit non-retention marker or later exchange")
	}
	assertPartsBelow(t, client.seenCalls(), 16384)
}

// An exchange whose preview cannot fit even an otherwise empty request falls
// back to the minimal preview rather than failing.
func TestTrajectoryCapacity_TightAllowanceUsesMinimalPreview(t *testing.T) {
	ctx, rc, routeOpt := routedCapacity(t, 4_300)
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, routeOpt)
	if err != nil {
		t.Fatal(err)
	}
	tr := trajFixture()
	tr.Steps[0].LLMObservation = strings.Repeat("m", 200*1024)
	if _, err := s.Summarise(ctx, rc, tr); err != nil {
		t.Fatalf("summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if !strings.Contains(payload, "showing the first 512 bytes") {
		t.Fatal("expected the minimal preview when the full preview cannot fit")
	}
}
