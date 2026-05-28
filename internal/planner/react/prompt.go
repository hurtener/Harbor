package react

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
//  1. System message: the ten XML-tagged sections (brief 13 §2.1,
//     reshaped by Phase 107c — D-167) assembled by
//     [buildSystemContent] —
//     `<identity>`, `<tool_discovery>`, `<heavy_results>`,
//     `<tool_usage>`, `<reasoning>`, `<tone>`, `<error_handling>`,
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
//     semantics + deferred-loading meta-tools; `<heavy_results>`
//     teaches the out-of-context artifact-store pattern + the
//     `artifact_fetch` meta-tool.
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
				// Phase 107d (D-169): a CallParallel step renders as ONE
				// assistant message carrying N tool_calls + N RoleTool
				// messages, one per branch, each ToolCallID matched to the
				// branch's CallID (AC-9). Decomposed from the AC-4
				// aggregate observation.
				if pcall, ok := step.Action.(planner.CallParallel); ok {
					asstMsg, toolMsgs := renderNativeParallelStep(step, pcall, replayMode, i)
					messages = append(messages, asstMsg)
					messages = append(messages, toolMsgs...)
					continue
				}
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

// The eleven static section bodies (Phase 107c — D-167 resizes from
// twelve). Brief 13 §4 carries the verbatim adapted copy; the
// constants below are that copy, split at the XML tag boundaries so
// each section is independently editable (brief 13 §2.1 design
// property 1). Phases 83b/c/d extend these section anchors; Phase
// 107c deletes `<output_format>`, `<action_schema>`, and
// `<finishing>` (the prompt-engineered JSON-action instruction
// block) and replaces them with `<tool_discovery>` + the
// `<heavy_results>` explainer for the out-of-context artifact
// store.
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
You have native tool-calling. Tools declared in this turn are schema-validated and dispatched by the runtime — you choose which tool to call and with what arguments.

Discovery meta-tools (use them to explore beyond the always-loaded set):

- tool_search(query, tags[], limit): search the catalog by capability keywords. Results are callable on the next turn after the planner observes the search.
- tool_get(name): fetch a specific tool's full schema + examples.
- skill_search(query, tags[], limit): search skill playbooks by topic.
- skill_get(name): fetch a specific skill's content.

How to respond, in two cases:

1. You need information or to take an action → emit one or more native tool calls. The runtime executes them, surfaces results back to you on the next turn, and you decide what to do next.
2. You have enough information to satisfy the user → reply as the assistant turn's plain prose content with NO tool calls. The runtime delivers your message to the user verbatim and ends the run.

Your prose is streamed live to the user as you type it, character by character. Markdown formatting is supported when the additional_guidance section permits it. The runtime adds nothing to your message — what you write IS what the user sees.
</tool_discovery>`

	// sectionHeavyResults is the factual explainer for the runtime's
	// out-of-context storage of large tool results plus the
	// meta-tools that operate on the resulting reference handles.
	// The wording is deliberately descriptive — it states the
	// mechanism, names a reference shape, lists the meta-tools, and
	// notes that re-calling the upstream tool produces another
	// stored copy rather than bypassing the threshold. New
	// meta-tools that act on stored references extend the bullet
	// list in this section as they land.
	sectionHeavyResults = `<heavy_results>
Some tools return payloads larger than fit cleanly in your context (multimedia metadata, file contents, query dumps). The runtime stores any tool result above its size threshold in an out-of-context artifact store and surfaces you a short preview plus a reference handle. Each reference looks like ref="abc123def456" and is unique per stored payload.

Meta-tools for working with stored references:

- artifact_fetch(ref, max_bytes?): retrieve the full payload of a stored result. Use it when the preview does not carry the field, value, or section you need to answer the user. max_bytes lets you bound the returned slice; the runtime defaults to a safe size when omitted.

The preview is the head of the payload only — fields further into the result live in the stored copy and require artifact_fetch to inspect. Re-calling the upstream tool produces a fresh stored copy of the same kind of payload; it does not bypass the threshold.
</heavy_results>`

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
//  3. <heavy_results>        — factual explainer for out-of-context
//     storage of large tool results + the artifact_fetch meta-tool.
//  4. <tool_usage>           — side_effects taxonomy + invocation rules.
//  5. <reasoning>            — 5-step systematic approach.
//  6. <tone>                 — voice defaults + intermediate-step
//     guidance (no JSON-action shape — native tool-calling owns
//     the wire form).
//  7. <error_handling>       — recovery framing; no requires_followup.
//  8. <available_tools>      — name + description quick reference
//     (schemas live in the provider's native Tools[] declaration —
//     Phase 107c — D-167).
//  9. <additional_guidance>  — operator-supplied content + Phase 83c
//     per-turn repair guidance. OMITTED only when BOTH are empty.
//  10. <planning_constraints> — runtime-supplied PlanningHints
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
			sectionHeavyResults,
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

// The `<available_tools>` section renders only `{name, description}`
// as a quick-reference; the typed schemas live in `req.Tools[]`. The
// example-ranking and schema-rendering helpers that fed the
// prompt-engineered shape are not needed and have been removed.

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
	// Replay the model's prior preamble prose so the assistant turn
	// retains its narrative thread. Reasoning-replay (D-148) layers
	// on top: when `ReasoningReplay=text` AND the step has a
	// provider-side ReasoningTrace, the trace is appended below the
	// preamble.
	assistantText := step.AssistantPreamble
	if replayMode == planner.ReasoningReplayText && step.ReasoningTrace != "" {
		if assistantText != "" {
			assistantText += "\n\nReasoning:\n" + step.ReasoningTrace
		} else {
			assistantText = "Reasoning:\n" + step.ReasoningTrace
		}
	}
	// Leave Content at zero value when the preamble is empty.
	// OpenAI's wire spec requires `content: null` (not `""`) when
	// tool_calls is present; the safety pass + translator carve-out
	// emit the null shape from the zero-value Content here.
	var asstContent llm.Content
	if assistantText != "" {
		asstContent = textContent(assistantText)
	}
	asst := llm.ChatMessage{
		Role:    llm.RoleAssistant,
		Content: asstContent,
		ToolCalls: []llm.ToolCallStructured{{
			ID:   callID,
			Name: call.Tool,
			Args: json.RawMessage(safeArgs(call.Args)),
		}},
	}
	observation := renderNativeObservation(step)
	if observation == "" {
		// OpenAI's native tool-calling wire spec requires every
		// assistant `tool_calls[i]` to be paired with a
		// `role:"tool"` message carrying the matching
		// `tool_call_id`. Emitting the assistant half without its
		// sibling produces a 400 on OpenAI and is non-recoverable
		// on Anthropic. Synthesise a placeholder tool body so the
		// pair is always complete; the slog.Warn surfaces the
		// underlying gap (a tool returned nil / empty without an
		// error) so an operator can fix the upstream producer.
		slog.Warn("react.renderNativeStepPair: empty observation — emitting placeholder tool message to preserve wire contract",
			"step_idx", stepIdx,
			"tool", call.Tool,
			"call_id", callID,
			"observation_nil", step.Observation == nil,
			"llm_observation_nil", step.LLMObservation == nil,
			"failure_nil", step.Failure == nil,
			"error_empty", step.Error == "",
		)
		observation = "(tool returned no observation)"
	}
	id := callID
	tool := &llm.ChatMessage{
		Role:       llm.RoleTool,
		Content:    textContent(observation),
		ToolCallID: &id,
	}
	return asst, tool, true
}

// renderNativeParallelStep projects a trajectory step whose Action is a
// [planner.CallParallel] into the native multi-tool-call wire shape
// (Phase 107d — D-169 / AC-9): ONE assistant [llm.ChatMessage] whose
// `ToolCalls` slice carries every branch's `{ID, Name, Args}`, followed
// by N `RoleTool` messages — one per branch — each `ToolCallID` matched
// to its branch and `Content` set to that branch's projected
// observation. Every `tool_call_id` declared on the assistant message
// has exactly one matching `RoleTool` answer (the provider wire-contract
// invariant); the messages are emitted in branch-index order (JoinAll
// semantics).
//
// Branch CallIDs that are empty (a programmatic CallParallel, or a
// provider that omitted IDs) get a deterministic synthetic
// `react.callid.<step-index>.<branch-index>` stamped on BOTH the
// assistant tool-call entry AND the matching RoleTool message so the
// pairing stays well-formed.
//
// Per-branch observations are decomposed from the AC-4 aggregate
// ([planner.ParallelObservation]) carried on `step.LLMObservation`
// (preferred — the D-026 projected forms) or `step.Observation`. The
// aggregate's branches are matched to assistant tool-calls by their
// `Index` (the deterministic merge key — robust to empty/duplicate
// CallIDs). When no aggregate is present (e.g. the runloop wrapped a
// whole-call executor abort as a flat error map), the fallback body is
// rendered once and reused for every branch so the N-answers invariant
// still holds.
func renderNativeParallelStep(step planner.Step, call planner.CallParallel, replayMode planner.ReasoningReplayMode, stepIdx int) (llm.ChatMessage, []llm.ChatMessage) {
	toolCalls := make([]llm.ToolCallStructured, len(call.Branches))
	for bi, b := range call.Branches {
		cid := b.CallID
		if cid == "" {
			cid = fmt.Sprintf("react.callid.%d.%d", stepIdx, bi)
		}
		toolCalls[bi] = llm.ToolCallStructured{
			ID:   cid,
			Name: b.Tool,
			Args: json.RawMessage(safeArgs(b.Args)),
		}
	}

	// Assistant preamble + reasoning replay — same shape as the single
	// CallTool path (renderNativeStepPair).
	assistantText := step.AssistantPreamble
	if replayMode == planner.ReasoningReplayText && step.ReasoningTrace != "" {
		if assistantText != "" {
			assistantText += "\n\nReasoning:\n" + step.ReasoningTrace
		} else {
			assistantText = "Reasoning:\n" + step.ReasoningTrace
		}
	}
	var asstContent llm.Content
	if assistantText != "" {
		asstContent = textContent(assistantText)
	}
	asst := llm.ChatMessage{
		Role:      llm.RoleAssistant,
		Content:   asstContent,
		ToolCalls: toolCalls,
	}

	// Decompose the aggregate observation, indexed by branch Index.
	byIndex, hasAgg := parallelBranchBodiesByIndex(step)
	var fallbackBody string
	if !hasAgg {
		fallbackBody = renderParallelFallbackBody(step)
		if fallbackBody == "" {
			fallbackBody = "(tool returned no observation)"
		}
	}

	toolMsgs := make([]llm.ChatMessage, len(call.Branches))
	for bi := range call.Branches {
		body := fallbackBody
		if hasAgg {
			if b, ok := byIndex[bi]; ok {
				body = renderParallelBranchBody(b)
			} else {
				body = ""
			}
		}
		if body == "" {
			slog.Warn("react.renderNativeParallelStep: empty branch observation — emitting placeholder to preserve wire contract",
				"step_idx", stepIdx,
				"branch_idx", bi,
				"tool", call.Branches[bi].Tool,
				"call_id", toolCalls[bi].ID,
			)
			body = "(tool returned no observation)"
		}
		id := toolCalls[bi].ID
		toolMsgs[bi] = llm.ChatMessage{
			Role:       llm.RoleTool,
			Content:    textContent(body),
			ToolCallID: &id,
		}
	}
	return asst, toolMsgs
}

// parallelBranchBodiesByIndex extracts the AC-4 aggregate observation
// from a CallParallel step and returns a map keyed by branch Index.
// Prefers `step.LLMObservation` (the D-026 projected forms) over the raw
// `step.Observation`. Returns (nil, false) when neither slot carries a
// [planner.ParallelObservation].
func parallelBranchBodiesByIndex(step planner.Step) (map[int]planner.ParallelBranchObservation, bool) {
	for _, obs := range []any{step.LLMObservation, step.Observation} {
		var agg planner.ParallelObservation
		switch v := obs.(type) {
		case planner.ParallelObservation:
			agg = v
		case *planner.ParallelObservation:
			if v == nil {
				continue
			}
			agg = *v
		default:
			continue
		}
		out := make(map[int]planner.ParallelBranchObservation, len(agg.Branches))
		for _, b := range agg.Branches {
			out[b.Index] = b
		}
		return out, true
	}
	return nil, false
}

// renderParallelBranchBody renders one branch's observation body for its
// RoleTool message. Errors surface first (the planner needs to see
// them); otherwise heavy-content wrappers project through the existing
// inlined-preview path (D-026), falling back to the generic renderer.
func renderParallelBranchBody(b planner.ParallelBranchObservation) string {
	if b.Error != "" {
		return "Tool error: " + oneLine(b.Error)
	}
	if body, ok := renderHeavyContentObservation(b.Value); ok {
		return body
	}
	if b.Value != nil {
		return renderAny(b.Value)
	}
	return ""
}

// renderParallelFallbackBody renders a body for the degenerate case
// where a CallParallel step carries no per-branch aggregate (a
// whole-call executor abort the runloop wrapped as a flat error map, or
// a malformed trajectory). Surfaces failures / errors first, then any
// heavy-content wrapper, then the generic projection.
func renderParallelFallbackBody(step planner.Step) string {
	if step.Failure != nil {
		return fmt.Sprintf("Tool failure: %s — %s",
			step.Failure.Code, oneLine(step.Failure.Message))
	}
	if step.Error != "" {
		return "Tool error: " + oneLine(step.Error)
	}
	for _, obs := range []any{step.LLMObservation, step.Observation} {
		if body, ok := renderHeavyContentObservation(obs); ok {
			return body
		}
		if obs != nil {
			return renderAny(obs)
		}
	}
	return ""
}

// renderNativeObservation returns the tool-result content body for the
// native RoleTool message. Failures + errors surface first (the
// planner needs to see them to course-correct); otherwise the
// LLMObservation projection is preferred over the raw Observation per
// D-026 heavy-content discipline. Returns the empty string when no
// observation is available yet (the runtime appended the Action but
// dispatch hasn't completed).
//
// Heavy-content wrappers — `*llm.ArtifactStub` (multimodal
// materialiser) or the executor's truncation map (`{"preview":...,
// "artifact_ref":...}`) — are projected as the inlined preview text
// plus a positional [artifact_fetch] footer carrying the ref + size.
// The wrapper JSON never reaches the LLM. The footer wording avoids
// wrapper terminology so the LLM doesn't acquire a prior on the
// internal shape.
func renderNativeObservation(step planner.Step) string {
	if step.Failure != nil {
		return fmt.Sprintf("Tool failure: %s — %s",
			step.Failure.Code, oneLine(step.Failure.Message))
	}
	if step.Error != "" {
		return "Tool error: " + oneLine(step.Error)
	}
	if body, ok := renderHeavyContentObservation(step.LLMObservation); ok {
		return body
	}
	if body, ok := renderHeavyContentObservation(step.Observation); ok {
		return body
	}
	if step.LLMObservation != nil {
		return renderAny(step.LLMObservation)
	}
	if step.Observation != nil {
		return renderAny(step.Observation)
	}
	return ""
}

// renderHeavyContentObservation detects the two known heavy-content
// wrapper shapes and returns the inlined preview text + a positional
// fetch-hint footer when the payload is truncated. Returns
// (body, true) when the input matched a wrapper shape; (body, false)
// otherwise (the caller falls through to the standard renderAny path).
//
// Recognised shapes (D-026 heavy-content boundary):
//
//  1. `*llm.ArtifactStub` — the multimodal materialiser shape from
//     `internal/llm/materialize.go`. `Summary` is treated as the
//     preview text; when empty (the common case for tool-result
//     materialisation), the body is a minimal "(no preview)" marker
//     plus the fetch hint so the LLM still sees the ref.
//
//  2. `map[string]any` carrying both `preview` and `artifact_ref`
//     keys — the runtime tool-executor's `heavyTruncationSummary`
//     shape (`cmd/harbor/cmd_dev_executor.go::heavyTruncationSummary`).
//     `preview` is the head bytes of the JSON-encoded payload;
//     `truncated: true` is the executor's explicit signal that the
//     full bytes live in the artifact store under `artifact_ref`.
//
// Adopted convention (no new ArtifactStub field): if the observation
// matched one of the two wrapper shapes, the preview MAY be
// truncated → always emit the fetch-hint footer. Callers that need
// to suppress the hint pass a non-wrapper observation. Documented
// here so the choice doesn't drift.
func renderHeavyContentObservation(obs any) (string, bool) {
	if obs == nil {
		return "", false
	}
	if stub, ok := obs.(*llm.ArtifactStub); ok && stub != nil {
		return renderArtifactStubObservation(stub), true
	}
	if m, ok := obs.(map[string]any); ok {
		if body, matched := renderHeavyContentMap(m); matched {
			return body, true
		}
	}
	return "", false
}

// renderArtifactStubObservation projects an `*llm.ArtifactStub` into
// the inlined-preview-plus-footer body shape. Uses `Summary` as the
// preview text (the materialiser's operator-overridable per-producer
// description; D-026); empty Summary falls back to a minimal marker
// so the LLM still sees the ref + the fetch hint.
func renderArtifactStubObservation(stub *llm.ArtifactStub) string {
	preview := oneLine(stub.Summary)
	if preview == "" {
		preview = "(no preview available)"
	}
	return preview + " " + artifactFetchFooter(stub.Ref, stub.MIME, stub.SizeBytes)
}

// asString returns v as a string, or "" when v is absent or not a
// string. Centralises the best-effort `any → string` extraction the
// heavy-content projection does over untyped observation maps.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// renderHeavyContentMap detects the runtime tool-executor's
// `heavyTruncationSummary` shape and projects it for the LLM.
// Returns (body, true) when the wrapper map carries a usable
// `preview` and/or `artifact_ref`; (body, false) otherwise. Picks
// the field-aware vs byte-truncation footer variant via
// [isFieldAwarePreview].
func renderHeavyContentMap(m map[string]any) (string, bool) {
	previewRaw, hasPreview := m["preview"]
	refRaw, hasRef := m["artifact_ref"]
	if !hasPreview && !hasRef {
		return "", false
	}
	preview := asString(previewRaw)
	ref := asString(refRaw)
	if ref == "" && preview == "" {
		return "", false
	}
	mime := asString(m["mime"])
	var size int64
	switch v := m["size_bytes"].(type) {
	case int:
		size = int64(v)
	case int64:
		size = v
	case float64:
		size = int64(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			size = parsed
		}
	}
	preview = oneLine(preview)
	if preview == "" {
		preview = "(no preview available)"
	}
	if ref == "" {
		// preview-only — no fetch hint possible. Returning matched=true
		// still routes through the inlined-preview path so the LLM
		// doesn't see the wrapper JSON.
		return preview, true
	}
	// Two footer variants — the field-aware variant names the
	// omitted-field sentinels as the unit artifact_fetch retrieves;
	// the byte-truncation variant says "full payload available."
	if isFieldAwarePreview(preview) {
		return preview + "\n\n" + artifactFetchFooterFieldAware(ref, mime, size), true
	}
	return preview + " " + artifactFetchFooter(ref, mime, size), true
}

// isFieldAwarePreview returns true when `s` parses as JSON AND
// contains the `[omitted: N bytes]` sentinel pattern that the
// field-aware preview emits for pruned fields. Used to pick the
// matching artifactFetchFooter* variant.
func isFieldAwarePreview(s string) bool {
	if !strings.Contains(s, `"[omitted:`) {
		return false
	}
	var probe any
	return json.Unmarshal([]byte(s), &probe) == nil
}

// artifactFetchFooterFieldAware is the footer variant for field-aware
// previews. The preview above already shows every scalar field; the
// only thing artifact_fetch retrieves is the specific fields marked
// `[omitted: N bytes]`. The wording names those sentinels as the
// retrieval unit and explicitly tells the model not to re-call the
// upstream tool (which would produce the same preview with the same
// omissions).
func artifactFetchFooterFieldAware(ref, mime string, size int64) string {
	var b strings.Builder
	b.WriteString(`[Fields marked "[omitted: N bytes]" above were pruned to fit context — call artifact_fetch(ref="`)
	b.WriteString(ref)
	b.WriteString(`") to retrieve the full payload`)
	if size > 0 {
		fmt.Fprintf(&b, " (size: %d bytes", size)
		if mime != "" {
			b.WriteString(", mime: " + mime)
		}
		b.WriteString(")")
	}
	b.WriteString(". The scalar fields above are complete; do not re-call the upstream tool — it will return the same preview with the same omissions.]")
	return b.String()
}

// artifactFetchFooter renders the positional fetch-hint footer for
// byte-truncated previews (non-JSON or non-field-aware). Names the
// ref + optional MIME + optional size; no wrapper-shape terminology.
func artifactFetchFooter(ref, mime string, size int64) string {
	var b strings.Builder
	b.WriteString("[Full payload available")
	first := true
	writeKV := func(k, v string) {
		if v == "" {
			return
		}
		if first {
			b.WriteString(" (")
			first = false
		} else {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
	}
	if size > 0 {
		writeKV("size", fmt.Sprintf("%d bytes", size))
	}
	writeKV("mime", mime)
	if !first {
		b.WriteString(")")
	}
	b.WriteString(` — call artifact_fetch(ref="`)
	b.WriteString(ref)
	b.WriteString(`") to retrieve it.]`)
	return b.String()
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
