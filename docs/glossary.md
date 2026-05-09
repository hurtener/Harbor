# Harbor — Glossary

Authoritative definitions for Harbor-specific vocabulary. Add a term here in the same PR that introduces it. Drift-audit checks that phase plans don't introduce undefined terms in their public API surface section without a matching glossary entry.

When in doubt, the RFC wins (AGENTS.md §15).

---

## A

**ActionParser** — runtime component that extracts a typed `PlannerAction` from raw LLM text. Owns multi-action discovery, JSON-fence extraction, and the salvage path. Knows Harbor's `next_node` / `args` schema; deliberately knows nothing about provider-native tool-call shapes (RFC §6.4).

**Artifact** — a heavy output (large text, binary, structured payload above threshold) routed through the `ArtifactStore` instead of inlined into the event stream by reference. Mandatory routing — no opt-in (RFC §6.10).

**ArtifactRef** — typed reference that points to an artifact in the `ArtifactStore`. Carries content hash, scope, and addressing info. Replaces inline payloads in event streams and LLM prompts.

**ArtifactScope** — `(tenant_id, user_id, session_id, run_id)`-keyed scope under which artifacts live. Inherits the identity triple plus run identity.

## B

**Brief** — a research artifact in `docs/research/NN-*.md`, distilled from predecessor source code and authoritative for context (not design). See `docs/research/INDEX.md`.

## C

**Circuit breaker** — Per-`(provider, key)` health monitor that trips when error rate exceeds threshold and auto-recovers on cool-down. Post-V1, phase 94. RFC §6.15.

**Code-level tool calling** — Harbor's elegance principle. The LLM emits text/JSON describing intent; the runtime parses, dispatches, and merges results. Provider-native tool calling APIs (`tools=[...]`, `tool_choice`, `function_call`) are NOT used. The runtime owns the protocol; providers don't need to. RFC §6.4 + brief 07.

**CompleteRequest / CompleteResponse** — the request/response pair for `LLMClient.Complete(ctx, req) (resp, error)`. Carries `Messages` (role+content), optional `ResponseFormat`, optional streaming callbacks. No `Tools`, no `ToolChoice`. RFC §6.5.

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

**Failover chain** — Operator-defined sequence of providers tried in order when the primary fails or hits its ceiling. Orchestrated by Harbor's Governance subsystem; audited per hop; distinct from bifrost's per-request `Fallbacks` field. Post-V1, phase 93. RFC §6.15.

## G

**Governance** — Harbor's middleware subsystem between the Runtime and the `LLMClient` driver. Owns identity-scoped policies: cost accumulators, ceilings, rate limits, MaxTokens, and (post-V1) key rotation, model swap, failover chains, circuit breakers, caching, PII redaction. The `LLMClient` interface stays one method; bifrost is unaware of identity scopes. RFC §6.15.

## I

**Identity triple** — `(tenant_id, user_id, session_id)`. Every layer carries this. The session is the innermost concurrency *boundary* — but within a session, multiple Runs may execute concurrently and require an additional identity dimension; see *Identity quadruple*. AGENTS.md §6 + RFC §4.

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

**Trajectory** — the planner execution log. First-class artifact; serializable; carries the sequence of `(action, observation|error|failure)` pairs. RFC §6.2.

## U

**Unified pause/resume primitive** — single runtime-level pause/resume that serves HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED` / `INPUT_REQUIRED`, and steering `PAUSE`. NOT per-feature. RFC §3.3 + §6.3 + cross-fork synthesis #1.

## V

**Virtual directory pattern** — pluggable-storage namespace addressing for skills (and potentially other artifacts). Logical paths over a swappable backing store. Inherited from the predecessor (the strongest pattern brief 04 names). RFC §6.7.
