# Phase 107c — Native tool-calling + deferred tools/skills + search meta-tools

## Summary

Alternative shape to Phase 107b. Where 107b adds a JSON-string-buffer extractor to gate which bytes of a prompt-engineered `{tool, args}` action reach the chunk channel, 107c **eliminates the prompt-engineered JSON shape itself** by switching the React planner to native provider tool-calling. The LLM returns a structured `Message{Content, ToolCalls, ReasoningContent}` with separate typed fields; Content deltas ARE the user-facing prose (no extractor needed), tool-call structures arrive on their own channel, reasoning streams cleanly on a third. Streaming + reasoning streaming become structural properties of the wire, not code that has to gate them.

Layered on top: deferred tool/skill loading (predecessor's pattern, adapted to Harbor's primitives). Each tool/skill declares `loading_mode: always | deferred` in the agent yaml. Always-loaded tools render in every LLM request's `Tools[]` declaration. Deferred tools live in a SQLite FTS5 search cache (mirroring `internal/skills/drivers/localdb/`); the LLM discovers them through four built-in meta-tools — `tool_search`, `tool_get`, `skill_search`, `skill_get` — and the discovered tool joins the next turn's `Tools[]` declaration. Tag filtering rides on every `*_search` meta-tool. Skill discovery uses the existing `skills.SkillStore.Search` surface (FTS5 already in place); tools get the analogous cache as a new driver.

Compatibility carve-out: an optional `declarative_action` built-in meta-tool (off by default; operator opts in per agent yaml) accepts a `{tool, args}` JSON body and dispatches through the existing prompt-engineered parser path. Models without reliable native tool-calling support — local Llama / Mistral / weaker fine-tunes — can be configured to use `declarative_action` exclusively (a single deferred tool the LLM discovers when it needs a structured-call format). This collapses the would-be `react` + `react-native` two-concrete carve-out into ONE planner concrete with ONE optional escape-hatch tool.

**Scope honesty (revised after coordinator audit):** this is a large plan — ~35 ACs across the LLM client, tools subsystem, React planner, prompt builder, repair loop, runloop executor, and agent-yaml config. Realistic single-agent dispatch is **10–15 days** of focused work. The earlier 5–10 day estimate was optimistic; the React package alone is ~7500 LOC including ~1900 LOC of prompt-building + ~1900 LOC of test fixtures asserting the prompt's exact shape — the cutover touches a meaningful slice of that. It ships the right long-term architecture in one cutover; Phase 107b's `streamAnswerFilter` becomes unnecessary and is NOT built. If the implementing agent runs into a blocking unknown, they should pause and ask rather than ship a partial migration — the pause-and-ask checkpoints at the end of this plan are load-bearing.

**Critical scope constraint — parallel-tool-call serialization carve-out.** The dev executor at `cmd/harbor/cmd_dev_executor.go:100` returns `ErrDecisionShapeUnsupported` for `CallParallel` decisions today (a documented post-V1.1 deferral). Adding the executor's CallParallel dispatch (goroutine fanout + `JoinSpec` evaluation + per-branch identity propagation) is a separate body of work outside this plan's scope. Phase 107c therefore takes a defensive posture: when the LLM emits N native ToolCalls in one message, the React planner SERIALIZES them by emitting `CallTool` for the FIRST call and recording the remaining calls on per-run state; on the next planner step, after observing the first call's result, the planner emits `CallTool` for the next pending call (and so on). The LLM perceives one call per turn; the runtime sees one CallTool per turn; the operator gets correct semantics with sequential dispatch. This is documented as a known limitation in the plan; a follow-up phase (110z or equivalent) extends the executor to dispatch CallParallel natively, at which point the planner's serialization fallback becomes a single-line opt-out.

**Primitives that already exist** (the agent should verify and reuse, not rebuild):

- `tools.LoadingMode` enum + `LoadingAlways` / `LoadingDeferred` constants — already declared at `internal/tools/tools.go:79-84`.
- `tools.Tool.Loading LoadingMode` field — already declared at `internal/tools/tools.go:139`.
- `tools.CatalogFilter.LoadingModes []LoadingMode` filter — already declared at `internal/tools/tools.go:244`. The catalog filter machinery is in place; what's missing is (a) operator-yaml wiring to set per-tool `loading_mode`, (b) the planner using the filter to narrow the always-loaded set, (c) the `SearchCache` driver + meta-tools that surface deferred tools to the LLM.
- `tools.Tool.Tags []string` + `tools.Tool.AuthScopes []string` — already declared at `internal/tools/tools.go:97,124-128`. Tag-based filtering primitives are in place; the `tool_search` meta-tool consumes them.
- `skills.SkillStore.Search(ctx, identity, query, limit)` — already in place via `internal/skills/drivers/localdb/`. The `skill_search` meta-tool wraps this surface.
- `tools/builtin` registration shape — already shipped at Phase 83n (D-153) with `clock.now` + `text.echo`. The five new meta-tools register through the same `builtin.Register(catalog, config)` seam.
- Brief 07's design rationale (the prompt-engineered tool-calling that Phase 107c reverses for the React planner) is the architectural baseline the agent must understand to do this migration honestly — read `docs/research/07-code-level-tool-calling.md` BEFORE touching the React planner. The reversal is documented in "Findings I'm departing from" below.

## RFC anchor

- RFC §6.2 — Planner interface + reasoning policy (the React planner concrete's parser path changes; the `Decision` sum contract is unchanged).
- RFC §6.4 — Tool catalog + transports (`Tool` gains `LoadingMode`; the catalog gains a per-tag search surface).
- RFC §6.5 — LLM client streaming + tool-calling contract (`CompleteResponse` gains `ToolCalls`; `CompleteRequest` gains `Tools[]` + `ParallelToolCalls`).
- RFC §6.7 — Skills subsystem (existing `SkillStore.Search` surfaces as a built-in meta-tool; no SkillStore change).
- RFC §1 — first-five-minutes adoption guarantee (streaming becomes clean by construction; reasoning streams live; catalog scales beyond the prompt-budget ceiling).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the React planner's decision-emission contract; the Decision sum survives the migration).
- brief 03 — tools + integrations + LLM client (the bifrost native-tool-calling surface; `Tools[]` declaration shape).
- brief 04 — memory + skills (the skills subsystem's existing FTS5 search; new meta-tool surfaces `SkillStore.Search` to the LLM).
- brief 07 — code-level tool calling (the "tools are first-class structured calls, not parsed strings" principle).
- brief 13 — react planner prompt engineering (what the prompt sheds when JSON-action instructions are removed; what the `<tool_discovery>` section adds).
- brief 15 — native tool-calling + deferred loading + tag scoping (the architectural rationale + carve-out analysis; this plan implements Path B from brief 15 in a single phase).

## Brief findings incorporated

- **brief 15 §6 "Decision-sum invariance".** "The Decision sum is the planner→runtime contract; it's orthogonal to how the planner parses its LLM input. Mapping is straightforward: 1 native ToolCall → CallTool; N native ToolCalls → CallParallel; 0 ToolCalls + Content → Finish." Phase 107c implements this mapping verbatim; the Runtime's per-branch goroutine fanout + `JoinSpec` + `MaxParallel` cap (RFC §6.2) are unchanged.
- **brief 15 §3 "Deferred loading"**. "Mark each tool with `loading_mode` (`always` / `deferred`). Always-tools declared every turn alongside meta-tools `tool_search` + `tool_get`; the LLM discovers deferred tools, the planner adds them to the next turn's `Tools[]` declaration." Phase 107c ships exactly this pattern.
- **brief 15 §5 "What Harbor already has"**. "`tools.Tool.Tags`, `tools.Tool.AuthScopes`, `tools.Tool.HandlesMIME`, `skills.SkillStore.Search` (FTS5-backed), `directory.CapabilityFilter` — primitives in place. Missing: `ToolLoadingMode` enum, `tool_search`/`tool_get` builtins, `tools.SearchCache`." Phase 107c closes exactly these gaps; skills already have the search surface, so skill discovery is just a new built-in meta-tool over the existing seam.
- **brief 07 §"Code-level tool calling principle".** "Tool calls are structured invocations the runtime dispatches through typed signatures; the LLM's job is to declare intent, not to format JSON." Phase 107c moves Harbor onto this principle for providers that support it natively, while preserving the prompt-engineered escape hatch as the `declarative_action` opt-in tool for providers that don't.
- **brief 04 §"Skills + Tags"**. "Skills carry `RequiredTags` and `Tags`; the `directory.CapabilityFilter` projection narrows the visible skill set per identity." Phase 107c's `skill_search` meta-tool consumes the same surface — tag filtering rides through verbatim.

- **brief 07 §1 "Code-level tool calling principle".** Brief 07 is THE settled Harbor architectural principle that biased AGAINST native provider tool-calling: "Harbor performs tool calling at the runtime/orchestration layer, not at the LLM provider layer ... no `tools=`, no `tool_choice=`, no `function_call`, no provider-native tool-call ID ... parallel tool calling and tool calling in general therefore work uniformly across every provider that can emit JSON". Phase 107c is a deliberate REVERSAL of this principle for the React planner concrete only. The justification: the principle's load-bearing value — uniform behavior across providers — has been validated; modern provider APIs (OpenAI / Anthropic / Gemini) have converged on a compatible structured tool-call shape, so the "uniformity" axis can now be paid for at the LLM-client mapping layer (bifrost) instead of the planner layer. The principle SURVIVES in the `declarative_action` escape-hatch meta-tool — operators with weaker / non-tool-calling providers opt that tool in, and the runtime dispatches through the existing `repair.ActionParser` (the brief 07 path is preserved as a per-tool capability, not a planner-wide default). The cost of the reversal — provider parity becomes a bifrost-layer concern instead of a planner-layer concern — is borne by ~80 LOC of bifrost mapping (AC-3) rather than by the planner's prompt engineering complexity. Document this departure with a new `docs/decisions.md` D-NNN entry per AC-32.
- **brief 15 §7 "Keep both concretes (`react` + `react-native`)".** Brief 15 sketched a two-planner-concretes carve-out (prompt-engineered React for weaker models, native React for tool-calling models). Phase 107c collapses this to ONE concrete with an optional escape-hatch tool (`declarative_action`). Justification: the carve-out's purpose is operator-visible compatibility for weaker models; a single deferred meta-tool that exposes the prompt-engineered shape achieves the same thing with much less duplication. The two-concretes shape was the conservative scoping; the meta-tool collapse is the cleaner architecture once you see it.
- **brief 13 §"Narrow action schema (`{tool, args}` with `_finish` reserved)".** The narrowed schema was the Phase 83a–e settled answer for prompt-engineered React. Phase 107c retires it as the planner's primary input shape — `resp.ToolCalls` becomes the discriminator. The `{tool, args}` schema survives in ONE place: the `declarative_action` meta-tool's input shape. A future schema change there only affects that one tool's contract.

## Goals

- **Native tool-calling end-to-end.** `llm.CompleteResponse` carries `ToolCalls []ToolCallStructured` (per-call ID + name + args bytes); `llm.CompleteRequest` carries `Tools []ToolDeclaration` (per-tool name + description + JSON schema) + `ParallelToolCalls bool`. The bifrost driver maps these to/from its upstream providers (OpenAI / Anthropic / Gemini all support; OpenRouter routes through).
- **React planner reads native ToolCalls.** Replace `ActionParser` (JSON-from-Content) with a `ToolCallProjector` (structured-array reader). Decision mapping: 1 ToolCall → `CallTool`; N ToolCalls → `CallParallel{Branches, Join: JoinAll}`; 0 ToolCalls + non-empty `Content` → `Finish{Reason: Goal, Payload: Content}`.
- **Per-tool `LoadingMode`.** Each tool declares `always` or `deferred` in the agent yaml. Operators control which tools render in the catalog every turn.
- **`tools.SearchCache` (SQLite FTS5).** New driver under `internal/tools/drivers/searchcache/` mirroring `internal/skills/drivers/localdb/`. Indexes tool name + description + tags. Backed by FTS5 with a regex fallback for environments without FTS5.
- **Four built-in meta-tools** at `internal/tools/builtin/`: `tool_search(query, tags[], limit)`, `tool_get(name)`, `skill_search(query, tags[], limit)`, `skill_get(name)`. Each is a normal `tools.Tool` registered alongside `clock.now` + `text.echo` (Phase 83n shipped this pattern).
- **Per-run discovered-tools state.** The React planner tracks tools the LLM has discovered (via `tool_search` results referenced in subsequent tool_calls) and adds them to the next turn's `Tools[]` declaration. Lives on per-run `RunContext` (D-025 — never on the planner struct).
- **`declarative_action` optional meta-tool.** Off by default. When operator enables it (`tools.builtin.declarative_action: enabled: true`), it appears in the catalog and accepts a `{tool, args}` JSON body; the meta-tool's implementation dispatches through the existing `repair.ActionParser` + dispatcher path. The tool itself can be marked deferred so it only loads on `tool_search("structured action format")` or similar.
- **Streaming becomes free.** The runloop's `OnChunk` closure forwards `Content` deltas as `kind: ChunkContent` with NO filter. Reasoning deltas forward as `kind: ChunkReasoning` with NO filter. Phase 107b's `streamAnswerFilter` is not built; if 107b already shipped, this phase deletes `internal/planner/react/stream_filter.go` in one PR.
- **Backward-compatible CompleteRequest behavior.** A request with `Tools: nil` (no tool declarations) calls the provider without the tool-calling block — preserves text-only completion for non-React planners (Deterministic, future Workflow) that don't use tool-calling.

## Non-goals

- **Two React planner concretes.** Phase 107c ships ONE planner (`internal/planner/react/` continues, but its parser changes shape). The compatibility floor for non-tool-calling providers is the optional `declarative_action` meta-tool, NOT a parallel `internal/planner/react-legacy/` package.
- **Console "tool discovery trace" panel.** Operators can inspect discovered tools via `inspect-runs` + `tasks.get`'s trajectory; a dedicated Console UI for "which deferred tools did the LLM discover this run" is deferred to a follow-up phase (Phase 73f extension or post-V1.4).
- **Operator-yaml schema evolution beyond `loading_mode` + meta-tool enablement.** The yaml gains two new fields (per-tool `loading_mode` + per-builtin `enabled`); no other config-shape changes.
- **Migration of non-React planners.** The Deterministic planner (Phase 48) doesn't use tool-calling at all; no change. A future Workflow planner that wants tool-calling reuses the same `Tools[]` + `ToolCalls` LLM-client surface; that's a per-planner-concrete adoption decision.
- **Multi-turn cache invalidation of discovered tools across runs.** Discovered tools are per-run state (added to `RunContext.DiscoveredTools` or equivalent). A new run starts with the always-loaded set fresh; the LLM rediscovers as needed. A future Phase 110-band can layer session-scoped discovery caching if operators want it.
- **Tag taxonomy governance.** Tags are operator-supplied free-text (matches the existing `Tool.Tags` posture). A recommended taxonomy + Console UI for "your most-used tags" is post-V1.4.
- **Streaming-aware parallel tool-call rendering in the Console.** When the LLM emits multiple `ToolCalls` in one streaming response, the chat bubble renders tool-call cards on `task.completed` from the trajectory (today's behavior). Live per-call streaming of tool-args is post-V1.
- **Provider-specific reasoning-stream formats.** Phase 107c forwards `OnReasoning` deltas as `kind: ChunkReasoning` chunks — provider-native reasoning streams (Anthropic extended-thinking signed blocks, OpenAI o-family thought tokens) ride through as opaque text. Structured reasoning rendering is a Phase 107a follow-up.

## Acceptance criteria

The bullets below are binding. Grouped for the implementing agent's clarity; numbering is sequential.

### LLM client surface

- [ ] **AC-1** `llm.CompleteResponse` gains `ToolCalls []ToolCallStructured` field. New type `ToolCallStructured = {ID string; Name string; Args json.RawMessage}` lives in `internal/llm/types.go` (or wherever `CompleteResponse` is declared). `ID` is the provider-assigned call id (round-trips on tool_result), `Name` is the tool name (matches `tools.Tool.Name`), `Args` is provider-validated JSON (already validated against the declared schema).
- [ ] **AC-2** `llm.CompleteRequest` gains `Tools []ToolDeclaration` + `ParallelToolCalls bool` fields. New type `ToolDeclaration = {Name string; Description string; Schema json.RawMessage}`. `Tools: nil` means "no tool-calling on this request" (text-only completion path, preserves non-React planner behavior). `ParallelToolCalls` defaults to `true` for providers that support it; bifrost's mapping suppresses it for providers that don't.
- [ ] **AC-3** bifrost driver (`internal/llm/drivers/bifrost/bifrost.go`) maps `req.Tools` → bifrost's `BifrostChatRequest.Tools` (or equivalent), and `BifrostChatResponse.ToolCalls` → `resp.ToolCalls`. Streaming path (`ChatCompletionStreamRequest`) maps `OnContent`/`OnReasoning` callbacks as today AND surfaces incremental `ToolCalls` deltas if the provider streams them (currently most providers send tool_call as a complete struct at end-of-stream; the per-delta tool_call stream is a future-proofing seam — assemble structured ToolCalls from the final response).
- [ ] **AC-4** mock LLM driver (`internal/llm/drivers/mock/`) gains support for scripted ToolCalls responses — a fixture mock returns `{Content: "", ToolCalls: [{Name: "search", Args: "{\"q\":\"x\"}"}]}` for tests. Existing mock fixtures using `Content: '{"tool":"search","args":{"q":"x"}}'` continue to work (the mock can return either; the test suite migrates to the structured form alongside the React planner rewrite).

### Tools subsystem

- [ ] **AC-5** `tools.Tool` gains `LoadingMode tools.LoadingMode` field. New enum `LoadingMode string` with constants `LoadingAlways = "always"` (default) and `LoadingDeferred = "deferred"`. Zero value is `LoadingAlways` — every existing tool's behavior is unchanged.
- [ ] **AC-6** `config.ToolEntry` (the agent-yaml tool-entry config block) gains a `loading_mode string` field. Validator (in `internal/config/validate.go`) rejects unknown values pre-boot per CLAUDE.md §13 fail-loud; valid values are `always` / `deferred` / `""` (empty defaults to `always`).
- [ ] **AC-7** `tools.Catalog` gains a `Search(ctx, query string, tags []string, limit int) []tools.Tool` method. Backed by a `tools.SearchCache` (AC-8). A catalog without an attached cache returns an empty slice (no panic; honest "discovery unavailable"). Tag filter is intersection — `tags: ["mcp", "filesystem"]` matches tools that carry BOTH tags.
- [ ] **AC-8** **NEW** `internal/tools/drivers/searchcache/` — SQLite FTS5-backed search cache for tools. Mirrors `internal/skills/drivers/localdb/`'s shape: schema migration with fingerprint check, FTS5 index over `name + description + tags`, regex fallback for environments without FTS5. Driver registers itself via the standard §4.4 factory pattern.
- [ ] **AC-9** `tools.Catalog.Sync()` (or a per-driver hook) populates the SearchCache on every catalog change — tool registration / unregistration / yaml reload (Phase 65 hot-reload). The cache fingerprint detects no-op syncs; only changed entries are re-indexed.

### Meta-tools (built-in)

- [ ] **AC-10** **NEW** `internal/tools/builtin/tool_search.go` — registers the `tool_search` builtin. Signature: `(query string, tags []string, limit int) → []{name, description, tags}`. Implementation calls `catalog.Search(ctx, query, tags, limit)`. Tags filter passes through. Default limit = 10, max = 50.
- [ ] **AC-11** **NEW** `internal/tools/builtin/tool_get.go` — registers the `tool_get` builtin. Signature: `(name string) → {name, description, args_schema, examples}`. Calls `catalog.Resolve(name)`; returns full schema + examples; returns error if tool not found.
- [ ] **AC-12** **NEW** `internal/tools/builtin/skill_search.go` + `skill_get.go` — analogous to AC-10 + AC-11 but route through the existing `skills.SkillStore.Search` + `Get` surface. The skills subsystem already has FTS5 via `internal/skills/drivers/localdb`; no new driver needed.
- [ ] **AC-13** **NEW** `internal/tools/builtin/declarative_action.go` — the optional escape-hatch tool. Signature: `(action json.RawMessage) → {dispatched: bool, error: string}`. Implementation parses the JSON body via `repair.ActionParser` (the existing Phase 44 parser), then dispatches the resulting `CallTool` through the runtime's tool executor. OFF by default; operator opts in via `tools.builtin.declarative_action.enabled: true` in `harbor.yaml`. The tool's `LoadingMode` defaults to `deferred` — the LLM only sees it after `tool_search("structured action format")` or similar, keeping the prompt budget clean when not needed.
- [ ] **AC-14** All five meta-tools (`tool_search` / `tool_get` / `skill_search` / `skill_get` / `declarative_action`) register through the standard `builtin.Register(catalog, config)` path the existing builtins use (Phase 83n / D-153 — `clock.now`, `text.echo`). Operator yaml controls per-builtin enablement (`tools.builtin.tool_search.enabled: true` etc). Default-enabled: `tool_search` + `tool_get` + `skill_search` + `skill_get`. Default-disabled: `declarative_action`.

### React planner migration

- [ ] **AC-15** `internal/planner/react/` (the existing package — NO new `react-native/` sibling) replaces its response-parsing path with a `ToolCallProjector` that reads `resp.ToolCalls` instead of parsing JSON from `resp.Content`. The `repair.ActionParser` is retained but only called from the `declarative_action` meta-tool's implementation — no other call site.
- [ ] **AC-16** React's prompt builder (`internal/planner/react/prompt_builder.go` or the Phase 83a–e prompt assembly site) drops the `<action_format>` JSON instruction. The `<available_tools>` section renders ONLY always-loaded tools — deferred tools are absent (the meta-tools' descriptions tell the LLM how to discover them). A new `<tool_discovery>` section instructs the LLM about deferred semantics: "use `tool_search` to find capabilities; `tool_get` for schemas; `skill_search`/`skill_get` for skill playbooks; deferred tools may be called once discovered."
- [ ] **AC-17** React planner's per-step `Next()` constructs `req.Tools` from: (a) the always-loaded subset of the run's visible catalog (filtered by `LoadingMode` + identity scope + `GrantedScopes`); (b) the meta-tools (always present when enabled); (c) per-run discovered tools (AC-18). Sets `req.ParallelToolCalls = true` by default.
- [ ] **AC-18** Per-run discovered-tools state lives on `RunContext.DiscoveredTools []string` (new field — names only; the planner resolves to declarations via `catalog.Resolve`). Populated by the React planner when it observes a `tool_search` tool-call result. Lives stack-local-per-run (D-025 — no field on the planner struct). The runtime pre-clears `DiscoveredTools` at run start; the field accumulates across planner steps within ONE run.
- [ ] **AC-19** Decision mapping in React's response handler:
  - `len(resp.ToolCalls) == 0 && resp.Content != ""` → `Finish{Reason: Goal, Payload: resp.Content}` (the model's natural-language answer is the payload).
  - `len(resp.ToolCalls) == 1` → `CallTool{Tool: resp.ToolCalls[0].Name, Args: resp.ToolCalls[0].Args, CallID: resp.ToolCalls[0].ID}`.
  - `len(resp.ToolCalls) > 1` → **serialization fallback per the scope constraint above**. Emit `CallTool` for `resp.ToolCalls[0]`; record `resp.ToolCalls[1:]` on `RunContext.PendingToolCalls` (new per-run field — AC-19a). On the next `Next()` call, if `rc.PendingToolCalls` is non-empty, emit `CallTool` for `PendingToolCalls[0]` and shrink the slice. The planner consumes pending calls before consulting the LLM again. When the operator-yaml sets `planner.react.parallel_tool_calls: true` AND the runloop executor advertises CallParallel support (a capability flag set at boot), the planner emits `CallParallel{Branches: [...], Join: JoinAll, MaxParallel: rc.Hints.MaxParallel}` instead — but the default V1.3 behavior is serialization.
  - All other shapes (e.g. empty Content + empty ToolCalls) → planner emits `Finish{Reason: NoPath, Metadata: {"followup": true}}` per existing graceful-failure pattern.
- [ ] **AC-19a** New per-run field `planner.RunContext.PendingToolCalls []ToolCallStructured` (companion to `DiscoveredTools` from AC-18). Stack-local-per-run (D-025). The React planner appends to this slice when AC-19's multi-ToolCall serialization fallback fires; the planner drains the slice over subsequent steps before issuing a new LLM call. Empty by default — single-tool-call responses never populate it.
- [ ] **AC-20** Reserved discriminator `_finish` is RETIRED from the prompt + the parser. Models that emit a finish action under the new shape simply produce a non-tool-calling response (`Content: "answer"`, `ToolCalls: []`). The `declarative_action` meta-tool DOES retain `_finish` in its body for backward compatibility with the prompt-engineered shape — its parser is `repair.ActionParser`, which knows `_finish`.

### Tool-result round-trip + prompt assembly

- [ ] **AC-20a** **Tool-result message shape.** Native tool-calling requires tool results to be threaded back into the next LLM turn's `Messages` slice as a structured tool-result message — specifically `ChatMessage{Role: RoleTool, ToolCallID: id, Content: result}`. `llm.ChatMessage` gains a `ToolCallID string` field (new — at `internal/llm/llm.go` near the existing `RoleTool` constant). The React prompt assembly (today in `internal/planner/react/prompt.go`'s message-builder path) projects each `trajectory.Step` whose `Action` is a `CallTool` AND whose `Observation` is non-nil into a pair: an assistant message carrying the `ToolCalls: [the call's structured ToolCall]` field + a `RoleTool` message carrying the observation as `Content` with the matching `ToolCallID`. The prompt builder NO LONGER renders trajectory steps as user-role tool-observation strings (the brief 07 pattern); the native shape uses provider-typed tool-result messages.
- [ ] **AC-20b** **`trajectory.Step.Action` shape change.** Today `trajectory.Step.Action` is `any` (declared at `internal/planner/trajectory/trajectory.go:110`) — Phase 83e records the parsed `planner.CallTool`. Under native tool-calling, the Action stores the same `planner.CallTool` but ALSO carries the provider-assigned `CallID` from `ToolCallStructured.ID` (already present on the Decision sum after AC-19). The prompt builder (AC-20a) round-trips this ID back to the next-turn `RoleTool` message. NO new trajectory wire-shape changes beyond field presence; the existing `Action any` slot holds it.
- [ ] **AC-20c** **Repair loop interaction under native path.** The existing `repair.RepairLoop.Run(ctx, rc, client, req, validator)` call site at `internal/planner/react/react.go:490` continues to fire on every LLM call — both native and `declarative_action` paths. The loop's behavior changes per response shape:
  - **Native response (`resp.ToolCalls` non-empty, `resp.Content` may be a preamble).** The repair loop's salvage / schema-repair / multi-action-salvage path is BYPASSED — the ToolCallProjector (AC-15) reads `resp.ToolCalls` directly. The loop still runs (and fires `OnContent`/`OnReasoning` streaming callbacks), but its `parser.Parse(resp.Content)` call is a no-op for native responses (parser returns "no actions found"; the projector path takes over).
  - **`declarative_action` response.** The model invokes the `declarative_action` meta-tool with `{tool, args}` JSON in its `args.action` field. The meta-tool's implementation calls `repair.ActionParser` (the existing brief-07 parser) — fully preserved behavior.
  - **Empty-content + empty-ToolCalls response.** The repair loop's existing graceful-failure path fires; React emits `Finish{NoPath}`. Unchanged.
  Document this clearly in `internal/planner/react/react.go`'s package godoc.
- [ ] **AC-20d** **Prompt-restructure scope honesty.** `internal/planner/react/prompt.go` is 1061 LOC; `prompt_test.go` is 943 LOC. The migration touches:
  - **DELETE** the `<action_format>` (or equivalent) section that instructs the LLM about the `{tool, args}` JSON shape.
  - **NARROW** `<available_tools>` to render `{name, description}` ONLY (not full schemas — schemas now live in `req.Tools[]`). The section becomes a quick-reference + a pointer to the provided tools list.
  - **ADD** `<tool_discovery>` section instructing the LLM about meta-tools (`tool_search` / `tool_get` / `skill_search` / `skill_get`) + the deferred-loading semantics ("tools surfaced by tool_search are callable on the next turn").
  - **PRESERVE** every other section verbatim — `<skills_context>`, `<read_only_*_memory>`, `<planning_constraints>`, `<repair_guidance>`, `<reasoning_replay>`, the question / quadruple / clock blocks.
  - `prompt_test.go` rewrites: every assertion that pins the `<action_format>` section's literal bytes is DELETED; every assertion that pins `<available_tools>` is RELOOSENED to assert presence of tool names but not schemas; assertions on `<skills_context>` / memory / planning hints stay verbatim. Expect ~40–60 test-site changes; per-test inspection required (mechanical sed-style replace is unsafe).
- [ ] **AC-20e** **Audit event payload shapes.** The runtime emits `tool.invoked` events (and similar) today carrying the parsed action payload. Under native tool-calling, the `tool.invoked` event's payload still includes `{name, args}` — verify it sources from the new structured CallID-carrying `CallTool` Decision instead of a parsed JSON object. No wire-shape change required (the existing event payload's fields are already `name` + `args`); just verify the source data path. Tests in `internal/runtime/steering/runloop_test.go` may need fixture updates if they assert on JSON-parsed shape.

### Runloop + chunk-emit

- [ ] **AC-21** `cmd/harbor/cmd_dev_runloop.go`'s `onChunk` closure is UNCHANGED. It already wires `kind: ChunkContent` and `kind: ChunkReasoning` per Phase 107. Phase 107b's `streamAnswerFilter` wrap site (if 107b shipped first) is REMOVED — chunks flow directly from bifrost's `OnContent` / `OnReasoning` callbacks to `rc.OnChunk` without gating. The chunk channel now carries clean prose by structural construction.
- [ ] **AC-22** If `internal/planner/react/stream_filter.go` exists (Phase 107b shipped before 107c), it is DELETED in the same PR. The streaming-filter wrap site in React's `Next()` is removed. Phase 107b becomes a superseded plan (marked in `docs/plans/README.md`).

### Tests

- [ ] **AC-23** Unit (Go) — `internal/tools/drivers/searchcache/searchcache_test.go`: schema migration; FTS5 query with tag filter (intersection semantics); regex fallback when FTS5 unavailable; cache fingerprint dedup; concurrent reads under `-race`.
- [ ] **AC-24** Unit (Go) — `internal/tools/builtin/tool_search_test.go` (and siblings for `tool_get`, `skill_search`, `skill_get`, `declarative_action`): each meta-tool's invocation against a stub catalog/store; error paths (tool not found, invalid args); identity propagation through the call.
- [ ] **AC-25** Unit (Go) — `internal/planner/react/projector_test.go`: maps `resp.ToolCalls` shapes to Decisions per AC-19 (single → CallTool, multi → CallParallel, none + content → Finish, empty → NoPath).
- [ ] **AC-26** Integration (Go) — `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_DiscoveryCycle`: scripted streaming LLM client. Turn 1: LLM emits `ToolCalls: [{Name: "tool_search", Args: {"query": "youtube download", "limit": 5}}]`. Runtime dispatches; planner observes results; adds discovered tool name to `RunContext.DiscoveredTools`. Turn 2: LLM emits `ToolCalls: [{Name: "youtube_download", Args: {...}}]` — discovered tool dispatches through normal CallTool path. Assert: full discovery cycle completes; identity propagates through both turns; trajectory captures both steps.
- [ ] **AC-27** Concurrent-reuse (Go) — `internal/planner/react/concurrent_test.go::TestReactPlanner_NativeToolCall_NoCrossTalk`: N=128 concurrent `Next()` calls against ONE shared `*ReactPlanner` instance. Each call has its own scripted ToolCalls response + per-run `DiscoveredTools` field. Assert: no cross-talk in discovered-tools state, no data race under `-race`, baseline goroutine count restored after all calls return.
- [ ] **AC-28** Integration (live LLM) — `internal/llm/drivers/bifrost/native_toolcall_integration_test.go` (or extends existing): real `harbor dev` against an LLM provider key. Send a prompt that elicits one tool-call → assert `resp.ToolCalls` is non-empty + `resp.Content` is the model's preamble (or empty). SKIP when no provider key.
- [ ] **AC-29** Phase 107b regression: if `phase-107b.sh` exists, its live probe (chunk concatenation == `result_inline.answer`) MUST still pass — Phase 107c's chunk stream is BETTER (no JSON wrapper at all because the LLM doesn't emit one), so the probe's "no leading `{` byte" assertion remains green.

### Console + Console-side regression

- [ ] **AC-30** No Console-side changes are strictly required. The chat bubble continues subscribing to `llm.completion.chunk`; the events now carry clean prose by structural construction. Existing Playwright specs (`reasoning-accordion.spec.ts`, `playground-page.spec.ts`) continue to pass. Operators see streaming that begins from byte 0 (no buffering for the JSON discriminator).

### Drift / hygiene

- [ ] **AC-31** Glossary entries: `ToolLoadingMode`, `ToolCallStructured`, `ToolDeclaration`, `tool_search`/`tool_get`/`skill_search`/`skill_get`/`declarative_action` (built-in meta-tools), `ToolSearchCache`, `RunContext.DiscoveredTools`. Alphabetised in `docs/glossary.md`.
- [ ] **AC-32** `docs/decisions.md` D-NNN entry (pre-assigned at dispatch) — pins the native-tool-calling cutover decision, lists the retired `_finish` discriminator, and names the `declarative_action` carve-out as the compatibility seam. References brief 15.
- [ ] **AC-33** `docs/skills/drive-the-playground/SKILL.md` + `docs/skills/configure-memory-and-skills/SKILL.md` + `docs/skills/add-an-in-process-tool/SKILL.md` — all three skills update per CLAUDE.md §18 same-PR drift rule:
  - drive-the-playground: notes that streaming is now byte-0-clean (no buffering / no JSON wrapper).
  - configure-memory-and-skills: notes that skills are now LLM-discoverable via `skill_search`/`skill_get` meta-tools; deferred-skill yaml config.
  - add-an-in-process-tool: notes `loading_mode` per-tool option; default `always`, opt into `deferred` for large catalogs.

## Files added or changed

### Runtime — LLM client

- `internal/llm/types.go` (or wherever `CompleteRequest`/`Response` live) — AC-1 + AC-2 field additions. ~40 LOC.
- `internal/llm/types_test.go` — pin the new field shapes + SafePayload composition (the request shape carries no secret-shaped data; tool schemas are operator-visible). ~30 LOC.

### Runtime — bifrost driver

- `internal/llm/drivers/bifrost/bifrost.go` — AC-3 mapping. ~80 LOC change at the existing `Complete` + streaming call sites.
- `internal/llm/drivers/bifrost/bifrost_native_tools_test.go` — fixture-driven tests of the bidirectional mapping. ~120 LOC.

### Runtime — mock LLM driver

- `internal/llm/drivers/mock/mock.go` — AC-4 ToolCalls support in scripted responses. ~30 LOC.

### Runtime — tools subsystem

- `internal/tools/tools.go` — AC-5 `LoadingMode` enum + field on `Tool`. ~20 LOC.
- `internal/tools/catalog/catalog.go` — AC-7 `Search` method + AC-9 cache sync hook. ~40 LOC.
- `internal/tools/drivers/searchcache/searchcache.go` — **NEW** AC-8 FTS5 driver. ~250 LOC.
- `internal/tools/drivers/searchcache/schema.sql` — **NEW** initial migration. ~30 LOC.
- `internal/tools/drivers/searchcache/searchcache_test.go` — AC-23. ~200 LOC.

### Runtime — built-in meta-tools

- `internal/tools/builtin/tool_search.go` — **NEW** AC-10. ~80 LOC.
- `internal/tools/builtin/tool_get.go` — **NEW** AC-11. ~60 LOC.
- `internal/tools/builtin/skill_search.go` — **NEW** AC-12. ~80 LOC.
- `internal/tools/builtin/skill_get.go` — **NEW** AC-12. ~60 LOC.
- `internal/tools/builtin/declarative_action.go` — **NEW** AC-13. ~100 LOC.
- `internal/tools/builtin/*_test.go` — AC-24. ~400 LOC total across the five tests.
- `internal/tools/builtin/builtin.go` — extend `Register(catalog, config)` for the new builtins. ~40 LOC change.

### Runtime — planner

- `internal/planner/planner.go` — AC-18 add `RunContext.DiscoveredTools []string` field. ~10 LOC.
- `internal/planner/react/projector.go` — **NEW** AC-15 ToolCallProjector. ~120 LOC.
- `internal/planner/react/projector_test.go` — AC-25. ~150 LOC.
- `internal/planner/react/react.go` — AC-15 + AC-17 + AC-19 swap parser, construct `req.Tools`, map response to Decision. ~80 LOC change at the existing `Next()` site.
- `internal/planner/react/prompt_builder.go` (or wherever `<available_tools>` + `<action_format>` are assembled) — AC-16 drop action-format, narrow `<available_tools>` to always-loaded, add `<tool_discovery>` section. ~60 LOC change.
- `internal/planner/react/discovered_tools.go` — **NEW** AC-18 per-run state machinery (the planner reads from `rc.DiscoveredTools`; the runtime writes after each meta-tool result). ~40 LOC.
- `internal/planner/react/integration_test.go` — AC-26 discovery-cycle test. ~200 LOC.
- `internal/planner/react/concurrent_test.go` — AC-27 N=128 concurrent reuse. ~100 LOC.

### Runtime — config

- `internal/config/config.go` — AC-6 per-tool `loading_mode` + per-builtin `enabled` config fields. ~30 LOC.
- `internal/config/validate.go` — reject unknown `loading_mode` values pre-boot. ~15 LOC.

### Runtime — runloop (cleanup)

- `cmd/harbor/cmd_dev_runloop.go` — AC-21 confirm onChunk closure unchanged.
- `internal/planner/react/stream_filter.go` — AC-22 DELETE if Phase 107b shipped first.
- `internal/planner/react/stream_filter_test.go` — AC-22 DELETE if Phase 107b shipped first.

### Runtime — repair

- `internal/planner/repair/parser.go` — UNCHANGED. The parser stays in place; it's only called from `declarative_action` now.
- `internal/planner/repair/repair.go` — UNCHANGED. The repair ladder applies to declarative-action's parsed CallTool; native-path validation happens provider-side before the response returns.

### Smoke + drift

- `scripts/smoke/phase-107c.sh` — **NEW**. PREFLIGHT_REQUIRES: live-server. Static asserts: meta-tools registered, `LoadingMode` field on Tool, searchcache driver exists. Live asserts: bootstrap dev token, register a deferred tool via test fixture, run a task that the LLM resolves via `tool_search` → assert the discovered tool is called on the next turn. SKIP gracefully on no provider key. ~150 LOC.

### Docs

- `docs/glossary.md` — AC-31 entries. ~10 lines added.
- `docs/decisions.md` — AC-32 D-NNN entry. ~30 lines added.
- `docs/skills/drive-the-playground/SKILL.md` — AC-33 prose update. ~10 lines change.
- `docs/skills/configure-memory-and-skills/SKILL.md` — AC-33 prose update. ~15 lines change.
- `docs/skills/add-an-in-process-tool/SKILL.md` — AC-33 prose update + `loading_mode` example. ~15 lines change.
- `docs/plans/phase-107b-streaming-answer-extractor.md` — if 107c is dispatched INSTEAD of 107b, this plan gets a header note marking it superseded; if 107c is dispatched AFTER 107b shipped, this plan gets a "delete the filter" note in the migration section.

## Public API surface

- `llm.CompleteRequest.Tools []ToolDeclaration` + `llm.CompleteRequest.ParallelToolCalls bool` — new optional fields. `Tools: nil` = text-only completion (preserves non-React planner behavior).
- `llm.CompleteResponse.ToolCalls []ToolCallStructured` — new optional field. Empty for non-tool-calling responses.
- `llm.ToolDeclaration` + `llm.ToolCallStructured` — new wire types.
- `tools.Tool.LoadingMode tools.LoadingMode` — new optional field, default `LoadingAlways`.
- `tools.Catalog.Search(ctx, query, tags, limit) []Tool` — new method.
- `planner.RunContext.DiscoveredTools []string` — new optional field. Empty unless a meta-tool populated it.
- Built-in tool names (operator-visible vocabulary): `tool_search`, `tool_get`, `skill_search`, `skill_get`, `declarative_action`.

## Test plan

### Unit (Go)

- `internal/llm/types_test.go::TestCompleteRequest_ToolsField_Optional`.
- `internal/llm/types_test.go::TestCompleteResponse_ToolCallsField_StructuralShape`.
- `internal/llm/drivers/bifrost/bifrost_native_tools_test.go::TestBifrost_MapsTools_Roundtrip`.
- `internal/tools/drivers/searchcache/searchcache_test.go` — AC-23 full suite.
- `internal/tools/builtin/*_test.go` — AC-24 across five meta-tools.
- `internal/planner/react/projector_test.go` — AC-25.

### Integration (Go)

- `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_DiscoveryCycle` — AC-26.
- `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_ParallelCalls` — N native parallel ToolCalls → `CallParallel{Join: JoinAll}` Decision.
- `internal/planner/react/integration_test.go::TestReactPlanner_NativeToolCall_FinishOnly` — `Content: "answer"` + `ToolCalls: []` → `Finish{Goal}`.
- `internal/planner/react/integration_test.go::TestReactPlanner_DeclarativeActionMetaTool_Dispatches` — escape-hatch round-trip (operator enables, LLM calls `declarative_action` with `{tool, args}` body, dispatcher routes to the named tool).

### Concurrency / leak

- `internal/planner/react/concurrent_test.go::TestReactPlanner_NativeToolCall_NoCrossTalk` — AC-27.
- `internal/tools/drivers/searchcache/concurrent_test.go::TestSearchCache_ConcurrentReads_NoRace` — N=100 concurrent `Search` calls.

### Conformance

- N/A — the React planner concrete passes the existing Phase 49 conformance pack with no extension; the pack tests the Decision sum + identity contract, both of which Phase 107c preserves.

### Live integration

- `internal/llm/drivers/bifrost/native_toolcall_integration_test.go` — AC-28 against a real provider when `OPENROUTER_API_KEY` is set.
- `scripts/smoke/phase-107c.sh` — discovery-cycle live probe.

## Smoke script additions

`scripts/smoke/phase-107c.sh` — PREFLIGHT_REQUIRES: live-server. Assertions:

1. SKIP when no LLM provider key.
2. Static: `internal/tools/drivers/searchcache/searchcache.go` exists; `internal/tools/builtin/tool_search.go` exists; `internal/llm/types.go` (or wherever) references `ToolCalls`/`Tools` fields.
3. Bootstrap a dev token via the Phase 105 endpoint.
4. POST `/v1/control/start` with a query that REQUIRES a deferred tool — e.g., the YouTube agent's `download_audio` if it's marked deferred in the test fixture's harbor.yaml.
5. Subscribe to `/v1/events/subscribe`; assert the first observed tool-call event is `tool_search` (the LLM discovered before invoking).
6. Assert a subsequent tool-call event names the discovered tool (the planner added it to the next turn's `Tools[]` and the LLM invoked).
7. Wait for `task.completed`; fetch `tasks.get`; assert trajectory has ≥2 steps (search + actual tool).
8. Assert no chunk event with leading `{` JSON wrapper byte (Phase 107c's chunks are clean by structural construction; this is the byte-stream legibility regression guard).

## Coverage target

- `internal/llm/`: 80% (existing).
- `internal/llm/drivers/bifrost/`: 80% (existing). The Tools/ToolCalls round-trip is covered by AC-3's test.
- `internal/tools/`: 85% (existing target).
- `internal/tools/drivers/searchcache/`: 85% — new package, target matches the skills/drivers/localdb baseline.
- `internal/tools/builtin/`: 85% — five new tools each with their own test.
- `internal/planner/react/`: 85% (existing). The projector + integration tests lift coverage above the parser path's previous numbers.
- `internal/config/`: 80% (existing).

## Dependencies

- 107 — chunk-event pipeline (Phase 107c removes the need for Phase 107b's filter; chunks still flow through the same pipeline, now carrying clean prose).
- 83a–e — React prompt depth (Phase 107c REPLACES the action-format prompt section; other sections — repair guidance, planning hints, skills context — are unchanged).
- 83n — built-in tools pattern (`internal/tools/builtin` registration shape — Phase 107c adds five new builtins through this seam).
- 37 — skills subsystem with FTS5 search (existing `SkillStore.Search` is what `skill_search` meta-tool wraps).
- 26 / 64a — tool catalog + Phase 64a operator-facing tool wiring (Phase 107c adds `loading_mode` to this surface).
- 32 / 33 / 33a — LLM client + bifrost driver + custom providers (Phase 107c extends `CompleteRequest`/`Response`; bifrost wiring carries the new fields).

## Risks / open questions

- **Plan size.** ~30 ACs across 7 subsystems is a single-agent stretch. Realistic delivery is 5–10 days of focused work. If the implementing agent is uncertain about scope at any point, they should pause and ask rather than ship a partial migration. A `progress_log.md` in the agent's worktree, updated daily with which ACs are done/in-progress/blocked, is the recommended self-check.
- **Provider parity.** Native tool-calling works first-class on OpenAI / Anthropic / Gemini. OpenRouter routes through to upstreams that support it; the YouTube agent's Claude-Haiku route works today. Local Llama / Mistral / smaller open-weight checkpoints WITHOUT tool-calling fine-tunes will fail — that's where the `declarative_action` escape-hatch tool earns its keep. The Phase 107c live test suite MUST cover at least one tool-calling-capable provider (OpenRouter Anthropic is the default smoke target) and the `declarative_action` round-trip against a stub model.
- **Existing React test rewrites.** The current React test suite (`internal/planner/react/*_test.go` minus the new files) uses JSON-in-Content fixtures. Those fixtures need to be rewritten to use the structured ToolCalls shape. The mechanical pattern: `Content: '{"tool":"x","args":{}}'` becomes `ToolCalls: []ToolCallStructured{{Name: "x", Args: json.RawMessage("{}")}}`. The agent should expect ~30–50 fixture sites to update. A single sed-style rewrite isn't safe (some tests verify the parser's specific JSON-handling); per-test inspection is required.
- **Discovered-tool dispatch validation.** When the LLM calls a discovered tool, the provider validates the call's args against the declared schema. If the planner adds the discovered tool to the next turn's `Tools[]` AFTER the LLM has already issued the call (same-turn race), the provider rejects. Mitigation: the discovery cycle is strictly two-turn (search → next turn adds the tool → next turn calls it). The prompt's `<tool_discovery>` section MUST make this explicit; the planner MUST NOT add discovered tools to the SAME turn's declaration mid-flight.
- **Streaming filter deletion path.** If Phase 107b ships before Phase 107c (sequenced delivery), the filter at `internal/planner/react/stream_filter.go` is deleted in 107c's PR. If Phase 107c ships directly (skipping 107b), the filter never lands. Both paths are explicitly handled by AC-22.
- **Tag taxonomy drift.** Operator-supplied tags are free-text. The `tool_search` + `skill_search` results carry whatever the operator declared. A future Console UI for "your most-used tags" + a recommended taxonomy is out of scope; tags are operator-managed for V1.4.
- **Backward compat for non-React planners.** The Deterministic planner (Phase 48) doesn't construct `Tools[]` in its CompleteRequest — its existing behavior (text-only completion) is preserved by AC-2's `Tools: nil` short-circuit. A future Workflow planner that wants tool-calling reuses the same surface; that's a per-planner-concrete adoption decision.

## Glossary additions

- **`ToolLoadingMode`** — Phase 107c enum on `tools.Tool`. Values: `always` (default — render in every turn's catalog) and `deferred` (hidden by default; LLM discovers via `tool_search`). (Alphabetised under T.)
- **`ToolCallStructured`** — Phase 107c wire type on `llm.CompleteResponse.ToolCalls`. Carries `{ID, Name, Args json.RawMessage}` — provider-validated structured tool-call entries replacing the prompt-engineered `{tool, args}` JSON parse. (Alphabetised under T.)
- **`ToolDeclaration`** — Phase 107c wire type on `llm.CompleteRequest.Tools`. Carries `{Name, Description, Schema json.RawMessage}` — the per-turn tool catalog the LLM sees. (Alphabetised under T.)
- **`tool_search`** / **`tool_get`** / **`skill_search`** / **`skill_get`** — Phase 107c built-in meta-tools the LLM uses to discover deferred tools + skills by capability text + tag filter. (Alphabetised under T / S.)
- **`declarative_action`** — Phase 107c optional built-in meta-tool (off by default; operator opt-in) that accepts a `{tool, args}` JSON body and dispatches through the existing `repair.ActionParser` path. The compatibility seam for models without reliable native tool-calling. (Alphabetised under D.)
- **`ToolSearchCache`** — Phase 107c SQLite FTS5 driver at `internal/tools/drivers/searchcache/` indexing tool name + description + tags. Mirrors the existing `internal/skills/drivers/localdb/` shape. (Alphabetised under T.)
- **`RunContext.DiscoveredTools`** — Phase 107c per-run field on `planner.RunContext`. Slice of tool names the LLM discovered via meta-tools during the current run; the React planner adds them to subsequent turns' `Tools[]` declarations. Stack-local-per-run (D-025). (Alphabetised under R.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: cross-session isolation test passes for the SearchCache (one tenant's deferred tools never leak into another tenant's `tool_search` results)
- [ ] Concurrent-reuse — AC-27 + the SearchCache concurrent-reads test
- [ ] Integration test — AC-26 discovery cycle + AC-28 live provider
- [ ] Glossary updated per AC-31
- [ ] `docs/decisions.md` D-NNN entry per AC-32
- [ ] All three skills updated per AC-33 (CLAUDE.md §18 same-PR drift)
- [ ] If Phase 107b shipped: `stream_filter.go` + `stream_filter_test.go` deleted per AC-22
- [ ] Operator-yaml example updated with `loading_mode` + meta-tool enablement (existing `examples/dev.yaml`)
- [ ] Live smoke against a tool-calling-capable provider (OpenRouter + Anthropic recommended)

## Implementation order (suggested)

The plan's size means rigorous sequencing matters. Strict layer-by-layer build minimizes mid-stream rework:

1. **LLM client surface FIRST** (AC-1 + AC-2 + AC-4) — extend `CompleteRequest`/`Response` + mock support. No consumer yet. `go test ./internal/llm/...` green.
2. **bifrost mapping** (AC-3) — bidirectional wire. Test against the mock; verify round-trip. Real-provider test deferred to step 12.
3. **`tools.Tool.LoadingMode`** (AC-5 + AC-6) — enum + config field + validator. Every existing tool stays `always`; no behavior change yet.
4. **`tools.SearchCache`** (AC-8 + AC-9) — new driver. Standalone tests (AC-23). Catalog wiring (AC-7).
5. **Built-in meta-tools** (AC-10–AC-14) — five new builtins through the existing `builtin.Register` path. Standalone tests (AC-24).
6. **`RunContext.DiscoveredTools`** field (AC-18 plumbing) — add the field; no consumer yet.
7. **`ToolCallProjector`** (AC-15 + AC-25) — new package file + tests. Independent of the React planner's existing Next() — can verify standalone.
8. **React prompt builder edits** (AC-16) — drop `<action_format>`, narrow `<available_tools>`, add `<tool_discovery>`. Existing prompt-builder tests update.
9. **React's `Next()` cutover** (AC-17 + AC-19) — wire `req.Tools` construction; swap parser to projector; map response to Decision. The existing React test suite's fixtures rewrite alongside (mechanical but per-test).
10. **`declarative_action` round-trip** (AC-13's test) — confirm the escape-hatch tool actually dispatches through `repair.ActionParser`.
11. **Integration tests** (AC-26 discovery cycle, AC-27 concurrent reuse) — both `go test -race` green.
12. **Live LLM test** (AC-28) — real provider, real discovery cycle.
13. **Cleanup of 107b's filter** (AC-22) if applicable — delete in this PR.
14. **Skill drift** (AC-33) — three skills updated.
15. **Glossary + decisions** (AC-31 + AC-32) — vocabulary lands.
16. **`make drift-audit && make preflight`** — both green.
17. Open PR.

**Pause-and-ask checkpoints — load-bearing.** If any of the following surfaces, the implementing agent stops and asks before proceeding:

1. **bifrost upstream `Tools[]` request field name + shape.** maximhq/bifrost may already use a `Tools` field name in `BifrostChatRequest`. Verify before drafting the mapping; if a collision exists, the bifrost mapping uses bifrost's field name verbatim and our `llm.CompleteRequest.Tools` translates. Same for `BifrostChatResponse.ToolCalls`. The exact bifrost shape needs to be confirmed by reading `~/.../bifrost/core/schemas/*.go` (a `go doc` against `bfschemas` is the fastest path).
2. **Provider-specific JSON-Schema compatibility.** OpenAI / Anthropic / Gemini accept slightly different JSON-Schema dialects. Anthropic rejects `additionalProperties: true` in some shapes; OpenAI requires `type: object` at the root. Today `tools.Tool.ArgsSchema json.RawMessage` is opaque bytes — the agent must verify whether bifrost normalizes the schema per-provider OR if we need to normalize Harbor-side. If bifrost doesn't normalize, the agent files a follow-up phase rather than attempting the normalizer here.
3. **CallParallel serialization fallback interaction with the existing parallel-emission Phase 47 path.** Today the React planner's multi-action salvage produces `CallParallel{Branches, Join: JoinAll}` from prompt-engineered JSON arrays. After Phase 107c, the salvage path becomes unreachable (the parser no longer fires on the native path), so Phase 47's `CallParallel` emission disappears UNTIL the runloop executor supports it. Verify this is the intended behavior; if operator yamls today rely on the salvage path firing, document the regression and file a follow-up.
4. **Same-turn race on discovered tools.** The discovery cycle test (step 11) — if the LLM emits a single response containing BOTH a `tool_search` call AND a call to a tool it expects the search to return (parallel ToolCalls in the same response), the provider rejects the second call as undeclared. Mitigation: the planner's serialization fallback (AC-19) naturally guards this — only the first call dispatches per turn. Verify the test reproduces the expected two-turn cycle.
5. **bifrost streaming tool-call assembly.** Most providers send tool_call entries as complete structs at end-of-stream (not per-delta). The plan's AC-3 says "assemble structured ToolCalls from the final response" — verify bifrost's streaming response in fact carries the final ToolCalls slice. If bifrost streams tool-call deltas (some providers do this for long arg payloads), the assembly logic on our side needs to merge them. If it does, the assembly is non-trivial; pause and confirm the expected shape.
6. **Test fixture rewrite blast radius.** The mechanical fixture rewrite (step 9 + AC-20d) finds tests asserting on parser-specific behavior that can't trivially translate to the structured form (especially the multi-action salvage tests in `repair_test.go` + the `prompt_test.go` `<action_format>` assertions). If the count exceeds 60 test sites OR if any test's intent is unclear, pause and confirm the rewrite strategy.
7. **Cancellation behavior during streaming tool-calls.** Phase 13 / 54 cancellation today drains the streaming `Content` channel. With tool-calls now streaming as a separate provider-side channel, verify bifrost cleanly closes the tool-call stream on `ctx.Done()`. If a partial tool-call structure can leak after cancellation, document the recovery path.
8. **Conformance pack assertions.** Phase 49's planner conformance pack may assert specific behaviors on `trajectory.Step.Action`'s JSON-action shape. Run `go test -race ./internal/planner/conformance/...` BEFORE shipping; if any assertion fails, the conformance pack itself may need updating (the pack should test the Decision sum, not the parser's input shape — if it tests the latter, it's a pack bug per CLAUDE.md §17.6).
9. **`make preflight` single-source check.** The new wire types `ToolCallStructured` + `ToolDeclaration` + `ToolCallID` field on `ChatMessage` live in `internal/llm/llm.go` (or `internal/llm/types.go`) — NOT in `internal/protocol/types/`. They are LLM-internal, not Protocol surface. Therefore NO registration in `internal/protocol/singlesource/singlesource.go` is required. Verify by running `go test ./internal/protocol/singlesource/...` after the LLM-side changes land — phases 54/58/59 should stay green.

### Gaps the implementing agent must close (real, not enumerated above)

The plan deliberately does NOT pre-specify every micro-decision; the agent owns these calls:

- **Per-turn `Tools[]` ordering.** The plan says "always-loaded set + meta-tools + discovered". The exact ordering (alphabetical? insertion-order? always-loaded first?) is unspecified — provider behavior on tool-list ordering varies. The agent picks one (insertion-order is the safe default) and pins it with a test.
- **`tool_get` schema rendering format.** When the LLM calls `tool_get("foo")`, the response includes the tool's `ArgsSchema json.RawMessage`. The agent decides whether to return the raw schema bytes (LLM parses) or pretty-print first. The predecessor returns minimal + a separate `examples` field; mirror that shape.
- **Discovery-cycle TTL.** `RunContext.DiscoveredTools` accumulates across a run's steps. Does it survive across plan-task spawn/await boundaries (Phase 21)? Likely yes (same run identity); the agent confirms by reading Phase 21 spawn semantics.
- **`declarative_action`'s response shape.** When the meta-tool's body dispatches a CallTool, what's the meta-tool's RETURN value? Probably `{dispatched: true, observation: <tool_result_json>}` — but the agent designs the shape such that the LLM sees the tool result as if it had called the tool natively.
- **Operator-yaml example update.** `examples/dev.yaml` (or equivalent) needs a working illustration of `loading_mode: deferred` + `tools.builtin.declarative_action.enabled: true`. The agent adds it.
- **Skill drift across THREE skills (AC-33).** The wording for each skill's prose update is operator-facing; the agent owns the exact text. Keep changes minimal and focused on the new operator-visible behavior.
