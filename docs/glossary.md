# Harbor — Glossary

Authoritative definitions for Harbor-specific vocabulary. Add a term here in the same PR that introduces it. Drift-audit checks that phase plans don't introduce undefined terms in their public API surface section without a matching glossary entry.

When in doubt, the RFC wins (AGENTS.md §15).

---

## A

**A2A (Agent-to-Agent)** — the open protocol Harbor adopts for cross-agent communication. Vendored spec at `docs/specifications/a2a.proto` (pinned at commit `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`, 2026-04-23); full spec compliance is settled per D-007 + D-031. Every A2A RPC has a Go counterpart on `RemoteTransport`; every A2A message has a Go shape in `internal/distributed/a2a`.

**A2A `Task`** — A2A's task abstraction. Distinct from Harbor's `tasks.Task` (Phase 20): Harbor's task is the local-runtime unit; A2A's `Task` is what a remote agent uses to model the same execution. Mapping happens at the Phase 29 boundary. RFC §6.12, D-031.

**A2A `Message`** — A2A's communication unit between client and server. Carries role (user / agent), parts (oneof: text / raw bytes / URL / structured data), context/task IDs, extensions, and metadata. Distinct from Harbor's runtime envelope.

**A2A `Part`** — A2A's discriminated message-content carrier (oneof: text, raw bytes, URL, structured data). Each part carries `media_type` + optional `filename`. Distinct from Harbor's `ContentPart` (D-021); the LLM-side multimodal types map onto A2A `Part` at the southbound boundary. RFC §6.4 + §6.12.

**Adjacency** — A `(From Node, To []Node)` pair the engine's `New` consumes to allocate channels. The full set of adjacencies forms the runtime DAG (with cycle opt-in per node). RFC §6.1.

**ActionParser** — runtime component that extracts a typed `PlannerAction` from raw LLM text. Owns multi-action discovery, JSON-fence extraction, and the salvage path. Knows Harbor's `next_node` / `args` schema; deliberately knows nothing about provider-native tool-call shapes (RFC §6.4).

**Audit redactor** — single central runtime component that produces a redacted copy of any payload before it is emitted to the event bus, logs, or audit storage. Owner of redaction per D-020 (Audit owns redaction; Governance owns thresholds). Pluggable via the §4.4 extensibility-seam pattern; default driver is pattern-based with built-in rules for credentials (`api_key`, `bearer`, `authorization`, `password`, `secret`, `token`, `cookie`) and configurable PII shapes. Returning an error from `Redact` means the caller MUST NOT emit — silent fall-through to the unredacted payload is forbidden. RFC §6.4 + §6.15.

**Artifact** — a heavy output (large text, binary, structured payload above threshold) routed through the `ArtifactStore` instead of inlined into the event stream by reference. Mandatory routing — no opt-in (RFC §6.10, D-022, D-026).

**ArtifactRef** — typed reference returned by `ArtifactStore.Put*` and resolved by `GetRef`. `ID = "{namespace}_{sha256_hex[:12]}"`; carries `MimeType`, `SizeBytes`, `Filename`, full `SHA256`, `Scope`, `Namespace`, and an opaque `Source map[string]any` for caller metadata. Replaces inline payloads in event streams and LLM prompts. RFC §6.10.

**ArtifactScope** — `(TenantID, UserID, SessionID, TaskID)` ownership tuple for an artifact. Identity-mandatory at the API boundary (tenant/user/session); empty `TaskID` is acceptable for session-scoped artifacts (parallels `state.StateStore`'s session-vs-run rule). `List` treats empty fields as wildcards. The consumer maps the runtime's `identity.Quadruple{Identity, RunID}` onto this shape (`RunID → TaskID` for foreground runs); the store stays decoupled from `internal/identity`. RFC §6.10.

**ArtifactStore** — Harbor's mandatory content-addressed blob store. Single eight-method interface (Phase 17) with two V1 drivers — `inmem` (zero-dependency floor) and `fs` (single-binary production target with `<root>/<tenant>/<user>/<session>/<task>/<namespace>/<id>` layout + atomic-rename writes + path-traversal guard). Phase 18 adds SQLite-blob + Postgres-blob; Phase 19 adds the S3-style driver (AWS S3 / MinIO / Cloudflare R2 / any S3-compat backend, built on `aws-sdk-go-v2`, with the optional `Presigner` capability for read-side URL hand-off); all inherit Phase 17's conformance suite verbatim. NO `NoOp` fallback (D-022, D-026). RFC §6.10, §9.

**ArtifactStub** — the model-agnostic JSON shape the LLM sees in place of heavy content during prompt assembly: `{artifact_ref, mime, size_bytes, hash, summary, fetch{tool, id}}`. Uniform across producers (tool result, memory turn, multimodal input) and providers — operators do not swap formats per model. The runtime stamps the stub; producers fill `Summary` when meaningful. RFC §6.5, D-026.

**A2A `Artifact`** — A2A's task-output container, carrying parts plus name, description, and extensions. Distinct from Harbor's `Artifact` (the heavy-output content-addressed blob). The two converge at the Phase 29 boundary when an A2A peer's artifact is materialised onto Harbor's `ArtifactStore`. RFC §6.12, D-031.

**AgentCard** — A2A's self-describing manifest for an agent. Carries name, capabilities, skills, supported interfaces (gRPC / JSON-RPC / HTTP+JSON), security schemes. Harbor consumes peers' AgentCards through `RemoteTransport.GetExtendedAgentCard`. RFC §6.12, D-031.

**AgentInterface** — A2A's declaration of a target URL + protocol binding (`JSONRPC` / `GRPC` / `HTTP+JSON`) + protocol version. An `AgentCard` carries one or more `AgentInterface`s. RFC §6.12, D-031.

**AgentSkill** — A2A's declaration of a distinct capability the agent exposes (id, name, description, tags, examples, input/output modes, security requirements). Distinct from Harbor's `Skill` (the token-savvy skill subsystem). RFC §6.12, D-031.

**A2A peer** — a remote agent Harbor connects to as a *client* via the A2A protocol. Declared in `ToolsConfig.A2APeers`. Distinct from "A2A northbound", which is the not-yet-shipped server surface (V1.1 candidate, RFC §6.4). The Phase 29 wire driver (`internal/distributed/drivers/a2a`) reads the peer list at construction and refuses any URL not in the allowlist. RFC §6.4, D-007.

**Agent Card cache** — the Phase 29 wire driver's in-memory TTL cache for `GET <peer>/.well-known/agent-card.json` responses. Default TTL 10 minutes; per-peer override via `A2APeerConfig.AgentCardTTL`. Coalesces concurrent first-time fetches via an inflight map so N concurrent `Discover` calls collapse into one underlying HTTP GET. RFC §6.4.

**AuthSpec** — Phase 27 HTTP tool driver: static-auth specification attached to an HTTP tool, carrying `Kind` (`api_key` / `bearer` / `cookie`) plus the kind-specific field (`HeaderName` xor `QueryParam` for api_key, `CookieName` for cookie). The secret value lives separately in operator config — never in the request payload or URL template. Templates that reference the `.Auth` namespace are rejected at load time (`ErrTemplateSecretLeak`). AGENTS.md §7, RFC §6.4.

**Route scoring** — Phase 29's deterministic selection of an A2A peer when more than one declares the same capability. Score formula at `internal/distributed/drivers/a2a/registry.go`: `(5 × TrustTier) + (1000 / max(1, LatencyTierMS)) + (10 × CapabilityScore)` — trust outranks latency; latency is the tie-breaker among similarly-trusted peers; capability match adds an additive boost. Tie-breakers: lower latency, then URL ascending so the result is reproducible. RFC §6.4, D-038.

## B

**Brief** — a research artifact in `docs/research/NN-*.md`, distilled from predecessor source code and authoritative for context (not design). See `docs/research/INDEX.md`.

**`BusEnvelope`** — the unit `MessageBus.Publish` accepts. Carries the identity quadruple, task ID, edge / source / target labels, the (pre-redacted) payload bytes, a caller-supplied `EventID` for idempotency keying, headers + metadata, and a timestamp. Identity-mandatory; consumers MUST be idempotent on `(TaskID, Edge, EventID)`. RFC §6.12, D-031.

**BifrostDriver** — Harbor's adapter (Phase 33) that wires `github.com/maximhq/bifrost/core` (the pure-Go LLM gateway settled by RFC §11 Q-3 / brief 08) behind `llm.Driver`. Self-registers under `"bifrost"`; blank-imported in `cmd/harbor`. Thin translation layer: `CompleteRequest` ↔ `BifrostChatRequest`, multimodal `ContentPart` → bifrost's `ChatContentBlock` shapes, cost passthrough → `llm.cost.recorded` emit. Provider-native tool-calling fields are intentionally never set (RFC §6.4 / brief 07; the Phase 32 smoke static guard enforces).

**BifrostContext** — `*schemas.BifrostContext`, bifrost's custom `context.Context` implementation that tracks user-set values and propagates cancellation. Harbor constructs one per `Complete` via `schemas.NewBifrostContext(ctx, schemas.NoDeadline)`. Wraps Harbor's parent ctx so cancellation propagates upstream; bifrost's internal goroutines exit when the upstream HTTP body completes (brief 08 §"Cancellation caveat").

## C

**`Cancel(runID)`** — `Engine` method (Phase 13) that idempotently cancels a run: sets a per-run cancellation flag, drops queued envelopes for that run from every channel, cancels in-flight worker invocations, drains the egress subqueue, releases capacity waiters. Returns `(bool, error)` — `true` if the run was active. Cancellation is remembered for a bounded TTL (default 60s) so an `Emit` landing just after `Cancel` is rejected with `ErrRunCancelled`. RFC §6.1, brief 01 §4.

**Cancellation TTL** — Bounded duration (default 60s) the engine remembers per-run cancellation flags for runs that may not have started yet. A periodic sweeper (every 10s) prunes expired flags; the goroutine is joined on `Engine.Stop`. Configurable via `WithCancelTTL(d)`. RFC §6.1.

**Capacity waiter (engine)** — Per-run `sync.Cond` the engine uses to gate `EmitChunk` when a run's pending-frame count has reached its `RunCapacity`. Released when the dispatcher drains a frame from the run's subqueue, or when `Stop` closes the engine with `ErrEngineStopped`. The mechanism that prevents the predecessor's deadlock-under-streaming sharp edge. RFC §6.1, brief 01 §4.

**Circuit breaker** — Per-`(provider, key)` health monitor that trips when error rate exceeds threshold and auto-recovers on cool-down. Post-V1, phase 94. RFC §6.15.

**Code-level tool calling** — Harbor's elegance principle. The LLM emits text/JSON describing intent; the runtime parses, dispatches, and merges results. Provider-native tool calling APIs (`tools=[...]`, `tool_choice`, `function_call`) are NOT used. The runtime owns the protocol; providers don't need to. RFC §6.4 + brief 07.

**Cycle detector (engine)** — Topological-sort-style check `engine.New` runs at construction time. Rejects unintended cycles with `ErrCycleDetected` listing the cycle path; per-node `AllowCycle: true` opts out so legitimate self-loop nodes (e.g. controller-loop planners) compose. RFC §6.1, brief 01 §3.

**Cursor (events)** — `(SessionID, Sequence)` pair identifying the last event a subscriber has consumed. Used by `Replayer.Replay` to compute "events strictly newer than this." Sequence is the per-bus monotonic value assigned by `Publish`; SessionID scopes the cursor so two subscribers on different sessions can use the same numeric Sequence without collision. RFC §6.13, brief 06 §4, D-029.

**CompleteRequest / CompleteResponse** — the request/response pair for `LLMClient.Complete(ctx, req) (resp, error)`. Carries `Messages` (role+content; content is text or multimodal `Parts`), optional `ResponseFormat`, optional streaming callbacks. No `Tools`, no `ToolChoice`. RFC §6.5.

**ContentPart** — one element of a multimodal `ChatMessage.Content.Parts` slice. Discriminated by `Type`: text, image, audio, or file. Concrete payload via `Image *ImagePart`, `Audio *AudioPart`, `File *FilePart` (or inline `Text` for text parts). RFC §6.5, D-021.

**Concurrent reuse contract** — Harbor's runtime-wide invariant that compiled artifacts (`flow.Engine`, `Tool`, `Planner`, `MemoryStore`, `Redactor`, `LLMClient`, `ToolCatalog`) are immutable after construction and reusable across N concurrent goroutines without data races, context bleed, cancellation cross-talk, or goroutine leaks. Per-run state lives in `ctx` + `RunContext`, never on the artifact. **Every phase that builds a reusable artifact ships a concurrent-reuse test** (N≥100 invocations under `-race`). RFC §3.5, AGENTS.md §5 + §11 + §13, D-025.

**Context-window safety net** — Harbor's runtime-wide invariant that **no message reaching the `LLMClient` carries raw heavy content**. Multi-stage: producers (tool dispatcher, memory, multimodal input materialization, `ObservationRenderer`) substitute heavy content with `ArtifactRef`s during normal output; a single catch-all pass at the LLM-client edge walks the assembled `CompleteRequest` and fails loudly with `ErrContextLeak` (≥-threshold raw payload found) or `ErrContextWindowExceeded` (estimated tokens within `ContextWindowReserve` of the model's context limit, default 5%). V1 fails loudly; auto-cascading recovery is post-V1. The pass is mandatory by construction — `internal/llm.Open` returns a wrapper that runs it before delegating to the underlying driver (D-039). RFC §6.5, D-026, D-039.

**ContextWindowReserve** — fraction of a model's context-window cap (`ModelProfile.ContextWindowTokens`) held back as a safety margin (default 0.05 / 5%). The LLM-edge safety pass fails with `ErrContextWindowExceeded` when the estimated token count of the assembled `CompleteRequest` is within this fraction of the cap. Configured at `LLMConfig.ContextWindowReserve`. RFC §6.5, D-026.

**Console** — the observability + control-plane UI. Architecturally a Protocol client of the Runtime; ships in its own product/repo. The Runtime never imports it; it never reads Runtime internals. RFC §5 + §7.

**Cost ceiling** — Identity-scoped budget cap (per tenant / user / session, optionally per model). PreCall check; emits `governance.budget_exceeded` on breach; fails loudly with `ErrBudgetExceeded`. RFC §6.15.

**CorrectionsProfile** — Per-model bundle of provider-quirk flags consumed by the Phase 34 corrections layer. Lives on `llm.ModelProfile.Corrections`. Zero-value means "no quirks declared for this model"; the corrections layer treats every zero field as the Harbor-default behaviour (no reorder, no schema mutation, OpenAI envelopes, usage backfill off). Fields: `MessageOrdering`, `SchemaMode`, `ReasoningEffortRouting`, `ResponseFormatShape`, `UsageBackfillEnabled`. RFC §6.5, D-041, brief 03 §4, brief 08.

**Corrections layer** — `internal/llm/corrections`. The wrapper that sits between Harbor's runtime and the Phase 32 `safetyClient(driver)`. Rewrites `CompleteRequest`s per `ModelProfile.Corrections` before delegating, and optionally backfills `Usage` when the driver returns all-zeros. Compose order: `corrections(safetyClient(driver))` — the safety pass sees the post-correction request (D-041). Five quirks: message reordering (NIM), schema sanitization (`additionalProperties`/`strict`), reasoning-effort routing (thinking-class models), response-format envelope translation, usage backfill. Single baked-in mode — no `use_native` toggle (brief 03 §5). RFC §6.5, D-041.

## D

**`DeadlineAt`** — Wall-clock deadline on an `Envelope`. Set once at the API boundary; checked before scheduling each node (Phase 10 worker loop). `nil` means "no deadline." Distinct from `Policy.TimeoutMS` (per-node timeout) and `flow.Budget.Deadline` (per-flow). RFC §6.1, brief 01 §2.

**`DefaultQueueSize`** — `64`. Default bounded per-adjacency channel capacity in `internal/runtime/engine`. Settled per RFC §6.1 (resolves brief 01 Q-4). Engine-wide override via `WithQueueSize(n)`; per-channel override via `WithChannelOverride(from, to, n)`.

**Dispatcher (engine)** — Single always-on goroutine the engine runs to demux egress envelopes by `RunID`. Phase 10 ships the dispatcher; Phase 13's `FetchByRun(runID)` reads from a per-run subqueue managed by it. Distinct from the tool-call dispatcher (RFC §6.4) — which takes a validated `PlannerAction` and runs it. RFC §6.1, brief 01 §4.

**Dispatcher (tools)** — runtime component that takes a validated `PlannerAction` and runs it. Single + parallel folded into one design unit. Validates `args` against the tool's input schema, runs with deadline + cancellation, stamps synthetic call IDs, returns `ToolOutcome` / `ParallelOutcome`. RFC §6.4.

**Driver** — a concrete implementation of an interface (per the §4.4 Extensibility seams pattern). Self-registers via `init()`; pulled in via blank import at `cmd/harbor`. Examples: SQLite driver of `StateStore`, OpenRouter driver of `bifrost`, in-proc driver of `Tool`.

## E

**`EmitChunk`** — `NodeContext` method (Phase 12) that emits a `StreamFrame`. Blocks when the originating run's pending-frame count has reached `Policy.RunCapacity`. Backpressure is per-run; one run's saturation never pauses another. The mechanism that makes streaming under parallel runs deadlock-free. RFC §6.1, brief 01 §4.

**Engine** — Harbor's runtime container — the typed, async, queue-backed graph executor. One in-memory implementation in V1 (`internal/runtime/engine`). Owns the worker loop (one goroutine per `Node`), bounded per-adjacency channels, the always-on egress dispatcher, cycle detection at construction, the reliability shell (`NodePolicy`), the streaming primitive (`EmitChunk`), per-run cancellation. Distinct from `events.EventBus` (the cross-subsystem event bus); the engine is the runtime kernel. RFC §6.1.

**Envelope** — Harbor's canonical message shape: `Payload`, `Headers`, identity quadruple (`RunID`, `SessionID` plus `Headers.{TenantID, UserID}`), `Timestamp`, `DeadlineAt`, free-form `Meta`. Flows along every runtime channel. Defined in `internal/runtime/messages`. RFC §6.1, brief 01 §2.

**Event bus** — Harbor's typed event subsystem. ONE bus, not two. Protocol-grade, not observability-grade. Replaces the predecessor's split between observability events and chunk-via-message. RFC §6.13.

**EventID** — a caller-supplied ULID used as the canonical idempotency key on `StateStore.Save`. Same EventID + same Bytes is a no-op; same EventID + different Bytes returns `ErrIdempotencyConflict`. `state.NewEventID()` is a convenience helper backed by `oklog/ulid`. RFC §6.11, D-027.

**EventPayload** — the sealed Go interface every concrete bus payload type embeds (via `events.Sealed`) to satisfy. The seal is enforced at compile time — declaring a payload requires importing `internal/events` so external types can't bypass the contract. RFC §6.13, D-028.

**Extensibility seam** — the `interface + factory + driver` pattern any subsystem with plausible alternate backends must follow. AGENTS.md §4.4.

## F

**Fail-loudly** — Harbor's runtime principle. Errors are explicit; capabilities are mandatory; identity is mandatory. No `try { ... } catch { return None }`-shaped patterns. AGENTS.md §5 (Errors) + §13.

**`FetchByRun(runID)`** — `Engine` method (Phase 13) that reads from the dispatcher's per-run subqueue. Concurrent fetchers per run are forbidden (`ErrConcurrentFetchByRun`) — the brief-01-recommended "no half-measure" choice for per-run roundtrip semantics. RFC §6.1, brief 01 §4-§5.

**Filter (events)** — the server-enforced subscription predicate on `EventBus.Subscribe`. Mandates the identity triple (`Tenant`, `User`, `Session`) unless `Admin` is set; the bus rejects empty-triple non-admin filters with `ErrIdentityScopeRequired` and audit-emits `audit.admin_scope_used` whenever admin scope is exercised. Optional `Types` slice filters by `EventType`. RFC §6.13, brief 06 §3-§4.

**Flow** — a typed DAG of `Node`s assembled into a runnable unit. Built on the same engine that powers subflows; can be registered as a Tool via `flow.RegisterAsTool(...)` so the planner invokes a multi-step orchestration the same way it invokes a single Tool. Per-node `NodePolicy` (retry / exponential backoff / timeout / validation) plus aggregate `flow.Budget` (deadline / hop budget / cost cap) compose with identity-tier Governance ceilings. RFC §6.1, D-023.

**`flow.Definition`** — the canonical Go shape describing a Flow: name, description, entry/exit nodes, node specs, optional intrinsic `Budget`, and derived `InSchema` / `OutSchema`. V1 operators write `Definition`s in Go; V1.1 adds a YAML recipe loader that parses into the same struct. RFC §6.1, D-023.

**`flow.Budget`** — per-flow intrinsic cap on `Deadline`, `HopBudget`, and `CostCap`. Enforced at flow boundaries via `min()` against parent-run `RunContext.Budget` and identity-tier ceilings; whichever fires first aborts the flow with `ErrFlowBudgetExceeded`. RFC §6.1, D-023.

**Failover chain** — Operator-defined sequence of providers tried in order when the primary fails or hits its ceiling. Orchestrated by Harbor's Governance subsystem; audited per hop; distinct from bifrost's per-request `Fallbacks` field. Post-V1, phase 93. RFC §6.15.

**`FailFast`** — `TaskGroup` flag (Phase 21): when true, the first member task that transitions to `StatusFailed` cancels the remaining non-terminal members AND transitions the group to `GroupCancelled`. Cancel reason is derived from the failing member's error code (`fail-fast:<code>` or `fail-fast` when no code is set). RFC §6.8, brief 05 §4.

## G

**Governance** — Harbor's middleware subsystem between the Runtime and the `LLMClient` driver. Owns identity-scoped policies: cost accumulators, ceilings, rate limits, MaxTokens, and (post-V1) key rotation, model swap, failover chains, circuit breakers, caching, PII redaction. The `LLMClient` interface stays one method; bifrost is unaware of identity scopes. RFC §6.15.

**GCPolicy** — Configuration knob group for the `SessionRegistry`'s GC sweeper. Defaults: `IdleTTL=24h, HardCap=720h (30d), SweepInterval=15m`. Carries the `RunningProbe` seam through which `TaskRegistry` (Phase 20) gates "never reap a session with a RUNNING task." Fields are not hot-reloadable in V1. RFC §6.9.

**`GroupCompletion`** — typed wake-up payload delivered by `WatchGroup` (and as the `task.group_resolved` / `task.group_cancelled` bus-event payload). Carries the group's terminal status (`GroupCompleted` | `GroupCancelled`), resolve timestamp, cancel reason (when cancelled), and a `MemberOutcome` per group member. Heavy results MUST already be substituted with `ArtifactRef`s upstream (D-022, D-026); the payload is ref-shaped, not byte-bound. The same canonical shape across consumers (Console, durable-event-log, sidecar status emitters, planner runtime). RFC §6.8, Phase 21.

## H

**Headers (envelope)** — Routing + identity sub-record on `Envelope`: `TenantID`, `UserID`, `Topic`, `Priority`. Distinct from HTTP headers; the term is RFC-settled vocabulary. RFC §6.1, brief 01 §2.

**HeavyOutputThreshold** — the byte size at which the runtime mandatorily routes a payload through the `ArtifactStore`. Default 32 KB (`config.ArtifactsConfig.HeavyOutputThresholdBytes`); runtime-configurable. Per-tool overrides land at Phase 26 via the tool catalog. Phase 17 ships the config field + default; enforcement lives at consumer layers — tool dispatcher (Phase 26) auto-routes, LLM-edge (Phase 32) fails loudly with `ErrContextLeak` if raw heavy content slipped through. D-022, D-026, RFC §6.5, §6.10.

## I

**Identity triple** — `(tenant_id, user_id, session_id)`. Every layer carries this. The session is the innermost concurrency *boundary* — but within a session, multiple Runs may execute concurrently and require an additional identity dimension; see *Identity quadruple*. AGENTS.md §6 + RFC §4.

**ImagePart / AudioPart / FilePart** — typed payloads for `ContentPart`. Each carries one of `URL` (provider-fetchable), `DataURL` (inline base64), or `Artifact *artifacts.Ref` (canonical Harbor reference per D-022) plus a `MIME` type. The runtime auto-materializes `DataURL` content above the heavy-output threshold (32 KB default, RFC §6.10) into `ArtifactRef`s before event emission, audit, and persistence. RFC §6.5, D-021/D-022.

**Identity quadruple** — the identity triple plus `RunID`. Used in `Envelope`s and run-scoped data flow (artifacts, state checkpoints, per-run audit). The triple is the load-bearing **isolation** key (cross-session leakage is forbidden); the `RunID` is the per-execution scope **within** a session. RFC §6.1, §6.10.

**IdempotencyKey** — caller-supplied string on `tasks.SpawnRequest` that, when paired with `Identity.SessionID`, deduplicates retried spawns. Same `(SessionID, IdempotencyKey)` → returns the original `TaskHandle` with `Reused=true`; divergent SpawnRequest under the same key returns `ErrIdempotencyConflict`. Empty key disables dedup entirely (every Spawn yields a fresh handle, no collisions). The key is namespaced by SessionID, so the same key across different sessions creates two distinct tasks. RFC §6.8.

## J

**`JoinK`** — Concurrency utility (Phase 14) that reads exactly K envelopes from a channel and cancels remaining producers. Short-read returns `ErrJoinKShortRead`. RFC §6.1, brief 01 §2.

## L

**LLMClient** — Harbor's interface for talking to an LLM provider. **One method**: `Complete(ctx, req) (resp, error)`. Tool dispatch is runtime-side. The single V1 driver wraps `bifrost`. RFC §6.5.

## M

**`MapConcurrent`** — Concurrency utility (Phase 14) that runs `fn` over a slice of envelopes with a max-in-flight bound. Preserves input order in output. Per-run capacity backpressure and cancellation propagate through the bound. RFC §6.1, brief 01 §2.

**`MemberOutcome`** — per-task entry inside `GroupCompletion`. Carries `TaskID`, terminal `Status` (`StatusComplete` | `StatusFailed` | `StatusCancelled`), and either `Result` (when complete) or `Error` (when failed); neither is populated on cancel. Heavy results are substituted with `ArtifactRef`s upstream (D-022, D-026); the entry is ref-shaped, not byte-bound. RFC §6.8, Phase 21.

**Memory strategy** — declared policy that controls how a session's memory is shaped: `none`, `truncation`, `rolling_summary`. Identity-mandatory; fail-closed. RFC §6.6.

**`MemoryStore`** — Harbor's mandatory memory subsystem interface. Seven methods (`AddTurn / GetLLMContext / EstimateTokens / Flush / Health / Snapshot / Restore`) plus `Close`. Phase 23 ships the InMem driver with `Strategy = none` operational; Phase 24 adds `truncation` + `rolling_summary`; Phase 25 ships SQLite + Postgres drivers under the same conformance suite. Identity-mandatory at every method; fail-closed on missing triple with `memory.identity_rejected` audit emit. RFC §6.6, AGENTS.md §6, D-001, D-027, D-033.

**`ConversationTurn`** — one turn of a memory-tracked conversation (`UserMessage`, `AssistantResponse`, optional `TrajectoryDigest`, artifact references, timestamp). The planner runtime (Phase 42+) is the producer; `MemoryStore.AddTurn` is the consumer. RFC §6.6.

**`MemoryHealth`** — `MemoryStore.Health` return: `healthy | retry | degraded | recovering`. Phase 23 only produces `healthy` (Strategy=none); Phase 24's `rolling_summary` drives the full FSM. RFC §6.6.

**`MemorySnapshot`** — the export shape returned by `MemoryStore.Snapshot` and consumed by `Restore`. Carries `(Strategy, Bytes)`. Bytes are opaque to consumers; the snapshot's `Strategy` must match the driver's at Restore time, else `ErrInvalidSnapshot`. Empty-zero snapshots round-trip the initial state. RFC §6.6.

**`LLMContextPatch`** — the patch a planner runtime applies to its LLM call after `MemoryStore.GetLLMContext`. Carries `(Strategy, Summary, RecentTurns, Tokens)`. Empty under Strategy=none. RFC §6.6.

**`memory.identity_rejected`** — bus event emitted when a `MemoryStore` method is called with an incomplete identity triple. `SafePayload` (bounded operation name + reason string; no caller-controlled bytes). The Event's `Identity` field carries the partial input with `"<missing>"` substitution for empty components (so `ValidateEvent`'s triple check passes); the payload's `Reason` field names the truly missing component(s). Subscribers MAY admin-scope-filter to fan-in cross-tenant rejections. RFC §6.6, D-001, D-033.

**`memory.health_changed`** — bus event emitted on every `Health` FSM transition under `rolling_summary` (`healthy ↔ retry ↔ degraded ↔ recovering`). `SafePayload` carries `(PriorHealth, NewHealth, Reason)` — bounded enumerable strings, no caller-controlled bytes. The explicit exception to AGENTS.md §13's "no silent degradation" rule: degraded mode IS the observable failure surface, and emitting the event makes it observable (and therefore not silent). Phase 24. RFC §6.6, D-034.

**`memory.recovery_dropped`** — bus event emitted when the `rolling_summary` recovery backlog overflows `RecoveryBacklogMax` and the executor drops the oldest queued batch to make room. `SafePayload` carries the drop reason; identity scopes the emit. Phase 24. RFC §6.6, D-034.

**`Summarizer`** — the injectable callable the `rolling_summary` memory strategy consumes. Single method `Summarize(ctx, id, req) (resp, error)` mirroring brief 04 §4.1's "input `{previous_summary, turns}`, output `{summary: string}`" shape. Phase 24 ships only the interface + a test-grade stub (`strategy.EchoSummarizer`); the LLM-backed implementation lands at Phase 32+. The interface is identity-aware so implementers can scope LLM calls; the executor cancels in-flight summaries on `Close`. RFC §6.6, D-034.

**`OverflowPolicy`** — buffer-overflow action under `truncation`. Phase 24 ships only `OverflowDropOldest` (drop oldest turns until the buffer's token estimate fits within `BudgetTokens`). Brief 04 §2's `truncate_summary` and `error` policies are deliberately omitted — the former conflates strategies, the latter is a silent-degradation footgun. RFC §6.6, D-034.

**`RecoveryBacklogMax`** — bounded queue size for the `rolling_summary` recovery loop. Default 16; overflow drops oldest and emits `memory.recovery_dropped`. Operator-tunable via `config.MemoryConfig.RecoveryBacklogMax`. The retry / backoff / cadence knobs from brief 04 §2 are NOT operator-tunable at Phase 24 — they live as constants on the rolling-summary executor (see D-034). RFC §6.6, D-034.

**`StrategyExecutor`** — the strategy-side surface a `MemoryStore` driver delegates to. Lives in `internal/memory/strategy/`; concrete implementations preserve `none` semantics (Phase 23), implement `truncation` (drop-oldest recent-window buffer), and implement `rolling_summary` (background-summarised long-term context with the `Health` FSM + bounded recovery loop). Strategies are behaviour modes of any memory driver per AGENTS.md §4.4 — Phase 25's SQLite + Postgres drivers will host the same executors verbatim. RFC §6.6, D-034.

**Meta (envelope)** — Free-form `map[string]any` propagated with the envelope. Last-write-wins on key collisions in V1; an explicit merge-function registry is reserved for a future RFC follow-up. Survives fan-out / fan-in / subflow boundaries. RFC §6.1, brief 01 §2.

**`MessageBus`** — Harbor's at-least-once cross-worker fan-out edge (`internal/distributed`). V1 ships an in-process loopback driver that projects published `BusEnvelope`s through the typed `events.EventBus`; durable backends (NATS / Redis Streams / Postgres-as-queue) are post-V1 phase 86. Handlers MUST be idempotent on `(TaskID, Edge, EventID)`. RFC §6.12, D-031.

**`MCPTransportMode`** — selects the wire transport for an MCP southbound attachment (Phase 28): `auto` / `sse` / `streamable_http` / `stdio`. `auto` tries streamable-HTTP first (with SSE fallback when the URL endpoint rejects it); stdio is selected when only `Command` is set. The validator's allowlist in `internal/config/validate.go` is mirrored verbatim against the driver's `IsValidTransportMode` (pinned by a drift test). RFC §6.4, brief 03 §4, D-034.

**`mcp.resource_updated`** — canonical event type emitted when an MCP southbound server pushes a `notifications/resources/updated` for a URI the driver previously `Subscribe`d to. `SafePayload` by construction — the URI is operator-trust-equivalent (server-controlled) and the source ID is operator-supplied. Registered in `internal/tools/drivers/mcp/events.go` via the driver's `init()`. RFC §6.4.

**ModelProfile** — per-model knobs (`ContextWindowTokens`, `TokenEstimator`, `JSONSchemaMode`, `DefaultMaxTokens`, `ReasoningEffort`, `CostOverrides`, `Corrections`). Keyed by canonical model name in `LLMConfig.ModelProfiles`. The LLM-edge safety pass requires a profile entry for every model that appears in a `CompleteRequest`; missing profiles surface as `ErrUnsupportedModel` at request time. Phase 32 ships the shape; Phase 33 reads `ContextWindowTokens` / `TokenEstimator` / `ReasoningEffort`; Phase 34 reads `Corrections` (per-provider quirk flags); Phase 35 reads `JSONSchemaMode`; Phase 36a/36b read `DefaultMaxTokens` + `CostOverrides`. RFC §6.5, brief 08 ("Per-model seam"), D-041.

**MessageNormalizer** — Phase 34 corrections-layer helper. `normalizeMessages([]ChatMessage, MessageOrderingPolicy) ([]ChatMessage, error)`. Today the only non-default policy is `OrderingSystemFirstStrict` (collapse all system-role messages to the front, preserving relative order). Operator-set via `ModelProfile.Corrections.MessageOrdering`. RFC §6.5, D-041.

**MessageOrderingPolicy** — enum on `CorrectionsProfile.MessageOrdering`. Values: `""` (default — passthrough); `"system_first_strict"` (NIM-style ordering). Brief 03 §4 documents the NIM quirk. RFC §6.5, D-041.

## O

**ObservationRenderer** — runtime component that turns a `(Trajectory, latest step)` into the next chat thread, interleaving assistant and user messages from `(action, observation | error | failure)` pairs and applying LLM-facing redaction (heavy outputs replaced with `ArtifactRef`s). RFC §6.4.

**OnContent** — optional content-delta streaming callback on `CompleteRequest`. `func(delta string, done bool)`. Fires for each text chunk when `req.Stream` is true; the final invocation has `done=true`. Drivers concatenate the deltas into `CompleteResponse.Content` before returning. RFC §6.5.

**OnReasoning** — optional thinking-channel-delta streaming callback on `CompleteRequest`. Provider-specific — fires only for thinking-class models (`o1`, `o3`, `deepseek-reasoner`) that expose a separate reasoning channel. Same shape as `OnContent`. RFC §6.5.

## N

**Node (engine)** — A typed async function inside the engine. Wraps a `NodeFunc` plus `NodePolicy` (Phase 11) and a per-node `AllowCycle` opt-in. One worker goroutine per node; one bounded channel per outgoing adjacency. RFC §6.1, brief 01 §2.

**`NodePolicy`** — Per-node reliability config: `Validate`, `TimeoutMS`, `MaxRetries`, `BackoffBase`/`Mult`/`MaxBackoff`, `ValidateFunc`, `RunCapacity` (Phase 12). Zero value is "no policy" (Phase 10's bare worker). Sensible defaults are set explicitly, not silently — fail-loud per AGENTS.md §5. RFC §6.1.

## P

**ProviderRouting** — the per-Harbor-instance bifrost provider selection (`LLMConfig.Provider`). Phase 33 V1 supports one configured provider per Harbor binary; the operator's `harbor.yaml` names the bifrost provider (e.g. `openrouter`, `openai`, `anthropic`) and the per-model `LLMConfig.ModelProfiles` keys carry the upstream identifier. Multi-provider routing per Harbor instance is a post-V1 consideration; if a deployment needs multiple LLM endpoints, an operator runs multiple Harbor instances.

**`ApplyPatch`** — registry action for accepting or rejecting a pending context patch (proposed by a planner / human reviewer). Patches transition `pending → applied | rejected` through the `TaskRegistry`'s typed surface (Phase 21). The patch payload is opaque bytes (the actual context-patch shape lives at the planner, Phase 42+); the registry stores + retrieves. Emits `task.patch_applied` or `task.patch_rejected` on a real transition; idempotent re-apply returns `(false, nil)`. RFC §6.8.

**`AcknowledgeBackground`** — registry action marking a list of completed background tasks as user-acknowledged (Phase 21). Emits one `task.background_acknowledged` event per task on the real-transition path. Idempotent on re-ack; unknown / non-background / non-terminal tasks are silently skipped (the returned count reflects only the real transitions). RFC §6.8.

**Planner** — the reasoning-policy interface: `Next(ctx, RunContext) (PlannerDecision, error)`. Concrete planners (ReAct first; Plan-Execute, Workflow, Graph, Deterministic, Supervisor, MultiAgent, HumanApproval over time) all sit on the same Runtime primitives. RFC §6.2 + §3.2.

**Push wake (background continuation)** — wake mode where the planner subscribes via `WatchGroup`; the runtime delivers a `GroupCompletion` payload at resolve time; the planner re-enters with the payload as input. Lowest latency, lowest cost — the planner sleeps until something actually happened. Suits long-running background work where intermediate progress isn't actionable. Phase 21 ships the mechanism; planner concretes (Phase 42+) wire the mode.

**Poll wake (background continuation)** — wake mode where the planner periodically calls `Get(taskID)` or `ListGroups(sessionID, filter)` until the group's status is terminal. No subscription required; suits planners interleaving background-work checks with other deterministic work, or environments where push delivery isn't reliable. Phase 21 ships the mechanism; planner concretes (Phase 42+) wire the mode.

**`PredicateRouter`** — Router (Phase 14) that selects the first branch whose predicate matches the input envelope. Default target catches "no match"; nil default returns `RunError(RouteNotFound)`. RFC §6.1.

**PlannerAction** — typed instruction emitted by a planner step. Reserved opcodes: `final_response`, `parallel`, `task.subagent`, `task.tool`. Plus tool-name actions. Carries `args`. Action shape is provider-independent. RFC §6.4.

**PlannerDecision** — the sum type returned from `Planner.Next`. Variants describe the next runtime mechanism to invoke (call-tool, emit-final, request-pause, spawn-subagent, etc.). See RFC §6.2 for the full variant list.

**`PresignGet`** — the read-side presigned-URL operation on the `Presigner` capability. `PresignGet(ctx, scope, id, expiry) (string, error)` returns a time-bounded HTTPS URL the caller can hand to a Console / Protocol client for direct download without proxying bytes. Bounded to `[1 minute, 7 days]` per S3's documented limit; out-of-range expiries are rejected (fail loudly). Identity is mandatory at this boundary; missing tenant/user/session returns wrapped `ErrIdentityRequired`. Read-side only — write-side presigned URLs are an attack surface intentionally not exposed at V1. RFC §6.10.

**`Presigner`** — optional capability interface (`internal/artifacts/presigner.go`) implemented only by backends with native presigned-URL support. The Phase 19 S3-style driver is the sole V1 implementor; the in-memory, FS, SQLite-blob, and Postgres-blob drivers do NOT implement it (verified by negative type-assertion tests). Callers type-assert from `ArtifactStore`; absence is a typed error (`ErrPresignUnsupported`), never a silent fallback. The explicit exception to AGENTS.md §4.4's no-optional-capability rule — only S3-compat stores have presigned URLs natively, and the capability cannot be reasonably faked by the other V1 drivers without bolting on a separate signing service. RFC §6.10, brief 05 §3.

**Protocol (Harbor Protocol)** — the canonical event/state contract between the Runtime and any client (Console, CLI, third-party). Versioned. RFC §5.

**PropagateOnCancel** — `tasks.Task` field controlling how `Cancel` walks descendants: `"cascade"` (default; cancels descendants in BFS order, emitting `task.cancelled` per cancel) or `"isolate"` (cancellation stays local to the target). Per `tasks.SpawnRequest`; the resolved value is stored on the Task and consulted at Cancel time. RFC §6.8.

**`TaskPushNotificationConfig`** — A2A's per-task push-notification configuration (URL + auth credentials + optional token). Harbor's `RemoteTransport` exposes CRUD (Create / Get / List / Delete); V1's loopback driver stores in memory, post-V1 + Phase 29 add durability + outbound dispatch. RFC §6.12, D-031.

**Push wake — see `Push wake`** (above).

## R

**Rate limit** — Identity-scoped token-bucket throttle on LLM calls keyed by `(identity, model)`. Bucket state persisted in StateStore so it survives runtime restart. PreCall check; emits `governance.rate_limited`; fails with `ErrRateLimited`. RFC §6.15.

**ReasoningEffort** — request-level hint mapped to per-provider reasoning controls (`off` / `low` / `medium` / `high` / `""`). Bifrost's `ChatReasoning` is the bridge for V1 providers; empty string means "use provider default" (the safety pass does not touch the field). Settable per request or via `ModelProfile.ReasoningEffort` defaults. RFC §6.5.

**ReasoningRouting** — enum on `CorrectionsProfile.ReasoningEffortRouting`. Values: `""` (default — bifrost's `Reasoning.Effort` consumes the hint); `"thinking_model"` (clears top-level `ReasoningEffort` and surfaces it in `req.Extra["reasoning_effort"]`). The thinking-class models (`o1`, `o3`, `deepseek-reasoner`) interpret the hint via a provider-specific path that bifrost passes opaquely. Brief 03 §4. RFC §6.5, D-041.

**ResponseFormatProfile** — enum on `CorrectionsProfile.ResponseFormatShape`. Values: `""` (default — OpenAI envelope); `"json_only"` (downgrade `FormatJSONSchema` to `FormatJSONObject` for providers that reject `json_schema`; the schema is stashed in `Extra["schema_hint"]`); `"anthropic"` (package schema in `Extra["anthropic_tool_schema"]`; clear top-level `ResponseFormat`). The corrections layer translates the envelope before the bifrost driver runs its own per-provider translator. RFC §6.5, D-041.

**Reliability shell** — Phase 11 worker-loop wrapper that applies `NodePolicy` per invocation: validate-in → invoke-with-timeout → retry-with-backoff → on terminal failure, emit `RunError` to logger + bus. Backoff math is exponential with jitter (`base * mult^attempt + jitter`, capped at `MaxBackoff`). RFC §6.1, brief 01 §4.

**ResponseFormat** — optional structured-output hint on `CompleteRequest`. Kinds: `text` (no constraint; default when `req.ResponseFormat == nil`), `json_object` (provider's "JSON mode"), `json_schema` (caller-supplied JSON Schema in strict mode). Phase 35 owns the per-provider downgrade chain `json_schema → json_object → text` on `invalid_json_schema` errors. RFC §6.5.

**Replayer (events)** — optional capability interface (`Replay(ctx, Cursor, Filter) ([]Event, error)`) that drivers may implement to support replay-from-cursor. The core `EventBus` interface stays at three methods; callers type-assert `bus.(events.Replayer)`. Returns events strictly newer than the cursor that match the filter, in `Sequence` order — no duplicates and no gaps within any single `RunID`. Returns `ErrCursorTooOld` when the cursor is older than the in-memory ring's tail (caller falls through to the durable log driver, Phase 57); returns `ErrReplayUnavailable` when retention is disabled (`EventsConfig.ReplayBufferSize=0`). RFC §6.13, D-029.

**`RegisterHTTPTool`** — inline registration helper (Phase 27) at `internal/tools/drivers/http`: `RegisterHTTPTool(cat, name, method, urlTemplate, opts...) error`. Mirrors `inproc.RegisterFunc`'s ergonomics for the HTTP transport. URL / body / header templates use `text/template` with `urlquery` escaping; the `.Auth` namespace is rejected at register time. Operators with operator-deployment manifests use `LoadManifest` + `RegisterManifest` instead. RFC §6.4.

**`RoutePolicy`** — Override mechanism (Phase 14) that bypasses predicate / union routing when an envelope's `Meta["route_policy"]` carries an explicit target. The planner-driven path. RFC §6.1.

**`RetainTurn`** — `TaskGroup` flag (Phase 21); when true, the owning session blocks foreground-turn dispatch until the group reaches a terminal state. The runtime engine reads `RetainTurn` and subscribes via `RegisterRetainTurnWaiter`; the waiter channel closes when the group resolves so the engine can resume turn dispatch. Distinct from the `WatchGroup` mechanism (which never blocks the foreground). RFC §6.8, brief 05 §4.

**Ring buffer (events)** — in-memory bounded retention queue inside the events `inmem` driver; default 10000 entries (configurable via `EventsConfig.ReplayBufferSize`). Eviction is drop-oldest. Distinct from per-subscriber buffers — ring eviction is a documented retention policy, not a delivery failure, and emits no `bus.dropped` notice.

**RunningProbe** — function-typed seam (`func(ctx, identity.Quadruple) (bool, error)`) the `SessionRegistry`'s GC consults before reaping a session. Default returns `(false, nil)`; Phase 20 (`TaskRegistry`) wires the real probe so GC honors "never reap a session with a RUNNING task." RFC §6.9.

**Recipe** — a declarative (YAML/JSON) representation of a `flow.Definition` so operators can author flows without writing Go. Parses into the same `Definition` struct the runtime consumes. **V1.1 (post-V1 phase 100)** — V1 ships Go-coded `Definition`s; the recipe loader is a parser added later without changing the contract. RFC §6.1, D-023.

**`RemoteTransport`** — Harbor's cross-process / cross-host call surface, designed end-to-end against the A2A v1 spec (`internal/distributed`). Every A2A `A2AService` RPC maps 1:1 to a Go method on this interface (Send / Stream / GetTask / ListTasks / Cancel / Subscribe / push-notification-config CRUD / GetExtendedAgentCard / Close). V1 ships an in-process loopback driver; the production A2A wire driver is Phase 29 (southbound). Identity-mandatory via `ctx`. RFC §6.12, D-031.

**`RemoteCallRequest`** — input shape for `RemoteTransport.Send` and `RemoteTransport.Stream`. Carries `AgentURL`, `Kind` (send / stream / subscribe), `ContextID`, `TaskID`, the A2A `Message`, optional `SendMessageConfiguration`, and a per-call `Timeout`. Wire-neutral; drivers translate to the configured A2A binding. RFC §6.12, D-031.

**RedactedMap** — the post-redaction payload form for events whose `EventPayload` did not implement `SafePayload`. The audit redactor's reflective walk normalises a struct payload to `map[string]any`; the bus wraps that result in `RedactedMap` so it still satisfies `EventPayload` for delivery to subscribers. Subscribers extract redacted fields via `RedactedMap.Data`. RFC §6.13, D-028.

**RepairLoop** — the runtime's recovery loop for malformed planner output. Drives `parser → validator → planner-prompt-on-failure` cycles up to `RepairAttempts`. Loud on exhaust. RFC §6.4.

**Run** — one execution of the planner loop within a Session. A Session contains many Runs. `RunID` is for runtime concurrency; `TraceID` (OTel) may span Runs.

**`RunCapacity`** — Per-run cap on pending stream frames (Phase 12). Default = `DefaultQueueSize` (64). Overridable per-run via `WithRunCapacity(n)` on `Engine.Emit`. The mechanism that gates `EmitChunk`'s capacity waiter. RFC §6.1.

**`RunError`** — Structured error envelope from the runtime's reliability shell (Phase 11). Carries `RunID`, `NodeName`, `NodeID`, `Code` (one of `node_timeout / node_exception / run_cancelled / deadline_exceeded / validation_failed`), `Message`, `Cause`, `Metadata`. Routes to logger + bus unconditionally; egress emission opt-in via `WithErrorEmissionToEgress`. RFC §6.1.

**`runtime.run_cancelled`** — `SafePayload` event type (Phase 13). Emitted by `Cancel(runID)` when the run was active. Carries `{run_id, cancelled_at, dropped_envelope_count}`. RFC §6.1.

**RunContext** — passed to each `Planner.Next` call. Carries identity (the triple), tools available, memory snapshot, control surface (`RunContext.Control`), trajectory pointer, deadlines. The planner reads from this; it never reads runtime internals directly.

## S

**SchemaSanitizer** — Phase 34 corrections-layer helper. `sanitizeSchema(json.RawMessage, SchemaSanitizationMode) (json.RawMessage, error)`. Walks the operator-supplied JSON-Schema bytes and applies per-mode mutations: `openai_strict` adds `additionalProperties:false`+`strict:true` on every object-typed schema; `permissive` strips both fields. Operator-set via `ModelProfile.Corrections.SchemaMode`. RFC §6.5, D-041.

**SchemaSanitizationMode** — enum on `CorrectionsProfile.SchemaMode`. Values: `""` (default — passthrough); `"openai_strict"` (insert structured-output required fields); `"permissive"` (strip them). Brief 03 §4 documents the per-provider variation. RFC §6.5, D-041.

**ScopedArtifacts** — immutable facade carrying a fixed `ArtifactScope`; auto-stamps writes, scope-checks reads (returns `ErrScopeMismatch` if the underlying ref's scope ever differs). Tools and runtime use the facade exclusively — they never see raw `ArtifactScope`. `NewScoped` panics on invalid scope at construction (fail loud, AGENTS.md §5). RFC §6.10.

**Sentinel errors** — typed errors that mark specific failure modes the runtime expects callers to compare against with `errors.Is`. The settled set:

- `ErrUnserializable` — pause-state cannot be JSON-serialized; raised loudly by the pause/resume serialize path (RFC §6.3, brief 02).
- `ErrToolContextLost` — pause-resume found a non-serializable handle key with no live runtime mapping; the pause cannot resume (RFC §6.3).
- `ErrBudgetExceeded` — Governance PreCall: identity ceiling reached (RFC §6.15).
- `ErrRateLimited` — Governance PreCall: token bucket exhausted (RFC §6.15).
- `ErrMaxTokensExceeded` — Governance PreCall: per-call MaxTokens cap hit (RFC §6.15).
- `ErrKeyUnavailable` — Governance PreCall: no usable provider key after rotation/circuit-breaker (RFC §6.15, post-V1).
- `ErrContextLeak` — LLM-edge safety pass: raw heavy content survived every producer's normalization step (`internal/llm`, RFC §6.5, D-026, D-039).
- `ErrContextWindowExceeded` — LLM-edge safety pass: assembled `CompleteRequest`'s estimated token count is within `ContextWindowReserve` of the model's configured `ContextWindowTokens` cap (`internal/llm`, RFC §6.5, D-026, D-039).
Additions to this set are RFC PRs.

**Session** — a longer-lived multi-turn conversation that contains many Runs. Identity for runtime concerns is `(tenant, user, session)`. RFC §6.9.

**`SecurityScheme`** — A2A's discriminated union of supported authentication schemes (API key, HTTP auth, OAuth 2.0, OpenID Connect, mutual TLS). Used by `AgentCard.security_schemes`. Each variant is a distinct Go type implementing the `SecurityScheme` interface; runtime discrimination via `Kind()`. RFC §6.12, D-031.

**SessionRegistry** — Harbor's session lifecycle subsystem. One concrete implementation, `StateStore`-backed (no driver pluralism — sessions consume the StateStore conformance suite for cross-driver correctness). Open / Get / Touch / Close / Inspect / GC. Identity captured immutably on `Open`; reopen-after-close rejected; cross-tenant `SessionID` reuse rejected with `ErrSessionIDReuse`. Carries the canonical example of the D-027 typed-wrapper pattern (`SessionRegistry.Save(s Session)` reduces to `StateStore.Save(StateRecord{Identity: q, Kind: "session.lifecycle", Bytes: marshal(s)})`). RFC §6.9.

**Skill** — a token-savvy unit of operational know-how the runtime can search and inject. Distinct from Portico's distribution role; Harbor consumes via `SkillProvider`. RFC §6.7.

**SkillProvider** — interface for skill sources (LocalDB, Portico via MCP, Git, OCI, HTTP). Drivers under `internal/skills/providers/*`. Extensibility-seam pattern.

**Steering** — out-of-band runtime control: `CANCEL`, `REDIRECT`, `INJECT_CONTEXT`, `USER_MESSAGE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `PRIORITIZE`. Lives at the runtime level; planners see only `RunContext.Control`. RFC §3.3 + §6.3.

**Sealed (events)** — the empty `events.Sealed` struct embedded in concrete payload types to satisfy the `EventPayload` seal. Standard Go pattern (mirrors `net/netip.Addr`'s seal). External payload types compose `Sealed` directly; bus-internal types compose `SafeSealed` (which itself embeds `Sealed`) so they additionally implement `SafePayload`. RFC §6.13, D-028.

**SafePayload** — a marker interface (composing `EventPayload`) for payloads whose contents are known not to carry secrets. The bus skips the audit redactor for `SafePayload` types — typed access is preserved on the subscriber side. Bus-internal payloads (`BusDroppedPayload`, `SubscriptionIdleClosedPayload`, `AuditRedactionFailedPayload`, `AdminScopeUsedPayload`) are SafePayload by construction; external payloads default to redactor-walked. RFC §6.13, D-028.

**Subscription (events)** — the typed handle returned by `EventBus.Subscribe`. Owns one bounded buffer per subscriber, drops the oldest event on saturation (emitting `bus.dropped` once per `DropWindow` with the dropped sequence range), and is reaped after `IdleTimeout` of un-drained backlog when the buffer is non-empty (a quiet bus does not trigger reaping; the reaper observes saturation, not silence). `Cancel()` is idempotent. RFC §6.13, brief 06 §4.

**StateRecord** — the unit of persistence on `StateStore`. Carries `(EventID, Quadruple, Kind, Version, Bytes, UpdatedAt)`. `Bytes` is opaque to the store — callers serialize their domain types and run them through audit redaction upstream of `Save`. `Version` is a hint for typed wrappers' optimistic-concurrency checks; the store does not enforce CAS. RFC §6.11, D-027.

**StateStore** — Harbor's persistence floor. Single mandatory five-method interface keyed on `(identity.Quadruple, Kind, Bytes)` with idempotency on a caller-supplied `EventID` (ULID). Three V1 drivers (in-memory, SQLite, Postgres) all pass the same `state.conformancetest.Run` suite. Consuming subsystems (sessions, tasks, governance, planner, memory, steering) land typed wrappers atop this generic surface — the leaf interface holds no domain types. RFC §6.11, §9, D-027.

**`StreamFrame`** — Chunked payload tied to a parent run (Phase 12). `StreamID` (defaults to `RunID`), `Seq` (engine-assigned, monotonic per StreamID), `Text`, `Done`, `Meta`. Distinct from `events.Event` (lifecycle markers); StreamFrames carry incremental output. RFC §6.1, brief 01 §2.

**`Subflow`** — Runtime primitive (Phase 14): `(nctx *NodeContext) CallSubflow(ctx, factory) (Envelope, error)`. Runs a child engine for one parent envelope, mirrors parent cancellation via a watcher goroutine, returns the first egress payload, then `Stop`s the child. RFC §6.1, brief 01 §4.

## T

**Task** — a unit of work the Runtime executes for a Planner. Foreground (within a Run) or Background (long-running). Identity unified: one `TaskID` with `Kind=foreground|background`. Lifecycle FSM: `Pending → Running → Complete`, with `Paused → Running` and terminal `Failed | Cancelled`. RFC §6.8.

**`TaskGroup`** — a sealed-or-open collection of tasks tracked as a unit for parallel-fan-out / retain-turn / aggregate-cancel semantics (Phase 21). Members spawn into the group via `SpawnRequest.GroupID`; `SealGroup` freezes membership; the driver resolves the group automatically when all members reach terminal states. `RetainTurn` blocks the foreground turn until resolve; `FailFast` cancels remaining members when one fails. Cross-session group membership is forbidden; nesting is post-V1. RFC §6.8, brief 05.

**`TaskGroupID`** — ULID-shaped identifier for a `TaskGroup`. The caller MAY pre-assign in `GroupRequest.ID` for idempotency; empty → the registry assigns a fresh ULID. RFC §6.8, Phase 21.

**`TaskGroupStatus`** — group lifecycle state. Values: `open`, `sealed`, `completed`, `cancelled`. FSM enforced at the driver: `Open → Sealed → Completed | Cancelled` (with the direct `Open → Cancelled` edge); `Completed` and `Cancelled` are terminal. Invalid transitions return `ErrGroupInvalidTransition`. RFC §6.8, Phase 21.

**TaskID** — ULID-shaped identifier unifying foreground runs and background tasks. Single namespace; `TaskKind` distinguishes the two. Closes the predecessor's `trace_id` vs `task_id` split (brief 05). Assigned by the registry; callers do not construct TaskIDs externally. RFC §6.8.

**TaskKind** — `"foreground"` (a run inside a session's primary turn) or `"background"` (a spawned-without-blocking task). Both share the same TaskID namespace; this field is the discriminator. RFC §6.8.

**`TaskStatusUpdateEvent`** — A2A streaming-event type emitted by an agent to notify the client of a change in a task's status (`state`, `message`, `timestamp`). Delivered via `SendStreamingMessage` / `SubscribeToTask`; Harbor's `RemoteEventStream.Recv` returns these inside `StreamResponse.StatusUpdate`. RFC §6.12, D-031.

**`TaskArtifactUpdateEvent`** — A2A streaming-event type emitted by an agent when an artifact is generated or appended (`artifact`, `append`, `last_chunk`). Delivered via `SendStreamingMessage` / `SubscribeToTask`; Harbor's `RemoteEventStream.Recv` returns these inside `StreamResponse.ArtifactUpdate`. RFC §6.12, D-031.

**TaskStatus** — lifecycle state. Values: `pending`, `running`, `paused`, `complete`, `failed`, `cancelled`. FSM enforced at the registry; invalid transitions return `ErrInvalidTransition` (wrapped with from/to states named in the message). Same-state transitions are invalid (no idempotent self-edges). Terminal states (`complete`, `failed`, `cancelled`) have no outgoing edges. RFC §6.8.

**TaskRegistry** — the orchestration surface for spawning, listing, cancelling, prioritising, and driving the lifecycle FSM of tasks. One mandatory interface; one V1 driver (`inprocess`); future durable backends post-V1 (Phase 87+). The Mark* methods are the lifecycle drive-points called by the runtime engine; Cancel / Prioritize are caller-initiated (planner, steering, Console). Phase 20 ships the per-task surface; Phase 21 extends it with groups + retain-turn + WatchGroup (D-030). RFC §6.8.

**Tool** — Harbor's planner-addressable unit. Same struct regardless of `Transport` (`inprocess` / `http` / `mcp` / `a2a` / `flow`); the unification is at the type level (brief 03 §1). Carries `ArgsSchema`, `OutSchema`, `Policy` (the reliability shell), `Source` (provider ID), and a `Loading` mode (`always` / `deferred`). RFC §6.4.

**ToolCatalog** — the planner-addressable registry. Three-method interface: `Register(d)`, `Resolve(name)`, `List(filter)`. V1 ships the in-memory catalog (`tools.NewCatalog`); future drivers (remote-catalog, persistent-catalog) plug in behind the same interface. Concurrent reuse safe (D-025): RWMutex-guarded; descriptors immutable after Register. RFC §6.4.

**ToolDescriptor** — the callable binding produced by a driver: `Tool` + `Invoke(ctx, args) (ToolResult, error)` + `Validate(args) error`. The planner never sees a `ToolDescriptor`; the dispatcher uses it. RFC §6.4.

**ToolProvider** — interface for external tool sources (HTTP / MCP / A2A). Phase 27+ drivers implement `Connect` / `Discover` / `Close` / `SourceID`. Phase 26 ships the interface shape; the in-process registrar does not need a provider lifecycle (it's a thin wrapper around `ToolCatalog.Register`). RFC §6.4.

**ToolContext** — per-tool-call runtime context split into a JSON-encodable half (persisted across pause/resume) and a runtime-handle half (re-attached by key on resume). The split is a fail-loudly contract: serializing the JSON-half MUST raise `ErrUnserializable` if any field is non-serializable rather than silently dropping data; resuming a missing handle raises `ErrToolContextLost`. RFC §6.3, brief 02.

**ToolPolicy** — the reliability shell applied to every tool invocation regardless of `Transport`. Mirrors `NodePolicy` (§6.1): `TimeoutMS`, `MaxRetries`, `BackoffBase`, `BackoffMax`, `RetryOn` (error classes), `Validate`. Sensible defaults fire on zero-value so `tools.RegisterFunc(name, fn)` is production-resilient with no ceremony. RFC §6.4, D-024.

**TransportKind** — discriminator on `Tool.Transport`. V1 values: `inprocess` (a Go function registered via `inproc.RegisterFunc`), `http` (Phase 27), `mcp` (Phase 28), `a2a` (Phase 29), `flow` (a typed DAG of Nodes registered as a Tool via `flow.RegisterAsTool`). RFC §6.4.

**SideEffect** — declared side-effect class on `Tool.SideEffects`: `pure` / `read` / `write` / `external` / `stateful`. Operators reason about which classes are safe to retry / parallelize. RFC §6.4.

**LoadingMode** — `always` (the planner always sees this Tool in its prompt-time catalog) or `deferred` (loaded lazily on demand). RFC §6.4.

**CatalogFilter** — server-enforced visibility predicate on `ToolCatalog.List`. Keys on the `(tenant, user, session)` triple plus `GrantedScopes`. A Tool is visible only if every entry in its `AuthScopes` is contained in `GrantedScopes`. `LoadingModes` defaults to `[LoadingAlways]` for the prompt-time view. RFC §6.4.

**Trajectory** — the planner execution log. First-class artifact; serializable; carries the sequence of `(action, observation|error|failure)` pairs. RFC §6.2.

## U

**`UnionRouter`** — Router (Phase 14) that dispatches by payload tag (a string discriminator). Used for sum-type-shaped payloads (e.g. planner `Decision` variants in Phase 42). RFC §6.1.

**Unified pause/resume primitive** — single runtime-level pause/resume that serves HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED` / `INPUT_REQUIRED`, and steering `PAUSE`. NOT per-feature. RFC §3.3 + §6.3 + cross-fork synthesis #1.

**UTCP manifest** — UTCP-style YAML schema (Phase 27) describing HTTP endpoints as Harbor tools. Operator deployment shape — paired with inline `RegisterHTTPTool` for the dev-loop. The manifest carries a top-level `auth:` map keyed on `auth_ref` (secrets MUST be `${ENV_VAR}` references — literal values are rejected at load time) and a `tools:` list each describing `{name, method, url_template, description?, args_schema?, out_schema?, headers?, body_template?, auth_ref?, side_effect?, tags?, loading?, policy?}`. Loaded at boot via `ToolsConfig.HTTPManifests`. AGENTS.md §7, RFC §6.4.

## V

**`ValidateMode`** — `both / in / out / none`. Per-node choice (`NodePolicy.Validate`) for whether the engine runs the validator on input, output, both, or skips it. `none` is the perf escape hatch for hot streaming paths. RFC §6.1, brief 01 §2.

**Virtual directory pattern** — pluggable-storage namespace addressing for skills (and potentially other artifacts). Logical paths over a swappable backing store. Inherited from the predecessor (the strongest pattern brief 04 names). RFC §6.7.

## W

**`WatchGroup`** — non-blocking dual of `RegisterRetainTurnWaiter` (Phase 21). Returns a channel that delivers a typed `GroupCompletion` payload when the group resolves; the planner runtime consumes the delivery as a wake-up signal so background-task results integrate back into the conversation without manual polling. Channel is buffered size 1; close-once invariant. Multiple subscribers on the same group all receive the same payload (D-025). Resolved-but-still-tracked groups return an already-primed channel so late subscribers don't deadlock. The mechanism for the three documented wake modes (push / poll / hybrid) — the planner picks the policy. RFC §6.8, brief 05.

**Hybrid wake (background continuation)** — wake mode where the main planner subscribes via `WatchGroup` (push) AND a sidecar (typically a small / cheap LLM, or a deterministic templater) polls the group's intermediate state and emits user-visible progress updates between push events. The main planner only wakes when the group resolves; the user sees liveness in the meantime. Suits user-facing agents where silence between turn close and group resolve looks broken. Phase 21 ships the mechanism; planner concretes (Phase 42+) wire the mode.
