# 07 — Code-Level Tool Calling (Provider-Independence by Design)

## 1. The principle in one paragraph

Harbor performs tool calling at the **runtime/orchestration layer**, not at the LLM provider layer. The LLM client is reduced to a JSON-producing chat completion: it accepts a list of role/content messages plus an optional `response_format` hint, and it returns a single string (or `(string, cost)`) of text — nothing else. The runtime renders the universe of available tools into the system prompt as a typed action schema (`next_node` + `args`), parses the model's reply into a `PlannerAction`, dispatches one or more tool calls in parallel from a single planner step, validates inputs and outputs against Pydantic-style schemas, formats observations back into the next prompt, and repeats. There is no `tools=`, no `tool_choice=`, no `function_call`, no provider-native tool-call ID, and no provider-side parallel-tool-calling toggle anywhere in the call path. **Parallel tool calling and tool calling in general therefore work uniformly across every provider that can emit JSON** — OpenAI, Anthropic, Google, DeepSeek, OpenRouter routes, local models — because the protocol the planner speaks is owned by Harbor, not by the provider.

This is the keystone architectural property: the entire LLM client surface is one method. It collapses the matrix of "provider × tool-calling mode × parallel-calls support × structured-output mode" into a single dimension Harbor controls.

## 2. Data flow trace

```text
                                                                            
           ┌──────────────────────────────────────────────────────────┐     
           │ Trajectory  ─── system prompt ───────┐                   │     
           │   (steps, query,                     │                   │     
           │    llm_context)            ┌─────────▼────────┐          │     
           └────────────┬───────────────│ build_messages() │          │     
                        │               │                  │          │     
                        ▼               └─────────┬────────┘          │     
              [system, user, asst, user, ...]     │                   │     
                                                  ▼                   │     
                                       ┌──────────────────┐           │     
                                       │ LLMClient.complete│           │     
                                       │  (JSON in/out)   │           │     
                                       └─────────┬────────┘           │     
                                                 │ raw text           │     
                              ┌──────────────────▼──────────────────┐ │     
                              │ extract JSON from text              │ │     
                              │ normalize action (multi-object)     │ │     
                              │ salvage action payload (fallback)   │ │     
                              │ regex finish-extract (last resort)  │ │     
                              └──────────────────┬──────────────────┘ │     
                                                 │ PlannerAction      │     
                                                 ▼                    │     
                            ┌──────────────────────────────────────┐  │     
                            │ next_node ∈ {                        │  │     
                            │   "final_response" → terminate       │  │     
                            │   "parallel"       → fan-out         │──┘     
                            │   "task.*"         → bg-spawn        │        
                            │   <tool name>      → single dispatch │        
                            │ }                                    │        
                            └──────────────────┬───────────────────┘        
                                               │                            
                       ┌───────────────────────┴──────────────────┐         
                       ▼                                          ▼         
        ┌─────────────────────────────┐         ┌──────────────────────────┐
        │ execute one tool call       │         │ gather over N tool calls │
        │   - validate args           │         │   - synthetic IDs        │
        │   - run the tool            │         │   - per-branch outcome   │
        │   - validate output         │         │   - explicit join        │
        │   - emit tool_call_*        │         │     injection            │
        │     events                  │         │                          │
        └──────────────┬──────────────┘         └──────────────┬───────────┘
                       │                                       │            
                       └───────────────┬───────────────────────┘            
                                       ▼                                    
                          observations + (errors|failures|pauses)           
                                       │                                    
                                       ▼                                    
                       trajectory.steps.append(TrajectoryStep)              
                                       │                                    
                                       └───── loops to build_messages ──────
```

The key stages along the path:

- **`build_messages()`** constructs the chat thread from the trajectory.
- **Prior-step rendering**: every prior step is rendered as `{"role": "assistant", "content": json.dumps({next_node, args})}` followed by `{"role": "user", "content": render_observation(...)}`. The "tool result" channel is just a user message; there is no provider-native tool-result role.
- **The LLM call**: `client.complete(messages, response_format, stream, on_stream_chunk, on_reasoning_chunk)`. That is the entire LLM API surface the planner depends on.
- **Parse → validate → repair loop** runs on the raw text.
- **Parallel dispatch** gathers over N concurrent tool calls.

## 3. The parsing surface

The model returns plain text. The runtime owns extraction.

1. **JSON extraction** strips ```` ```json ```` fences, then falls back to "first `{` to last `}`". Always idempotent and string-pure.
2. **Action normalization** tries a strict JSON parse first, then a real-decoder-based scanner that can find multiple JSON objects in mixed prose, returning the primary action plus an `other_actions` list (these become `PlannerAction.alternate_actions`, used later by the runtime as a sequential queue without a second LLM call).
3. **Action-shape validation** enforces the action schema = two fields: `next_node: str` and `args: dict[str, Any]`. Special opcodes (`final_response`, `parallel`, `task.subagent`, `task.tool`) trigger fan-out or termination paths; any other string is a tool name.
4. **Salvage path** is the soft fallback when strict validation fails — it accepts legacy/hybrid shapes and normalizes them.
5. **Repair loop**: on parse/validation failure, prepend a rendered repair message to the system messages and retry up to `RepairAttempts` times.
6. **Last-ditch finish extraction**: a regex `"(?:raw_answer|answer)"\s*:\s*"((?:[^"\\]|\\.)*)"` salvages a final answer from malformed JSON when every repair attempt has failed. The runtime would rather hand the user a probably-correct answer than crash.
7. **Schema sanitization for provider quirks**: the client downgrades the `response_format` per-provider (`no_format` → text-only, `json_object` → loose, `strict_schema` → minimal-schema, `default` → sanitized). The prompt is always the source of truth; `response_format` is a hint that strengthens compliance when the provider supports it.

## 4. Parallel dispatch mechanics

When the LLM emits `next_node="parallel"`, `args.steps` is a list of `{node, args}` objects and `args.join` is an optional spec for a follow-on aggregation tool. The dispatcher:

1. **Validates the plan** statically: every `node` resolves in the tool catalog (with a lazy-activation fallback); every per-branch `args` validates against the tool's input schema. **If any branch fails validation, the entire parallel plan is rejected** with a single setup-error message back to the LLM. Fail-fast at validation; no half-plans.
2. **Runs branches concurrently** via a gather over N single-tool dispatches.
3. **Synthesizes correlation IDs**: `call_{action_seq}_parallel_{branch_index}` for each branch and `call_{action_seq}_parallel_join` for the optional join step. The runtime — not the model — owns the ID space, so cross-provider portability is automatic.
4. **Steering / cancellation**: a steering-inbox race is wired into each single-tool dispatch: the runtime starts the tool task and a "cancel event" task in parallel and waits for the first to complete. If steering cancels, the tool task is cancelled, awaited, and a cancellation error is raised. The pause variant works the same way.
5. **Per-branch outcome model**: each branch returns one of `{observation, error+failure, pause}`. Errors are caught at the dispatch boundary so a single failing branch does NOT cancel sibling branches — the gather collects, and the merger sorts results into success/failure buckets.
6. **Join step**: if `args.join` is provided AND no branch failures occurred, a follow-on tool call runs with the parallel results injected via an explicit `inject` mapping. On any branch failure the join is skipped with reason `"branch_failures"`. On any branch pause the join is skipped with reason `"pause"`.

Identity correlation, deadline propagation, and partial-failure handling all live in this one module. There is nothing provider-specific in the entire dispatcher.

## 5. Result merging

After the planner step completes, results are merged back into the trajectory and rendered as the **next** chat thread:

- **Single tool call**: `TrajectoryStep(action=action, observation=observation_dict, llm_observation=...)`. Rendered as `{"role": "assistant", "content": "{\"next_node\":\"X\",\"args\":{...}}"}` then `{"role": "user", "content": render_observation(observation, error, failure)}`.
- **Parallel call**: one `TrajectoryStep` whose `observation` is `{"branches": [{node, args, observation|error|failure}, ...], "stats": {success, failed}, "join": {...}?}`. One assistant message, one observation user message — even though N tools ran. The structured shape lets the next LLM step reason over per-branch outcomes without losing identity.
- **`llm_observation` vs `observation`**: Harbor maintains two views — the canonical `observation` for trajectory persistence and audit, and an LLM-facing `llm_observation` that may have been redacted (artifacts replaced with refs, secrets masked) before going back into the prompt. This split lines up with Harbor's "fail-loudly + audit-clean" stance.
- **Ordering**: branches are ordered by their plan index. The next LLM prompt sees the branches in the same order the model proposed them, which preserves the LLM's mental model.

## 6. What the LLM client does (and doesn't) provide

### Required from the LLM client (the entire surface)

- **Async chat completion** taking `messages: Sequence[Mapping[str, str]]` (`role`/`content` only — system/user/assistant — no `tool_calls`, no `tool_call_id`).
- **Optional `response_format`** hint: `{"type": "json_object"}` or `{"type": "json_schema", "json_schema": {...}}`. The runtime is responsible for sanitizing/downgrading per provider; the client just forwards.
- **Optional streaming** with two callbacks: `on_stream_chunk(text, done)` for content deltas and `on_reasoning_chunk(text, done)` for thinking-channel deltas.
- **Cancellation** via `asyncio.timeout(...)` wrapping the call.
- **Cost/usage**: return `str` or `(str, cost: float)`. The runtime aggregates token-cost in its own cost tracker.

That's it. The full LLM-client contract is a six-line interface — one method.

### NOT required from the LLM client

- Provider-native tool calling (`tools=[...]`, `tool_choice=...`, OpenAI `function_call`, Anthropic `tool_use` blocks, Gemini `function_calling`).
- Provider-native parallel tool calls.
- Provider-native tool-call-ID generation.
- A `tool_message` / `tool_result` chat role (it's just a `user` message with structured JSON).
- Schema repair (the runtime does this).
- Cost/usage reporting *per tool call* (the runtime does this from its own dispatch path).

### What a structured-output correction layer actually does

A provider-correction adapter wraps a richer internal client but **implements the exact same single-method `complete` interface**. Its corrections are:

- **Structured-output mode selection** per provider (`response_format` shape, sanitization, `additionalProperties: false`, `strict: true` modes).
- **Message normalization** (some providers reject system messages after the first user turn; some require role merging; some routes need a route-specific transform).
- **Reasoning-effort routing** for thinking-class models (`o1`, `o3`, `deepseek-reasoner`).
- **Retry policy** wrapping the provider completion call with a timeout.

It does **not** add or change tool-calling semantics — it is about provider-native *structured-output* correctness, not provider-native tool calls. Harbor's lesson: **bake structured-output correction into the single LLM client from t=0; don't ship two parallel modes.**

## 7. Implications for Harbor's LLM client

Harbor's `LLMClient` interface is the smallest possible surface. Concretely:

```go
type LLMClient interface {
    // Complete runs one request/response. Streaming is optional and signalled via opts.
    // The runtime owns prompt construction, tool semantics, parsing, and parallel dispatch.
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
}

type CompleteRequest struct {
    Messages       []ChatMessage      // role + content only
    ResponseFormat *ResponseFormat    // nil | json_object | json_schema(schema)
    Stream         bool
    OnContent      func(delta string, done bool) // optional
    OnReasoning    func(delta string, done bool) // optional, for thinking models
    Temperature    *float64
    MaxTokens      *int
    Stops          []string
    // No tools, no tool_choice, no function_call.
}

type CompleteResponse struct {
    Content string
    Cost    Cost   // tokens in/out + dollars; runtime aggregates
    Usage   Usage  // tokens, latency, provider-specific extras
}
```

**Capability checklist `liter-llm` (or any candidate) must satisfy:**

- [ ] Async chat completion with role/content messages.
- [ ] `response_format = {type: json_object}` and `{type: json_schema, json_schema: ...}` passthroughs (runtime sanitizes the schema before passing in).
- [ ] Streaming with content deltas via callback or async iterator.
- [ ] Streaming with reasoning deltas (when the provider exposes them). Optional but valuable.
- [ ] Hard cancellation via `context.Context` (Go) — the call must terminate cleanly when ctx is cancelled.
- [ ] Token usage in the response (`{prompt_tokens, completion_tokens, total_tokens}` shape).
- [ ] Cost in dollars (or a deterministic mapping from usage to cost via a pricing table the runtime owns).
- [ ] At least: OpenAI, Anthropic, Google, DeepSeek, OpenRouter, an "OpenAI-compatible" generic.

**What `liter-llm` does NOT need to support uniformly:** provider-native tool calling, parallel tool calling, function-call mode, provider tool-IDs. **This collapses Q-3 in the RFC** — the library viability question reduces to "can it stream JSON from these providers reliably and report usage."

## 8. Implications for Harbor's tool dispatch subsystem

The tool dispatcher is **inside** the runtime, not the LLM client. Harbor's design pieces:

1. **`ActionParser`** (in `internal/runtime/planner/parser/`): a typed extractor that converts LLM raw text → `PlannerAction`. Owns the JSON-fence extraction, multi-action discovery, schema sanitization for the salvage path. **Not** LLM-shape-aware in the sense of "knows about OpenAI tool_calls"; it only knows Harbor's `next_node`/`args` schema.
2. **`Dispatcher`** (in `internal/runtime/dispatch/`): takes a validated `PlannerAction`, resolves the tool via the `ToolCatalog`, validates `args` against the tool's input schema, runs the tool with deadline + cancellation hooks, validates output, returns a `ToolOutcome`. Single-tool path.
3. **`ParallelDispatcher`** (sibling): given a parallel `PlannerAction`, validates every branch upfront, fans out via `errgroup` or a bounded worker pool, collects per-branch outcomes, runs an optional join tool, returns a `ParallelOutcome` with per-branch identity correlation. Synthetic call IDs are stamped here.
4. **`ObservationRenderer`** (in `internal/runtime/planner/observation/`): given a `(Trajectory, latest step)`, produces the next chat thread. This is where assistant + user messages are interleaved from `(action, observation|error|failure)` pairs. It also performs LLM-facing redaction (heavy outputs replaced with artifact refs).
5. **`RepairLoop`**: drives parser → validator → planner-prompt-on-failure cycles up to `RepairAttempts` (default 3). Loud on exhaust, with the regex finish-fallback as the documented last resort.
6. **`SchemaSanitizer`** (in `internal/llm/correction/`): not in the LLM client — between the runtime and the LLM client. Per-provider response-format adjustments live here. The single LLM client is dumb; the correction layer is a runtime utility called *before* the client request.

All of these are runtime concerns. The LLM client is one method.

## 9. Phase impact

This finding **collapses several phases** in the master plan and **shifts risk** elsewhere:

- **Q-3 (RFC §11) — `liter-llm` viability — substantially de-risked.** Tool-call breadth across providers is no longer a blocking concern. The reduced gating list is: streaming JSON content, structured-output passthrough, cancellation, usage reporting, multi-provider support. If `liter-llm` covers those, it ships. If it doesn't, the fallback is provider-native SDKs, but they still need only those four things.
- **Phase L-2 (LLM client + provider correction)** stays as drafted but the "correction layer" scope shrinks: it covers `response_format` per-provider and message-shape normalization only. It does NOT touch tool-call APIs.
- **Earlier "tool-intent extractor" phase emerges** — call it **D-1: ActionParser**, before any concrete tool driver. It's small (parser + repair loop) but it has to land before any planner concrete can run end-to-end.
- **Parallel dispatch + tool catalog dispatch fold into one earlier phase** (call it **D-2: Dispatcher** unifying single + parallel) — they share the validation, identity, deadline, and cancellation plumbing. Splitting single and parallel dispatch into separate modules is a historical accident; they should be one design unit.
- **Schema-repair loop** is its own small phase (**D-3: RepairLoop**) — needs unit + property tests for convergence.
- **Observation renderer** is its own phase (**D-4: ObservationRenderer**) — heavy because it owns the assistant/user interleaving, redaction, parallel-branch shape, and trajectory persistence.
- **Tool transports (HTTP, MCP, A2A, in-proc)** become "drivers behind the same `ToolCatalog` + `Dispatcher`" — each its own phase but each ~half the original weight because none of them need provider-native tool-calling negotiation.

## 10. Sharp edges to design out

- **Implicit join-arg injection is a foot-gun.** A parallel-join path that injects results by field-name match when no explicit `inject` mapping is given swallows shape mismatches. Harbor requires explicit `inject` mappings from t=0.
- **UI-payload deduplication is a bandage.** Deduplicating re-rendered UI payloads patches the symptom of "the model re-renders the same payload because it can't tell the previous call succeeded from `{ok: true}`." Harbor solves this at the **dispatch contract** level: tool outputs always include a meaningful summary, and the observation renderer doesn't compress to `{ok: true}` for non-trivial side-effects.
- **Last-ditch regex finish-extraction needs its own event.** The regex finish-extraction is *correct* as a final safety net, but logging only a generic "fallback extracted" line hides how often it fires. Harbor must emit a distinct event so operators can quantify how often models fail JSON entirely.
- **Positional synthetic IDs must be session-scoped.** With IDs like `call_{action_seq}_{step_index}`, an `action_seq` that is per-planner-instance only becomes ambiguous in long-lived planners. Harbor: scope synthetic IDs by `(session_id, action_seq, branch_index)` to keep them globally unique within a session and replay-stable.
- **JSON extraction is unaware of nested fenced blocks inside reasoning text.** A model that emits ```` ```python ```` examples followed by ```` ```json {action} ``` ```` can have the wrong block extracted; a first-`{`/last-`}` fallback mitigates but is brittle. Harbor: prefer the multi-object scanner as the primary extractor and fall back to fence-extraction only when the multi-object scan fails.
- **Schema-repair loop is bounded but failure-mode-blind.** If the model's response is *consistently* malformed, a fixed number (default 3) of identical-shape feedback attempts may never converge. Harbor: track repair-attempt diversity (different errors → continue retrying; identical error twice → escalate to a different prompt template).
- **No retry storm guard on the repair loop.** Each attempt costs an LLM call. At 3 attempts × failure × action-seq per session, runaway sessions can incur multiplied cost. Harbor: the `RepairLoop` phase plan must include a per-session budget gate that aborts to a `final_response` with an error answer rather than spinning.
- **Tool-side guardrails fire twice** (start + result). On a STOP decision in either, the call short-circuits — but the result-side guardrail can produce REDACT decisions that mutate `result_json` in-place. Harbor: redaction must be a deterministic transform with an audit trail, not a mutate-in-place.

## 11. Test strategy

- **Unit (parser)**: golden tests for fence extraction, multi-object discovery, alternate-actions extraction; property tests for "JSON-extract is idempotent on already-extracted JSON" and "extract is robust to leading/trailing prose."
- **Unit (repair loop)**: convergence tests — given a failing-then-correct fixture, the loop terminates in ≤N steps; given a permanently-failing fixture, the loop terminates with an explicit error event (no infinite spin).
- **Unit (dispatcher)**: per-tool argument validation; output validation; deadline propagation (mock tool that sleeps past deadline → cancellation); cancellation via steering inbox.
- **Integration (parallel)**: 5-branch parallel plan with one slow tool, one failing tool, one paused tool — assert: gather completes, partial success aggregated, pause propagates, no orphan goroutines, branch order preserved.
- **Conformance (provider-independence)**: a recorded canonical "act-then-tool-then-finish" trajectory must replay against ≥4 LLM provider stubs that all just return the same JSON action stream. Same dispatch path, same outcomes — proving the runtime is provider-agnostic.
- **Property tests**: for any `(action, observation)` pair, `ObservationRenderer` produces a bytes-equal result on repeated calls (deterministic).
- **Chaos**: random subset of branches return errors; random subset paused; random tool exceeds deadline; assert no goroutine leak via `runtime.NumGoroutine()` baseline assertion.
- **Cost-budget tests**: a session with a permanently-failing parser → assert the per-session repair budget aborts the planner and emits a structured "budget_exhausted" event.
- **Identity uniqueness**: across N concurrent sessions and M concurrent runs each, every synthesized `call_<id>` is globally unique within `(session_id, run_id)`.

## 12. Open questions

1. **Repair budget defaults** — a default of 3 attempts is a reasonable starting point. Harbor: same? higher? per-session-token-budget aware?
2. **Reasoning-channel exposure on the protocol** — the LLM client takes an `on_reasoning_chunk` callback; Harbor's Protocol must decide whether reasoning deltas are first-class events for Console (yes, leaning) or a debug-only channel.
3. **`response_format` strategy table** — should the per-provider sanitization policy live in the runtime (compiled in) or in a config file (operator-tunable)? Runtime is faster; config is friendlier when a new provider lands.
4. **Synthetic ID scope** — confirm `(session_id, run_id, action_seq, branch_index)` is the canonical key. A flatter key is ambiguous in long-lived planners; Harbor should pick before D-1.
5. **Tool-intent format flexibility** — the `next_node` + `args` action shape is excellent. Should Harbor tolerate provider-native tool-call shapes (Anthropic `tool_use` blocks, OpenAI `tool_calls`) when a provider emits them despite the prompt? If yes, we need a small adapter; if no, we strictly prompt our way through.

---

## Appendix A — Concept map

The design concepts this brief establishes:

| Harbor concept | Role |
|----------------|-------------------|
| `LLMClient` interface | one-method JSON chat-completion contract |
| Provider-backed concrete client | wires a provider library behind the interface |
| Structured-output correction layer | per-provider `response_format` correctness |
| Action shape | typed `next_node` + `args` planner action |
| JSON extraction | fence-stripping + first-`{`/last-`}` fallback |
| Action normalization | multi-object scan over mixed prose |
| Salvage path | soft fallback for legacy/hybrid shapes |
| Repair loop | parse → validate → repair-prompt retry cycle |
| Single-call dispatch | validate args, run tool, validate output |
| Parallel dispatch | atomic-setup fan-out with deterministic merge |
| Steering / cancel race | tool task vs cancel-event task, first-wins |
| Observation renderer | builds the next chat thread from the trajectory |
| Schema sanitization per provider | downgrades `response_format` per provider quirk |
