package react_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// timingDispatchClient simulates the provider dispatch boundary. The actual
// ReAct planner must label its call so it can consume the turn observation.
type timingDispatchClient struct{}

func (timingDispatchClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	llm.ObserveProviderRequest(ctx)
	return llm.CompleteResponse{Content: "done"}, nil
}

func (timingDispatchClient) Close(context.Context) error { return nil }

func TestReAct_TurnTiming_MainCallAfterAuxiliaryWork(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	q := fixedQuadruple(t, "timing-main-call")
	ctx := telemetry.BeginTurnTiming(ctxWith(t, q), logger, time.Now())
	client := timingDispatchClient{}
	for _, role := range []llm.CallRole{llm.CallRoleCompletion, llm.CallRoleTitle, llm.CallRoleCompaction, llm.CallRoleRepair, llm.CallRoleSkillDraft, llm.CallRoleAgentProposal} {
		if _, err := client.Complete(llm.WithCallRole(ctx, role), llm.CompleteRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("auxiliary work consumed turn observation: %s", logs.String())
	}
	p := react.New(client)
	rc := rcWith(q, "complete me", (&recordingEmit{}).emit)
	for range 2 {
		if _, err := p.Next(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	if count := strings.Count(logs.String(), "\n"); count != 1 {
		t.Fatalf("got %d timing records, want exactly one: %s", count, logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "turn.pre_model" || record["run_id"] != q.RunID {
		t.Fatalf("unexpected main model timing: %v", record)
	}
}
