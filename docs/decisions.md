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

<!--
Append new entries below this line in the form:

## D-NNN — <one-line summary>
**Date:** YYYY-MM-DD
**Status:** Settled | Tentative | Superseded by D-MMM | Reverted
**Where it lives:** <files>
**Why:** <2-3 sentences>
-->
