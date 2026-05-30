// cmd/harbor/cmd_dev_executor.go — the dev binary's `steering.ToolExecutor`
// implementation. Phase 83i (D-152).
//
// Before 83i the runloop's default case dropped every planner CallTool
// decision on the floor (Phase 53's deliberately-punted scope), which
// made multi-step ReAct structurally broken against real LLMs because
// the planner never saw tool observations. The audit pinned this as
// the root cause of the "64 steps, 0 tools called" failure mode that
// surfaced in the v1.1 operator validation.
//
// devToolExecutor closes that seam against the production
// `tools.ToolCatalog`:
//
//   - CallTool: look up the descriptor by name, call Invoke under
//     the per-step ctx, return the typed ToolResult.Value (plus Meta)
//     as the observation. The planner's next step sees this on
//     `RunContext.Trajectory.Steps[N].Observation`.
//
//   - CallParallel / SpawnTask / AwaitTask: V1.1 returns
//     ErrDecisionShapeUnsupported. The runloop surfaces this to the
//     planner as the step's observation, and the planner can choose
//     a different path (most ReAct planners will repair to a serial
//     CallTool or Finish).
//
// The executor is constructed once per dev-stack and shared across
// every run; it holds only the catalog (immutable after construction)
// + a logger, so the D-025 reuse contract is trivially satisfied.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// devToolExecutor is the dev binary's production `steering.ToolExecutor`
// implementation. Concurrent-safe — the catalog + artifact store are
// immutable after the dev-stack boot wiring runs.
//
// D-026 heavy-content discipline: tool results whose JSON encoding
// exceeds `heavyThreshold` get stored in the artifact store; the
// runloop's trajectory append uses the small llmObservation summary
// so the next LLM prompt never carries raw heavy content. Before
// this discipline the v1.1 operator validation hit `ErrContextLeak`
// after the first tool call (the youtube_get_metadata tool returns
// ~1.5 MB which would otherwise reach the LLM verbatim).
type devToolExecutor struct {
	cat            tools.ToolCatalog
	artifacts      artifacts.ArtifactStore
	heavyThreshold int
	logger         *slog.Logger

	// parallel dispatches CallParallel decisions (Phase 107d — D-169).
	// Constructed once over the same catalog (which satisfies
	// parallel.Resolver via Resolve); immutable after construction
	// (D-025). The native React path drives it in non-atomic mode so a
	// single bad-args branch becomes that branch's error result rather
	// than aborting the whole call (every provider tool_call_id must be
	// answered).
	parallel *parallel.Executor

	// tasks is the background-task registry SpawnTask / AwaitTask dispatch
	// drives (Phase 107e — D-170). SpawnTask creates a KindBackground
	// task the per-task RunLoop driver picks up and runs; AwaitTask and a
	// retain-turn SpawnTask poll Get until the task reaches a terminal
	// status. Immutable after construction (D-025); the registry is itself
	// concurrent-safe. Nil only in degraded / legacy wiring — Spawn / Await
	// then fail loud with ErrDecisionShapeUnsupported rather than panic.
	tasks tasks.TaskRegistry

	// maxSpawnDepth caps the ParentTaskID-chain depth of planner-spawned
	// background tasks (Phase 107e — D-170; planner.absolute_max_spawn_depth).
	// A SpawnTask whose child would exceed it is rejected loudly so a
	// background sub-agent that itself emits SpawnTask cannot recurse
	// without bound. The cap bounds depth, not breadth.
	maxSpawnDepth int
}

// defaultMaxSpawnDepth bounds planner-spawned background-task recursion
// when planner.absolute_max_spawn_depth is unset / non-positive. Four
// levels of background nesting is generous for V1.1.x dev workloads.
const defaultMaxSpawnDepth = 4

// spawnAwaitPollInterval is the cadence at which AwaitTask + a retain-turn
// SpawnTask poll the registry for a terminal status. The registry's
// documented poll wake mode (internal/tasks/groups.go) is a cheap
// in-memory Get on the dev path; the wait is bounded by the caller's ctx.
const spawnAwaitPollInterval = 100 * time.Millisecond

// newDevToolExecutor binds the catalog + artifact store the runloop
// dispatches against. Both are the SAME instances bootDevStack
// already constructs. `heavyThreshold` is the operator-configured
// cfg.Artifacts.HeavyOutputThresholdBytes; tool results whose JSON
// encoding exceeds it get promoted to ArtifactStub-shaped
// llmObservations.
func newDevToolExecutor(cat tools.ToolCatalog, artStore artifacts.ArtifactStore, taskReg tasks.TaskRegistry, heavyThreshold, maxSpawnDepth int, logger *slog.Logger) *devToolExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	if heavyThreshold <= 0 {
		heavyThreshold = 32 * 1024 // safety floor matches Wave 11 default
	}
	if maxSpawnDepth <= 0 {
		maxSpawnDepth = defaultMaxSpawnDepth
	}
	return &devToolExecutor{
		cat:            cat,
		artifacts:      artStore,
		heavyThreshold: heavyThreshold,
		logger:         logger,
		// AC-1: the catalog already satisfies parallel.Resolver via
		// Resolve(name); reuse it as the dispatcher's resolver. No second
		// fanout engine (§13).
		parallel:      parallel.New(cat),
		tasks:         taskReg,
		maxSpawnDepth: maxSpawnDepth,
	}
}

// ExecuteDecision implements `steering.ToolExecutor`. Dispatches the
// decision per its shape:
//
//   - CallTool: catalog.Resolve(name) → descriptor.Invoke(ctx, args)
//     under the run's identity-scoped ctx. The raw `observation` is
//     the typed ToolResult.Value; the `llmObservation` is the same
//     value unless the encoded result exceeds `heavyThreshold`, in
//     which case it gets promoted to an artifact-backed summary.
//   - CallParallel: dispatch the branches concurrently via the shared
//     parallel.Executor in non-atomic mode (Phase 107d — D-169), then
//     assemble a per-branch aggregate observation (raw + D-026
//     projected) so the prompt builder can round-trip N RoleTool
//     messages.
//   - SpawnTask: spawn a KindBackground task via the TaskRegistry under
//     the run's identity triple, bounded by the spawn-depth cap (Phase
//     107e — D-170). A non-retain-turn spawn returns {task_id, kind,
//     status} immediately; a retain-turn spawn blocks (ctx-bounded) until
//     the spawned task reaches a terminal status and returns its outcome.
//   - AwaitTask: poll the named task (Get) until it reaches a terminal
//     status, then return its answer-envelope / error as the observation
//     (Phase 107e — D-170). Both spawn/await observations go through the
//     same D-026 projectForLLM discipline so a heavy result never trips
//     ErrContextLeak.
func (e *devToolExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, decision planner.Decision) (any, any, error) {
	switch d := decision.(type) {
	case planner.CallTool:
		return e.callTool(ctx, rc, d)
	case planner.CallParallel:
		return e.callParallel(ctx, rc, d)
	case planner.SpawnTask:
		return e.spawnTask(ctx, rc, d)
	case planner.AwaitTask:
		return e.awaitTask(ctx, rc, d)
	default:
		return nil, nil, fmt.Errorf("%w: %T", steering.ErrDecisionShapeUnsupported, decision)
	}
}

// callTool dispatches a single CallTool. Errors:
//   - tool name does not resolve → wrapped tools.ErrToolNotFound.
//   - descriptor.Invoke returns an error → wrapped + surfaced.
//   - the result's Value is the observation the planner sees on its
//     next step (after D-026 heavy-content projection).
func (e *devToolExecutor) callTool(ctx context.Context, rc planner.RunContext, d planner.CallTool) (any, any, error) {
	if d.Tool == "" {
		return nil, nil, errors.New("CallTool.Tool is empty")
	}
	desc, ok := e.cat.Resolve(d.Tool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, d.Tool)
	}
	if desc.Invoke == nil {
		return nil, nil, fmt.Errorf("tool %q is registered without an Invoke function", d.Tool)
	}
	result, err := desc.Invoke(ctx, d.Args)
	if err != nil {
		e.logger.Warn("devToolExecutor: tool invoke failed",
			slog.String("tool", d.Tool),
			slog.String("err", err.Error()))
		return nil, nil, fmt.Errorf("tool %q invoke: %w", d.Tool, err)
	}
	raw := result.Value
	if raw == nil && len(result.Meta) > 0 {
		raw = map[string]any{"meta": result.Meta}
	}
	llmObs := e.projectForLLM(ctx, rc, d.Tool, raw)
	return raw, llmObs, nil
}

// callParallel dispatches a CallParallel decision (Phase 107d — D-169 /
// AC-1..AC-4). The branches fan out concurrently through the shared
// parallel.Executor in NON-ATOMIC mode (AC-2): a branch whose tool fails
// to resolve or whose args fail Validate surfaces as that branch's error
// result, NOT a whole-call abort — so every provider tool_call_id can be
// answered. dispatchAll returns one Result per branch in branch-index
// order; this method assembles two aggregates:
//
//   - raw observation: the untruncated per-branch tool values, what the
//     trajectory persists as Step.Observation.
//   - llmObservation: each branch's value run through the same D-026
//     projectForLLM discipline as the single-CallTool path (AC-3), so a
//     parallel observation with several heavy branches never trips the
//     LLM-edge ErrContextLeak guard. The prompt builder decomposes this
//     into N RoleTool messages (AC-9).
//
// Whole-call aborts that survive non-atomic mode — branch-count cap
// exceeded, missing identity, empty branch set, malformed join — come
// back as the Execute error and are surfaced verbatim (fail-loud per
// §13); the runloop wraps them as the step's error observation and the
// planner re-plans.
func (e *devToolExecutor) callParallel(ctx context.Context, rc planner.RunContext, d planner.CallParallel) (any, any, error) {
	results, err := e.parallel.Execute(ctx, d, parallel.WithNonAtomicSetup())
	if err != nil {
		return nil, nil, fmt.Errorf("parallel dispatch: %w", err)
	}
	raw := planner.ParallelObservation{Branches: make([]planner.ParallelBranchObservation, 0, len(results))}
	llmAgg := planner.ParallelObservation{Branches: make([]planner.ParallelBranchObservation, 0, len(results))}
	for _, r := range results {
		callID := ""
		if r.Index >= 0 && r.Index < len(d.Branches) {
			callID = d.Branches[r.Index].CallID
		}
		if r.Err != nil {
			branchErr := planner.ParallelBranchObservation{
				CallID: callID,
				Tool:   r.Tool,
				Index:  r.Index,
				Error:  r.Err.Error(),
			}
			raw.Branches = append(raw.Branches, branchErr)
			llmAgg.Branches = append(llmAgg.Branches, branchErr)
			continue
		}
		var rawVal any
		if r.Result != nil {
			rawVal = r.Result.Value
			if rawVal == nil && len(r.Result.Meta) > 0 {
				rawVal = map[string]any{"meta": r.Result.Meta}
			}
		}
		raw.Branches = append(raw.Branches, planner.ParallelBranchObservation{
			CallID: callID,
			Tool:   r.Tool,
			Index:  r.Index,
			Value:  rawVal,
		})
		// AC-3: per-branch D-026 projection — heavy branch values get
		// promoted to an artifact-stub summary independently.
		llmAgg.Branches = append(llmAgg.Branches, planner.ParallelBranchObservation{
			CallID: callID,
			Tool:   r.Tool,
			Index:  r.Index,
			Value:  e.projectForLLM(ctx, rc, r.Tool, rawVal),
		})
	}
	return raw, llmAgg, nil
}

// spawnTask dispatches a planner.SpawnTask (Phase 107e — D-170).
//
// It maps the decision into a tasks.SpawnRequest under the run's
// identity triple (NEVER a global — CLAUDE.md §6) and calls Spawn. The
// spawned task's Kind defaults to KindBackground (the projector already
// defaults it); its ParentTaskID is the current run's task so the
// spawn-depth cap (AC-8) and the registry's cancel-cascade both see the
// lineage. A Spawn error (including ErrIdentityRequired) is surfaced as
// the step's error — never swallowed (§13).
//
//   - Non-retain-turn (Spec.RetainTurn == false): returns immediately
//     with {task_id, kind, status:"spawned"}. The per-task RunLoop driver
//     picks up the task.spawned event and drives the background sub-run;
//     the planner joins later by emitting AwaitTask.
//   - Retain-turn (Spec.RetainTurn == true): blocks (ctx-bounded) until
//     the spawned task reaches a terminal status, returning its outcome —
//     a synchronous spawn-and-join in one decision.
func (e *devToolExecutor) spawnTask(ctx context.Context, rc planner.RunContext, d planner.SpawnTask) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: SpawnTask (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("SpawnTask: attach identity: %w", idErr)
	}

	// AC-8 recursion guard: the new task's depth is the parent chain
	// depth + 1. The parent is the current run's task (RunID doubles as
	// the TaskID at the dev layer). Reject loudly above the cap — never a
	// silent drop.
	parentID := tasks.TaskID(rc.Quadruple.RunID)
	if depth := e.spawnChainDepth(taskCtx, parentID); depth+1 > e.maxSpawnDepth {
		return nil, nil, fmt.Errorf(
			"SpawnTask: spawn would reach depth %d, exceeding planner.absolute_max_spawn_depth=%d (parent task %q)",
			depth+1, e.maxSpawnDepth, parentID)
	}

	kind := d.Kind
	if kind == "" {
		kind = tasks.KindBackground
	}
	req := tasks.SpawnRequest{
		Identity:         identity.Quadruple{Identity: rc.Quadruple.Identity},
		Kind:             kind,
		Description:      d.Spec.Description,
		Query:            d.Spec.Query,
		Priority:         d.Spec.Priority,
		GroupID:          d.GroupID,
		NotifyOnComplete: true,
	}
	if parentID != "" {
		req.ParentTaskID = &parentID
	}
	handle, err := e.tasks.Spawn(taskCtx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("SpawnTask: registry spawn: %w", err)
	}

	if d.Spec.RetainTurn {
		task, awaitErr := e.awaitTerminal(taskCtx, handle.ID)
		if awaitErr != nil {
			return nil, nil, fmt.Errorf("SpawnTask(retain-turn, %q): await: %w", handle.ID, awaitErr)
		}
		raw := taskOutcomeObservation(task)
		return raw, e.projectForLLM(ctx, rc, react.SpawnTaskToolName, raw), nil
	}

	raw := map[string]any{
		"task_id": string(handle.ID),
		"kind":    string(kind),
		"status":  "spawned",
	}
	return raw, raw, nil
}

// awaitTask dispatches a planner.AwaitTask (Phase 107e — D-170): it
// blocks (ctx-bounded) until the named task reaches a terminal status,
// then returns its answer-envelope / error as the observation. Get
// enforces identity scope, so a cross-session / cross-tenant task id
// surfaces as a not-found error observation (AC-11) rather than leaking.
func (e *devToolExecutor) awaitTask(ctx context.Context, rc planner.RunContext, d planner.AwaitTask) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: AwaitTask (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	if d.TaskID == "" {
		return nil, nil, errors.New("AwaitTask: TaskID is empty")
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("AwaitTask: attach identity: %w", idErr)
	}
	task, err := e.awaitTerminal(taskCtx, d.TaskID)
	if err != nil {
		return nil, nil, fmt.Errorf("AwaitTask(%q): %w", d.TaskID, err)
	}
	raw := taskOutcomeObservation(task)
	return raw, e.projectForLLM(ctx, rc, react.AwaitTaskToolName, raw), nil
}

// awaitTerminal polls the registry for `id` until it reaches a terminal
// status (Complete / Failed / Cancelled) or `ctx` is done. The ctx-done
// path returns the ctx error so a never-terminating child surfaces as a
// deadline error observation rather than a hang (the runloop wraps it;
// the planner re-plans). The poll cadence is spawnAwaitPollInterval.
func (e *devToolExecutor) awaitTerminal(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	ticker := time.NewTicker(spawnAwaitPollInterval)
	defer ticker.Stop()
	for {
		task, err := e.tasks.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch task.Status {
		case tasks.StatusComplete, tasks.StatusFailed, tasks.StatusCancelled:
			return task, nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// spawnChainDepth returns the number of ParentTaskID hops from `id` up to
// the root: a top-level (foreground) task has depth 0, a task spawned
// from it has depth 1, and so on. The walk is bounded to maxSpawnDepth+1
// iterations so a corrupt or cyclic parent chain cannot loop unbounded; a
// Get error mid-walk stops the walk and returns the count so far
// (best-effort — the cap still bounds creation at the spawn site).
func (e *devToolExecutor) spawnChainDepth(ctx context.Context, id tasks.TaskID) int {
	depth := 0
	cur := id
	for cur != "" && depth <= e.maxSpawnDepth {
		task, err := e.tasks.Get(ctx, cur)
		if err != nil || task == nil || task.ParentTaskID == nil {
			break
		}
		depth++
		cur = *task.ParentTaskID
	}
	return depth
}

// taskOutcomeObservation projects a terminal task record into the
// planner-readable observation for AwaitTask / retain-turn SpawnTask. The
// answer envelope on Result.Value is JSON (the per-task driver's
// {answer, finish_reason, tool_calls_seen} shape); it is embedded parsed
// so the planner sees structured data rather than a JSON-string. A failed
// / cancelled task carries its error code + message instead.
func taskOutcomeObservation(task *tasks.Task) any {
	out := map[string]any{
		"task_id": string(task.ID),
		"status":  string(task.Status),
	}
	if task.Result != nil && len(task.Result.Value) > 0 {
		var v any
		if err := json.Unmarshal(task.Result.Value, &v); err == nil {
			out["result"] = v
		} else {
			out["result"] = string(task.Result.Value)
		}
	}
	if task.Error != nil {
		out["error"] = map[string]any{
			"code":    task.Error.Code,
			"message": task.Error.Message,
		}
	}
	return out
}

// projectForLLM applies D-026 heavy-content discipline to the tool
// result before it reaches the planner's next-step renderer. When the
// JSON encoding is small enough (< heavyThreshold), the projection is
// the raw value (the planner sees the full result). When it exceeds
// the threshold, the projection is a summary map referencing an
// ArtifactRef stored in the artifact store — the planner sees a small
// representation + a stable id it can mention in its Finish answer.
//
// If artifact storage is unavailable OR fails, the projection
// degrades to a truncated string preview. We log a Warn (silent
// degradation is forbidden) but do not fail the run — losing a
// tool's full result is recoverable in the planner's eyes.
func (e *devToolExecutor) projectForLLM(ctx context.Context, rc planner.RunContext, tool string, raw any) any {
	if raw == nil {
		return raw
	}
	encoded, encErr := json.Marshal(raw)
	if encErr != nil {
		// Marshaling failure: hand the planner a summary string so
		// SOMETHING lands — silent context loss is §13-forbidden.
		return map[string]any{
			"tool":  tool,
			"error": fmt.Sprintf("observation could not be JSON-encoded: %v", encErr),
		}
	}
	size := len(encoded)
	if size < e.heavyThreshold {
		return raw
	}
	// Heavy result — promote to artifact store.
	if e.artifacts == nil {
		// No artifact store wired (degraded dev stack) — truncate
		// loudly so the operator can see exactly what was elided.
		e.logger.Warn("devToolExecutor: heavy tool result without artifact store; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size))
		return heavyTruncationSummary(tool, raw, encoded, size, "")
	}
	scope := artifacts.ArtifactScope{
		TenantID:  rc.Quadruple.TenantID,
		UserID:    rc.Quadruple.UserID,
		SessionID: rc.Quadruple.SessionID,
	}
	// W6 (Phase 83x): stamp `created_at` on the Source map so the
	// Protocol wire layer's `extractCreatedAt` populates the row with
	// a real timestamp. Without this the Console renders the Go
	// zero-value `0001-01-01T00:00:00Z` for every heavy-promoted
	// artifact. The wire layer accepts a time.Time directly.
	ref, putErr := e.artifacts.PutText(ctx, scope, string(encoded), artifacts.PutOpts{
		Filename: fmt.Sprintf("tool-result-%s.json", tool),
		MimeType: "application/json",
		Source: map[string]any{
			// Phase 107f (D-176): stamp the canonical `source`
			// discriminator so artifacts.list and the session-artifact
			// manifest project a real provenance ("tool") instead of a
			// blank source. The `tool`/`producer` keys stay for the
			// originating-tool name.
			"source":     "tool",
			"producer":   "dev-tool-executor",
			"tool":       tool,
			"run_id":     rc.Quadruple.RunID,
			"created_at": time.Now().UTC(),
		},
	})
	if putErr != nil {
		e.logger.Warn("devToolExecutor: artifact PutText failed; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size),
			slog.String("err", putErr.Error()))
		return heavyTruncationSummary(tool, raw, encoded, size, "")
	}
	return heavyTruncationSummary(tool, raw, encoded, size, ref.ID)
}

// previewFieldMaxBytes caps each top-level field's serialized form in
// the field-aware preview. Fields whose serialized value exceeds this
// budget are replaced with a `[omitted: N bytes]` sentinel so they
// still appear as keys (the model sees what's available) but don't
// blow the preview budget. 1 KiB is chosen as the threshold because
// it captures normal scalar fields (numbers, short strings, small
// arrays of tags/categories) but prunes nested heavy objects like
// yt-dlp's `automatic_captions`, `formats`, `subtitles` (each
// hundreds of KB).
const previewFieldMaxBytes = 1024

// previewTotalMaxBytes is the hard cap on the entire field-aware
// preview's serialized size. Even with per-field pruning a result
// with hundreds of small scalar fields could still grow large; this
// is the back-stop.
const previewTotalMaxBytes = 16384

// heavyTruncationSummary builds the small llmObservation the
// planner renders for a heavy tool result. For JSON-object results
// it emits a field-aware preview that preserves every top-level
// scalar / small field verbatim and replaces oversized nested
// values with a `[omitted: N bytes]` sentinel — so the model sees
// both what's available and what was pruned. For non-object
// results (arrays, scalars at top level) it falls back to
// byte-truncation at `previewTotalMaxBytes`.
//
// Carries: the tool name, byte size of the full result, the
// preview, the `truncated: true` signal, and the artifact ID
// when available.
func heavyTruncationSummary(tool string, raw any, encoded []byte, size int, artifactID string) any {
	prev := buildPreview(raw, encoded)
	out := map[string]any{
		"tool":       tool,
		"size_bytes": size,
		"truncated":  true,
		"preview":    prev,
	}
	if artifactID != "" {
		out["artifact_ref"] = artifactID
	}
	return out
}

// buildPreview renders the field-aware preview when the result is a
// JSON object (after unwrapping common single-key wrappers like
// `{"result": {...}}` that MCP tools and Go structs produce), or
// falls back to byte-truncation for non-object shapes (arrays,
// scalars at top). Returns a string so the existing `preview` key
// shape on the observation map is preserved.
//
// Why the unmarshal step: `raw` here may be a typed Go struct (the
// MCP driver returns its own value type), not a `map[string]any`.
// A type assertion against `map[string]any` would miss those. The
// encoded bytes are guaranteed JSON, so we re-derive the generic
// shape from them.
//
// Why the unwrap step: MCP tools (and many in-process tools)
// wrap their payload in `{"result": <value>}` so the top-level has
// exactly one key whose value is the real data. Applying
// field-aware preview to the outer wrapper would prune `result`
// itself as oversized — defeating the whole point. The unwrap is
// bounded to one level and only fires when the outer has exactly
// one key AND that key's value is itself a map.
func buildPreview(raw any, encoded []byte) string {
	var m map[string]any
	if asMap, ok := raw.(map[string]any); ok {
		m = asMap
	} else if err := json.Unmarshal(encoded, &m); err != nil || m == nil {
		// Top-level is an array or scalar (not a JSON object) —
		// fall back to byte truncation.
		prev := string(encoded)
		if len(prev) > previewTotalMaxBytes {
			prev = prev[:previewTotalMaxBytes] + "...(truncated)"
		}
		return prev
	}

	// Unwrap a single-key wrapper one level when its value is itself
	// a map. Captures the `{"result": {<actual metadata>}}` shape MCP
	// tools emit.
	if len(m) == 1 {
		for _, v := range m {
			if inner, ok := v.(map[string]any); ok && len(inner) > 1 {
				m = inner
			}
		}
	}

	if s, ok := fieldAwarePreview(m); ok {
		return s
	}
	// Field-aware build failed — fall back to byte truncation.
	prev := string(encoded)
	if len(prev) > previewTotalMaxBytes {
		prev = prev[:previewTotalMaxBytes] + "...(truncated)"
	}
	return prev
}

// fieldAwarePreview emits a JSON object where each top-level key from
// `m` is included verbatim when its serialized form is under
// `previewFieldMaxBytes`, OR replaced with a `[omitted: N bytes]`
// sentinel string otherwise. Returns (preview, true) on success; the
// false return signals the caller to fall back to byte-truncation
// (e.g. the marshal of the assembled preview failed somehow).
func fieldAwarePreview(m map[string]any) (string, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]any, len(m))
	for _, k := range keys {
		v := m[k]
		vBytes, err := json.Marshal(v)
		if err != nil {
			out[k] = fmt.Sprintf("[unrenderable: %v]", err)
			continue
		}
		if len(vBytes) > previewFieldMaxBytes {
			out[k] = fmt.Sprintf("[omitted: %d bytes]", len(vBytes))
			continue
		}
		// Embed the parsed value (not the raw bytes) so the assembled
		// preview is one JSON document, not a JSON-string of JSON.
		out[k] = v
	}

	prevBytes, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	if len(prevBytes) > previewTotalMaxBytes {
		// Even after per-field pruning the assembled doc is too big
		// — most often happens when there are hundreds of small
		// scalar fields. Truncate the assembled doc but signal it.
		return string(prevBytes[:previewTotalMaxBytes]) + "...(truncated)", true
	}
	return string(prevBytes), true
}

// ensure interface satisfaction at compile time.
var _ steering.ToolExecutor = (*devToolExecutor)(nil)
