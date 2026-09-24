package summarizer_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestTrajectoryAdmission_RefusesBeforeExternalDispatch(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing-identity", "invalid-grant", "query-too-large", "goal-too-large", "unserializable-action"} {
		t.Run(name, func(t *testing.T) {
			client := newStubClient()
			s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryHeavyOutputThreshold(8192))
			if err != nil {
				t.Fatal(err)
			}
			rc, tr := trajRC(name), trajFixture()
			tr.Summary = &planner.TrajectorySummary{Facts: []string{"keep previous constraints"}}
			prior := tr.Summary
			var want error
			switch name {
			case "missing-identity":
				rc.Quadruple.TenantID = ""
			case "invalid-grant":
				rc.ExternalGrant = json.RawMessage(`{"private":"PRIVATE-CONTENT",`)
				want = llm.ErrExternalGrantInvalid
			case "query-too-large":
				tr.Query = strings.Repeat("q", 8193)
				want = summarizer.ErrTrajectorySummaryCapacity
			case "goal-too-large":
				rc.Goal = strings.Repeat("g", 8193)
				want = summarizer.ErrTrajectorySummaryCapacity
			case "unserializable-action":
				tr.Steps[0].Action = func() {}
			}
			got, err := s.Summarise(t.Context(), rc, tr)
			if err == nil || got != nil || len(client.seenCalls()) != 0 || tr.Summary != prior {
				t.Fatalf("refusal dispatched or replaced checkpoint: summary=%v error=%v calls=%d", got, err, len(client.seenCalls()))
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			if strings.Contains(err.Error(), "PRIVATE-CONTENT") {
				t.Fatal("grant content leaked through refusal")
			}
		})
	}
}

func TestTrajectoryAdmission_StandaloneGrantAndAssistantEvidence(t *testing.T) {
	t.Parallel()
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryHeavyOutputThreshold(0))
	if err != nil {
		t.Fatal(err)
	}
	rc, tr := trajRC("grant-evidence"), trajFixture()
	// Transport propagation only: the composed client, not the summarizer,
	// authenticates this envelope. No fixture credential is exercised.
	rc.ExternalGrant = json.RawMessage(`{"grant_id":"fixture-grant"}`)
	rc.Goal = ""
	tr.Query = ""
	tr.Steps[0].AssistantPreamble = "I will inspect the current revision first."
	if _, err := s.Summarise(t.Context(), rc, tr); err != nil {
		t.Fatal(err)
	}
	calls := client.seenCalls()
	if len(calls) != 1 || calls[0].req.ExternalGrant == nil || calls[0].req.ExternalGrant.GrantID != "fixture-grant" {
		t.Fatal("standalone compaction dropped its grant before governed dispatch")
	}
	for _, want := range []string{rc.Query, "(same as query)", tr.Steps[0].AssistantPreamble} {
		if !strings.Contains(calls[0].content, want) {
			t.Fatalf("maintenance request omitted %q", want)
		}
	}
	if strings.Contains(calls[0].content, "read the vault first") {
		t.Fatal("private reasoning entered maintenance request")
	}
}

func TestTrajectoryAdmission_RouteRefusalPreservesCheckpoint(t *testing.T) {
	t.Parallel()
	denied := errors.New("PRIVATE-RESOLVER-CONTENT: maintenance route revoked")
	selections := 0
	resolver := trajectoryRouteResolver{selectFn: func(_ context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
		selections++
		if req.RouteID != "maintenance" || req.EffectiveAgentID != "agent" || req.TaskID != "task" {
			t.Fatalf("maintenance changed admitted identity: %+v", req)
		}
		return llm.SelectedProviderRoute{}, denied
	}}
	route := llm.ProviderRoute{RouteID: "maintenance", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "compact"}
	client := newStubClient()
	s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryProviderRoute(route, llm.ProviderRouteConfig{Resolver: resolver, RuntimeID: "runtime"}, .05))
	if err != nil {
		t.Fatal(err)
	}
	tr := trajFixture()
	tr.Summary = &planner.TrajectorySummary{Facts: []string{"retained constraint"}}
	before, err := tr.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	rc := trajRC("revoked")
	ctx, err := identity.WithRun(t.Context(), rc.Quadruple.Identity, rc.Quadruple.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithTrustedProviderRoute(ctx, llm.TrustedProviderRouteContext{Route: route, RuntimeID: "runtime", EffectiveAgentID: "agent", TaskID: "task", Purpose: llm.ProviderRoutePurposeRun})
	got, err := s.Summarise(ctx, rc, tr)
	if !errors.Is(err, llm.ErrProviderRouteResolutionFailed) || got != nil || len(client.seenCalls()) != 0 || selections != 1 {
		t.Fatalf("revoked maintenance route reached inference: got=%v err=%v calls=%d", got, err, len(client.seenCalls()))
	}
	if strings.Contains(err.Error(), "PRIVATE-RESOLVER-CONTENT") {
		t.Fatal("resolver content leaked through maintenance refusal")
	}
	after, err := tr.Serialize()
	if err != nil || string(before) != string(after) {
		t.Fatal("route refusal changed checkpoint/source")
	}
}
