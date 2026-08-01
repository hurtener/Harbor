package react

import (
	"encoding/json"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// toolSearchToolName is the canonical builtin meta-tool whose results
// the React planner harvests for per-run discovered-tool surfacing
// (AC-18). The builtin lives at
// `internal/tools/builtin/tool_search.go`; its `Invoke` returns a JSON
// payload whose top-level `tools` array carries `{name, description}`
// entries. The planner reads `tools[].name` and adds each name to the
// next turn's `req.Tools` declaration so the LLM can call discovered
// surfaces natively without re-discovery.
//
// A second producer (`skill_search`) lives alongside it but does NOT
// contribute to the discovered-TOOLS set — skills are pre-prompt
// retrieval surfaces, not invokable tools.
const toolSearchToolName = "tool_search"

// deriveDiscoveredFromTrajectory walks the trajectory's executed steps
// and returns the union of tool names surfaced by every prior
// `tool_search` invocation's observation (AC-18). The function reads
// only — it never mutates the trajectory. Returns a nil slice when no
// `tool_search` step landed yet.
//
// Per-step observation shape (the `tool_search` builtin's contract):
//
//	{
//	  "tools": [
//	    {"name": "<tool-1-name>", ...},
//	    {"name": "<tool-2-name>", ...}
//	  ],
//	  ...
//	}
//
// The walker tolerates either a `map[string]any` (the typical
// dispatcher observation) or a `json.RawMessage` / `[]byte` shape
// (when the dispatcher serialised the result to bytes before storing).
// A `LLMObservation` projection is preferred over the
// raw `Observation` when both are present, matching the prompt
// renderer's heavy-content discipline.
//
// Malformed observations are ignored silently — discovery is best-
// effort, and the LLM still observed the step's content in the prior
// turn's prompt; failing the next turn over an unparseable observation
// would burn the run for no benefit.
func deriveDiscoveredFromTrajectory(t *planner.Trajectory) []string {
	if t == nil || len(t.Steps) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, step := range t.Steps {
		call, ok := step.Action.(planner.CallTool)
		if !ok || call.Tool != toolSearchToolName {
			continue
		}
		obs := step.LLMObservation
		if obs == nil {
			obs = step.Observation
		}
		names := extractDiscoveredNames(obs)
		for _, n := range names {
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// extractDiscoveredNames returns the `tools[].name` slice carried by a
// `tool_search` observation. Tolerates the three common observation
// shapes the runtime produces (typed map, JSON bytes, JSON
// RawMessage). Unknown shapes return nil.
func extractDiscoveredNames(obs any) []string {
	switch v := obs.(type) {
	case nil:
		return nil
	case map[string]any:
		return extractNamesFromMap(v)
	case json.RawMessage:
		return extractNamesFromBytes(v)
	case []byte:
		return extractNamesFromBytes(v)
	case string:
		return extractNamesFromBytes([]byte(v))
	default:
		// Best-effort: re-encode any other shape (struct, typed
		// map) and parse the bytes. This catches the
		// `inproc.publishToolOutcome` path that hands the planner a
		// struct projection rather than a generic map.
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return extractNamesFromBytes(raw)
	}
}

// extractNamesFromBytes parses a `tool_search` observation's bytes
// form. Returns nil on any parse failure (silent — see godoc).
func extractNamesFromBytes(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var shaped struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		return nil
	}
	if len(shaped.Tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(shaped.Tools))
	for _, t := range shaped.Tools {
		out = append(out, t.Name)
	}
	return out
}

// extractNamesFromMap pulls names from a `tool_search` observation
// already decoded as `map[string]any`. The `tools` key may be a
// `[]any` (each entry a map[string]any) or a `[]map[string]any`
// depending on the runtime's decoding choice.
func extractNamesFromMap(m map[string]any) []string {
	raw, ok := m["tools"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			if em, ok := entry.(map[string]any); ok {
				name, _ := em["name"].(string) //nolint:errcheck // a non-string `name` is treated as "" — the empty-string guard below skips it
				if name != "" {
					out = append(out, name)
				}
			}
		}
		return out
	case []map[string]any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			name, _ := entry["name"].(string) //nolint:errcheck // a non-string `name` is treated as "" — the empty-string guard below skips it
			if name != "" {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// mergeDiscovered returns the deduplicated union of two name slices.
// The result preserves the input order: every entry of `existing` is
// kept in place, then every entry of `derived` not already present is
// appended. A nil result for both-nil inputs.
func mergeDiscovered(existing, derived []string) []string {
	if len(existing) == 0 && len(derived) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(existing)+len(derived))
	out := make([]string, 0, len(existing)+len(derived))
	for _, n := range existing {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range derived {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// buildToolDeclarations assembles the per-turn `req.Tools` slice from
// the always-loaded catalog subset (returned by `rc.Catalog.List()`,
// already identity- and LoadingMode-filtered by the runtime's catalog
// view) plus the per-run discovered tools (AC-17 / AC-18). The
// declarations carry name + description + the args schema; the LLM
// uses them to emit native ToolCalls.
//
// Ordering is insertion-order, with the reserved-name planner controls
// (`_spawn_task` / `_await_task`) at the FRONT so the LLM always sees
// them regardless of catalog size (step 10 follow-up to
// AC-20a — Path 1 from the planner reserved-name native declaration
// gap). Then always-loaded tools (in catalog registration order),
// then discovered tools (in discovery order). A discovered name that
// already exists in the always-loaded set is skipped — duplicates would
// confuse provider-side dispatch.
//
// The `_finish` discriminator is INTENTIONALLY NOT declared: AC-20
// retires `_finish` from the prompt + the parser; models that emit a
// finish under the new shape simply produce a non-tool-calling response
// (`Content: "answer"`, `ToolCalls: []`). Declaring `_finish` would
// re-introduce a dual finish path and confuse providers' tool-choice
// heuristics.
//
// A nil `rc.Catalog` returns an empty slice; the LLM still receives
// the prompt and can emit a tool-free response (the projector then
// produces Finish{Goal} or Finish{NoPath}).
func buildToolDeclarations(rc planner.RunContext, discovered []string) []llm.ToolDeclaration {
	decls, _ := buildToolDeclarationProjection(rc, discovered)
	return decls
}

// buildToolDeclarationProjection returns both surfaces created by the same
// catalog snapshot: the declarations sent to the provider and the immutable
// declared-name-to-catalog-key projection used to interpret that provider's
// response. The caller must retain the projection across Complete; rebuilding
// it afterward could let a concurrent catalog publication retarget a name the
// model saw under a different schema.
func buildToolDeclarationProjection(rc planner.RunContext, discovered []string) ([]llm.ToolDeclaration, tools.ModelToolNameProjection) {
	if rc.Catalog == nil {
		return nil, tools.NewModelToolNameProjection(nil, tools.ReservedModelToolNames())
	}
	projected, projection := projectModelTools(rc, discovered)
	if len(projected) == 0 && len(projection.Collisions()) == 0 {
		// Even an empty catalog still gets the planner-reserved
		// controls — they're how the LLM signals "spawn a side task"
		// or "await a previously-spawned task" under native tool-calling.
		return reservedPlannerControlDeclarations(), projection
	}
	reserved := reservedPlannerControlDeclarations()
	decls := make([]llm.ToolDeclaration, 0, len(reserved)+len(projected))
	decls = append(decls, reserved...)
	for _, t := range projected {
		decls = append(decls, toolToDeclaration(t))
	}
	for _, collision := range projection.Collisions() {
		emitToolDeclarationCollision(rc, collision.DeclaredName, collision.DeclaredTool, collision.DroppedTool)
	}
	return decls, projection
}

// projectModelTools is the ONE ReAct projection of catalog candidates onto
// the model-visible namespace. Declarations and the prompt quick-reference
// call it; Next retains its returned immutable projection across Complete for
// provider-returned-name resolution, so collision winner and ordering cannot
// drift within a turn.
func projectModelTools(rc planner.RunContext, discovered []string) ([]tools.Tool, tools.ModelToolNameProjection) {
	if rc.Catalog == nil {
		return nil, tools.NewModelToolNameProjection(nil, nil)
	}
	candidates := append([]tools.Tool(nil), rc.Catalog.List()...)
	for _, name := range discovered {
		if name == "" {
			continue
		}
		if tool, ok := rc.Catalog.Resolve(name); ok {
			candidates = append(candidates, tool)
		}
	}
	names := make([]string, 0, len(candidates))
	byName := make(map[string]tools.Tool, len(candidates))
	for _, tool := range candidates {
		names = append(names, tool.Name)
		if _, exists := byName[tool.Name]; !exists {
			byName[tool.Name] = tool
		}
	}
	projection := tools.NewModelToolNameProjection(names, tools.ReservedModelToolNames())
	projected := make([]tools.Tool, 0, len(projection.Entries()))
	for _, entry := range projection.Entries() {
		projected = append(projected, byName[entry.CatalogName])
	}
	return projected, projection
}

// emitToolDeclarationCollision announces a tool that could not be declared
// because another catalog tool already claimed its provider-safe function
// name. It is a no-op when `declared` and `dropped` name the SAME catalog
// tool — that is the benign re-encounter (discovery re-surfacing an
// already-loaded tool), not a lost surface.
//
// A nil Emit closure (tests without observability) makes this a no-op, per
// the RunContext.Emit contract; production always wires it.
func emitToolDeclarationCollision(rc planner.RunContext, declaredName, declared, dropped string) {
	if declared == dropped || rc.Emit == nil {
		return
	}
	now := time.Now()
	if rc.Clock != nil {
		now = rc.Clock()
	}
	rc.Emit(events.Event{
		Type:       planner.EventTypePlannerToolDeclarationCollision,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: planner.ToolDeclarationCollisionPayload{
			Identity:     rc.Quadruple,
			DeclaredName: declaredName,
			DeclaredTool: declared,
			DroppedTool:  dropped,
			OccurredAt:   now,
		},
	})
}

// reservedPlannerControlDeclarations returns the synthetic
// `llm.ToolDeclaration` entries for the React planner's reserved
// control names (`_spawn_task` / `_await_task` / `_task_status` /
// `_cancel_task` / `_steer_task` / `_pause_task` / `_resume_task`). The
// schemas mirror the projector's `translateNativeSpawn` /
// `translateNativeAwait` / `translateNativeTaskStatus` /
// `translateNativeCancelTask` / `translateNativeSteerTask` /
// `translateNativePauseTask` / `translateNativeResumeTask` args
// envelopes verbatim. The task observation/cancel/steer/pause/resume
// controls are descendant-scoped at dispatch (a run reaches only its own
// spawned tasks) — the declaration descriptions state that honestly so
// the model never expects to reach a sibling run's tasks, and that the
// operator always supersedes.
//
// Background — the AC-20a follow-up. Step 9 of An earlier phase shipped the
// React projector's reserved-name interception (`_finish` /
// `_spawn_task` / `_await_task` in `resp.ToolCalls[0].Name` are
// translated to planner.Finish / SpawnTask / AwaitTask). The projector
// works under scripted mocks because mocks bypass provider validation;
// real providers (OpenAI / Anthropic / Gemini) REJECT undeclared
// tool_call names. Without these synthetic declarations the
// conformance pack was mocks-only and any task-spawning agent would
// fail on the next live workload.
//
// Path 1 from the step 10 plan: declare them. `_finish` stays
// undeclared (AC-20 retired finish as a tool-call shape).
//
// The descriptions deliberately frame these as "planner controls" so
// the LLM doesn't confuse them with operator-supplied catalog tools.
func reservedPlannerControlDeclarations() []llm.ToolDeclaration {
	return []llm.ToolDeclaration{
		{
			Name:        SpawnTaskToolName,
			Description: "Planner control — spawn a background task the foreground turn does not wait on. You MAY call it alongside catalog tools and alongside other _spawn_task calls in the same response; the runtime dispatches each concurrently and returns a task_id per spawn. When you batch a _spawn_task with ANY other call in the same response it is ALWAYS non-blocking — do NOT set retain_turn:true in a multi-call response (the runtime rejects the whole response). To block on a spawn instead, emit that single _spawn_task ALONE in its own response with retain_turn:true. To wait on an already-spawned task's result, send _await_task with its task_id in a LATER response, on its own. Set spec.propagate_on_cancel:\"isolate\" to let this task survive YOUR OWN later cancellation (including a cascade from a task you spawned it under); it never survives a direct cancel by the operator, who can always stop any task. Omit it (or use \"cascade\") for the default, where cancelling a parent sweeps this task too.",
			Schema:      jsonSchemaRawSpawnTask,
		},
		{
			Name:        AwaitTaskToolName,
			Description: "Planner control — block the foreground turn on a previously-spawned task's completion. Send it ALONE (never alongside any other tool call in the same response): pass the task_id returned by an earlier _spawn_task. The runtime resumes the planner with the task's resolved outcome.",
			Schema:      jsonSchemaRawAwaitTask,
		},
		{
			Name:        TaskStatusToolName,
			Description: "Planner control — check the status of the background tasks THIS run spawned (directly or transitively). Send it ALONE (never alongside any other tool call in the same response). Pass task_ids to report specific tasks, or omit it to list every task your run has spawned, including nested descendants. You can only see tasks your OWN run spawned — never another run's tasks, even in the same session. Each row is {task_id, status, description, group_id}. Use it before deciding whether to _await_task a spawn or _cancel_task the ones you no longer need.",
			Schema:      jsonSchemaRawTaskStatus,
		},
		{
			Name:        CancelTaskToolName,
			Description: "Planner control — cancel one background task THIS run spawned (e.g. abandon the losing branches of a fan-out once the first answered). Send it ALONE (never alongside any other tool call in the same response). Pass the task_id (required) and an optional reason. You can only cancel tasks your OWN run spawned, directly or transitively — never another run's tasks. Cancelling a task you spawned always works, even one you marked propagate_on_cancel:\"isolate\" — isolate only detaches a task from YOUR cancellation of an ANCESTOR, never from a direct cancel of that task itself.",
			Schema:      jsonSchemaRawCancelTask,
		},
		{
			Name:        SteerTaskToolName,
			Description: "Planner control — steer one background task THIS run spawned by sending it a free-text directive it will see at its next step, without waiting on or cancelling it. Send it ALONE (never alongside any other tool call in the same response). Pass the task_id (required) and a directive (required). You can only steer tasks your OWN run spawned, directly or transitively — never another run's tasks, even in the same session. The operator can always steer any task and overrides you. Returns {task_id, steered}; steering a task that has already finished returns steered:false (not an error).",
			Schema:      jsonSchemaRawSteerTask,
		},
		{
			Name:        PauseTaskToolName,
			Description: "Planner control — pause one background task THIS run spawned so you can resume it later; it parks at its next step boundary through the runtime's pause/resume primitive. Send it ALONE (never alongside any other tool call in the same response). Pass the task_id (required) and an optional reason. Pausing a descendant NEVER pauses your own turn — only the named task parks. You can only pause tasks your OWN run spawned, directly or transitively — never another run's tasks. The operator can always pause any task and overrides you. A paused descendant only continues via _resume_task or an operator resume. Returns {task_id, paused}: paused is true when the pause was delivered to a still-running task, and false only when the task has already finished. A redundant pause of an already-paused task is harmless (the runtime parks it once).",
			Schema:      jsonSchemaRawPauseTask,
		},
		{
			Name:        ResumeTaskToolName,
			Description: "Planner control — resume one background task THIS run previously paused, through the same pause/resume primitive _pause_task parked it with. Send it ALONE (never alongside any other tool call in the same response). Pass the task_id (required) and an optional directive to inject as it continues. You can only resume tasks your OWN run spawned, directly or transitively — never another run's tasks. The operator can always resume any task and overrides you. Returns {task_id, resumed}: resumed is true when the resume was delivered to a still-running task, and false only when the task has already finished. Only ever resume a task you actually paused: resuming one that is not paused makes the runtime treat it as a spurious resume and STOPS that task (the same effect an operator's mistaken resume has) — resumed:true means the control was delivered, not that a valid pause was released.",
			Schema:      jsonSchemaRawResumeTask,
		},
	}
}

// jsonSchemaRawSpawnTask / jsonSchemaRawAwaitTask are the JSON-Schema
// representations of the `_spawn_task` / `_await_task` args envelopes
// the projector accepts (see translateNativeSpawn / translateNativeAwait).
//
// Provider-compatibility considerations:
//   - OpenAI strict-mode tool-calling rejects schemas missing
//     `additionalProperties: false` at every object level. Both
//     schemas pin it explicitly.
//   - Anthropic accepts looser schemas; the explicit
//     `additionalProperties: false` is harmless there.
//   - Gemini follows OpenAPI 3.0; the JSON-Schema dialect maps cleanly.
//
// Bytes literals (not Go-side maps) so the schema is opaque to the
// bifrost translator — the wire bytes pass through verbatim, matching
// the inproc-driver deriver's output shape (which also emits raw
// JSON-Schema bytes via `json.Marshal(schema)`).
var (
	jsonSchemaRawSpawnTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "kind": {
      "type": "string",
      "enum": ["foreground", "background"],
      "description": "foreground holds the planner's turn until completion; background returns control immediately (default)."
    },
    "group_id": {
      "type": "string",
      "description": "Optional logical group id. Tasks sharing a group_id can be joined as a unit via the runtime's group-completion delivery."
    },
    "spec": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "description": {"type": "string"},
        "query": {"type": "string"},
        "priority": {"type": "integer"},
        "retain_turn": {"type": "boolean"},
        "fail_fast": {"type": "boolean"},
        "propagate_on_cancel": {
          "type": "string",
          "enum": ["cascade", "isolate"],
          "description": "cascade (default; omit for this): cancelling a parent task sweeps this task too. isolate: this task survives YOUR cancellation of an ancestor, but never a direct cancel by the operator."
        }
      }
    }
  },
  "required": ["spec"]
}`)
	jsonSchemaRawAwaitTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The id returned by a prior _spawn_task call."
    }
  },
  "required": ["task_id"]
}`)
	jsonSchemaRawTaskStatus = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_ids": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Task ids to report on. Omit or leave empty to list every task this run has spawned, including nested descendants. Every id must be a task your own run spawned; an out-of-scope id fails the whole call."
    }
  }
}`)
	jsonSchemaRawCancelTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The id of a task this run spawned (directly or transitively) to cancel."
    },
    "reason": {
      "type": "string",
      "description": "Optional human-readable cancellation reason, recorded on the task.cancelled event."
    }
  },
  "required": ["task_id"]
}`)
	jsonSchemaRawSteerTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The id of a task this run spawned (directly or transitively) to steer."
    },
    "directive": {
      "type": "string",
      "description": "The free-text steering guidance enqueued onto the task's steering inbox; it sees this at its next step."
    }
  },
  "required": ["task_id", "directive"]
}`)
	jsonSchemaRawPauseTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The id of a task this run spawned (directly or transitively) to pause. Pausing it never pauses your own turn."
    },
    "reason": {
      "type": "string",
      "description": "Optional human-readable pause reason, carried onto the task's pause record."
    }
  },
  "required": ["task_id"]
}`)
	jsonSchemaRawResumeTask = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The id of a paused task this run spawned (directly or transitively) to resume."
    },
    "directive": {
      "type": "string",
      "description": "Optional steering guidance injected as the task continues on resume."
    }
  },
  "required": ["task_id"]
}`)
)

// toolToDeclaration projects a `tools.Tool` view onto the wire-facing
// `llm.ToolDeclaration`. Carries name + description + the args JSON
// Schema verbatim — the bifrost translator (and downstream provider
// adapters) consume this shape directly.
func toolToDeclaration(t tools.Tool) llm.ToolDeclaration {
	return llm.ToolDeclaration{
		// Sanitize the catalog name to the provider-safe form native
		// tool-calling requires; the projector reverses it on dispatch.
		Name:        sanitizeToolName(t.Name),
		Description: t.Description,
		Schema:      t.ArgsSchema,
	}
}
