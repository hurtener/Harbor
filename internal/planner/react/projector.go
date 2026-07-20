package react

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/output"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// projectResponse maps an [llm.CompleteResponse] onto a
// [planner.Decision]:
//
//   - 0 ToolCalls + Content  → Finish{Goal}
//   - 0 ToolCalls + no content → Finish{NoPath}
//   - 1 ToolCall              → CallTool (or reserved-name translation
//     to Finish / SpawnTask / AwaitTask)
//   - N>1 ToolCalls           → CallParallel{Branches, Join: nil}
//     when `parallelEnabled` (the default), else the
//     serialization fallback (head CallTool + tail queued on
//     `rc.PendingToolCalls`).
//
// Narrowed successor to the silent-tail-drop guard: the
// terminal/blocking reserved-control names — `_finish`, `_await_task`,
// and the task-management controls `_task_status` / `_cancel_task` /
// `_steer_task` / `_pause_task` / `_resume_task` — stay standalone. Any
// of them co-occurring with ANY other tool-call in one response is
// rejected loudly with [planner.ErrInvalidDecision]: a terminal
// decision, a single-target block, and the task
// observation/cancel/steer/pause/resume controls have no coherent
// multi-call semantics (and a batched await would depend on a sibling
// spawn's not-yet-existing task_id). The guard runs BEFORE the head switch so it
// fires whether the reserved name is the head or in the tail, and
// independent of `parallelEnabled`.
//
// `_spawn_task` is NO LONGER standalone: a response mixing spawns with
// catalog tools (or with other spawns) projects to [planner.Batch] — a
// spawn may co-occur with tools and other spawns in one native response.
// The projector never constructs a degenerate one-branch Batch: a
// lone `_spawn_task` still projects to a plain SpawnTask, and a response
// with no spawn projects to CallTool / CallParallel exactly as before.
func projectResponse(resp llm.CompleteResponse, rc *planner.RunContext, parallelEnabled bool) (planner.Decision, error) {
	if len(resp.ToolCalls) == 0 {
		if resp.Content != "" {
			// Tools-mode unwrap (S1 seam closure): a schema-constrained
			// run whose profile is on OutputModeTools carries a
			// `{"name":"respond_with","arguments":...}` envelope in
			// resp.Content (see internal/llm/output's write half). Unwrap
			// it BEFORE it becomes the terminal Payload, so the runtime-
			// edge validation and AnswerPayload see the caller's schema
			// shape, never the envelope. A non-envelope answer (no
			// schema on this run, or a Native/Prompted profile) is
			// untouched — Payload stays the plain string exactly as
			// before (the runctx envelope builder's payload-capture
			// `string` case and runctx.ExtractAssistantAnswer both
			// depend on that shape).
			var payload any = resp.Content
			if rc.OutputSchema != nil {
				if args, ok := output.ParseRespondWith(resp.Content); ok {
					payload = args
				}
			}
			return planner.Finish{
				Reason:  planner.FinishGoal,
				Payload: payload,
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

	// Standalone guard: the terminal/blocking reserved-control names
	// (`_finish` / `_await_task` / `_task_status` / `_cancel_task` /
	// `_steer_task` / `_pause_task` / `_resume_task`) are
	// standalone. When any co-occurs with one or more other tool-calls,
	// fail loudly rather than silently honour the head and drop the rest
	// (the §13-forbidden silent-degradation pattern this fix closes).
	// Fires for head OR tail position, on BOTH the native-parallel path
	// and the serialization opt-out. `_spawn_task` is NOT standalone — it
	// partitions into a Batch below.
	if len(resp.ToolCalls) > 1 {
		for _, tc := range resp.ToolCalls {
			if isStandaloneControlName(tc.Name) {
				return nil, fmt.Errorf(
					"%w: planner-control meta-tool %q is standalone and cannot co-occur with other tool-calls (got %d tool-calls in one response — %q must be sent alone)",
					planner.ErrInvalidDecision, tc.Name, len(resp.ToolCalls), tc.Name,
				)
			}
		}
	}

	// Batch partition (AC-21′): a multi-call response carrying at least
	// one `_spawn_task` alongside ≥1 other call (a catalog tool or
	// another spawn) projects to a single planner.Batch — never to a
	// CallParallel with a synthetic spawn branch, never to two decisions.
	// The standalone guard above already rejected any `_finish` /
	// `_await_task` co-occurrence, so every remaining call is a catalog
	// tool or a spawn; with len > 1 the combined branch count is always
	// ≥ 2, so the projector never constructs a degenerate one-branch
	// Batch (a lone `_spawn_task` falls through to the plain SpawnTask
	// head switch below).
	if len(resp.ToolCalls) > 1 && responseHasSpawn(resp.ToolCalls) {
		return projectBatch(resp, rc)
	}

	first := resp.ToolCalls[0]
	switch first.Name {
	case FinishToolName:
		return translateNativeFinish(first), nil
	case SpawnTaskToolName:
		return translateNativeSpawn(first)
	case AwaitTaskToolName:
		return translateNativeAwait(first)
	case TaskStatusToolName:
		return translateNativeTaskStatus(first)
	case CancelTaskToolName:
		return translateNativeCancelTask(first)
	case SteerTaskToolName:
		return translateNativeSteerTask(first)
	case PauseTaskToolName:
		return translateNativePauseTask(first)
	case ResumeTaskToolName:
		return translateNativeResumeTask(first)
	default:
	}

	// Single regular tool-call. Resolve the provider-returned name back
	// to the real catalog name (declarations are sent sanitized).
	if len(resp.ToolCalls) == 1 {
		return planner.CallTool{
			Tool:   resolveDeclaredToolName(rc, first.Name),
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
				Tool:   resolveDeclaredToolName(rc, tc.Name),
				Args:   tc.Args,
				CallID: tc.ID,
			}
		}
		return planner.CallParallel{Branches: branches, Join: nil}, nil
	}

	// Serialization fallback (parallel_tool_calls: false):
	// dispatch the head, queue the tail on rc.PendingToolCalls.
	call := planner.CallTool{
		Tool:   resolveDeclaredToolName(rc, first.Name),
		Args:   first.Args,
		CallID: first.ID,
	}
	for _, tc := range resp.ToolCalls[1:] {
		rc.PendingToolCalls = append(rc.PendingToolCalls, planner.ToolCallDeferred{
			Name:   resolveDeclaredToolName(rc, tc.Name),
			Args:   tc.Args,
			CallID: tc.ID,
		})
	}
	return call, nil
}

// isStandaloneControlName reports whether name is a reserved
// planner-control meta-tool that must be sent alone: the terminal
// `_finish`, the single-target block `_await_task`, and the five
// task-management controls `_task_status` / `_cancel_task` /
// `_steer_task` / `_pause_task` / `_resume_task`. These names reject
// co-occurrence with any other tool-call in one response — a terminal
// decision, a single-target block, and the task
// observation/cancel/steer/pause/resume controls have no coherent
// multi-call semantics (widening a task-management control to batchable
// later is additive; retracting a shipped batchable surface is not, so
// the conservative non-batchable grammar is the first-wave choice).
// `_spawn_task` is deliberately EXCLUDED: it is batchable (see
// projectBatch).
func isStandaloneControlName(name string) bool {
	switch name {
	case FinishToolName, AwaitTaskToolName, TaskStatusToolName, CancelTaskToolName,
		SteerTaskToolName, PauseTaskToolName, ResumeTaskToolName:
		return true
	default:
		return false
	}
}

// responseHasSpawn reports whether any call in the response is a
// `_spawn_task` reserved-control call — the trigger for Batch
// partitioning when the response carries more than one call.
func responseHasSpawn(calls []llm.ToolCallStructured) bool {
	for _, tc := range calls {
		if tc.Name == SpawnTaskToolName {
			return true
		}
	}
	return false
}

// projectBatch partitions a native multi-call response into a
// planner.Batch: `_spawn_task` calls become Batch.Spawns (each stamped
// with its native call id), every other call becomes a Batch.Tool
// branch (catalog name resolved from the sanitized declaration name).
// Join stays nil → the executor collapses it to JoinAll, matching the
// native CallParallel path.
//
// Two projection-time semantic checks fail loud with
// planner.ErrInvalidDecision:
//
//   - a malformed `_spawn_task` args envelope (surfaced by
//     translateNativeSpawn — reused verbatim so the batch and
//     single-spawn paths agree on the schema); and
//   - FailFast disagreement across the spawns the executor would
//     auto-group (≥2 spawns sharing no explicit GroupID) — a batch
//     cannot honour two contradictory group-cancellation policies.
//
// NewBatch enforces the remaining structural invariants (non-degenerate,
// non-retain-turn); by construction len(calls) > 1 with ≥1 spawn and no
// standalone control name, so the combined branch count is always ≥ 2.
func projectBatch(resp llm.CompleteResponse, rc *planner.RunContext) (planner.Decision, error) {
	var tools []planner.CallTool
	var spawns []planner.SpawnTask
	for _, tc := range resp.ToolCalls {
		if tc.Name == SpawnTaskToolName {
			sp, err := translateNativeSpawn(tc)
			if err != nil {
				return nil, err
			}
			sp.CallID = tc.ID
			spawns = append(spawns, sp)
			continue
		}
		tools = append(tools, planner.CallTool{
			Tool:   resolveDeclaredToolName(rc, tc.Name),
			Args:   tc.Args,
			CallID: tc.ID,
		})
	}

	if err := validateAutoGroupedFailFast(spawns); err != nil {
		return nil, err
	}

	batch, err := planner.NewBatch(tools, spawns, nil)
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// validateAutoGroupedFailFast rejects a Batch whose auto-grouped spawns
// (≥2 entries sharing no explicit GroupID — the ones the executor folds
// into one resolve-or-create group) disagree on FailFast. A single
// group cannot both cancel-on-first-failure and not; the disagreement
// fails the whole batch loud, naming both conflicting values. Spawns
// carrying an explicit GroupID are never auto-grouped and are exempt.
func validateAutoGroupedFailFast(spawns []planner.SpawnTask) error {
	var (
		seen     bool
		failFast bool
	)
	for _, sp := range spawns {
		if sp.GroupID != "" {
			continue
		}
		if !seen {
			seen = true
			failFast = sp.Spec.FailFast
			continue
		}
		if sp.Spec.FailFast != failFast {
			return fmt.Errorf(
				"%w: Batch auto-grouped spawns disagree on fail_fast (%t vs %t) — a single auto-created group cannot honour both cancellation policies",
				planner.ErrInvalidDecision, failFast, sp.Spec.FailFast,
			)
		}
	}
	return nil
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
			Description       string `json:"description"`
			Query             string `json:"query"`
			Priority          int    `json:"priority"`
			RetainTurn        bool   `json:"retain_turn"`
			FailFast          bool   `json:"fail_fast"`
			PropagateOnCancel string `json:"propagate_on_cancel"`
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
	// Validate propagate_on_cancel against the accepted set, mirroring the
	// kind enum check above. Empty maps to the cascade default at the
	// dispatch edge; an out-of-enum value fails loud naming the offending
	// value — never silently clamped to cascade.
	switch env.Spec.PropagateOnCancel {
	case "", tasks.PropagateCascade, tasks.PropagateIsolate:
	default:
		return planner.SpawnTask{}, fmt.Errorf(
			"%w: react._spawn_task propagate_on_cancel %q not in {cascade, isolate}",
			planner.ErrInvalidDecision, env.Spec.PropagateOnCancel,
		)
	}
	return planner.SpawnTask{
		Kind: kind,
		Spec: planner.SpawnSpec{
			Description:       env.Spec.Description,
			Query:             env.Spec.Query,
			Priority:          env.Spec.Priority,
			RetainTurn:        env.Spec.RetainTurn,
			FailFast:          env.Spec.FailFast,
			PropagateOnCancel: env.Spec.PropagateOnCancel,
		},
		GroupID: tasks.TaskGroupID(env.GroupID),
	}, nil
}

// translateNativeTaskStatus parses a `_task_status` native call into a
// [planner.TaskStatusQuery]. A missing / null / empty `task_ids` array
// yields `TaskIDs: nil`, which the executor reads as "report every task
// this run spawned". Malformed JSON fails loud with a wrapped
// [planner.ErrInvalidDecision] (mirroring translateNativeSpawn's malformed
// guard); a valid-but-empty list is the list-everything sentinel, never an
// error.
func translateNativeTaskStatus(tc llm.ToolCallStructured) (planner.TaskStatusQuery, error) {
	type taskStatusArgs struct {
		TaskIDs []string `json:"task_ids"`
	}
	var args taskStatusArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.TaskStatusQuery{}, fmt.Errorf(
				"%w: react._task_status args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if len(args.TaskIDs) == 0 {
		return planner.TaskStatusQuery{TaskIDs: nil}, nil
	}
	ids := make([]tasks.TaskID, 0, len(args.TaskIDs))
	for _, id := range args.TaskIDs {
		ids = append(ids, tasks.TaskID(id))
	}
	return planner.TaskStatusQuery{TaskIDs: ids}, nil
}

// translateNativeCancelTask parses a `_cancel_task` native call into a
// [planner.CancelTask]. An empty `task_id` fails loud with
// [planner.ErrInvalidDecision] (mirroring translateNativeAwait's empty-id
// guard verbatim); `reason` is optional.
func translateNativeCancelTask(tc llm.ToolCallStructured) (planner.CancelTask, error) {
	type cancelArgs struct {
		TaskID string `json:"task_id"`
		Reason string `json:"reason"`
	}
	var args cancelArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.CancelTask{}, fmt.Errorf(
				"%w: react._cancel_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.CancelTask{}, fmt.Errorf(
			"%w: react._cancel_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	return planner.CancelTask{TaskID: tasks.TaskID(args.TaskID), Reason: args.Reason}, nil
}

// translateNativeSteerTask parses a `_steer_task` native call into a
// [planner.SteerTask]. An empty `task_id` OR an empty `directive` fails
// loud with [planner.ErrInvalidDecision] (mirroring
// translateNativeCancelTask's empty-id guard); malformed JSON fails loud
// the same way.
func translateNativeSteerTask(tc llm.ToolCallStructured) (planner.SteerTask, error) {
	type steerArgs struct {
		TaskID    string `json:"task_id"`
		Directive string `json:"directive"`
	}
	var args steerArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.SteerTask{}, fmt.Errorf(
				"%w: react._steer_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.SteerTask{}, fmt.Errorf(
			"%w: react._steer_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	if args.Directive == "" {
		return planner.SteerTask{}, fmt.Errorf(
			"%w: react._steer_task requires non-empty directive (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	return planner.SteerTask{TaskID: tasks.TaskID(args.TaskID), Directive: args.Directive}, nil
}

// translateNativePauseTask parses a `_pause_task` native call into a
// [planner.PauseTask]. An empty `task_id` fails loud with
// [planner.ErrInvalidDecision] (mirroring translateNativeCancelTask's
// empty-id guard verbatim); `reason` is optional. Malformed JSON fails
// loud the same way.
func translateNativePauseTask(tc llm.ToolCallStructured) (planner.PauseTask, error) {
	type pauseArgs struct {
		TaskID string `json:"task_id"`
		Reason string `json:"reason"`
	}
	var args pauseArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.PauseTask{}, fmt.Errorf(
				"%w: react._pause_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.PauseTask{}, fmt.Errorf(
			"%w: react._pause_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	return planner.PauseTask{TaskID: tasks.TaskID(args.TaskID), Reason: args.Reason}, nil
}

// translateNativeResumeTask parses a `_resume_task` native call into a
// [planner.ResumeTask]. An empty `task_id` fails loud with
// [planner.ErrInvalidDecision] (mirroring translateNativeCancelTask's
// empty-id guard verbatim); `directive` is optional. Malformed JSON
// fails loud the same way.
func translateNativeResumeTask(tc llm.ToolCallStructured) (planner.ResumeTask, error) {
	type resumeArgs struct {
		TaskID    string `json:"task_id"`
		Directive string `json:"directive"`
	}
	var args resumeArgs
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return planner.ResumeTask{}, fmt.Errorf(
				"%w: react._resume_task args malformed JSON: %w (raw=%q)",
				planner.ErrInvalidDecision, err, string(tc.Args),
			)
		}
	}
	if args.TaskID == "" {
		return planner.ResumeTask{}, fmt.Errorf(
			"%w: react._resume_task requires non-empty task_id (raw=%q)",
			planner.ErrInvalidDecision, string(tc.Args),
		)
	}
	return planner.ResumeTask{TaskID: tasks.TaskID(args.TaskID), Directive: args.Directive}, nil
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
