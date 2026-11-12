package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tasks"
)

type limitAnswerClient struct{}

func (limitAnswerClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Content.Text != nil && strings.Contains(*last.Content.Text, "Do not call any tools") {
		return llm.CompleteResponse{Content: "Stopped at the limit; the comparison remains.", FinishReason: "stop"}, nil
	}
	return llm.CompleteResponse{Content: "unexpected full answer", FinishReason: "stop"}, nil
}

func (limitAnswerClient) Close(context.Context) error { return nil }

// A served run that reaches its limit completes (never fails) with the
// partial answer and finish_reason limit_reached in its result envelope.
func TestServedRun_LimitReachedCompletesWithPartialAnswer(t *testing.T) {
	env, _, _ := retainedServerHarness(t, func(opts *RunLoopDriverOptions) {
		opts.Planner = react.New(limitAnswerClient{})
		opts.RunLimits = planner.RunLimits{MaxDuration: time.Nanosecond}
	})
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "served-limit"}
	result := retainedServerTurn(t, env, id, "Compare every vendor", nil)
	if result.Status != tasks.StatusComplete || result.Result == nil {
		t.Fatalf("limit failed the run: %v %+v", result.Status, result.Error)
	}
	var envelope planner.AnswerEnvelope
	if err := json.Unmarshal(result.Result.Value, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.FinishReason != string(planner.FinishLimitReached) || envelope.Answer != "Stopped at the limit; the comparison remains." {
		t.Fatalf("envelope = %+v", envelope)
	}
}
