package steering

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// turnTimingPlanner simulates provider dispatch without contacting a provider.
// Its first step injects a control at the runtime's real inbox boundary.
type turnTimingPlanner struct {
	registry *Registry
	control  ControlType
	step     int
}

func (p *turnTimingPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	telemetry.ObserveFirstModelRequest(ctx)
	telemetry.ObserveFirstModelRequest(ctx)
	p.step++
	if p.step > 1 {
		return planner.Finish{Reason: planner.FinishGoal}, nil
	}
	inbox, err := p.registry.Lookup(rc.Quadruple)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	switch p.control {
	case ControlUserMessage:
		payload = map[string]any{"message": "documented dummy private message"}
	case ControlRedirect:
		payload = map[string]any{"goal": "documented dummy private goal"}
	case ControlInjectContext:
		payload = map[string]any{"context": "documented dummy private context"}
	}
	if err := inbox.Enqueue(ControlEvent{
		Type: p.control, Identity: rc.Quadruple,
		CallerScope: ScopeOwnerUser, CallerTenant: rc.Quadruple.TenantID,
		Payload: payload,
	}); err != nil {
		return nil, err
	}
	if p.control == ControlResume || p.control == ControlApprove {
		return planner.RequestPause{Reason: planner.PauseApprovalRequired}, nil
	}
	return planner.CallTool{Tool: "noop"}, nil
}

func TestRun_TurnTiming_ControlStartsFreshObservation(t *testing.T) {
	for _, control := range []ControlType{ControlUserMessage, ControlRedirect, ControlInjectContext, ControlResume, ControlApprove} {
		t.Run(string(control), func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			rl, registry, _ := newTestRunLoop(t, WithRunLoopLogger(logger))
			p := &turnTimingPlanner{registry: registry, control: control}
			if _, err := rl.Run(t.Context(), runSpecFor(runA, p)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] == "turn.pre_model" {
					count++
					if record["session_id"] != runA.SessionID || record["run_id"] != runA.RunID {
						t.Fatalf("missing dispatch identity: %v", record)
					}
					if strings.Contains(line, "documented dummy private") {
						t.Fatalf("timing exposed control content: %s", line)
					}
				}
			}
			if count != 2 {
				t.Fatalf("got %d pre-model records, want one initial and one control turn: %s", count, output.String())
			}
		})
	}
}

func TestRun_TurnTiming_NoModel_NoRecord(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	rl, _, _ := newTestRunLoop(t, WithRunLoopLogger(logger))
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	if _, err := rl.Run(t.Context(), runSpecFor(runA, p)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "turn.pre_model") {
		t.Fatalf("deterministic run logged a model dispatch: %s", output.String())
	}
}
