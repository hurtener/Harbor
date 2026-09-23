package summarizer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
)

type trajectoryRouteResolver struct {
	selectFn func(context.Context, llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error)
}

func (r trajectoryRouteResolver) SelectProviderRoute(ctx context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	return r.selectFn(ctx, req)
}
func (trajectoryRouteResolver) ResolveProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	panic("summary preparation cannot fetch credentials")
}

func TestTrajectorySummariser_IndependentRouteRejectsInvalidConstruction(t *testing.T) {
	for _, mode := range []string{"empty", "incomplete", "missing-resolver", "missing-runtime", "ambiguous-model"} {
		t.Run(mode, func(t *testing.T) {
			route := llm.ProviderRoute{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection",
				ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "compact"}
			cfg := llm.ProviderRouteConfig{Resolver: trajectoryRouteResolver{}, RuntimeID: "runtime"}
			var options []summarizer.TrajectoryOption
			switch mode {
			case "empty":
				route = llm.ProviderRoute{}
			case "incomplete":
				route.RouteGeneration = 0
			case "missing-resolver":
				cfg.Resolver = nil
			case "missing-runtime":
				cfg.RuntimeID = ""
			case "ambiguous-model":
				options = append(options, summarizer.WithTrajectoryModel("other"))
			}
			options = append(options, summarizer.WithTrajectoryProviderRoute(route, cfg, .05))
			if _, err := summarizer.NewTrajectorySummariser(&stubClient{}, options...); err == nil {
				t.Fatal("invalid route configuration accepted")
			}
		})
	}
}

func TestTrajectorySummariser_IndependentRoutePacksActualRequestsAndPreservesParent(t *testing.T) {
	maintenance := llm.ProviderRoute{RouteID: "maintenance", RouteGeneration: 1, ProviderConnectionID: "connection",
		ProviderConnectionGeneration: 2, CredentialAssetGeneration: 3, ModelSelector: "compact-alias"}
	parent := llm.TrustedProviderRouteContext{Route: maintenance, RuntimeID: "runtime", EffectiveAgentID: "agent", TaskID: "task", Purpose: llm.ProviderRoutePurposeRun}
	parent.Route.RouteID, parent.Route.ModelSelector = "driving", "large-alias"
	rc := trajRC("run")
	ctx, err := identity.WithRun(t.Context(), rc.Quadruple.Identity, rc.Quadruple.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, parent)
	selections := make(map[string]bool)
	resolver := trajectoryRouteResolver{selectFn: func(_ context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
		if req.TenantID != "t1" || req.UserID != "u1" || req.SessionID != "s1" || req.LogicalRunID != "run" ||
			req.EffectiveAgentID != "agent" || req.RuntimeID != "runtime" || req.TaskID != "task" || req.RouteID != "maintenance" {
			t.Fatalf("changed admitted identity or route: %+v", req)
		}
		if req.LogicalCallID == "" || selections[req.LogicalCallID] {
			t.Fatal("missing or reused maintenance invocation")
		}
		selections[req.LogicalCallID] = true
		return llm.SelectedProviderRoute{Provider: "openai", Model: "compact-model", KeyName: "fixture-only-key-label",
			RouteID: req.RouteID, RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID,
			ProviderConnectionGeneration: req.ProviderConnectionGeneration, CredentialAssetGeneration: req.CredentialAssetGeneration,
			ModelSelector: req.ModelSelector, ExpiresAt: time.Now().Add(time.Minute),
			ModelProfile: &llm.ProviderModelProfile{ContextWindowTokens: 8000, MaxOutputTokens: 1024}}, nil
	}}
	calls := 0
	client := &funcClient{fn: func(callCtx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		calls++
		trusted, _ := llm.TrustedProviderRouteFrom(callCtx)
		if trusted.Route != maintenance || req.Model != "compact-model" || req.MaxTokens == nil || *req.MaxTokens != 1024 ||
			req.ReasoningEffort != "" || !req.ReasoningEffortExplicit {
			t.Fatalf("wrong outgoing compaction controls: %+v", req)
		}
		if llm.EstimateRequestTokens(req, llm.ModelProfile{ContextWindowTokens: 8000}) >= 6576 {
			t.Fatal("chunk exceeds independent model input capacity")
		}
		return llm.CompleteResponse{Content: goodSummaryJSON, FinishReason: "stop"}, nil
	}}
	s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryProviderRoute(maintenance, llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}, .05))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Summarise(ctx, rc, budgetTrajectory(20)); err != nil {
		t.Fatal(err)
	}
	if calls < 2 || calls != len(selections) {
		t.Fatalf("expected multiple independently budgeted chunks: %d calls, %d selections", calls, len(selections))
	}
	after, _ := llm.TrustedProviderRouteFrom(ctx)
	if after != parent {
		t.Fatal("summary changed the driving route")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Summarise(cancelled, rc, trajFixture()); !errors.Is(err, context.Canceled) || calls != len(selections) {
		t.Fatalf("cancelled summary dispatched: %v", err)
	}
}
