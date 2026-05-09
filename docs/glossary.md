# Harbor — Glossary

Authoritative definitions for Harbor-specific vocabulary. Add a term here in the same PR that introduces it. Drift-audit checks that phase plans don't introduce undefined terms in their public API surface section without a matching glossary entry.

When in doubt, the RFC wins (AGENTS.md §15).

---

## A

**ActionParser** — runtime component that extracts a typed `PlannerAction` from raw LLM text. Owns multi-action discovery, JSON-fence extraction, and the salvage path. Knows Harbor's `next_node` / `args` schema; deliberately knows nothing about provider-native tool-call shapes (RFC §6.4).

**Audit redactor** — single central runtime component that produces a redacted copy of any payload before it is emitted to the event bus, logs, or audit storage. Owner of redaction per D-020 (Audit owns redaction; Governance owns thresholds). Pluggable via the §4.4 extensibility-seam pattern; default driver is pattern-based with built-in rules for credentials (`api_key`, `bearer`, `authorization`, `password`, `secret`, `token`, `cookie`) and configurable PII shapes. Returning an error from `Redact` means the caller MUST NOT emit — silent fall-through to the unredacted payload is forbidden. RFC §6.4 + §6.15.

**Artifact** — a heavy output (large text, binary, structured payload above threshold) routed through the `ArtifactStore` instead of inlined into the event stream by reference. Mandatory routing — no opt-in (RFC §6.10).

**ArtifactRef** — typed reference that points to an artifact in the `ArtifactStore`. Carries content hash, scope, and addressing info. Replaces inline payloads in event streams and LLM prompts.

**ArtifactStub** — the model-agnostic JSON shape the LLM sees in place of heavy content during prompt assembly: `{artifact_ref, mime, size_bytes, hash, summary, fetch{tool, id}}`. Uniform across producers (tool result, memory turn, multimodal input) and providers — operators do not swap formats per model. The runtime stamps the stub; producers fill `Summary` when meaningful. RFC §6.5, D-026.

**ArtifactScope** — `(tenant_id, user_id, session_id, run_id)`-keyed scope under which artifacts live. Inherits the identity triple plus run identity.

## B

**Brief** — a research artifact in `docs/research/NN-*.md`, distilled from predecessor source code and authoritative for context (not design). See `docs/research/INDEX.md`.

## C

**Circuit breaker** — Per-`(provider, key)` health monitor that trips when error rate exceeds threshold and auto-recovers on cool-down. Post-V1, phase 94. RFC §6.15.

**Code-level tool calling** — Harbor's elegance principle. The LLM emits text/JSON describing intent; the runtime parses, dispatches, and merges results. Provider-native tool calling APIs (`tools=[...]`, `tool_choice`, `function_call`) are NOT used. The runtime owns the protocol; providers don't need to. RFC §6.4 + brief 07.

**CompleteRequest / CompleteResponse** — the request/response pair for `LLMClient.Complete(ctx, req) (resp, error)`. Carries `Messages` (role+content; content is text or multimodal `Parts`), optional `ResponseFormat`, optional streaming callbacks. No `Tools`, no `ToolChoice`. RFC §6.5.

**ContentPart** — one element of a multimodal `ChatMessage.Content.Parts` slice. Discriminated by `Type`: text, image, audio, or file. Concrete payload via `Image *ImagePart`, `Audio *AudioPart`, `File *FilePart` (or inline `Text` for text parts). RFC §6.5, D-021.

**Concurrent reuse contract** — Harbor's runtime-wide invariant that compiled artifacts (`flow.Engine`, `Tool`, `Planner`, `MemoryStore`, `Redactor`, `LLMClient`, `ToolCatalog`) are immutable after construction and reusable across N concurrent goroutines without data races, context bleed, cancellation cross-talk, or goroutine leaks. Per-run state lives in `ctx` + `RunContext`, never on the artifact. **Every phase that builds a reusable artifact ships a concurrent-reuse test** (N≥100 invocations under `-race`). RFC §3.5, AGENTS.md §5 + §11 + §13, D-025.

**Context-window safety net** — Harbor's runtime-wide invariant that **no message reaching the `LLMClient` carries raw heavy content**. Multi-stage: producers (tool dispatcher, memory, multimodal input materialization, `ObservationRenderer`) substitute heavy content with `ArtifactRef`s during normal output; a single catch-all pass at the LLM-client edge walks the assembled `CompleteRequest` and fails loudly with `ErrContextLeak` (≥-threshold raw payload found) or `ErrContextWindowExceeded` (estimated tokens within `ContextWindowReserve` of the model's context limit, default 5%). V1 fails loudly; auto-cascading recovery is post-V1. RFC §6.5, D-026.

**Console** — the observability + control-plane UI. Architecturally a Protocol client of the Runtime; ships in its own product/repo. The Runtime never imports it; it never reads Runtime internals. RFC §5 + §7.

**Cost ceiling** — Identity-scoped budget cap (per tenant / user / session, optionally per model). PreCall check; emits `governance.budget_exceeded` on breach; fails loudly with `ErrBudgetExceeded`. RFC §6.15.

## D

**Dispatcher** — runtime component that takes a validated `PlannerAction` and runs it. Single + parallel folded into one design unit. Validates `args` against the tool's input schema, runs with deadline + cancellation, stamps synthetic call IDs, returns `ToolOutcome` / `ParallelOutcome`. RFC §6.4.

**Driver** — a concrete implementation of an interface (per the §4.4 Extensibility seams pattern). Self-registers via `init()`; pulled in via blank import at `cmd/harbor`. Examples: SQLite driver of `StateStore`, OpenRouter driver of `bifrost`, in-proc driver of `Tool`.

## E

**Event bus** — Harbor's typed event subsystem. ONE bus, not two. Protocol-grade, not observability-grade. Replaces the predecessor's split between observability events and chunk-via-message. RFC §6.13.

**Extensibility seam** — the `interface + factory + driver` pattern any subsystem with plausible alternate backends must follow. AGENTS.md §4.4.

## F

**Fail-loudly** — Harbor's runtime principle. Errors are explicit; capabilities are mandatory; identity is mandatory. No `try { ... } catch { return None }`-shaped patterns. AGENTS.md §5 (Errors) + §13.

**Flow** — a typed DAG of `Node`s assembled into a runnable unit. Built on the same engine that powers subflows; can be registered as a Tool via `flow.RegisterAsTool(...)` so the planner invokes a multi-step orchestration the same way it invokes a single Tool. Per-node `NodePolicy` (retry / exponential backoff / timeout / validation) plus aggregate `flow.Budget` (deadline / hop budget / cost cap) compose with identity-tier Governance ceilings. RFC §6.1, D-023.

**`flow.Definition`** — the canonical Go shape describing a Flow: name, description, entry/exit nodes, node specs, optional intrinsic `Budget`, and derived `InSchema` / `OutSchema`. V1 operators write `Definition`s in Go; V1.1 adds a YAML recipe loader that parses into the same struct. RFC §6.1, D-023.

**`flow.Budget`** — per-flow intrinsic cap on `Deadline`, `HopBudget`, and `CostCap`. Enforced at flow boundaries via `min()` against parent-run `RunContext.Budget` and identity-tier ceilings; whichever fires first aborts the flow with `ErrFlowBudgetExceeded`. RFC §6.1, D-023.

**Failover chain** — Operator-defined sequence of providers tried in order when the primary fails or hits its ceiling. Orchestrated by Harbor's Governance subsystem; audited per hop; distinct from bifrost's per-request `Fallbacks` field. Post-V1, phase 93. RFC §6.15.

## G

**Governance** — Harbor's middleware subsystem between the Runtime and the `LLMClient` driver. Owns identity-scoped policies: cost accumulators, ceilings, rate limits, MaxTokens, and (post-V1) key rotation, model swap, failover chains, circuit breakers, caching, PII redaction. The `LLMClient` interface stays one method; bifrost is unaware of identity scopes. RFC §6.15.

## I

**Identity triple** — `(tenant_id, user_id, session_id)`. Every layer carries this. The session is the innermost concurrency *boundary* — but within a session, multiple Runs may execute concurrently and require an additional identity dimension; see *Identity quadruple*. AGENTS.md §6 + RFC §4.

**ImagePart / AudioPart / FilePart** — typed payloads for `ContentPart`. Each carries one of `URL` (provider-fetchable), `DataURL` (inline base64), or `Artifact *artifacts.Ref` (canonical Harbor reference per D-022) plus a `MIME` type. The runtime auto-materializes `DataURL` content above the heavy-output threshold (32 KB default, RFC §6.10) into `ArtifactRef`s before event emission, audit, and persistence. RFC §6.5, D-021/D-022.

**Identity quadruple** — the identity triple plus `RunID`. Used in `Envelope`s and run-scoped data flow (artifacts, state checkpoints, per-run audit). The triple is the load-bearing **isolation** key (cross-session leakage is forbidden); the `RunID` is the per-execution scope **within** a session. RFC §6.1, §6.10.

## L

**LLMClient** — Harbor's interface for talking to an LLM provider. **One method**: `Complete(ctx, req) (resp, error)`. Tool dispatch is runtime-side. The single V1 driver wraps `bifrost`. RFC §6.5.

## M

**Memory strategy** — declared policy that controls how a session's memory is shaped: `none`, `truncation`, `rolling_summary`. Identity-mandatory; fail-closed. RFC §6.6.

## O

**ObservationRenderer** — runtime component that turns a `(Trajectory, latest step)` into the next chat thread, interleaving assistant and user messages from `(action, observation | error | failure)` pairs and applying LLM-facing redaction (heavy outputs replaced with `ArtifactRef`s). RFC §6.4.

## P

**Planner** — the reasoning-policy interface: `Next(ctx, RunContext) (PlannerDecision, error)`. Concrete planners (ReAct first; Plan-Execute, Workflow, Graph, Deterministic, Supervisor, MultiAgent, HumanApproval over time) all sit on the same Runtime primitives. RFC §6.2 + §3.2.

**PlannerAction** — typed instruction emitted by a planner step. Reserved opcodes: `final_response`, `parallel`, `task.subagent`, `task.tool`. Plus tool-name actions. Carries `args`. Action shape is provider-independent. RFC §6.4.

**PlannerDecision** — the sum type returned from `Planner.Next`. Variants describe the next runtime mechanism to invoke (call-tool, emit-final, request-pause, spawn-subagent, etc.). See RFC §6.2 for the full variant list.

**Protocol (Harbor Protocol)** — the canonical event/state contract between the Runtime and any client (Console, CLI, third-party). Versioned. RFC §5.

## R

**Rate limit** — Identity-scoped token-bucket throttle on LLM calls keyed by `(identity, model)`. Bucket state persisted in StateStore so it survives runtime restart. PreCall check; emits `governance.rate_limited`; fails with `ErrRateLimited`. RFC §6.15.

**Recipe** — a declarative (YAML/JSON) representation of a `flow.Definition` so operators can author flows without writing Go. Parses into the same `Definition` struct the runtime consumes. **V1.1 (post-V1 phase 100)** — V1 ships Go-coded `Definition`s; the recipe loader is a parser added later without changing the contract. RFC §6.1, D-023.

**RepairLoop** — the runtime's recovery loop for malformed planner output. Drives `parser → validator → planner-prompt-on-failure` cycles up to `RepairAttempts`. Loud on exhaust. RFC §6.4.

**Run** — one execution of the planner loop within a Session. A Session contains many Runs. `RunID` is for runtime concurrency; `TraceID` (OTel) may span Runs.

**RunContext** — passed to each `Planner.Next` call. Carries identity (the triple), tools available, memory snapshot, control surface (`RunContext.Control`), trajectory pointer, deadlines. The planner reads from this; it never reads runtime internals directly.

## S

**SchemaSanitizer** — runtime utility that lives BETWEEN the runtime and the `LLMClient` (NOT inside the client). Applies per-provider `response_format` shape adjustments before the request goes out. RFC §6.5.

**Sentinel errors** — typed errors that mark specific failure modes the runtime expects callers to compare against with `errors.Is`. The settled set:
- `ErrUnserializable` — pause-state cannot be JSON-serialized; raised loudly by the pause/resume serialize path (RFC §6.3, brief 02).
- `ErrToolContextLost` — pause-resume found a non-serializable handle key with no live runtime mapping; the pause cannot resume (RFC §6.3).
- `ErrBudgetExceeded` — Governance PreCall: identity ceiling reached (RFC §6.15).
- `ErrRateLimited` — Governance PreCall: token bucket exhausted (RFC §6.15).
- `ErrMaxTokensExceeded` — Governance PreCall: per-call MaxTokens cap hit (RFC §6.15).
- `ErrKeyUnavailable` — Governance PreCall: no usable provider key after rotation/circuit-breaker (RFC §6.15, post-V1).
Additions to this set are RFC PRs.

**Session** — a longer-lived multi-turn conversation that contains many Runs. Identity for runtime concerns is `(tenant, user, session)`. RFC §6.9.

**Skill** — a token-savvy unit of operational know-how the runtime can search and inject. Distinct from Portico's distribution role; Harbor consumes via `SkillProvider`. RFC §6.7.

**SkillProvider** — interface for skill sources (LocalDB, Portico via MCP, Git, OCI, HTTP). Drivers under `internal/skills/providers/*`. Extensibility-seam pattern.

**Steering** — out-of-band runtime control: `CANCEL`, `REDIRECT`, `INJECT_CONTEXT`, `USER_MESSAGE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `PRIORITIZE`. Lives at the runtime level; planners see only `RunContext.Control`. RFC §3.3 + §6.3.

## T

**Task** — a unit of work the Runtime executes for a Planner. Foreground (within a Run) or Background (long-running). Identity unified: one `TaskID` with `Kind=foreground|background`. RFC §6.8.

**TaskID** — stable identifier for a Task. Format includes `Kind`. Schema-keyed at the StateStore level. Closes the predecessor's `trace_id` vs `task_id` split (brief 05).

**ToolContext** — per-tool-call runtime context split into a JSON-encodable half (persisted across pause/resume) and a runtime-handle half (re-attached by key on resume). The split is a fail-loudly contract: serializing the JSON-half MUST raise `ErrUnserializable` if any field is non-serializable rather than silently dropping data; resuming a missing handle raises `ErrToolContextLost`. RFC §6.3, brief 02.

**ToolPolicy** — the reliability shell applied to every tool invocation regardless of `Transport`. Mirrors `NodePolicy` (§6.1): `TimeoutMS`, `MaxRetries`, `BackoffBase`, `BackoffMax`, `RetryOn` (error classes), `Validate`. Sensible defaults fire on zero-value so `tools.RegisterFunc(name, fn)` is production-resilient with no ceremony. RFC §6.4, D-024.

**Trajectory** — the planner execution log. First-class artifact; serializable; carries the sequence of `(action, observation|error|failure)` pairs. RFC §6.2.

## U

**Unified pause/resume primitive** — single runtime-level pause/resume that serves HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED` / `INPUT_REQUIRED`, and steering `PAUSE`. NOT per-feature. RFC §3.3 + §6.3 + cross-fork synthesis #1.

## V

**Virtual directory pattern** — pluggable-storage namespace addressing for skills (and potentially other artifacts). Logical paths over a swappable backing store. Inherited from the predecessor (the strongest pattern brief 04 names). RFC §6.7.
