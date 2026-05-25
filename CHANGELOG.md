# Changelog

All notable changes to Harbor are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and Harbor adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two versions move independently in Harbor (RFC §5.3):

- The **product release version** — the `harbor` binary's own semver,
  reported by `harbor version` and the subject of the headings in this
  file.
- The **Harbor Protocol version** — the Runtime↔Console wire-contract
  version, pinned in `internal/protocol/types/version.go` (currently
  `0.1.0`). A breaking Protocol change is an RFC change and carries its
  own deprecation window.

## [1.1.0] — 2026-05-25

The V1.1 cut, focused on **Playground multimodal input** and **1:1 Console↔Runtime feature parity**. Five round-N walkthroughs against the YouTube validation agent + real bifrost surfaced eleven findings (F1, F2, F5, F6, F7, F9, F10, F11, F12, plus phase 84a F1 + F8); every finding shipped a fix or a deferred-with-plan entry. Harbor Protocol stays at `0.1.0` (no breaking wire changes — `topology_snapshot` is an additive capability, `StartRequest.InputArtifactIDs` is opt-in via `omitempty`).

### Added — Playground multimodal artifact input (round-7 F11 / D-166)

- **`StartRequest.InputArtifactIDs []string`** — opt-in wire field on the canonical `start` request. Text-only spawns elide the field entirely (`omitempty` honours the existing wire shape). Operator-uploaded artifacts attach to a foreground task's first planner turn; the runtime resolves each id, materializes the appropriate `Content.Parts` shape, and routes per MIME. `tasks.SpawnRequest.InputArtifactIDs` folds into the idempotency-key content hash so "same key, different attachments" surfaces as `ErrIdempotencyConflict`.
- **`Tool.HandlesMIME []string`** — new tool-descriptor field declaring which MIME types a tool consumes. The planner's multimodal materializer populates `ArtifactStub.Fetch.Tool` from the first matching descriptor — explicit "use this tool for this ref" hint to the LLM, no catalog-discovery guesswork. `Tool.MatchesMIME(mime string)` helper supports exact + `type/*` wildcard matching (no full-`*/*` to keep operator-declared MIMEs predictable).
- **Planner per-MIME dispatcher** (`internal/planner/multimodal.go`) — pure-function `MaterializeInputContent(goal, []InputArtifactView, ToolCatalogView) → llm.Content`. Routes: `image/*` inlines bytes as `ImagePart{DataURL}` (Path 1 from D-166 — vision-capable providers see the image directly); `application/pdf` → `FilePart{Artifact}` (Anthropic native PDF; others see the canonical ArtifactStub-JSON text); `audio/*` → `AudioPart{Artifact}`; everything else → `ArtifactStub` text the LLM routes via the catalog with the `Fetch.Tool` hint.
- **Run-loop pre-fetch** — `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` reads `task.InputArtifactIDs`, calls `ArtifactStore.GetRef` for metadata + `Get` for bytes when MIME starts with `image/`, and pre-resolves the `InputArtifactView` slice the planner consumes synchronously. Cleared from `RunSpec.Base.InputArtifacts` after the first step so subsequent turns never re-inline bytes. D-094 mirror in `harbortest/devstack`.
- **Console Playground multimodal end-to-end** — `ControlNamespace.start(query, {inputArtifactIDs})` typed method; `buildChatClient.sendMessage` plumbs the chat-attach uploads through. Round-7 F12: the prior `uploadArtifact` adapter shipped the wrong body (`{filename, mime, size_bytes}` vs `{scope, bytes, opts: {mime_type, filename}}`) and read `resp.id` instead of `resp.ref.id` — every chat-attach had been silently producing empty artifact ids. Fixed wire-shape + `fileToBase64` helper + fail-loud on missing `resp.ref.id`.

### Added — Playground queue-vs-steer when a run is active (round-6 F10)

- Chat composer (`web/console/src/lib/chat/ChatComposer.svelte`) gains a `running` prop and a two-radio mode picker that appears only while a foreground task is in flight: **Queue after current run** (default) stashes the message and dispatches it via `start` as soon as the active task reaches a terminal state; **Steer current run** dispatches the SHIPPED `user_message` control verb (Phase 54) to inject the message into the running task's next planner turn.
- Playground page hooks an `EventSource` lifecycle subscription to `task.completed` / `task.failed` / `task.cancelled` and drains the FIFO queue when `activeTaskID` clears. The SSE envelope's `payload.TaskID` (capital T) is the load-bearing read; an initial draft looked for `task_id` and the queue never drained — caught on a live wire-tap before shipping.

### Added — Runtime capability gate + session aggregates (phase 84a / round-8 F1 + F8)

- **Per-instance capability advertisement** — `runtime.info.capabilities` now reflects what THIS runtime instance has actually wired. `topology_snapshot` is registered in the canonical capability set (the handshake universe) and advertised by a runtime IFF the engine-graph projection accessor is wired. `harbor dev` against a planner/RunLoop agent yaml correctly omits it; a future engine-graph runtime advertises it by setting `PostureDeps.TopologyAvailable=true`. D-094 mirror in `harbortest/devstack`.
- **Console capability gate** — `HarborClient.capabilities()` lazy-fetches the runtime's advertised set at attach time and memoises a frozen `ReadonlySet<string>`. Live Runtime + Playground + Playground's trace toggle gate their `topology.snapshot()` calls behind `caps.has('topology_snapshot')`; on planner/RunLoop runtimes the browser fetch never fires, the friendly info banner renders directly, and the operator's DevTools console stays clean.
- **Console-side session counter enrichment (D-122 compliant)** — Session detail's `tasks_count` + `events_count` are now truthful. The page fetches `tasks.list` (filtered locally by session id) + `events.aggregate` (30-day session-scoped window, single bucket summed across all event types) after `sessions.inspect` and merges into the snapshot. `sessions.inspect` itself stays a pure registry projection — the Console computes the aggregates. `total_tokens` + `total_cost_cents` remain at zero pending the V1.3 `cost.aggregate` work (phase 84b); TODO comment on the page calls out the gap.

### Fixed — Round-6 walkthrough (PR #229)

- **F2 — Sessions catalog empty across reboots** (`Registry.Open` did not hydrate `idIndex` when the StateStore returned an existing-record sentinel). On a `harbor dev` boot against a SQLite state store with a pre-existing dev session, the Sessions page rendered "No sessions match these filters" and `runtime.counters.sessions_active` was zero even with a live session in the store. Fix: hydrate `idIndex` (and `openSessions`, for an open record) before returning `ErrSessionAlreadyOpen` / `ErrReopenAfterClose`. Tests cover both branches.
- **F5 — `tasks.get` crashed the Cost-breakdown rail** (`cost.per_step` returned `null` against a TS contract that declares `TaskCostStep[]`). Go projector normalizes empty `PerStep` to `[]TaskCostStep{}` so the wire honours its shape; the Console rail uses `?.length ?? 0` for defence in depth. Test pin asserts `"per_step":[]` appears in JSON output and `"per_step":null` does not.
- **F6 — Playground composer hidden on empty stream**. `PageState`'s `empty` snippet was swallowing the `ChatPanel` children. `status` now always goes to `'ready'` on a successful load; ChatPanel owns its own empty-state copy + composer. Two previously-skipped Playwright specs un-skipped (the deferred-issue-#178 rationale was the F6 bug shape itself).
- **F7 — sendMessage shipped the wrong wire body**. `dispatch('user_message', sessionID, ...)` treated sessionID as a run id AND used the steering-verb body shape; `start` has a flat shape. New typed `control.start(query, opts)` method ships the correct wire body; both sendMessage + restartRun route through it.
- **F9 — Memory page rendered Go zero-time `0001-01-01`** for nullable `expires_at`. `shortTime` helper now returns `"—"` when the ISO starts with `0001-01-01`, matching the Tools page's existing guard.

### Fixed — bifrost conformance fixture (PR #231)

- **Multimodal conformance probe used a malformed 1×1 PNG** below OpenAI's image-API minimum-pixel threshold, surfacing as a generic "Provider returned error" 400 that looked like a wire-shape bug for months. Swap to a 132-byte 64×64 solid-red PNG (`internal/llm/drivers/bifrost/conformance_test.go::runLiveMultimodal`). All six providers in the matrix + multimodal/claude-haiku-4.5 now pass under `HARBOR_LIVE_LLM=1`. Round-7 multimodal e2e is verified end-to-end: operator uploads a PNG via the Playground composer → bifrost ↔ OpenRouter ↔ claude-haiku-4.5 vision returns "Red".

### Protocol additions

- **Capability `topology_snapshot`** — registered in `internal/protocol/types/version.go::canonicalCapabilities`. Per-instance advertisement in `runtime.info.capabilities` is conditional via the new `PostureDeps.TopologyAvailable` flag; the canonical handshake set is unconditional. RFC §5.3 minor-class additive change — no Protocol version bump.
- **`StartRequest.InputArtifactIDs []string`** — additive field on the existing `start` Protocol method (`internal/protocol/types/control.go`). `omitempty` keeps the wire shape backward-compatible for text-only callers. Round-trip test pins the wire shape.

### Decisions logged

- **D-166** — Playground multimodal artifact input. Three settled calls: (a) Path 1 (runtime inlines image bytes via DataURL) over Path 2 (driver-side resolution); (b) per-MIME dispatcher lives in the planner package, not the run loop or LLM driver; (c) `HandlesMIME` is an opt-in descriptor field with bounded `type/*` wildcards, not a global registry.

### V1.2 / V1.3 staged

- **Phase 84b** (`docs/plans/phase-84b-bifrost-multimodal-v13.md`) — V1.3 bifrost extended multimodal: provider-native file uploads for over-threshold images, native PDF / audio / video / document parts, streaming-with-multimodal, per-MIME conformance probe matrix, `cost.aggregate` follow-up that completes F8's `total_tokens` / `total_cost_cents`. Anchored on the bifrost SDK docs (multimodal + streaming).
- **Phase 85a–85j** (V1.2) — MCP wave. Plans already in the master plan README.

### Notes

Five round-N walkthroughs surfaced this work end-to-end: rounds 3 + 4 + 5 (already in 1.0.0), round-6 (#229), round-7 (#230, #231), round-8 walkthrough → phase 84a (#236), round-9 confirmation. The §17.6 "fix what the test finds" rule pulled latent bugs (F2 across reboots, F12 chat-attach wire shape, the malformed-fixture-masking-a-real-failure pattern) into the same wave they surfaced in — none deferred.

## [1.0.0] — 2026-05-22

The first stable release. The entry below is the complete V1 surface,
grouped by subsystem.

### Added — Identity, configuration, and foundations

- **Identity & isolation triple** — `internal/identity`: the
  `(tenant, user, session)` triple, the `Quadruple` (triple + `run`),
  context carriers, and a conformance suite. Multi-isolation is a Day-1
  guarantee (RFC §4).
- **Configuration loader** — `internal/config`: a typed YAML loader
  (`goccy/go-yaml`), environment overrides, validation, secret
  redaction, and `examples/harbor.yaml`.
- **Audit redactor** — `internal/audit`: a single deep-redaction pass,
  a driver registry, canonical secret rules, and multimodal-aware
  redaction. Every payload is redacted before it is persisted.
- **slog logger + standard attribute set** — `internal/telemetry`: an
  identity-aware structured logger that redacts every record through the
  audit redactor, plus the `BusEmitter` seam for `runtime.error` events.

### Added — Events, state, and sessions

- **Event taxonomy + in-memory `EventBus`** — `internal/events`: a typed
  event bus with server-enforced identity-scoped filtering, drop-oldest
  backpressure with a `bus.dropped` signal, an idle reaper, and
  audit-before-emit.
- **Bus replay + ring buffer + cursor** — bounded replay history with
  cursor-based catch-up.
- **`StateStore` interface + in-memory driver + conformance suite** —
  `internal/state`: a generic `(Quadruple, Kind, Bytes)` surface,
  ULID-keyed idempotency, and a `conformancetest.Run` suite every
  downstream driver inherits.
- **`SessionRegistry` + lifecycle + GC** — `internal/sessions`: session
  creation, lifecycle states, and garbage collection.

### Added — Runtime engine

- **Envelopes, headers, identity quadruple** — `internal/runtime/messages`:
  the message envelope, headers, and `trace_id` propagation.
- **Engine + workers + cycle detection** — `internal/runtime/engine`:
  the node-graph executor with a bounded worker pool and graph cycle
  detection.
- **Reliability shell** — retry / timeout policy wrapping for node
  execution.
- **Streaming + per-run capacity backpressure** —
  `internal/runtime/streaming`: chunked outputs with per-run capacity
  limits and parent-trace correlation.
- **Cancellation + per-run fetch dispatcher** — per-run cancellation
  with no cross-run cancellation cross-talk.
- **Routers + concurrency utilities + subflows** —
  `internal/runtime/routers`, `concurrency`, `playbooks`: routing
  policies, `map_concurrent` / `join_k`, and composable subflows.

### Added — Persistence drivers

- **SQLite `StateStore` driver** — `internal/state/drivers/sqlite`: a
  CGo-free (`modernc.org/sqlite`) driver with forward-only migrations
  and WAL journal mode.
- **Postgres `StateStore` driver** — `internal/state/drivers/postgres`:
  a `pgx`-backed driver with advisory-lock-serialised migrations,
  exercised against `postgres:16` in CI.
- **`ArtifactStore` interface + in-memory + filesystem drivers** —
  `internal/artifacts`: a content-addressed blob store with mandatory
  routing above the heavy-output threshold (no `NoOp` fallback).
- **`ArtifactStore` SQLite-blob + Postgres-blob drivers** — durable
  artifact storage on the persistence triad.
- **`ArtifactStore` S3-style driver** — `internal/artifacts/drivers/s3`:
  an S3-compatible driver, exercised against MinIO in CI.

### Added — Tasks, distributed contracts, and memory

- **`TaskRegistry` interface + in-process driver + lifecycle** —
  `internal/tasks`: a unified foreground/background task service keyed
  by `TaskID`.
- **`TaskGroup` + retain-turn + patches** — task grouping, retain-turn
  semantics, and incremental patches.
- **`MessageBus` + `RemoteTransport` contracts** —
  `internal/distributed`: the V1 in-process loopback driver and the
  contracts a post-V1 durable bus / A2A wire will satisfy.
- **`MemoryStore` interface + in-memory driver + conformance suite** —
  `internal/memory`: session-scoped memory with a conformance suite.
- **Memory strategies** — `truncation` and `rolling_summary`.
- **SQLite + Postgres `MemoryStore` drivers** — durable memory on the
  persistence triad.

### Added — Tools and integrations

- **Tool catalog core + in-process registration + `ToolPolicy`** —
  `internal/tools`: a transport-agnostic tool catalog with identity-
  filtered visibility.
- **Flow-as-Tool registration + per-flow budget** — registering a flow
  as a callable tool with its own budget.
- **HTTP tool driver** — `internal/tools/http`: tools backed by HTTP
  endpoints.
- **MCP southbound driver** — `internal/tools/mcp`: tools sourced from
  MCP servers.
- **A2A southbound driver (full spec)** — `internal/tools/a2a`: tools
  sourced over the A2A protocol.
- **Tool-side OAuth + HITL via pause/resume** — OAuth flows for tools
  routed through the unified pause/resume primitive.
- **Tool-side approval gates** — human-in-the-loop approval gates on
  tool execution.

### Added — LLM client and governance

- **LLM client core + `StreamSink` contract + context-window safety
  net** — `internal/llm`: the LLM client surface, streaming sink, and
  the always-on heavy-content leak guard (`ErrContextLeak`).
- **bifrost integration** — `internal/llm/drivers/bifrost`: the
  production LLM driver.
- **Custom OpenAI-compatible providers + per-provider timeouts** —
  arbitrary OpenAI-API-compatible endpoints with per-provider timeout
  configuration.
- **Provider correction layer + `SchemaSanitizer`** — a single,
  baked-in correction mode for provider quirks.
- **Structured output strategies + downgrade chain** — structured-output
  enforcement with a graceful downgrade chain.
- **Retry with feedback** — retry of malformed LLM responses with
  corrective feedback, failing loudly with `ErrRetryExhausted`.
- **Cost accumulator + per-identity ceilings** — `internal/governance`:
  per-identity cost ceilings.
- **Per-identity rate limits + per-call `MaxTokens`** — per-identity
  rate limiting and per-call token caps.

### Added — Skills subsystem

- **Skill store + LocalDB driver + FTS5 ladder** — `internal/skills`: a
  DB-backed, token-savvy skill catalog with a full-text-search ranking
  ladder.
- **Skill planner tools** — `skill_search` / `skill_get` / `skill_list`
  exposed to the planner.
- **Virtual directory subsystem** — a virtual filesystem view over the
  skill catalog.
- **Skills.md importer** — importing skills from a `Skills.md` manifest,
  with path-traversal-safe normalisation.
- **In-runtime skill generator with persistence** — generating and
  persisting new skills at runtime.

### Added — Planner subsystem

- **Planner interface + Decision sum + RunContext** — `internal/planner`:
  the one `Planner` interface, the Decision sum type, and the per-run
  `RunContext`. The planner is swappable; the Runtime owns mechanism.
- **Trajectory + fail-loudly `Serialize` contract** — the `Trajectory`
  type, whose `Serialize` raises `ErrUnserializable` rather than
  silently dropping context.
- **Schema repair pipeline** — `internal/planner/repair`: salvage →
  schema repair → graceful failure → multi-action salvage.
- **Reference ReAct planner** — `internal/planner/react`: the reference
  planner, shipped in the box.
- **Trajectory compression / summariser** — trajectory compaction for
  long runs.
- **Parallel-call executor + ReAct `CallParallel` / `SpawnTask` /
  `AwaitTask` emission** — parallel tool calls and background-task
  spawn/await as a twinned pair.
- **Deterministic planner** — a second concrete planner that proves the
  `Planner` interface holds.
- **Planner conformance pack** — a conformance suite every planner
  concrete must pass.

### Added — Steering, pause/resume, and the Agent Registry

- **Pause/Resume Coordinator + handle registry** —
  `internal/runtime/pauseresume`: Harbor's one pause/resume primitive,
  serving HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`, and
  operator/Console `PAUSE`.
- **Pause-state serialise contract** — fail-loud pause-state
  serialisation (`ErrUnserializable`, never a half-persisted
  checkpoint).
- **Steering inbox + control taxonomy** — the steering inbox and the
  nine-type control taxonomy.
- **Steering wiring (9 control events)** — `INJECT_CONTEXT`, `REDIRECT`,
  `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`,
  `USER_MESSAGE` wired end-to-end.
- **Agent Registry** — `internal/runtime/registry`: registration
  identity, the three-ID model (`agent_id`, `version_hash`,
  `incarnation`), and `agent.*` events.

### Added — Observability and the durable event log

- **Protocol task control surface** — the start/cancel/pause/resume/
  redirect/inject control surface.
- **OTel traces + propagation** — `internal/telemetry`: OpenTelemetry
  tracing baked in from the start, with trace-context propagation.
- **Metrics + OTLP + Prometheus drivers** — OTLP-push and Prometheus-
  pull metric exporters.
- **Durable event log driver** — `internal/events/drivers/durable`: a
  StateStore-backed durable event log (load-bearing for post-V1
  replay-based evaluation).

### Added — Harbor Protocol

- **Protocol types/methods/errors single source** — `internal/protocol`:
  the canonical wire-type / method / error-code home, lint-enforced as
  the single source.
- **Protocol versioning + deprecation policy** — the parsed `Version`
  (semver, same-major `Compatible`), the structured `Deprecation` note
  format, and the `Capability` + `VersionHandshake` negotiation shape.
- **Protocol wire transport (SSE + REST)** —
  `internal/protocol/transports`: SSE for the event stream, REST/JSON
  for the control surface.
- **Protocol auth + identity-scope enforcement** — JWT (asymmetric
  algorithms only) identity-scope enforcement at the Protocol edge.
- **Protocol conformance suite** — a conformance suite for the Protocol
  surface.

### Added — Harbor CLI

- **`harbor` binary** — `cmd/harbor`: a cobra-rooted CLI with global
  `--quiet` / `--json` flags.
- **`harbor dev`** — boots the local Runtime + Protocol surface, with
  hot-reload on agent-source change and draft-save scaffolding.
- **`harbor console`** — serves the Harbor Console (baked into the
  binary) against a co-resident Runtime.
- **`harbor scaffold`** — scaffolds a new agent project.
- **`harbor validate`** — validates a Harbor config; wired into CI as a
  pre-flight check for the example configs.
- **`harbor version`** — reports the product release version and the
  Harbor Protocol version as distinct fields.
- **`harbor inspect-events` / `inspect-runs` / `inspect-topology`** —
  inspect the event stream, run history, and runtime topology.
- **`harbortest` test kit package** — `harbortest/`: an operator-
  importable public test kit (`RunOnce`, `AssertSequence`,
  `AssertNoLeaks`, `SimulateFailure`, `RecordedEvents`).

### Added — Harbor Console

- **Console subscription protocol surface** — the `events.subscribe`
  Protocol surface the Console consumes, with filter extensions and an
  `events.aggregate` time-bucket method.
- **Runtime / governance / LLM posture surfaces** — the read-only
  `runtime.*`, `metrics.*`, `governance.posture`, `llm.posture`, and
  `pause.list` Protocol methods.
- **Console DB local schema + SvelteKit scaffold** — `web/console`: the
  Console-local schema and the SvelteKit (Svelte 5 runes) application.
- **Console pages** — Overview, Live Runtime, Sessions, Tasks, Agents,
  Tools, MCP Connections, Background Jobs, Events, Flows, Memory,
  Artifacts, Settings, and Playground — fourteen pages, each a Protocol
  client that never reads a Runtime object directly.
- **Console state inspection + topology projection** — the
  state-snapshot Protocol surface and the topology projection events
  behind the Console topology view.
- **Console e2e Playwright harness** — the Playwright e2e suite, gated
  by the `frontend-e2e` CI job.

### Added — Conformance harnesses, benchmarks, and release engineering

- **Cross-tenant isolation conformance harness** — `test/integration`:
  a `-race` harness running concurrent sessions and asserting no
  cross-tenant or cross-session leak. The integrity gate.
- **Goroutine leak conformance harness** — a `-race` harness that
  constructs, exercises, and tears down every long-lived component and
  asserts the goroutine count returns to baseline.
- **Chaos / fault injection harness** — a `-race` harness that injects
  five failure modes (kill mid-run, dropped messages, provider quirks,
  StateStore disconnect, pause-deserialize failure) and asserts each
  produces its documented loud error and recovery path.
- **Performance benchmarks** — `test/benchmarks`: a `go test -bench`
  suite over the hottest runtime seams, with a CI perf-regression gate.
- **Documentation hygiene** — an enforced `golangci-lint` gate (godoc /
  package-comment + the full linter set), worked examples under
  `examples/`, and recipe how-to guides under `docs/recipes/`.
- **Release engineering** — build-time product-version stamping via
  `-ldflags -X` (`harbor version` reports it); `scripts/release-build.sh`
  and `scripts/release-dryrun.sh` with the `make release-build` /
  `make release-dryrun` targets; and the `release.yml` workflow that, on
  a `v*` tag push, builds the CGo-free static binary, emits a SHA-256
  checksum, attaches SLSA-style build provenance, and publishes a GitHub
  Release.

[1.0.0]: https://github.com/hurtener/Harbor/releases/tag/v1.0.0
