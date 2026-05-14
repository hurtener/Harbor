# Harbor — Master Phase Plan

## How to read this file

This is the canonical execution index for Harbor's V1 build. Every individual phase plan (`docs/plans/phase-NN-<slug>.md`) lives under it and inherits its done-definition, dependency declarations, and coverage discipline.

- **Source of truth:** `/RFC-001-Harbor.md` (referenced as RFC §X.X). Every phase below traces to one or more RFC sections; if a phase plan and the RFC drift, the RFC wins (`AGENTS.md` §2).
- **Research substrate:** the eleven briefs in `docs/research/01..11.md` (canonical index: `docs/research/INDEX.md`). Decisions on shape, sharp edges, and Go-flavored types come from there.
- **Numbering:** `phase-NN-<slug>.md`, two-digit zero-padded; lettered suffixes (`36a`, `53a`) insert work into an existing band without renumbering. Phases 01–82 + 36a + 36b + 53a are V1; 83–99 are post-V1 follow-ups listed for completeness so we don't lose track.
- **Done-definition (binding, from `AGENTS.md` §4.2):** (a) all acceptance criteria pass; (b) coverage targets met; (c) `scripts/smoke/phase-NN.sh` shows `OK ≥ count(criteria)` and `FAIL = 0`; (d) prior phases' smoke scripts still pass.
- **Coverage defaults (override per phase):** 80% for new packages; 85% for persistence drivers and conformance-tested subsystems; 70% for CLI/tooling.
- **Predecessor name:** does not appear in this repository, ever. (`AGENTS.md` §13.)

## Phase index

| #  | Name                                          | Subsystem            | RFC §       | Deps                  | Cov. | Status   |
|---:|-----------------------------------------------|----------------------|-------------|-----------------------|-----:|----------|
| 00 | Skeleton                                      | repo / hygiene       | n/a         | —                     | n/a  | Shipped  |
| 01 | Identity & isolation triple                   | identity             | §4          | 00                    | 90%  | Shipped  |
| 02 | Configuration loader                          | config               | §10         | 00                    | 85%  | Shipped  |
| 03 | Audit redactor                                | audit                | §6.4, §6.15 | 00                    | 90%  | Shipped  |
| 04 | slog Logger + standard attribute set          | telemetry            | §6.14       | 03                    | 85%  | Shipped  |
| 05 | Event taxonomy + InMem `EventBus` + isolation | events               | §6.13       | 01, 03                | 85%  | Shipped  |
| 06 | Bus replay + ring buffer + cursor             | events               | §6.13       | 05                    | 85%  | Shipped  |
| 07 | StateStore iface + InMem + conformance suite  | state                | §6.11, §9   | 01, 03                | 85%  | Shipped  |
| 08 | SessionRegistry + lifecycle + GC              | sessions             | §6.9        | 01, 07                | 85%  | Shipped  |
| 09 | Envelopes, Headers, Identity quadruple        | runtime/messages     | §6.1        | 01, 08                | 85%  | Shipped  |
| 10 | Engine + workers + cycle detection            | runtime/engine       | §6.1        | 09                    | 85%  | Shipped  |
| 11 | Reliability shell (timeout/retry/validate)    | runtime/engine       | §6.1        | 10                    | 85%  | Shipped  |
| 12 | Streaming + per-run capacity backpressure     | runtime/streaming    | §6.1        | 10, 11                | 85%  | Shipped  |
| 13 | Cancellation + per-run fetch dispatcher       | runtime/engine       | §6.1        | 10, 12                | 85%  | Shipped  |
| 14 | Routers + concurrency utils + subflows        | runtime/routers      | §6.1        | 10, 11                | 85%  | Shipped  |
| 15 | SQLite StateStore driver                      | state/sqlite         | §6.11, §9   | 07                    | 90%  | Shipped  |
| 16 | Postgres StateStore driver                    | state/postgres       | §6.11, §9   | 07                    | 90%  | Shipped  |
| 17 | ArtifactStore iface + InMem + FS drivers      | artifacts            | §6.10, §9   | 01, 07                | 85%  | Shipped  |
| 18 | ArtifactStore SQLite-blob + Postgres-blob     | artifacts            | §6.10, §9   | 17, 15, 16            | 85%  | Shipped  |
| 19 | ArtifactStore S3-style driver                 | artifacts            | §6.10       | 17                    | 80%  | Shipped  |
| 20 | TaskRegistry iface + InProcess + lifecycle    | tasks                | §6.8        | 01, 07                | 85%  | Shipped  |
| 21 | TaskGroup + retain-turn + patches             | tasks                | §6.8        | 20                    | 85%  | Shipped  |
| 22 | MessageBus + RemoteTransport contracts        | distributed          | §6.12       | 09, 20                | 85%  | Shipped  |
| 23 | MemoryStore iface + InMem + conformance       | memory               | §6.6        | 01, 07                | 85%  | Shipped  |
| 24 | Memory strategies (truncation, summary)       | memory               | §6.6        | 23                    | 85%  | Shipped  |
| 25 | SQLite + Postgres memory drivers              | memory               | §6.6, §9    | 23, 15, 16            | 90%  | Shipped  |
| 26 | Tool catalog core + InProcess registration    | tools                | §6.4        | 01, 05, 09            | 85%  | Shipped  |
| 26a| Flow-as-Tool registration + per-flow Budget   | runtime/flow + tools | §6.1, §6.4  | 14, 26                | 85%  | Shipped  |
| 27 | HTTP tool driver                              | tools/http           | §6.4        | 26                    | 85%  | Shipped  |
| 28 | MCP southbound driver                         | tools/mcp            | §6.4        | 26                    | 80%  | Shipped  |
| 29 | A2A southbound driver (full spec)             | tools/a2a            | §6.4        | 26, 22                | 80%  | Shipped  |
| 30 | Tool-side OAuth + HITL via pause/resume       | tools/auth           | §6.4, §3.3  | 26, 50, 53a           | 85%  | Pending  |
| 31 | Tool-side approval gates                      | tools/auth           | §6.4, §3.3  | 30                    | 80%  | Pending  |
| 32 | LLM client core + StreamSink contract         | llm                  | §6.5        | 09                    | 85%  | Shipped  |
| 33 | bifrost integration                           | llm                  | §6.5, §11Q3 | 32                    | 80%  | Shipped  |
| 33a| Custom OpenAI-compatible providers + timeouts | llm                  | §6.5        | 33                    | 80%  | Shipped  |
| 34 | Provider correction layer (one mode, baked)   | llm                  | §6.5        | 33                    | 85%  | Shipped  |
| 35 | Structured output strategies + downgrade      | llm                  | §6.5        | 33, 34                | 85%  | Shipped  |
| 36 | Retry with feedback                           | llm                  | §6.5        | 35                    | 85%  | Shipped  |
| 36a| Cost accumulator + per-identity ceilings      | governance           | §6.15       | 11, 15, 33            | 85%  | Shipped  |
| 36b| Per-identity rate limits + per-call MaxTokens | governance           | §6.15       | 36a                   | 85%  | Shipped  |
| 37 | Skill store + LocalDB driver + FTS5 ladder    | skills               | §6.7        | 01, 07, 15            | 85%  | Shipped  |
| 38 | Skill planner tools (search/get/list)         | skills/tools         | §6.7        | 26, 37                | 85%  | Shipped  |
| 39 | Virtual directory subsystem                   | skills               | §6.7        | 37                    | 80%  | Shipped  |
| 40 | Skills.md importer (gap-closer)               | skills/importer      | §6.7        | 37                    | 90%  | Shipped  |
| 41 | In-runtime skill generator with persistence   | skills/generator     | §6.7        | 37, 38, 03            | 90%  | Shipped  |
| 42 | Planner iface + Decision sum + RunContext     | planner              | §6.2, §3.2  | 09, 13, 26, 32        | 90%  | Shipped  |
| 43 | Trajectory + serialise (fail-loudly contract) | planner/trajectory   | §6.2, §3.4  | 42, 07                | 90%  | Shipped  |
| 44 | Schema repair pipeline                        | planner/repair       | §6.2        | 42, 32                | 85%  | Shipped  |
| 45 | Reference ReAct planner (minimum viable)      | planner/react        | §6.2        | 42, 43, 44, 32        | 85%  | Shipped  |
| 46 | Trajectory compression / summariser           | planner              | §6.2        | 43, 32                | 80%  | Shipped  |
| 47 | Parallel-call exec + ReAct emission upgrade   | planner+runtime      | §6.2        | 45, 14, 42, 20, 21    | 85%  | Shipped  |
| 48 | Deterministic planner (proves the iface)      | planner/deterministic| §6.2, §11Q6 | 42                    | 85%  | Shipped  |
| 49 | Planner conformance pack                      | planner              | §6.2        | 42, 45, 48            | 90%  | Shipped  |
| 50 | Pause/Resume Coordinator + handle registry    | runtime/pauseresume  | §6.3, §3.3  | 07, 09, 13            | 90%  | Shipped  |
| 51 | Pause-state serialise contract (fail-loud)    | runtime/pauseresume  | §6.3, §3.4  | 50, 43                | 90%  | Shipped  |
| 52 | Steering inbox + control taxonomy             | runtime/steering     | §6.3        | 50, 05                | 85%  | Shipped  |
| 53 | Steering wiring (9 control events)            | runtime/steering     | §6.3        | 52, 13                | 85%  | Shipped  |
| 53a| Agent Registry (registration identity + IDs)  | runtime/registry     | §6.16, §7   | 01, 05, 07, 08        | 85%  | Shipped  |
| 54 | Protocol task control surface                 | protocol             | §5.2, §6.3  | 50, 53, 20            | 85%  | Shipped  |
| 55 | OTel traces + propagation conventions         | telemetry            | §6.14       | 04, 05                | 85%  | Pending  |
| 56 | Metrics + OTLP + Prometheus drivers           | telemetry            | §6.14, §11Q5| 55, 05                | 85%  | Pending  |
| 57 | Durable event log driver (StateStore-backed)  | events               | §6.13       | 05, 07, 15, 16        | 85%  | Pending  |
| 58 | Protocol types/methods/errors single source   | protocol             | §5, §8      | 01                    | 90%  | Shipped  |
| 59 | Protocol versioning + deprecation policy      | protocol             | §5.3        | 58                    | 85%  | Pending  |
| 60 | Protocol wire transport (SSE + REST)          | protocol             | §5.4, §11Q1 | 58, 05                | 85%  | Pending  |
| 61 | Protocol auth + identity-scope enforcement    | protocol             | §5.5, §4    | 58, 60, 01            | 90%  | Pending  |
| 62 | Protocol conformance suite                    | protocol             | §5          | 58, 60, 61            | 85%  | Pending  |
| 63 | Harbor CLI skeleton (`harbor` + cobra)        | cmd/harbor           | §8          | 60                    | 70%  | Pending  |
| 64 | `harbor dev` v1 (boot runtime + protocol)     | cmd/harbor           | §8          | 63, 60                | 75%  | Pending  |
| 65 | `harbor dev` hot-reload                       | cmd/harbor           | §8          | 64                    | 75%  | Pending  |
| 66 | `harbor dev` draft-save scaffolding           | cmd/harbor           | §8          | 64                    | 75%  | Pending  |
| 67 | `harbor scaffold`                             | cmd/harbor           | §8          | 63                    | 70%  | Pending  |
| 68 | `harbor validate`                             | cmd/harbor           | §8          | 63, 02                | 75%  | Pending  |
| 69 | `harbor inspect-events / inspect-runs`        | cmd/harbor           | §8          | 63, 60                | 70%  | Pending  |
| 70 | `harbor inspect-topology` (ASCII renderer)    | cmd/harbor           | §8          | 63, 60                | 70%  | Pending  |
| 71 | `harbortest` test kit package                 | testing              | §6.13       | 05, 09, 07            | 85%  | Pending  |
| 72 | Console subscription protocol surface         | protocol             | §5.2, §7    | 60, 05, 06            | 85%  | Pending  |
| 73 | Console state inspection surface              | protocol             | §5.2, §7    | 60, 07, 17            | 85%  | Pending  |
| 74 | Console topology projection events            | protocol             | §5.2, §6.13 | 05, 09                | 85%  | Pending  |
| 75 | Console e2e Playwright (CI gate)              | testing              | §7          | 64, 72, 73            | n/a  | Pending  |
| 76 | Cross-tenant isolation conformance harness    | testing              | §4.3        | 07, 17, 23, 37, 20    | 95%  | Pending  |
| 77 | Goroutine leak conformance harness            | testing              | §5(Go)      | 10, 13, 50            | n/a  | Pending  |
| 78 | Chaos / fault injection harness               | testing              | n/a         | 76, 77                | n/a  | Pending  |
| 79 | Performance benchmarks                        | testing              | n/a         | 10, 12, 05            | n/a  | Pending  |
| 80 | Documentation hygiene polish (godoc, recipes) | docs                 | §2          | all V1                | n/a  | Pending  |
| 81 | Release engineering (versioning, changelog)   | release              | §12         | all V1                | n/a  | Pending  |
| 82 | V1 cut                                        | release              | §1, §12     | 81                    | n/a  | Pending  |
| 83 | Auto-sequence detection (planner opt.)        | planner              | §12         | 45                    | n/a  | Post-V1  |
| 84 | Reflection / critique loop                    | planner              | §12         | 45                    | n/a  | Post-V1  |
| 85 | Skills Portico provider driver                | skills/portico       | §6.7        | 37, 28                | n/a  | Post-V1  |
| 86 | Durable distributed bus driver                | distributed          | §6.12, §12  | 22                    | n/a  | Post-V1  |
| 87 | Durable TaskService backend                   | tasks                | §12         | 20, 22                | n/a  | Post-V1  |
| 88 | Episodic memory tier                          | memory               | §6.6, §11Q4 | 24, 25                | n/a  | Post-V1  |
| 89 | A2A northbound (Harbor as A2A server)         | tools/a2a            | §6.4, §11Q2 | 29                    | n/a  | Post-V1  |
| 90 | Additional planner concretes                  | planner              | §12         | 49                    | n/a  | Post-V1  |
| 91 | Console-driven key rotation (Protocol)        | governance           | §6.15       | 36a, 60, 73           | n/a  | Post-V1  |
| 92 | Console-driven mid-session model swap         | governance           | §6.15       | 36a, 60, 73           | n/a  | Post-V1  |
| 93 | Failover chains as Harbor policy              | governance           | §6.15       | 36a, 33               | n/a  | Post-V1  |
| 94 | Provider circuit breakers (provider, key)     | governance           | §6.15       | 33, 93                | n/a  | Post-V1  |
| 95 | LLM cache (exact-match + semantic)            | governance/cache     | §6.15       | 33                    | n/a  | Post-V1  |
| 96 | PII redaction at the LLM boundary             | audit                | §6.15       | 03, 33                | n/a  | Post-V1  |
| 97 | Media-input tool wrappers                     | tools/media          | §6.5, D-021 | 17, 26, 33            | n/a  | Post-V1  |
| 98 | Media-output tool wrappers                    | tools/media          | §6.5, D-021 | 17, 26, 33            | n/a  | Post-V1  |
| 99 | Vision-aware memory summarization             | memory               | §6.6, D-021 | 24, 33, 97            | n/a  | Post-V1  |
|100 | Recipe loader (declarative YAML flows)        | runtime/flow/recipe  | §6.1, D-023 | 26a                   | n/a  | Post-V1  |

V1 critical path: phases 01–82 + 26a + 36a + 36b (85 phases beyond skeleton). Post-V1 follow-ups: phases 83–100 (18 phases — Governance 91–96, Multimodal-output 97–99, Recipe loader 100). Total tracked: 100 + 26a + 36a + 36b + Phase 00 = 104 entries.

---

## Per-phase detail

Format: **Phase NN — Name** (RFC §X.X). Each entry is the stub the per-PR plan file expands. Acceptance criteria are binding once the phase ships.

### 01 — Identity & isolation triple (RFC §4)

**Goal.** Provide the `identity` package: `Identity{TenantID, UserID, SessionID}`, `From / MustFrom / With(ctx)`. The triple flows through every layer.
**Acceptance.** `MustFrom` panics in handler-only paths; `From` returns ok-bool elsewhere; round-trips through JWT claims and JSON; identity scopes can be derived (admin / console:fleet).
**Smoke.** `phase-01.sh` asserts the package exists and tests pass; no protocol surface yet.
**Tests.** Unit + property (round-trip).
**Risks.** None significant.

### 02 — Configuration loader (RFC §10)

**Goal.** YAML + env + flag layering; per-key annotation `restart_required` vs `live`; structured validation errors that point to the offending source.
**Acceptance.** Loader returns typed `Config`; missing required keys fail with file:line; `examples/harbor.yaml` round-trips.
**Smoke.** `harbor validate --config examples/harbor.yaml` returns 0 (subcommand auto-skip until phase 68).
**Tests.** Unit on layering precedence; golden tests on validation errors.

### 03 — Audit redactor (RFC §6.4, §6.15)

**Goal.** A single `audit.Redactor` that summarizes/truncates/redacts payloads before persistence or emission. Used by Logger, EventBus persistence, tool audit.
**Acceptance.** Redactor handles nested maps, byte arrays, secret-shaped strings (bearer/api-key/jwt), and oversize payloads; configurable allowlist/denylist; audit emits `audit.redacted` events for inspection.
**Smoke.** N/A (library only).
**Tests.** Unit + golden (fixed-input fixed-output).

### 04 — slog Logger + standard attribute set (RFC §6.14)

**Goal.** `Logger` wrapper around `log/slog`; pinned attribute set `(tenant_id, user_id, session_id, run_id, task_id, trace_id, span_id, tool)`; JSON in production, text in dev; emits a paired `runtime.error` bus event on `Error`.
**Acceptance.** Loggers accept `WithIdentity(Identity)`; no log carries unredacted secret payloads (uses phase 03); CLI flag `--log-format=text|json` selects handler at process start.
**Smoke.** N/A.
**Tests.** Unit; integration with phase 03 redactor.
**Deps.** 03.

### 05 — Event taxonomy + InMem `EventBus` + isolation (RFC §6.13)

**Goal.** `Event`, `EventType` (exhaustive sealed enum), `EventPayload` sealed interface, `EventBus.Publish/Subscribe`, `Filter` with server-enforced identity gates. In-memory MPSC ingress + per-subscriber bounded fan-out + drop-oldest with `bus.dropped` events.
**Acceptance.** Subscribe rejects filters that elide the identity triple unless the caller has `admin` scope; identity-scope mismatches are audited; cardinality lint check fails CI on `RunID`/`TraceID` metric labels.
**Smoke.** `phase-05.sh` asserts `EventType` exhaustiveness via `go test`; protocol smoke skips.
**Tests.** Unit + fan-out + drop-policy + cross-tenant isolation; goroutine leak test.
**Deps.** 01, 03.

### 06 — Bus replay + ring buffer + cursor (RFC §6.13)

**Goal.** `Replay(from Cursor, filter)` against an in-memory ring (default 10k events, configurable). `Cursor = (SessionID, Sequence)`; gap-free guarantee within a `RunID`.
**Acceptance.** Late subscriber resumes cleanly; no duplicates; documented loss when ring overrun (durable log handled in phase 57).
**Tests.** Unit + concurrency (subscribe-during-publish); idle-subscription reaper test.
**Deps.** 05.

### 07 — StateStore iface + InMem + conformance suite (RFC §6.11, §9)

**Goal.** Single mandatory `StateStore` interface (no `Supports*` ceremony). InMem driver. `conformance.RunSuite(t, factory)` covering save/load/idempotency/identity-mandatory/cross-tenant-isolation/cross-session-isolation/concurrency/leak.
**Acceptance.** InMem passes the suite; the suite is the gate every later driver must pass; documented `EventID` (ULID) idempotency.
**Smoke.** N/A.
**Tests.** Unit + the conformance suite itself.
**Deps.** 01, 03.

### 08 — SessionRegistry + lifecycle + GC (RFC §6.9)

**Goal.** `SessionRegistry` over phase 07 store. Open/get/touch/close/inspect/GC. Identity triple captured on Open and immutable; reopen-after-close rejected; GC sweeps idle sessions but never reaps `RUNNING`.
**Acceptance.** Defaults: idle 24 h, hard cap 30 days, sweep 15 min; configurable via `GCPolicy`.
**Tests.** Unit + integration; cross-tenant isolation test on `Open`.
**Deps.** 01, 07.

### 09 — Envelopes, Headers, Identity quadruple (RFC §6.1)

**Goal.** `Envelope{Payload, Headers, RunID, SessionID, Timestamp, DeadlineAt, Meta}`. `Headers{TenantID, UserID, Topic, Priority}`. `RunID` is the runtime concurrency boundary; `TraceID` reserved for OTel.
**Acceptance.** `WithRunID` returns a copy; `(Tenant, User, Session, Run)` round-trips through JSON; `Meta` last-write-wins on collision (until merge function lands as RFC follow-up).
**Tests.** Unit + JSON round-trip.
**Deps.** 01, 08.

### 10 — Engine + workers + cycle detection (RFC §6.1)

**Goal.** `Engine` with one goroutine per node, bounded channels per adjacency (default 64), cycle detector at construction (`AllowCycle` opt-in), `Run / Stop / Emit / Fetch`. Egress dispatcher always-on.
**Acceptance.** Linear graph end-to-end works; `Stop` joins all workers; goroutine-leak test passes; cycle detector rejects without `AllowCycle`.
**Smoke.** `harbor dev` boots an empty engine; `/healthz` returns 200 (gated by phase 64).
**Tests.** Unit + integration + leak.
**Deps.** 09.

### 11 — Reliability shell (RFC §6.1)

**Goal.** Per-node `NodePolicy{Validate, TimeoutMS, MaxRetries, BackoffBase, BackoffMult, MaxBackoff}`. `RunError{Code, Message, Cause, Metadata}`. Errors route to Protocol unconditionally; egress emission is opt-in via engine option.
**Acceptance.** Timeout produces `RunError(NodeTimeout)`; retries respect `MaxRetries`; `validate=both` rejects malformed envelopes.
**Tests.** Unit on backoff math; integration per error code.
**Deps.** 10.

### 12 — Streaming + per-run capacity backpressure (RFC §6.1)

**Goal.** `StreamFrame{StreamID, Seq, Text, Done, Meta}`. `EmitChunk` honors per-run capacity waiters keyed by `RunID`. **Backpressure baked in, not bolted on** — the seam closes the predecessor's deadlock-under-streaming gap.
**Acceptance.** N parallel runs × K frames each: ordering preserved per `StreamID`; no cross-run deadlock; goroutine-leak under streaming returns to baseline after `Stop`.
**Tests.** Integration + concurrency + leak.
**Deps.** 10, 11.
**Risks.** This is Brief 01's "must bake in." Don't accept a "we'll add it later" PR.

### 13 — Cancellation + per-run fetch dispatcher (RFC §6.1)

**Goal.** `Cancel(runID)` is idempotent, drops queued envelopes for that run only, cancels in-flight invocations, drains per-run egress. `FetchByRun(runID)` demuxes via per-run dispatcher (always-on, no dual mode).
**Acceptance.** Two concurrent runs; cancelling one leaves the other completing; `FetchByRun` never returns frames from another run.
**Tests.** Concurrency + property (cancel idempotency).
**Deps.** 10, 12.

### 14 — Routers + concurrency utils + subflows (RFC §6.1)

**Goal.** `PredicateRouter`, `UnionRouter`, `RoutePolicy`, `MapConcurrent`, `JoinK`, `Subflow(factory, parent, opts...)` (mirrors parent cancellation; runs to first egress payload).
**Acceptance.** Each pattern matches its specified behavior; subflow cancellation mirrors parent.
**Tests.** Integration per pattern.
**Deps.** 10, 11.

### 15 — SQLite StateStore driver (RFC §6.11, §9)

**Goal.** `modernc.org/sqlite` (CGo-free), WAL journal, forward-only migrations under `internal/state/sqlite/migrations/`.
**Acceptance.** Passes the phase 07 conformance suite end-to-end; clean DB starts cleanly; existing DB at version N migrates to N+1 idempotently.
**Tests.** Conformance suite + migration tests.
**Deps.** 07.

### 16 — Postgres StateStore driver (RFC §6.11, §9)

**Goal.** `pgx/v5/stdlib`-backed `state.StateStore`, embedded forward-only migrations gated by `pg_advisory_lock` for safe multi-replica boot, opaque `BYTEA` payloads (per RFC §6.11 + D-027 — superseding the older brief 05 §1 "JSONB payloads" narrative).
**Acceptance.** Passes the phase 07 conformance suite end-to-end; CI matrix exercises against a containerized Postgres.
**Tests.** Conformance suite + migration tests (clean-start, idempotency, advisory-lock concurrent boot) + Postgres-specific concurrent-reuse stress.
**Deps.** 07.

### 17 — ArtifactStore iface + InMem + Filesystem drivers (RFC §6.10, §9)

**Goal.** Mandatory routing above heavy-output threshold (default 32 KB, runtime-configurable, per-tool overridable). `ScopedArtifacts` facade auto-stamps identity. Content-addressed IDs.
**Acceptance.** Re-uploading identical bytes returns the existing ref; cross-scope reads rejected; `NoOp` fallback explicitly absent.
**Tests.** Unit + isolation; dedup test.
**Deps.** 01, 07.

### 18 — ArtifactStore SQLite-blob + Postgres-blob (RFC §6.10, §9)

**Goal.** Persistent artifact lifetimes that survive restart; same conformance suite as InMem + FS.
**Acceptance.** Bytes round-trip; deletion is scope-checked; size enforcement matches thresholds.
**Tests.** Conformance suite.
**Deps.** 17, 15, 16.

### 19 — ArtifactStore S3-style driver (RFC §6.10)

**Goal.** S3-compatible driver behind the same interface (suitable for MinIO/AWS/R2/GCS-via-compat).
**Acceptance.** Conformance suite; lifecycle integration; presigned-URL `GetRef` path.
**Tests.** Conformance + integration against MinIO container.
**Deps.** 17.
**Risks.** V1 stretch — can slip to V1.1 if calendar pressure builds.

### 20 — TaskRegistry iface + InProcess + lifecycle (RFC §6.8)

**Goal.** Single `TaskID` namespace unifying foreground + background; lifecycle state machine (`PENDING → RUNNING → COMPLETE`, with `PAUSED → RUNNING`, `FAILED|CANCELLED` terminal); idempotency via `IdempotencyKey`; cancellation propagates per `PropagateOnCancel`.
**Acceptance.** Spawning with same `IdempotencyKey` returns same handle; cascade vs isolate behave per spec.
**Tests.** Unit + concurrency + isolation.
**Deps.** 01, 07.

### 21 — TaskGroup + retain-turn + patches (RFC §6.8)

**Goal.** Group resolution/sealing/cancel/apply; retain-turn semantics block foreground until group completes; `ApplyPatch` for human-approved context patches; `AcknowledgeBackground`.
**Acceptance.** Group sealing freezes membership; retain-turn correctly blocks; patches transition through pending → applied/rejected.
**Tests.** Integration; group lifecycle property tests.
**Deps.** 20.

### 22 — MessageBus + RemoteTransport contracts (RFC §6.12)

**Goal.** Contract definitions + in-process `MessageBus` (loopback) + `RemoteTransport` capable of A2A. `Publish` is at-least-once; handlers idempotent on `(TaskID, Edge, EventID)`. No durable distributed driver at V1.
**Acceptance.** In-process loopback delivers; RemoteTransport returns request/reply and stream with final `done=true`.
**Tests.** Unit + integration; contract tests for distributed driver (skip when no driver wired).
**Deps.** 09, 20.

### 23 — MemoryStore iface + InMem + conformance (RFC §6.6)

**Goal.** `MemoryStore` interface with mandatory identity (`require_explicit_key=true`, no opt-out). `Strategy=none` only. Conformance harness includes fail-closed-on-missing-`SessionID` test.
**Acceptance.** Missing identity fails closed + emits audit event; InMem passes the suite.
**Tests.** Conformance suite.
**Deps.** 01, 07.

### 24 — Memory strategies (RFC §6.6)

**Goal.** Add `truncation` and `rolling_summary`. Health states `healthy → retry → degraded → recovering → healthy`. Summarizer is an injectable `Summarizer` interface (LLM call lives in phase 32+).
**Acceptance.** Strategy matrix tested; degraded mode falls back to recent-window + queues recovery loop bounded by `RecoveryBacklogMax`; `memory.health_changed` events emitted.
**Tests.** Strategy matrix + property + integration with a stub summarizer.
**Deps.** 23.
**Status.** Shipped (D-035 — `OverflowDropOldest`-only enum, bounded recovery loop with `memory.recovery_dropped` overflow emit, retry/backoff/cadence constants not exposed as config; phase plan `phase-24-memory-strategies.md`).

### 25 — SQLite + Postgres memory drivers (RFC §6.6, §9)

**Goal.** Persistent memory state across restarts; same conformance suite.
**Acceptance.** All three drivers (InMem, SQLite, PG) pass; `Snapshot/Restore` round-trips byte-stable.
**Tests.** Conformance + Snapshot round-trip.
**Deps.** 23, 15, 16.

### 26 — Tool catalog core + InProcess registration (RFC §6.4)

**Goal.** `Tool`, `ToolDescriptor`, `ToolCatalog`, `ToolProvider` interfaces + the `ToolPolicy` reliability shell (D-024). In-process registration via Go generics + reflection (schemas derived from input/output types) — `tools.RegisterFunc(name, fn, opts...)` is the minimum-expression API. `CatalogFilter` keyed on `(tenant, user, session)` triple plus `GrantedScopes`. Argument validation at the catalog edge using `santhosh-tekuri/jsonschema`. Dispatcher wraps every invocation in the `ToolPolicy` shell (timeout / retry-with-exponential-backoff / validation) regardless of transport — so even a zero-config `RegisterFunc` is production-resilient.
**Acceptance.** A registered Go function appears in `cat.List(filter)` for the matching identity; arg validation produces typed `tool.invalid_args` events on failure; default `ToolPolicy` (zero-value) yields a 3-retry / 100ms→30s exponential backoff / 30s timeout shell on transient errors; `tools.WithPolicy(...)` overrides each axis.
**Tests.** Unit (filter combinations + ToolPolicy default firing); integration; concurrency (N concurrent calls under a misbehaving tool — backoff respected).
**Deps.** 01, 05, 09.

### 26a — Flow-as-Tool registration + per-flow Budget (RFC §6.1, §6.4, D-023)

**Goal.** `flow.Definition` shape (entry/exit nodes, node specs, optional intrinsic `Budget`). `flow.Compose(def) → Engine` builds a runnable engine reusable across invocations. `flow.RegisterAsTool(catalog, def, eng)` wires the Engine into the Tool catalog with `Transport: Flow` and schemas derived from entry/exit types. Per-flow `Budget` (deadline / hop-budget / cost-cap) composes with parent run + identity-tier ceilings via `min()`; whichever fires first aborts the flow with `ErrFlowBudgetExceeded`. Reliability shell: per-node `NodePolicy` from §6.1 still applies inside the flow; no double-wrapping.
**Acceptance.** A 3-node flow registers as a Tool whose schema reflects entry-input → exit-output; planner invokes it through the standard dispatcher; per-flow budget exceedance emits `flow.budget_exceeded` and produces `ErrFlowBudgetExceeded`; identity-tier governance can still abort the same flow via `ErrBudgetExceeded`. Tests assert both abort paths fire correctly under contention.
**Tests.** Unit (Definition validation; min() composition math). Integration (flow-as-tool round-trip via planner mock; budget-exceedance events). Concurrency (parallel flow invocations don't bleed budget state across runs).
**Smoke additions.** `flow.budget_exceeded` event observable; `ErrFlowBudgetExceeded` mappable to a `tool.error` payload.
**Coverage target.** `internal/runtime/flow`: 85%.
**Deps.** 14 (subflows + reliability shell), 26 (tool catalog + ToolPolicy).
**Briefs.** `brief 01` §6.1 / §6.5 (subflow lifecycle and reliability shell).
**Risks.** Budget-composition math under concurrent flow invocations — must be lock-free / atomic, same pattern as 36a's accumulator. Document.
**RFC anchor.** §6.1 (Flow-as-Tool subsection) + §6.4 (Flow transport variant).

### 27 — HTTP tool driver (RFC §6.4)

**Goal.** Inline (`RegisterHTTPTool(name, method, urlTemplate, ...)`) and out-of-process via UTCP-style manifest. Static auth (API key, bearer, cookie). Retry + rate-limit handling.
**Acceptance.** Both inline + manifest paths drive the same `ToolDescriptor`; integration against `httptest.Server`. **Shipped** — `internal/tools/drivers/http` exports `RegisterHTTPTool`, `LoadManifest`, `RegisterManifest`, three `AuthKind`s; URL/body/header templates use `text/template` with `urlquery` escaping and reject `{{ .Auth.* }}` references at load time (AGENTS.md §7 — no credential passthrough). `Retry-After` (seconds-integer + HTTP-date) honoured before returning the rate-limit error so the policy shell's exponential backoff stacks on top — driver consumes ONE retry budget per Invoke (D-024 no double-wrap). 4xx maps to `ErrToolInvalidArgs` (planner-reformulation channel); 5xx + transport errors are transient. `ToolsConfig.HTTPManifests []string` added to `internal/config`. Coverage: 88% (target 85%). D-025 concurrent-reuse test exercises N=128 invocations against a shared `httptest.Server` under `-race`; no context bleed, no goroutine leaks.
**Tests.** Integration; retry test.
**Deps.** 26.

### 28 — MCP southbound driver (RFC §6.4)

**Goal.** Go MCP client over stdio + streamable-HTTP + SSE. Auto-detect via `MCPTransportMode = Auto | SSE | StreamableHTTP`. Tool/resource/prompt mapping into `Tool`. Transport-level reconnect lives in `ToolPolicy` (D-024 retry shell), not in a parallel state machine inside the driver (D-037).
**Acceptance.** Mock MCP server (in-process) integration tests pass; resource subscriptions emit a separate event topic (`mcp.resource_updated`).
**Tests.** Integration + transport-fallback test; D-025 concurrent-reuse (N=100) against the in-process mock server pair.
**Deps.** 26.
**Implementation note.** Wraps `github.com/modelcontextprotocol/go-sdk@v1.6.0` — the official Go SDK. Auto-mode fallback (streamable-HTTP → SSE) lives at `Provider.Connect`, not at `Transport.Connect`, so failures during the MCP initialize handshake (a `client.Connect` error) trigger the fallback the same as transport-level connect errors. See `docs/decisions.md` D-037.

### 29 — A2A southbound driver (full spec) (RFC §6.4)

**Goal.** Agent Card discovery (`GET /.well-known/agent-card.json`); JSON-RPC `message/send`, `message/stream` (SSE), `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`. Registry with route scoring (trust tier, latency tier, capability match).
**Acceptance.** Mock A2A server integration (full Agent Card); registry resolves remote skills; A2A peers appear as `Tool` entries via `ToolProvider`.
**Tests.** Integration + spec-compliance suite.
**Deps.** 26, 22.

### 30 — Tool-side OAuth + HITL via pause/resume (RFC §6.4, §3.3)

**Goal.** `TokenStore` interface (InMem + SQLite + Postgres drivers) with **encryption-at-rest** for token material. `OAuthProvider` covering both **user-bound** and **agent-bound** binding scopes — `BindingScope` is a declared config field, not inferred. On `tool.auth_required`, the tool driver emits a typed `ErrAuthRequired` carrying a structured payload (provider, scope, binding-scope, flow-initiation URL); the runtime pauses via the unified pause/resume primitive (phase 50). Resume reattaches the token; A2A `AUTH_REQUIRED` converges on the same primitive. Authorization flows use **PKCE**; **RFC 7591 dynamic client registration** and authorization-server **metadata discovery** are supported. Agent-bound tokens are keyed by the Agent Registry's registration `agent_id` (phase 53a, D-059) — never by an isolation-tuple element, since `agent_id` is not part of the isolation tuple.
**Acceptance.** OAuth full pause/resume cycle round-trips for both binding scopes; A2A `AUTH_REQUIRED` triggers an identical event shape; `ErrAuthRequired` payload is typed and audit-redacted (no raw token material in events); PKCE challenge/verifier round-trips; dynamic registration + discovery exercised against a test authorization server; token material is encrypted at rest (driver conformance asserts ciphertext on disk); admin-scope authz gates protect provider configuration; cross-tenant / cross-user / cross-agent isolation conformance — one identity's tokens never resolve for another; user-bound and agent-bound tokens coexist for the same tool without collision; initiate-then-cancel emits no goroutine leak.
**Tests.** Integration end-to-end (both binding scopes); conformance with phase 50; isolation conformance (cross-tenant/user/agent); encryption-at-rest driver conformance; goroutine-leak (initiate-then-cancel).
**Deps.** 26, 50, 53a.
**Briefs.** **brief 09** (`docs/research/09-mcp-oauth-from-bifrost.md`) — documents bifrost's OAuth surface (`OAuth2Provider`, `OAuth2Config`, `OAuth2Token`, `OAuth2FlowInitiation`, `MCPUserOAuthRequiredError`, `MCPClientConfig` OAuth fields) as a Go-shaped reference for what to lift, what to leave, and what Harbor must add. **Bring back into the conversation when authoring the per-phase plan file** (§"Re-discussion checklist" at the bottom of the brief).

### 31 — Tool-side approval gates (RFC §6.4, §3.3)

**Goal.** Synchronous "approve this tool call" gates using the same pause/resume primitive — distinct from OAuth, simpler payload shape.
**Acceptance.** APPROVE/REJECT round-trip via the protocol; reject path raises typed `tool.rejected` events.
**Tests.** Integration.
**Deps.** 30.

### 32 — LLM client core (RFC §6.5)

**Goal.** `LLMClient` interface — **one method**, `Complete(ctx, req) (resp, error)`. `CompleteRequest` carries `Messages` whose `Content` is a sum-type (`Text *string` for the common case, or multimodal `Parts []ContentPart` for image/audio/file inputs — D-021), optional `ResponseFormat`, optional `OnContent`/`OnReasoning` streaming callbacks, cancellation via `ctx`, reasoning-effort hint. **No `Tools`, no `ToolChoice`, no `FunctionCall`** — tool dispatch lives in the runtime (RFC §6.4 "Code-level tool dispatch"). Inline `DataURL` content above the heavy-output threshold is auto-materialized to `ArtifactRef` before persistence/emit (D-022). **Context-window safety net (D-026)**: a catch-all pass at the LLM-client edge walks the assembled `CompleteRequest` immediately before the driver call and (a) fails loudly with `ErrContextLeak` if any message field carries raw bytes/strings ≥ heavy-output threshold that aren't `ArtifactStub`-shaped, (b) estimates total tokens against the model's configured context limit and fails with `ErrContextWindowExceeded` when the estimate is within `ContextWindowReserve` (default 5%) of the cap. V1 fails loudly; auto-cascade is post-V1.
**Acceptance.** Mock LLM client passes round-trip with text-only AND multimodal payloads (text + image part). Cancellation aborts streaming cleanly. Interface compiles without any tool-calling type ever appearing in `internal/llm/...`. Auto-materialization of oversized `DataURL` content is observable via `llm.image.materialized` event. **Safety-net catch-all pass exists; planted-leak test (a deliberately-buggy producer that emits ≥-threshold raw bytes) triggers `ErrContextLeak` + `llm.context_leak` audit event. Token-budget test (a synthetic huge prompt) triggers `ErrContextWindowExceeded` cleanly with a reservedness margin matching config.**
**Tests.** Unit + integration with mock (text + multimodal); assert no `Tool*` symbol leaks into the LLM package; auto-materialize threshold test; **planted-leak test (raw bytes survive a producer); token-budget test (synthetic big prompt); ArtifactStub round-trip test (a stub renders to the model-agnostic JSON shape and parses back).**
**Deps.** 09.

### 33 — bifrost integration (RFC §6.5, §11 Q-3)

**Goal.** Wire `github.com/maximhq/bifrost/core` (pure Go LLM gateway library) behind `LLMClient`. Implement a thin `Driver` adapter that translates Harbor's `CompleteRequest` ↔ bifrost's `BifrostChatRequest` / `BifrostChatResponse`, and a minimal `schemas.Account` providing API keys. Translation includes multimodal `ContentPart`s (D-021): map Harbor's `ImagePart`/`AudioPart`/`FilePart` (with `URL` / `DataURL` / `Artifact` supply forms) to bifrost's per-provider content shapes; auto-materialize oversized `DataURL` content to `ArtifactRef` (D-022) before sending. Bifrost's `Tools` / `ToolChoice` parameters are intentionally NOT used — Harbor's runtime owns tool dispatch (RFC §6.4). Q-3 is **resolved**; this is a normal implementation phase, not a decision gate.
**Acceptance.** Six-provider smoke green: basic chat + `json_object` response_format + streaming with content callback + ctx cancellation accepted by the runtime + token usage parsed + cost parsed + **one multimodal text+image round-trip** against a vision-capable model. Driver registers via `init()` blank-import per AGENTS.md §4.4. The driver package contains zero references to bifrost's `Tools` / `ToolChoice` types.
**Tests.** Unit (request/response translation); integration with mock; six-provider live conformance test (gated behind `HARBOR_LIVE_LLM=1` so CI does not burn API credits by default — the local dev loop and `harbor dev` do exercise it).
**Deps.** 32.
**Risks.** Bifrost requires Go 1.26+; Harbor's go.mod was bumped during validation. Stream-channel close timing on long streams may exceed naive cancel budgets — mitigation is `ctx.Done()`-driven channel-reader abandonment + goroutine-leak tests.
**See also.** `docs/research/08-llm-client-validation.md` (full validation report and results).

### 33a — Custom OpenAI-compatible providers + per-provider timeouts (RFC §6.5)

**Goal.** Extend Phase 33's bifrost driver so operators can wire any OpenAI-compatible LLM endpoint (NIM, vLLM, ollama, lm-studio, in-house gateways) via `harbor.yaml` without per-provider Go code. Adds `LLMConfig.CustomProviders []LLMCustomProviderConfig` (`Name` / `BaseURL` / `APIKeyEnvVar` / `Models` / per-provider `Timeout` / retry/backoff/concurrency knobs / `RequestPathOverrides`) + `LLMConfig.NetworkDefaults` (global fallthrough for native + custom). When `llm.provider` names a custom entry, the entry's network knobs apply and legacy `llm.api_key` / `llm.base_url` / `llm.timeout` are ignored. Phase 33a supports only `base_provider_type: openai`; future phases widen.
**Acceptance.** Account widened to multi-entry (single-PRIMARY contract per D-040 preserved — `GetConfiguredProviders` returns the one configured primary). `GetConfigForProvider` returns `*ProviderConfig` with `CustomProviderConfig.BaseProviderType = schemas.OpenAI` when the primary is a custom entry. Missing env var fails closed at `New` with `ErrMissingAPIKey` naming the var. httptest integration (happy / timeout / 5xx) green. D-025 N≥100 concurrent stress green on mixed config. No tool-call API symbol leak (extends Phase 33 static guard).
**Tests.** Unit (custom-provider construction + validation; `NetworkDefaults` fallthrough + per-provider override; native-and-custom coexist). Integration (`httptest.Server` mimicking OpenAI-compatible `/v1/chat/completions`: happy + 5xx + timeout). Concurrency (D-025 mixed config). Smoke `scripts/smoke/phase-33a.sh`.
**Deps.** 33.
**Risks.** Operator-facing BaseURL gotcha — bifrost's OpenAI provider appends `/v1/chat/completions`; operators set the host root, not the full `/v1` path. Documented in yaml + the wire-test asserts the correct path. Sub-second timeouts get rounded down to 0 by bifrost's `int(seconds)` cast — practical minimum is 1s today; widening waits for a NetworkConfig API rev. Corrections (Phase 34) match by model-name prefix; custom-provider model names are typically unprefixed — operators declare `ModelProfiles[<model>].Corrections` explicitly to get quirks applied.
**Settled decisions:** D-042.
**See also.** `docs/plans/phase-33a-custom-providers.md`.

### 34 — Provider correction layer + SchemaSanitizer (one mode, baked in) (RFC §6.5)

**Goal.** A **thin** correction layer — bifrost already normalizes provider-specific transport quirks across its 23 first-class providers (brief 08), so this phase is NOT a "native vs. LiteLLM" dual-architecture; it is a narrow `SchemaSanitizer` + message-shape normalizer that lives **between** the runtime and the `LLMClient` (NOT inside the client), handling only what bifrost does not. Scope: `response_format` shape adjustments, reasoning-effort routing for thinking-class models (`o1`, `o3`, `deepseek-reasoner`), schema normalization (`additionalProperties: false`, `strict: true` modes), message reordering (NIM), usage backfill (proxies that report 0/0). **No `use_native` toggle** — there is one mode, baked in. Scope is structured-output and message-shape correctness only — never tool-call APIs (those don't exist on this layer).
**Acceptance.** Each documented quirk has a passing normalizer test; switching providers does not require a configuration toggle; no tool-call API references in this package; the layer is demonstrably thin — quirks bifrost already handles are NOT re-implemented here.
**Tests.** One unit test per quirk; assert no `Tool*` symbol leaks.
**Deps.** 33.
**Briefs.** **brief 07** (code-level tool calling — runtime owns dispatch, so this layer never touches tool-call APIs), **brief 08** (bifrost validation — what the LLM substrate already normalizes, so this phase doesn't).

### 35 — Structured output strategies + downgrade chain (RFC §6.5)

**Goal.** `OutputMode = Native | Tools | Prompted`. Per-provider `ModelProfile` selects mode. Downgrade chain: `json_schema → json_object → text` on `invalid_json_schema` errors. `llm.mode_downgraded` events.
**Acceptance.** Forced-failure on each step of the chain results in observable downgrade and continued completion.
**Tests.** Integration per provider.
**Deps.** 33, 34.

### 36 — Retry with feedback (RFC §6.5)

**Goal.** Validation/parse failures feed back into the planner via `LLMClient` retry; bounded by `MaxRetries`; observable.
**Acceptance.** A planner-tagged invalid arg triggers a single LLM retry with corrective sub-prompt; retry count respects bound.
**Tests.** Integration with mock + bounded-loop assertion.
**Deps.** 35.

### 36a — Cost accumulator + per-identity ceilings (RFC §6.15)

**Goal.** Subscribe to `llm.cost.recorded` events; aggregate `Usage.Cost.TotalCost` by `(tenant, user, session)` and by model in StateStore-backed accumulators; gate the next call when ceiling exceeded; emit `governance.budget_exceeded`; fail loudly with `ErrBudgetExceeded`. Establish the `governance.Subsystem` interface with `PreCall`/`PostCall` hooks wrapping the `LLMClient` driver.
**Acceptance.** Three-driver conformance (in-mem / SQLite / Postgres) green for accumulators. Ceilings settable via config (Protocol-driven setters land post-V1 phase 91). Ceiling exceedance emits `governance.budget_exceeded` with the identity triple; runtime can route to the unified pause/resume primitive when configured. Cross-session isolation test passes.
**Tests.** Unit (accumulator math). Integration per driver. Concurrency (N concurrent calls do not overshoot ceiling — atomic / lock-free path documented). Cross-session isolation. Failure-mode (StateStore read failure → fail-loud, no silent permit).
**Smoke additions.** Healthz still 200; `governance.budget_exceeded` observable when synthesized; config knob round-trip.
**Coverage target.** `internal/governance`: 85%.
**Deps.** 11 (event bus skeleton — `llm.cost.recorded` shape lives there). 15 (StateStore SQLite driver — accumulator persistence). 33 (bifrost integration — cost reporting passthrough is the source).
**Briefs.** `brief 03` §6 (LLM client surface, cost reporting), `brief 06` §3 (event bus + identity-scoped subscriptions).
**Risks.** Concurrent-call ceiling overshoot if accumulator math isn't atomic — the design must be lock-free (atomic add + compare-and-swap) and the test must exercise high-concurrency.
**RFC anchor.** §6.15.

### 36b — Per-identity rate limits + per-call MaxTokens (RFC §6.15)

**Goal.** Token-bucket rate limiter per `(identity, model)` with bucket-state persisted in StateStore so it survives runtime restart. Per-call `MaxTokens` enforced from the identity's tier in `PreCall`. Emits `governance.rate_limited` and `governance.maxtokens_exceeded` events; fails loudly with `ErrRateLimited` and `ErrMaxTokensExceeded`.
**Acceptance.** Bucket fills/drains per config; bucket state survives runtime restart; MaxTokens tier resolved from identity in PreCall and applied to the request before it leaves Harbor; events emitted with identity triple; CLI smoke configures a tiny bucket and asserts the limit kicks in.
**Tests.** Unit (token-bucket math under fast and slow refill rates). Integration per driver. High-concurrency (N concurrent calls — bucket never goes negative; never permits more than `capacity`). Restart-survival.
**Smoke additions.** `governance.rate_limited` observable when bucket exhausted; bucket-fill timestamps consistent with config.
**Coverage target.** `internal/governance`: 85%.
**Deps.** 36a (Subsystem interface + identity scaffolding).
**Briefs.** `brief 03` §6 (LLM client surface), `brief 06` (event bus).
**Risks.** Token-bucket race conditions under concurrent call paths — must be lock-free.
**RFC anchor.** §6.15.

### 37 — Skill store + LocalDB driver + FTS5 ladder (RFC §6.7)

**Goal.** SQLite-backed skill store; FTS5 → regex → exact ranking ladder; CI tests both FTS-on and FTS-off builds. Schema with `Origin / OriginRef / Scope / ContentHash`.
**Acceptance.** Same scoring constants documented in brief 04 §4.4 produce stable rankings; `existing_origin != "pack"` short-circuit refuses overwrites.
**Tests.** Unit (golden ranking) + FTS-off-fallback test.
**Deps.** 01, 07, 15.

### 38 — Skill planner tools (search/get/list) (RFC §6.7)

**Goal.** `skill_search`, `skill_get`, `skill_list` registered through phase 26 catalog. Capability filter (`RequiredTools/Namespaces/Tags` ⊆ allowed). PII + tool-name redaction at injection. Tiered budgeter (full → drop optional → cap steps to 3).
**Acceptance.** Filter excludes mismatched skills; redactor strips disallowed names; budgeter fits within `max_tokens`.
**Tests.** Unit + integration.
**Deps.** 26, 37.

### 39 — Virtual directory subsystem (RFC §6.7)

**Goal.** `Directory(cfg)` API + `pinned_then_recent` / `pinned_then_top` selectors; identity-scoped; capability-filtered; redacted before injection.
**Acceptance.** Default `max_entries=30`, range 1–200; pinned skills always included; selection respects identity.
**Tests.** Unit + property.
**Deps.** 37.

### 40 — Skills.md importer (RFC §6.7)

**Goal.** Spec-compliant CommonMark parser; YAML frontmatter; section normalization (`## Steps`, `## Preconditions`, `## Failure modes`); attachments resolved as `ArtifactRef` (option (b) — RFC settled). Round-trip byte-stable.
**Acceptance.** Golden corpus of N spec-compliant Skills.md files imports without source edits and re-exports byte-stable; missing `trigger`/empty `steps` fail loudly.
**Tests.** Golden corpus + negative tests.
**Deps.** 37.
**Risks.** This is the predecessor's gap-closer. The byte-stable round-trip is a tested invariant.

### 41 — In-runtime skill generator with persistence (RFC §6.7)

**Goal.** `skill_propose(persist=true)` validates draft, stamps `Origin=Generated`, `OriginRef = "gen:{session_id}:{run_id}"`, scopes by operator-provided `Scope` (default `project`), upserts via store. Conflict policy: refuse to overwrite `Origin=PackImport`; for Generated→Generated, content-hash gates last-write-wins. **Audit is mandatory.**
**Acceptance.** Generator persists; subsequent search discovers; audit event emitted on every persist.
**Tests.** Integration end-to-end + isolation (cross-session no-leak unless promoted).
**Deps.** 37, 38, 03.

### 42 — Planner iface + Decision sum + RunContext (RFC §6.2, §3.2)

**Goal.** Define `Planner.Next(ctx, RunContext) (Decision, error)`; `Decision` sum (`CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, `Finish`); `RunContext` is the only surface planner sees.
**Acceptance.** Stub planner returning `Finish` runs end-to-end; planner package imports no Runtime internals.
**Tests.** Conformance harness skeleton; import-graph lint.
**Deps.** 09, 13, 26, 32.
**Wake-on-resolution contract (D-032).** When the planner emits a `SpawnTask` (or group `SpawnTask` via the patched surface from Phase 21) WITHOUT retain-turn, it MUST consume `tasks.WatchGroup(sessionID, groupID) (<-chan GroupCompletion, func(), error)` from `internal/tasks` to learn when the group resolves. The three wake modes (`push`, `poll`, `hybrid`) are documented at the `internal/tasks` package godoc; this phase ships the planner-side interface contract that each concrete (45, 48, future) maps onto exactly one mode. The TaskRegistry stays neutral — no `WakeMode` field, no `Supports*` capability protocol.

### 43 — Trajectory + serialise contract (RFC §6.2, §3.4)

**Goal.** `Trajectory.Serialize() (bytes, error)` returns `(nil, ErrUnserializable{Field:...})` on any non-JSON-encodable entry. **No silent-drop path.** `ToolContext` split: serialisable half + handle registry (process-local at V1 — see RFC §6.3).
**Acceptance.** Round-trip is byte-stable; non-serialisable handle returns `ErrUnserializable`; resume with missing handle returns `ErrToolContextLost`.
**Tests.** Round-trip + negative cases (per RFC contract).
**Deps.** 42, 07.
**Risks.** This phase closes the predecessor's silent-context-loss bug. The fail-loudly tests are the gate.

### 44 — Schema repair pipeline (RFC §6.2)

**Goal.** Salvage → schema repair → graceful failure → multi-action salvage, in `internal/planner/repair/`. Configurable per concrete (`arg_fill_enabled`, `repair_attempts`, `max_consecutive_arg_failures`).
**Acceptance.** Each step passes its targeted unit test; graceful failure forces `Finish{Reason: NoPath, Followup: true}` after N consecutive arg failures.
**Tests.** Unit per step + integration with malformed mock LLM responses.
**Deps.** 42, 32.

### 45 — Reference ReAct planner (minimum viable) (RFC §6.2)

**Goal.** LLM call loop, JSON-only action format, tool selection, completion detection, single tool call per step. Functional options for the small policy-shaped knobs.
**Acceptance.** 3-step reasoning task succeeds against a mock LLM; planner package has no Runtime imports; planner is concurrent-safe across runs.
**Tests.** Conformance pack (skeleton) + scenario.
**Deps.** 42, 43, 44, 32.
**Wake mode.** ReAct ships the **`push`** wake mode (D-032): a non-retain-turn `SpawnTask` returns control to the runtime; the runtime registers the planner against `tasks.WatchGroup`; on `GroupCompletion` the runtime re-invokes `Planner.Next` with the resolved `MemberOutcome` slice surfaced through `RunContext`. The LLM sees the next planner step only after the group resolves — no LLM call burns while children are in flight.

### 46 — Trajectory compression / summariser (RFC §6.2)

**Goal.** Configurable summariser invoked by runtime when `token_budget` exceeded. Produces `TrajectorySummary{Goals, Facts, Pending, LastOutputDigest, Note}`. Compression is a runtime concern; planner sees only the compacted view.
**Acceptance.** Over-budget trajectory triggers summarisation; summary replaces raw step history in subsequent prompt builds.
**Tests.** Integration with mock summariser.
**Deps.** 43, 32.

### 47 — Parallel-call execution + ReAct CallParallel/SpawnTask/AwaitTask emission (RFC §6.2)

**Goal.** `CallParallel{Branches, Join}` executes branches concurrently; atomic setup validation (any branch's invalid args fails the whole call before execution); parallel-pause atomicity (no branch starts side-effecting tools, or all reach checkpointed observation before pause commits); system cap `absolute_max_parallel=50`. PLUS the §13 primitive-with-consumer bundle: ReAct upgrades to EMIT `CallParallel` (delete the Phase 45 D-051 single-tool-call-per-step stop-gap) AND emit `SpawnTask` / `AwaitTask` via the two new reserved tool names (`_spawn_task`, `_await_task`). Phase 47 closes three primitive-with-consumer gaps in one wave (CallParallel runtime + SpawnTask emitter + AwaitTask emitter). D-056.
**Acceptance.** Atomicity contract holds under fault injection; ordering preserved per-branch; deterministic merge keys (branch index + tool name); 51-branch input fails with `ErrParallelCapExceeded`; `JoinFirstSuccess` cancels remainder; `JoinN` waits for N successes; ReAct emits `_spawn_task` → runtime spawns real task → group resolves → planner re-enters via `RunContext.Trajectory.Background` → planner emits Finish end-to-end.
**Tests.** Concurrency + property (atomicity invariant) + spawn → wake → re-entry integration test against real TaskRegistry + EventBus + ArtifactStore drivers.
**Deps.** 45, 14, 42, 20, 21.
**Wake-mode interaction.** ReAct's WakePush declaration (Phase 45 / D-032) is wired end-to-end: a non-retain-turn `SpawnTask` returns control to the runtime; the runtime registers against `tasks.WatchGroup`; on `GroupCompletion` the runtime re-invokes `Planner.Next` with the resolved `MemberOutcome` slice surfaced through `RunContext.Trajectory.Background`. The integration test asserts the round-trip.
**Parallel-pause atomicity contract surface.** Phase 47 ships the stub (`ErrParallelPauseUnsupported`) — the executor fails loud on a mid-execution pause request. Phase 50 (unified pause/resume primitive) upgrades the path to a checkpointed atomic pause.

### 48 — Deterministic planner (proves the iface) (RFC §6.2, §11 Q-6)

**Goal.** A second concrete that exercises a non-LLM `Decision` shape. Executes a programmatic decision tree without an LLM call.
**Acceptance.** Deterministic planner passes the conformance pack; the same Runtime executes both deterministic and React without changes.
**Tests.** Conformance pack.
**Deps.** 42.
**Wake mode.** Deterministic ships the **`poll`** wake mode (D-032): each `Planner.Next` invocation reads its outstanding group's `GroupCompletion` via a non-blocking receive on the channel returned from `tasks.WatchGroup`. If the channel hasn't fired, the planner emits `AwaitTask` and the runtime sleeps the step until the next deterministic boundary; if it has fired, the planner reads the resolved `MemberOutcome` slice and proceeds. No LLM, no eager wake — a clean deterministic shape that proves the registry's `WatchGroup` surface is mode-neutral.

### 49 — Planner conformance pack (RFC §6.2)

**Goal.** A shared test pack any `Planner` implementation must pass: top-20 prompts produce valid `Decision` against canned tool catalog + LLM mock; respects budget; never panics on malformed LLM output.
**Acceptance.** Pack runs against React and Deterministic; `go test ./internal/planner/conformance/...` exits 0.
**Tests.** The pack itself.
**Deps.** 42, 45, 48.
**Wake-mode round-trip (D-032).** The conformance pack MUST include a `SpawnTask` → group completes → planner re-enters → reads `MemberOutcome` round-trip exercising whichever wake mode the concrete declares (push / poll / hybrid). React validates `push`; Deterministic validates `poll`; future hybrid concretes validate `hybrid`. Failure to wire `tasks.WatchGroup` is the test's failure mode, not silent deadlock.

### 50 — Pause/Resume Coordinator + handle registry (RFC §6.3, §3.3)

**Goal.** `pauseresume.Coordinator` with `Request/Resume/Status`. `Token` is opaque (runtime-owned encoding). Handle registry is process-local at V1 (documented constraint; distributed handle directory deferred — RFC §12).
**Acceptance.** Round-trip pause→serialise→load→resume succeeds; pauses survive Runtime restart only when StateStore-backed checkpoint is configured.
**Tests.** Unit + integration; durability (in-mem / SQLite / Postgres).
**Deps.** 07, 09, 13.

### 51 — Pause-state serialise contract (fail-loud) (RFC §6.3, §3.4)

**Goal.** Pause record serialises with `format_version: 1` JSON. Non-serialisable handles → `ErrUnserializable` (no silent `nil`); missing-on-resume handles → `ErrToolContextLost`.
**Acceptance.** Negative tests are the gate. CI fails on any silent-drop regression.
**Tests.** Conformance with phase 43 `Trajectory.Serialize`.
**Deps.** 50, 43.
**Shipped.** `internal/runtime/pauseresume/pauserecord.go` ships `SerializeRecord` / `DeserializeRecord` + the `FormatVersion` constant. The Phase 43 reflective walker is exported as `trajectory.ValidateEncodable` and **shared** (not forked) by the pause-record contract — `SerializeRecord` walks it, surfacing `trajectory.ErrUnserializable` rooted at `PauseRecord.payload.<key>`; `DeserializeRecord` enforces `format_version: 1` (`ErrUnsupportedFormatVersion` on any other value). `Coordinator.Request`'s Payload-encodability check is **unconditional** (fails loud with or without a checkpoint store). Negative tests (`pauserecord_test.go`, `pauserecord_contract_test.go`, `test/integration/phase51_pause_serialise_test.go`) are the gate. Coverage 94.0% (target 90%). See D-069.

### 52 — Steering inbox + control taxonomy (RFC §6.3)

**Goal.** Per-run inbox owned by Runtime. Nine control event types: `INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`. Validation/sanitisation at Protocol edge: depth ≤ 6, ≤ 64 keys, ≤ 50 list items, ≤ 4096 chars/string, ≤ 16 KiB total. Per-event scopes per RFC §6.3.
**Acceptance.** Oversize/over-deep payloads rejected at edge; per-event scope mismatch returns 403 + audit.
**Tests.** Unit (validation) + integration (auth scope per event).
**Deps.** 50, 05.

### 53 — Steering wiring (9 control events) (RFC §6.3)

**Goal.** Drain-between-steps; planner sees only `RunContext.Control`. CANCEL hard/soft propagation; PAUSE blocks at next boundary; RESUME unblocks; INJECT_CONTEXT/REDIRECT/USER_MESSAGE visible on next planner step; APPROVE/REJECT advance pause; PRIORITIZE updates task; control-history capped per session.
**Acceptance.** Each event type has a passing integration test; no event applied mid-tool-call.
**Tests.** Integration matrix; concurrency mid-step.
**Deps.** 52, 13.
**Shipped.** `internal/runtime/steering/runloop.go` ships `RunLoop` — the per-run planner-step loop, the §13 first consumer of BOTH the Phase 50 `pauseresume.Coordinator` AND the Phase 52 steering inbox/taxonomy. `RunLoop.Run` drains the per-run `Inbox` once per step boundary (`apply.go` applies the nine control-event side effects; the planner sees only `RunContext.Control`), routes a planner's `RequestPause` through `Coordinator.Request` and blocks via the new `Inbox.WaitForEvent` (a coalesced 1-buffered notify channel — no busy-spin) until a RESUME/APPROVE arrives, and caps per-session applied-control history (`history.go`, `MaxControlHistory` newest-wins ring). **Deviation (§4.3):** Phase 53 *builds* the per-run planner loop rather than retrofitting an existing one — `internal/runtime/engine` is a graph executor, not a planner-step loop; the only `Planner.Next` driver before Phase 53 was the Phase 49 conformance harness. The loop lives in `internal/runtime/steering` (its master-plan subsystem); no new top-level directory, no RFC change (RFC §6.3 §4: "the runtime implements this loop"). CANCEL is soft-by-default with an optional `WithHardCancelHook` seam (no hard import of the engine). The nine-event integration matrix + the §13 pause-Coordinator round-trip + the drain-between-steps invariant test + the concurrency-mid-step test live in `test/integration/phase53_steering_wiring_test.go`. Coverage 92.4% (target 85%). See D-071.

### 53a — Agent Registry (registration identity + IDs) (RFC §6.16, §7)

**Goal.** An in-process, per-runtime-instance `registry.AgentRegistry` subsystem, StateStore-backed (in-mem / SQLite / Postgres, §4.4 seam). Owns the **registration identity** of agents and the three-ID model (D-059): a stable `agent_id` (minted once at first registration, persisted, rehydrated on restart), an ephemeral `incarnation` (bumps every process start), and a content-derived `version_hash` (deterministic hash over prompt set, tool set + schemas, planner config, model policy — bumps only when configuration changes). `agent_id` is a registration identity, **not** an isolation principal — the isolation tuple stays `(tenant, user, session, run)` (D-059, CLAUDE.md §6). Handles both creation cases (D-060): locally-hosted agents (the runtime mints a local `agent_id`) and connect-to-remote agents (the local `agent_id` is a *handle*; the canonical identity is the remote A2A AgentCard, owned by the remote operator). Emits `agent.*` events (`agent.registered`, `agent.restarted`, `agent.health`, `agent.drained`, `agent.deregistered`) so the Console Agents page renders runtime state, never Console-local state (D-061). Fleet *control* (pause / drain / restart / force-stop) is a distinct, more-elevated privilege tier than fleet *observation* (D-066) — every control command is audit-redacted and emitted.
**Acceptance.** `agent_id` is stable across restart when a durable StateStore driver is configured (rehydration test); the in-mem driver is dev-only and documented as non-persistent. `incarnation` bumps on every restart; `version_hash` bumps iff configuration content changed and is stable otherwise (`restart ≠ recreate` — restart keeps the record, recreate mints a fresh `agent_id`). Remote-agent registration stores a handle + AgentCard reference; the handle is runtime-instance-local and never assumed globally unique. `agent.*` events carry the registration `agent_id`. Cross-tenant / cross-session isolation conformance — one identity's registry view never bleeds into another. Fleet-control commands require the elevated scope claim and emit audit events; fleet-observation does not. Concurrent-reuse test: N≥100 concurrent registrations / lookups / control commands against one shared `AgentRegistry` under `-race` (no data races, no context bleed, no goroutine leaks).
**Tests.** Unit (three-ID model, `version_hash` determinism, restart-vs-recreate); integration (StateStore-backed rehydration across all three drivers, real `events.EventBus` on the seam, identity propagation, ≥1 failure mode — missing identity fails closed); conformance (cross-tenant/session isolation); concurrency (D-025 N≥100 reuse stress).
**Deps.** 01, 05, 07, 08.
**Briefs.** **brief 09** (agent-as-actor / agent-bound OAuth — the registration `agent_id` is what Phase 30 keys agent-bound tokens by), **brief 11** (operator Console mockup — the Agents page is a runtime lens over this subsystem; `console-agents-page.png`).
**Why here.** Slotted into the 50–53 band (steering / pause-resume wave) because the earlier runtime-subsystem bands are already shipped; its real dependencies (01, 05, 07, 08) all landed long ago, so it can be implemented any time after them, but it must land **before** the Protocol surface (54+) and the Console-attaching wave (72–75) that consume it.
**Settled decisions:** D-059, D-060, D-061, D-062, D-066.

### 54 — Protocol task control surface (RFC §5.2, §6.3)

**Goal.** Protocol endpoints: `start`, `cancel`, `pause`, `resume`, `redirect`, `inject_context`, `approve`, `reject`, `prioritize`, `user_message`.
**Acceptance.** All nine endpoints + `start` round-trip via SSE+REST (phase 60); identity scope enforced.
**Tests.** Smoke `phase-54.sh` exercises each method.
**Deps.** 50, 53, 20.

### 55 — OTel traces + propagation (RFC §6.14)

**Goal.** `Tracer` wrapper; spans derived from events. Propagation: `traceparent` HTTP southbound; `_meta.traceparent` per request for stdio MCP; `HARBOR_TRACEPARENT` env on stdio spawn.
**Acceptance.** Trace continuity across HTTP and stdio; spans align with run/step boundaries.
**Tests.** Integration with Jaeger/OTLP collector.
**Deps.** 04, 05.

### 56 — Metrics + OTLP + Prometheus (RFC §6.14, §11 Q-5 settled)

**Goal.** `MetricsRegistry` derives from `Event.Type / NodeName / Producer` only. OTLP exporter default; built-in Prometheus `/metrics` endpoint at V1.
**Acceptance.** Cardinality-lint test fails CI on `RunID`/`TraceID` labels; both exporters emit core counters.
**Tests.** Integration; static cardinality lint.
**Deps.** 55, 05.

### 57 — Durable event log driver (RFC §6.13)

**Goal.** Persists `Event` records keyed by `(SessionID, Sequence)` via StateStore. Replay-from-cursor exact across restarts.
**Acceptance.** Late subscriber after Runtime restart sees no gaps; ring buffer mode auto-degrades to "best-effort" with warning.
**Tests.** Integration across all three StateStore drivers.
**Deps.** 05, 07, 15, 16.
**Downstream (load-bearing).** This is not just the Console event-stream backing — it is the **hard dependency for the post-V1 Evaluations / agent version-control program** (D-064). Evaluations is built on *fully replayable sessions* ("create eval from session", "mark as test case"); a session is only replayable if its event log is durable and gap-free. Lossy events (ring-buffer-only) in V1 would foreclose Evaluations entirely, since you cannot retrofit completeness into already-shipped sessions. Treat this phase's durability guarantees as binding for that reason, not optional.

### 58 — Protocol types/methods/errors single source (RFC §5, §8)

**Goal.** `internal/protocol/types/`, `internal/protocol/methods/`, `internal/protocol/errors/` are the only definitions. Lint check forbids hardcoded method strings outside `methods/`.
**Acceptance.** Build succeeds with the lint check active; new methods land only in `methods/`.
**Tests.** Lint test (CI).
**Deps.** 01.
**Status.** Shipped — D-075. Phase 54 (D-072 §1) already laid the `methods`/`errors`/`types` single-source layout, so Phase 58 is the *enforcement*: `internal/protocol/singlesource` ships `ScanProtocolTree`, a `go/parser` AST-walking checker, and `TestSingleSource_ProtocolTreeIsClean` is the build-gating `go test` (the same AST-lint pattern as `internal/planner/conformance/importgraph_test.go` — zero external-tool dependency, no `golangci-lint` plugin). The checker lints `internal/protocol/` only (method-name *strings* are legitimate unrelated vocabulary in other subsystems — a repo-wide scan would be all false positives) and lints `_test.go` files too. It surfaced and consolidated three pre-existing hardcoded method literals (`control.go`'s `dispatchStart`, two `_test.go` fixtures) — now re-derived from the `methods` constants. **Citation note (§4.3):** the row's "§8" is **CLAUDE.md §8** ("Harbor Protocol rules") — RFC-001 has no §8; RFC §5 is the design anchor, CLAUDE.md §8 is the rule the checker enforces. Coverage on `internal/protocol/singlesource` 94.5% (target 90%).

### 59 — Protocol versioning + deprecation policy (RFC §5.3)

**Goal.** `ProtocolVersion` constant; deprecation window discipline; capability negotiation.
**Acceptance.** Version constant returned on `harbor version` (after phase 63); deprecation note format settled.
**Tests.** Unit.
**Deps.** 58.

### 60 — Protocol wire transport (SSE + REST) (RFC §5.4, §11 Q-1)

**Goal.** SSE stream for events; REST/JSON for control surface. Identity-scope enforcement at edge. **Tentative — Q-1.** If WebSocket+JSON-RPC or gRPC server-streaming wins, this phase forks accordingly.
**Acceptance.** Console can stream events and submit control over the chosen transport; smoke covers both directions.
**Tests.** Integration; full duplex stress.
**Deps.** 58, 05.
**Risks.** Q-1 is the load-bearing decision. Owner sign-off required before this phase ships.

### 61 — Protocol auth + identity-scope enforcement (RFC §5.5, §4)

**Goal.** JWT (asymmetric only); `(tenant, user, session)` in claims; admin/console:fleet scopes for elevated subscriptions.
**Acceptance.** Missing claim rejected with audit; HS\*/`none` algorithms rejected at parser level.
**Tests.** Unit + integration; security suite.
**Deps.** 58, 60, 01.

### 62 — Protocol conformance suite (RFC §5)

**Goal.** A single conformance suite the protocol surface passes; covers every method, every error code, every event filter.
**Acceptance.** `go test ./internal/protocol/conformance/...` exits 0; smoke runs the same suite against `harbor dev`.
**Tests.** The suite itself.
**Deps.** 58, 60, 61.

### 63 — Harbor CLI skeleton (RFC §8)

**Goal.** `harbor` cobra binary with subcommands `dev`, `scaffold`, `validate`, `version`, `inspect-events`, `inspect-runs`, `inspect-topology`. All structured-error / `--quiet` / `--json` output mode.
**Acceptance.** `harbor --help` matches a golden file; `harbor version` returns version + build hash + Protocol version.
**Tests.** CLI golden tests.
**Deps.** 60.

### 64 — `harbor dev` v1 (RFC §8)

**Goal.** Boot embedded Runtime + open Protocol on `127.0.0.1:<port>`. No hot-reload yet. Identity injection via dev-token.
**Acceptance.** `harbor dev` returns `/healthz` 200; events stream cleanly to a test Console subscriber.
**Smoke.** `phase-64.sh` boots dev; `assert_status 200 /healthz`.
**Tests.** Integration (boot, smoke, teardown).
**Deps.** 63, 60.

### 65 — `harbor dev` hot-reload (RFC §8)

**Goal.** fsnotify watcher; graceful-drain restart on Go-source change; configurable retain-in-flight policy.
**Acceptance.** File change triggers drain; in-flight runs cancel cleanly; new code picked up.
**Tests.** Integration with file mutation.
**Deps.** 64.

### 66 — `harbor dev` draft-save scaffolding (RFC §8)

**Goal.** Project-local `.harbor/drafts/` scratchpad endpoint; iterate on agent without committing scaffold; "save" promotes to `harbor scaffold`-emitted layout.
**Acceptance.** Draft round-trip: edit → preview run → save → resulting scaffold passes `harbor validate`.
**Tests.** Integration + golden.
**Deps.** 64.

### 67 — `harbor scaffold` (RFC §8)

**Goal.** Generate a new agent skeleton from a template (default = "minimal-react"). Templates discoverable; output passes `harbor validate`.
**Acceptance.** `harbor scaffold my-agent` creates a buildable project; `harbor validate` returns 0.
**Tests.** Golden output.
**Deps.** 63.

### 68 — `harbor validate` (RFC §8)

**Goal.** Validate config / skills / agent definitions without booting. Errors include file:line.
**Acceptance.** Each error category produces a stable message; CI uses validate as a pre-flight check.
**Tests.** Golden errors.
**Deps.** 63, 02.

### 69 — `harbor inspect-events / inspect-runs` (RFC §8)

**Goal.** Tail/filter event bus; list recent runs + show trajectory.
**Acceptance.** `harbor inspect-events --session SID --type tool.completed` filters server-side; `harbor inspect-runs SID` shows run trajectory.
**Tests.** Golden CLI outputs.
**Deps.** 63, 60.

### 70 — `harbor inspect-topology` (RFC §8)

**Goal.** Render run's node graph as ASCII; consumes `topology.snapshot` events.
**Acceptance.** Sample run produces stable ASCII matching golden.
**Tests.** Golden.
**Deps.** 63, 60.

### 71 — `harbortest` test kit package (RFC §6.13)

**Goal.** Public `harbortest` package: `RunOnce(ctx, agent, input) (Output, EventLog, error)`, `AssertSequence(log, []EventType{...})`, `AssertNoLeaks(log)` (cross-tenant/session leakage detector), `SimulateFailure(toolName, code, n)`, `RecordedEvents(runID) []Event`.
**Acceptance.** Flow-level test ≤ 10 lines; `AssertNoLeaks` catches a deliberate cross-session bug in a regression test.
**Tests.** Self-test of the kit.
**Deps.** 05, 09, 07.

> **Console wave — re-decomposition pending (tracked, not yet expanded).** Phases 72–75 currently cover the Runtime-side Protocol hooks for a *subset* of the Console. RFC §7 now defines the full Console information architecture: a 14-page observability + control plane (Overview, Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background Jobs, Flows, Memory, MCP Connections, Artifacts, Evaluations, Settings) organized as **runtime lenses** — every page is a projection over `state snapshots + realtime events + control commands`. The binding structuring rule (RFC §7, CLAUDE.md §13): **no Console page phase ships without its feeding Protocol-surface phase landing first or in the same wave.** When this wave is re-decomposed, the heavy pages (Live Runtime, Events, Agents) each become their own phase twinned with a Protocol-surface phase; the lighter pages cluster. The Agents page is a lens over the Agent Registry (phase 53a). The `notification.*` topic (Overview intervention queue) and `search.*` Protocol methods (global ⌘K) land as named acceptance criteria of their consuming page phases, not as free-floating primitives. Evaluations is explicitly **post-V1** (D-064) — it is a subsystem, not a page. Re-decomposition itself follows the §16 phase-authoring ritual per new phase and is not done in this edit.

### 72 — Console subscription protocol surface (RFC §5.2, §7)

**Goal.** Read-only event subscription scoped by identity triple; admin/console:fleet scope for cross-session/tenant.
**Acceptance.** Console can subscribe to a session's events; cross-tenant call rejected unless scoped admin.
**Tests.** Integration.
**Deps.** 60, 05, 06.

### 73 — Console state inspection surface (RFC §5.2, §7)

**Goal.** `sessions.inspect`, `tasks.get`, `state.history`, `state.list_trajectories`, `state.load_planner_checkpoint`, `artifacts.list`, `artifacts.get`, `artifacts.get_ref`, `artifacts.delete` — all scope-checked, redacted on emit.
**Acceptance.** Each method enforces identity; redaction applied; pagination defined.
**Tests.** Integration + scope mismatch.
**Deps.** 60, 07, 17.

### 74 — Console topology projection events (RFC §5.2, §6.13)

**Goal.** `topology.snapshot` events emitted on engine construction + on edge change; static graph + live queue depth.
**Acceptance.** Console renders a topology view from these events alone (no internal access).
**Tests.** Integration.
**Deps.** 05, 09.

### 75 — Console e2e Playwright (CI gate) (RFC §7)

**Goal.** Playwright suite under `web/console/tests/*.spec.ts` runs against `harbor dev`. Per the binding rule: every operator-facing flow shipped in a phase has a matching `.spec.ts`. (Console implementation lives in its own repo; this phase covers the Runtime-side hooks + CI gate skeleton in this repo.)
**Acceptance.** A baseline harness exists; CI runs it (skipped if the Console repo isn't checked out as a dev dependency); future Console phases hook into it.
**Tests.** Playwright baseline.
**Deps.** 64, 72, 73.

### 76 — Cross-tenant isolation conformance harness (RFC §4.3)

**Goal.** A master conformance harness asserting cross-tenant + cross-session isolation across StateStore / ArtifactStore / MemoryStore / SkillStore / TaskRegistry / EventBus. 100 sessions × random ops × 30 s under `-race`.
**Acceptance.** Final invariant: every read's identity matches the caller's identity exactly; CI runs the harness on every PR.
**Tests.** The harness is the test.
**Deps.** 07, 17, 23, 37, 20.
**Risks.** This is the integrity gate. A regression here is a security bug.

### 77 — Goroutine leak conformance harness (RFC §5 Go conventions)

**Goal.** Harness wrapping every long-lived component asserting `runtime.NumGoroutine` returns to baseline after `Stop()`.
**Acceptance.** All Runtime components pass; CI runs on every PR.
**Tests.** The harness is the test.
**Deps.** 10, 13, 50.

### 78 — Chaos / fault injection harness

**Goal.** Kill mid-run, drop messages, simulate provider quirks, simulate StateStore disconnect, force pause-deserialize failures. Used in integration tests; not on hot path.
**Acceptance.** Each failure mode produces the documented event + recovery path.
**Tests.** Chaos suite.
**Deps.** 76, 77.

### 79 — Performance benchmarks

**Goal.** Engine throughput (envelopes/sec under N runs); bus fan-out (subscribers vs latency); memory-strategy latency (truncation vs rolling_summary).
**Acceptance.** Baseline numbers committed; perf regression threshold gates PRs (e.g. > 10% slowdown blocks).
**Tests.** `go test -bench`.
**Deps.** 10, 12, 05.

### 80 — Documentation hygiene polish

**Goal.** Every package has a doc comment; every exported symbol has godoc; example agents in `examples/`; recipe docs (`docs/recipes/`).
**Acceptance.** `golangci-lint`'s `revive exported` and `package-comments` clean; `examples/` builds end-to-end.
**Tests.** Lint + example builds in CI.
**Deps.** All V1 phases.

### 81 — Release engineering (versioning, changelog) (RFC §12)

**Goal.** Semver tagging, `CHANGELOG.md`, build provenance (SLSA-style attestations as a stretch).
**Acceptance.** `git tag v1.0.0-rc.1` produces a release artifact; CHANGELOG covers all V1 phases.
**Tests.** Release dry-run.
**Deps.** All V1 phases.

### 82 — V1 cut (RFC §1, §12)

**Goal.** `v1.0.0` tag; release notes; migration notes (if any); blog/announcement scaffold.
**Acceptance.** `harbor version` returns `v1.0.0`; preflight green; protocol conformance suite green; cross-tenant + leak harnesses green.
**Tests.** Full preflight.
**Deps.** 81.

### Post-V1 follow-ups (83–90)

Listed for tracking. Not on the V1 critical path.

- **83 — Auto-sequence detection.** Skip the LLM call on deterministic single-tool transitions. Off by default. RFC §12. Deps: 45.
- **84 — Reflection / critique loop.** Optional per planner. Self-critique before Finish. RFC §12. Deps: 45.
- **85 — Skills Portico provider driver.** Consume Portico-distributed skill packs via MCP; same SkillProvider interface. Deps: 37, 28.
- **86 — Durable distributed bus driver.** NATS / Redis Streams / Postgres-as-queue behind `MessageBus`. RFC §12. Deps: 22.
- **87 — Durable TaskService backend.** Background tasks survive restart. RFC §12. Deps: 20, 22.
- **88 — Episodic memory tier.** Durable summaries promoted from session → user/tenant scope. RFC §11 Q-4. Deps: 24, 25.
- **89 — A2A northbound.** Expose Harbor as an A2A server. RFC §11 Q-2. Deps: 29.
- **90 — Additional planner concretes.** PlanExecute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval. RFC §12. Deps: 49.
- **91 — Console-driven key rotation (Protocol).** `governance.rotate_key` Protocol method; `Account` impl atomically swaps the live key set; bifrost picks up the new key on the next `Account.GetKeysForProvider` lookup (no `ReloadConfig` race). RFC §6.15, D-019. Deps: 36a, 60 (Protocol transport), 73 (Console-attaching).
- **92 — Console-driven mid-session model swap.** `governance.swap_model` Protocol method; future runs in a session use the swapped model; the planner sees the change via `RunContext`. Audited. RFC §6.15. Deps: 36a, 60, 73.
- **93 — Failover chains as Harbor policy.** Operator-defined chain `[primary, secondary, ...]` per identity / model; orchestrated at the Governance layer with audit per hop; NOT pushed into bifrost's per-call `Fallbacks`. RFC §6.15, D-018. Deps: 36a, 33.
- **94 — Provider circuit breakers per `(provider, key)`.** Aggregate error rate; trip on threshold; auto-recover on cool-down; events emitted. Builds on 93. RFC §6.15. Deps: 33, 93.
- **95 — LLM cache (exact-match + semantic).** Plugin pre-hook checks the cache; semantic uses an embedding similarity threshold. Big complexity; deferred. RFC §6.15. Deps: 33.
- **96 — PII redaction at the LLM boundary.** Audit subsystem owns the redactor; Governance hooks it into the LLM call path. Outgoing prompts are scrubbed; raw forms are never persisted. RFC §6.15, D-020. Deps: 03 (audit redactor), 33.
- **97 — Media-input tool wrappers.** Bifrost-backed tools that accept `ArtifactRef`s and pass image/audio/file content to LLM-side analysis (e.g. a generic `image.analyze` wrapper that accepts an image artifact + a text prompt and routes through the planner's normal LLM call). Mostly a convention layer — the plumbing already exists once D-021 + Phase 33 ship. RFC §6.5, D-021. Deps: 17 (artifacts), 33 (bifrost), 26 (tool catalog).
- **98 — Media-output tool wrappers.** Image generation, speech synthesis, transcription, and video tools that wrap bifrost's media APIs (`SpeechRequest`, `TranscriptionRequest`, `ImageGenerationRequest`, etc.) and return `ArtifactRef`s. Each tool is a separate registration; they share a common `MediaTool` helper. The planner invokes them as ordinary tool calls; no `LLMClient` change. RFC §6.5, D-021. Deps: 17, 33, 26.
- **99 — Vision-aware memory summarization.** Extends the `rolling_summary` memory strategy to call a vision model when summarizing turns that include `ImagePart`s, replacing the V1 placeholder (`[image: <ref>]`) with a generated description. Optional per identity tier; off by default for cost. RFC §6.6, D-021. Deps: 24 (memory strategies), 33 (bifrost), 97 (media-input tools).

---

## Wave / parallelism map

The phase queue is a DAG, not a line. Here are the parallelizable waves; phases inside a wave can be implemented in parallel by separate workers, phases in later waves wait for earlier waves' completion (or for the specific phases their `Deps` column names).

**Wave 1 — Pure foundation (no upstream Harbor deps):**
01 (identity), 02 (config), 03 (audit redactor) — three independent, parallelizable.

**Wave 2 — Logger + bus skeleton:**
04 (slog Logger; needs 03), 05 (Event taxonomy + InMem bus; needs 01, 03), 07 (StateStore iface + InMem; needs 01, 03). Parallelizable across three workers.

**Wave 3 — Bus replay + sessions:**
06 (replay; needs 05), 08 (SessionRegistry; needs 01, 07). Parallelizable.

**Wave 4 — Core runtime serial chain (mostly):**
09 (envelopes; needs 01, 08) → 10 (engine; needs 09) → 11 (reliability; needs 10) → 12 (streaming; needs 10, 11) → 13 (cancel; needs 10, 12) → 14 (routers; needs 10, 11). 11+14 can parallelize once 10 lands; 12, 13 serialize after 11.

**Wave 5 — Persistence drivers (parallelizable across drivers):**
15 (SQLite state), 16 (PG state), 17 (Artifacts iface + InMem + FS — needs 01, 07). Three parallel.

**Wave 6 — Tasks + remaining persistence:**
18 (Artifact SQLite/PG; needs 17, 15, 16), 19 (Artifact S3; needs 17), 20 (TaskRegistry; needs 01, 07), 21 (TaskGroup + WatchGroup + retain-turn + patches; needs 20), 22 (Distributed contracts; needs 09, 20). Stage 1 (18, 19, 20) parallelizable; Stage 2 (21, 22) once 20 lands.

**Wave 7 — Memory + tools core + LLM core (parallel tracks):**

- Memory track: 23 → 24 → 25
- Tools track: 26 → 27 / 28 / 29 (HTTP, MCP, A2A in parallel after 26)
- LLM track: 32 → 33 → 34 → 35 → 36 (largely serial)
- Governance track (slots in after 33): 33 → 36a → 36b (serial; relies on cost-passthrough from bifrost integration)

**Wave 8 — Skills + planner core (after wave 7's foundations):**

- Skills track: 37 → 38 / 39 / 40 / 41 (after 37, the four can run in parallel-ish)
- Planner track: 42 → 43 / 44 (parallel) → 45 → 46 / 47 (parallel) → 48 → 49

**Wave 9 — Pause/Resume + Steering + Telemetry + Protocol (cross-track):**

- 50 (needs 07, 09, 13) → 51 → 52 → 53 → 54
- 53a (Agent Registry; needs 01, 05, 07, 08) — parallelizable with the 50→54 chain; its deps are all long-shipped. Must land before 54 and the Console-attaching wave (72–75).
- 55 (OTel; after 04, 05) parallel with 56 (metrics; after 55, 05); 57 (durable event log; after 05, 07, 15, 16)
- 58 (protocol types) → 59 (versioning) → 60 (transport) → 61 (auth) → 62 (conformance)
- 30 (Tool OAuth/HITL; needs 26, 50, 53a), 31 (approval gates; needs 30) slot in once 50 + 53a are up

**Wave 10 — CLI + test kit:**
63 → 64 → 65 / 66 / 67 / 68 / 69 / 70 (mostly parallel after 64). 71 (test kit; needs 05, 09, 07) parallel.

**Wave 11 — Console-attaching + hardening:**
72 / 73 / 74 (parallel; need 60, 05, 06, 07, 17, 09). 75 (e2e gate; needs 64, 72, 73).
76, 77, 78, 79 (parallel; need their respective subsystems). 80 (docs polish; needs all V1).

**Wave 12 — Release:**
81 → 82 (serial).

Practical reading: with three or four engineers (or three concurrent worker subagents), waves 5–8 hide enormous parallelism behind their tracks. The serial sections that resist parallelism are: the core runtime chain (09→10→11→12→13), the LLM-client chain (32→33→34→35→36), and the Protocol chain (58→60→61→62).

---

## V1 cut line

**V1 ships phases 01–82 + 36a + 36b + 53a.** Seventeen follow-ups (83–99) are intentionally deferred to post-V1: eight original (83–90), six Governance (91–96), and three Multimodality follow-ups (97–99) for media-input tool wrappers, media-output tool wrappers, and vision-aware memory summarization. Multimodal **inputs** ship in V1 (RFC §6.5 + D-021); only multimodal **outputs** and richer memory handling are post-V1. The Evaluations subsystem and code-mode (Starlark) are also post-V1 — see RFC §12.

The cut line is justified by RFC §12 (Out of Scope for V1):

- **Auto-sequence + reflection (83, 84)** — explicit RFC §12 entries: "optional optimization, off by default" and "optional per concrete; not on V1's critical path." Shipping the planner without them does not weaken the swappable-planner property; both can land as planner-internal upgrades without runtime change.
- **Skills Portico provider (85)** — depends on Portico's MCP surface stabilizing; not a runtime gating factor.
- **Durable distributed bus + durable TaskService backend (86, 87)** — RFC §6.12 settles "V1 ships contracts only; in-process default." A durable backend is a driver phase, not a runtime-architecture phase.
- **Episodic memory tier (88)** — RFC §11 Q-4 leans post-V1 unless V1 user feedback demands it.
- **A2A northbound (89)** — RFC §11 Q-2 leans V1.1 unless an early adopter demands it.
- **Additional planner concretes (90)** — RFC §12 explicitly: "wait on V1 evidence that the interface holds." V1 ships React + Deterministic; the rest land as evidence accrues.

If under calendar pressure, **phase 19 (ArtifactStore S3-style)** and **phase 75 (Playwright CI gate)** are the most reasonable V1 → V1.1 slip candidates inside the V1 list, in that order.

---

## Critical path

The longest dependency chain to V1, in order:

00 → 01 → 03 → 04 → 05 → 07 → 08 → 09 → 10 → 11 → 12 → 13 → 50 → 51 → 52 → 53 → 54 → 26 → 32 → 33 → 34 → 35 → 36 → 42 → 43 → 44 → 45 → 49 → 60 → 61 → 62 → 64 → 76 → 80 → 81 → 82.

That is **36 phases on the critical path** out of 84 V1 phases. (Governance phases 36a/36b sit on the LLM track but are not themselves on the critical path; they branch off after phase 33 and rejoin via the StateStore conformance suite.) Practical implications:

- **The runtime kernel chain (09→14)** is six phases of deeply serial work — half a critical-path month if one engineer.
- **The pause/resume coordinator chain (50→54)** is the second cluster of serial work — and depends on the runtime chain landing through 13.
- **The LLM client chain (32→36)** must complete before the planner reference (45) lands.
- **The protocol chain (58→62)** is independent until 60 needs a wire decision (Q-1) — which can block the Console-attaching wave.

**Highest-risk phases on the critical path** (in priority order):

1. **Phase 12 (Streaming + per-run backpressure)** — the predecessor's deadlock-under-streaming sharp edge; if shipped wrong, parallel runs deadlock.
2. **Phase 33 (bifrost integration)** — **Q-3 is resolved**. The phase is now a routine implementation rather than a decision gate. Risk dropped to "ordinary integration risk" — driver translation correctness + cancellation-timing diligence on long streams. See `docs/research/08-llm-client-validation.md`.
3. **Phase 50 (Pause/Resume Coordinator)** — the unified primitive; if it leaks abstractions to planner code, the swappable-planner property regresses.
4. **Phase 60 (Protocol wire transport)** — Q-1; locking the wrong transport now means a v1→v2 migration later.
5. **Phase 76 (Cross-tenant isolation harness)** — the integrity gate. If it lands late, regressions are not detected.

Risk-mitigation strategy: **front-load Q-1 and Q-3 decisions** so phases 33 and 60 don't enter implementation with open architecture questions.

---

## Open RFC questions affecting the plan

The RFC's open questions (RFC §11) directly gate or shape these phases:

- **Q-1 (Protocol wire transport).** Gates **phase 60**. Lean is SSE+REST. If the answer becomes WebSocket+JSON-RPC or gRPC, phase 60 forks accordingly; phases 64–75 (CLI + Console-attaching) inherit the new transport but their shapes do not change materially.
- **Q-2 (A2A northbound at V1).** Determines whether **phase 89** is V1 or post-V1. Default plan keeps it post-V1.
- **Q-3 (LLM client choice).** **RESOLVED 2026-05-08.** Replaced the original CGo-required candidate with `github.com/maximhq/bifrost/core` (pure Go). Empirically validated against six OpenRouter-routed models — 23/24 gating items pass. Phase 33 is now a routine integration; phases 34–36 carry only ordinary implementation risk. See `docs/research/08-llm-client-validation.md`.
- **Q-4 (Episodic memory tier).** Determines whether **phase 88** is V1 or post-V1. Default plan keeps it post-V1.
- **Q-5 (Skill versioning model).** Shapes **phase 41** (generator persistence) — content-hash-as-version is the V1 default; explicit semver is V1.5.
- **Q-6 (Second V1 planner concrete).** Settled in RFC as `deterministic`. Phase **48** is locked.

**Action:** Q-1 and Q-3 should be resolved before the corresponding phases enter the implementation queue. Q-2, Q-4 can be resolved at V1 cut.

---

## Notes

- **Phase numbers are stable once shipped.** A phase number is reused only via a `phase-NN-supersedes-MM.md` PR per AGENTS.md §15.
- **Phase plans are immutable post-ship**, except for typo/clarification fixes. Material change = new RFC PR + new phase plan that supersedes.
- **If the RFC switches to subsystem-prefixed numbering** (e.g. `R-01`, `P-01`), all phase plans rename in a single PR and this README reorganizes; phase numbering is therefore deliberately stable but not load-bearing for code or filenames in `internal/`.
- **Cross-references:** RFC Appendix A (subsystem ↔ brief table) is the canonical map for "which brief informs which RFC section." Use it when reaching for context on any phase.
- **Coverage targets** in the index column are starting points; per-phase plans may raise them. They never lower.
- **Smoke scripts:** every phase has `scripts/smoke/phase-NN.sh`. The skeleton lands when the phase begins; assertions land as the surface implements.
- **Phase 0 already passes.** Per `phase-00-skeleton.md`: 24 OK / 0 SKIP / 0 FAIL on the doc & mirror invariants. Subsequent phases inherit that gate.

---

## Appendix: runtime tool-dispatch trio mapping (post brief 07)

Brief 07 codified Harbor's "code-level tool calling" principle (RFC §6.4) and surfaced four discrete runtime components: `ActionParser`, `Dispatcher` (single + parallel folded), `RepairLoop`, `ObservationRenderer`. The current phase set covers them across existing phases — no renumbering required, but reviewers should anchor on this mapping when authoring per-phase plans:

| Trio component | Owner phase(s) | Notes |
|----------------|----------------|-------|
| `ActionParser` (`internal/runtime/planner/parser/`) | 44 (Schema repair pipeline) + 45 (Reference ReAct planner) | The parser belongs with the repair loop; the ReAct phase wires it into the planner step. |
| `Dispatcher` — single tool path | 26 (Tool catalog core + InProcess) | Validation, identity stamping, cancellation hooks. |
| `Dispatcher` — parallel branches | 47 (Parallel-call execution + JoinSpec) | Same validation/identity/cancel plumbing as 26; the two phases ship the same dispatcher, not two dispatchers. |
| `RepairLoop` | 44 (Schema repair pipeline) | Drives parser → validator → planner-prompt-on-failure cycles up to `RepairAttempts`. |
| `ObservationRenderer` (`internal/runtime/planner/observation/`) | 45 (Reference ReAct planner) + 46 (Trajectory compression / summariser) | Renderer interleaves assistant/user messages from `(action, observation \| error \| failure)` pairs; compression in 46 plugs into the same renderer. |
| `SchemaSanitizer` (`internal/llm/correction/`) | 34 (Provider correction layer) | Lives between runtime and LLM client; per-provider `response_format` adjustments. |

If a future PR renames the package layout from `internal/runtime/planner/...` to a flatter `internal/dispatch/` etc., the mapping table above moves with it and the phases retain their numbers. The trio is a design unit; splitting a single phase into "parser" + "dispatcher" + "renderer" sub-phases is allowed but not required.
