package react

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

func projectResponse(resp llm.CompleteResponse, rc *planner.RunContext) (planner.Decision, error) {
	if len(resp.ToolCalls) == 0 {
		if resp.Content != "" {
			return planner.Finish{
				Reason:  planner.FinishGoal,
				Payload: resp.Content,
				Metadata: map[string]any{
					"via":        "react.projectResponse",
					"goal_reach": true,
				},
			}, nil
		}
		return planner.Finish{
			Reason:   planner.FinishNoPath,
			Metadata: map[string]any{"followup": true, "via": "react.projectResponse.empty"},
		}, nil
	}

	first := resp.ToolCalls[0]
	switch first.Name {
	case FinishToolName:
		return translateNativeFinish(first), nil
	case SpawnTaskToolName:
		return translateNativeSpawn(first)
	case AwaitTaskToolName:
		return translateNativeAwait(first)
	default:
	}

	call := planner.CallTool{
		Tool:   first.Name,
		Args:   first.Args,
		CallID: first.ID,
	}

	if len(resp.ToolCalls) > 1 {
		for _, tc := range resp.ToolCalls[1:] {
			rc.PendingToolCalls = append(rc.PendingToolCalls, planner.ToolCallDeferred{
				Name:   tc.Name,
				Args:   tc.Args,
				CallID: tc.ID,
			})
		}
	}

	return call, nil
}

func drainPending(rc *planner.RunContext) *planner.CallTool {
	if len(rc.PendingToolCalls) == 0 {
		return nil
	}
	d := rc.PendingToolCalls[0]
	rc.PendingToolCalls = rc.PendingToolCalls[1:]
	return &planner.CallTool{
		Tool:   d.Name,
		Args:   d.Args,
		CallID: d.CallID,
	}
}

func translateNativeFinish(tc llm.ToolCallStructured) planner.Finish {
	type finishArgs struct {
		Answer any `json:"answer"`
	}
	var args finishArgs
	if len(tc.Args) > 0 {
		_ = json.Unmarshal(tc.Args, &args) //nolint:errcheck // best-effort decode; a missing/non-string answer surfaces as nil Payload (the metadata carries raw_args for observability)
	}
	return planner.Finish{
		Reason:  planner.FinishGoal,
		Payload: args.Answer,
		Metadata: map[string]any{
			"raw_args":   string(tc.Args),
			"via":        "react.projectResponse._finish",
			"tool":       FinishToolName,
			"goal_reach": true,
		},
	}
}

func translateNativeSpawn(tc llm.ToolCallStructured) (planner.SpawnTask, error) {
	type spawnArgsEnvelope struct {
		Kind    string `json:"kind"`
		GroupID string `json:"group_id"`
		Spec    struct {
			Description string `json:"description"`
			Query       string `json:"query"`
			Priority    int    `json:"priority"`
			RetainTurn  bool   `json:"retain_turn"`
			FailFast    bool   `json:"fail_fast"`
		} `json:"spec"`
	}
	var env spawnArgsEnvelope
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &env); err != nil {
			return planner.SpawnTask{}, fmt.Errorf(
				"%w: react._spawn_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	kind := tasks.TaskKind(env.Kind)
	if kind == "" {
		kind = tasks.KindBackground
	}
	switch kind {
	case tasks.KindForeground, tasks.KindBackground:
	default:
		return planner.SpawnTask{}, fmt.Errorf(
			"%w: react._spawn_task kind %q not in {foreground, background}",
			planner.ErrInvalidDecision, env.Kind,
		)
	}
	return planner.SpawnTask{
		Kind: kind,
		Spec: planner.SpawnSpec{
			Description: env.Spec.Description,
			Query:       env.Spec.Query,
			Priority:    env.Spec.Priority,
			RetainTurn:  env.Spec.RetainTurn,
			FailFast:    env.Spec.FailFast,
		},
		GroupID: tasks.TaskGroupID(env.GroupID),
	}, nil
}

func translateNativeAwait(tc llm.ToolCallStructured) (planner.AwaitTask, error) {
	type awaitArgs struct {
		TaskID string `json:"task_id"`
	}
	var args awaitArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.AwaitTask{}, fmt.Errorf(
				"%w: react._await_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.AwaitTask{}, fmt.Errorf(
			"%w: react._await_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	return planner.AwaitTask{TaskID: tasks.TaskID(args.TaskID)}, nil
}
