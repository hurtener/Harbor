# Research Brief 15 — Native tool-calling + deferred loading + tag-based scoping

Status: research / pre-RFC. Authored after Phase 107b lands; informs a V1.4 planner-migration phase plan (provisionally Phase 110-band). Internal vocabulary proposed below; final names settle in the RFC update if the migration is adopted.

## 1. Subsystem overview

The React planner today (Phases 83a–e) emits its decisions as prompt-engineered `{tool, args}` JSON inside the LLM's `Content` field. The parser path (`internal/planner/repair/parser.go` + Phase 47 multi-action salvage) reads that JSON post-hoc and lifts it into the `Decision` sealed sum (`CallTool`, `CallParallel`, `Finish`). It's a deliberately portable shape — works on any LLM that emits text, no provider-specific structured-output API required, and gives Harbor's React concrete its compatibility ceiling: weaker models, smaller models, models without tool-calling fine-tunes.

It also creates two structural costs:

1. **Streaming requires a JSON-extractor** (Phase 107b's `streamAnswerFilter`) — chunks arrive mixed with the JSON wrapper, the planner has to gate which bytes are user-facing prose.
2. **Tool catalogs render full schemas in the prompt every turn.** The prompt builder (Phase 83b) renders every visible tool's name + description + JSON schema into `<available_tools>`. A 30-tool catalog already approaches the system-prompt budget; a 200-tool catalog blows it. Today's compatibility ceiling is the bottom — the top (large catalogs at scale) is unreachable.

Modern provider APIs (OpenAI chat-completions `tools[]` + parallel function-calling, Anthropic `tool_use` + parallel `tool_use`, Gemini `function_calling` + parallel-call) all expose **native tool-calling**: the model returns a structured `Message` with separate fields for `Content` (prose), `ToolCalls []ToolCall` (function-call structures), and `ReasoningContent` (thinking trace). Tool args arrive on a typed channel separate from prose; streaming forwards `Content` deltas directly as user-facing text without a wrapper.

The eino reference (`~/Repos/_research/eino`) confirms this pattern is industry-standard in Go: their React-equivalent (`adk.NewChatModelAgent` + `compose.ToolsNode`) reads `len(chunk.ToolCalls) > 0` as the branch discriminator and fans out parallel tool-calls via a per-task `sync.WaitGroup` in `compose/tool_node.go::parallelRunToolCall`. No JSON extractor exists anywhere in eino — the streaming-vs-tool-call problem is solved structurally by the provider's API shape.

This brief asks: **can Harbor's React planner switch to native tool-calling without losing Harbor's distinct savings, and what does it unlock that the prompt-engineered shape cannot?**

## 2. Key data shapes

Sketches — not final. The migration would extend our existing `llm.CompleteResponse` to surface tool-call structures the planner already gets via JSON parsing today.

```go
// Currently:
type CompleteResponse struct {
    Content    string              // model output (the planner parses JSON out of this)
    Reasoning  string              // provider-side reasoning trace (Phase 83e captures into trajectory)
    Usage      Usage
    FinishReason string
    // ... no structured ToolCalls field
}

// Path B extension (provisional):
type CompleteResponse struct {
    Content       string
    Reasoning     string
    ToolCalls     []ToolCallStructured  // NEW — structured per-call entries
    Usage         Usage
    FinishReason  string
}

type ToolCallStructured struct {
    ID    string           // provider-assigned call id (round-trip for tool_result)
    Name  string           // tool name
    Args  json.RawMessage  // already-parsed JSON args (provider-validated against the declared schema)
}

// CompleteRequest gains a Tools field surfaced to the provider:
type CompleteRequest struct {
    // ... existing fields ...
    Tools []ToolDeclaration  // NEW — declared per-turn (mutable!)
    ParallelToolCalls bool   // NEW — opt-in (default true for providers that support it)
}

type ToolDeclaration struct {
    Name        string           // matches tools.Tool.Name
    Description string           // operator-visible description
    Schema      json.RawMessage  // JSON schema of args (already declared on the Tool)
}
```

bifrost already produces structured tool-calls — the OpenAI / Anthropic / Gemini providers it wraps all surface `ToolCalls` natively; bifrost's own response struct carries them. Path B surfaces the field upstream onto `llm.CompleteResponse` (one shape change), then the React planner reads `len(resp.ToolCalls) > 0` as the discriminator instead of parsing JSON out of `Content`.

The `Tools []ToolDeclaration` request field is the key per-turn lever — it's what lets the planner control which tools the LLM sees on THIS specific call. That's the seam deferred loading rides on.

## 3. The deferred-loading problem at scale

The token-budget math: a typical tool's declaration (name + description + JSON schema for 5–10 args + 1 example) is ~300–500 tokens. 30 tools ≈ 12k tokens of prompt budget gone before the user query. 100 tools ≈ 40k tokens — already over the context window for most cost-conscious models. 1000 tools (a realistic enterprise catalog if you map every internal API + every MCP server's tools) ≈ 400k tokens — unreachable even on Claude 4.5 Sonnet's 200k window.

The fix at scale is **deferred loading**: don't ship the full catalog every turn. Three sub-strategies, each with different tradeoffs:

### B1 — always-declared catalog (simple, small-catalog only)

Declare all tools every turn. Works fine for <50 tools, which is most agent yamls today. No catalog cost beyond the declaration size. The system prompt's `<available_tools>` section becomes a thin pointer ("see the provider's tool list") instead of full schemas — saves prompt budget but doesn't change the per-turn declaration cost.

### B2 — meta-tool discovery (the predecessor's pattern)

Mark each tool with a loading mode (`always` / `deferred`). Always-tools are declared every turn alongside two built-in meta-tools: `tool_search` (free-text query + optional tag filter → returns matching tool names + descriptions) and `tool_get` (fetch the full schema + examples for a named tool). The LLM uses the meta-tools to discover deferred tools, then the planner re-builds the LLM request with the discovered tool ADDED to the declared list for the next turn. Two LLM calls per discovery cycle, but the per-call declaration cost stays bounded.

The predecessor implements this with:

- `ToolLoadingMode` enum (`ALWAYS` / `DEFERRED`) per `NodeSpec`.
- `ToolSearchCache` — SQLite FTS5-backed search index over tool name + description + tags, with `always_loaded_patterns` (glob patterns the operator can mark as "always visible regardless of mode").
- Prompt section `<tool_discovery>` instructs the LLM: "use `tool_search` to find capabilities; the runtime will activate deferred tools on first call."

For Harbor with native tool-calling, the same pattern works but requires per-turn mutation of the `Tools []ToolDeclaration` request field. The planner inspects the previous turn's `tool_search` results, adds the discovered tools to the next request's declared list. The provider then accepts a call to the previously-deferred tool without rejection.

Cost: every discovery cycle is one extra LLM call. For agents with stable tool usage patterns (most operator yamls), the discovery happens once early and the per-tool declaration cost stays bounded thereafter.

### B3 — planner-side tag prefilter (no meta-tool, no LLM round-trip)

Each tool declares semantic tags. The planner pre-filters tools by tag relevance to the query — either via the operator's per-task tag hint (today: nothing; could be added to `tasks.SpawnRequest.Filter`) OR via semantic similarity over an embedding index. The LLM sees only the prefiltered subset every turn, no discovery cycle needed.

Pros: zero extra LLM calls; works without provider tool-calling.
Cons: requires either operator-supplied tags (manual upkeep) or an embedding model + index (Phase 39 virtual-directory pattern, currently unused at runtime). Misses queries whose tag relevance is non-obvious from the user's phrasing.

The predecessor combines B2 + B3 — `tool_search` supports a `tags` argument; the planner can pass operator-supplied tags as a filter to scope the LLM's view.

## 4. What the predecessor solved cleanly (and we should inherit)

From the prior-art reference implementation's `planner/tool_search_cache.py` + `catalog.py` + `planner/prompts.py`, the load-bearing patterns are:

- **Per-tool `ToolLoadingMode` is a first-class field.** Operators decide which tools are always-visible vs deferred at agent-yaml authoring time — no runtime guesswork, no implicit promotion.
- **The discovery surface is two meta-tools, not a special API.** `tool_search` and `tool_get` are tools the LLM calls like any other tool. The provider's tool-calling API handles them uniformly; the runtime activates returned-deferred-tools on first call.
- **Tag filtering is server-side.** `tool_search(query, tags=["mcp", "filesystem"])` filters the FTS5 index BEFORE returning results — the LLM gets a narrowed candidate set, not a re-rank job.
- **Skills follow the identical pattern.** `skill_search` / `skill_get` + `required_tags` for visibility filtering. Skills are deferred-loaded the same way tools are.
- **The system prompt instructs the LLM about deferred semantics.** A dedicated `<tool_discovery>` block tells the LLM that `tool_search`-returned tools are CALLABLE even though absent from the visible catalog. Without this prompt instruction the LLM refuses to call them.
- **The runtime activates discovered tools transparently.** No special "discovered tool" code path — the call goes through the same dispatcher as any always-loaded tool; the dispatcher just checks the tool exists in the registry.

The pattern decouples the LLM's visible-catalog size from the runtime's catalog size — the runtime can host thousands of tools while the LLM sees <30 per turn.

## 5. What Harbor already has, and what's missing

Reading our existing primitives:

**Have (today):**

- `tools.Tool.Tags []string` — operator-facing capability tags. Already declared on every tool.
- `tools.Tool.AuthScopes []string` + Phase 83m's `GrantedScopes` filter — visibility gated by identity/scope (orthogonal to deferred loading; both layers compose).
- `tools.Tool.HandlesMIME []string` — MIME-routing hint for the multimodal materializer.
- `skills.SkillStore.Search(ctx, identity, query, limit) RankedSkill` — full-text search over skills, already SQLite FTS5-backed in the localdb driver.
- `skills.Skill.RequiredTags + Tags` + `directory.CapabilityFilter` — Phase 39's tag-filtered skill projection, currently CONSUMED by the React planner's `<skills_context>` injection (Phase 83d), but the *capability* filter exists.
- `planner.ToolCatalogView` interface — the planner-facing read view (Phase 83i wires the production projection). The view today exposes `Resolve(name) (Tool, bool)` + `List() []Tool` — no per-tag filter method.

**Missing (gaps for B2):**

- A `ToolLoadingMode` field on `tools.Tool` — every tool is implicitly `ALWAYS` today.
- A `tool_search` / `tool_get` built-in tool pair (Phase 83n shipped `clock.now` + `text.echo` builtins via `internal/tools/builtin`; the discovery meta-tools would land alongside).
- A `tools.SearchCache` analogue — FTS5 over tool name + description + tags, with cache-fingerprint invalidation on catalog change. The skills package's SQLite FTS5 driver is a viable template.
- A `planner.ToolCatalogView.Search(query, tags []string, limit int) []Tool` method — the planner-facing surface the React planner calls when the LLM invokes `tool_search`.
- A runtime activation seam — "the LLM just called a tool that wasn't in the declared list this turn, but it WAS returned by a previous `tool_search`; add it to the next turn's declaration." This is per-run state, lives on the `RunContext` or a per-run companion struct (NEVER on the planner artifact — D-025).
- A prompt-template instruction explaining deferred semantics. Phase 83b's `<available_tools>` becomes either fully populated (B1) or partially populated + a `<tool_discovery>` instruction (B2).

**Missing (gaps for native tool-calling more broadly):**

- `llm.CompleteResponse.ToolCalls []ToolCallStructured` — bifrost gets this from upstream providers; we don't surface it.
- `llm.CompleteRequest.Tools []ToolDeclaration` + `ParallelToolCalls bool` — bifrost accepts these in its request shape; we'd thread them through `ConfigSnapshot` → `Complete` call site.
- A `react-native` planner concrete (vs today's `react`) — the prompt builder swaps `{tool, args}` JSON instructions for "use the provided tools" + the native ToolCalls path in the response parser. Both modes coexist behind the planner driver registry (Phase 42 / D-103); operators opt in via `planner.driver: react-native` in `harbor.yaml`.

## 6. Decision-sum invariance — the migration does NOT cost us `CallParallel`

The user concern that prompted this brief: **eino does parallel-tool fanout, but is it Harbor-grade?** Specifically — does the Decision sum + Runtime-owned execution survive a native-tool-calling migration?

Yes. The Decision sum is the planner→runtime contract; it's orthogonal to how the planner parses its LLM input. Mapping is straightforward:

- LLM emits 1 native `ToolCall` → React planner emits `CallTool{Branch}`.
- LLM emits N native `ToolCalls` in one message (the provider's parallel function-calling API) → React planner emits `CallParallel{Branches: [...], Join: JoinAll, MaxParallel: hint}`.
- LLM emits no ToolCalls + `Content != ""` → React planner emits `Finish{Reason: Goal, Payload: Content}`.

The Runtime's existing `CallParallel` machinery (Phase 47's emission + the runloop's per-branch goroutine fanout governed by `MaxParallel` hint + system `absolute_max_parallel` cap per RFC §6.2) is untouched. Identity propagation through each branch's `ctx` is unchanged. `JoinSpec` semantics (`JoinAll`, `JoinFirstNonError`, `JoinFirstNonNil`) are unchanged.

What native tool-calling unlocks that the prompt-engineered shape struggles with:

- **Parallel calls without multi-action salvage.** Today Phase 47's `CallParallel` emits from React only via the `[{tool,args}, {tool,args}]` multi-JSON path the parser salvages. Models that don't reliably emit multi-action JSON (most non-tool-aware fine-tunes) emit sequential single-action turns instead, which the planner serializes. Native parallel-function-calling is a first-class provider feature — even small models reliably emit it.
- **Provider-validated args.** The provider already JSON-schema-validates args against the declared tool schema before returning. Phase 44's repair loop's schema-repair ladder still serves as defense in depth, but the first-pass error rate drops dramatically (the provider rejects the malformed call BEFORE returning, the model re-tries internally).
- **Streaming separation.** `Content` deltas stream cleanly (Phase 107b's `streamAnswerFilter` becomes unnecessary — the wrapper code deletes in one PR). `ReasoningContent` deltas stream cleanly too (Phase 107c becomes a one-line forward of `OnReasoning` callbacks to `rc.OnChunk` with `kind: ChunkReasoning`).
- **Structured tool-result feedback.** The provider's `tool_result` payload shape (`{tool_call_id, content}`) maps directly to our trajectory's tool-result step. Today we synthesize this round-trip from text; natively it round-trips structured.

## 7. When the prompt-engineered React path still wins (the carve-out)

Path B is the right long-term shape but it carries a real cost: **provider lock-in to the native tool-calling subset.** Specifically:

- **Local / self-hosted models without tool-calling fine-tunes** (Ollama base models, raw Llama / Mistral / smaller open-weight checkpoints) — they can emit prompt-engineered JSON but not native function-calls. Path B excludes them.
- **Provider versions / endpoints where tool-calling is unstable or unsupported** — Anthropic's older Haiku checkpoints, Gemini Flash on some routes, OpenRouter's free-tier passthrough to weaker upstreams.
- **Determinism for tests.** The prompt-engineered JSON is byte-stable across a wider model surface — easier to mock, easier to fingerprint for replay.
- **Compatibility-bridging during the migration window.** A deprecation period where operators move from `react` to `react-native` benefits from both modes running side-by-side, with a per-agent-yaml opt-in.

The realistic conclusion: **keep both concretes.** `internal/planner/react/` (today's prompt-engineered) stays as the compatibility floor; `internal/planner/react-native/` (V1.4) becomes the recommended-for-tool-calling-capable-models default. The planner driver registry (Phase 42 / D-103) already supports this — operators select via `planner.driver: react-native` in `harbor.yaml`.

A future operator-facing recommendation matrix:

| Model class | Recommended driver | Reason |
|---|---|---|
| OpenAI gpt-4o / 5 / o-family | `react-native` | First-class parallel function-calling + reasoning streaming |
| Anthropic Claude 3.5+ / 4.x | `react-native` | First-class `tool_use` + extended thinking |
| Gemini 1.5+ / 2.x | `react-native` | First-class function-calling |
| Local Llama / Mistral / open-weight | `react` (today's) | No tool-calling fine-tune; prompt-engineered JSON works |
| Weaker / older Haiku / Flash variants | `react` (today's) | Tool-calling reliability not consistent across endpoints |

## 8. Architectural integration with the rest of Harbor

The migration touches multiple subsystems; brief enumeration of the seams:

- **`internal/llm/` (Phase 32 + Phase 33 bifrost driver).** Extend `CompleteResponse` with `ToolCalls []ToolCallStructured`. Extend `CompleteRequest` with `Tools []ToolDeclaration` + `ParallelToolCalls bool`. bifrost's driver maps these to/from its upstream provider calls. Safe-payload classification: tool args carry user-visible content — `SafePayload` (matches today's `CompletionChunkPayload` posture).
- **`internal/planner/` (Phase 42 contracts).** No interface change. The `react-native` concrete registers itself via the existing driver registry (Phase 42 / D-103); `cmd/harbor/main.go` blank-imports it like every other planner driver.
- **`internal/planner/react-native/` (NEW).** The new concrete. Prompt builder removes the `{tool, args}` JSON instruction + the `<tool_discovery>` instruction (if B2). Response parser reads `resp.ToolCalls` instead of `resp.Content`. Maps to existing Decision sum.
- **`internal/planner/repair/` (Phase 44).** Schema-repair ladder applies to `args` of `ToolCalls[i]` instead of parsed JSON. The same `ToolValidator` signature works (it takes a tool name + args bytes); the parser layer below it changes shape but the validator surface is unchanged.
- **`internal/tools/` (Phase 26 + Phase 64a).** Add `ToolLoadingMode` field on `Tool`. Add `tools.SearchCache` + a `tools.Catalog.Search(query, tags, limit)` method. Add `tools/builtin/tool_search.go` + `tool_get.go` — the meta-tools, registered like `clock.now` and `text.echo` today.
- **`internal/planner/ToolCatalogView` (Phase 42).** Extend with `Search(query, tags, limit)` so the React-native planner's meta-tool dispatch surface has a way in.
- **`cmd/harbor/cmd_dev.go` (Phase 64).** `tools.entries[]` config block gains a `loading_mode` knob per entry (operator-declared). Validator rejects an unknown mode pre-boot per CLAUDE.md §13 fail-loud.
- **Console (Phase 73f Tools page).** Render the `loading_mode` per tool. Future Console "tool discovery trace" panel surfaces which deferred tools the LLM discovered per run (Phase 73f extension, post-V1.4).
- **Skills (Phase 37+).** The pattern is already in place for skills — `Search(query, limit)` + `RequiredTags`. The migration extends the seam to tools but reuses the skills package's FTS5 driver shape (`internal/skills/drivers/localdb/`) as the template for `internal/tools/drivers/searchcache/`.

## 9. What this brief does NOT settle (open questions for the RFC update)

- **Should `react-native` and `react` share a parser, or be fully independent?** The args-validation logic is reusable; the prompt builders are not. Decision: share the validator + repair loop; fork the prompt builder + response parser.
- **`tool_search` result shape.** Should it return tool name + description only (minimal) or include the schema (full)? Predecessor returns minimal + a `tool_get` follow-up. The minimal-first pattern keeps `tool_search` cheap and lets the LLM be discerning.
- **Concurrency of `tool_search` calls.** If the LLM emits `parallel tool_calls` whose first call is `tool_search`, can subsequent calls in the same parallel emission USE the discovered tools? Probably no — the discovered tool is added to the NEXT turn's declared list, not this turn's. The provider would reject a call to an undeclared tool. This is a real UX paper cut; the predecessor sidesteps it because their prompt-engineered shape doesn't validate against a provider's tool list. Path B requires the LLM to two-turn (search, then call).
- **Deprecation window for the prompt-engineered React.** When does `react` move from "default" to "compatibility fallback"? Probably V1.4 → V1.5: V1.4 ships `react-native` as opt-in, V1.5 flips the default once the model-recommendation matrix has shaken out from operator feedback.
- **Skill deferred-loading.** Skills already have Search; the same prompt-discovery pattern (`skill_search` / `skill_get`) layers identically. Does it ship in the same wave as tool deferred-loading or a separate phase?
- **Tag taxonomy governance.** Predecessor's `declared_tags` are operator-supplied free-text. A future Console UI for "your most-used tags" + a recommended taxonomy (`io`, `memory`, `external`, `admin`, ...) would help operators pick tags consistently. Out of scope for the migration phase plan; could be a Console-side enhancement post-V1.4.

## 10. Suggested Phase 110-band decomposition (V1.4)

If the RFC update adopts the migration, the phase plan tree:

- **Phase 110 — `llm.CompleteResponse.ToolCalls` + `CompleteRequest.Tools` surface.** Extend the LLM client contract; bifrost driver maps to/from upstream. ~3 days. No planner change yet.
- **Phase 110a — `tools.SearchCache` + `tools.Tool.LoadingMode`.** SQLite FTS5 backend (mirroring `internal/skills/drivers/localdb/`). Per-tool loading-mode field. ~3 days.
- **Phase 110b — `tool_search` + `tool_get` built-in meta-tools.** Registered like `clock.now` / `text.echo`. Wired to the SearchCache. ~2 days.
- **Phase 110c — `react-native` planner concrete.** New `internal/planner/react-native/` package. Prompt builder + response parser. Registered via the planner driver registry. Defaults to `react` for backward compatibility; `react-native` is opt-in via `harbor.yaml`. ~5 days.
- **Phase 110d — `react-native` integration tests + recommendation matrix.** Real-LLM tests against the V1 provider set; the operator-facing matrix in `docs/CONFIG.md`. Phase 107b's `streamAnswerFilter` deletes here (the prompt-engineered path keeps it; native doesn't need it). ~3 days.
- **Phase 110e — Console "tool discovery trace" panel.** Renders per-run which deferred tools the LLM discovered. Post-V1.4 if operator feedback requires it; otherwise defer. ~3 days.

Total scope: ~3 weeks for a single contributor, parallelizable to ~10 days with two agents (110 + 110a + 110b in parallel, 110c on top, 110d wave-end).

## 11. Decision criterion for "do we migrate?"

Two operator-facing signals would justify Path B:

1. **Catalog scale.** When an operator's `harbor.yaml` declares >50 tools (or sources tools from >3 MCP servers each contributing 10+), the always-declared cost hits the system-prompt ceiling. Track via a runtime warning when `len(tools) > N` at boot.
2. **Streaming quality complaints.** Phase 107b's `streamAnswerFilter` solves the wall-of-text bug but leaves the `streaming-but-the-bubble-feels-jerky` second-order issue (chunks emit only after the discriminator + opening-quote stages match — typically ~30 tokens of buffering before the first emit). Path B's `Content` deltas stream from byte 0. Operators will feel the difference on long answers.

Until either signal materializes, Phase 107b is sufficient. The brief exists so the migration's design is ready when the signal does arrive — no scoping cost to defer.

## 12. Closing summary

Native tool-calling is the right long-term shape for Harbor's React planner. The Decision sum + `CallParallel` survives the migration unchanged (the Runtime mechanism doesn't care how the planner sourced the call shape). Deferred loading + tag-based scoping ride naturally on native tool-calling — the predecessor's `tool_search` / `tool_get` pattern transplants cleanly, gated on extending our existing primitives (`tools.Tool.Tags` already exists; `LoadingMode` is the new field; `SearchCache` mirrors the skills FTS5 driver).

The carve-out — keeping the prompt-engineered `react` driver alongside `react-native` — protects Harbor's distinct saving (compatibility with weaker / non-tool-aware models). Both concretes ship side-by-side; operators pick per agent yaml.

Phase 107b ships the V1.3 fix in days. The V1.4 migration is a 3-week phase plan tree (~110-band). They sequence cleanly because the V1.3 filter lives wholly inside `internal/planner/react/` — when V1.4 lands, the filter deletes in one PR with zero cross-subsystem cleanup.
