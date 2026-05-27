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
//  1. System message: the nine XML-tagged sections (brief 13 §2.1,
//     reshaped by Phase 107c — D-167) assembled by [buildSystemContent]
//     — `<identity>`, `<tool_discovery>`, `<tool_usage>`,
//     `<reasoning>`, `<tone>`, `<error_handling>`,
//     `<available_tools>`, `<additional_guidance>`,
//     `<planning_constraints>` — in that fixed order, separated by
//     `\n\n`. Optional sections (`<additional_guidance>`,
//     `<planning_constraints>`) are omitted entirely when empty.
//     Phase 107c deletes `<output_format>`, `<action_schema>`,
//     `<finishing>`, and `<parallel_execution>` (the prompt-engineered
//     JSON-action shapes — parallel emission is now a native-side
//     property: the runtime accepts multiple `ToolCalls` in one
//     response and serialises them per the AC-19 fallback). A single
//     `<tool_discovery>` section instructs on native tool-calling
//     semantics + deferred-loading meta-tools.
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
	// maxToolExamples is retained for backward compatibility with
	// react.go's constructor (Phase 107c step 8 does not touch
	// react.go). Under native tool-calling (Phase 107c — D-167) the
	// <available_tools> prompt section renders name+description only
	// (schemas live in req.Tools[]), so renderTool no longer reads
	// this field. It will be deleted when step 9 wires the projector.
	maxToolExamples int
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
	// Round-7 F11 / D-166 — when the run carries operator-uploaded
	// input artifacts (first-turn only; the run loop nils the slice
	// on subsequent turns so this block is a no-op once the planner
	// is mid-trajectory), the materializer fans them out into typed
	// `Content.Parts`: `image/*` inlines as `ImagePart.DataURL`;
	// pdf/audio stay as `Artifact`-form parts the bifrost driver
	// translates natively for capable providers and falls back to
	// stub-JSON text for the rest; every other MIME emits an
	// `ArtifactStub` text block the LLM routes via the tool catalog.
	// Text-only turns fall through to the unchanged `textContent`
	// wrap.
	userMessageContent := planner.MaterializeInputContent(userContent, rc.InputArtifacts, rc.Catalog)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleUser,
		Content: userMessageContent,
	})

	// 3. Trajectory rendering. Phase 46 contract (D-055): when
	// rc.Trajectory.Summary is non-nil, SKIP the per-step assistant +
	// user pair loop. The compacted summary in the user block above is
	// the trajectory representation; rendering both would double-count
	// tokens and defeat the compression (brief 02 §4: "The compressed
	// digest replaces the raw step history in subsequent prompt
	// builds."). When Summary is nil, render the raw step history as
	// before (the Phase 45 V1 minimum-viable shape).
	//
	// Phase 107c (D-167) — native tool-calling replay (AC-20a / AC-20b).
	// A trajectory Step whose Action is a `planner.CallTool` now renders
	// as a pair of native chat messages:
	//
	//   - a RoleAssistant message whose `ToolCalls` slice carries the
	//     prior CallTool's ID + Name + Args (the bifrost translator
	//     emits this as a `tool_calls` block — OpenAI / Anthropic /
	//     Gemini all consume it).
	//   - a RoleTool message whose `ToolCallID` matches the assistant
	//     entry's ID; its Content is the rendered observation.
	//
	// When the prior CallTool has an empty CallID (legacy trajectory or
	// a planner-emitted CallTool that pre-dates a provider-supplied ID),
	// a deterministic `react.callid.<step-index>` is synthesised and
	// stamped on BOTH the assistant ToolCalls entry AND the RoleTool
	// ToolCallID so the round-trip is well-formed regardless. The
	// projector's reserved-name path emits Finish/SpawnTask/AwaitTask
	// directly (no trajectory Step), so this renderer's only CallTool-
	// shaped input is a normal tool dispatch.
	//
	// Non-CallTool action shapes (a defensive prior Finish, an unknown
	// shape) fall through to the legacy assistant-text rendering so
	// observability is preserved even in malformed trajectories.
	if rc.Trajectory != nil {
		if rc.Trajectory.Summary == nil {
			replayMode := planner.EffectiveReasoningReplay(rc, b.configuredReplay)
			for i, step := range rc.Trajectory.Steps {
				asstMsg, toolMsg, native := renderNativeStepPair(step, replayMode, i)
				if native {
					messages = append(messages, asstMsg)
					if toolMsg != nil {
						messages = append(messages, *toolMsg)
					}
					continue
				}
				// Legacy fallback for non-CallTool actions.
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

// The ten static section bodies (Phase 107c — D-167 resizes from
// twelve). Brief 13 §4 carries the verbatim adapted copy; the constants
// below are that copy, split at the XML tag boundaries so each section
// is independently editable (brief 13 §2.1 design property 1).
// Phases 83b/c/d extend these section anchors; Phase 107c deletes
// `<output_format>`, `<action_schema>`, and `<finishing>` (the
// prompt-engineered JSON-action instruction block) and replaces them
// with `<tool_discovery>`.
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

	sectionToolDiscovery = `<tool_discovery>
You have access to tools declared natively — the schemas and validation are handled by the runtime.
Your task is to decide which tool to call and with what arguments; the runtime validates and dispatches.

You also have these discovery meta-tools to explore what is available:

- tool_search(query, tags[], limit): search the tool catalog by capability keywords. Tools surfaced by tool_search are callable on the next turn after the planner observes the search result.
- tool_get(name): fetch the full schema + examples for a specific tool.
- skill_search(query, tags[], limit): search skill playbooks by topic.
- skill_get(name): fetch a specific skill's content.

If you cannot emit a native tool call (e.g. your model lacks native tool-calling support), you may call the declarative_action tool with {"tool": "...", "args": {...}}. The runtime routes this through a backward-compatible dispatch path.

The _finish reserved-discriminator is RETIRED. When you have enough information to satisfy the user's goal, produce your final answer as plain content (no tool call). The runtime interprets a response with content and zero tool calls as a terminal answer.

CRITICAL final-answer shape (Phase 107c live-test finding — Claude Haiku 4.5 and other RLHF-trained models default to wrapping their terminal in {"tool":"_finish","args":{"answer":"..."}} JSON; this shape is now WRONG):
- Emit ONLY the user-facing prose. No JSON wrapper. No markdown code fences around the answer. No "tool" or "args" keys.
- Do NOT call _finish, finish, respond_with, or any other "terminal" tool — there is no such tool declared. Reply as the assistant turn's content and stop.
- The streaming surface forwards your tokens to the user verbatim. If you emit a markdown json code fence or {"tool":"_finish", the user sees that text live; the runtime later extracts the inner answer, but the streaming UX is broken until completion.
- If you are unsure whether to call a tool or finish: when no tool would add information, finish with prose.
</tool_discovery>`

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

	sectionReasoning = `<reasoning>
Approach problems systematically:

1. Understand first: Parse the query to identify what's actually being asked
2. Plan before acting: Consider which tools will help and in what order
3. Gather evidence: Use tools to collect relevant information
4. Synthesize: Combine observations into a coherent answer
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
- Writing user-facing text during intermediate steps (save it for the terminal answer message)
- Generating "preview" answers before you're done gathering information
</reasoning>`

	sectionTone = `<tone>
When delivering your final answer to the user:
- Be direct and informative — get to the point
- Use clear, professional language
- Acknowledge limitations honestly rather than hedging excessively
- Match the formality level to the query (technical queries get technical answers)
- Avoid unnecessary caveats, but do note important limitations
- Don't apologize unless you've actually made an error
- These are safe defaults. Your tone or voice can be changed in the additional_guidance section.
- You can use markdown formatting if suggested in additional_guidance.

During intermediate steps (when you're calling tools, not yet answering):
- Emit only tool calls — keep any narration to the final answer turn.
- Internal reasoning is captured automatically by the runtime through provider-side channels when the provider exposes one; you do not need to echo it.
</tone>`

	sectionErrorHandling = `<error_handling>
When things go wrong:

Tool validation error: Fix your args to match the schema and retry
Tool execution error: Note the error, try alternative tools or approaches
No suitable tools: Explain what you cannot do and why
Ambiguous query: Make reasonable assumptions and note them, or ask for clarification
Conflicting information: Acknowledge the conflict and explain your reasoning

If you cannot complete the task after reasonable attempts:
- Explain what you tried and why it didn't work in your final answer
- Suggest what additional information or tools would help
- If you need clarification from the user before you can proceed, ask for it directly in your final answer
  (Harbor surfaces your answer as the next user-visible turn; a follow-up question is a valid finish)
</error_handling>`
)

// buildSystemContent assembles the nine XML-tagged sections (Phase 107c
// D-167) in their fixed order, separated by `\n\n`.
//
//  1. <identity>             — role framing + current date.
//  2. <tool_discovery>       — native tool-calling instructions +
//     deferred-loading meta-tools (Phase 107c — D-167).
//  3. <tool_usage>           — side_effects taxonomy + invocation rules.
//  4. <reasoning>            — 5-step systematic approach.
//  5. <tone>                 — voice defaults + intermediate-step
//     guidance (no JSON-action shape — native tool-calling owns
//     the wire form).
//  6. <error_handling>       — recovery framing; no requires_followup.
//  7. <available_tools>      — name + description quick reference
//     (schemas live in the provider's native Tools[] declaration —
//     Phase 107c — D-167).
//  8. <additional_guidance>  — operator-supplied content + Phase 83c
//     per-turn repair guidance. OMITTED only when BOTH are empty.
//  9. <planning_constraints> — runtime-supplied PlanningHints
//     (Phase 83c). OMITTED when nil / empty.
//
// `systemPrompt` is the legacy override surface ([WithSystemPrompt]):
// when an operator passes a non-default string it REPLACES the entire
// ten-section structure (the structured sections are
// [DefaultSystemPrompt]'s content). `extraGuidance` flows into section
// 9. Sections 9 and 10 are omitted entirely — not emitted as empty tag
// pairs — when their content is absent.
//
// `maxToolExamples` is retained for backward compatibility with the
// builder field (react.go still sets it — step 9 deletes). Under
// Phase 107c native tool-calling, `renderAvailableToolsSection`
// ignores it and renders name+description only. The param will be
// removed when step 9 lands.
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
			sectionToolDiscovery,
			sectionToolUsage,
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
// (Phase 107c — D-167). Under native tool-calling, the prompt-side
// catalog is a name+description quick-reference only — schemas,
// side_effects, and examples live in the provider's native Tools[]
// declaration. `maxToolExamples` is ignored (kept for backward compat
// with react.go — step 9 deletes it).
//
// The section lists BOTH the always-loaded catalog subset (returned
// by `rc.Catalog.List()`) AND the per-run discovered tools (resolved
// by name from `rc.DiscoveredTools`). Discovered tools that already
// appear in the always-loaded set are not duplicated. This mirrors
// the `req.Tools` construction in `react.Next` (AC-17) so the LLM's
// prompt and its native tool surface stay in sync.
func renderAvailableToolsSection(rc planner.RunContext, maxToolExamples int) string {
	// Phase 107c (D-167): `maxToolExamples` is ignored — schemas live
	// in req.Tools[]; the prompt renders name+description only.
	_ = maxToolExamples

	var b strings.Builder
	b.WriteString("<available_tools>\n")

	catalog := listTools(rc)
	// Append discovered tools (resolved by name) that aren't already
	// in the always-loaded set. Mirrors buildToolDeclarations() — the
	// section stays consistent with the per-turn req.Tools slice.
	seen := make(map[string]struct{}, len(catalog))
	for _, t := range catalog {
		seen[t.Name] = struct{}{}
	}
	if rc.Catalog != nil {
		for _, name := range rc.DiscoveredTools {
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			if t, ok := rc.Catalog.Resolve(name); ok {
				catalog = append(catalog, t)
				seen[name] = struct{}{}
			}
		}
	}

	if len(catalog) == 0 {
		b.WriteString("(no tools registered for this run)\n")
	} else {
		for i, t := range catalog {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(renderToolNameDesc(t))
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

// renderToolNameDesc renders a tool as name + description only for the
// prompt-side <available_tools> quick reference (Phase 107c — D-167).
// Schemas, side_effects, and examples live in the provider's native
// Tools[] declaration; the prompt duplicates none of them.
func renderToolNameDesc(t tools.Tool) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(t.Name)
	if t.Description != "" {
		b.WriteString(": ")
		b.WriteString(oneLine(t.Description))
	}
	b.WriteString("\n")
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

// renderNativeStepPair projects a single trajectory step into a pair
// of native chat messages — an assistant `tool_calls` block + a
// matching `tool` role observation — per the Phase 107c contract
// (D-167 / AC-20a / AC-20b). Returns (assistantMsg, *toolMsg, true)
// when the step's Action is a `planner.CallTool`; the third return is
// false for any other Action shape (the caller falls back to the
// legacy assistant-text + user-observation pair so observability
// survives malformed trajectories).
//
// The assistant message carries one ToolCalls entry whose ID +
// Name + Args mirror the prior CallTool. When the CallTool's CallID
// is empty (legacy trajectory shape, or a planner-emitted call that
// pre-dates a provider-supplied ID), a deterministic synthetic ID
// `react.callid.<step-index>` is generated and stamped on BOTH the
// assistant tool-call entry AND the RoleTool ToolCallID so the
// round-trip stays well-formed.
//
// When the step has no observation, error, or failure surface yet
// (the runtime appended the Action but the dispatch hasn't completed
// — an unusual state outside tests), the returned `toolMsg` is nil so
// the caller emits only the assistant turn. The provider will see an
// outstanding tool_call awaiting a tool result; the planner's next
// step will append the matching RoleTool message when the observation
// lands.
//
// `replayMode` controls reasoning replay (Phase 83e — D-148). When
// set to [planner.ReasoningReplayText] AND the step carries a
// non-empty `ReasoningTrace`, the captured reasoning is prepended as
// a text block in the assistant message's Content body (above the
// tool_calls block from the provider's perspective).
func renderNativeStepPair(step planner.Step, replayMode planner.ReasoningReplayMode, stepIdx int) (llm.ChatMessage, *llm.ChatMessage, bool) {
	call, ok := step.Action.(planner.CallTool)
	if !ok {
		return llm.ChatMessage{}, nil, false
	}
	callID := call.CallID
	if callID == "" {
		callID = fmt.Sprintf("react.callid.%d", stepIdx)
	}
	// Assistant content: empty by default (the provider reads the
	// tool_calls block; no text needed). When reasoning replay is on
	// AND the step has a trace, prepend it as the assistant text body.
	assistantText := ""
	if replayMode == planner.ReasoningReplayText && step.ReasoningTrace != "" {
		assistantText = "Reasoning:\n" + step.ReasoningTrace
	}
	asst := llm.ChatMessage{
		Role:    llm.RoleAssistant,
		Content: textContent(assistantText),
		ToolCalls: []llm.ToolCallStructured{{
			ID:   callID,
			Name: call.Tool,
			Args: json.RawMessage(safeArgs(call.Args)),
		}},
	}
	observation := renderNativeObservation(step)
	if observation == "" {
		return asst, nil, true
	}
	id := callID
	tool := &llm.ChatMessage{
		Role:       llm.RoleTool,
		Content:    textContent(observation),
		ToolCallID: &id,
	}
	return asst, tool, true
}

// renderNativeObservation returns the tool-result content body for the
// native RoleTool message. Failures + errors surface first (the
// planner needs to see them to course-correct); otherwise the
// LLMObservation projection is preferred over the raw Observation per
// D-026 heavy-content discipline. Returns the empty string when no
// observation is available yet (the runtime appended the Action but
// dispatch hasn't completed).
func renderNativeObservation(step planner.Step) string {
	if step.Failure != nil {
		return fmt.Sprintf("Tool failure: %s — %s",
			step.Failure.Code, oneLine(step.Failure.Message))
	}
	if step.Error != "" {
		return "Tool error: " + oneLine(step.Error)
	}
	if step.LLMObservation != nil {
		return renderAny(step.LLMObservation)
	}
	if step.Observation != nil {
		return renderAny(step.Observation)
	}
	return ""
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
//
// Phase 107c (D-167): this function is retained for the legacy
// non-CallTool action fallback path in [defaultBuilder.baseRequest]
// — CallTool actions now route through [renderNativeStepPair] and
// emit native assistant `tool_calls` + RoleTool ChatMessage pairs.
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
