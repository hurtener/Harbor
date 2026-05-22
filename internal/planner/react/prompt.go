package react

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// defaultBuilder is the in-package [PromptBuilder]. It produces a
// conversation whose shape depends on whether the trajectory has been
// compacted (Phase 46):
//
//  1. System message: the twelve XML-tagged sections (brief 13 §2.1)
//     assembled by [buildSystemContent] — `<identity>`,
//     `<output_format>`, `<action_schema>`, `<finishing>`,
//     `<tool_usage>`, `<parallel_execution>`, `<reasoning>`, `<tone>`,
//     `<error_handling>`, `<available_tools>`, `<additional_guidance>`,
//     `<planning_constraints>` — in that fixed order, separated by
//     `\n\n`. Optional sections (`<additional_guidance>`,
//     `<planning_constraints>`) are omitted entirely when empty.
//  2. User message: the run's Goal (or Query when Goal is empty),
//     followed by — when rc.Trajectory.Summary is non-nil — a single
//     compacted block that lists the summary's Goals / Facts /
//     Pending / LastOutputDigest / Note fields.
//  3. **Trajectory rendering — Phase 46 contract (D-055):**
//     - When `rc.Trajectory.Summary == nil`: render each completed
//     Step as an assistant turn (the prior planner action as JSON) +
//     a user turn (the rendered observation, preferring
//     LLMObservation over raw Observation per D-026 heavy-content
//     discipline).
//     - When `rc.Trajectory.Summary != nil`: SKIP the per-step loop.
//     The summary block in the user message (block 2) IS the
//     trajectory representation. Brief 02 §4: "The compressed
//     digest replaces the raw step history in subsequent prompt
//     builds." Rendering both would double-count tokens and defeat
//     the compression.
//  4. Optional background-task outcomes block: resolved
//     [planner.BackgroundResult] entries surface as a final user
//     message (the D-032 push-wake seam). Renders independently of
//     compaction.
//
// `extraGuidance` is operator-supplied content (Phase 83a — set by
// [WithSystemPromptExtra] / `PlannerConfig.ExtraGuidance`) injected
// into the `<additional_guidance>` section. Empty → the section is
// omitted entirely (unless Phase 83c repair guidance fills it).
//
// **Dynamic-augmentation pass — Phase 83c contract (D-145).** Build
// merges two runtime-supplied, per-turn surfaces into the otherwise
// static twelve-section layout:
//
//   - `<additional_guidance>` (section 11) — below the operator
//     content, the builder appends escalating repair guidance
//     (`reminder → warning → critical`) for each non-zero
//     `RunContext.RepairCounters` field. This closes the across-step
//     feedback loop Phase 44's per-step repair leaves open (brief 13
//     §2.2). One [planner.EventTypePlannerRepairGuidanceInjected]
//     event is emitted per rendered block.
//   - `<planning_constraints>` (section 12) — rendered from
//     `RunContext.PlanningHints`; omitted entirely when nil / empty.
//
// Both surfaces are read from `rc`; the builder still MUST NOT mutate
// `rc`. The counters live on the per-run `rc`, never on the builder
// or planner struct (D-145 + D-025).
//
// **Reasoning replay — Phase 83e contract (D-148).** The builder
// resolves the effective [planner.ReasoningReplayMode] via
// [planner.EffectiveReasoningReplay] — the per-run
// `RunContext.ReasoningReplay` override wins over the agent-configured
// `configuredReplay`. When the resolved mode is
// [planner.ReasoningReplayText], a prior step's captured
// `ReasoningTrace` is prepended as a text block ABOVE the prior
// `{tool, args}` action JSON in the assistant turn. When the mode is
// [planner.ReasoningReplayNever] (the default for ALL models), only
// the `{tool, args}` JSON is rendered — captured reasoning is never
// re-injected. An empty trace produces no prepended block regardless
// of mode.
//
// The builder reads from rc; it MUST NOT mutate rc. The result is
// always safe to discard / re-build per call — the builder is
// stateless. All fields are set at construction; a `defaultBuilder`
// value is immutable thereafter, so it satisfies the D-025 concurrent-
// reuse contract trivially (no mutable state, no locks needed).
type defaultBuilder struct {
	// extraGuidance is operator-supplied domain-specific guidance.
	// Rendered verbatim into the <additional_guidance> section.
	// Empty string → the section is omitted from the prompt.
	extraGuidance string
	// configuredReplay is the agent-configured reasoning-replay mode
	// (from config.PlannerConfig.ReasoningReplay). The per-run
	// RunContext.ReasoningReplay override wins over it at render time.
	// Zero value ("" → resolves to never) is the safe default.
	configuredReplay planner.ReasoningReplayMode
	// maxToolExamples caps how many curated examples each tool renders
	// in the <available_tools> section (Phase 83b — D-144). Zero
	// resolves to defaultMaxToolExamples (3) at render time. Set once
	// at construction by [New]; read-only thereafter (D-025).
	maxToolExamples int
}

// defaultMaxToolExamples is the per-tool example cap applied when the
// operator leaves `PlannerConfig.MaxToolExamplesPerTool` at its zero
// value (Phase 83b — D-144, brief 13 §2.4).
const defaultMaxToolExamples = 3

// toolRenderConfig carries the per-render knobs [renderTool] consults.
// It is a value type — [renderTool] is pure with respect to it, which
// keeps the helper trivially safe for concurrent reuse (D-025).
type toolRenderConfig struct {
	// maxExamples is the resolved (non-zero) per-tool example cap.
	maxExamples int
}

// Build implements [PromptBuilder].
//
// Build cannot return an error (the [PromptBuilder] interface is fixed
// — D-146 keeps the signature). Memory / skills injection
// (Phase 83d) CAN fail loudly when a `RunContext.MemoryBlocks` tier or
// a `SkillsContext` entry is not JSON-serialisable. The ReAct planner
// therefore drives the default builder via [defaultBuilder.buildRequest]
// — the error-returning worker — and surfaces
// [planner.ErrMemoryBlockUnserializable] from `Next`. This `Build`
// method exists for the [PromptBuilder] contract and for operator-
// supplied builders that wrap the default; when a memory tier is
// unserialisable it returns a request WITHOUT the offending injection
// rather than silently corrupting the prompt — but the planner never
// reaches this path because it calls `buildRequest` directly and
// aborts on the error first.
func (b defaultBuilder) Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest {
	req, err := b.buildRequest(rc, systemPrompt)
	if err != nil {
		// Unreachable via ReActPlanner.Next (it calls buildRequest and
		// aborts on error). Reachable only if an operator calls Build
		// directly with an unserialisable MemoryBlocks. Render the
		// base prompt minus the broken injection — the planner-side
		// path is the fail-loud one; this is the interface-contract
		// fallback for the rare direct caller.
		return b.baseRequest(rc, systemPrompt)
	}
	return req
}

// buildRequest is the error-returning worker behind [Build]. The ReAct
// planner calls it directly so memory / skills serialisation failures
// surface loudly as [planner.ErrMemoryBlockUnserializable] from `Next`
// (D-146 — fail-loud, never a silently dropped memory tier).
func (b defaultBuilder) buildRequest(rc planner.RunContext, systemPrompt string) (llm.CompleteRequest, error) {
	req := b.baseRequest(rc, systemPrompt)

	// Phase 83d (D-146): memory + skills injection. The wrappers are
	// emitted as SEPARATE system-role messages immediately after the
	// base twelve-section system message — NOT concatenated into it —
	// so Console traces and debugging tools can isolate each tier.
	// Order: external memory → conversation memory → skills_context.
	injection, err := renderInjectionMessages(rc)
	if err != nil {
		return llm.CompleteRequest{}, err
	}
	if len(injection) > 0 {
		// Splice the injection messages between the base system
		// message (index 0) and the user / trajectory messages.
		spliced := make([]llm.ChatMessage, 0, len(req.Messages)+len(injection))
		spliced = append(spliced, req.Messages[0])
		spliced = append(spliced, injection...)
		spliced = append(spliced, req.Messages[1:]...)
		req.Messages = spliced
	}
	return req, nil
}

// baseRequest builds the request WITHOUT the Phase 83d memory / skills
// injection — the twelve-section system message, the user block, and
// the trajectory replay. [buildRequest] splices the injection messages
// in afterwards.
func (b defaultBuilder) baseRequest(rc planner.RunContext, systemPrompt string) llm.CompleteRequest {
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}

	var messages []llm.ChatMessage

	// 1. System block: the twelve XML-tagged sections.
	sysContent := buildSystemContent(systemPrompt, b.extraGuidance, b.maxToolExamples, rc)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleSystem,
		Content: textContent(sysContent),
	})

	// Phase 83c (D-145): emit one planner.repair_guidance_injected
	// event per escalating guidance block merged into
	// <additional_guidance> this turn. The emit reflects exactly what
	// the LLM will see — a nil RepairCounters / all-zero counters is a
	// no-op. Best-effort; a nil rc.Emit (tests without observability)
	// is a no-op.
	emitRepairGuidanceInjected(rc)

	// 2. User block: goal/query + optional summary.
	userContent := buildUserContent(rc)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleUser,
		Content: textContent(userContent),
	})

	// 3. Trajectory rendering. Phase 46 contract (D-055): when
	// rc.Trajectory.Summary is non-nil, SKIP the per-step assistant +
	// user pair loop. The compacted summary in the user block above is
	// the trajectory representation; rendering both would double-count
	// tokens and defeat the compression (brief 02 §4: "The compressed
	// digest replaces the raw step history in subsequent prompt
	// builds."). When Summary is nil, render the raw step history as
	// before (the Phase 45 V1 minimum-viable shape).
	if rc.Trajectory != nil {
		if rc.Trajectory.Summary == nil {
			replayMode := planner.EffectiveReasoningReplay(rc, b.configuredReplay)
			for _, step := range rc.Trajectory.Steps {
				asst := renderAssistantTurn(step, replayMode)
				obs := renderObservationForLLM(step)
				if asst != "" {
					messages = append(messages, llm.ChatMessage{
						Role:    llm.RoleAssistant,
						Content: textContent(asst),
					})
				}
				if obs != "" {
					messages = append(messages, llm.ChatMessage{
						Role:    llm.RoleUser,
						Content: textContent(obs),
					})
				}
			}
		}
		// Optional: emit any resolved background-task outcomes (push
		// wake — D-032 / Phase 45 spec) as a final user message so
		// the planner sees them on the very next step. This block
		// fires regardless of compaction — background outcomes are
		// the latest signal the planner has and must reach it on the
		// NEXT step. (A future phase MAY route them through the
		// summariser; Phase 46 keeps them as a separate trailing
		// user turn.)
		if len(rc.Trajectory.Background) > 0 {
			if bg := renderBackground(rc.Trajectory.Background); bg != "" {
				messages = append(messages, llm.ChatMessage{
					Role:    llm.RoleUser,
					Content: textContent(bg),
				})
			}
		}
	}

	return llm.CompleteRequest{
		Messages: messages,
	}
}

// The twelve static section bodies. Brief 13 §4 carries the verbatim
// adapted copy; the constants below are that copy, split at the XML
// tag boundaries so each section is independently editable (brief 13
// §2.1 design property 1). Phases 83b/c/d extend these section anchors:
//
//   - 83b replaces sectionAvailableToolsTag's body with per-tool
//     `args_schema` + curated examples.
//   - 83c populates the <planning_constraints> section from
//     `RunContext.PlanningHints` and merges per-turn repair guidance
//     into <additional_guidance> (D-145 — done).
//   - 83d injects `<read_only_*_memory>` UNTRUSTED-framed blocks.
//
// The `{{current_date}}` placeholder in <identity> is the ONLY
// template marker the builder resolves at Build time; everything else
// is static text. (Smoke phase-83a.sh asserts no other `{{` markers
// survive into the golden fixture.)
const (
	// sectionIdentityTemplate carries a `{{current_date}}` marker the
	// builder resolves per-call. Date-only (no time-of-day) keeps the
	// prompt stable across a session for KV-cache hit rates (brief 13
	// §4 note on `{{current_date}}`).
	sectionIdentityTemplate = `<identity>
You are an autonomous reasoning agent that solves tasks by selecting and orchestrating tools.
Your name and voice on how to answer will come at the end of the prompt in additional_guidance.

Your role is to:
- Understand the user's intent and break complex queries into actionable steps
- Select appropriate tools from your catalog to gather information or perform actions
- Synthesize observations into clear, accurate answers
- Know when you have enough information to answer and when you need more

Current date: {{current_date}}
</identity>`

	sectionOutputFormat = `<output_format>
Think briefly (internally), then respond with a single JSON object that matches the action schema.
If a tool would help, set "tool" to the tool name and provide "args".
Write your JSON inside one markdown code block (` + "```json ... ```" + `).
Do not emit multiple JSON objects or extra commentary after the code block.

Important:
- Emit keys in this order for stability: tool, args.
- User-facing answers go ONLY in args.answer when tool is "_finish" (finished).
- During intermediate steps (when calling tools), the user sees nothing; only tool outputs are recorded internally.
</output_format>`

	sectionActionSchema = `<action_schema>
Every response follows this structure:

{
  "tool": "tool_name" | "parallel" | "_spawn_task" | "_await_task" | "_finish",
  "args": { ... }
}

Field meanings:
- tool:
  - Tool call: a tool name from the catalog. If a tool was returned by ` + "`skill_search`" + ` but is deferred/hidden,
    you may still call it by name; the runtime will activate it on first call.
  - Parallel: "parallel" (executes tools concurrently)
  - Background tasks: "_spawn_task" or "_await_task" (spawns or joins a task)
  - Terminal: "_finish" (streams args.answer to the user)
- args:
  - Tool call: tool arguments matching args_schema
  - Parallel: {"steps": [{"tool": "...", "args": {...}}, ...], "join": {...} | null}
  - Task: see examples below
  - Final: {"answer": "..."} — plain text only; no metadata fields.

Background task examples (use only when task management is enabled):

Example - _spawn_task (for complex reasoning tasks):
{
  "tool": "_spawn_task",
  "args": {
    "name": "Research market trends",
    "query": "Analyze Q4 2024 market trends and provide a summary",
    "merge_strategy": "HUMAN_GATED",
    "group": "analysis",
    "retain_turn": false
  }
}

Example - _await_task (join a previously-spawned task):
{
  "tool": "_await_task",
  "args": { "task_id": "tsk_abc123" }
}

Args schema for task actions:
- name: Human-readable task name (for _spawn_task)
- query: The task instruction (for _spawn_task)
- merge_strategy: "HUMAN_GATED" (default), "APPEND", or "REPLACE"
- group: Optional group name for coordinated tasks
- group_sealed: true to seal the group (no more tasks can join)
- retain_turn: true to wait for result (requires APPEND/REPLACE merge)

Remember: The ONLY place for user-facing text is args.answer when tool is "_finish".
</action_schema>`

	sectionFinishing = `<finishing>
When you have gathered enough information to answer the query:

1. Set "tool" to "_finish"
2. Provide "args" with this structure:

{
  "answer": "Your complete, human-readable answer to the user's query"
}

The answer field is REQUIRED and is the ONLY field. Write a full, helpful response — not a summary or fragment.
Focus on solving the user query, going to the point of answering what they asked.

` + "`answer`" + ` is plain text. Do NOT include structured metadata, status flags, confidence scores, or
classification routes — Harbor's renderer is responsible for any structured presentation, not the planner.
If rich UI is needed (cards, charts, structured layouts), call the appropriate MCP-Apps rendering tool
BEFORE you finish, and reference the rendered artifact in ` + "`answer`" + ` as ordinary prose.

Do NOT include heavy data (charts, files, large JSON) in args — artifacts from tool outputs are collected automatically.

Example finish:
{
  "tool": "_finish",
  "args": {
    "answer": "Q4 2024 revenue increased 15% YoY to $1.2M. December was strongest."
  }
}
</finishing>`

	sectionToolUsage = `<tool_usage>
Rules for using tools:

1. Only use tools listed in the catalog below - never invent tool names
2. Match your args to the tool's args_schema exactly
3. Consider side_effects before calling:
   - "pure": Safe to call multiple times, no external changes
   - "read": Reads external data but doesn't modify anything
   - "write": Modifies external state - use carefully
   - "external": Calls external services - may have rate limits or costs
4. Use the tool's description to understand when it's appropriate
5. If a tool fails, consider alternative approaches before giving up
</tool_usage>`

	sectionParallelExecution = `<parallel_execution>
For tasks that benefit from concurrent execution, use parallel plans:

{
  "tool": "parallel",
  "args": {
    "steps": [
      {"tool": "tool_a", "args": {...}},
      {"tool": "tool_b", "args": {...}}
    ],
    "join": {
      "tool": "aggregator_tool",
      "args": {},
      "inject": {"results": "$results", "count": "$success_count"}
    }
  }
}

Available injection sources for args.join.inject:
- $results: List of successful outputs
- $branches: Full branch details with tool names
- $failures: List of failed branches with errors
- $success_count: Number of successful branches
- $failure_count: Number of failed branches
- $expect: Expected number of branches

Use parallel execution when:
- Multiple independent data sources need to be queried
- Multiple independent queries can be made to the same source in parallel
- Breakdown of multiple independent queries is more efficient than sequential calls
- A single query seems too difficult to answer directly and several simpler queries can help
- Tasks can be decomposed into non-dependent subtasks
- Speed matters and tools don't have ordering dependencies
</parallel_execution>`

	sectionReasoning = `<reasoning>
Approach problems systematically:

1. Understand first: Parse the query to identify what's actually being asked
2. Plan before acting: Consider which tools will help and in what order
3. Gather evidence: Use tools to collect relevant information
4. Synthesize: Combine observations into a coherent answer (in args.answer when done)
5. Verify: Check if your answer actually addresses the query

When uncertain:
- If you lack information to answer confidently, note it in your final answer
- If multiple interpretations exist, address the most likely one and note alternatives in the final answer
- If a tool fails, try alternatives - explain in the final answer only when finished
- If you cannot complete the task, explain why in the final answer when finished

Avoid:
- Making up information not supported by tool observations
- Calling the same tool repeatedly with identical arguments
- Ignoring errors or unexpected results
- Writing user-facing text during intermediate steps (save it for args.answer)
- Generating "preview" answers before you're done gathering information
</reasoning>`

	sectionTone = `<tone>
In your answer (ONLY when tool is "_finish"):
- Be direct and informative — get to the point
- Use clear, professional language
- Acknowledge limitations honestly rather than hedging excessively
- Match the formality level to the query (technical queries get technical answers)
- Avoid unnecessary caveats, but do note important limitations
- Don't apologize unless you've actually made an error
- These are safe defaults. Your tone or voice can be changed in the additional_guidance section.
- You can use markdown formatting if suggested in additional_guidance.

CRITICAL:
- During intermediate steps, produce ONLY the JSON action object. Do not add commentary.
- Do not include a 'thought' or 'reasoning' field in the JSON. Internal reasoning is captured
  by the runtime through provider-side channels when the provider exposes one; you do not need
  to echo it as part of your structured output.
</tone>`

	sectionErrorHandling = `<error_handling>
When things go wrong:

Tool validation error: Fix your args to match the schema and retry
Tool execution error: Note the error, try alternative tools or approaches
No suitable tools: Explain what you cannot do and why
Ambiguous query: Make reasonable assumptions and note them, or ask for clarification
Conflicting information: Acknowledge the conflict and explain your reasoning

If you cannot complete the task after reasonable attempts:
- Explain what you tried and why it didn't work in args.answer
- Suggest what additional information or tools would help
- If you need clarification from the user before you can proceed, ask for it directly in args.answer
  (Harbor surfaces your answer as the next user-visible turn; a follow-up question is a valid finish)
</error_handling>`
)

// buildSystemContent assembles the twelve XML-tagged sections (brief
// 13 §2.1) in their fixed order, separated by `\n\n`.
//
//  1. <identity>             — role framing + current date.
//  2. <output_format>        — one JSON object, one code block.
//  3. <action_schema>        — the {tool, args} envelope.
//  4. <finishing>            — terminal condition; only args.answer.
//  5. <tool_usage>           — side_effects taxonomy + invocation rules.
//  6. <parallel_execution>   — parallel plan schema + injection sources.
//  7. <reasoning>            — 5-step systematic approach.
//  8. <tone>                 — voice defaults + the CRITICAL clamp.
//  9. <error_handling>       — recovery framing; no requires_followup.
//  10. <available_tools>      — rendered tool catalog (per-tool).
//  11. <additional_guidance>  — operator-supplied content + Phase 83c
//     per-turn repair guidance. OMITTED only when BOTH are empty.
//  12. <planning_constraints> — runtime-supplied PlanningHints
//     (Phase 83c). OMITTED when nil / empty.
//
// `systemPrompt` is the legacy override surface ([WithSystemPrompt]):
// when an operator passes a non-default string it REPLACES the entire
// twelve-section structure (the structured sections are
// [DefaultSystemPrompt]'s content). `extraGuidance` flows into section
// 11. Sections 11 and 12 are omitted entirely — not emitted as empty
// tag pairs — when their content is absent.
//
// `maxToolExamples` is the per-tool curated-example cap (Phase 83b —
// D-144); zero resolves to [defaultMaxToolExamples].
//
// Phase 83a establishes the section anchors; 83b/c/d build on them.
func buildSystemContent(systemPrompt, extraGuidance string, maxToolExamples int, rc planner.RunContext) string {
	// When the operator overrode the prompt via WithSystemPrompt with a
	// non-default string, honour the override verbatim as the leading
	// content — the structured sections ARE the default; an explicit
	// override is the operator's deliberate replacement. The optional
	// injection sections (available_tools / additional_guidance /
	// planning_constraints) still append so tool rendering and operator
	// guidance survive a custom base prompt.
	var sections []string
	if systemPrompt == DefaultSystemPrompt {
		sections = []string{
			renderIdentitySection(),
			sectionOutputFormat,
			sectionActionSchema,
			sectionFinishing,
			sectionToolUsage,
			sectionParallelExecution,
			sectionReasoning,
			sectionTone,
			sectionErrorHandling,
		}
	} else {
		sections = []string{systemPrompt}
	}

	// Section 10: <available_tools> — always present (renders a
	// "no tools" marker when the catalog is empty). Phase 83b: the cap
	// is threaded from the builder so each tool's curated examples are
	// bounded; the builder value carries the resolved knob.
	sections = append(sections, renderAvailableToolsSection(rc, maxToolExamples))

	// Section 11: <additional_guidance> — operator-supplied guidance
	// PLUS the Phase 83c per-turn repair guidance (D-145). The repair
	// guidance is the across-step feedback loop: when a
	// `RunContext.RepairCounters` field is non-zero, an escalating
	// `reminder → warning → critical` block is merged below the
	// operator content for THIS TURN ONLY. The section is omitted
	// entirely only when BOTH are empty.
	if g := buildAdditionalGuidance(extraGuidance, rc); g != "" {
		sections = append(sections, "<additional_guidance>\n"+g+"\n</additional_guidance>")
	}

	// Section 12: <planning_constraints> — runtime-supplied planning
	// hints (Phase 83c — D-145). Rendered from `RunContext.PlanningHints`
	// and omitted entirely when nil / empty (brief 13 §2.1: optional
	// sections are omitted, never emitted as empty tag pairs).
	if hints := renderPlanningConstraints(rc); hints != "" {
		sections = append(sections, hints)
	}

	return strings.Join(sections, "\n\n")
}

// renderIdentitySection resolves the `{{current_date}}` marker in the
// <identity> section to today's UTC date in `YYYY-MM-DD` form. Date-
// only is deliberate (brief 13 §4): the value stays stable across a
// session, which helps KV-cache hit rates. No time-of-day component.
func renderIdentitySection() string {
	date := time.Now().UTC().Format("2006-01-02")
	return strings.ReplaceAll(sectionIdentityTemplate, "{{current_date}}", date)
}

// renderAvailableToolsSection renders the <available_tools> section
// (brief 13 §2.1 section 10). Phase 83b upgrades the Phase 45
// name+description-only shape: each tool now renders `name`,
// `description`, `args_schema` (compact one-line JSON), `side_effects`,
// and up to `maxToolExamples` curated examples — closing the
// args-validation-failure cascade caused by the LLM guessing argument
// shapes (brief 13 §2.4 + §3, D-144). `maxToolExamples` ≤ 0 resolves
// to [defaultMaxToolExamples].
func renderAvailableToolsSection(rc planner.RunContext, maxToolExamples int) string {
	cfg := toolRenderConfig{maxExamples: maxToolExamples}
	if cfg.maxExamples <= 0 {
		cfg.maxExamples = defaultMaxToolExamples
	}

	var b strings.Builder
	b.WriteString("<available_tools>\n")

	catalog := listTools(rc)
	if len(catalog) == 0 {
		b.WriteString("(no tools registered for this run)\n")
	} else {
		for i, t := range catalog {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(renderTool(t, cfg))
		}
	}
	b.WriteString("</available_tools>")
	return b.String()
}

// buildAdditionalGuidance composes the `<additional_guidance>` section
// body (Phase 83c — D-145): operator-supplied guidance first, then the
// per-turn repair guidance below it when a `RunContext.RepairCounters`
// field has tripped. A blank line separates the two when both are
// present. Returns the empty string when neither contributes — the
// caller then omits the section entirely.
//
// Pure read of `extraGuidance` + `rc`: it never mutates the counters.
func buildAdditionalGuidance(extraGuidance string, rc planner.RunContext) string {
	op := strings.TrimSpace(extraGuidance)
	repair := renderRepairGuidance(rc.RepairCounters)
	switch {
	case op != "" && repair != "":
		return op + "\n\n" + repair
	case op != "":
		return op
	default:
		return repair
	}
}

// renderTool emits the per-tool block for the <available_tools>
// section (Phase 83b — D-144). The block is:
//
//   - <name>: <description>
//     args_schema: <compact one-line JSON>
//     side_effects: <class>
//     examples:
//   - <description> → <compact args JSON>
//
// The `args_schema` and `examples:` lines are omitted entirely when
// the tool declares no schema / no examples — a no-examples tool
// renders exactly through the `side_effects` line, so existing tool
// registrations need no code change (the new fields are opt-in on
// `tools.Tool`). `side_effects` always renders, defaulting to `pure`
// when the tool leaves the field unset.
//
// renderTool is a pure function: it reads only its arguments, holds no
// shared state, and is therefore trivially safe for concurrent reuse
// (D-025). The compact-JSON discipline (one line, deterministic key
// order via `encoding/json`'s sorted map marshalling) maximises the
// KV-cache hit rate across turns (brief 13 §5).
func renderTool(t tools.Tool, cfg toolRenderConfig) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(t.Name)
	if t.Description != "" {
		b.WriteString(": ")
		b.WriteString(oneLine(t.Description))
	}
	b.WriteString("\n")

	if schema := compactJSON(t.ArgsSchema); schema != "" {
		b.WriteString("  args_schema: ")
		b.WriteString(schema)
		b.WriteString("\n")
	}

	b.WriteString("  side_effects: ")
	b.WriteString(sideEffectOf(t))
	b.WriteString("\n")

	examples := rankedExamples(t.Examples, cfg.maxExamples)
	if len(examples) > 0 {
		b.WriteString("  examples:\n")
		for _, ex := range examples {
			b.WriteString("    - ")
			if d := oneLine(ex.Description); d != "" {
				b.WriteString(d)
				b.WriteString(" → ")
			}
			b.WriteString(compactArgs(ex.Args))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// sideEffectOf returns the tool's declared side-effect class, defaulting
// to "pure" when the field is unset — a tool that makes no claim is
// treated as the safest class so the planner's <tool_usage> guidance
// reads consistently.
func sideEffectOf(t tools.Tool) string {
	if t.SideEffects == "" {
		return string(tools.SideEffectPure)
	}
	return string(t.SideEffects)
}

// exampleTagRank maps a [tools.ToolExample]'s tag set to a sort rank:
// `minimal` (0) > `common` (1) > `edge-case` (2) > untagged (3). The
// lowest-numbered (highest-priority) tag on the example wins — an
// example tagged both `common` and `edge-case` ranks as `common`.
func exampleTagRank(tags []string) int {
	rank := 3 // untagged
	for _, tag := range tags {
		switch tag {
		case "minimal":
			return 0 // highest priority — short-circuit
		case "common":
			if rank > 1 {
				rank = 1
			}
		case "edge-case":
			if rank > 2 {
				rank = 2
			}
		}
	}
	return rank
}

// rankedExamples returns up to `limit` examples from `in`, ordered by
// tag priority (`minimal` > `common` > `edge-case` > untagged). The
// sort is stable on `(rank, originalIndex)` so equal-rank examples
// keep their registration order. A non-positive `limit` yields no
// examples; the input slice is never mutated (the helper copies).
func rankedExamples(in []tools.ToolExample, limit int) []tools.ToolExample {
	if limit <= 0 || len(in) == 0 {
		return nil
	}
	ranked := make([]tools.ToolExample, len(in))
	copy(ranked, in)
	sort.SliceStable(ranked, func(i, j int) bool {
		return exampleTagRank(ranked[i].Tags) < exampleTagRank(ranked[j].Tags)
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// compactJSON re-marshals a JSON-Schema document to a single-line
// compact form (no insignificant whitespace, deterministic map-key
// order via `encoding/json`). Returns the empty string when the input
// is empty or not valid JSON — a tool with no schema simply omits the
// `args_schema:` line. Brief 13 §5: compact JSON keeps the prompt
// stable across turns for KV-cache hit rates. HTML escaping is
// disabled so `<`, `>`, `&` survive verbatim in tool-schema
// descriptions like `"value < 100"`; the schema renders inside an
// XML-ish wrapper the model reads as data and the un-escaped form is
// both smaller and more readable.
//
// **Distinct contract from [compactValueJSON] (memory_wrappers.go).**
// `compactJSON` is lossy on error (returns "" so a malformed
// tool-schema simply omits the args_schema line); `compactValueJSON`
// is fail-loud (returns an error so a malformed memory tier raises
// `planner.ErrMemoryBlockUnserializable`). The split is deliberate
// per D-144 (lenient schema renderer) and D-146 (loud memory render).
// Do not unify the two without changing both decisions.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// compactArgs marshals an example's `Args` map to single-line compact
// JSON. A nil / empty map renders as `{}` — matching the parser's
// normalisation for an argument-free call. Marshalling failure (an
// unserialisable value the example author placed in the map) yields
// `{}` rather than leaking a Go `%v` rendering into the prompt.
func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// renderPlanningConstraints renders the <planning_constraints> section
// (brief 13 §2.1 section 12) from `RunContext.PlanningHints`
// (Phase 83c — D-145). Returns the empty string when the hints are
// nil or carry no content, so the section is omitted from the prompt
// (acceptance criterion: missing optional injections omit their
// section). The render is delegated to [renderPlanningHints].
func renderPlanningConstraints(rc planner.RunContext) string {
	return renderPlanningHints(rc.PlanningHints)
}

// buildUserContent composes the user goal + optional summary.
func buildUserContent(rc planner.RunContext) string {
	goal := rc.Goal
	if goal == "" {
		goal = rc.Query
	}
	if goal == "" {
		goal = "(no goal supplied)"
	}

	var b strings.Builder
	b.WriteString("User goal: ")
	b.WriteString(goal)

	if rc.Trajectory != nil && rc.Trajectory.Summary != nil {
		s := rc.Trajectory.Summary
		b.WriteString("\n\nTrajectory summary so far:\n")
		if len(s.Goals) > 0 {
			b.WriteString("  Goals tracked: ")
			b.WriteString(strings.Join(s.Goals, "; "))
			b.WriteString("\n")
		}
		if len(s.Facts) > 0 {
			b.WriteString("  Facts: ")
			b.WriteString(strings.Join(s.Facts, "; "))
			b.WriteString("\n")
		}
		if len(s.Pending) > 0 {
			b.WriteString("  Pending: ")
			b.WriteString(strings.Join(s.Pending, "; "))
			b.WriteString("\n")
		}
		if s.LastOutputDigest != "" {
			b.WriteString("  Last output: ")
			b.WriteString(oneLine(s.LastOutputDigest))
			b.WriteString("\n")
		}
		if s.Note != "" {
			b.WriteString("  Note: ")
			b.WriteString(oneLine(s.Note))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// listTools returns the tools visible to the planner via the
// RunContext's catalog view. Nil catalog yields an empty slice.
func listTools(rc planner.RunContext) []tools.Tool {
	if rc.Catalog == nil {
		return nil
	}
	return rc.Catalog.List()
}

// renderAssistantTurn renders one prior trajectory step as the
// assistant turn for the next prompt. It is the Phase 83e (D-148)
// replay-aware wrapper around [renderActionForLLM]: when `replayMode`
// is [planner.ReasoningReplayText] AND the step carries a non-empty
// `ReasoningTrace`, the trace is prepended as a text block ABOVE the
// action JSON; when the mode is [planner.ReasoningReplayNever] (the
// default) — or the trace is empty — only the action JSON is rendered.
//
// Returns the empty string when the action itself is unrenderable
// (the prompt builder skips empty messages).
func renderAssistantTurn(step planner.Step, replayMode planner.ReasoningReplayMode) string {
	action := renderActionForLLM(step.Action)
	if action == "" {
		return ""
	}
	if replayMode == planner.ReasoningReplayText && step.ReasoningTrace != "" {
		// Prepend the captured reasoning as a text block above the
		// action JSON. The block is plainly labelled so the model
		// reads it as prior chain-of-thought, not as another action.
		return "Reasoning:\n" + step.ReasoningTrace + "\n\n" + action
	}
	return action
}

// renderActionForLLM converts a Step.Action (typed as `any` in the
// trajectory subpackage to avoid an import cycle) into the JSON
// envelope the LLM previously emitted. Supports the V1 minimum-viable
// Decision shapes; non-CallTool actions render as a JSON object
// carrying the shape's name + a debug field.
//
// Phase 83e (D-147) narrowed the echoed envelope to `{tool, args}` —
// the former `reasoning` key is dropped, matching the narrowed
// `planner.CallTool` shape. Captured reasoning is replayed (when the
// agent opts in) as a separate text block by [renderAssistantTurn],
// never as a field inside the action JSON.
//
// Returns the empty string when the action is nil or unrenderable
// (the prompt builder skips empty messages — defensive against
// trajectory shapes the planner doesn't recognise).
//
// The echoed envelope carries `{tool, args}` only — `reasoning` is
// NOT replayed (brief 13 §2.6 + the Phase 83a prompt-side alignment:
// reasoning is captured from the provider channel, never re-injected
// across turns).
func renderActionForLLM(action any) string {
	if action == nil {
		return ""
	}
	switch a := action.(type) {
	case planner.CallTool:
		// Echo the JSON envelope the LLM emitted, normalised. No
		// `reasoning` key — the prompt's <action_schema> and the
		// trajectory replay both omit it (brief 13 §2.6).
		env := map[string]any{
			"tool": a.Tool,
			"args": json.RawMessage(safeArgs(a.Args)),
		}
		out, err := json.Marshal(env)
		if err != nil {
			return ""
		}
		return string(out)
	case planner.Finish:
		// A prior Finish in the trajectory is unusual (the runtime
		// should have terminated), but render defensively for
		// observability.
		out, err := json.Marshal(map[string]any{
			"action": "finish",
			"reason": string(a.Reason),
		})
		if err != nil {
			return ""
		}
		return string(out)
	default:
		// Unknown action shape — render a minimal marker so the
		// trajectory render preserves ordering.
		out, err := json.Marshal(map[string]any{
			"action": fmt.Sprintf("%T", action),
		})
		if err != nil {
			return ""
		}
		return string(out)
	}
}

// renderObservationForLLM picks the projection the planner shows to
// the LLM. Per D-026 ("heavy content discipline"), the planner prefers
// LLMObservation over raw Observation: producer-side renderers
// (Phase 44+) populate LLMObservation as the compressed / redacted
// projection; raw Observation may carry full tool results that aren't
// safe to round-trip through the LLM.
//
// Error / Failure are surfaced first when present (the planner needs
// to see failures to course-correct).
func renderObservationForLLM(step planner.Step) string {
	if step.Failure != nil {
		return fmt.Sprintf("Observation (failure): %s — %s",
			step.Failure.Code, oneLine(step.Failure.Message))
	}
	if step.Error != "" {
		return "Observation (error): " + oneLine(step.Error)
	}
	if step.LLMObservation != nil {
		return "Observation: " + renderAny(step.LLMObservation)
	}
	if step.Observation != nil {
		return "Observation: " + renderAny(step.Observation)
	}
	return ""
}

// renderBackground renders resolved background-task outcomes as a
// JSON-encoded user message. Phase 45 ships the read path (D-032 push
// wake declaration); the runtime engine populates
// `rc.Trajectory.Background` in Phase 47+.
func renderBackground(bg map[string]planner.BackgroundResult) string {
	if len(bg) == 0 {
		return ""
	}
	out, err := json.Marshal(map[string]any{
		"background_resolved": bg,
	})
	if err != nil {
		return ""
	}
	return string(out)
}

// renderAny is a small renderer for arbitrary observation values. We
// avoid `fmt.Sprintf("%v", x)` for structured shapes because it can
// surface heavy content that should have been redacted. Strings pass
// through (one-lined); maps / structs render via json.Marshal with a
// fall-through to the type name when marshalling fails.
func renderAny(v any) string {
	if v == nil {
		return "(nil)"
	}
	switch x := v.(type) {
	case string:
		return oneLine(x)
	case json.RawMessage:
		return string(x)
	case []byte:
		// Avoid leaking raw bytes; show a marker. The planner contract
		// is that the producer should have rendered LLMObservation as
		// a string-shaped projection; a []byte here is a producer-side
		// bug surfaced rather than papered over.
		return fmt.Sprintf("(<%d raw bytes — producer should render LLMObservation as text>)", len(x))
	default:
		out, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("(<unrenderable %T>)", v)
		}
		return string(out)
	}
}

// safeArgs returns the args slice, or `{}` when empty/nil — matches
// the Phase 44 parser's normalisation so the echoed envelope is
// byte-identical to the LLM's original (modulo whitespace).
func safeArgs(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// oneLine collapses internal newlines + carriage returns to spaces so
// the rendered prompt remains a single line per message. The LLM-side
// tokenisation is largely whitespace-insensitive; collapsing keeps the
// prompt size bounded against malicious or runaway observations.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// textContent constructs the [llm.Content] sum-type with the supplied
// text. Helper to keep call sites compact.
func textContent(s string) llm.Content {
	return llm.Content{Text: &s}
}
