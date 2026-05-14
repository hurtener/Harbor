# Harbor — Architectural decisions log

Append-only record of decisions that have been settled. **One entry per decision.** Reading this file is the fastest way to answer "wait, why did we pick X?" without re-litigating.

If a decision is later reversed or superseded, do NOT delete the original entry — append a new entry with `Supersedes: D-NN` and update the `Status` of the superseded entry to `Superseded by D-MM`.

The decisions here are mirrored in the RFC (which is the design source of truth). When they conflict, the RFC wins; file an entry here noting the discrepancy and resolve in the same PR.

---

## D-001 — Identity is the triple `(tenant, user, session)`

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §4, AGENTS.md §6
**Why:** The runtime must support concurrent sessions for the same user without context leakage. Tenant-only isolation is insufficient for multi-user agents. The triple is mandatory; there is no opt-out knob.

---

## D-002 — Console is a Protocol client; Runtime is headless

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §5, AGENTS.md §1, §4.5, §13
**Why:** The predecessor's Playground re-implemented runtime concepts (2,478 lines, 30+ HTTP routes, parallel state-store protocol). Decoupling unlocks remote attach, fleet view, third-party consoles, IDE/TUI clients, and prevents the "framework with a playground" trap. The Runtime never imports Console code.

---

## D-003 — Planner is swappable behind one interface

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §3.2, §6.2, AGENTS.md §1
**Why:** The biggest architectural lift over the predecessor. The runtime owns mechanism; planners own reasoning policy. ReAct is the V1 reference; Plan-Execute, Workflow, Graph, Deterministic, Supervisor, MultiAgent, HumanApproval can plug in over time without runtime changes.

---

## D-004 — Persistence triad shipped at V1: in-mem + SQLite + Postgres

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §9, AGENTS.md §9
**Why:** Three drivers from t=0 forces a clean abstraction. Designing against one tends to leak that backend's assumptions into the contract. The predecessor shipped contracts with no production backends; operators DIY-ed queueing. Harbor closes that gap.

---

## D-005 — Skills are a Harbor subsystem (not pushed entirely to Portico)

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.7, harbor_skills_subsystem memory
**Why:** The token-savvy DB-backed search/context/virtual-directory pattern is the predecessor's strongest subsystem; Harbor inherits it cleanly. Portico still owns distribution across tenants; Harbor consumes via a `SkillProvider` driver. Plus: Skills.md importer (closes the per-skill manual-adaptation gap) and an in-runtime skill generator with persistence (the predecessor's draft generator can't save).

---

## D-006 — Background-task persistence: in-process at V1, durable post-V1

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.8 (and §6.12 contracts), master plan post-V1 list
**Why:** V1 ships the contract. A durable backend (Postgres-as-queue or similar) lands post-V1 once the operational shape is clear.

---

## D-007 — A2A: full spec compliance from V1

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.4, master plan phase 29
**Why:** The predecessor ships full A2A spec compliance in code (the public docs lagged — that's the lesson Harbor's doc hygiene closes). Harbor inherits the surface verbatim from t=0; A2A peers appear as just-another-tool-source under the unified abstraction.

---

## D-008 — Sessions = longer-lived multi-turn conversations containing many Runs

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.9, glossary
**Why:** Resolves the predecessor's ambiguity between `StreamingSession` and `SessionManager`. Identity is `(tenant, user, session)`; `RunID` is per-execution; `TraceID` (OTel) may span Runs.

---

## D-009 — CLI dev-loop subcommand: `harbor dev`

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §8, master plan phase 64
**Why:** Boots local Runtime + Console + observability + hot reload + dynamic agent scaffolding with draft saving. Console is still a protocol client even on localhost; same code path as remote attach.

---

## D-010 — Code-level tool calling (LLM = decision-maker, not runner)

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.4 + §6.5, brief 07, harbor_design_principles memory
**Why:** The LLM emits text/JSON describing intent; the runtime parses, dispatches, and merges. Provider-native tool calling APIs are NOT used. Provider differences disappear; the runtime owns the protocol. The LLM client surface collapses to one method. The runtime trio (`ActionParser` / `Dispatcher` / `ObservationRenderer`) plus siblings (`RepairLoop`, `SchemaSanitizer`) are the design pieces. **Reversibility:** if community standard hardens around native tool calling later, a second `LLMClient` driver can be added — the runtime doesn't change.

---

## D-011 — Unified pause/resume primitive (HITL + OAuth + A2A AUTH_REQUIRED + steering PAUSE)

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §3.3 + §6.3, harbor_protocol memory
**Why:** Four seemingly-distinct features all converge on one runtime-level pause. The predecessor implements pause inside the planner loop, forcing every pause-shaped feature to reinvent coordination. Harbor's primitive lives at the runtime; planners and tools both signal "I need a pause" and the runtime drives the protocol-level event + resume token.

---

## D-012 — LLM client: `bifrost` (resolves Q-3); rejects CGo-required candidate

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.5, RFC §11 Q-3 RESOLVED, brief 08
**Why:** Original candidate (`liter-llm`) requires CGo bindings to a Rust core, conflicting with AGENTS.md §5/§13. Bifrost is pure Go (verified by direct source inspection: zero `import "C"`, zero `#cgo`, zero binary blobs), 23 first-class providers, empirically validated against six OpenRouter-routed models — 23/24 gating items pass. Bifrost's `Tools`/`ToolChoice` parameters are NOT used (see D-010).

---

## D-013 — Go 1.26+ minimum

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** AGENTS.md §5, RFC §10, .golangci.yml, .github/workflows/ci.yml, go.mod
**Why:** Bumped from 1.22 to match bifrost's `go.mod` floor. Go 1.26 is current; no downside to the bump.

---

## D-014 — License: Apache-2.0 (MIT was the considered alternate)

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §10, /LICENSE, README.md
**Why:** Patent grant matters for an SDK companies will build on; NOTICE-file mechanism makes attribution explicit; consistency with the infrastructure neighborhood (Go, Kubernetes, OTel, gRPC, bifrost). MIT remains a real alternate; flip is mechanical (no code changes).

---

## D-015 — Code-level tool calling justification recorded in RFC §6.4

**Date:** 2026-05-08
**Status:** Settled (acknowledged as a minority position)
**Where it lives:** RFC §6.4, glossary, this entry
**Why:** Maintainer explicitly questioned whether to switch to provider-native tool calling. Trade-off analysis confirmed code-level is the right call for Harbor's architecture: consistent with runtime/planner separation, swappable planner, cross-provider uniformity, single-method LLM client, custom opcodes (`task.subagent`, `parallel` with join spec), simpler streaming, and future-reversibility. Accuracy gap is closing as instruction-tuned models improve. Recorded so future re-reads understand it was a deliberate, examined choice.

---

## D-016 — Governance is a Harbor subsystem; middleware between Runtime and `LLMClient` driver

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.15, master plan phases 36a + 36b + 91–96, glossary
**Why:** Bifrost (the LLM-call substrate) doesn't know Harbor's identity triple. Identity-scoped policies (cost ceilings, rate limits, per-call MaxTokens, key rotation, model swap, failover, circuit breakers) live in a Harbor middleware layer that wraps the `LLMClient` interface. The `LLMClient` interface stays one method.

---

## D-017 — V1 Governance scope: cost ceilings + rate limits + MaxTokens; operator-driven runtime control is post-V1

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.15, master plan phases 36a + 36b (V1) and 91–96 (post-V1)
**Why:** A solo dev running production agents needs bankruptcy prevention from t=0 (cost accumulators + ceilings + rate limits). Live operator-driven controls (key rotation via Protocol, mid-session model swap, failover chains, circuit breakers, caching, PII redaction) require Console to land first; their phases sit explicitly in the post-V1 cluster (91–96) so they are tracked, not forgotten.

---

## D-018 — Failover is a Harbor policy, not bifrost per-request `Fallbacks`

**Date:** 2026-05-08
**Status:** Settled (post-V1 implementation, phase 93)
**Where it lives:** RFC §6.15, master plan phase 93
**Why:** Bifrost has a `Fallbacks []Fallback` field on each request — that's a per-call escape hatch with no audit awareness of Harbor's identity scopes. Harbor's failover is a policy with cost + rate-limit + audit implications; centralizing it in the Governance subsystem keeps every fallback hop a Harbor event with the identity triple attached.

---

## D-019 — Key rotation via `Account.GetKeysForProvider` per-request lookup, not bifrost `ReloadConfig`

**Date:** 2026-05-08
**Status:** Settled (post-V1 implementation, phase 91)
**Where it lives:** RFC §6.15, master plan phase 91
**Why:** `ReloadConfig` is whole-config replacement and races with in-flight requests. `Account.GetKeysForProvider(ctx, provider)` is invoked by bifrost on each request; Harbor's `Account` impl reads the live key set from a runtime-controlled atomic source. Console-pushed key rotations take effect on the next call with no config-swap race; old keys are invalidated immediately.

---

## D-020 — PII redaction at the LLM boundary lives in Audit; Governance owns thresholds

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.15, master plan phase 96
**Why:** Redaction is one canonical concern with multiple emit paths (logs, audit events, persisted state). Owning it in Audit gives one redactor; Governance owning it would split responsibility and risk inconsistent output. Governance owns thresholds (cost, rate, tokens) where the canonical concern is policy enforcement.

---

## D-021 — Multimodality scope: inputs in V1, outputs as post-V1 tool wrappers

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.5 (Multimodal inputs subsection), master plan phases 32 + 33 (V1 inputs) + 97 + 98 (post-V1 outputs), glossary (`ContentPart`, `ImagePart`, `AudioPart`, `FilePart`)
**Why:** The predecessor accumulated an ambient "text-only" assumption that became expensive to retrofit. Harbor settles multimodal inputs at V1 (image/audio/file via `ChatMessage.Content`'s `Parts` slice; `bifrost` handles per-provider translation) so the LLM call surface is correct from t=0 — sending images to LLMs as part of analysis is the common case, not a feature. **Outputs** (image generation, TTS, transcription, video) are delivered as **tools** that return `ArtifactRef`s; the planner dispatches them via the existing tool catalog (RFC §6.4 code-level dispatch). This keeps the `LLMClient` interface one method and aligns multimodal output with the runtime's existing tool-dispatch story.

---

## D-022 — `ArtifactRef` is the canonical binary representation for multimodal content

**Date:** 2026-05-08
**Status:** Settled
**Where it lives:** RFC §6.5 (canonical binary representation paragraph), §6.10 (Artifacts), glossary (`Artifact`, `ArtifactRef`, multimodal part types)
**Why:** Three supply forms exist for image/audio/file content (URL, DataURL, ArtifactRef). Above the heavy-output threshold (32 KB default — RFC §6.10), the runtime *automatically* materializes inline `DataURL` content into `ArtifactRef`s and rewrites the message before event emission, audit, and persistence. This keeps event payloads, audit logs, and memory turns from carrying raw bytes; it also gives audit redaction a stable canonical form to handle (`ArtifactRef` passes through unredacted; `DataURL` is rewritten to placeholder + ref). URLs pass through unchanged when the provider can fetch them directly.

---

## D-023 — Flow-as-Tool: Go-coded `flow.Definition` ships V1; declarative recipe (YAML) format ships V1.1

**Date:** 2026-05-09
**Status:** Settled
**Where it lives:** RFC §6.1 (Flow-as-Tool subsection) + §6.4 (Flow transport variant), master plan phase 26a (V1) + phase 100 (post-V1 recipe loader), glossary (`Flow`, `Definition`, `Budget` for flows, `Recipe`)
**Why:** A Flow is a typed DAG of `Node`s assembled into a runnable unit and registered as a Tool the planner can call. This composes (a) the existing subflow + reliability shell (`NodePolicy` retry / exponential backoff / timeout) from §6.1, (b) the unified tool dispatch path from §6.4, and (c) the identity-tier Governance ceilings from §6.15, without adding a parallel orchestration concept. **V1 ships the Go-coded `Definition` shape** so the contract is settled and operators can ship flows in code; **recipes (declarative YAML loaders into the same `Definition` struct) ship V1.1** to keep V1 scope tight without losing the surface. Per-flow `Budget` composes with run-level + identity-level budgets via `min()`: any layer can abort the flow, whichever cap fires first.

---

## D-024 — `ToolPolicy` reliability shell wraps every tool invocation, regardless of transport

**Date:** 2026-05-09
**Status:** Settled
**Where it lives:** RFC §6.4 (`Tool.Policy` field + reliability-shell paragraph), master plan phase 26 (acceptance criteria), glossary (`ToolPolicy`)
**Why:** A predecessor pattern worth preserving: even the minimum-expression tool — a plain Go function decorated as a tool — got per-call timeout / retry-with-backoff / validation for free. Harbor settles this at the catalog level: `Tool.Policy` is a `ToolPolicy` mirroring `NodePolicy` (§6.1). The Dispatcher trio (§6.4) wraps every tool invocation in the shell once; `Transport` (InProcess / HTTP / MCP / A2A / Flow) does not change the resilience guarantees. Defaults fire when `ToolPolicy` is zero-valued, so `tools.RegisterFunc(name, fn)` is production-resilient with no ceremony. Operators who want non-default policy pass `tools.WithPolicy(...)`. Same backoff math + retry classes as `NodePolicy` so the surface is one mental model.

---

## D-025 — Concurrent reuse contract: compiled artifacts immutable; per-run state lives in `ctx` + `RunContext`

**Date:** 2026-05-09
**Status:** Settled
**Where it lives:** RFC §3.5 ("The concurrent reuse contract"), AGENTS.md §5 ("Concurrent reuse contract — non-negotiable"), §11 (mandatory concurrent-reuse tests), §13 (forbidden: mutable state on compiled artifacts), `docs/plans/_template.md` (pre-merge checklist), every Wave 1+ phase plan.
**Why:** The predecessor's most expensive retrofit was thread-safety on its first-version flow runtime — the singleton "build a flow once, reuse across runs" pattern had mutable state that bled across concurrent invocations once parallelism was enabled. Harbor closes this from t=0 by settling four guarantees for every compiled artifact (`flow.Engine`, `Tool`, `Planner`, `MemoryStore`, `Redactor`, `LLMClient`, `ToolCatalog`): no data races, no context bleed, no cancellation cross-talk, no goroutine leaks. **Every phase that builds a reusable artifact ships a concurrent-reuse test** (N≥100 invocations under `-race`); the test is part of the pre-merge checklist and the drift-audit-adjacent phase plan template. Mutable state on artifacts that crosses run boundaries is a forbidden practice (AGENTS.md §13). Per-run state lives in `ctx` + `RunContext`; this constraint shapes every interface signature in the runtime.

---

## D-026 — Context-window safety net: no raw heavy content reaches the LLM; standard `ArtifactStub` everywhere

**Date:** 2026-05-09
**Status:** Settled
**Where it lives:** RFC §6.5 ("Context-window safety net" subsection + standard `ArtifactStub` schema), RFC §6.10 (heavy-output threshold), AGENTS.md §13 (forbidden: raw heavy content in LLM messages), master plan phase 32 (LLM client core enforces the catch-all pass), glossary (`Context-window safety net`, `ArtifactStub`, `ErrContextLeak`, `ErrContextWindowExceeded`).
**Why:** The predecessor learned the hard way that LLM context windows balloon when artifacts (images, PDFs, large tool outputs, memory turns) are not consistently offloaded — the safety net was retrofitted later. Harbor settles the pattern as a runtime-wide invariant from t=0: **no message reaching the `LLMClient` carries raw heavy content.** Multi-stage enforcement: (1) producers (tool dispatcher, memory subsystem, multimodal input materialization, `ObservationRenderer`) substitute heavy content with `ArtifactRef`s as part of their normal output; (2) a single catch-all pass at the LLM-client edge walks the assembled `CompleteRequest` and fails loudly with `ErrContextLeak` if any ≥-threshold raw payload survived, plus fails with `ErrContextWindowExceeded` if the estimated token count is within the configured `ContextWindowReserve` (default 5%) of the model's context limit. **V1 does not auto-truncate** when the budget guard fires — the planner receives a typed error and is responsible for recovery (drop older turns, summarize, etc.); auto-cascade is post-V1 work. The standard `ArtifactStub` schema (`{artifact_ref, mime, size_bytes, hash, summary, fetch}`) is the *only* thing the LLM sees in place of heavy content; format is uniform across all producers and providers — no per-model swapping.

---

## D-028 — Event bus surface reconciliation: `identity.Quadruple` field, `EventBus` name, replay deferred to Phase 06, sealed-via-embedded-Sealed payload pattern, SafePayload bypass

**Date:** 2026-05-09
**Status:** Settled (supersedes the earlier RFC §6.13 sketch)
**Where it lives:** RFC §6.13 (revised; earlier sketch retained as "kept for history"), `docs/plans/phase-05-events.md` ("Findings I'm departing from"), `internal/events/events.go` (the shipped surface), `internal/events/payloads.go` (bus-internal SafePayload types).
**Why:** The earlier RFC §6.13 sketch carried flat identity strings (`TenantID, UserID, SessionID, RunID`), an `EmittedAt` time, optional metric-shaped fields (`LatencyMs *float64`, `TokensIn/Out *uint32`, `CostUSD *float64`, `QueueDepth`), called the bus interface `Bus`, and ranged it over a `Replay(ctx, Cursor, Filter)` method. The shipped Phase 05 surface diverged in five load-bearing ways: (1) **identity reuse** — `Identity identity.Quadruple` re-uses Phase 01's type so a single concept lives in one place rather than four scattered string fields; (2) **renamed to `EventBus`** so the symbol doesn't collide with generic Go vocabulary at call sites; (3) **`Replay` deferred** to Phase 06 (the in-memory ring-buffer driver) and exposed through a future capability interface, keeping the core EventBus surface to three methods; (4) **no inline metric fields** — Phase 56 will derive metric labels from `Event.Extra` (a bounded `map[string]string`) so the cardinality boundary is explicit; (5) **sealed-via-embedded-`Sealed` payload pattern** plus the `SafePayload` marker (composing `SafeSealed`) — bus-internal payloads bypass the audit redactor (preserving typed access on the subscriber side), external payloads default to redactor-walked `RedactedMap`. The `OccurredAt` rename (from `EmittedAt`) keeps the field's verb consistent with the new "emit"/"publish" terminology in the bus implementation. Phase 05 plan acknowledged none of these in its "Findings I'm departing from" section; this entry closes that drift retrospectively. The earlier sketch is preserved verbatim in §6.13's "kept for history" paragraph.

---

## D-027 — StateStore is a generic `(Quadruple, Kind, Bytes)` surface; typed wrappers land at consumer phases

**Date:** 2026-05-09
**Status:** Settled (supersedes RFC §6.11's typed-multi-method sketch)
**Where it lives:** RFC §6.11 (revised to the generic surface), `docs/plans/phase-07-state.md` ("Findings I'm departing from"), `internal/state/` (will land in Phase 07 implementation), every consuming phase that wraps the surface (08 sessions, 20 tasks, 22 distributed, 23 memory, 42 planner, 50 steering — each ships its own typed adapter atop the generic interface).
**Why:** The earlier RFC §6.11 sketch listed 21 typed methods (`SaveTask`, `SaveTrajectory`, `SaveBinding`, `SaveSteering`, `SaveMemoryState`, …) keyed on Go types (`Task`, `Trajectory`, `RemoteAgentBinding`, `SteeringEvent`, `MemoryKey`, …) that **do not exist yet** — they belong to phases not in Wave 2 (sessions Phase 08, tasks Phase 20, distributed Phase 22, memory Phase 23, planner Phase 42, steering Phase 50). A leaf persistence interface cannot import types from its consumers without inverting the dependency graph. Harbor settles the call: `StateStore` is a five-method surface keyed on `(identity.Quadruple, Kind string, Bytes []byte)` with idempotency on a caller-provided `EventID` (ULID). Consuming phases land their typed wrapper at their own layer (`SessionRegistry.Save(s Session)` reduces to `StateStore.Save(StateRecord{Identity: s.Identity, Kind: "session.lifecycle", Bytes: marshal(s)})`). Strictly more general than the typed surface, fully covered by the conformance suite, and avoids the leaf-imports-consumer cycle. Forward-only migrations, three-driver parity (in-mem / SQLite / Postgres), and the no-`Supports*`-ceremony rule from §9 still apply unchanged. The earlier sketch is not deleted from history (it captured intent); this entry supersedes it.

---

## D-029 — Replay returns `[]Event`, not a fresh `Subscription`

**Date:** 2026-05-09
**Status:** Settled (supersedes brief 06 §2 sketch for Phase 06)
**Where it lives:** `docs/plans/phase-06-events-replay.md` ("Findings I'm departing from"), brief 06 §2 (the original sketch is preserved unchanged in the brief), `internal/events/events.go` (the `Replayer` interface lands in Phase 06's implementation PR), glossary (`Replayer capability interface`, `Cursor`).
**Why:** Brief 06 §2 sketched `Replay(ctx, Cursor, Filter) (Subscription, error)` returning a fresh Subscription whose stream interleaves historical-then-live events. That coupling makes the historical/live boundary fuzzy and forces the bus to dedupe at the seam between snapshot and live tail — exactly the kind of "subtle invariant maintained by clever code" pattern the predecessor learned to regret. Harbor settles the surface as `Replay(ctx, Cursor, Filter) ([]Event, error)`: a snapshot of historical events strictly between the cursor and the bus's current sequence, with the caller responsible for combining the snapshot with a fresh `Subscribe` if it wants to continue live. The split gives the no-duplicate / no-gap guarantee a clean home — `Publish` stamps every event with `Sequence`, and a subscriber's cursor is "the last sequence I have." If a future phase needs a one-shot `ReplayAndSubscribe`, it composes on top of these two primitives without changing driver implementations. The brief sketch is preserved unchanged in `docs/research/06-events-observability-devx.md` §2; this entry records the implementation departure.

---

## D-030 — TaskRegistry surface split: per-task in Phase 20, groups + retain-turn + WatchGroup in Phase 21

**Date:** 2026-05-10
**Status:** Settled
**Where it lives:** `docs/plans/phase-20-tasks.md` ("Findings I'm departing from"), `docs/plans/phase-21-tasks-groups.md` (the follow-up surface), `internal/tasks/tasks.go` (the shipped Phase 20 surface), brief 05 §7 (the original sketch recommending one phase for the full surface).
**Why:** Brief 05 §7 phase decomposition recommended one phase for the full `TaskRegistry` (per-task surface + groups + retain-turn + patches + ack-background). Harbor splits this across Phase 20 (per-task surface) and Phase 21 (groups + retain-turn + WatchGroup + patches). Per-task lifecycle is independently shippable and has zero dependencies on group governance; bundling the whole TaskService into one phase would slow the wave-end E2E and delay the per-task surface that downstream phases (steering Phase 53, planner Phase 42) want as a stable foundation. The split keeps Phase 20's `TaskRegistry` interface narrow (Spawn / SpawnTool / Get / List / Cancel / Prioritize / Mark*) while Phase 21's PR extends the same interface with group + retain-turn methods against a stable per-task subset. Brief 05's recommendation is preserved verbatim in `docs/research/05-state-tasks-artifacts-sessions.md` §7; this entry records the implementation departure.

---

## D-031 — Distributed contracts: full A2A v1 surface mapping + loopback V1 driver; vendored proto pinned by commit SHA

**Date:** 2026-05-10
**Status:** Settled
**Where it lives:** RFC §6.4 + §6.12, `docs/plans/phase-22-distributed.md`, `docs/specifications/a2a.proto` (vendored at commit `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`, 2026-04-23), `docs/specifications/README.md`, `internal/distributed/` (the shipped Phase 22 surface), this entry.
**Why:** D-007 settled "A2A full spec compliance from V1." Phase 22 realises that commitment by hand-transcribing the entire A2A v1 surface into Go: every `A2AService` RPC maps 1:1 to a `RemoteTransport` method, every proto `message` has a Go counterpart in `internal/distributed/a2a/types.go`, every `oneof` variant (`Part`, `SecurityScheme`, `OAuthFlows`, `StreamResponse`, `SendMessageResponse`) is represented as a Go interface + concrete-type-per-variant discriminated union with a `Kind() string` discriminator. The `TaskState` 8-value enum, the `Role` 3-value enum, and every nested message (`AgentCard`, `AgentInterface`, `AgentSkill`, `AgentCardSignature`, `TaskPushNotificationConfig`, `AuthenticationInfo`, the five `SecurityScheme` concretes, the five OAuth flow concretes including the two deprecated ones for spec parity, every request/response envelope) ship as named Go types. Phase 29's southbound A2A driver inherits the surface without churn. The proto is vendored at a pinned commit SHA so the source-of-truth is searchable from inside the repo; bumps land as `deps(specs):` PRs. The Go shapes are hand-written (not `protoc`-generated) because: (a) Phase 22 must not pull `google.golang.org/grpc` / `google.golang.org/protobuf` into a contracts-only package — Phase 29 owns that decision; (b) the hand-written shapes integrate cleanly with `identity.Quadruple`, slog logging, and Harbor's error idioms; (c) the `types_test.go` coverage gate (a hand-maintained list of 50 expected type names with a count assertion) makes the transcription auditable. The V1 driver is `loopback` — in-process dispatch routed through an in-memory `Agent` interface (in `internal/distributed/drivers/loopback/agent.go`) so the conformance suite can simulate every A2A RPC without leaving the process. The conformance suite IS the gate: future drivers (durable bus at phase 86, A2A wire at phase 29) inherit it verbatim.

---

## D-032 — Wake-on-resolution is a planner-concrete responsibility; TaskRegistry stays neutral

**Date:** 2026-05-10
**Status:** Settled
**Where it lives:** `docs/plans/phase-21-task-groups.md` (the `WatchGroup` surface + the three wake-mode names documented at `internal/tasks/groups.go` package godoc), `docs/plans/README.md` Phase 42 / 45 / 48 / 49 detail blocks (the consumption contract), `internal/tasks/tasks.go` (the neutral `WatchGroup(sessionID identity.Identity, groupID TaskGroupID) (<-chan GroupCompletion, func(), error)` surface — no `Mode` enum baked in), planner phase plans when authored.
**Why:** Phase 21 closed the predecessor's silent gap where non-retain-turn `SpawnTask` groups left the planner with no signal that all members had resolved. The fix is `WatchGroup` + `GroupCompletion` — a non-blocking notification channel the planner subscribes to. But the *policy* of how a planner reacts to that channel (wake the LLM eagerly, poll on its next deterministic iteration, or hybrid push + sidecar status emitter) is a planner-shape concern, not a TaskRegistry concern. Burning a `WakeMode` enum into the registry would either force every planner concrete onto the same policy or introduce a `Supports*` capability protocol — both anti-patterns under AGENTS.md §4.4. So the registry stays neutral and the three wake modes (`push` / `poll` / `hybrid`) are documented at the `internal/tasks` package godoc with the same vocabulary the planner phases consume. Each concrete planner (Phase 42+) MUST implement at least one of the three modes for non-retain-turn group continuation; the planner conformance pack (Phase 49) MUST exercise the round-trip (SpawnTask → group completes → planner re-enters → reads `MemberOutcome`). The retain-turn flow (turn-bound parallel) keeps its existing `RegisterRetainTurnWaiter` path — `WatchGroup` is strictly the non-retain-turn dual. Naming the wake modes in one canonical place keeps third-party planner authors aligned and makes the conformance assertion testable.

---

## D-033 — Memory subsystem: identity-rejection emits `memory.identity_rejected` on the bus with `"<missing>"` substitution for the partial-triple identity field

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.6, `docs/plans/phase-23-memory.md` ("Brief findings incorporated" + "Risks / open questions"), `internal/memory/events.go` (the event-type constant + registration + `MemoryIdentityRejectedPayload`), `internal/memory/reject.go` (`EmitIdentityRejected` + `identityRejectionReason`), brief 04 §4.2 + §6.
**Why:** Brief 04 §4.2 settles that a `MemoryStore` operation with a missing identity component MUST (a) fail closed with `ErrIdentityRequired` and (b) emit an audit event so the rejection is observable on the event bus. The brief does not name the event type. Harbor settles it as `memory.identity_rejected`, registered in the canonical `events` registry via this phase's `init()`. The payload (`MemoryIdentityRejectedPayload`) is `SafePayload` by construction — `Operation` is a bounded enumerable method name, `Reason` is a static string naming the missing component(s); no caller-controlled bytes survive on the payload. The naming and the SafePayload classification are mine, recorded so a later phase auditor doesn't flag either as drift. The event's `Identity` field is also load-bearing: Phase 05's `ValidateEvent` rejects empty-triple events with `ErrIdentityRequired`, so the rejection event itself cannot be `Identity = identity.Quadruple{}` even though the rejected input was. The settled solution: substitute any empty component with a `"<missing>"` sentinel on the published event so `ValidateEvent` passes; the payload's `Reason` field names the truly missing component(s), and subscribers MAY `Admin: true`-filter to fan-in cross-tenant rejections. The memory record persistence key is also settled at this phase: `Kind = "memory.state"` for the typed-wrapper-over-`StateStore` write (D-027 pattern), per-`Quadruple` slot, with the persisted bytes shaped as `{strategy, turns}` JSON. Phase 23 only writes empty records (Strategy=none has no mutations); Phase 24 will append turn data; Phase 25's persistent drivers will inherit the shape unchanged.

---

## D-034 — Persistent memory drivers own their `memory_state` tables; `Deps.State` accepted-but-unused; wire envelope `memory.Record` exported for cross-driver byte-stable Snapshot/Restore

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.6, RFC §9, `docs/plans/phase-25-memory-drivers.md` ("Findings I'm departing from" + "Risks / open questions"), `internal/memory/wire.go` (`Record` + `KindMemoryState`), `internal/memory/drivers/sqlite/{sqlite.go,migrations/0001_init.sql}`, `internal/memory/drivers/postgres/{postgres.go,migrations/0001_init.sql}`.
**Why:** Phase 23's InMem MemoryStore persists records through the injected `state.StateStore` per D-027 (typed-wrapper-over-generic, `Kind="memory.state"`). The Phase 25 persistent drivers (SQLite + Postgres) instead maintain their own `memory_state` table — this is a deliberate departure from D-027's "one StateStore, many typed wrappers" model and is mandated by the master plan ("Your SQLite/PG drivers persist memory state to their OWN tables ... but the byte serialisation contract is the same shape so cross-driver Snapshot/Restore round-trips byte-stable"). Two consequences are now settled: (1) the `memory.Deps.State` field is accepted by the persistent drivers but unused — the existing `validateDeps` contract still requires non-nil to preserve backward compatibility with the InMem driver (which DOES use `State`), so the persistent drivers hold the reference without writing to it; (2) the wire envelope previously named `memoryStateRecord` inside the InMem driver is promoted to an exported `memory.Record` type at `internal/memory/wire.go` (with the canonical `KindMemoryState` routing constant alongside it) so all three drivers marshal byte-identical JSON, enabling the Phase 25 acceptance criterion that a Snapshot taken by one driver Restore-round-trips byte-stably through another. Each persistent subsystem's Postgres migration runner uses a distinct `pg_advisory_lock` key (`fnv64aSigned("harbor-memory-migrations")`) so the state + memory migration runners cannot collide.

---

## D-035 — Memory strategies: `OverflowDropOldest` is the only `OverflowPolicy`; recovery loop is bounded by `RecoveryBacklogMax` with drop-oldest + `memory.recovery_dropped` emit; retry/backoff/cadence are constants, not config

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.6, `docs/plans/phase-24-memory-strategies.md` ("Findings I'm departing from" + "Risks / open questions"), `internal/memory/memory.go` (`OverflowPolicy` enum + `OverflowDropOldest` constant + `ValidateHealthTransition` + `ErrInvalidHealthTransition` + transition table), `internal/memory/events.go` (`EventTypeMemoryHealthChanged` + `EventTypeMemoryRecoveryDropped` + `HealthChangedPayload` + `RecoveryDroppedPayload`), `internal/memory/health.go` (`EmitHealthChanged` + `EmitRecoveryDropped`), `internal/memory/strategy/rolling_summary.go` (the constants `defaultRetryAttempts = 3`, `defaultRetryBackoffBase = 100*time.Millisecond`, `defaultDegradedRetryEvery = 10*time.Second`; the bounded recovery loop), brief 04 §2 + §4.1.
**Why:** Three narrow scope calls at this phase, all driven by AGENTS.md §13's "no silent degradation" rule + the "fail loudly" principle:

1. **`OverflowPolicy` narrows from brief 04 §2's three-option enum to a single `OverflowDropOldest`.** Brief 04 §2 names `truncate_oldest`, `truncate_summary`, and `error`. Harbor ships only `OverflowDropOldest`. Rationale: (a) `truncate_summary` requires the summariser inside the truncation path which conflates two strategies; (b) `error` is a silent-degradation footgun — an over-budget AddTurn returning `ErrBudgetExceeded` would force every caller to handle the error or silently lose turns, which is exactly the pattern AGENTS.md §13 closes. The narrow enum lets the surface grow if a real LLM-client integration (Phase 32+) surfaces a use case for `truncate_summary` ("always keep a summary line, drop oldest verbatim turns first"); today the simpler shape avoids the footgun.
2. **Recovery loop is bounded by `RecoveryBacklogMax` with drop-oldest + `memory.recovery_dropped` emit on overflow.** Brief 04 §4.1 names the bound; the drop-oldest action + the recovery-dropped event are mine, recorded here so a later auditor doesn't flag the naming or the SafePayload classification as drift. The payload is `SafePayload` by construction — only a bounded `Reason` string survives ("backlog_overflow" today). Default `RecoveryBacklogMax = 16` sized to absorb a short summariser outage (≈4 minutes at `defaultDegradedRetryEvery = 10s` × 16 retries) without unbounded memory growth.
3. **Retry / backoff / cadence knobs from brief 04 §2 (`RetryAttempts`, `RetryBackoffBase`, `DegradedRetryEvery`) do NOT land in `config.MemoryConfig`.** Only `RecoveryBacklogMax` is operator-tunable. The three constants live in `internal/memory/strategy/rolling_summary.go` as package constants (`defaultRetryAttempts = 3`, `defaultRetryBackoffBase = 100*time.Millisecond`, `defaultDegradedRetryEvery = 10*time.Second`). Rationale: nobody has needed to tune them yet, exposing knobs no one has a calibrated answer for is fighting yaml; if the LLM-client integration (Phase 32+) surfaces real-world miscalibration we re-litigate via an RFC PR + a new `MemoryConfig` field. Keeping the surface narrow today avoids version-skew between an operator's `harbor.yaml` and a future Harbor that retunes the defaults internally.

The `Health` FSM transition table is also settled at this phase: `healthy ↔ retry ↔ degraded ↔ recovering` with the explicit edges listed in `internal/memory/memory.go`'s `healthTransitions` map. Self-loops are valid; any other pair is rejected by `ValidateHealthTransition` with `ErrInvalidHealthTransition` (fail-loud — an invalid transition is a programming error in the calling executor, not a recoverable state). The full matrix is property-tested in `internal/memory/strategy/strategy_test.go::TestValidateHealthTransition_Matrix`. The `Health` FSM's observable degradation path (`memory.health_changed` emit on transition) is the explicit, documented exception to AGENTS.md §13's "no silent degradation" rule — degraded mode IS the observable failure surface, and emitting the event makes it observable (and therefore not silent).

---

## D-036 — HTTP tool driver: URL/body/header templates use `text/template` + explicit `urlquery`; secrets live in `Auth` only

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** `docs/plans/phase-27-tools-http.md` ("Findings I'm departing from"), `internal/tools/drivers/http/http.go` (the `checkNoSecretLeak` guard + `compileTemplate` with `missingkey=error` + `urlquery` funcmap), `internal/tools/drivers/http/manifest.go` (the loader's pre-compile leak check + the `${ENV_VAR}`-only secret form), `docs/glossary.md` (`AuthSpec` + `UTCP manifest` + `RegisterHTTPTool`), AGENTS.md §7 (credential boundary rule this implements).
**Why:** Brief 03 §3 sketched HTTP tool registration with "url-template substitution from args" but did not specify the credential boundary. Without a constraint, the simplest implementation lets operators interpolate `${API_KEY}` or `{{ .Auth.token }}` directly into the URL — which means the secret crosses the audit redactor, lives in observability logs, and rides through any caching layer. Harbor's tools-HTTP driver tightens this from t=0: URL / body / header templates are `text/template` strings whose only namespace is `.Args.*`; the loader runs a regex check (`{{[\s-]*\.Auth\b`) against every template at register / load time and rejects matches with `ErrTemplateSecretLeak`. Secrets enter the driver only via the `Auth` map (operator-supplied), and the manifest loader requires the `${ENV_VAR}` reference form — literal secret strings are also rejected at load time. Combined: a leaked secret in an HTTP tool config is a register-time error, never a runtime data leak. Templates use `missingkey=error` so `{{ .Args.unknown }}` fails loudly rather than silently rendering as empty (consistent with the runtime's "fail loudly" rule, AGENTS.md §5). The `urlquery` funcmap alias is documented in package godoc so operators write `{{ .Args.city | urlquery }}` explicitly when the substituted value must be URL-escaped; the default rendering does NOT auto-escape (Go's `text/template` is byte-faithful), so this is the operator's responsibility for now — a future enhancement could auto-escape every substitution if the asymmetry proves error-prone.

---

## D-037 — MCP southbound driver wraps `github.com/modelcontextprotocol/go-sdk@v1.6.0`; transport-reconnect lives in `ToolPolicy`, not in a parallel state machine

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.4, `docs/plans/phase-28-tools-mcp.md` ("Findings I'm departing from" + "Risks / open questions"), `internal/tools/drivers/mcp/` (the shipped driver), `internal/tools/drivers/mcp/auto.go` (the `MCPTransportMode` selector + auto-fallback at Provider.Connect, not at Transport.Connect), brief 03 §4 (the "reconnect-on-failure" brief recommendation), this entry.
**Why:** Brief 03 §4 named "reconnect-on-failure" as a Phase 28 requirement. The Go MCP SDK's `StreamableClientTransport` already ships an internal exponential-backoff reconnect loop for the standalone SSE stream; stdio + SSE transports leave session-level failures to the caller. Harbor handles those failures at the `ToolPolicy` retry shell (D-024) — the descriptor's `Invoke` closure re-runs `callTool` which re-reads `sessionForRead`, so a `ToolPolicy` retry transparently uses a reconnected session when the operator runs `Provider.Connect` again. Implementing a parallel reconnect state machine inside the driver would have shipped "two parallel implementations of the same conceptual feature" (AGENTS.md §13 forbidden practice) — one in `ToolPolicy`, one in the driver — and required new sentinels + new audit events to make the per-driver reconnect observable. The settled design keeps reliability at the catalog edge and the driver thin. SDK version `v1.6.0` is pinned (its Go floor 1.25 ≤ Harbor floor 1.26); bumps are routine deps PRs with conformance suite re-run. Auto-mode fallback (streamable-HTTP → SSE) was lifted from Transport.Connect to Provider.Connect so the SDK's `client.Connect` initialize-handshake failures are also covered — a Transport.Connect-only fallback would miss "endpoint answers HTTP but isn't really streamable".

---

## D-038 — A2A southbound driver: JSON-RPC binding, route-scoring weights settled, push-config storage forwarded to peer (no local mirror)

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.4, master plan phase 29, `docs/plans/phase-29-tools-a2a.md`, `internal/distributed/drivers/a2a/registry.go`, `internal/distributed/drivers/a2a/a2a.go` package godoc, `internal/tools/drivers/a2a/a2a.go`, glossary (`A2A peer`, `Agent Card cache`, `Route scoring`).
**Why:** Phase 29 lands the first wire-level A2A driver. Three design calls warrant a settled entry so a later auditor doesn't churn them. (1) **Wire binding.** The vendored proto carries both `service A2AService { rpc … }` (gRPC stubs) AND `google.api.http` annotations (HTTP+JSON binding). Phase 29 implements the **JSON-RPC 2.0 over HTTPS** binding per the master-plan Phase 29 detail block and brief 03 §5; gRPC + HTTP+JSON bindings on the same peer's AgentCard are accepted as read-only metadata until those drivers ship. The driver matches `AgentInterface.ProtocolBinding == "JSONRPC"` (the Phase 22 constant `a2a.ProtocolBindingJSONRPC`); peers declaring no JSONRPC interface fail loudly with `ErrNoJSONRPCInterface`. (2) **Route-scoring weights.** The Registry's `CompositeScore = (5 × TrustTier) + (1000 / max(1, LatencyTierMS)) + (10 × CapabilityScore)`. Trust outranks latency 5:1 (safety first); latency is the tie-breaker among similarly-trusted peers (the `1000/lat_ms` term saturates at the LatencyWeight when latency is 1ms, drops to 1.0 at 1000ms); capability match adds an additive boost so a peer that declares the exact `AgentSkill.ID` outranks a tag-match. Lower latency + lexicographic URL break composite ties so the result is deterministic. Weights are tunable post-V1 but not exposed at V1 (a single deployment uses one canonical scoring policy). (3) **Push-notification config storage.** The master-plan detail block specifies "store push-notification configs in-memory at V1." Phase 29's southbound driver IS the *client* (issuing `Create/Get/List/Delete` against the *peer*); the peer is responsible for durability. The wire driver forwards CRUD verbatim and stores nothing locally. A multi-replica Harbor consequently sees per-peer push-config state — acceptable for V1; durable mirroring is a Phase 23 (memory) / Phase 15 (SQLite state) / Phase 16 (Postgres state) compose post-V1. HTTPS-only is enforced for non-loopback peers (AGENTS.md §7); HTTP is allowed for `127.0.0.1`, `::1`, `localhost`, and operator-allowlisted loopback shapes only. The conformance suite (`internal/distributed/conformancetest.RunRemoteTransport`) is the gate — passes verbatim against the wire driver bound to an `httptest.Server`-shaped mock A2A peer.

---

## D-039 — LLM-edge safety pass: mandatory-by-construction, ordering = materialize → leak-detect → token-budget; safety wrapper is the registry's only handout

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.5 ("Context-window safety net" subsection), AGENTS.md §13 (forbidden: raw heavy content reaches LLMClient), master plan phase 32 (acceptance criteria), `docs/plans/phase-32-llm-client.md`, `internal/llm/safety.go` (`safetyClient`), `internal/llm/registry.go` (`Open` returns the wrapper, not the raw `Driver`), glossary (`Context-window safety net`).
**Why:** D-026 settled the **what** ("no message reaching the `LLMClient` carries raw heavy content; fail loudly via `ErrContextLeak` / `ErrContextWindowExceeded`"); Phase 32 settles the **how**. Three design calls warrant a settled entry so a later auditor doesn't churn them. (1) **Mandatory-by-construction.** `internal/llm.Open(...)` returns an `LLMClient` interface whose only concrete implementation is the package-private `*safetyClient`. The factory builds a `Driver` (the unexported-by-naming surface) and wraps it. A caller cannot bypass the safety pass through the registry; a caller who genuinely needs a bare `Driver` (an evaluation harness that has already run the pass) constructs the wrapper directly in its own package — but the production code path is the registry. This is the AGENTS.md §13 "fail-loudly + capability mandatory" pattern applied to the safety net: the runtime fails closed, not "fails open with a feature-flag." (2) **Pass ordering.** Inside `safetyClient.Complete`, the steps are: **identity → structural-validate → materialize → leak-detect → token-budget → driver**. Materialize runs BEFORE leak-detect so a producer that ships a oversize `DataURL` gets one more chance to be rewritten; a producer that ships raw bytes in a `Text` field (not a `DataURL`) is caught by leak-detect. The token-budget guard runs LAST so it sees the post-materialize byte count (an ArtifactStub-rewritten message is small; estimation reflects what the driver will actually send). Cancellation is honoured between every step via `ctx.Err()`. (3) **No auto-cascade at V1.** The token-budget guard fails loudly with `ErrContextWindowExceeded`; V1 does NOT truncate or summarize automatically. The planner is responsible for recovery (drop older turns, summarize, etc.). Auto-cascade is post-V1 work — an extension of memory's `rolling_summary` plus a `PromptAssembler` orchestrator; tracked but not on V1's floor. The acceptance bar of "fails loudly = observable" is settled at the bus emit (`llm.context_window_exceeded`); operators quantify how often the guard fires and tune `ContextWindowReserve` accordingly.

---

## D-040 — bifrost driver design: single-provider per Harbor instance; `env.NAME` API-key resolution at New time (fail-closed on missing); stream cancellation abandons the chunk reader; cost emit lives in the driver

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.5, RFC §11 Q-3 (RESOLVED), `docs/plans/phase-33-bifrost.md`, `internal/llm/drivers/bifrost/bifrost.go` (Driver + Complete + streamComplete), `internal/llm/drivers/bifrost/account.go` (Account + `resolveAPIKey`), `internal/llm/drivers/bifrost/cost.go` (emit helper), glossary (`BifrostDriver`, `BifrostContext`, `ProviderRouting`), brief 08.
**Why:** Brief 08 settled the **adoption** ("bifrost is the V1 LLM driver"); Phase 33 settles the **adapter shape**. Four design calls warrant a settled entry so a later auditor doesn't churn them. (1) **Single-provider per Harbor instance.** `LLMConfig` (Phase 32) ships `Provider` / `Model` / `APIKey` / `BaseURL` / `Timeout` as singular fields; Phase 33's `Account` advertises exactly one configured provider. The operator's `harbor.yaml` carries the bifrost-side `Provider` (e.g. `openrouter`); the per-model `ModelProfiles` keys carry the upstream identifier (`openai/gpt-5.3-chat`). Multi-provider routing per Harbor instance is post-V1; deployments needing multiple endpoints run multiple Harbor instances. (2) **API-key resolution at New, not at Complete.** `Account.resolveAPIKey` reads `cfg.APIKey` once at construction. The literal `"sk-..."` form is the value; the `"env.NAME"` form looks up `os.Getenv(NAME)` and fails closed with `ErrMissingAPIKey` (naming the env var) if unset. Fail-at-boot is the runtime principle (AGENTS.md §5); a runtime that boots clean and fails the first user request because of a missing key is the silent-degradation footgun §13 closes. The key value is NEVER logged, surfaced in errors, or emitted on the bus. (3) **Stream cancellation abandons the chunk reader.** Brief 08 §"Cancellation caveat" observed that bifrost's chunk channel can take a few seconds to close on some providers after `ctx` cancel. The driver's `streamComplete` does a `select` on `<-ctx.Done()` and the chunk channel; on ctx-cancel the driver returns `ctx.Err()` IMMEDIATELY and never waits for the channel close. Bifrost's worker goroutine continues draining upstream on its own; the goroutine-leak test asserts baseline restoration. (4) **Cost emit lives in the driver, not the safety client.** The Phase 32 safety client is provider-blind; the driver knows the request's model and observes bifrost's `BifrostCost` shape. `cost.go::emitCostRecorded` publishes `llm.cost.recorded` after a successful Complete with the full identity quadruple + model + cost + usage; Phase 36a's governance accumulator subscribes against this emit site. If a future phase ships a second non-mock LLM driver and wants cost emission to fold into the safety client (so all drivers emit for free), the wave-end audit can re-litigate — V1 has one production LLM driver, so the redundancy doesn't matter.

---

## D-041 — Provider corrections: outside the safety pass; single baked-in mode; `CorrectionsProfile` lives on `ModelProfile`; hook-registered wrapper

**Date:** 2026-05-11
**Status:** Settled
**Where it lives:** RFC §6.5, `docs/plans/phase-34-provider-corrections.md`, `internal/llm/llm.go` (`CorrectionsProfile` + four enum types), `internal/llm/registry.go` (`RegisterCorrectionsWrapper` hook + `Open` compose order), `internal/llm/corrections/corrections.go` (`Wrap` + `init()` self-registration), `internal/config/config.go` (`LLMCorrectionsConfig`, `LLMCorrectionsProfileConfig`), `internal/config/validate.go` (enum allowlists), brief 03 §4–§5, brief 08 §"Phase 34 scope shrinks slightly".

**Why:** Phase 34 ships the per-provider correction layer between Harbor's runtime and the Phase 32 `safetyClient(driver)`. Four design calls warrant a settled entry so a later auditor doesn't churn them.

1. **Compose order is `corrections(safetyClient(driver))` — corrections OUTSIDE safety.** The safety pass (D-026 / D-039) materializes oversize `DataURL`s, asserts no raw heavy content survived, and runs the token-budget guard. If corrections wrapped INSIDE safety, the safety pass would evaluate the PRE-correction request and any future correction that grows token count would slip past. With corrections outside, the safety pass sees the POST-correction request (the final outgoing payload reaching the driver) and its invariants apply to what actually leaves the runtime. Phase 34's quirks today are content-preserving (reordering, schema mutation, envelope translation, usage backfill); future quirks may not be. The outside-safety arrangement is the safe default.

2. **Single baked-in mode — no `use_native` toggle.** Brief 03 §5 documented the predecessor's `use_native_llm=True/False` toggle that shipped TWO LiteLLM/native implementations in parallel and is exactly the "two parallel implementations of the same conceptual feature" §13 rejects. Harbor picks one architecture (`corrections.Wrap` over a `bifrost`-backed driver) and compiles the per-provider quirks into a single layer. The operator's only choice is `enable: true` (production default) or `enable: false` (test-only escape hatch). The yaml field is a `*bool` so the loader distinguishes "operator omitted" (nil → default true) from "operator explicitly disabled."

3. **`CorrectionsProfile` lives on `llm.ModelProfile`, not in `internal/llm/corrections`.** Two reasons: (a) Import-cycle avoidance — `corrections` imports `llm`; placing the profile TYPE on `ModelProfile` in the `llm` package lets the corrections sub-package consume it without a back-edge. (b) Single source of truth — `ModelProfile` already carries `JSONSchemaMode` (Phase 35), `DefaultMaxTokens` (Phase 36b), `ReasoningEffort` (Phase 33), `CostOverrides` (Phase 36a). The corrections fields belong in the same bundle so an operator's `harbor.yaml` `model_profiles[<model>]:` block is the one canonical place per-model quirks land. The corrections LOGIC stays in `internal/llm/corrections/`.

4. **Hook-registered wrapper, blank-imported in `cmd/harbor/main.go`.** `llm.RegisterCorrectionsWrapper(fn)` is the seam: the corrections package's `init()` calls it with `Wrap`. Production binaries blank-import `_ "github.com/hurtener/Harbor/internal/llm/corrections"` so the registration fires at boot. Tests that exercise the safety pass in isolation set `cfg.DisableCorrections = true`; tests that exercise the corrections layer directly call `corrections.Wrap` without going through `llm.Open`. This pattern mirrors §4.4's driver-registry seam — write-once-at-init, blank-import for production wiring, opt-out for tests.

Inverse-naming the snapshot field `DisableCorrections` (instead of `CorrectionsEnabled`) means the zero-value matches the production default: programmatic snapshot construction in tests does not have to flip an extra knob to get correct behaviour. The config loader's `*bool` Enabled field resolves to `DisableCorrections = !*Enabled` at the boundary (Phase 64+ implements the mapping; today the snapshot is constructed directly by tests).

---

## D-042 — Custom OpenAI-compatible providers: operator-declared via yaml, OpenAI base-type only (Phase 33a), per-provider network knobs override global `NetworkDefaults`, env var resolves at `New` time

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.5, `docs/plans/phase-33a-custom-providers.md`, `internal/config/config.go` (`LLMCustomProviderConfig` + `LLMNetworkDefaults`), `internal/config/validate.go` (cross-check against native ∪ custom names; `nativeBifrostProviders` mirror; `allowedCustomBaseProviderTypes`), `internal/llm/registry.go` (`CustomProviderSpec` + `NetworkDefaults` on `ConfigSnapshot`), `internal/llm/drivers/bifrost/account.go` (Account widened to support custom primary; `buildCustomProviderConfig`; `customByName` table), brief 03 §"Provider catalog", brief 08 §"Architecture".

**Why:** Phase 33 shipped a thin bifrost adapter for the native provider list (OpenAI / OpenRouter / Anthropic / Cohere / Mistral / NIM / etc.). Operators want to wire OpenAI-compatible endpoints (NIM as the canonical first case, plus vLLM, ollama, lm-studio, in-house gateways) without per-provider Go code. Bifrost ships `schemas.CustomProviderConfig` for exactly this use case — Phase 33a exposes the operator-tunable subset. Four design calls warrant a settled entry.

1. **OpenAI-compatible base type only at Phase 33a.** `LLMCustomProviderConfig.BaseProviderType` defaults to `"openai"` and only that value is accepted at this phase. Bifrost itself supports Anthropic / Mistral / etc. as base types for custom providers; widening Harbor's surface is a Phase 33b/c task once we have evidence operators need it. The narrow surface today avoids fighting yaml when no one's calibrated the per-base-type quirks yet. The validator's `allowedCustomBaseProviderTypes` map gates this; widening is a one-line table edit + a phase plan.

2. **Per-provider network knobs override global `NetworkDefaults`.** Phase 33a unifies `Timeout` / `MaxRetries` / `RetryBackoff*` / `Concurrency` / `BufferSize` under one operator-facing surface (`llm.network_defaults`) with per-provider overrides on each custom entry. Zero-valued per-provider fields fall through to the global; zero-valued globals fall through to bifrost's package-level defaults. The fallthrough order (per-provider > global > bifrost-default) is identical for native primary and custom primary — operators tune them with one mental model. The motivating case is NIM cold-start latency (often > 60s); a 180-second per-provider `Timeout` on the NIM entry survives the cold-start without pulling every other provider's timeout up.

3. **API key resolution: `env.NAME` for native primary, raw env var NAME for custom providers.** The native primary path (Phase 33) inherited `LLMConfig.APIKey` with the `env.NAME` form because the field overloads literal-or-env. Custom providers have a dedicated `APIKeyEnvVar` field; operators write the env var NAME directly (e.g. `"NVIDIA_API_KEY"`, NOT `"env.NVIDIA_API_KEY"`). This is one indirection shorter and avoids the literal-vs-env ambiguity for the multi-provider case. The validator rejects `env.` prefixes on `APIKeyEnvVar` with a clear error so the operator notices the asymmetry. Both forms resolve `os.Getenv(NAME)` at `New` time; missing env vars fail closed with `ErrMissingAPIKey` naming the unset variable.

4. **`GetConfiguredProviders` returns the single PRIMARY provider only — D-040 preserved.** Phase 33a's Account holds a `customByName` map of every declared custom provider but `GetConfiguredProviders` returns only the one named by `LLMConfig.Provider`. Multi-provider routing within a single Harbor instance is a future extension; the seam (the table, the per-provider config resolution) is ready but Phase 33a does not commit to multi-routing semantics. This keeps D-040's "single-provider per Harbor instance" intact while making the future widening additive (no API change to `GetConfiguredProviders` — just return the full table when the time comes).

The operator-facing BaseURL gotcha lands in this entry too: bifrost's OpenAI provider appends `/v1/chat/completions` to whatever `BaseURL` the operator sets. Operators write the HOST root (`https://integrate.api.nvidia.com`) — NOT the full `/v1/` path — for the canonical case. Endpoints whose URL already includes `/v1` use `RequestPathOverrides` to override the suffix. The example yaml documents this; the wire-level integration test asserts the path is `/v1/chat/completions` (not `/v1/v1/...`).

Sub-second `Timeout` values get rounded down to zero by bifrost's `int(seconds)` conversion at the `NetworkConfig.DefaultRequestTimeoutInSeconds` boundary. Operators who need sub-second timeouts wait for Phase 33b's `NetworkConfig` widening; today the practical minimum is 1 second. The custom-provider wire timeout test uses 1s vs 3s server sleep to clear this boundary.

---

## D-043 — LLM-edge compose order: `retry(downgrade(corrections(safety(driver))))`; `OutputMode.Tools` is Harbor-side prompted output, not provider tool-calling; `Validator` is a `CompleteRequest` field

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.5, `docs/plans/phase-35-structured-output.md`, `docs/plans/phase-36-retry-feedback.md`, `internal/llm/llm.go` (`OutputMode` enum + `CompleteRequest.Validator` field + `ModelProfile.OutputMode`/`MaxRetries`), `internal/llm/registry.go` (`RegisterDowngradeWrapper` + `RegisterRetryWrapper` + the compose chain in `Open`), `internal/llm/output/` (downgrade wrapper), `internal/llm/retry/` (retry wrapper), `internal/llm/errors.go` (`IsInvalidJSONSchemaError` + new sentinels), `internal/llm/events.go` (`ModeDowngradedPayload` + `RetryWithFeedbackPayload`), brief 03 §6, brief 07.

**Why:** Phases 35 + 36 ship two new wrappers on top of Phase 32's `safetyClient` and Phase 34's `corrections.Wrap`. Three design calls warrant a settled entry.

1. **Compose order — `retry(downgrade(corrections(safety(driver))))`.** Three principles drive the order, outermost first:
   - **Retry is outermost.** A validator-driven retry appends a corrective user turn to the conversation; the new turn must flow through corrections + downgrade + safety on each attempt. Corrections normalize message ordering (NIM rejects mid-thread system) — if retry sat INSIDE corrections, the corrected message slice would be augmented with the corrective turn AFTER the reorder, breaking the invariant on the second attempt.
   - **Downgrade sits between retry and corrections.** A downgrade rewrites `ResponseFormat` (e.g. `json_schema` → `json_object` + system-prompt instruction); corrections then re-shape the per-provider envelope for the rewritten format (Anthropic envelope translation; JSONOnly stash-the-schema hint). If downgrade sat INSIDE corrections, the corrections layer would only see the ORIGINAL format; the downgraded format would skip the per-provider shaping.
   - **Corrections sit between downgrade and safety.** Settled by D-041. The safety net (D-039 / mandatory-by-construction) sees the post-corrections request — leak-detection and the token-budget guard apply to the final outgoing payload regardless of whether downgrade or retry rewrote it.

   The chain is composed in `llm.Open` via three write-once hooks: `RegisterCorrectionsWrapper` (Phase 34), `RegisterDowngradeWrapper` (Phase 35), `RegisterRetryWrapper` (Phase 36). The wrappers self-register via `init()` in their respective sub-packages; `cmd/harbor/main.go` blank-imports them. The `ConfigSnapshot.DisableDowngrade` / `DisableRetry` inverse-named knobs (zero-value = enabled) let tests exercise lower layers in isolation.

2. **`OutputMode.Tools` is a Harbor-side prompted-output strategy, NOT provider tool-calling.** RFC §6.4 + brief 07 keep tool dispatch runtime-side. `OutputMode.Tools` asks the model to emit `{"name":"respond_with","arguments":{...}}` as plain JSON output (parsed locally by the runtime); the bifrost driver never sees provider-native `tools=` / `tool_choice=` / `function_call` / `tool_use` parameters. The static guard in `scripts/smoke/phase-35.sh` greps `internal/llm/output/` for the canonical provider-tool-call symbol names; a leak fails the smoke. The package godoc names the boundary explicitly so future readers don't reintroduce the violation by reaching for bifrost's native tool-call API.

3. **`Validator` is a field on `CompleteRequest`, not a separate method.** Two alternatives were considered: (a) `Validator func(CompleteResponse) error` field on `CompleteRequest` — the retry wrapper runs the loop internally; (b) a `client.Validate(resp) error` method on `LLMClient` — callers run the loop themselves. Option (a) wins because Phase 36a / governance wraps the OUTER client with `PreCall` / `PostCall` hooks — the retry loop must stay INSIDE the governance wrapper so each retry's call counts against the identity budget. Surfacing the loop as a caller-driven `Validate` method would leak retry semantics to governance and require every caller to re-implement the bounded loop. The field-on-request shape also lets the validator be `nil` (the common case) — the wrapper becomes a pure pass-through with one branch.

The wrapper's corrective-sub-prompt template ships fixed at Phase 36: assistant turn echoes the rejected content; a user turn says "Your previous response failed validation: <truncated reason>. Please respond again, addressing this issue exactly." Tuning is post-V1 — operators who need a different template can shadow-wrap the retry layer in their own code.

`IsInvalidJSONSchemaError` is the boundary the downgrade wrapper uses to classify driver errors. The classifier checks (1) `errors.Is(err, ErrInvalidJSONSchema)` for drivers that wrap with the sentinel, and (2) a small case-insensitive substring allowlist (`json_schema`, `json schema`, `invalid schema`, `schema validation`, `response_format`, `response format`, `structured output`, `json mode`, `json_object`). The allowlist is deliberately narrow to avoid false-positive downgrades on transient / auth / 5xx failures. Drivers can tighten the classification by wrapping their provider-specific schema errors with `ErrInvalidJSONSchema` — Phase 33's bifrost driver is a §17.6 follow-up candidate.

`ResponseFormatProfile.ResponseFormatJSONOnly` (Phase 34) and `OutputMode.Prompted` (Phase 35) are deliberately distinct concepts. JSONOnly is a corrections-layer per-provider quirk: "this provider rejects `json_schema` at the wire level, surface schema as `Extra["schema_hint"]`." Prompted is a Harbor-side output-mode strategy: "skip native schema enforcement entirely, instruct the model to emit JSON matching the schema via system prompt." They compose: a Prompted request flowing through a JSONOnly profile would have the schema both in the system prompt (Prompted's job) and in `Extra["schema_hint"]` (JSONOnly's job, when a `FormatJSONSchema` survives). Operators who want one or the other (not both) set OutputMode and leave the corrections profile default, or vice versa.

---

<!--
Append new entries below this line in the form:

## D-NNN — <one-line summary>
**Date:** YYYY-MM-DD
**Status:** Settled | Tentative | Superseded by D-MMM | Reverted
**Where it lives:** <files>
**Why:** <2-3 sentences>
-->

## D-044 — Governance ships latent at V1: interface + math wired, every enforcement path is opt-in; PostCall is the in-band cost accumulator; compose order `governance(retry(downgrade(corrections(safety(driver)))))`

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.15, `docs/plans/phase-36a-cost-accumulator.md`, `docs/plans/phase-36b-rate-limit-maxtokens.md`, `internal/governance/` (Subsystem + Wrap + CostAccumulator + RateLimiter + MaxTokensEnforcer + Compound + registry + events + errors), `internal/llm/registry.go` (`RegisterGovernanceWrapper` + the new outermost compose step in `Open`), `internal/config/config.go` (`GovernanceConfig.IdentityTiers` + `DefaultTier` + `GovernanceTierConfig` + `GovernanceRateLimitConfig`), `internal/config/validate.go` (the tier validator block), `examples/harbor.yaml` (the latent-default + commented opt-in block), brief 03 §6, brief 06 §3.

**Why:** Phases 36a + 36b establish Harbor's governance subsystem (cost ceilings, rate limits, per-call MaxTokens) wrapping the LLM-edge chain. Four design calls warrant a settled entry.

1. **Latent V1 default.** The interface, accumulator math, token-bucket math, persistence (three-driver state-store conformance), event taxonomy (`governance.budget_exceeded` / `governance.rate_limited` / `governance.maxtokens_exceeded`), and the compose seam all ship in Wave 7b. **Every enforcement path defaults to permit** — an operator must populate `Governance.IdentityTiers` with at least one tier (and set `DefaultTier` or supply a custom `TierResolver`) for any policy to fire. Each tier's fields (`BudgetCeilingUSD`, `RateLimit`, `MaxTokens`) are independently opt-in. This is the Wave 7b scoping decision: V1 ships plumbing visible to operators but enforcement waits on operator policy. Future Protocol-driven setters (post-V1 phase 91) let Console flip tiers without restart.

2. **PostCall is the in-band cost accumulator path — NOT a subscription to `llm.cost.recorded`.** The cost-recorded event fires from the bifrost driver (Phase 33's `emitCostRecorded`) and remains the operator-facing observability stream. The governance accumulator updates synchronously in `PostCall` per RFC §6.15 line 1128 ("PostCall... Accumulates cost / tokens / latency"). A subscriber-based accumulator opens a race window where the next `PreCall` checks the ceiling before the previous call's cost lands; ceiling enforcement correctness requires synchronous update. The atomic CAS (`math.Float64bits` + `CompareAndSwap`) lets concurrent PostCalls accumulate lock-free.

3. **Compose order `governance(retry(downgrade(corrections(safety(driver)))))`.** Governance is the OUTERMOST wrapper, sitting outside Phase 36's retry per D-043 + master plan line 420. A `PreCall` that fires `ErrBudgetExceeded` MUST short-circuit before retry / downgrade burn attempts; rejecting once is the correct semantics. PostCall runs after the entire downstream chain returns, so it sees the final outcome (post-downgrade, post-retry). `governance.SetFactory` is the per-process hook; `cmd/harbor` blank-imports `internal/governance` so the wrapper hook seats at boot. With no factory set, the hook is a pass-through (latent default — even a registered package import does not implicitly enforce).

4. **Concurrent-call ceiling overshoot is bounded, not zero.** The PreCall→inner→PostCall sequence creates a race where N concurrent in-flight calls can each see "below ceiling" before any PostCall lands. The accumulator overshoots by at most `in_flight × per_call_max_cost`. The conformance test asserts `total ≤ ceiling + N × per_call_max_cost` rather than strict equality. Operators who need first-cross-blocks-everyone semantics get them post-V1 via the unified pause/resume primitive (RFC §6.15 line 1181) — V1 ships eventually-consistent ceilings. `governance.budget_exceeded` events emit only from PreCall on the NEXT call after a breach; a PostCall that pushes the accumulator over the ceiling is accepted (the call already happened) and the breach surfaces via the cost-recorded observability stream.

The `MaxTokens` semantic is fail-loud not clamp (master plan line 420 + RFC §6.15 line 1122 both say `ErrMaxTokensExceeded`). Refunds on call failure are out of scope (RFC §6.15 simplicity — drain-on-PreCall is final). State persistence is one record per identity (Kind=`governance.cost` for accumulator, Kind=`governance.bucket` for buckets), JSON-encoded for cross-driver byte-stability; the wire shape carries a schema version field for forward-compat.

`governance.NewCompound(subs...)` bundles `MaxTokensEnforcer` (cheapest reject — no state I/O), `RateLimiter` (per-key mutex + per-identity state write), and `CostAccumulator` (state I/O on every PostCall) into one Subsystem. Fan-out order is operator-driven; the default ordering puts cheapest-first so a likely rejection short-circuits before reaching the state-heavy accumulator.

## D-045 — Skills LocalDB driver owns its own tables (no piggyback on StateStore); FTS5 detected at open with deterministic regex/exact fallback

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-37-skills-store.md`, `internal/skills/skills.go` (`Deps` has no `State` field), `internal/skills/drivers/localdb/localdb.go` (driver opens its own DB), `internal/skills/drivers/localdb/migrations/0001_init.sql` (own `skills` + `skills_fts` schema), `internal/skills/drivers/localdb/search.go` (FTS5 → regex → exact ladder), brief 04 §4.3 + §4.4.

**Why:** Phase 37 lands the SkillStore subsystem. Two design calls warrant a settled entry.

1. **D-034 analog: skills drivers own their tables; the `Deps` struct does NOT carry a `StateStore`.** Memory's Phase 25 settled the precedent — persistent memory drivers own a dedicated `memory_state` table rather than piggybacking on the StateStore's `state_records` shape (D-034). The skills LocalDB driver follows the same pattern with a dedicated `skills` + `skills_fts` schema. Three reasons compound:
   - **Schema fit.** `Skill` has 20+ load-bearing columns (`Origin`, `OriginRef`, `Scope`, `ScopeTenantID`, `ScopeProjectID`, `ContentHash`, JSON-encoded slices, lifecycle timestamps). The StateStore's `(Quadruple, Kind, Bytes)` envelope means every column lookup is a JSON probe — fine for opaque memory blobs, not for an indexed FTS5 corpus.
   - **FTS5 needs a real table.** The FTS5 virtual table uses `content='skills' content_rowid='rowid'` external-content mode + INSERT/DELETE/UPDATE triggers to mirror the skills table. Building this against `state_records` would require a phantom-content table and a custom rowid mapping; the per-driver `skills` schema is cleaner.
   - **Cross-driver portability.** The Portico SkillStore driver (post-V1) talks to a remote MCP server and has no StateStore need either — keeping the seam free of StateStore obligations widens the door for future drivers (Git, OCI, HTTP).

2. **FTS5 availability detected at open via a probe query; the ranking ladder gracefully falls through to regex/exact when FTS5 is unavailable.** brief 04 §4.4 mandates the fallback test. `modernc.org/sqlite` compiles FTS5 in by default, so the production path always uses FTS5; the fallback is a correctness gate for builds (and for tests that force `ftsAvailable = false` via the internal test surface). **No operator-facing knob** to force the regex/exact path — that would be a "two parallel implementations of the same feature" (AGENTS.md §13 forbidden practice). Detection is mechanical: `SELECT count(*) FROM skills_fts WHERE skills_fts MATCH '__fts_probe__'` either succeeds (FTS5 alive) or errors (the migration's `CREATE VIRTUAL TABLE` rolled back on a build without FTS5).

## D-046 — Skill ContentHash is sha256 over canonicalised content fields, excluding Origin / OriginRef / Scope / lifecycle timestamps

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-37-skills-store.md`, `internal/skills/wire.go` (`CanonicalContentHash`), `internal/skills/drivers/localdb/localdb.go` (LWW + idempotency check uses the hash), brief 04 §4.8.

**Why:** Conflict policy needs a deterministic gate. brief 04 §4.8 says "Generated → Generated: last-write-wins gated by `content_hash` change" — the hash must be stable across re-imports (the same Skills.md pack imported twice produces the same hash) and resilient to caller-side normalisation noise.

The canonical hash envelope:

- **Included:** `Name`, `Title`, `Description`, `Trigger`, `TaskType`, sorted `Tags`, ordered `Steps`, ordered `Preconditions`, ordered `FailureModes`, sorted `RequiredTools`, sorted `RequiredNS`, sorted `RequiredTags`, `Extra` (key-sorted text rendering).
- **Excluded:** `Origin`, `OriginRef`, `Scope`, `ScopeTenantID`, `ScopeProjectID` — provenance metadata that legitimately differs across import paths without representing content drift.
- **Excluded:** `CreatedAt`, `UpdatedAt`, `LastUsed`, `UseCount` — lifecycle state that evolves over a row's life.

Slice fields are sorted before hashing when ordering is non-semantic (`Tags`, `RequiredTools`, `RequiredNS`, `RequiredTags`); preserved when ordering is semantic (`Steps`, `Preconditions`, `FailureModes` — these are procedural prose rendered to the planner in declared order). Field separator is `\x1f` (ASCII unit-separator) so caller-supplied newlines / whitespace / pipes can't collide with the envelope framing.

`Extra` participates because the generator may stamp model-specific metadata there that legitimately differs between drafts even when the body text is identical (e.g. the model fingerprint that produced the skill). The renderer accepts string / int / int64 / float64 / bool / nil and substitutes `<unhashable>` for anything else so a caller-side type bug yields a stable hash rather than a panic or non-deterministic ordering.

The hash version is implicit at V1 — changes to the envelope format (adding / removing / reordering fields) require a `0002_*.sql` migration that rehashes existing rows AND a new decisions entry naming the old/new envelope. Operators with frozen content_hash values in external systems are explicitly out of scope at V1; we cross that bridge when a downstream system surfaces the hash externally.

---

## D-047 — Planner package owns `PauseReason`, `FinishReason`, `WakeMode`, and the `SpawnSpec` shape; the TaskRegistry stays neutral on wake-mode

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.2, RFC §6.3, RFC §3.2, `docs/plans/phase-42-planner-iface.md`, `internal/planner/planner.go` (`PauseReason`, `FinishReason`), `internal/planner/wake.go` (`WakeMode`, `WakeAware`, `ResolveWakeMode`), `internal/planner/decision.go` (`SpawnSpec` wrapping the planner-side subset of `tasks.SpawnRequest`), brief 02 §2.

**Why:** Phase 42 lands the planner's swappable seam (CLAUDE.md §1 / RFC §3.2). Four design calls warrant a settled entry.

1. **`PauseReason` lives in the planner package, not in a `pauseresume` package.** The unified pause/resume primitive (later phase) is not yet shipped; brief 02 §2 sketches `PauseReason` as a planner-local type. Phase 42 follows the sketch — the four canonical values (`approval_required`, `await_input`, `external_event`, `constraints_conflict`) live in `internal/planner/planner.go`. When the unified pauseresume phase lands, it MAY canonicalise via a typedef bridge (`pauseresume.Reason = planner.PauseReason`) without changing call sites. The enum values match the RFC §6.3 canonical strings exactly, so the bridge is byte-stable.

2. **`SpawnSpec` is a planner-side projection of `tasks.SpawnRequest`, not a duplicate type.** Brief 02 §2 sketches `SpawnTask{ Kind TaskKind; Spec TaskSpec }` as planner-local types. Phase 42 departs: `SpawnTask.Kind` is the production `tasks.TaskKind`; `SpawnTask.Spec` is `planner.SpawnSpec` (the planner-visible subset — Description, Query, Priority, RetainTurn, FailFast). The Runtime fills the rest of `tasks.SpawnRequest` (Identity from the run quadruple; IdempotencyKey from the planner step counter; PropagateOnCancel from the default; NotifyOnComplete from the spawn intent) at dispatch time. Duplicating `tasks.TaskKind` in the planner would be a §13 "two parallel implementations of the same conceptual feature" smell — `internal/tasks` is NOT a `internal/runtime/...` package, so the import is fine.

3. **`WakeMode` enum + optional `WakeAware` interface live in the planner package — the TaskRegistry stays neutral (D-032).** D-032 settled that the wake-on-resolution strategy (`push` / `poll` / `hybrid`) is a planner-concrete concern, not a registry concern. Phase 42's `internal/planner/wake.go` ships the canonical enum + the optional `WakeAware` interface a concrete may implement to declare its mode. The conformance pack (Phase 49) uses `planner.ResolveWakeMode(planner.Planner) WakeMode` (which falls back to `WakePush` for concretes that skip `WakeAware`) to assert the round-trip. The `WakeAware` interface is NOT a `Supports*` capability protocol (§4.4 forbids those when all V1 drivers implement everything) — it's identity / metadata for a single mode each concrete picks at construction time. The conformance assertion exercises BOTH branches (concretes with WakeAware AND concretes without).

4. **`FinishReason` is canonical at the planner edge, NOT at the Protocol edge.** The Protocol's `task.completed` / `task.failed` event payloads (later phase) project `FinishReason` into a Protocol-stable representation; the planner-internal enum is the truth source. Phase 42's enum (`goal`, `no_path`, `cancelled`, `deadline_exceeded`, `constraints_conflict`) covers the V1 terminals; future phases (`phase-44-schema-repair`, `phase-50-pauseresume`) add no new reasons — every terminal collapses to one of these five. `IsValidFinishReason` is the validator the Runtime executor will use to reject malformed Decisions before dispatch.

Additionally, Phase 42 declares the planner-emitted event taxonomy (`planner.decision`, `planner.finish`, `planner.error`) in `internal/planner/events.go` and registers the types via `events.RegisterEventType` from the package `init()`. The payload structs land at Phase 45 (the first concrete that emits); registering the type names at Phase 42 lets future concretes emit without re-registering. The stub `finish.Planner` does not emit (`Emit` may be nil); concrete planners (Phase 45+) MUST nil-check before calling.

The §13 import-graph lint test (`internal/planner/conformance/importgraph_test.go`) is the gate that keeps `internal/planner/...` decoupled from `internal/runtime/...`. The test walks the planner subtree with `go/parser` and fails the build on any `internal/runtime/...` import. Concretes added at Phase 45 / 48 inherit the gate without re-authoring.

## D-049 — Trajectory fail-loudly Serialize contract lives in `internal/planner/trajectory/`; process-local handle registry at V1; canonical JSON ordering; Phase 42 stub retired

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.2, RFC §3.4, RFC §6.3, `docs/plans/phase-43-trajectory.md`, `internal/planner/trajectory/trajectory.go` (Trajectory + Step + nested types), `internal/planner/trajectory/toolcontext.go` (ToolContext split + HandleID), `internal/planner/trajectory/registry.go` (HandleRegistry interface + process-local driver), `internal/planner/trajectory/errors.go` (`ErrUnserializable` + `ErrToolContextLost` struct sentinels), `internal/planner/trajectory/serialize.go` (Serialize + Deserialize + reflective walker), `internal/planner/trajectory.go` (alias re-exports from the subpackage), brief 02 §4.

**Why:** Phase 43 closes the load-bearing predecessor-bug: the silent-context-loss path where a non-serialisable handle in pause state was dropped silently. Four design calls warrant a settled entry.

1. **The trajectory subsystem lives at `internal/planner/trajectory/`, not directly in `internal/planner/`.** The master plan's `Subsystem` column reads `planner/trajectory`; Phase 42 shipped the type skeleton inline in `internal/planner/trajectory.go` (file, not subpackage) as a placeholder. Phase 43 moves the load-bearing types (`Trajectory`, `Step`, `ToolContext`, `HandleID`, `HandleRegistry`, `ErrUnserializable`, `ErrToolContextLost`) into the canonical subpackage so the §4.4 extensibility-seam pattern applies — future drivers (a distributed handle registry, alternate serialisers) land alongside the existing process-local driver. The legacy planner-package types become type aliases (`type Trajectory = trajectory.Trajectory`) so existing call sites compile unchanged. Phase 42's stub `ErrTrajectoryNotImplemented` is retired: the only consumer was Phase 42's own test, which Phase 43 updates to exercise the real fail-loudly contract.

2. **`Trajectory.Serialize` uses a reflective pre-flight walker; the walker drives the canonical fail-loudly contract.** The stdlib `json.Marshal` reports non-encodable types via `*UnsupportedTypeError` / `*UnsupportedValueError` — adequate for binary outcome but inadequate for the actionable field path the contract requires. Phase 43's walker recursively traverses the trajectory by `reflect.Value`, tracking the dotted field path (`"Trajectory.Steps[3].Observation.callback"`); on the first non-encodable leaf it returns `(nil, ErrUnserializable{Field: <path>})`. The walker mirrors `encoding/json`'s encoding rules verbatim (chan / func / unsafe.Pointer / complex are rejected; nil interfaces / nil pointers / nil slices encode as JSON null; `[]byte` encodes as base64; `json.Marshaler` implementers are probed; struct fields with `json:"-"` are skipped; cyclic graphs surface as `ErrUnserializable{Field: ... <cycle>}` via a visited-pointer-address map). On the happy path the walker passes; `json.Marshal` then produces the canonical bytes.

3. **HandleRegistry is process-local at V1; distributed-handle directory is a post-V1 RFC concern.** RFC §6.3 already documents this constraint: "V1: process-local. Resume must run in the same Runtime process. The seam for a distributed handle directory exists (the registry is an interface) but no production driver ships at V1." Phase 43 ships the `HandleRegistry` interface (`Set` / `Get` / `Delete`) with one V1 driver — `processLocalRegistry` backed by `sync.Map`. The choice of `sync.Map` over `map + RWMutex` matches the read-heavy access pattern (one Set on tool dispatch, many Gets across pause/resume / planner steps); D-025 concurrent-reuse stress under `-race` is green with N=128. **The fail-loud contract is `Get` returns `(nil, ErrToolContextLost{Handle: id})` on miss — never `(nil, nil)`.** This is the load-bearing closure: the predecessor's `try { ... } catch { return None }` shape is rejected here, in `Trajectory.Serialize`, and in the planner-package alias re-exports — three places enforcing the same invariant.

4. **Canonical JSON ordering: declaration-order struct fields + alphabetised map keys.** The stdlib `encoding/json` emits struct fields in declaration order (per JSON tag) and alphabetises `map[string]X` keys. Combined with explicit JSON tags on every `Trajectory` field, the canonical form is stable across re-encoding **when `any`-valued fields hold JSON-tree shapes** (`map[string]any` / `[]any` / primitives). The runtime planner-step builder (later phase) follows this discipline; Phase 43's golden-bytes test uses the same shape and pins the canonical encoding. When `any` values hold Go structs, the first encoding (declaration-order) and the second encoding (alphabetised map after Deserialize) MAY diverge — the godoc on `LLMContext` / `HintState` / `Step.Observation` documents the discipline.

Round-trip byte stability is the load-bearing acceptance criterion from RFC §3.4 + brief 02 §4: `Serialize → Deserialize → Serialize` produces byte-identical output. The invariant is asserted in `trajectory_test.go::TestRoundTrip_ByteStable` against a populated trajectory using JSON-tree shapes throughout.

The §11 mandatory pause/resume serialisation test (`toolcontext_test.go::TestPauseStateSerialisation_FailsLoudlyOnUnserializableContext`) constructs a pause-state-shaped trajectory whose `ToolContext.Serializable` carries a live channel masquerading as a "config" value; asserts `Serialize` returns `ErrUnserializable` with the channel's key in the field path. The companion test `TestResumeWithStaleHandle_ReturnsErrToolContextLost` verifies the second half of the contract: a serialised trajectory carrying a `HandleID` whose registry mapping has died (simulated by a fresh `HandleRegistry` on the resume side) surfaces `ErrToolContextLost` on `Get` — never `(nil, nil)`.

The D-025 concurrent-reuse contract is pinned in `concurrent_test.go` across four tests: N=128 goroutines serialising distinct trajectories, N=128 goroutines exercising `HandleRegistry.Set/Get/Delete` on disjoint IDs, N=128 goroutines reading a shared handle, and N=128 goroutines serialising a shared read-only trajectory. All four exit under `-race` with no leaks (baseline `runtime.NumGoroutine` restored), no context bleed, no byte-stability violations across concurrent invocations.

The §13 forbidden practice of "silent degradation" is closed by construction at three layers: `Trajectory.Serialize` (no `try/catch → nil` path), `HandleRegistry.Get` (no `(nil, nil)` return), and the planner-package alias re-exports (`ErrUnserializable` / `ErrToolContextLost` are public sentinels callers reach for via `errors.As`). Phase 51's pause-record contract (later phase) consumes this phase's Serialize bytes; Phase 51 inherits the fail-loud invariants without re-authoring.

---

## D-050 — Repair ladder ordering (salvage → schema repair → graceful failure → multi-action salvage); graceful failure is `Finish{NoPath}` not error; `Followup` carried via `Metadata`; parser+loop both live under `internal/planner/repair/`

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.2 (Settled — "salvage → schema repair → graceful failure → multi-action salvage" + `arg_fill_enabled` / `repair_attempts` / `max_consecutive_arg_failures` knobs), `docs/plans/phase-44-schema-repair.md`, `internal/planner/repair/repair.go` (`Config`, `RepairLoop`, `Run`, `gracefulFailure`), `internal/planner/repair/parser.go` (`ActionParser`), `internal/planner/events.go` (`EventTypePlannerRepairExhausted`, `RepairExhaustedPayload`), brief 02 §6, brief 07 §3 + §8 + §10.

**Why:** Phase 44 lands the salvage / schema-repair / graceful-failure / multi-action-salvage ladder for planner steps. Four design calls warrant a settled entry.

1. **Ladder ordering is load-bearing: salvage → schema repair → graceful failure → multi-action salvage.** RFC §6.2 states the ladder explicitly. The order is binding because each step's invariant depends on the prior step:
   - **Salvage is FIRST** because a malformed parse leaves the loop without typed `[]planner.CallTool` to validate. The parser is the only tolerant pass in the ladder — it accepts fenced JSON (` ```json `), prose-wrapped JSON, multi-object scans, and bare arrays. Brief 07 §3 catalogued the predecessor's parser modes; Phase 44's `ActionParser.Parse` ships them as the salvage step.
   - **Schema repair is SECOND** because validating args is meaningful only on a parsed action. The corrective sub-prompt names the tool + the validator's complaint verbatim ("argument `X` failed: <validator error>; please re-emit with the corrected field"), which is a focused signal the LLM can act on. Bounded by `Config.RepairAttempts`.
   - **Graceful failure is THIRD (a terminal short-circuit, not a step)** because brief 07 §10 catalogued the failure-mode-blind footgun in the predecessor: "if the model's response is consistently malformed, `_repair_attempts` (default 3) of identical-shape feedback may never converge." Phase 44's `Config.MaxConsecutiveArgFailures` is a *separate* counter from `Config.RepairAttempts` — identical-shape failures terminate via the consecutive-failure path even when the attempts budget is high. Default `MaxConsecutiveArgFailures = 2 < RepairAttempts = 3` so the storm guard typically fires first.
   - **Multi-action salvage is FOURTH (a packaging step, not a step)** because it operates on the OUTPUT of salvage + repair. When the parser returned >1 well-formed `CallTool` and every one validates, the loop packages them as `CallParallel{Branches: [...], Join: JoinAll}` rather than re-asking the LLM. Concretes that want sequential salvage opt out by setting `Config.ArgFillEnabled = false`.

2. **Graceful failure is `Finish{Reason: NoPath, Metadata["followup"]=true}`, NOT an error.** The repair loop's `Run` returns `(planner.Decision, error)`. On graceful failure the loop returns `(Finish{}, nil)` — Finish IS the success path the planner contract describes. An error return would conflate two distinct conditions: (a) the LLM client surfaced a transient error (caller must retry / abort the run), vs. (b) the repair ladder exhausted (the planner step itself produced a terminal Finish that the runtime executor maps to `task.completed` with `reason=no_path`). Conflating them would break the planner contract (§13 forbids two-parallel-implementations of the same feature; here the feature is "what shape the planner returns at step end"). The `planner.repair_exhausted` event emit is the load-bearing observability surface — graceful failure is NOT silent (§13 silent-degradation ban). The event payload carries the attempt count, consecutive-failure counter, and truncated chain of validator reasons; operators see the failure loudly via the bus + audit pipeline.

3. **`Followup` carried via `Metadata["followup"] = true`, NOT a new field on `planner.Finish`.** Brief 02 §6 spec'd `Finish{Reason: NoPath, Followup: true}` but Phase 42 froze the `Finish` struct (Reason, Payload, Metadata — D-047). Adding a `Followup bool` field would require touching every Phase 45 / 48 / 49 concrete and the conformance pack, and would re-litigate D-047. `Metadata` is already the documented surface for terminal-decision annotations (the stub `finish.Planner` uses it for `run_id` round-trip; Phase 45 will use it for the planner's free-form Reasoning hash). Phase 49's conformance pack reads `Metadata["followup"]` to detect the followup signal; glossary entries spell out the convention. Same applies to the auxiliary fields the loop stamps for observability: `Metadata["repair_attempts"]`, `Metadata["repair_consecutive_arg_failures"]`, `Metadata["repair_chain"]`, `Metadata["repair_error"]`.

4. **Parser + loop both live under `internal/planner/repair/`, NOT `internal/runtime/planner/parser/`.** Brief 07 §8 sketched `ActionParser` at `internal/runtime/planner/parser/`. Phase 44 co-locates the parser with the loop under `internal/planner/repair/`. Three reasons:
   - **Import-graph contract (Phase 42 settled).** The planner subtree MUST NOT import `internal/runtime/...` — `internal/planner/conformance/importgraph_test.go` is the §13 gate. The parser is a planner-side utility (it produces `planner.CallTool` shapes the loop returns to the runtime executor); it cannot live at `internal/runtime/planner/parser/` without breaking the gate.
   - **Master-plan glossary lines 927 + 930.** "`ActionParser` (`internal/runtime/planner/parser/`) | 44 (Schema repair pipeline) + 45 (Reference ReAct planner)" + "`RepairLoop` | 44 (Schema repair pipeline)". The path "internal/runtime/planner/parser/" in the glossary is pre-RFC nomenclature; the RFC's settled home is `internal/planner/...`. Co-locating with the loop matches the "owned in one phase, consumed in another" pattern that the master plan glossary describes.
   - **Single-package cohesion.** Parser + loop + feedback builder + events live in one Go package. The package's godoc describes the ladder; the implementation is one file per concern (`repair.go`, `parser.go`, `feedback.go`); the test files mirror that split (`repair_test.go`, `parser_test.go`, `integration_test.go`, `d025_test.go`). Splitting parser into a sibling package would force a public API on what is structurally an implementation detail of the repair loop.

Additionally, Phase 44 ships:

- **`Config.ArgFillEnabled`** — opt-in. When false the loop returns the parser's first valid action(s) verbatim and lets the dispatcher's `tool.invalid_args` reject path handle schema misfits. Phase 45 (ReAct) defaults this to true; Phase 48 (Deterministic) defaults it to false (the deterministic planner does not consume LLM output, so the knob is structurally irrelevant).
- **`Config.RepairAttempts` default = 3** matching brief 07 §3 step 5's predecessor default. The storm guard is `Config.MaxConsecutiveArgFailures = 2 < 3` so the typical malformed-shape session terminates after 2 LLM calls rather than burning the full 3.
- **`planner.repair_exhausted` event taxonomy.** The event type registers in `internal/planner/events.go::init()` alongside `planner.decision` / `planner.finish` / `planner.error` (Phase 42 entries). The typed `RepairExhaustedPayload` (SafePayload) carries `Identity`, `Attempts`, `ConsecutiveArgFailures`, `Reasons []string` (each entry truncated to 256 bytes), `OccurredAt`. The payload struct ships in the same PR as the emit site — distinct from the Phase 42 pattern where payload structs deferred to Phase 45 — because Phase 44 IS the first emitter, so deferral would be a fail-loudly violation.
- **No two-parallel-retry-implementations (§13).** The repair loop calls `llm.LLMClient.Complete`; the LLM client (composed at `internal/llm/registry.go::Open`) already has the Phase 36 retry-with-feedback wrapper inside. Repair is OUTSIDE the LLM call (it consumes the response); retry-with-feedback is INSIDE the LLM call (it wraps a single attempt). The smoke script guards against `internal/planner/repair/` importing `internal/llm/retry` — composition stays at the registry edge.

The `internal/planner/repair/d025_test.go` ships the N=128 concurrent-reuse stress: one shared `RepairLoop` instance, per-goroutine identity quadruples, four response patterns (clean salvage / parser-correction / multi-action / graceful-failure), per-call identity round-trip assertion at three boundaries (the stub client's seen-ctx, the success-path Decision's `Reasoning` field, the graceful-failure-path `RepairExhaustedPayload.Identity`). Pre-cancelled ctxes on i%5==0 verify cancellation cross-talk is absent.

---

## D-048 — Phase 38 planner-skill tools: split into three Tools (not a SkillProvider struct); default-deny capability filter; chars/4 budgeter aligned with §6.5 LLM safety net

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-38-skill-planner-tools.md`, `internal/skills/tools/tools.go` (`Register`, `searchHandler`, `getHandler`, `listHandler`), `internal/skills/tools/filter.go` (capability subset gate), `internal/skills/tools/redactor.go` (tool-name + PII redaction), `internal/skills/tools/budgeter.go` (`Fit` ladder + `ErrSkillTooLarge`), brief 04 §4.5.

**Why:** Phase 38 lands the planner-facing surface for the skills subsystem. Three design calls warrant a settled entry.

1. **The planner-facing surface is three discrete Tools, not a `SkillProvider` struct.** RFC §6.7's sketch shows a `SkillProvider` interface with `Search / GetByName / List / Directory / FormatForInjection` — modelled after the predecessor's monolithic provider. Phase 38 splits the surface across three Tools registered through the Phase 26 catalog (`skill_search`, `skill_get`, `skill_list`) plus Phase 39's `Directory(cfg)` API rather than a single struct. Two reasons:
   - **Catalog dispatch uniformity.** Every other Harbor tool (HTTP, MCP, A2A, in-process, flow) goes through the catalog; carving a separate dispatch path for skills would split the reliability shell (`ToolPolicy` — D-024) and the audit emit taxonomy (`tool.invoked` / `tool.completed` / `tool.failed`). Three Tools-on-the-catalog gives the planner the same observability surface as any other tool.
   - **Per-tool ergonomics.** `skill_get` carries the tiered budgeter; `skill_search` carries ranking; `skill_list` carries paging. A single `SkillProvider.FormatForInjection` would have to multiplex these concerns — three Tools keep each handler narrow. The capability filter + redactor are shared utilities (`Filter`, `Redact`), not a methods-on-a-struct API, so future tools (Phase 39 `Directory`, Phase 41 `skill_propose`) reuse them by calling.

   The departure from the RFC sketch is recorded here, not silent — future readers chasing the RFC's `SkillProvider` shape land here and see the rationale.

2. **Capability filter is default-deny.** When `CapabilityContext.AllowedTools / AllowedNamespaces / AllowedTags` is empty, a skill with non-empty `Required*` lists is **rejected**. The predecessor's `_skill_is_applicable` documents the same stance ("required must be a subset of allowed"; empty allowed is a strict subset only of empty required) — Phase 38 ports it verbatim. The alternative ("empty allowed = everything passes") would silently leak high-capability skills into low-capability runs the first time an operator forgot to populate the allowed-set; default-deny fails closed, matches CLAUDE.md §6 rule 9 ("identity is mandatory"), and is the only stance that survives an operator-config-bug audit. Skills with empty `Required*` lists for every axis are unconstrained — they neither carry nor demand a capability.

3. **The tiered budgeter uses the chars/4 token estimator, aligned with the §6.5 LLM safety net (D-026).** Two alternatives existed: (a) a tokenizer-backed estimator (tiktoken / Anthropic counter) for precision; (b) chars/4 for simplicity. Phase 38 picks (b) because:
   - **Consistency with the safety net.** RFC §6.5's context-window safety net uses chars/4 as its budget envelope at V1 (D-026); the planner-side budgeter MUST agree on the cost model or the safety net would surface `ErrContextWindowExceeded` on payloads the budgeter accepted. Same estimator → coherent gate.
   - **CGo-free constraint.** Most production tokenizers (tiktoken-go, anthropic-tokenizer) either pull a C library or a sizeable pre-trained vocabulary table; both inflate the binary and either break the CGo-free constraint or burden the cold-start footprint. The chars/4 envelope is byte-counting — zero binary cost, deterministic, well-understood industry low-precision heuristic.
   - **Swappable point.** The estimator lives in one function (`tokensFor` → `charsEstimate`); a post-V1 swap-in via a tokenizer interface is a one-package change. We cross that bridge when an operator surfaces a real over-budget bug; until then, the safety net's chars/4 is the cost authority.

   The budgeter's ladder (full → drop optional → cap steps to 3 → `ErrSkillTooLarge`) ports brief 04 §4.5 verbatim. Step 4 fails loud per CLAUDE.md §5 — no silent degradation; the planner sees `ErrSkillTooLarge` wrapped and can either reformulate via LLM retry feedback or shrink its `MaxTokens` and retry.

The `CapabilityContext` value is a value-type carried on the args of all three Tools; it is never mutated in-flight and is safe to share across N goroutines (D-025). The Phase 38 helpers (`Filter`, `Redact`, `Fit`) are pure functions over value inputs — no shared state, no closures over per-run data. Phase 39's `Directory(cfg)` will reuse `Filter` + `Redact` directly; Phase 41's `skill_propose(persist=true)` will reuse the validator path on the input draft.

## D-051 — Phase 45 ReAct planner: JSON-only action format with `_finish` reserved tool name; single-tool-call-per-step (multi-action salvage reduced to first); `MaxSteps` circuit breaker + `planner.max_steps_exceeded` fail-loudly emit; WakePush declaration ships ahead of the SpawnTask emission path; SpawnTask / AwaitTask / RequestPause emission deferred to later phases

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.2, RFC §3.2, `docs/plans/phase-45-react-planner.md`, `internal/planner/react/react.go` (`ReActPlanner`, `FinishToolName`, `DefaultMaxSteps`, `DefaultSystemPrompt`, the six functional options, `Next`, `WakeMode`, `mapDecision`, `translateFinishCall`, `reduceToSingleAction`, `maxStepsExceeded`, `emitMaxStepsExceeded`), `internal/planner/react/prompt.go` (`PromptBuilder` interface + `defaultBuilder`), `internal/planner/events.go` (`EventTypePlannerMaxStepsExceeded`, `MaxStepsExceededPayload`), `internal/planner/conformance/conformance.go` (`Harness.RunContextFactory` extension), brief 02 §2 + §4 + §5 + §6 + §7, brief 07 §2 + §3 + §5 + §10.

**Why:** Phase 45 lands Harbor's first concrete `Planner` implementation — the LLM-driven ReAct step loop that bridges Phase 32's `LLMClient`, Phase 43's `Trajectory`, Phase 44's `RepairLoop`, and Phase 42's `Planner` seam. Five design calls warrant a settled entry.

1. **JSON-only action format with `_finish` as a reserved prompt-time tool name — NOT a magic-string opcode in the `Decision` sum.** Brief 02 §2 sketches the LLM emitting one of six Decision shapes (`CallTool` / `CallParallel` / `SpawnTask` / `AwaitTask` / `RequestPause` / `Finish`); brief 07 §3 catalogues the predecessor's parser stack and §5 documents the assistant/user-rendered observation shape. Phase 45 narrows the V1 prompt-emission surface to exactly two envelopes:
   - `{"tool": "<name>", "args": {...}, "reasoning": "..."}` — a tool call.
   - `{"tool": "_finish", "args": {"answer": "..."}, "reasoning": "..."}` — completion.

   The reserved `_finish` name is intercepted by the planner BEFORE it returns the `Decision`. `react.translateFinishCall` translates the parsed `CallTool{Tool: "_finish"}` into `planner.Finish{Reason: planner.FinishGoal, Payload: <args.answer>}` — the `Decision` sum stays sealed; the planner contract surfaces only the typed `Finish` shape. The predecessor's "magic strings as `next_node`" anti-pattern (D-047) is explicitly rejected: `_finish` lives in the LLM-prompt convention, NOT in the planner-internal Decision opcodes. The leading underscore is a documented hygiene convention; future runtime catalog registration MAY reject `_`-prefixed tool names to make the collision impossible. The integration with Phase 44's `repair.RepairLoop` flows naturally — the loop returns a `CallTool` with the reserved name; the planner's `mapDecision` switch detects and translates BEFORE the runtime executor would dispatch the reserved name as a real tool.

2. **Single-tool-call-per-step semantics with multi-action salvage reduced to the first action.** RFC §6.2 + the Phase 45 master-plan detail block: "LLM call loop, JSON-only action format, tool selection, completion detection, single tool call per step. No parallel, no schema repair beyond a single retry." Phase 44's `RepairLoop` ships multi-action salvage as a `CallParallel` (D-050 — when the parser returns >1 well-formed `CallTool` and every one validates, the loop promotes to `CallParallel{JoinAll}`). Phase 45 overrides this at the planner concrete level: `react.reduceToSingleAction` collapses a `CallParallel` from the loop to its first `CallTool`. The rest are dropped — V1 minimum viable per the master-plan detail block. Three rationales:
   - **No parallel executor exists yet.** Phase 47 ships `CallParallel` execution (`Deps: 45, 14`); returning `CallParallel` from Phase 45 would prematurely commit to a runtime dispatch path with no executor. The error would surface as `planner.ErrInvalidDecision` at runtime dispatch time, but the planner's contract is to return a Decision the runtime CAN execute.
   - **Unwind point is one method.** `reduceToSingleAction` is the entire override surface. Phase 47 deletes the override; the rest of the planner is unchanged. The brief 02 §6 "queue the additional read-only tool calls for sequential execution without another LLM hop" promise revisits at Phase 47 — until then, the dropped actions ARE NOT surfaced as fallback context to the next prompt (a forwarding-the-rejected-actions path would have no test coverage until Phase 47 lands).
   - **Special case for the `_finish` first branch.** When the first branch of a multi-action salvage is the reserved `_finish` tool, the reduction must still translate to a `Finish` Decision (the completion semantics MUST NOT change with the reduction). The unit test `TestNext_ParallelWithFinishFirstStillFinishes` pins this.

   Brief 02 §6 lists multi-action salvage as the Phase 44 default; Phase 45 departs at the planner concrete level (not at the loop level — the loop is reused as-is per §13 two-parallel-implementations ban).

3. **`MaxSteps` circuit breaker as planner-side defence in depth — accompanied by the `planner.max_steps_exceeded` fail-loudly emit.** Brief 02 §2 puts `MaxSteps` / `HopBudget` at runtime level only; RFC §6.2's `RunContext.Budget.HopBudget` is the authoritative runtime gate. Phase 45 ALSO ships a planner-side `WithMaxSteps` functional option (default 12) as a circuit breaker against a buggy LLM mock that never returns `_finish` AND a runtime that hasn't yet wired the hop-budget enforcement (Phase 47+). When `len(rc.Trajectory.Steps) >= MaxSteps` at the start of `Next`, the planner:
   - Emits `planner.max_steps_exceeded` (registered in `internal/planner/events.go` alongside `planner.repair_exhausted`; typed `MaxStepsExceededPayload` SafePayload carries `Identity`, `MaxSteps`, `StepsObserved`, `LastTool`, `OccurredAt`).
   - Returns `Finish{Reason: planner.FinishNoPath, Metadata: {"max_steps_exceeded": true, "max_steps": <cap>, "steps_observed": <count>, "last_tool": <name>, "run_id": <runID>, "via": "react.maxStepsExceeded"}}`.
   - Does NOT call the LLM (the breaker fires BEFORE any LLM call — a runaway must not burn additional completions).

   The emit is the load-bearing observability surface that makes the breaker NOT silent (§13 silent-degradation ban). The same fail-loudly shape as Phase 44's `planner.repair_exhausted` — different graceful-failure source (repair-loop exhaustion vs. planner-side step cap), same observability shape. When Phase 47's runtime hop-budget enforcement lands, `MaxSteps` becomes a redundant defence in depth (preferred over a load-bearing single gate). The runtime's hop budget remains the authoritative gate; the planner's `MaxSteps` is the secondary one.

4. **WakePush declaration (D-032) ships at Phase 45 ahead of the SpawnTask emission path.** Phase 45's master-plan detail block: "ReAct ships the `push` wake mode (D-032): a non-retain-turn `SpawnTask` returns control to the runtime; the runtime registers the planner against `tasks.WatchGroup`; on `GroupCompletion` the runtime re-invokes `Planner.Next` with the resolved `MemberOutcome` slice surfaced through `RunContext`." Phase 45's `ReActPlanner` implements `planner.WakeAware` returning `planner.WakePush`; the conformance pack's `WakeMode_Declared` subtest asserts `planner.ResolveWakeMode(reactPlanner) == planner.WakePush`. The SpawnTask emission PATH itself is deferred to a later concrete-planner upgrade — the V1 prompt schema is intentionally narrow (only `CallTool` / `_finish`); SpawnTask emission would need additional prompt-engineering surface to describe background tasks to the LLM, which is out of scope for "minimum viable." The WakePush declaration is still load-bearing: it binds ReAct to the conformance pack's wake-mode-round-trip subtest (Phase 49) so that when SpawnTask emission lands, the binding is already in place.

5. **Phase 45 V1 deferrals: SpawnTask / AwaitTask / RequestPause emission, multi-action fallback-context forwarding, runtime loop, trajectory compression.** The master-plan detail block reads "minimum viable"; Phase 45 ships exactly the surface the spec names. Deferrals:
   - **SpawnTask / AwaitTask emission:** the prompt schema doesn't describe background tasks. A later planner upgrade (or a separate concrete) extends the schema; Phase 45 surfaces only `CallTool` / `_finish` to the LLM.
   - **RequestPause emission:** Phase 50 ships the unified pause/resume primitive; until then, there's no `pauseresume.Coordinator` for `RequestPause` to dispatch into. Phase 45 observes `rc.Control.PauseRequested` from incoming steering but does NOT emit RequestPause itself.
   - **Multi-action fallback-context forwarding:** the rejected actions in `reduceToSingleAction` are dropped at V1 (no forwarding to the next prompt). Phase 47 will revisit when the parallel executor exists.
   - **Runtime loop / multi-step orchestration:** Phase 45 ships `Next(ctx, rc) (Decision, error)` — ONE step. The runtime executor that calls Next in a loop, executes Decisions, and threads observations back into the next prompt lands in the planner-runtime wiring phases (Phase 47+).
   - **Trajectory compression / summariser:** the prompt builder consumes `Trajectory.Summary` when set (the read path is shipped); the summariser that populates `Trajectory.Summary` lands in Phase 46.

   The deferrals are recorded here, not silent — future readers chasing the planner concrete's full surface land in this entry first.

Additionally, Phase 45 extends `internal/planner/conformance/conformance.Harness` with an optional `RunContextFactory` field so the Sanity scenario receives a populated identity quadruple. The Phase 42 harness skeleton's Sanity subtest passed a zero `RunContext`; the stub `finish.Planner` accepted it because that stub does NOT enforce identity. Phase 45's planner enforces identity (§6 rule 9 + D-001) and would otherwise fail the Sanity scenario. The harness extension is backward-compatible (nil `RunContextFactory` falls back to the zero `RunContext` for the stub).

The `internal/planner/react/d025_test.go` ships the N=128 concurrent-reuse stress: one shared `*ReActPlanner` instance, per-goroutine identity quadruples + ctxes, per-goroutine LLM stubs returning `_finish` envelopes whose `args.answer` carries the run's `RunID`. The terminal `Finish.Payload` is asserted to match the goroutine's `RunID` (no identity bleed); pre-cancelled ctxes on i%5==0 return ctx.Err() (no cancellation cross-talk); the goroutine baseline is restored within 500ms of WaitGroup join (no leak). The shared planner's `StepsTaken()` atomic counter is asserted to match the expected non-cancelled count, proving the per-call mutation is correctly atomic.

The §13 import-graph contract is preserved by construction — `internal/planner/react/` imports only `internal/llm`, `internal/planner`, `internal/planner/repair`, `internal/events`, `internal/tools`, and stdlib packages. No `internal/runtime/...` imports; the Phase 42 lint test (`internal/planner/conformance/importgraph_test.go`) covers the new package by construction (it walks the entire planner subtree). The Phase 45 smoke script asserts the same via grep at every preflight gate.

---

## D-053 — Phase 40 Skills.md importer: byte-stable round-trip via raw-frontmatter passthrough and line-based body parsing; attachments as ArtifactRef option (b); fail-closed at every parse failure mode

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-40-skills-importer.md`, `internal/skills/importer/importer.go` (`Importer` interface + `Import` / `Export` / `Close`, `Deps{Store}`, sentinels `ErrMissingFrontmatter` / `ErrMalformedYAML` / `ErrMissingTrigger` / `ErrEmptySteps` / `ErrUnknownSection` / `ErrAttachmentOutsideRoot` / `ErrInvalidAttachmentRef` / `ErrRoundTripDrift` / `ErrImporterClosed`), `internal/skills/importer/parser.go` (`scanFrontmatter`, `parseFrontmatter`, `bodyParse`, `resolveAttachments`, `uploadAttachment`, `classifySection`, `slugify`, `nameFallbackFromHint`, `doImport`), `internal/skills/importer/exporter.go` (`doExport`, `synthesiseFrontmatter`, `desubstituteArtifacts`), `internal/skills/importer/path_safety.go` (`resolveSafePath`, `pathHasPrefix`), `internal/skills/importer/testdata/golden/*.md` + `*.want.json` (5 fixtures), brief 04 §4.7 + §5 + §6.

**Why:** Phase 40 closes the predecessor's per-skill-manual-adaptation gap — the load-bearing Harbor-defining feature (RFC §6.7, brief 04 §1). The byte-stable round-trip `Export(Import(b)) == b` is the tested invariant that distinguishes a working importer from a working-by-coincidence importer. Four design calls warrant a settled entry.

1. **Byte-stable round-trip via raw-frontmatter passthrough — NOT YAML re-emission.** Brief 04 §4.7 step 1 says "CommonMark-only parser"; step 5 says "round-trip byte-stable." A naive implementation parses YAML into a struct, parses Markdown into an AST, and re-emits both — which **never** survives the round-trip because (a) every YAML emitter has its own key-ordering / quoting / spacing convention, (b) every CommonMark AST loses some source-side fidelity (heading underline style, list bullet character, blank-line-between-paragraphs count). Phase 40 picks a different shape:
   - **Frontmatter**: the importer captures the raw bytes between the `---` fences VERBATIM via `scanFrontmatter`. The parsed `frontmatterFields` struct is used for value extraction (validation, slugified-name fallback, Skill struct population); the raw bytes are stashed in `Skill.Extra["_importer.frontmatter_raw"]` for Export. `doExport` reads the raw bytes back and emits them between fresh `---` fences. Authors hand-ordering keys (`name` first, `description` second — a common Skills.md convention) round-trip byte-stable.
   - **Body**: the importer ships a line-based deterministic parser (`bodyParse`) — strictly stricter than CommonMark, but deterministic by construction. Section headings (`## Steps`, `## Preconditions`, `## Failure modes`) are accepted with case + plural + trailing-colon variations on the parse side; Export emits the canonical heading (`canonicalHeading`). A source with `## steps` parses correctly but does NOT round-trip byte-stable — the invariant gates canonical sources only. The golden corpus uses canonical headings throughout.
   - **List items**: one line per item, prefix `-␣` (dash-space) required. The parser rejects lazy-continuation list items (CommonMark allows them; Skills.md is stricter). Blank lines inside a section are tolerated as separators; non-list-item prose inside a section is rejected via `ErrUnknownSection`.

   The departure from "CommonMark-only parser" is recorded in the plan's "Findings I'm departing from" section. Two reasons: (a) full CommonMark parsers (e.g. `goldmark`) ship AST-rendering only, not AST-to-source emission, so a round-trip through one would still need to carry the original source text and re-emit from it — which is what the line-based parser does directly; (b) adding a new dependency for a single use-case violates the CLAUDE.md §13 forbidden-practices section on heavy frameworks. The line-based parser uses only stdlib + the existing `goccy/go-yaml` (already used by `internal/config/loader.go`).

2. **Attachments resolve to `artifacts.ArtifactRef` (option (b) per RFC §6.7).** Brief 04 §5 surfaced three options for inline `![alt](path)` references: (a) inline at import (simple, blows up the Skill row); (b) store as artifact references (clean but couples to artifact subsystem); (c) keep filesystem-backed and re-resolve at injection (fast but breaks once skills move between machines). RFC §6.7 settled on (b) — Phase 40 implements it:
   - On Import, `resolveAttachments` walks the description + every section list item via `imageRefRegexp`. For each `![alt](path)` reference: read the file under `ImportSource.AllowedRoot` (path-safety guarded — see point 3), upload via `Deps.Store.PutBytes(ctx, src.Scope, data, {Namespace: "skills-importer"})`, replace the path in the body with `artifact://<ArtifactRef.ID>`. The mapping (Path → Ref) is captured in `ImportArtifacts.PathToRef`.
   - On Export, `desubstituteArtifacts` walks the body for `artifact://<ID>` markers and substitutes each ID back to its source-side path verbatim via the reverse lookup. A dangling ID (not in `PathToRef`) returns wrapped `ErrInvalidAttachmentRef` — Export never silently emits a broken reference.
   - **URL / data:URI refs** (`http://`, `https://`, `data:`, `artifact://`) are kept verbatim and NOT uploaded — they don't resolve to filesystem paths; the importer stays offline at V1 (no network calls).
   - **Duplicate paths** in one source return `ErrInvalidAttachmentRef` at Import time. Duplicates would break Export's injectivity (one path → many refs → the reverse mapping is ambiguous) and are an authoring smell — fail-closed at parse time.
   - **`ArtifactScope`** is caller-supplied via `ImportSource.Scope`. The importer does NOT synthesise the scope itself — callers (Phase 60+ upload handlers) thread the identity quadruple plus the import-task ID through. The convention (documented in the plan, not enforced by the importer) is `TaskID = "import:" + sha256(src)[:12]` so all attachments of one Skills.md file cluster under a stable task-shaped key.

3. **Path-traversal protection at `path_safety.go` (CLAUDE.md §7 #5).** Every relative attachment path is resolved via:
   - **`filepath.IsAbs` rejection** — Skills.md is path-relative-to-source; absolute paths are rejected with wrapped `ErrAttachmentOutsideRoot`.
   - **Empty path / empty AllowedRoot rejection** — the operator must declare a safe root; empty fields are rejected to fail closed.
   - **`filepath.Clean` + `pathHasPrefix(joined, canonicalRoot)` lexical check** — the standard traversal guard. The `pathHasPrefix` helper avoids the `/a` matching `/abc` false-positive by appending the OS separator to the root before the prefix check.
   - **`filepath.EvalSymlinks` symlink check** — when the path exists, both the joined path and the canonical root are evaluated for symlinks; the prefix check is repeated on the evaluated paths. This blocks the `attachments/link -> ../../outside.txt` escape. When the path does NOT exist (the caller is probing — currently not a path the importer takes, but defended for future read-before-write callers), the symlink-eval step is skipped and the lexical check carries.

   The helper is the canonical path-safety guard for the skills subsystem; future skills-side callers (Phase 41 generator if it persists attachments, etc.) reuse it.

4. **Fail-closed at every parse failure mode — no lenient flag at V1 (CLAUDE.md §13 silent-degradation ban).** The exhaustive failure-mode set:
   - **`ErrMissingFrontmatter`**: source does not begin with `---\n`. Empty file lands here too.
   - **`ErrMalformedYAML`**: opening fence found but closing fence missing, or YAML parser failed.
   - **`ErrMissingTrigger`** (wraps `skills.ErrInvalidSkill`): frontmatter parsed but `trigger:` empty after trim. The Phase 37 validator pinned `trigger` as the planner-visible match cue (brief 04 §4.7 step 4); empty trigger is a hard reject.
   - **`ErrEmptySteps`** (wraps `skills.ErrInvalidSkill`): body parsed but `## Steps` absent or had zero list items. Same Phase 37 validator rule.
   - **`ErrUnknownSection`**: body contained a `## Heading` outside the canonical set, or a duplicate section, or non-list-item prose inside a section. A lenient flag that accepted unknown sections would silently drop content (the planner would see only the canonical fields); fail-closed avoids surprise.
   - **`ErrMalformedYAML`** (also covers): YAML keys that fail to decode into the typed `frontmatterFields` struct.
   - **`ErrAttachmentOutsideRoot`**: path-safety rejection (see point 3).
   - **`ErrInvalidAttachmentRef`**: duplicate attachment path at Import, OR dangling artifact:// reference at Export.
   - **`ErrRoundTripDrift`**: reserved for tests that explicitly assert byte-stable round-trip; the importer does not emit it from production code.
   - **`ErrImporterClosed`**: any method called after `Close`.

   The set is exhaustive — every failure mode has a typed sentinel that callers compare via `errors.Is`. There is no silent-degradation path; every parse failure surfaces with a wrapped error and a `%v` context string naming the offending input.

Additionally, Phase 40 ships:

- **N=128 D-025 concurrent-reuse test.** One shared `*importerImpl` instance; per-goroutine distinct in-memory Skills.md payloads (the `Name` field encodes `idx` so cross-goroutine bleed surfaces as a name-mismatch); pre-cancelled ctxes on `i%5==0` return `ctx.Err()` without affecting siblings; goroutine baseline restored within 500ms of `WaitGroup.Wait`. Under `-race`. The Importer holds no per-call mutable state on itself — `closed` is an `atomic.Bool`; the injected `ArtifactStore` is D-025 safe per Phase 17's conformance suite.

- **5-fixture golden corpus** under `internal/skills/importer/testdata/golden/`: `minimal.md` (trigger + steps only), `full.md` (every section + every frontmatter field), `preconditions-only.md`, `failure-modes-only.md`, `with-attachments.md`. Each fixture ships a `.want.json` mirror that the importer's `Skill` output must match deep-equal (lifecycle fields excluded; ContentHash is recomputed at Import via `skills.CanonicalContentHash`). The `with-attachments.want.json` carries a `<REF:attachments/example.txt>` placeholder that the test substitutes with the actual `ArtifactRef.ID` before comparing. Every fixture is asserted byte-stable via `bytes.Equal(src, Export(Import(src)))`.

- **93.8% statement coverage** on `internal/skills/importer` (target 90%). The uncovered branches are defensive (`filepath.Abs` error path on the canonical root, `EvalSymlinks` root-eval error path, the `Export` method's closed-state branch when ctx.Err() also fires — race-window edge case). Not material to the load-bearing surface.

- **Phase 37 hand-off via `Skill.Extra`**: the raw frontmatter bytes and the source-hash are stashed in `Skill.Extra["_importer.frontmatter_raw"]` and `Skill.Extra["_importer.source_sha256"]`. The Phase 37 `CanonicalContentHash` includes `Extra` via its key-sorted text rendering, so changes to the raw frontmatter (even when the parsed fields are identical) produce a different ContentHash — exactly the LWW gate the Phase 37 conflict policy needs. The hash exclusion of Origin / OriginRef / Scope (D-046) is preserved — a Skills.md re-imported via a different `OriginRef` (different pack version) still hashes identically when the content is the same.

The `internal/skills/importer/concurrent_test.go` ships the N=128 stress; `internal/skills/importer/path_safety_test.go` ships the 6-entry path-safety rejection table + the symlink-escape test; `internal/skills/importer/negative_test.go` ships the 10 negative cases; `internal/skills/importer/importer_test.go` ships the golden corpus assertions; `internal/skills/importer/attachments_test.go` wires the real `inmem.ArtifactStore` through the seam and asserts round-trip + duplicate-rejection + URL-passthrough + close-survival. The Phase 40 smoke script (`scripts/smoke/phase-40.sh`) asserts the test surface passes under `-race` AND the golden corpus directory is non-empty (the round-trip invariant has nothing to assert against without fixtures).

---

## D-054 — Phase 41 skill generator: `skill_propose(persist=true)` with conflict-policy precedence (PackImport-protected; Generated→Generated content-hash-gated LWW); audit-mandatory with persist rollback on emit failure; default `Scope=project`; `Promote` is a Go-level API not a planner tool

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-41-skill-generator.md`, `internal/skills/generator/generator.go` (`Register`, `Propose`, `Promote`, `SkillDraft`, `SkillReceipt`, `ProposeResult`, `ErrSkillConflict`, `ErrSkillConflictSentinel`, `ToolNameSkillPropose`, `buildSkillFromDraft`), `internal/skills/generator/events.go` (`SkillProposedPayload`), `internal/skills/generator/audit.go` (`redactExcerpt`, `emitProposed`, `auditExcerptCap`), `internal/skills/events.go` (`EventTypeSkillProposed`), `internal/skills/skills.go` (`ScopeSession`), brief 04 §4.8 + §5 + §6.

**Why:** Phase 41 closes the predecessor's "draft generator can't save" gap — Harbor's runtime persists generated skills, and every persist emits a mandatory audit event. Four design calls warrant a settled entry.

1. **Conflict policy precedence: PackImport-protected first; Generated→Generated content-hash-gated LWW second; insert otherwise.** The policy is the load-bearing rule from RFC §6.7 + brief 04 §4.8 ("refuse to overwrite a `Origin=PackImport` skill with the same `name`. For `Origin=Generated → Origin=Generated`, last-write-wins gated by `content_hash` change"). Phase 41 centralizes the precedence in `generator.Propose`:
   - Probe via `SkillStore.Get` BEFORE the upsert. If the existing row is `Origin=PackImport`, refuse with `*ErrSkillConflict{Reason:"pack_import_protected"}` AND emit `skill.proposed` with `Result="rejected"`. The audit emit on rejection is load-bearing — the rejection IS observable on the audit pipeline (matches RFC §6.7's "audit is mandatory" framing extended to refusals).
   - If the existing row is `Origin=Generated` AND `ContentHash` matches the incoming draft's canonical hash, return `Result="idempotent"`. No DB write needed; the audit event still lands so subscribers can correlate the call.
   - Otherwise fall through to `SkillStore.Upsert` — which is either an insert (no existing row) or a LWW overwrite (existing Generated with different hash). The Phase 37 storage layer's `ErrPackOverwriteRefused` is still wrapped defensively at the generator boundary in case a probe-then-upsert race lets a fresh pack row slip in between probe and upsert.

   The order of probes is binding: pack-protection wins over hash-idempotency because a same-content-hash drafted skill against an existing pack row should still be refused (the operator's invariant is that pack rows are inviolable to the generator, regardless of whether the generated draft happens to match the pack's content).

2. **Audit-mandatory with persist rollback on emit failure.** Every `persist=true` call emits a `skill.proposed` event BEFORE returning success. Caller-controlled excerpts (`SkillDraft.Title` / `Trigger`) flow through `audit.Redactor.Redact` BEFORE the payload is built; the bounded post-redactor excerpts land on the typed `SkillProposedPayload` (SafePayload, so the bus does not re-run them through the redactor). The payload also carries `Name`, `Origin`, `OriginRef`, `ContentHash`, `Scope`, `Result`, `Reason`, and `Promotion` — all bounded enumerable strings or hex hashes; no untyped tool arguments in audit payloads (CLAUDE.md §7 rule 7).

   **Audit-emit failure aborts the persist.** Three branches:
   - **Insert / LWW emit failure:** the DB row was committed by `store.Upsert`; on `skill.proposed` emit failure the generator calls `store.Delete(ctx, q, name)` to roll back. The caller's subsequent `Get` returns `ErrSkillNotFound`. The wrapped error names the audit-emit failure as the cause; if the rollback Delete ALSO fails, the wrapped error names both failures. This is the spec's "audit-emit failure elevated to a first-class concern" requirement.
   - **Idempotent emit failure:** no DB write happened; the wrapped error simply surfaces the emit failure. The row stays intact (matches existing Generated content).
   - **Rejection emit failure:** no DB write happened; the wrapped error surfaces the audit-emit failure rather than the `*ErrSkillConflict` so the audit pipeline's drift is the dominant fault.

   The same fail-loud shape applies to `Promote`: per-target audit emit failure rolls back the target's row via `store.Delete(ctx, target, name)`. The strict-fail model — the first failing target aborts the whole call, subsequent targets are NOT attempted — is the simplest semantics matching the storage layer's transactional shape.

3. **Default `Scope=project`.** RFC §6.7's "Generator scope default — Settled" decision + brief 04 Q-4 are honored verbatim: when `SkillDraft.Scope` is empty, `Propose` stamps `ScopeProject` before validation. Three rationales:
   - `Scope=session` (the narrower default) would mean every generated skill stays trapped in the originating session — the predecessor's "draft generator" pattern in even more degraded form. The user-facing promise of `skill_propose` is "the LLM authored a reusable skill"; the default must be reusable.
   - `Scope=tenant` (the broader default) overshares — a skill authored by user A's planner should not auto-leak to user B's projects.
   - `project` is the operator-declared aggregation point at which "this team's planners should see this generated skill" — the right default-with-room-for-explicit-broaden.

   The `Promote` API explicitly handles broadening (session → project, project → tenant): operators or composition code can elevate a skill's visibility after the fact without touching `Propose`.

4. **`Promote` is a Go-level API, not a planner-callable Tool.** Cross-session promotion is an operator concern (who decides which sessions get a generated skill is a policy question, not a planner reasoning question). Surfacing `skill_promote` as a planner tool would expose every running session to the cross-session-write capability — a privilege escalation that would let one user's planner write into another user's session. Phase 41 ships `Promote(ctx, store, deps, src, name, []targets, scope)` as a Go-level function only; the planner-callable catalog tool is `skill_propose` alone. Phase 39's Directory subsystem will layer a more ergonomic promotion surface (e.g. an operator-facing endpoint that takes a project ID and fans out to discovered session siblings) on top of `Promote`'s primitive; Phase 41's API is the minimum-viable seam.

   The cross-session no-leak invariant is testable end-to-end: identity A persists `Scope=session` → identity B sees nothing via `skill_search` AND direct `store.Get`. Identity A calls `Promote(idA, name, []{idB}, ScopeProject)` → identity B sees the skill via both surfaces. The integration test `TestIntegration_CrossSessionPromotion_AgainstLocalDB` exercises this exactly. CLAUDE.md §6 rule 10: cross-session isolation tests are mandatory.

Additionally, Phase 41 adds `ScopeSession` to the `skills.Scope` enumeration (Phase 37 declared only `Project | Tenant | Global`; the session-scope marker was missing). The validator at `skills.Skill.Validate` accepts the new value; the `localdb` driver's existing identity filter already enforces session-only visibility for `Scope=session` rows (the storage layer's `WHERE tenant = ? AND user = ? AND session = ?` is unconditional). The `Promote` API rejects `scope=session` as contradictory (a promotion target other than the source session at session scope is meaningless).

The `internal/skills/generator/concurrent_test.go` ships the D-025 N=128 concurrent-reuse stress: per-goroutine identity quadruples + distinct skill names against ONE shared catalog. The identity-bleed detector asserts each receipt's `Name` + `OriginRef` reflect the calling goroutine's identity (no cross-goroutine state leaks via shared map or closure capture). Pre-cancelled ctxes on i%5==0 surface either `context.Canceled` or graceful exit (no cancellation cross-talk); the goroutine baseline is restored within 500ms of WaitGroup join (no leak). The companion `TestConcurrent_SameNameResolvesDeterministically` proves that 16 concurrent writers proposing the SAME `(identity, name)` resolve to exactly one persisted state with the remaining 15 reporting `idempotent` (their hash matches the first writer's). Coverage: 92.2% on `internal/skills/generator` (target 90%).

---

## D-052 — Phase 39 virtual directory: dual-source pinning (`DirectoryConfig.Pinned` + `Skill.Extra["pinned"]`); pinned partition exempted only from MaxEntries cap when fits, never from capability filter; `IncludeFields` deferred; deterministic `Name ASC` tie-break

**Date:** 2026-05-12
**Status:** Settled
**Where it lives:** RFC §6.7, `docs/plans/phase-39-virtual-directory.md`, `internal/skills/directory.go` (`Directory`, `DirectoryConfig`, `SkillView`, `SelectionPinnedThenRecent`, `SelectionPinnedThenTop`, `NewDirectory`, `View`, `partitionByPinning`, `sortBySelection`, `filterByCapability`, `projectToSkillView`), `internal/skills/capfilter/capfilter.go` (`BuildSet`, `Subset`, `DisallowedNames`, `Replacement`, `Scrub` — the shared capability-filter / scrub primitives, see the correction note below), brief 04 §3 + §4.5 + §4.6 + §6.

**Why:** Phase 39 lands the planner-facing virtual-directory snapshot of the SkillStore. Four design calls warrant a settled entry.

1. **Dual-source pinning: `DirectoryConfig.Pinned` (config-declared name list) PLUS `Skill.Extra["pinned"] == true` (runtime-stamped boolean).** Brief 04 §3 sketches `VirtualDir.Pinned []string` as a static config field; Phase 39 keeps the config-declared list (operator-authored, survives restart) AND honours a runtime-stamped boolean on the skill itself (`Extra["pinned"]`) that a future operator tool / Console action will set. The two channels are OR'd at `partitionByPinning` time — a skill marked pinned by EITHER channel is in the pinned partition. The LocalDB driver's `marshalExtra` / `unmarshalExtra` round-trips `Extra` through JSON unchanged, so no schema change is required at this phase; a future `skill_pin` planner tool can stamp the boolean without touching the storage shape. The dual source means operators can pin via config (declarative, version-controlled) and the runtime can pin via skill update (dynamic, identity-scoped) without two parallel implementations of the same concept (§13).
2. **Pinned skills are exempted ONLY from the MaxEntries cap, NEVER from the capability filter.** Brief 04 §4.5 documents the injection-time concerns (capability filter + redaction + budgeter). The V1 stance: a pinned skill that fails the capability filter under the run's identity is NOT in the View. Reason: if a misconfigured allowed-set could leak a high-capability skill via the pin channel, the pin channel becomes a security bypass. The pin is a *prominence* signal, not a *visibility* signal. The pinned partition is filled in declaration order (the config-declared `Pinned` list first, then `Extra["pinned"]` skills sorted by the selection rule), then the unpinned remainder is filled until `MaxEntries`. When `count(pinned-after-filter) > MaxEntries`, pinned skills truncate to the first `MaxEntries` (in declaration order, then per-selection sort on the `Extra` tail) and no unpinned skill appears. This is the load-bearing invariant `Property_PinnedAlwaysIncluded_WhenFitsBudget` asserts.
3. **`IncludeFields` is deferred.** Brief 04 §3 lists `IncludeFields []string` on `VirtualDir`; Phase 39 always emits the four `SkillView` projection fields (`Name`, `Title`, `Trigger`, `TaskType`). Rationale: the projection is consumer-side; the cost of carrying the four strings per entry is negligible (≤ 200 rows × four strings); a per-call field knob would introduce a hidden-state branch (some callers see Title, some don't) that breaks the SkillView's wire-stability for downstream consumers. If a future caller surfaces a real need to drop a field (e.g. to keep a Console projection under a render budget), the knob lands then with one test per included combination. The deferred knob matches D-048's stance that operator-facing surfaces narrow at V1 to avoid hidden-state branches.
4. **Deterministic ordering: `Name ASC` is the tie-break on both selection rules.** `pinned_then_recent` sorts the unpinned remainder by `UpdatedAt DESC, Name ASC`; `pinned_then_top` by `UseCount DESC, Name ASC`. The tie-break is load-bearing because two skills with the same `UpdatedAt` (or `UseCount`) would otherwise produce a non-deterministic View across calls, breaking the byte-stability promise downstream Console projections rely on. The pinned partition follows the same per-selection sort on its `Extra["pinned"]` tail (after the declaration-order config pins). `MaxEntries` default = 30, range `[1, 200]` per brief 04 §3 verbatim; pinned by the smoke script so a silent change surfaces here.

**Correction (Wave 8 §17.5 checkpoint audit, 2026-05-14).** This entry originally claimed Phase 39 "reuses Phase 38's `tools.Filter` and `tools.Redact` by direct import — no parallel filter / redactor implementation." That was factually wrong about the import mechanics: `internal/skills/tools` imports `internal/skills`, so `internal/skills` (where `directory.go` lives) cannot import `internal/skills/tools` — an import cycle. As shipped, Phase 39 duplicated the subset/scrub logic inline in `directory.go` ("the two implementations MUST stay in lockstep" comments and all) — the exact CLAUDE.md §13 "two parallel implementations of one feature" anti-pattern. The audit closed it for real (per §17.6 — fix the bug where it lives): the subset gate, disallowed-name computation, replacement selection, and word-boundary scrub were extracted into a new stdlib-only leaf package `internal/skills/capfilter`. Both `internal/skills` and `internal/skills/tools` import `capfilter` (no cycle — it depends on neither). The capability-filter *logic* now lives in exactly one place; `tools.Filter` / `tools.Redact` keep their `skills.Skill`-typed signatures and the directory does its own per-`Skill` field plumbing over the shared primitives. The decision (capability filter is integrity-critical, default-deny, pinned skills not exempt) is unchanged — only the false claim about *how* the code is shared is corrected.

The directory is the consumer of the catalog primitives the planner already trusts. Identity-mandatory: every `View` call reads the identity quadruple from ctx (matching `internal/skills/tools/`'s shape), returns wrapped `skills.ErrIdentityRequired` on a missing component, AND emits `skill.identity_rejected` via `skills.EmitIdentityRejected` so the rejection is observable on the bus, not silent (§13).

The `internal/skills/directory_concurrent_test.go` ships the D-025 stress: N=128 goroutines invoking `View` against ONE shared `*Directory`, per-goroutine identity quadruples, per-goroutine expected pin sets. The shared `*Directory` is immutable after `NewDirectory`; per-call state lives in ctx + the `CapabilityContext` value-type input. Property tests (`testing/quick`) on three invariants: pinned-always-included when count ≤ MaxEntries, View length ≤ MaxEntries, identity scoping (a skill scoped to identity A is NEVER in the View of identity B).

---

## D-055 — Phase 46 trajectory summariser: `Summariser` interface + `CompressionRunner` live in `internal/planner/`; `TrajectorySummary` alias on Phase 43's `Summary`; chars/4 estimator mirrors LLM-edge surface; compression replaces step history in prompt builds; ReAct is the in-PR consumer satisfying the §13 primitive-with-consumer rule

**Date:** 2026-05-13
**Status:** Settled
**Where it lives:** RFC §6.2, brief 02 §4, `docs/plans/phase-46-trajectory-summariser.md`, `internal/planner/compression.go` (`Summariser`, `TrajectorySummary`, `TokenEstimator`, `DefaultTokenEstimator`, `CompressionRunner`, `NewCompressionRunner`, `WithTokenEstimator`, `MaybeCompress`, `ErrNilTrajectory`, `ErrEmptySummary`), `internal/planner/events.go` (`EventTypeTrajectoryCompressed`, `EventTypeTrajectoryCompressionFailed`, `TrajectoryCompressedPayload`, `TrajectoryCompressionFailedPayload`), `internal/planner/planner.go` (`Budget.TokenBudget`), `internal/planner/react/prompt.go` (`defaultBuilder.Build` summary-replaces-step-history swap), `internal/planner/react/compression_integration_test.go` (the §13 consumer gate).

**Why:** Phase 46 closes the runtime-side trajectory summariser primitive. Six design calls warrant a settled entry.

1. **`Summariser` interface + `CompressionRunner` live in `internal/planner/` (NOT in `internal/planner/trajectory/`).** The master plan's `Subsystem` column for Phase 46 reads `planner`. The Summariser's signature requires `planner.RunContext`; the trajectory subpackage CANNOT import the planner package without an import cycle (Phase 43's D-049 settled that the planner package imports trajectory via aliases, not the reverse). The compression primitive sits alongside `Decision`, `Planner`, and `RunContext` in the planner package — the same level the rest of Phase 42's load-bearing types sit at. The `TrajectorySummary` type is a `type TrajectorySummary = trajectory.Summary` alias declared at the planner-package level so callers outside the trajectory subpackage use the RFC's canonical name without ambiguity. Underlying struct stays in `internal/planner/trajectory/trajectory.go` (Phase 43's D-049 location); the JSON tag (`"summary"`) is unchanged so wire compatibility is preserved across the pause-record contract (Phase 51's future consumer).

2. **`defaultBuilder.Build` swaps the per-step assistant/user pair loop for the summary block when `rc.Trajectory.Summary != nil`.** Phase 45 shipped the builder reading `Summary` ADDITIVELY (the summary appeared as an extra block alongside step history). Phase 46 departs from that shape: when Summary is non-nil, the per-step loop is SKIPPED entirely; the summary IS the trajectory representation. Brief 02 §4 explicitly says: "The compressed digest **replaces** the raw step history in subsequent prompt builds." Rendering both would double-count tokens and defeat the compression. The Phase 45 additive shape was a forward-compatibility seam against Phase 46 (the master plan called Phase 46 "compression / summariser" and reserved the field); Phase 46 closes the seam by tightening the rendering rule. The existing Phase 45 `TestDefaultBuilder_RendersSummary` test passes (it doesn't pass Summary AND non-empty Steps simultaneously); the new `TestDefaultBuilder_WithSummary_SkipsStepHistory` test pins the Phase 46 contract. Background-task outcomes (the D-032 push-wake seam) still surface as a trailing user turn regardless of compaction — they are the LATEST signal the planner has and must reach it on the next step. A future phase MAY route background outcomes through the summariser; Phase 46 keeps them as a separate trailing turn.

3. **`DefaultTokenEstimator` uses chars/4 over `Trajectory.Serialize` bytes — mirroring `internal/llm/tokens.go::chars4Estimator`.** §13 bans two parallel implementations of the same conceptual feature; the LLM-edge estimator is the canonical chars/4 surface, and the trajectory-compression estimator deliberately mirrors its `len/4 + 1` per-fragment formula. The trajectory is treated as one fragment by the runner since `Serialize` produces the planner-facing JSON projection. The chars/4 algorithm under-counts multimodal content compared to the LLM-edge estimator (which adds 256 tokens per non-text part); trajectories don't typically carry multimodal parts directly in `LLMContext` — heavy content is upstream of the trajectory per the D-026 safety pass — so the simpler walker is sufficient at Phase 46. A future estimator that structurally walks the trajectory (re-using the LLM-edge tokeniser to count multimodal parts at 256 tokens each) is a Phase 47+ refinement; the `TokenEstimator` functional-option seam is the unwind point. Estimator errors propagate verbatim through `MaybeCompress` — a Phase 43 `ErrUnserializable` from `Serialize` is the typical failure mode and is surfaced loudly with the `trajectory.compression_failed` emit carrying `ErrorCode="estimator_error"`.

4. **Fail-loudly contract at the summariser boundary (§13).** A non-nil error from `Summariser.Summarise` propagates verbatim through `CompressionRunner.MaybeCompress`; the runner does NOT fall through to "skip compression and use raw history" — silent degradation is the bug §13 explicitly bans. A `(nil, nil)` return from the summariser is also a contract violation (the implementation MUST return a non-nil summary on success OR a non-nil error); the runner surfaces this as `ErrEmptySummary` so the bug is loud, not silent. Both failure paths emit `trajectory.compression_failed` BEFORE returning, classified by an error-code bucket (`summariser_error` / `empty_summary` / `estimator_error`). The success path emits `trajectory.compressed`. Together the two emits make compression observable in both directions — companion to Phase 44's `planner.repair_exhausted` and Phase 45's `planner.max_steps_exceeded`. Identity is mandatory at the runner boundary (§6 rule 9 + D-001): a partial quadruple returns wrapped `llm.ErrIdentityMissing` — the same sentinel the rest of the runtime uses.

5. **Idempotency short-circuit when `tr.Summary != nil`.** A second `MaybeCompress` call against an already-compressed trajectory returns nil without invoking the summariser. The engine that owns the cadence policy (Phase 47+ planner-runtime stitch) is the layer responsible for clearing `tr.Summary` when re-compaction is needed. Phase 46 ships the V1 idempotency contract; cadence + re-compaction triggers land at the engine wire-up phase. Concretely: the unit test `TestMaybeCompress_AlreadyCompressed_Idempotent` pins the current behaviour; a future engine that decides "after the trajectory grows 2× past the last compression, re-summarise" will clear the field via `tr.Summary = nil` before re-calling `MaybeCompress`. This keeps the runner stateless across calls (per-call inspection of `Trajectory.Summary` is enough) while leaving the cadence seam open for the engine to fill.

6. **ReAct is the in-PR consumer that satisfies CLAUDE.md §13's primitive-with-consumer rule.** A primitive that lands without a concrete that exercises it bit-rots. Phase 46's primitive is the `Summariser` interface + `CompressionRunner`; the in-PR consumer is the Phase 45 ReAct planner via the `prompt.go::defaultBuilder` swap (call sites: `internal/planner/react/prompt.go` → reads `rc.Trajectory.Summary`; `internal/planner/react/compression_integration_test.go` → drives the end-to-end test). The integration test wires real `events.EventBus` + real `CompressionRunner` + real `ReActPlanner`: an over-budget trajectory triggers compression, the planner's next prompt is built from the summary only (zero raw-step assistant turns; the LLM is called exactly once). The failure-mode scenario (`errSummariser`) surfaces `trajectory.compression_failed` on the real bus with the run's identity. Without this consumer the primitive would have no test-time witness that the prompt builder actually reads `Trajectory.Summary` correctly; the integration test IS the gate.

The `internal/planner/compression_concurrent_test.go` ships the D-025 N=128 concurrent-reuse stress: shared `*CompressionRunner`, per-goroutine identity quadruples + per-goroutine trajectories, the `countingSummariser` stamps the goroutine's RunID into the summary's `Note` field for context-bleed detection (no other goroutine's RunID surfaces). Pre-cancelled ctxes on i%5==0 verify cancellation honoring without cross-talk; baseline `runtime.NumGoroutine` restored within 500ms of WaitGroup join. The supplementary `TestCompressionRunner_SharedAcrossGoroutines_NoRaceOnEstimator` exercises the idempotent read path under -race (single shared trajectory with pre-stamped summary; 64 goroutines short-circuit cleanly). The `TestCompressionRunner_EmitClosure_ConcurrentSafe` asserts the emit closure receives every event without drops when N=64 goroutines emit through a shared runner. Coverage targets met: `internal/planner` ≥ 80% (Phase 46 incremental); `internal/planner/react` ≥ 85% (Phase 45 surface preserved; the Phase 46 prompt-builder swap is covered by `TestDefaultBuilder_WithSummary_SkipsStepHistory` + `TestDefaultBuilder_NoSummary_RendersStepHistory` regression guard).

---

## D-057 — Phase 48 deterministic planner: `DecisionTreeStep` abstraction over typed `Decision` returns; `WakePoll` non-blocking `WatchGroup` semantics; iface-validation lens proving `Planner` swappability; §13 primitive-with-consumer closed by in-scenario SpawnTask + AwaitTask emission

**Date:** 2026-05-13
**Status:** Settled
**Where it lives:** RFC §6.2 + RFC §11 Q-6, `docs/plans/phase-48-deterministic-planner.md`, `internal/planner/deterministic/deterministic.go` (`DeterministicPlanner`, `Option`, `NewDeterministicPlanner`, `Next`, `WakeMode`, `WithSteps`, `WithRegistry`, `WithName`), `internal/planner/deterministic/steps.go` (`DecisionTreeStep`, `CallToolStep`, `FinishStep`, `PauseStep`, `SpawnAndAwaitStep`, `WatchGroupStep`), `internal/planner/errors.go` (`ErrIdentityRequired`, `ErrInvalidConfig`, `ErrDeterministicStep`), brief 02 §1 + §2 + §5 + §7, brief 05 §1.

**Why:** Phase 48 lands Harbor's second concrete `Planner` implementation. Four design calls warrant a settled entry.

1. **`DecisionTreeStep` interface — typed step abstraction over a sealed-sum `Decision` return; no magic-string opcodes.** Each step exposes `Decide(ctx, rc) (planner.Decision, bool, error)`. The boolean reports whether the step claimed the call: `true` → walker returns the decision verbatim; `false` → walker advances; non-nil error → walker propagates wrapped `planner.ErrDeterministicStep` (fail-loudly per §13 — no silent skip on error). The predecessor's "magic strings as `next_node`" anti-pattern (brief 02 §2) is rejected: every step returns one of the six sealed-sum `Decision` shapes directly. The interface is exported so operators can implement custom steps; five in-package types ship (`CallToolStep`, `FinishStep`, `PauseStep`, `SpawnAndAwaitStep`, `WatchGroupStep`). A tree that exhausts every step without a claim returns `Finish{NoPath, Metadata["deterministic"]="no_step_matched"}` — fail-loudly per §13 (a silently looping planner is the worst kind of misconfiguration shape).
2. **`WakePoll` semantics — non-blocking receive on `tasks.WatchGroup` from the planner side; emit `AwaitTask` on not-ready.** The deterministic planner declares `planner.WakeAware` returning `planner.WakePoll`. The on-disk realisation lives in `SpawnAndAwaitStep` and `WatchGroupStep`: each `Decide` call performs a `select { case completion := <-ch: ...; default: AwaitTask }` against the channel returned by `tasks.TaskRegistry.WatchGroup`. When the channel hasn't fired, the planner emits `AwaitTask{TaskID: <owner>}` and the runtime sleeps the step until the next deterministic boundary; when it has fired, the operator-supplied `OnResolved([]MemberOutcome)` callback returns the next decision. No LLM, no eager wake — a clean deterministic shape that proves the TaskRegistry's `WatchGroup` surface is mode-neutral (D-032). The registry has no knowledge that a poller is reading its channel non-blockingly; no `WakeMode` field on registry types, no `Supports*` capability protocol.
3. **The deterministic planner is the iface-validation lens — proves CLAUDE.md §1 property 3 ("the Planner is swappable") on disk.** RFC §11 Q-6 settled the second V1 planner concrete as `deterministic` precisely because it exercises a non-LLM `Decision` shape end-to-end: same Runtime, same `Planner` interface, same `RunContext` view, same `Decision` sum — but no LLM, no prompt builder, no retry / downgrade / corrections / safety / governance composition. If the interface were structurally biased toward an LLM-driven concrete, the deterministic planner would surface the bias loudly at construction time. It does not. Phase 48 is the on-disk proof, not a doc claim.
4. **§13 primitive-with-consumer policy — `SpawnTask` + `AwaitTask` emission closes the policy for the deterministic-planner side of the wave.** Phase 42 shipped the `Decision` sum's `SpawnTask` and `AwaitTask` shapes; Phase 20/21 shipped the `TaskRegistry` + `TaskGroup` + `WatchGroup` mechanism. The Phase 48 `SpawnAndAwaitStep`'s scenario test (`spawn_await_scenario_test.go`) wires a real `tasks.TaskRegistry` (in-process driver) + real `events.EventBus` (inmem driver) and asserts the planner emits `SpawnTask` → `AwaitTask` → `CallTool` → `Finish` across four `Next` calls, with the registry's task lifecycle driven through `Spawn` → `SealGroup` → `MarkRunning` → `MarkComplete` between calls. The §13 rule (added to CLAUDE.md via PR #67) is binary: a primitive lands with its first consumer in the same wave. Phase 48 supplies the deterministic-planner side; future planner upgrades (or Phase 47's ReAct emission upgrade) close the ReAct side. Phase 49's conformance pack uses Phase 48's concrete as the second leg of cross-planner round-trip scenarios — the deterministic planner exercises each of `CallTool`, `SpawnTask`, `AwaitTask`, `Finish` in its scenario test so Phase 49 has the cross-planner coverage.

**Identity is mandatory** (§6 rule 9 + D-001). The deterministic planner returns wrapped `planner.ErrIdentityRequired` on a partial quadruple at `Next` entry — defensive in depth alongside the runtime engine's identity propagation. **Fail-loud construction** (§13). `NewDeterministicPlanner` returns wrapped `planner.ErrInvalidConfig` when the configured step set is empty, when any configured step is nil, or when a group-aware step is configured without `WithRegistry`. Configuration errors NEVER surface at `Next` time; the constructor is the boundary.

**Concurrent reuse pinned (D-025).** `DeterministicPlanner` is a reusable artifact: the receiver is read-only after construction; per-run state lives on the stack and in `RunContext`. `SpawnAndAwaitStep` holds an internal `sync.Map` keyed by `(SessionID, StepID)` so per-run spawn-tracking state is safe across N concurrent runs against the shared planner. `internal/planner/deterministic/d025_test.go` pins N=128 concurrent `Next` invocations against one shared instance under `-race` — asserts no races, no identity bleed (each call's `Finish.Metadata["run_id"]` matches the goroutine's RunID), no cancellation cross-talk (pre-cancelled ctx on i%5==0 returns ctx.Err() without affecting siblings), no goroutine leak (baseline `runtime.NumGoroutine` restored within 500ms of WaitGroup join). Coverage: 90.1% on `internal/planner/deterministic` (target 85%).

---

## D-056 — Phase 47 parallel executor + ReAct CallParallel / SpawnTask / AwaitTask emission: three reserved tool names as V1 emission surface; `reduceToSingleAction` deletion timing; `AbsoluteMaxParallel = 50`; JoinSpec enum semantics (JoinAll / JoinFirstSuccess / JoinN); atomic-setup vs in-flight failure handling; §13 primitive-with-consumer compliance

**Date:** 2026-05-13
**Status:** Settled
**Where it lives:** RFC §6.2, `docs/plans/phase-47-parallel-emission.md`, `internal/runtime/parallel/parallel.go` (`Executor`, `Resolver`, `Result`, `New`, `Execute`, `normaliseJoin`, `validateJoin`, `dispatchAll`, `dispatchFirstSuccess`, `dispatchN`, `invokeBranch`), `internal/planner/decision.go` (`JoinN`, `JoinSpec.N`), `internal/planner/errors.go` (`AbsoluteMaxParallel`, `ErrParallelCapExceeded`, `ErrParallelInvalidJoin`, `ErrParallelBranchInvalidArgs`, `ErrParallelPauseUnsupported`), `internal/planner/react/react.go` (`SpawnTaskToolName`, `AwaitTaskToolName`, `translateSpawnCall`, `translateAwaitCall`, `mapDecision`, `DefaultSystemPrompt`), `test/integration/phase47_spawn_await_test.go`, `scripts/smoke/phase-47.sh`.

**Why:** Phase 47 closes three primitive-with-consumer gaps in one PR per CLAUDE.md §13's "shipping a primitive without its first consumer in the same wave" forbidden practice. Six design calls warrant a settled entry.

1. **Three reserved tool names as the V1 emission surface — `_finish` (Phase 45 / D-051), `_spawn_task` and `_await_task` (Phase 47 / D-056).** The reserved-name convention follows D-051: prompt-time strings translated by `mapDecision` to typed Decisions BEFORE return; the Decision sum stays sealed (no "magic string as next_node" anti-pattern). Two design rationales:
   - **Why a reserved-tool convention vs. a top-level JSON envelope `{"decision":"spawn","args":{...}}`.** The reserved-tool shape lets the LLM stay in one prompt-schema mode — it ALWAYS emits a `{"tool":..., "args":..., "reasoning":...}` shape, never switching between "tool envelope" and "decision envelope" mid-conversation. Single-mode prompts compress better in the LLM's representation (fewer competing patterns to navigate) and reduce repair-loop pressure: the parser already handles tool envelopes; spawn/await go through the same path. The downside (the LLM can in principle emit a `_spawn_task` shape that looks identical to a real tool with that name) is mitigated by the leading-underscore convention — future runtime catalog registration MAY reject underscore-prefixed tool names; today the dispatcher would reject any `_`-prefixed tool that wasn't intercepted by `mapDecision` first.
   - **Why fail-loudly on malformed args** (vs. silent emit of the literal `_spawn_task` CallTool the dispatcher would reject anyway). The §13 silent-degradation ban means errors must be explicit. `mapDecision` returning `(Decision, error)` surfaces the translation failure at the planner boundary; the runtime sees a clean error rather than a CallTool-shaped pseudo-decision the catalog cannot dispatch.

2. **`reduceToSingleAction` deletion timing — Phase 47, NOT later.** The Phase 45 plan named the deletion timing explicitly ("the `reduceToSingleAction` method is the unwind point — Phase 47 deletes the override"). The Phase 47 PR honours the hand-off because the §13 "two parallel implementations of the same conceptual feature" rule is active: Phase 44's `RepairLoop` already produces `CallParallel{Join: JoinAll}` when multi-action salvage triggers; the Phase 45 collapse override was a V1 stop-gap. Carrying both shapes (the Phase 44 emission + the Phase 45 collapse) past Phase 47 would mean two parallel implementations of "what happens when the LLM emits multiple actions" — Phase 47 picks the deepening (let the executor dispatch) and deletes the override. The smoke script asserts the absence of the symbol via grep-v as the drift gate.

3. **`AbsoluteMaxParallel = 50` system cap rationale.** RFC §6.2 settled the value at 50. Three rationales:
   - **Defence in depth against a runaway emission.** A buggy LLM emitting 1000 branches must not consume 1000 goroutines + 1000 tool-dispatch budgets. 50 is comfortably above the "I want to parallelise this small fan-out" use case (3-10 branches typical) while staying below "the LLM ran away."
   - **The soft cap is the planner's `PlanningHints.MaxParallel`.** Operators tune the soft cap per session / per tenant; the hard cap is system-wide. Operator-tunable hard cap would re-introduce the "two parallel implementations" smell (a config-driven cap + a code-driven cap); the system cap stays settled.
   - **Defence against a malicious / adversarial LLM emission.** A jailbreak prompt that coerces the LLM into emitting "1000 branches of `delete_everything`" gets rejected at the executor boundary; even if every branch's validator was permissive, the cap fires first. The cap is the last line of defence before goroutine + descriptor multiplication.

4. **JoinSpec enum semantics — JoinAll / JoinFirstSuccess / JoinN.** Three explicit shapes ship; `JoinKeyed` remains a documented future surface but is rejected at dispatch with `ErrParallelInvalidJoin` (the "not implemented at Phase 47" message names the deferral). Per-shape rationales:
   - **JoinAll (the default).** The most common shape: fan out, collect every observation, surface them all back to the planner for the next step. The Phase 44 repair loop's multi-action salvage uses this as its default join.
   - **JoinFirstSuccess.** The "race to first success" shape: the planner emits N alternate tool calls and wants whichever responds first (e.g. three different search providers; whichever finishes first). Cancellation: the executor derives a child ctx; on first success, the child ctx cancels; slow branches that honour ctx exit promptly. Failures do NOT cancel until every branch terminates — a slow success can still arrive after a fast failure.
   - **JoinN.** The "fault-tolerant fan-out" shape: emit N+M branches, wait for N successes, cancel the rest. Setup validates `0 < N ≤ len(Branches)`. JoinN returns successes in COMPLETION order (each Result still carries its original branch Index for the deterministic merge key downstream — the merge ordering is the branch's input position, NOT completion order).

5. **Atomic-setup vs in-flight failure handling.** RFC §6.2's "atomicity contract": atomic setup validation (any branch's invalid args fails the whole call BEFORE execution); in-flight failures land per-branch on `Result.Err`. Two failure modes, two different shapes:
   - **Setup-time failures (atomic):** branch count cap exceeded, JoinSpec malformed, descriptor not registered, args validator rejects — ALL surface as the executor's return error. The slice is nil. NO branch executes. This is the load-bearing "atomicity contract" surface.
   - **In-flight failures (per-branch):** a branch's `desc.Invoke` returns an error; the executor catches it, populates `Result.Err`, surfaces the result alongside the successful peers. The call-level error stays nil for JoinAll (mixed-success-and-failure is a normal observation shape); JoinFirstSuccess and JoinN exhaustion return a joined error wrapping every failure when no branch met the threshold. The distinction prevents the planner from seeing a "whole call failed" when in fact one tool returned a soft error the LLM can incorporate into its next step's reasoning.

6. **§13 primitive-with-consumer policy compliance — three primitives, three consumers in one PR.** CLAUDE.md §13 forbids shipping a primitive without its first consumer in the same wave. Phase 47 closes three gaps in one wave:
   - **Parallel-call executor (the master-plan Phase 47 row's original scope).** Consumer: ReAct emits `CallParallel` (pass-through); Phase 44's repair loop already produces the shape from multi-action salvage. Both ends ship in this PR.
   - **`SpawnTask` Decision shape (shipped Phase 42 without emitter).** Consumer: ReAct's `_spawn_task` reserved tool translation + the integration test's spawn → group → wake → re-entry round-trip.
   - **`AwaitTask` Decision shape (shipped Phase 42 without emitter).** Consumer: ReAct's `_await_task` reserved tool translation.

   The §13 rule explicitly names `SpawnTask` and `AwaitTask` as the pair that MUST land together: "a planner that can spawn a background task but cannot join it produces orphan work the runtime cannot recover." Phase 47's PR bundles them per the binding rule. The unified pause/resume primitive (Phase 50) is the next §13 application of the same rule — it will land with a `RequestPause`-emitting consumer in the same wave.

The `internal/runtime/parallel/concurrent_test.go` ships the D-025 N=128 reuse stress: one shared `*parallel.Executor`, per-goroutine identity quadruples (no bleed), pre-cancelled ctxes on i%17==0 (no cross-talk), goroutine baseline restored within 2s of WaitGroup join (no leak). The Phase 45 `internal/planner/react/d025_test.go` test already covers the upgraded ReAct emission paths transitively (any `Next` call exercises `mapDecision`'s new cases). Coverage: `internal/runtime/parallel` ≥ 85% (master-plan target).

---

## D-058 — Phase 49 planner conformance pack: shared scenario suite both Wave 8 concretes pass; capability-gated subtests; wake-mode round-trip wired against real `tasks.TaskRegistry` + real `events.EventBus` (D-032 binding); Wave 8 wave-end E2E bundled in same PR per §17.5

**Date:** 2026-05-13
**Status:** Settled
**Where it lives:** RFC §6.2, `docs/plans/phase-49-conformance-pack.md`, `internal/planner/conformance/conformance.go` (Phase 42 skeleton scenarios + Phase 49 scenario bodies: `Capability` flags + `ScenarioName` constants + `Harness` extensions: `ScenarioFactory`, `Capabilities`, `TaskRegistryFactory`, `PrebuiltPlannerFactory`; `WakeRoundTripDeps` + `DefaultTaskRegistryFactory` + `DefaultRunContext` + `DefaultReactContentMap` + `SecondStepContent` + `scenarioContentTrim`; per-scenario implementations `runTopPromptsScenario`, `runMalformedLLMScenario`, `runParallelAtomicityScenario`, `runWakeRoundTripScenario` (with push/poll dispatch), `runBudgetAwareScenario`, `runPauseBoundsScenario`, `runSteeringDrainScenario`, `runConcurrentReuseScenario`), `internal/planner/react/conformance_test.go` + `internal/planner/react/conformance_helpers_test.go` (ReAct's full-suite invocation; scripted multi-response LLM for the push wake-round-trip), `internal/planner/deterministic/conformance_test.go` (Deterministic's full-suite invocation; `parallelEmitStep` + `SpawnAndAwaitStep`-based `PrebuiltPlannerFactory`), `test/integration/wave8_test.go` (Wave 8 wave-end E2E — three focused tests covering the push round-trip across the assembled surface, the missing-identity fail-closed scenario, and the N=10 concurrency stress).

**Why:** Phase 49 closes the planner-track wave by filling the Phase 42 conformance harness skeleton AND landing the Wave 8 wave-end E2E in one PR. Three design calls warrant a settled entry.

1. **Capability-gated scenarios: `Capability` flags + `Harness.Capabilities` bitmask let one Run entrypoint drive both LLM-driven and non-LLM concretes without dual-suite drift.** A non-LLM concrete (Deterministic) calling the LLM-only scenario (`MalformedLLM_Salvage`) would either (a) skip silently if we picked permissive defaults — §13's silent-degradation ban catches this — or (b) fail with a `nil LLM client` shape that's a configuration bug, not a planner bug. The capability flags (`CapabilityLLMDriven`, `CapabilityCanPause`, `CapabilityWakeRoundTrip`, `CapabilityHonoursCancelControl`) gate scenarios at the entry point; a scenario whose required capability is absent calls `t.Skip` WITH A REASON — never silently. Phase 49 ships two pre-built capability sets (`CapabilitySetReAct`, `CapabilitySetDeterministic`); future concretes (Plan-Execute, Workflow, Graph, Supervisor) pick the capability set that matches their shape, and the conformance pack scales without modification.

2. **The `WakeMode_RoundTrip` scenario is the LOAD-BEARING D-032 binding — real `TaskRegistry` + real `EventBus`, no mocks at the seam.** RFC §6.2 + master plan Phase 49 detail block ship the wake-mode round-trip as the unmissable scenario: "Failure to wire `tasks.WatchGroup` is the test's failure mode, not silent deadlock." Phase 49 wires the scenario against the production inprocess `tasks.TaskRegistry` driver, the production inmem `events.EventBus` driver, and the production inmem `state.StateStore`. Mocks at this seam would defeat the test's purpose (a mock that always delivers `GroupCompletion` instantly would mask a real wiring bug that delays the delivery; a mock that always blocks would mask a planner that fails to honour the non-blocking receive contract). The harness's `TaskRegistryFactory` field exposes the production-driver factory (`DefaultTaskRegistryFactory`); the harness's `WakeMode` field dispatches the round-trip to push or poll. For push (ReAct): the scenario simulates the runtime engine's role at Phase 60+, spawning the real task and surfacing the `MemberOutcome` through `RunContext.Trajectory.Background`. For poll (Deterministic): the scenario uses `PrebuiltPlannerFactory` to construct the planner WITH the registry bound (via `deterministic.WithRegistry`), then calls `Next` repeatedly — observing the non-blocking receive pattern: SpawnTask → AwaitTask (group open) → resolved Decision (group complete). The §17.4 "no time.Sleep for synchronisation" rule holds: bounded eventually-style waits with 2s deadlines and `runtime.Gosched` yields between retries.

3. **Wave 8 wave-end E2E bundled in same PR per §17.5 — three focused tests covering happy path, failure mode, and concurrency stress.** §17.5 makes the wave-end checkpoint audit + wave-end E2E binding at every wave boundary. The wave-end E2E exercises the same primitives the conformance pack tests, but ACROSS the full assembled surface: Skills (localdb) + Planner (ReAct) + Tools (in-process) + Tasks (inprocess) + Memory (inmem) + LLM (mock) + Events (inmem) + State (inmem). Three tests cover §17.3's mandatory dimensions:
   - **`TestE2E_Wave8_ReactSpawnWakeRoundTrip_AssembledSurface`**: real ReAct planner emits `_spawn_task` against scripted mock LLM → real registry spawns and resolves → planner re-enters with `MemberOutcome` surfaced → emits Finish. Memory captures the turn. Skill store presence on the surface is asserted via an Upsert + Get round-trip.
   - **`TestE2E_Wave8_MissingIdentity_FailsClosed`**: ReAct's identity-mandatory pre-check rejects a `Next` call with no identity in the RunContext quadruple, returning wrapped `llm.ErrIdentityMissing` BEFORE the LLM completion fires. Memory + skill stores also reject missing identity — same fail-loudly contract. The scenario is the §17.3 #3 "at least one failure mode" requirement.
   - **`TestE2E_Wave8_Concurrency_NoCrossTalk`**: N=10 concurrent ReAct runs against ONE shared planner + ONE shared catalog + ONE shared registry + ONE shared memory store. Every 3rd goroutine derives a pre-cancelled ctx (cancellation cross-talk gate); the race detector is the gate for data races. Baseline goroutine count restored on teardown (within a +16 tolerance for driver-retained background workers).

   The §17.6 fix-in-same-PR rule was honoured but no cross-phase bug surfaced — the per-package tests Phase 42-48 land had already covered the seam interactions Phase 49's E2E exercises. The conformance pack DID surface a usability gap (the Phase 42 `Harness` shape needed extension to drive scenario-specific planner configurations); the extension is additive (existing fields preserved verbatim) so no per-concrete test regresses.

**Identity is mandatory** across every scenario (§6 rule 9 + D-001). The conformance pack's `DefaultRunContext` factory stamps a populated quadruple; per-concrete tests build their own factories on the same shape. **§13 import-graph contract** preserved: the conformance package imports `internal/audit`, `internal/config`, `internal/events`, `internal/state`, `internal/tasks`, `internal/identity`, `internal/planner` — NONE of which are `internal/runtime/...`. The Phase 42 `importgraph_test.go` walks the planner subtree and gates the contract; Phase 49 adds no `internal/runtime/...` imports.

**Concurrent reuse pinned (D-025).** The `ConcurrentReuse_D025` scenario in the pack runs N=64 parallel `Next` calls against ONE shared planner from the harness factory; the race detector is the gate; per-goroutine RunID round-trip checks for context bleed. The Wave 8 E2E's concurrency stress (N=10) provides the cross-package complement. **Coverage:** `internal/planner/conformance` ≥ 80% (Phase 49 target). The pack's coverage is asserted by Run-against-both-concretes: each concrete's test exercises every non-skipped scenario, and the skip paths are exercised by the capability-gating fallthrough.

---

## D-059 — Agent identity model: `agent_id` is a runtime *registration* identity, NOT an isolation principal; the isolation tuple stays `(tenant, user, session, run)`; agents carry a three-ID model (`agent_id` / `incarnation` / `version_hash`)

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §6.16 (Agent Registry), CLAUDE.md / AGENTS.md §6 (clarifying note), `docs/plans/phase-53a-agent-registry.md`, `docs/glossary.md` (`agent_id`, `incarnation`, `version_hash`, registration identity).

**Why:** During Console information-architecture planning the question "is `agent_id` a fourth element of the identity tuple?" surfaced repeatedly and threatened to leak implicit assumptions into every identity-touching phase. It is settled here so it does not get re-litigated.

1. **`agent_id` is a registration identity, not an isolation principal.** Harbor's isolation boundary is and stays the tuple `(tenant, user, session, run)` (RFC §4, §6 rules, D-001). An agent is a runtime *entity* — it has a planner, tool bindings, memory bindings, policies, health — but it runs *within* `(tenant, user, session)`; it does not widen the isolation boundary. Memory drivers, state drivers, event subscribers continue to scope by the tuple, never by `agent_id`. This dissolves the recurring confusion: there are two orthogonal concepts — "agent as a registered, runtime-tracked entity with a stable ID and lifecycle" (this decision) and "agent as an isolation boundary" (explicitly rejected for V1).
2. **Agents carry a three-ID model.** `agent_id` — stable, "which logical agent," minted once at first registration, persisted, rehydrated on restart. `incarnation` — ephemeral, "which boot of it," bumps on every process start. `version_hash` — content-derived, "which configuration," a deterministic hash over (prompt set, tool set + schemas, planner config, model policy); bumps **only** when configuration content changes. The three answer different questions and must not be collapsed: a restart with no config change yields the same `agent_id` + same `version_hash` + a new `incarnation`; a restart after a prompt edit bumps both `incarnation` and `version_hash`.
3. **`version_hash` is load-bearing for the post-V1 Evaluations / version-control program (D-064).** If every agent carries a config hash from V1, Evaluations can later attribute success-rate changes to a specific configuration version with zero retrofit. It is cheap to compute at registration and is the free precursor to prompt/tool evolution work.
4. **Consumers.** Phase 30 (tool-side OAuth) keys agent-bound tokens by the registration `agent_id` — never by an isolation-tuple element. The Console Agents page (RFC §7, D-062) renders the three-ID model. See [[D-060]] for the subsystem that owns minting and persistence, and [[D-061]] for the Console-DB boundary.

---

## D-060 — Agent Registry is an in-process, per-runtime-instance, StateStore-backed subsystem; it covers both creation cases (locally-hosted + connect-to-remote); restart rehydrates (`restart ≠ recreate`)

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §6.16, CLAUDE.md / AGENTS.md §3 (`internal/runtime/registry/` layout entry), `docs/plans/phase-53a-agent-registry.md`, master plan phase 53a.

**Why:** "How can the registry mint an `agent_id` if Harbor can be used by anybody?" — Harbor is a Go-native SDK + single static binary; there is no central Harbor service, and there must not be one. The minting and ownership model is settled here.

1. **The registry is not a central authority — it is an in-process subsystem inside each `harbor` instance.** Every `harbor` process (or every embedding of the library) maintains its own `registry.AgentRegistry`, persisted via *that instance's* configured StateStore driver (in-mem / SQLite / Postgres, §4.4 seam). This is the same shape Harbor already uses for `(tenant, user, session)`: Harbor never mints identity globally — it receives identity from the operator's auth boundary and scopes state locally. `agent_id` never needs to be globally unique; it only needs to be unique within the runtime instance that issued it, which is collision-free by construction (ULID/UUID).
2. **Two creation cases, both landing in the registry.** *Locally-hosted agent* — the runtime instance is running the agent; it mints a local `agent_id`. *Connect-to-remote agent* — the agent runs in someone else's Harbor (or is any A2A-speaking peer); the local runtime assigns a **handle** (`agent_id` local to this instance), and the canonical identity of the remote agent is its A2A AgentCard, owned by the remote operator. See [[D-061]] — neither case puts the agent list in a Console DB.
3. **`restart` rehydrates; `restart ≠ recreate`.** With a durable StateStore driver, a process restart rehydrates the registry and the agent comes back with the *same* `agent_id` (a stable fleet view depends on this). The in-mem driver loses the registry on restart and is documented as dev-only — the "id changes on restart" behaviour is a dev-mode artifact, not the intended fleet posture. Teardown-and-recreate is distinct from restart: recreate genuinely mints a fresh `agent_id` because it is a new logical entity; restart keeps the StateStore record.
4. **The registry emits `agent.*` events** (`agent.registered`, `agent.restarted`, `agent.health`, `agent.drained`, `agent.deregistered`) so the Console renders runtime state. See [[D-059]] for the three-ID model the registry owns and [[D-066]] for the fleet-control privilege tier.

---

## D-061 — A Console DB holds Console-local state only; it is never a shadow source of truth for runtime entities

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §7, CLAUDE.md / AGENTS.md §13 (forbidden practice), `docs/glossary.md` (Console DB).

**Why:** The instinct to track "which agents exist" in a Console-side database is exactly how the predecessor's Console drifted into re-implementing runtime APIs. The boundary is settled before any Console phase is authored.

1. **If a Console DB exists, it holds Console-local state only** — saved views, dashboard layouts, per-operator preferences, annotations. It must never be the source of truth for runtime entities (agents, sessions, tasks, tools, events, artifacts). Those live in the Runtime and reach the Console exclusively through the Protocol's canonical events / state snapshots / control commands.
2. **Rationale: a Console DB as a shadow source of truth breaks the "Console is a Protocol client" rule** (RFC §5, §7, CLAUDE.md §4.5). If the Console DB owned the agent list, a third-party Console would have a *different* agent list, and the Agents page would be a standalone app rather than a runtime lens. The Agent Registry ([[D-060]]) is the runtime-side owner; the Console renders it.
3. **A runtime-side control-plane client allowlist is the legitimate inverse** and is a separate concern from a Console DB — see [[D-066]].

---

## D-062 — Harbor Console is a 14-page observability + control plane organized as runtime lenses; `Live Runtime ≠ Sessions`; `Agents ≠ chatbots`; no Console page phase ships without its feeding Protocol-surface phase

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §7 (expanded), `docs/research/11-console-feature-surface.md`, `docs/rfc/assets/console-agents-page.png`, master plan README.md (Console-wave re-decomposition note), CLAUDE.md / AGENTS.md §13 (forbidden practice — Console page without Protocol surface).

**Why:** The Console is not "the Playground plus widgets" — it is a full control/observability plane. Its information architecture is settled so the (currently under-scoped) phases 72–75 can be re-decomposed against a fixed target.

1. **Fourteen pages, five clusters, all runtime lenses.** Runtime (Overview, Live Runtime); Execution (Sessions, Tasks, Agents, Tools, Events, Background Jobs); Resources (Flows, Memory, MCP Connections, Artifacts); Evaluation (Evaluations); Settings. Every page is a *projection* over `state snapshots + realtime events + control commands` — never a standalone app feature. The canonical Agents-page mockup is `docs/rfc/assets/console-agents-page.png`.
2. **`Live Runtime ≠ Sessions`.** Live Runtime is the present-tense interactive workbench (initiate / observe / steer / debug a live execution — the spiritual replacement of the predecessor's Playground, with the chat as one panel among many). Sessions are the past-and-active durable execution records (replay / continue / clone / convert-to-eval). Conflating them produces two half-built versions of the same surface.
3. **`Agents ≠ chatbots`.** Agents are runtime execution entities with planners, tool bindings, memory bindings, policies, task ownership, event streams, and operational health — not personas. The Agents page is fleet management, not an assistant gallery; it is a lens over the Agent Registry ([[D-060]]).
4. **Structuring rule: no Console page phase ships without its feeding Protocol-surface phase landing first or in the same wave.** This is the §13 "no primitive without its consumer" rule read backwards — it keeps the Console honest as a Protocol client instead of letting it grow private hooks. The `notification.*` topic (Overview intervention queue) and `search.*` Protocol methods (global ⌘K) land as named acceptance criteria of their consuming page phases, not as free-floating primitives.
5. **MCP Apps `DisplayMode` (`inline` / `fullscreen` / `pip`) is a Protocol-level concern** — the MCP app declares its preferred mode, the runtime forwards it, the Console honours it. DisplayMode lives in `internal/protocol/types/`, not in Console-only state.

---

## D-063 — The Console Flows page is a view over engine graphs scoped to graph-family planners; V1 = read / run / inspect-history; authoring / versioning / import-export is post-V1

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §7, `docs/glossary.md` (Flows), master plan README.md (Console-wave note).

**Why:** "Flows" risked being scoped as a new runtime subsystem when it is really a projection. Settled to bound it.

1. **Flows are engine graphs, not a new subsystem.** A "Flow" in the Console is the graph structure that a graph-family planner (Graph / Workflow / Deterministic — RFC §6.2, §12) runs on. It is a view over `internal/runtime/engine/` node graphs, filtered to agents whose planner is graph-shaped.
2. **V1 Flows page = read / run / inspect-run-history** — a pure lens, needing only a Protocol method that exposes the engine graph structure + run history.
3. **Authoring / versioning / import-export is post-V1** — that is the part that may need a real subsystem, and it is deliberately deferred. This splits Flows along the same present-vs-authoring line as [[D-062]]'s `Live Runtime ≠ Sessions`.

---

## D-064 — Evaluations is a post-V1 subsystem built as a §4.4 extensibility seam; it depends on fully-replayable sessions, which makes the durable event log (Phase 57) a hard dependency; a premium/hosted variant must be a driver, not a fork

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §12 (post-V1 / future work), master plan README.md (phase 57 detail block, V1 cut line), `docs/glossary.md` (Evaluations).

**Why:** The Console IA lists an Evaluations page; that page implies a substantial runtime subsystem. Its scope and dependencies are settled so V1 does not foreclose it.

1. **Evaluations is a subsystem, not a page.** "Eval suites, golden sessions, replay-based evaluation, regression diffs, baseline promotion" is a runtime program — an eval runner, eval storage, replay machinery — with the Console page as its thin front-end. It is explicitly **post-V1**.
2. **It is the foundation for post-V1 agent version-control** — success-rate-over-`version_hash` ([[D-059]]), prompt evolution, tool evolution.
3. **It depends on fully-replayable sessions, so Phase 57 (durable event log) is a hard dependency.** "Create eval from session" / "mark as test case" only work if a session's event log is durable and gap-free. Lossy V1 sessions (ring-buffer-only) would foreclose Evaluations entirely — you cannot retrofit completeness into already-shipped sessions. Phase 57's durability guarantees are therefore binding, not optional.
4. **Built as a §4.4 seam from day one** — interface + drivers — so a premium / hosted / enterprise variant is a *driver*, not a fork of the runtime. This keeps a future monetization path open without polluting the V1 open-source surface.

---

## D-065 — The session `priority` dimension is dropped from V1

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §6.9 (sessions), `docs/research/11-console-feature-surface.md`.

**Why:** The operator Console mockup showed a "Priority: Normal / High / Low" field on the session detail panel. Brief 11 recommends dropping it from V1 unless real load patterns demand it.

1. **Dropped from V1.** No session-level priority field; no router or task-registry plumbing for it. Task-level prioritization via the `PRIORITIZE` steering control (Phase 52/53) already exists and covers the concrete operator need.
2. **Revisit only on evidence.** If post-V1 load patterns show a genuine need for session-level priority, it is a scoped phase touching sessions (RFC §6.9) and routers (Phase 14) — not a V1 retrofit.

---

## D-066 — Fleet control is a distinct, more-elevated privilege tier than fleet observation; a runtime-side control-plane client enrollment allowlist is deferred ("decide later")

**Date:** 2026-05-14
**Status:** Settled
**Where it lives:** RFC §6.16 (Agent Registry security), RFC §5.5 (Protocol authentication), `docs/plans/phase-53a-agent-registry.md`.

**Why:** A Console deployed to manage a fleet of Harbor runtimes is a control plane; the security model for that needs to be explicit so it is not discovered late.

1. **Fleet control is a distinct, more-elevated privilege tier than fleet observation.** Observation (read events, view topology, list agents) and control (pause / drain / restart / force-stop) are different privilege tiers. Control requires a more-elevated scope claim than observation — this extends the §6.5 / §6 elevated-scope-claim concept to the fleet surface. A leaked read-only Console token must not be able to force-stop a fleet. Every fleet-control command is audit-redacted and emitted ("who restarted which agent, from which Console, when").
2. **The Console is just another Protocol client** — it authenticates to each runtime with an operator-issued JWT (asymmetric algorithms only, §7), and the Protocol never accepts a request without an identity scope (§8). Deployment posture (private subnet, Console as the only reachable client, optional transport mTLS) is defense-in-depth and is mostly the operator's responsibility, not a runtime feature.
3. **A runtime-side control-plane client enrollment allowlist is deferred.** A runtime recording "control-plane client with key-fingerprint F is authorized at scope S" is stronger than per-request JWT scope alone, but the JWT scope covers the core V1 need. This is a "decide later" item, not V1 scope. It is the legitimate inverse of a Console DB ([[D-061]]) — a runtime-side record of authorized controllers, not a Console-side record of agents.
