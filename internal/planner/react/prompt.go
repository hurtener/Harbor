package react

import (
	"encoding/json"
	"fmt"
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
// omitted entirely.
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
}

// Build implements [PromptBuilder].
func (b defaultBuilder) Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest {
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}

	var messages []llm.ChatMessage

	// 1. System block: the twelve XML-tagged sections.
	sysContent := buildSystemContent(systemPrompt, b.extraGuidance, rc)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleSystem,
		Content: textContent(sysContent),
	})

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
//     `RunContext.PlanningHints` and merges per-turn repair guidance.
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
//  11. <additional_guidance>  — operator-supplied. OMITTED when empty.
//  12. <planning_constraints> — runtime-supplied. OMITTED when empty.
//
// `systemPrompt` is the legacy override surface ([WithSystemPrompt]):
// when an operator passes a non-default string it REPLACES the entire
// twelve-section structure (the structured sections are
// [DefaultSystemPrompt]'s content). `extraGuidance` flows into section
// 11. Sections 11 and 12 are omitted entirely — not emitted as empty
// tag pairs — when their content is absent.
//
// Phase 83a establishes the section anchors; 83b/c/d build on them.
func buildSystemContent(systemPrompt, extraGuidance string, rc planner.RunContext) string {
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
	// "no tools" marker when the catalog is empty).
	sections = append(sections, renderAvailableToolsSection(rc))

	// Section 11: <additional_guidance> — omitted entirely when empty.
	if g := strings.TrimSpace(extraGuidance); g != "" {
		sections = append(sections, "<additional_guidance>\n"+g+"\n</additional_guidance>")
	}

	// Section 12: <planning_constraints> — omitted entirely until
	// Phase 83c wires `RunContext.PlanningHints`. The anchor exists so
	// 83c is a localised edit, not a structural change.
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
// (brief 13 §2.1 section 10). Phase 83a renders `name + description`
// per tool — the Phase 45 catalog shape. Phase 83b upgrades this to
// emit `args_schema` + curated examples per tool.
func renderAvailableToolsSection(rc planner.RunContext) string {
	var b strings.Builder
	b.WriteString("<available_tools>\n")

	catalog := listTools(rc)
	if len(catalog) == 0 {
		b.WriteString("(no tools registered for this run)\n")
	} else {
		for _, t := range catalog {
			b.WriteString("- ")
			b.WriteString(t.Name)
			if t.Description != "" {
				b.WriteString(": ")
				b.WriteString(oneLine(t.Description))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("</available_tools>")
	return b.String()
}

// renderPlanningConstraints renders the <planning_constraints> section
// (brief 13 §2.1 section 12) from runtime-supplied hints. Phase 83a
// ships the anchor only — it always returns the empty string, so the
// section is omitted from the prompt. Phase 83c wires
// `RunContext.PlanningHints` and gives this function a real body.
func renderPlanningConstraints(_ planner.RunContext) string {
	// Phase 83c populates this from RunContext.PlanningHints. Until
	// then the section is omitted entirely (acceptance criterion:
	// missing optional injections omit their section).
	return ""
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
