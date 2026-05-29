package react

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// projectResponse maps an [llm.CompleteResponse] onto a
// [planner.Decision] (brief 15 §6 "Decision-sum invariance"):
//
//   - 0 ToolCalls + Content  → Finish{Goal}
//   - 0 ToolCalls + no content → Finish{NoPath}
//   - 1 ToolCall              → CallTool (or reserved-name translation
//     to Finish / SpawnTask / AwaitTask)
//   - N>1 ToolCalls           → CallParallel{Branches, Join: nil}
//     when `parallelEnabled` (Phase 107d — D-169, the default), else the
//     Phase 107c serialization fallback (head CallTool + tail queued on
//     `rc.PendingToolCalls`).
//
// AC-21 (carried-over 107c silent tail-drop fix): a reserved
// planner-control name (`_finish` / `_spawn_task` / `_await_task`)
// co-occurring with ANY other tool-call in one response is rejected
// loudly with [planner.ErrInvalidDecision] — reserved control meta-tools
// are standalone, never batchable / parallelisable branches. The guard
// runs BEFORE the head switch so it fires whether the reserved name is
// the head or in the tail, and independent of `parallelEnabled`.
func projectResponse(resp llm.CompleteResponse, rc *planner.RunContext, parallelEnabled bool) (planner.Decision, error) {
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

	// AC-21: a reserved planner-control meta-tool is standalone. When it
	// co-occurs with one or more other tool-calls, fail loudly rather
	// than silently honour the head and drop the rest (the §13-forbidden
	// silent-degradation pattern this fix closes). Fires for head OR
	// tail position, on BOTH the native-parallel path and the
	// serialization opt-out.
	if len(resp.ToolCalls) > 1 {
		for _, tc := range resp.ToolCalls {
			if isReservedControlName(tc.Name) {
				return nil, fmt.Errorf(
					"%w: planner-control meta-tool %q is standalone and cannot co-occur with other tool-calls (got %d tool-calls in one response — control meta-tools are not batchable or parallelisable branches)",
					planner.ErrInvalidDecision, tc.Name, len(resp.ToolCalls),
				)
			}
		}
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

	// Single regular tool-call.
	if len(resp.ToolCalls) == 1 {
		return planner.CallTool{
			Tool:   first.Name,
			Args:   first.Args,
			CallID: first.ID,
		}, nil
	}

	// N>1 regular tool-calls. AC-21 guard above already guaranteed none
	// is a reserved control name.
	if parallelEnabled {
		// AC-8: emit a native CallParallel. Join stays nil → the
		// executor's normaliseJoin collapses it to JoinAll (AC-5).
		branches := make([]planner.CallTool, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			branches[i] = planner.CallTool{
				Tool:   tc.Name,
				Args:   tc.Args,
				CallID: tc.ID,
			}
		}
		return planner.CallParallel{Branches: branches, Join: nil}, nil
	}

	// Serialization fallback (Phase 107c — parallel_tool_calls: false):
	// dispatch the head, queue the tail on rc.PendingToolCalls.
	call := planner.CallTool{
		Tool:   first.Name,
		Args:   first.Args,
		CallID: first.ID,
	}
	for _, tc := range resp.ToolCalls[1:] {
		rc.PendingToolCalls = append(rc.PendingToolCalls, planner.ToolCallDeferred{
			Name:   tc.Name,
			Args:   tc.Args,
			CallID: tc.ID,
		})
	}
	return call, nil
}

// isReservedControlName reports whether name is one of the reserved
// planner-control meta-tools the projector translates to a terminal /
// standalone Decision (Finish / SpawnTask / AwaitTask). These are never
// catalog tools and never parallelisable branches (AC-21).
func isReservedControlName(name string) bool {
	switch name {
	case FinishToolName, SpawnTaskToolName, AwaitTaskToolName:
		return true
	default:
		return false
	}
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
