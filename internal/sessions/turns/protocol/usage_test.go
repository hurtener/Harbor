package protocol

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	turns "github.com/hurtener/Harbor/internal/sessions/turns"
)

func TestService_Get_Usage_ExactSessionContentFreeAndHonest(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	row := fullContentRow("turn-usage")
	row.Status = turns.StatusRunning
	row.Sealed = false
	exact := int64(42)
	estimated := int64(7)
	row.Usage = turns.Usage{
		PromptTokens: turns.UsageMeasure{State: turns.UsageUnavailable},
		TotalTokens:  turns.UsageMeasure{State: turns.UsageExact, Value: &exact},
		CostMicroUSD: turns.UsageMeasure{State: turns.UsageEstimated, Value: &estimated},
		Model:        "model-usage",
	}
	mustSeedRow(t, st, fixtureID, row)

	ctx := auth.WithAgentReach(verifiedCtx(t, fixtureID), []string{"agent-a"})
	resp, err := svc.Get(ctx, GetRequest{SessionID: fixtureID.SessionID, TaskID: "turn-usage", Projection: ProjectionUsage})
	if err != nil {
		t.Fatalf("usage get: %v", err)
	}
	if resp.UsageTurn == nil {
		t.Fatal("usage get returned nil usage turn")
	}
	if resp.Turn.TurnID != "" || resp.OpsTurn != nil {
		t.Fatalf("usage lane must populate exactly UsageTurn: %+v", resp)
	}
	got := resp.UsageTurn
	if got.TurnID != "turn-usage" || got.TaskID != "turn-usage" || got.SessionID != fixtureID.SessionID || got.AgentID != "agent-a" {
		t.Fatalf("usage identifiers = %+v", got)
	}
	if got.Status != turns.StatusRunning || got.Sealed {
		t.Fatalf("usage lifecycle = status %q sealed %v, want running/unsealed", got.Status, got.Sealed)
	}
	if got.Usage.PromptTokens.State != turns.UsageUnavailable || got.Usage.PromptTokens.Value != nil {
		t.Fatalf("unavailable prompt measure changed: %+v", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens.State != turns.UsageExact || got.Usage.TotalTokens.Value == nil || *got.Usage.TotalTokens.Value != exact {
		t.Fatalf("exact total measure changed: %+v", got.Usage.TotalTokens)
	}
	if got.Usage.CostMicroUSD.State != turns.UsageEstimated || got.Usage.CostMicroUSD.Value == nil || *got.Usage.CostMicroUSD.Value != estimated {
		t.Fatalf("estimated cost measure changed: %+v", got.Usage.CostMicroUSD)
	}
	if got.Usage.Model != "model-usage" {
		t.Fatalf("reported model changed: %q", got.Usage.Model)
	}

	fields := reflect.VisibleFields(reflect.TypeOf(UsageTurnRow{}))
	gotFields := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		gotFields[field.Name] = struct{}{}
	}
	for _, forbidden := range []string{"Query", "Answer", "Reasoning", "Activity", "Pause", "Apps", "Inputs", "Outputs", "RunID", "FinishMessage", "ErrorMessage", "TenantID", "UserID"} {
		if _, ok := gotFields[forbidden]; ok {
			t.Errorf("UsageTurnRow exposes forbidden content field %q", forbidden)
		}
	}
}

func TestService_Get_Usage_ForeignSessionRemainsNonOracular(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-usage", turns.StatusComplete, ""))

	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-foreign", TaskID: "turn-usage", Projection: ProjectionUsage}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("foreign usage get: err = %v, want ErrTurnNotFound", err)
	}
}

func TestService_Get_Usage_ForeignPrincipalIsNotObservable(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	foreign := identity.Identity{TenantID: "tenant-foreign", UserID: "user-foreign", SessionID: fixtureID.SessionID}
	mustSeedRow(t, st, foreign, fixtureRow("turn-usage", turns.StatusComplete, ""))

	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet})
	if _, err := svc.Get(ctx, GetRequest{SessionID: fixtureID.SessionID, TaskID: "turn-usage", Projection: ProjectionUsage}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("foreign-principal usage get: err = %v, want ErrTurnNotFound", err)
	}
}

func TestService_Get_Usage_UsesOneDurableProjectionRead(t *testing.T) {
	_, st, proj, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-usage", turns.StatusComplete, ""))
	rec := &recordingProjector{inner: proj}
	svc, err := NewService(rec)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Get(verifiedCtx(t, fixtureID), GetRequest{SessionID: fixtureID.SessionID, TaskID: "turn-usage", Projection: ProjectionUsage}); err != nil {
		t.Fatalf("usage get: %v", err)
	}
	if calls := rec.snapshot(); !reflect.DeepEqual(calls, []string{"Get"}) {
		t.Fatalf("usage get calls = %v, want exactly [Get]", calls)
	}
}
