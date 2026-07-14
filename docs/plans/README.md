# Harbor — Master Phase Plan

## How to read this file

This is the canonical execution index for Harbor's V1 build. Every individual phase plan (`docs/plans/phase-NN-<slug>.md`) lives under it and inherits its done-definition, dependency declarations, and coverage discipline.

- **Source of truth:** `/RFC-001-Harbor.md` (referenced as RFC §X.X). Every phase below traces to one or more RFC sections; if a phase plan and the RFC drift, the RFC wins (`AGENTS.md` §2).
- **Research substrate:** the eleven briefs in `docs/research/01..11.md` (canonical index: `docs/research/INDEX.md`). Decisions on shape, sharp edges, and Go-flavored types come from there.
- **Numbering:** `phase-NN-<slug>.md`, two-digit zero-padded; lettered suffixes (`26a`, `33a`, `36a`, `36b`, `53a`, `64a`, `83a`–`83e`, `85a`–`85j`, `85k`, `85m`) insert work into an existing band without renumbering. Phases 01–82 + 26a + 33a + 36a + 36b + 53a + 64a are V1; 83–100 + 83a–e + 85a–j + 85k + 85m are post-V1 follow-ups listed for completeness so we don't lose track. The integer phase **85 (Skills Portico provider driver) was removed** — Portico is an MCP gateway and speaks MCP like any server, so the generic MCP client driver is its consumer; the 85-band is now MCP client/host compliance (85a–j + 85m; 85k is Harbor agent-builder skills). Per the MCP 2026-07-28 RC re-plan (2026-05-28), phases 85c / 85e / 85h / 85i are **Cut** and 85m is **new**; see the 85-band detail block. See brief 14.
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
| 25a| Durable memory strategies (truncation + rolling_summary on SQL drivers; Summarizer through `memory.Open`) | memory | §6.6, §9 | 23, 24, 25, 15, 16 | n/a | Shipped (V1.1.x) |
| 26 | Tool catalog core + InProcess registration    | tools                | §6.4        | 01, 05, 09            | 85%  | Shipped  |
| 26a| Flow-as-Tool registration + per-flow Budget   | runtime/flow + tools | §6.1, §6.4  | 14, 26                | 85%  | Shipped  |
| 26b| Per-MCP-server + per-tool tool-policy config (`policy:` / `tool_policies:`) | tools + config | §6.4 | 26, 28 | n/a | Shipped (V1.1.x) |
| 27 | HTTP tool driver                              | tools/http           | §6.4        | 26                    | 85%  | Shipped  |
| 28 | MCP southbound driver                         | tools/mcp            | §6.4        | 26                    | 80%  | Shipped  |
| 29 | A2A southbound driver (full spec)             | tools/a2a            | §6.4        | 26, 22                | 80%  | Shipped  |
| 30 | Tool-side OAuth + HITL via pause/resume       | tools/auth           | §6.4, §3.3  | 26, 50, 53a           | 85%  | Shipped  |
| 31 | Tool-side approval gates                      | tools/approval       | §6.4, §3.3  | 30                    | 80%  | Shipped  |
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
| 55 | OTel traces + propagation conventions         | telemetry            | §6.14       | 04, 05                | 85%  | Shipped  |
| 56 | Metrics + OTLP + Prometheus drivers           | telemetry            | §6.14, §11Q5| 55, 05                | 85%  | Shipped  |
| 57 | Durable event log driver (StateStore-backed)  | events               | §6.13       | 05, 07, 15, 16        | 85%  | Shipped  |
| 58 | Protocol types/methods/errors single source   | protocol             | §5, §8      | 01                    | 90%  | Shipped  |
| 59 | Protocol versioning + deprecation policy      | protocol             | §5.3        | 58                    | 85%  | Shipped  |
| 60 | Protocol wire transport (SSE + REST)          | protocol             | §5.4, §11Q1 | 58, 05                | 85%  | Shipped  |
| 61 | Protocol auth + identity-scope enforcement    | protocol             | §5.5, §4    | 58, 60, 01            | 90%  | Shipped  |
| 62 | Protocol conformance suite                    | protocol             | §5          | 58, 60, 61            | 85%  | Shipped  |
| 63 | Harbor CLI skeleton (`harbor` + cobra)        | cmd/harbor           | §8          | 60                    | 70%  | Shipped  |
| 64 | `harbor dev` v1 (boot runtime + protocol)     | cmd/harbor           | §8          | 63, 60                | 75%  | Shipped  |
| 64a | Tool catalog OAuth + approval wiring         | tools/catalog        | §6.4        | 26, 30, 31, 50, 64    | 80%  | Shipped  |
| 65 | `harbor dev` hot-reload                       | cmd/harbor           | §8          | 64                    | 75%  | Shipped  |
| 66 | `harbor dev` draft-save scaffolding           | cmd/harbor           | §8          | 64                    | 75%  | Shipped  |
| 67 | `harbor scaffold`                             | cmd/harbor           | §8          | 63                    | 70%  | Shipped  |
| 68 | `harbor validate`                             | cmd/harbor           | §8          | 63, 02                | 75%  | Shipped  |
| 69 | `harbor inspect-events / inspect-runs`        | cmd/harbor           | §8          | 63, 60                | 70%  | Shipped  |
| 70 | `harbor inspect-topology` (ASCII renderer)    | cmd/harbor           | §8          | 63, 60                | 70%  | Shipped  |
| 71 | `harbortest` test kit package                 | testing              | §6.13       | 05, 09, 07            | 85%  | Shipped  |
| 72 | Console subscription protocol surface         | protocol             | §5.2, §7    | 60, 05, 06            | 85%  | Shipped  |
| 72a| `events.subscribe` filter ext + `events.aggregate` | protocol+events | §5.2, §6.13 | 60, 61, 72            | 85%  | Shipped  |
| 72b| `IdentityScope` admin-impersonation extension | protocol             | §5.5, §7    | 60, 61                | 89%  | Shipped  |
| 72c| `search.*` cluster (5 methods)                | protocol+search      | §5.2, §7    | 60, 61, 08, 20, 05    | 85%  | Shipped  |
| 72d| `notification.*` event topic + mapper         | protocol+events      | §5.2, §6.13 | 05, 06, 20            | 85%  | Shipped  |
| 72e| `pause.list` snapshot Protocol method          | protocol             | §5.2, §6.3  | 50, 60, 61, 17        | 90%  | Shipped  |
| 72f| Runtime posture surface (`runtime.*`/`metrics.snapshot`) | protocol  | §5.3, §6.15, §7 | 60, 61, 56            | 85%  | Shipped  |
| 72g| `governance.posture` + `llm.posture`          | protocol             | §5.5, §6.15 | 36a, 36b, 64, 72f     | 85%  | Shipped  |
| 72h| Console DB local schema + SvelteKit scaffold  | web/console          | §7          | 60                    | 85%  | Shipped  |
| 73 | Console state inspection surface              | protocol             | §5.2, §7    | 60, 07, 17            | 85%  | Shipped* |
| 73l| Console Artifacts page                        | web/console          | §5.2, §6.10, §7 | 73, 75            | 80%  | Shipped  |
| 73i| Console Flows page (Protocol + UI)            | protocol+web/console | §5.2, §6.1, §7 | 73, 75, 26a        | 85%  | Shipped  |
| 73g| Console Events page                           | web/console          | §5.2, §6.13, §7 | 72a, 73, 75       | 80%  | Shipped  |
| 73c| Console Sessions page (Protocol + UI)         | protocol+web/console | §5.2, §6.9, §7 | 08, 60, 61, 72a, 72b, 72c, 75 | 80%  | Shipped  |
| 74 | Console topology projection events            | protocol             | §5.2, §6.13 | 05, 09                | 85%  | Shipped  |
| 75 | Console e2e Playwright harness baseline       | testing              | §7          | 60, 72                | n/a  | Shipped  |
| 73k| Console MCP Connections page                  | web/console          | §6.4, §7    | 28, 30, 50, 60, 61, 64a, 72a, 75 | 80%  | Shipped  |
| 73d| Console Tasks page (kanban + bulk control)    | protocol+web/console | §5.2, §6.8, §7 | 20, 21, 54, 60, 61, 72c, 75 | 85%  | Shipped  |
| 73b| Console Live Runtime page (Protocol + UI)     | protocol+web/console | §5.2, §6.3, §6.13, §7 | 60, 61, 72a, 73, 73i, 74, 75 | 85%  | Shipped  |
| 73n| Console Playground page (Protocol + UI)       | protocol+web/console | §5.1, §6.4, §6.13, §7 | 54, 60, 61, 72b, 73l, 74, 75 | 85%  | Shipped  |
| 73a| Console Overview page (composition-only UI)   | web/console          | §5.2, §6.13, §6.15, §7 | 54, 60, 61, 72a, 72e, 72f, 73d, 75 | 70%  | Shipped  |
| 73m| Console Settings page + `harbor console` subcommand | protocol+web/console+cmd | §5.3, §5.5, §6.15, §7 | 72d, 72f, 72g, 72h, 75 | 75%  | Shipped  |
| 75a| Console e2e Playwright wave-end suite          | testing              | §7          | 75, 73a-73n           | n/a  | Shipped  |
| 76 | Cross-tenant isolation conformance harness    | testing              | §4.3        | 07, 17, 23, 37, 20    | 95%  | Shipped  |
| 77 | Goroutine leak conformance harness            | testing              | §5(Go)      | 10, 13, 50            | n/a  | Shipped  |
| 78 | Chaos / fault injection harness               | testing              | n/a         | 76, 77                | n/a  | Shipped  |
| 79 | Performance benchmarks                        | testing              | n/a         | 10, 12, 05            | n/a  | Shipped  |
| 80 | Documentation hygiene polish (godoc, recipes) | docs                 | §2          | all V1                | n/a  | Shipped  |
| 81 | Release engineering (versioning, changelog)   | release              | §12         | all V1                | n/a  | Shipped  |
| 82 | V1 cut                                        | release              | §1, §12     | 81                    | n/a  | Shipped  |
| 83 | Auto-sequence detection (planner opt.)        | planner              | §12         | 45                    | n/a  | Post-V1  |
| 83a| ReAct prompt structured sections              | planner/react        | §6.2        | 45                    | 85%  | Shipped  |
| 83b| ReAct tool schema injection (catalog rendering)| planner/react       | §6.2, §6.4  | 83a, 26               | 85%  | Shipped  |
| 83c| ReAct dynamic repair guidance + planning hints | planner/react       | §6.2        | 83a, 44, 05           | 85%  | Shipped  |
| 83d| ReAct skills + memory injection (UNTRUSTED)   | planner/react        | §6.2, §6.6  | 83a, 23, 37           | 85%  | Shipped  |
| 83e| ReAct reasoning channel decoupling            | planner/react+llm    | §6.2, §6.5  | 45, 32, 33, 44        | 90%  | Shipped  |
| 83f| Dev RunLoop populates 83-band RunContext      | runtime/dev          | §6.2, §6.6  | 83c, 83d, 23, 37, 20  | 80%  | Shipped  |
| 83g| MCP southbound consumer in harbor dev         | runtime/dev          | §6.4        | 28, 26                | 80%  | Shipped  |
| 83h| Dev-binary fixes (hot-reload sqlite + LLM Model)| runtime/dev + llm  | §6.5, §8    | 83g, 64, 32           | 80%  | Shipped  |
| 83i| RunContext wiring closure (Catalog/Trajectory/Memory/Emit)| runtime/dev + steering | §6.2, §6.6, §6.8 | 83f, 83g, 83h, 26, 23 | 80% | Shipped  |
| 83n| `harbor init` + tiered yaml + docs/CONFIG.md + built-in tools | cli + tools/builtin | §8, §6.4  | 67, 63, 26            | 85%  | Shipped  |
| 83o| scaffold reads operator yaml + per-custom-tool Go stubs + --patch | cli/scaffold + config | §8, §6.4  | 67, 83n, 26       | 85%  | Shipped  |
| 83l| real-bifrost integration tests + snapshot CustomProviders bug fix | test/integration + cli | §6.5     | 33, 33a, 45, 83h, 83i | 80% | Shipped  |
| 83m| WARN cleanup band (MCP push id, sqlite watcher, closers, skills kw, llm timeout, scopes, tool_count, reasoning) | cmd/harbor + mcp + llm + tasks + steering + planner | §6.2, §6.4, §6.5, §6.8, §8 | 83g, 83h, 83i, 83l | 85% | Shipped |
| 83k| Console release embed (make build + release pipeline rebuild Console; staleness gate; placeholder copy) | cmd/harbor + Makefile + release pipeline | §5, §8 | 73m, 81, 83n | n/a | Shipped |
| 83p| Settings two-group layout (console-local always; runtime-posture wrapped) — closes walkthrough F1 | web/console + Settings page | §5, §8 | 73m, 73p | n/a | Shipped |
| 83q| Playground sidebar nav + breadcrumb case — closes walkthrough F2 + N1   | web/console + (console) layout | §5, §7 | 73n           | n/a | Shipped |
| 83r| Disconnected-state hygiene + isDisconnected() predicate — closes W1/W2/W3 + N4/N5/N8/N9/N10 | web/console (cross-page) | §5    | 73m, 73p, 83p     | n/a | Shipped |
| 83s| Saved-views label "Save view" + per-page footer dedup — closes N2 + N7  | web/console (cross-page) | §5         | 73m, 73p           | n/a | Shipped |
| 83u| Console DB chicken-and-egg fix — attachConnection() + best-effort DB upsert (closes round-2 F3) | web/console + Settings page | §5, §7 | 73m, 73p, 83p | n/a | Shipped |
| 83v| Runtime CORS allowlist — default-deny + per-origin echo + dev-only escape (closes round-2 F4) | internal/protocol/transports + config + cmd/harbor | §5, §7 | 60 | 90% | Shipped |
| 83w| Wire-surface gaps — friendly unknown_method info banner (F5) + mcp.servers.list (F6) | web/console + cmd/harbor + mcpconsole | §5, §6.4, §7 | 83g, 83m, 73k | n/a | Shipped |
| 83x| Real-data layout polish — W4-W11 + N11-N14 (incl. W6 created_at + W8 session-row Go fixes) | web/console + cmd/harbor + internal/protocol/artifacts | §5, §6.4, §6.6, §6.10, §6.13 | 73m, 73p, 83i, 83m | n/a | Shipped |
| 84 | Reflection / critique loop                    | planner              | §12         | 45                    | n/a  | Post-V1  |
| 84a| Runtime-capability gate + session aggregates (round-8 F1+F8 closeout) | internal/protocol + web/console | §5.3, §6.4, §7 | 72f, 73c, 73d, 72b, 83w | 90% | Shipped |
| 84b| Multimodal attachment disposition policy (mechanism→policy; default `ref`) | internal/planner + internal/config + internal/protocol + web/console | §6.4, §6.5, §6.10 | F11/D-166, 107c | n/a | Shipped (V1.1.x) |
| 84c| Provider-native multimodal mechanism (image/audio/video first, files/PDF last; opt-in via 84b) | llm/drivers/bifrost + planner | §6.5, §6.10, §11Q3 | 84b, 107, 32 | n/a | Shipped (V1.1.x) |
| 84d| Embedding client (`Embedder`→bifrost) + semantic memory & skill retrieval (opt-in) | internal/embeddings + internal/memory + internal/skills | §6.5, §6.6, §6.7 | 32, 23, F11/84b | n/a | Shipped (V1.1.x) |
| 84e| Semantic memory consumption in the run loop (`SearchTurns` recall → `<read_only_external_memory>`; opt-in via 84d) | internal/runtime/runctx + internal/memory + internal/config + cmd/harbor + harbortest | §6.2, §6.5, §6.6 | 84d, 83d, 83f, 110b, 110c, 107c | n/a | Shipped (V1.1.x) |
| 85a| MCP client core-compliance fixes (roots-empty now permanent) | tools/mcp | §6.4    | 28                    | 85%  | Ready now |
| 85b| MCP HTTP OAuth (RFC 9728 + 8707 + RC auth SEPs)| tools/mcp+auth       | §6.4, §3.3  | 28, 30, 50            | 85%  | Ready now (scope ↑) |
| 85c| ~~MCP sampling provider~~                     | tools/mcp+llm        | §6.4, §6.5  | 28, 32, 50            | —    | Cut — RC deprecates sampling |
| 85d| MCP elicitation provider (RC `InputRequiredResult` shape) | tools/mcp | §6.4, §3.3  | 28, 50, 85m           | 85%  | Revisit after SDK-RC |
| 85e| ~~MCP roots provider~~                        | tools/mcp            | §6.4        | 28, 85a               | —    | Cut — RC deprecates roots |
| 85f| MCP remaining server features (sans logging)  | tools/mcp            | §6.4        | 28, 85a               | 85%  | Ready now (slim) |
| 85g| ~~MCP Apps host (Console `ui://` renderer)~~  | web/console          | §6.4, §7    | 28, 85a               | —    | Deprecated → superseded by 109a–c (D-172) |
| 85h| ~~MCP Tasks wire types (hand-transcribed)~~   | tools/mcp            | §6.4        | 28                    | —    | Cut — RC redesigns Tasks |
| 85i| ~~MCP Tasks client~~                          | tools/mcp            | §6.4        | 85h, 28               | —    | Cut — RC redesigns Tasks |
| 85j| MCP client conformance + compliance statement (target: RC) | tools/mcp + docs | §6.4 | 85a, 85b, 85d, 85f, 85g, 85m | 85%  | Revisit after RC-final |
| 85m| MCP 2026-07-28 RC adoption (sessions, headers, errors, schema, cache, trace) | tools/mcp | §6.4 | 28, 85a | 85% | Revisit after SDK-RC |
| 85k| Harbor agent-builder skills (adoption surface, ~10 SKILL.md playbooks; MCP wiring is one of them) | docs/skills + scripts | §1, §7, §6.4 | V1.1 closure, 85a (for the MCP skill), sibling Dockyard `skills/` | n/a | Pending (V1.1.x) |
| 86 | Durable distributed bus driver                | distributed          | §6.12, §12  | 22                    | 85%  | Shipped  |
| 87 | Durable TaskService backend                   | tasks                | §6.8, §12   | 20, 22                | 85%  | Shipped  |
| 86a| Distributed task dispatcher (the MessageBus consumer) + multi-worker deployment | distributed + tasks + runtime | §6.8, §6.12, §12 | 86, 87 | 85% | Post-V1 |
| 88 | Episodic memory tier                          | memory               | §6.6, §11Q4 | 24, 25                | n/a  | Post-V1  |
| 89 | A2A northbound (Harbor as A2A server)         | tools/a2a            | §6.4, §11Q2 | 29                    | n/a  | Post-V1  |
| 90 | Additional planner concretes                  | planner              | §12         | 49                    | n/a  | Post-V1  |
| 91 | Console-driven key rotation (`governance.rotate_key`) | governance    | §6.15       | 36a, 60, 73           | 85%  | Shipped  |
| 92 | Admin-set tenant-default LLM overrides         | governance           | §6.15       | 36a, 60, 73           | 85%  | Shipped  |
|92b | Tenant-override completion (session swap field + Console admin UI + multi-replica freshness) | governance/console | §6.15, §7 | 92 | 85% | Shipped |
|92a | Agent-config control plane: versioned desired-state registry + Protocol diff/rollback (the primitive) | agentcfg | §6.15, §6.16, §7.4 | 86, 87, 92b, 53a, 110a | 85% | Shipped |
|92c | Agent-config: skills control (first consumer of 92a) | agentcfg/skills | §6.7, §6.16 | 92a, 37 | 85% | Shipped |
|92d | Agent-config: MCP pause/resume + per-tool policy (next-turn projection) | agentcfg/tools | §6.4, §6.16 | 92a, 110a, 109i | 85% | Shipped |
|92e | Agent-config: layered system prompt (operator base + user layer) | agentcfg/prompt | §6.2, §6.16 | 92a, 83a, 92b | 85% | Shipped |
|92f | Agent-config: add a new MCP connection (async dial + handshake + OAuth) | agentcfg/tools | §6.4, §7.4 | 92a, 92d, 28, 30, 50 | 85% | Shipped |
|92g | Agent-config: session-user safe subset (user prompt layer + already-allowed source toggles + ephemeral skills) | agentcfg | §6.16, §5.5 | 92a, 92c, 92d, 92e | 85% | Shipped |
|92h | Console: agent-config control panel (revisions/diff/rollback + skills + MCP policy + prompt + add-connection) | web/console | §7, §6.16 | 92a, 92c, 92d, 92e, 92f | n/a | Shipped |
|92j | Agent-config: per-agent LLM parameters (versioned model/temperature/max-tokens/reasoning-effort section; precedence session › per-agent › tenant-wide baseline › config) | agentcfg/governance | §6.15, §6.16, §6.5 | 92a, 92, 92b, 92d, 92e, 110a | 85% | Shipped |
|92i | Console: agent-config revision UX — derived per-revision change summary + diff-before-rollback + atomic multi-section Save-all | web/console | §7, §6.16 | 92a, 92h, 92j | n/a | Shipped |
|92k | Runtime MCP OAuth: `auth.Provider` runtime config registration seam | tools/auth | §6.4, §3.3 | 30, 50 | 85% | Pending |
|92l | Runtime MCP OAuth: MCP transport agent-bound token + typed `ErrAuthRequired` | tools/mcp, tools/auth | §6.4, §3.3 | 28, 30, 92k | 85% | Pending |
|92m | Runtime MCP OAuth: `add_mcp_connection` OAuth config + `InitiateFlow` parking | agentcfg/tools | §6.4, §6.16, §3.3 | 92f, 92k, 92l | 85% | Pending |
|92n | Runtime MCP OAuth: resume-completes-attach bridge (closes #375 gap 1) | agentcfg/tools | §3.3, §6.16 | 92m, 50 | 85% | Pending |
|92o | Runtime MCP OAuth: run-start connection reconciliation (closes #375 gap 2) | agentcfg/projection | §6.16, §6.4 | 92m | 85% | Pending |
|92p | Runtime MCP OAuth: spec-faithful discovery (401 → RFC 9728 → AS) | tools/mcp, tools/auth | §6.4, §3.3 | 92l, 92m | 85% | Pending |
|92q | Runtime MCP OAuth: Console advisory + wave-end live E2E + §17.5 audit | web/console, integration | §6.16, §6.4 | 92k, 92l, 92m, 92n, 92o, 92p | n/a | Pending |
| 93 | Failover chains as Harbor policy              | governance           | §6.15       | 36a, 33               | n/a  | Post-V1  |
| 94 | Provider circuit breakers (provider, key)     | governance           | §6.15       | 33, 93                | n/a  | Post-V1  |
| 95 | LLM cache (exact-match + semantic)            | governance/cache     | §6.15       | 33                    | n/a  | Post-V1  |
| 96 | PII redaction at the LLM boundary             | audit                | §6.15       | 03, 33                | n/a  | Post-V1  |
| 97 | Media-input tool wrappers                     | tools/media          | §6.5, D-021 | 17, 26, 33            | n/a  | Post-V1  |
| 98 | Media-output tool wrappers                    | tools/media          | §6.5, D-021 | 17, 26, 33            | n/a  | Post-V1  |
| 99 | Vision-aware memory summarization             | memory               | §6.6, D-021 | 24, 33, 97            | n/a  | Post-V1  |
|100 | Recipe loader (declarative YAML flows)        | runtime/flow/recipe  | §6.1, D-023 | 26a                   | n/a  | Post-V1  |
|101 | GitHub Actions Node 24 modernisation          | .github/workflows    | §12         | 81                    | n/a  | Shipped (V1.1.x) |
|102 | Godoc hygiene — strip internal phase jargon   | internal/ + cmd/     | §1, §12     | (none hard)           | n/a  | Shipped (V1.1.x) |
|103 | GitHub Pages docs site (Dockyard parity)      | docs/site + workflows| §1, §7, §12 | 85k (102 soft — see D-208) | n/a  | Shipped (V1.3) |
|104 | Composable resilient flows — value proposition| RFC §1 + README + docs/skills | §1, §6.1 | 85k                   | n/a  | Pending (V1.1.x) |
|105 | Console first-attach UX (zero-clicks-to-attached) | web/console + cmd/harbor + internal/server | §1, §7 | 85k, 73m | n/a | Shipped |
|106 | Playground displays the real assistant response | cmd/harbor + internal/tasks + web/console | §1, §6.5, §7 | 73 | n/a | Shipped |
|107 | Streaming completion pipeline (bifrost → events bus → Playground) | internal/llm + internal/planner + cmd/harbor + web/console | §1, §6.5, §7 | 106, 105, 84b, 83e | n/a | Shipped |
|107a| Reasoning trace projection (`tasks.get` enricher + Playground accordion) | internal/protocol + internal/tasks + cmd/harbor + web/console | §1, §6.5, §6.8, §7 | 73d, 83e, 106 | n/a | Shipped |
|107b| Streaming answer extractor (React planner `streamAnswerFilter`) | internal/planner/react | §1, §6.2, §6.5, §7 | 107, 83a, 83b, 83c, 83d, 83e | n/a | Superseded by 107c (not shipped) |
|107c| Native tool-calling + deferred tools/skills + search meta-tools (alt to 107b — collapses Path B into one wave) | internal/llm + internal/tools + internal/planner/react + internal/config + cmd/harbor | §1, §6.2, §6.4, §6.5, §6.7, §7 | 107, 83a, 83b, 83c, 83d, 83e, 83n, 37, 26, 32, 33, 33a | n/a | Shipped |
|107d| Native parallel tool-calls (dev executor `CallParallel` branch + React `CallParallel` emission + default flip; closes 107c's serialization carve-out) | cmd/harbor + internal/runtime/parallel + internal/planner/react + internal/config | §6.2, §6.5 | 107c, 47, 83i | n/a | Shipped (V1.1.x) |
|107e| SpawnTask + AwaitTask dev-executor dispatch (background-task execution; closes the last `ErrDecisionShapeUnsupported` carve-out) | cmd/harbor + internal/config | §6.2, §6.5, §6.8 | 107c, 47, 83i, 83f | n/a | Shipped (V1.1.x) |
|107f| Session artifact manifest (read-only `<session_artifacts>` prompt block + provenance canonicalisation) | internal/planner + cmd/harbor + internal/protocol + internal/runtime/flow | §6.2, §6.4, §6.5 | 107c, 17, 33 | n/a | Shipped (V1.1.x) |
|108 | Playground page polish + Console shell layout (first of 14 page-polish phases) | web/console | §1, §7 | 73n, 105, 106 | n/a | Pending (V1.1.x) |
|109a| MCP Apps runtime + Protocol surface (`_meta.ui.resourceUri` parse, `ui://` projection, `mcp.servers.read_resource`, real DisplayMode negotiation, app-tool-call proxy) | internal/tools/drivers/mcp + internal/protocol + cmd/harbor | §6.4, §6.5, §7 | 28, 85a, 84a | n/a | Shipped (V1.1.x) |
|109b| Console MCP Apps host (sandboxed iframe + CSP + official AppBridge in manual-handler mode + inline DisplayMode) | web/console | §6.4, §7 | 109a, 73n, 108 | n/a | Shipped (V1.1.x) |
|109c| MCP Apps DisplayMode layout (fullscreen tab + pip 50/50 split + rail toggle) | web/console | §7 | 109b | n/a | Shipped (V1.1.x) |
|109d| Inline MCP-app discovery (`mcp.app_available` event + `MCPAppRef.server_id` + ChatMessage app ref + MessageBubble renderer mount) | internal/tools/drivers/mcp + internal/protocol + web/console | §6.4, §6.5, §7 | 109a, 109b, 109c | 85% | Shipped (V1.1.x) |
|109e| MCP App discovery reads the tool-DEFINITION `_meta.ui` (spec-conformance fix — discovery fires against real ext-apps servers; live-test-found) | internal/tools/drivers/mcp | §6.4, §6.5, §7 | 109a, 109d | 85% | Shipped (V1.1.x) |
|109f| Render heavy MCP App documents (fetch the offloaded artifact) + operator "pop to side-by-side" affordance (live-test-found) | web/console | §6.4, §6.5, §7 | 109a, 109b, 109c, 109d | n/a (Console) | Shipped (V1.1.x) |
|109g| MCP App documents render inline on every artifact driver (`read_resource` scopes the heavy threshold out of `ui://` app docs; live-test-found) | internal/mcpconsole | §6.5, §7 | 109a | 55% | Shipped (V1.1.x) |
|109h| MCP Apps UI-host capability advertisement (the driver advertises `io.modelcontextprotocol/ui` displayModes on the initialize handshake — the write side of `negotiateDisplayModes` — preserving roots) | internal/tools/drivers/mcp + internal/config | §6.4, §7 | 109a | n/a | Shipped (V1.1.x) |
|109i| MCP Apps tool-context capture + `mcp.apps.tool_context` (the Data-Delivery backend — capture input+result behind a declared `ui://` app, identity-scoped read) | internal/tools/drivers/mcp + internal/mcpconsole + internal/protocol + cmd/harbor | §6.4, §6.5, §7 | 109a, 109d, 109g | 85% | Shipped (V1.1.x) |
|109j| Console pushes tool-input/tool-result into the app after `ui/initialize` (official AppBridge `sendToolInput`/`sendToolResult`) — the Data Delivery Console half | web/console | §6.4, §7 | 109i, 109b | n/a (Console) | Reverted (#346 — handshake regression; re-land #347) |
|109k| MCP Apps spec-conformance hardening (wave-end audit fixes: `mimeTypes` UI capability not `displayModes`; server-namespaced app→host tool calls; size-changed + teardown + live-theme host obligations) | internal/tools/drivers/mcp + internal/mcpconsole + internal/protocol + web/console | §6.4, §7 | 109a, 109b, 109h, 109i, 109j | 100% | Shipped (V1.1.x) |
|110a| Tool-executor promotion (`internal/runtime/dispatch` + exported answer envelope + `tools.NewPlannerView`; devstack degraded executor deleted) | internal/runtime/dispatch + internal/planner + internal/tools + cmd/harbor + harbortest | §6.4, §6.5, §6.2 | D-192 fix, 107d, 107e, 83i | 85% | Shipped (V1.1.x) |
|110b| RunContext population + event-closure promotion (`internal/runtime/runctx` + `events.IdentityStampingEmitter` + `llm.NewChunkPublisher`; devstack Emit/OnChunk/envelope parity) | internal/runtime/runctx + internal/events + internal/llm + cmd/harbor + harbortest | §6.2, §6.5, §6.13 | 110a, 83f, 83i, 83m, 107 | 90% | Shipped (V1.1.x) |
|110c| Config-projection exporters (five `FromConfig` + `config.Defaults()` + `ValidateCore` + `internal/drivers/prod` aggregator; fixes live devstack planner drift B3) | internal/llm + internal/memory + internal/skills + internal/planner + internal/governance + internal/config + internal/drivers/prod | §6.5, §6.6, §6.7, §9, §10 | 83l, 83f, 107d, 107e | 95% | Shipped (V1.1.x) |
|110d| Assembly promotion (exported error-returning `assemble.Assemble` + MCP attach + `auth.BuildProviders` + `events.OpenWith`; D-094 mirror collapses to thin callers; headless recipe) | internal/runtime/assemble + tools/mcp + tools/auth + internal/events + cmd/harbor + harbortest | §6.4, §6.13, §9, §10 | 110a, 110b, 110c, 64, 83g, 30, 57 | 80% | Shipped (V1.1.x) |
|111a| Governance enforcement assembly (`identity_tiers` actually enforce; `SetFactory`'s first production caller) | internal/governance + cmd/harbor + harbortest | §6.15, §6.5, §6.11 | 32, 36a, 36b, 110c (soft) | 90% | Shipped (V1.1.x) |
|111b| Tool-OAuth completion leg (`auth.CallbackHandler` + full pause→callback→resume choreography E2E) | internal/tools/auth + cmd/harbor | §6.4, §3.3, §6.3 | 30, 50, 31, D-192 fix | 85% | Shipped (V1.1.x) |
|111c| Durable pauses + pause lifecycle (checkpoint-store wiring, trajectory threading, max-park sweeper → `DecisionTimeout`'s first producer) | internal/runtime/pauseresume + internal/runtime/steering + cmd/harbor | §3.3, §6.3, §6.11 | 50, 51, D-192 fix | 90% | Shipped (V1.1.x) |
|111d| Skills canonical surface + ingestion (builtin→Phase-38 delegation; `harbor skill import`/`rm`; Directory disposition decision) | internal/skills + internal/tools/builtin + cmd/harbor | §6.7, §8 | 37, 38, 39, 40, 41, 107c | 85% | Shipped (V1.1.x) |
|111e| Trajectory compression consumer (LLM-backed `planner.Summariser` + RunLoop `MaybeCompress` + `token_budget` wiring) | internal/llm/summarizer + internal/planner + internal/runtime/steering + cmd/harbor | §6.2, §6.5 | 46, 35, 107, D-192 fix | 85% | Shipped (V1.1.x) |
|111f| Telemetry assembly + approval-gate authorizer seam (`telemetry.New` in production; `BridgeBusToTracer`; protocolauth out of approval) | internal/telemetry + internal/tools/approval + internal/runtime/steering + cmd/harbor | §6.14, §6.4, §5.1 | 03, 04, 05, 31, 55, 56, D-192 fix | 85% | Shipped (V1.1.x) |
|112a| The public SDK facade (`sdk/` alias-based re-export tree per RFC §3.6) | sdk/ (new top-level) | §3.6, §1 | 110a-d, 111a-f, D-204 | n/a | Shipped (V1.2) |
|112b| External consumers on the facade + the external-module compile gate (scaffold templates, harbortest vocabulary, recipes/README, the standing gate) | cmd/harbor/scaffold + harbortest + docs | §3.6, §8 | 112a | n/a | Shipped (V1.2) |
|113a| Protocol adoption track — generated contract reference (`cmd/harbor-gen-protocol-docs` + `protocol-docs-gen-check` gate) + the executed quickstart + choreographies 1–3 + nav/README/§18 | cmd/harbor-gen-protocol-docs + docs/site + workflows | §5, §3.6 | 103, 58, 59, 60, 61, 62, 110c | 70% | Shipped |
|113b| Protocol adoption track — pause + versioning choreographies, build-a-client (worked event-viewer + compile gate), conformance-certification page | docs/site + examples/protocol-clients | §5, §3.3, §6.3 | 113a, 50, 72e, 111b, 111c, 84a | n/a | Shipped |
|114 | Steering verified-identity authority (control surface derives caller scope + tenant from the verified ctx, not the request body — closes a steering privilege escalation) | internal/protocol | §6.3, §5.5 | 52, 55, 56 | 85% | Shipped (V1.1.x) |
|115 | Production JWT verification (JWKS) + `harbor serve` (the `JWKSURL`/`JWKSFile` config fields gain a consumer; a production auth path beyond the dev signer) | internal/protocol/auth + cmd/harbor + internal/config | §5.5 | 114, 55, 56 | n/a | Shipped (V1.1.x) |
|116 | Non-admin session-scoped token contract (lesser-privileged tokens — the steering-authority consumer that makes 114 load-bearing; safe `session_user` derivation) | internal/protocol/auth + internal/protocol | §5.5, §6.3 | 114, 115 | n/a | Shipped (V1.1.x) |
|117 | Chat module encapsulation hardening (self-contained theming contract + font-family inheritance + host/theme parameterization per D-091; no Console look-and-feel leakage) | web/console | §7 | 109b, 108 | n/a (Console) | Shipped (V1.1.x) |
|118 | Protocol TS lockstep gate (`cmd/harbor-protocol-ts-lockstep` emits a committed wire manifest; `protocol-ts-gen-check` VERIFIES the hand-maintained per-page TS client field-by-field — D-093's "generate" half deferred, D-223; generator name reserved) | cmd/harbor-protocol-ts-lockstep + web/console + workflows | §5 | 113a | n/a | Shipped (V1.1.x) |
|119 | Runtime retention + ctx hardening (reap engine streaming-capacity maps on run-end; bound governance cost/rate-limit caches; cancellable rolling-summary recovery ctx + its missing leak test; dead-`select` + busy-poll cleanups — 2026-06 audit findings, D-248/D-249/D-250) | internal/runtime/engine + internal/governance + internal/memory/strategy + internal/runtime/concurrency | §6.1, §6.6, §6.15 | 12, 13, 24, 36a, 36b | 85% | Pending (V1.5.x) |
|120 | Runtime observability foundation (Go + Process collectors via a new `metrics.go` registration seam; Harbor runtime gauges on the `MetricsRegistry`/`MeterProvider` so the shipped `metrics.snapshot` projects them; `goleak` added to RFC §10 + `VerifyTestMain`; `runtime/pprof` behind a gated loopback `HARBOR_DEBUG_ADDR` — never on the Protocol mux; benchmarks reconciled with the existing `test/benchmarks`, D-251) | internal/telemetry + internal/telemetry/runtimegauges + cmd/harbor + internal/config + RFC §10 | §6.14, §10 | 04, 119, 111f, 72f | 80% | Pending (V1.5.x) |
|121 | Surface runtime gauges in the Console Live Runtime health panel (route the Phase 120 gauges through the SHIPPED `metrics.snapshot` — NO new method; `runtime.health`/`RuntimeHealth` already ship at Phase 72f; extend the existing `live-runtime/health-panel.svelte` via the 108e capability→panel registry, D-252) | internal/telemetry + internal/protocol (additive only) + web/console | §5.2, §7.1, §7.2 | 72f, 120, 108e, 117, 118, 113a | n/a (Console) | Pending (V1.5.x) |
|122 | Persistence + driver-registry dedup (extract `internal/persistence/sqlmigrate` from the 4 conformant SQLite + 3 Postgres copies — searchcache handled separately; generic `internal/driverreg.Registry[T]` across the 15 `(registered:)` factories with one canonical message FORMAT wrapping each subsystem's `ErrUnknownDriver` sentinel; no behaviour change, D-253) | internal/persistence/sqlmigrate + internal/driverreg + persistence drivers | §9 | 15, 16, 18, 25, 37, 41, 110c | 90% | Pending (V1.5.x) |
|124 | Durable event-bus sequence rehydration on restart (rehydrate the durable driver's `nextSeq` from the persisted head records at construction via the `ListKind` maintenance scan so post-restart sequences stay strictly monotonic and a client reconnecting at a high `Last-Event-ID` is not silently skipped; also close the same skip class for transient notices — `Sequence 0`, no SSE `id:` line; D-255) | internal/events/drivers/durable | §6.13, §6.11 | 06, 57, 60, 12, 13 | 85% | Shipped (V1.6) |
|125 | Session state-history windowed event-replay surface (ship the Pending RFC §5.2 `state.history` method — a by-id, identity-scoped, read-only TAIL-FIRST windowed read of the durable event stream: discover head/tail + a bounded backward page with a scroll-up cursor, heavy content by ROUTABLE artifact ref; client-side reduction; rewires the open-source Console session-reopen hydration off its full-load reconstruction; `CapStateSnapshots`, no ProtocolVersion bump; tight dep on 124; D-254) | internal/protocol + internal/events + web/console | §5.2, §6.5, §6.9, §6.10, §6.13 | 124, 72, 72a, 73, 58, 60, 113a, 118 | 85% | Shipped (V1.6) |
|126a| USER-scope agent-config tier: durable per-user config variant + user-keyed revision store + the one durable user-scope write verb (set/list/diff/rollback) | agentcfg | §5.5, §6.16, §6.3 | 92a, 92g, 116, 61 | 85% | Shipped (V1.6) |
|126b| USER-scope durable prompt-layer projection (PROJECTION-ONLY consumer of 126a's durable user_prompt: reads the active ConfigScopeUser revision and threads user_prompt into `<user_instructions>` as the middle segment — admin Base > admin User > USER-durable > session User; no new store/verb) | internal/runtime/agentcfg/projection | §6.16, §5.5 | 126a, 92e, 92g | 85% | Shipped (V1.6) |
| 126c | USER-scope tool-policy run-start projection | agentcfg | §6.16 | 126a, 92d | 85% | Shipped (V1.6) |
|127 | Protocol wire-manifest consumability (runtime.info digest) — STRETCH (a canonical `internal/protocol/wiresurface.Digest()` over the name-level wire surface — Protocol version + methods + errors + capabilities + wire-type names — returned as an additive `runtime.info.wire_surface_digest` field AND stamped into the committed `wire-manifest.gen.json`; a connected Protocol client compares the live digest against the vendored one and surfaces a loud drift signal; NO new method, NO version bump, NO field-shape exposure; Console app-shell status-bar connect-time drift check is the consumer, D-259) | internal/protocol/wiresurface + internal/protocol (additive) + cmd/harbor-protocol-ts-lockstep + web/console | §5, §5.2, §5.3 | 58, 118, 60, 72f | 90% | Shipped (V1.6) |
|128 | Advertise the agent-config control plane as a Protocol capability (add the canonical `CapAgentConfig = "agent_config"` constant to `canonicalCapabilities`; `runtime.info.capabilities` advertises it IFF the agent-config surface is mounted — the `topology_snapshot` conditional-advertisement pattern, wired to `WithAgentConfigService` via one source-of-truth boolean; lets a Protocol client gate the `agent_config.*` surfaces at attach instead of method-probing `501`/`unknown_method`; NO new method, NO error code, NO wire type, NO version bump; conformance handshake set 5→6; only the wire-surface digest regenerates; D-260) | internal/protocol/types + internal/protocol (additive) + cmd/harbor + harbortest/devstack + internal/protocol/conformance | §5.2, §5.3, §6.16 | 58, 72f, 92a, 127 | 85% | Shipped (V1.7) |
|129 | JWKS max-stale / revocation ceiling (bound how long the production JWKS validator honors a cached key when refreshes fail; `identity.jwks_max_stale` + a fail-closed ceiling in `auth.JWKSKeySet` returning a distinct `ErrJWKSStale` / `jwks_stale` reason instead of serving a possibly-revoked key indefinitely; the Validator is the same-phase consumer, proven by a controllable-clock test; NO new method/type/code, ProtocolVersion untouched, D-261) | internal/protocol/auth + internal/config | §5.5 | 115, 55, 56, 61 | 85% | Shipped (V1.7) |
|130 | Session erasure Protocol method (data-lifecycle deletion) (ship the additive identity-scoped `sessions.delete` — deletes a session and CASCADES deletion of its scoped State + Memory + Artifacts; refuses fail-loud on a RUNNING task with a distinct `session_running` (409) mirroring the GC never-reap-running invariant; own-session-only scope contract (no admin / cross-tenant path); new `DeleteScope` StateStore primitive with conformance parity; redacted audited `session.erased` event; new `CapSessionLifecycle` capability; consumer = the real three-store cascade handler + E2E; NO ProtocolVersion bump; D-262) | internal/protocol + internal/sessions + internal/state + web/console | §5.2, §6.9, §6.11, §6.13, §7 | 08, 11, 17, 18, 58, 60, 61 | 85% | Shipped (V1.7) |
|139 | Public-site honesty sweep (docs-only — correct three stale claims on the public marketing landing surface `docs/site/.vitepress/theme/landingSpec.ts` + one stale godoc ref in the `harbor dev` hot-reload test: canonical Protocol method count `109 → 110` and drop the `at v1.6` qualifier (count is current, pinned by `methods_test.go` + `methods.md`); qualify the `harbor dev` hot-reload claim to config/YAML reload (`.go` reload is honest-WARN-only after 138); remove the cosmetic/unprinted `3 drivers registered` dev banner; repoint `cmd_dev_hot_reload_test.go` at the in-package real-bus tests instead of the non-existent `test/integration/phase65_hot_reload_test.go`; gate = static honesty greps + VitePress build; no D-NNN) | docs/site + cmd/harbor (test godoc) | §7.1, §7.3 | 138 | n/a | Shipped (V1.8) |
|131a| Production identity setup guide (the v1.8.0 Adopter-Path P0 — the missing operator manual for getting a verifiable JWT into a client and attaching it to `harbor serve`; `docs/site/protocol/production-identity-setup.md` documents the claim shape the verifier enforces lifted from the parser `internal/protocol/auth/auth.go`, the `iss`/`aud` exact-match contract serve hard-rejects on mismatch, an OIDC app-registration walkthrough with Auth0 / Okta / Keycloak / Cognito snippets, a mint-and-test loop, and BOTH attach on-ramps — a real IdP (131a/131c) and the no-IdP `harbor token` self-issuing path (131d) with an honesty/grade callout; serve's verifier UNTOUCHED, D-220 preserved; docs-only, links auth-and-identity + build-a-client + the `use-the-harbor-protocol` skill in the same PR; D-263) | docs/site/protocol + docs/skills | §5.5, §4.2, §8 | 130 | n/a | Shipped (V1.8) |
|132 | Embed production runner (`RunOnce`) + `NewRunContext` factory (ship the production one-call goal runner `Stack.RunOnce(ctx, goal, identity, opts...)` — blocking, no `Sync` suffix — that builds the run's `RunContext` via the shared `runctx.NewRunContext` factory and drives the assembled `RunLoop`; the factory COMPOSES the existing memory/skills/artifact/streaming projection helpers, never a third construction site, proven by a parity test; `sdk/assemble` + `sdk/runctx` facade aliases; identity-mandatory fail-loud + `ErrNotRunnable`; N≥100 concurrent-reuse `-race` test; recipe step 4a + checked-in `examples/embed-runonce/`; `WithStream` deferred to 132-stream; D-265) | internal/runtime/runctx + internal/runtime/assemble + sdk + examples | §3.6, §6.2, §6.4 | 110d, 110c | 80% | Shipped (V1.8) |
|133 | Scaffold-with-tools execution gate (close the CLI adopter-path false-green: the scaffolded `agent_test.go.tmpl` never called `RegisterTools`, so a tools-declaring agent compiled + passed while no tool was ever invoked; the template's `{{if .CustomTools}}` block now calls `RegisterTools` AND dispatches ≥1 declared tool THROUGH the catalog (narrowed from `{{if or .BuiltIns .CustomTools}}` in v1.13.1 — built-ins never travel through the registrar; the runtime registers them from `tools.built_in`) (`cat.Resolve` + `desc.Invoke` under `harbortest.RunOnce`), asserting an observable dispatch signal — not merely that `RegisterTools` is defined; `phase-112b.sh` gains a `go test ./...` execution leg on the tool-declaring external scaffold + a registration-name-rewrite self-test proving the gate bites (compiles but fails `go test`); static pins in `phase-133.sh`; NO Go API change; D-267) | cmd/harbor/scaffold/templates + scripts/smoke + docs/skills | §8, §6.4 | 112b, 67, 83o | n/a | Shipped (V1.8) |
|138 | Hot-reload Go-source honesty (route `.go` changes in `harbor dev`'s watcher to a WARN + `make build`/restart guidance path instead of an in-process `bootDevStack` reboot that emits `dev.hot_reload.completed{Success=true}` without recompiling — a loud false-success; config / YAML / scaffold changes keep the in-process rebuild; complete the dangling `cmd_dev_hot_reload.go` doc sentence; recipe gains the `.go`-changes caveat; `policy: rebuild-binary` DEFERRED; live `.go`-edit smoke proves the WARN fires + no `completed`; D-268) | cmd/harbor | §8 | 65 | 75% | Shipped (V1.8) |
|131b| `configure-production-identity` operator skill (the Claude-Code-style playbook that operationalizes the 131a guide — `docs/skills/configure-production-identity/SKILL.md` (`metadata.surface: protocol`) is the fast path for getting a verifiable JWT into a client and attaching it to `harbor serve`: the `(tenant, user, session)` + `scopes` claim shape, the `iss`/`aud` exact-match contract, OIDC app registration, and BOTH attach on-ramps — a real IdP and the no-IdP `harbor token` self-issuing path with the honesty/grade callout; same-PR Phase-103 site mirror — the `docs/site/skills/configure-production-identity/` include stub + the `docs/site/.vitepress/config.ts` nav entry so `phase-103.sh`'s skill→page mapping passes; INDEX entry; docs-only, no D-NNN) | docs/skills + docs/site/skills | §5.5, §4.2, §8 | 131a | n/a | Shipped (V1.8) |
|131c| Worked OIDC client + serve round-trip smoke (the load-bearing P0 CONSUMER of 131a — a SDK-free, stdlib-only `examples/protocol-clients/oidc-client-example/` that runs the OAuth2 client-credentials grant against an IdP token endpoint, obtains a JWT, and attaches it to `harbor serve` via `runtime.info`; the binding production-leg gate `scripts/smoke/phase-131c.sh` drives a hermetic in-test ES256/JWKS mock-OIDC issuer → `harbor serve` (pointed at the issuer's published JWKS) → the example obtains a token → `runtime.info` OK with the granted scope, plus a failure-mode leg rejecting an unpublished-key token; the mock-OIDC fixture signs with the same `golang-jwt` lib + JWK shape the REAL parser/`internal/protocol/auth/jwks.go` consumes, §17.8 — not a self-consistent lookalike; asserts the example is SDK-free; serve's verifier UNTOUCHED, D-220 preserved; reaffirms D-263, no new D-NNN) | examples/protocol-clients + test/integration + scripts/smoke | §5.5, §5.4, §8 | 131a | n/a | Shipped (V1.8) |
|131d| `harbor token` bring-your-own-issuer subcommand (the no-IdP on-ramp — a new CLI subcommand that self-issues the JWTs `harbor serve` verifies: `harbor token keygen` generates an asymmetric keypair (ES256 default / RS256 opt-in) + emits a public RFC-7517 JWK Set whose `kid` is the RFC-7638 JWK thumbprint, writing `private.pem` 0600 under a 0700 dir; `harbor token mint` self-issues a Harbor JWT with the claim shape the parser `internal/protocol/auth/auth.go` enforces, signed with the keypair, `--issuer`/`--audience` MANDATORY + matching the operator's `identity.issuer`/`identity.audience` (serve 401s a mismatch), least-privilege defaults — NO scopes unless `--scopes`, short 1h `--ttl`; reuses ONLY the dev path's JWT claim shaper (`harborClaims`), net-new persistable signer + hand-written stdlib JWK-Set emitter; `cmd/harbor/cmd_serve.go` UNTOUCHED, serve still mints nothing — D-220 preserved; RFC §8 subcommand enumeration updated same-PR; D-264) | cmd/harbor + RFC + examples + docs/site/protocol | §5.5, §8 | 131a | 80% | Shipped (V1.8) |
|132-stream| `WithStream` sink on `RunOnce` (add `assemble.WithStream(func(StreamEvent)) RunOption` — ONE streaming sink on the SAME blocking `Stack.RunOnce`: `RunOnce` still blocks and returns the terminal `planner.AnswerEnvelope` while emitting token/step/tool-dispatched chunks as they occur; wired to the EXISTING synchronous run-loop seam — `planner.RunContext.OnChunk` (token + step) + `steering.RunSpec.OnToolDispatched` (tool dispatch), both fired on the run goroutine so "chunks arrive before the final envelope" is DETERMINISTIC; no separate streaming package, no sync/async method split, no signature change; new public `StreamEvent` type, `sdk/assemble` re-exports; §13 primitive-with-consumer e2e ordered-chunks test; the N≥100 concurrent-reuse `-race` test extended to assert no cross-run chunk bleed; D-266) | internal/runtime/assemble + sdk | §3.6, §6.2, §6.4 | 132 | 80% | Shipped (V1.8) |
|134 | `sdk` `Example_` functions (the FIRST `_test.go` files under `sdk/` — runnable, deterministic, offline `Example_*` functions across the four primary adopter surfaces so pkg.go.dev renders the facade's first-contact worked code: `sdk/assemble` golden `Assemble`→`Stack.RunOnce` (`// Output: goal`) + a `WithStream` variant whose sink reassembles the streamed content to the terminal answer (`// Output: true`); `sdk/config` `Defaults`→`ValidateCore` headless profile; `sdk/planner` `RegisteredDrivers` after seating `react` + the `FinishReason` vocabulary; `sdk/steering` per-run control-inbox round-trip (`Open`→`Enqueue` scoped CANCEL→`Drain`); example BODIES import only `sdk/` (the dev-only mock LLM blank import is the one D-089 test-file exception, mirroring `runonce_test.go`); thin static `scripts/smoke/phase-134.sh` pins the examples present + `go test ./sdk/... -run Example` green under `-race`; no D-NNN — examples) | sdk | §3.6, §6.2, §6.3, §6.4 | 132, 132-stream | N/A (test-only) | Shipped (V1.8) |
|137| Conformance worked example (in-tree) (ship a runnable in-tree worked example of certifying a Protocol-server fork / embedder assembly against the conformance suite — `examples/protocol-clients/conformance-fork/` as a `go test`-compiled `_test.go` harness wiring a CUSTOM `conformance.Factory` (its own real-driver event bus + state store + task registry + control surface + wire mux + ES256 validator) + `conformance.RunSuite`, with the suite running green over the custom Factory as the §17.8 gate; SCOPE CORRECTION — the suite stays under `internal/protocol/conformance`, deliberately NOT externally importable, and `RunSuite` is `*testing.T`-bound, so the example is a test harness, NOT a runnable client binary like `event-viewer`; adds a worked-example pointer to `docs/site/protocol/conformance-certification.md`; reaffirms the in-tree Factory-seam posture, does NOT make the suite externally importable; reaffirms D-210, no new D-NNN) | examples/protocol-clients + docs/site/protocol | §5.1, §5.2, §5.3 | 62, 113b | n/a | Shipped (V1.8) |
|136 | MCP agent-calls-tool integration test (the test-only phase that proves the agent-call leg for an MCP-sourced tool — a planner decides to call a tool DISCOVERED from a real stdio MCP server (`cmd/harbor-mcptest-stdio`'s `echo`, registered `mcptest_echo`) and the runtime dispatches it THROUGH the executor to the live subprocess, routing the result back into the trajectory; closes the gap where MCP discovery-reaches-catalog was tested (`…ReachTheCatalog`) but the executor dispatch leg was not — the only executor-level tool-invocation test today calls an in-process BUILTIN; net-new `TestE2E_Phase83g_MCPAgentCallsTool` reusing the 83l scripted-bifrost devstack harness over a real wire (§17.8), dispatch signal = the echoed sentinel in the CallTool step OBSERVATION + the second LLM prompt (not the input args), identity propagated end-to-end + cross-identity `Tasks.Get` isolation, ≥1 failure mode (bad-args server-side rejection surfaced through the executor → re-plan), `-race`; `scripts/smoke/phase-136.sh` pins the EXACT name + a `go test -list` no-match-fails guard so the gate cannot silently match zero tests; no D-NNN — test) | test/integration + scripts/smoke | §6.4, §6.2 | 83g, 83l | n/a | Shipped (V1.8) |
|135 | TS wire-type generator + `event-viewer-ts` (ship the DISTINCT external-client generator `cmd/harbor-protocol-ts-types` — reflects over `internal/protocol/singlesource.CanonicalWireTypes` + the method/error/event sets and emits a vendorable, dependency-free external-client TS module `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts`: one `interface` per canonical wire type + `HarborMethod`/`HarborErrorCode`/`HarborEventType` string-unions + pinned `PROTOCOL_VERSION`/`WIRE_SURFACE_DIGEST`, types-only no client logic; new make targets `protocol-ts-types-gen`/`-check` (regenerate + `git diff` + Go lockstep), ADDITIVE to and NON-interfering with the D-223 Console gate (`cmd/harbor-protocol-ts-lockstep` + `wire-manifest.gen.json` + `protocol-ts-gen[-check]` UNTOUCHED); the reserved `cmd/harbor-gen-protocol-ts` name stays reserved for the full Console generator; §13 consumer = worked `event-viewer-ts` client (Node `--experimental-strip-types`, vendors the module, SDK-free) round-trips `runtime.info` against the dev runtime; §18 same-PR skill/doc updates (`use-the-harbor-protocol` SKILL lines 17/286/354 + `build-a-client.md`); partially retires D-132 for external clients; D-269) | cmd/harbor-protocol-ts-types + examples/protocol-clients/event-viewer-ts + Makefile + docs/skills + docs/site | §5, §5.3, §3.6 | 113b, 118 | 80% | Shipped (V1.8) |
|140 | Wave E2E + v1.8.0 checkpoint audit (the wave-end composing E2E for the v1.8.0 Adopter-Path band — `test/integration/wave_v18_test.go` (matching the `wave_v17_test.go` precedent) proves the three advertised adopter paths are alive TOGETHER on real drivers under `-race`: EMBED (`assemble.Assemble`→`Stack.RunOnce` + a `WithStream` ordered-chunks-before-envelope variant, 132/132-stream), PROTOCOL (BOTH on-ramps against a real `harbor serve` subprocess — the BINDING hermetic ES256/JWKS mock-OIDC issuer→serve→`runtime.info` OK + granted scope (131c), AND the `harbor token keygen`→`identity.jwks_file`→`harbor token mint`→`runtime.info` bring-your-own leg (131d), plus an asserted 401 on a mismatched-`iss` token), and MCP TOOL DISPATCH (in-process goal driving the planner to invoke `mcptest_echo` THROUGH the executor to a real stdio subprocess, 136); ≥1 failure mode per edge (mismatched-iss 401, garbage-token 401, missing-identity 401) + the D-220 "serve mints nothing" invariant re-asserted at the binary surface; identity propagation asserted end-to-end + cross-identity `Tasks.Get` isolation; N≥16 concurrent `RunOnce` against ONE shared Stack asserting no answer/chunk bleed + goroutine baseline restored; reuses the 131c mock-OIDC + `bootServe`, the 131d keygen/mint CLI, the 83l/136 scripted-bifrost+MCP harness, and the 132/132-stream `RunOnce`/`WithStream` surface; static `scripts/smoke/phase-140.sh` pins the file + a `go test -list` no-match-fails guard on `TestE2E_WaveV18`; then the read-only §17.5 checkpoint audit → one `chore(checkpoint): v1.8.0 audit fixes` PR that flips the 131a…139 rows + the root README Status table to Shipped and authors the `[1.8.0]` CHANGELOG entry; no D-NNN — audit/test) | test/integration + scripts/smoke | §5.4, §5.5, §6.4, §8 | 131c, 131d, 132, 132-stream, 133, 135, 136, 137, 138 | n/a (test-only) | Shipped (V1.8) |
|141 | Native tool-name sanitization for provider-safe tool-calling (live verification of the scaffold-with-tools path found Harbor's dotted tool names — built-ins `clock.now` / `text.echo` + scaffolded custom tools like `inventory.check` — break native tool-calling against OpenAI-compatible providers, which reject any function name not matching `^[a-zA-Z0-9_-]{1,64}$` with a `400`; the React planner now sanitizes every name it sends the LLM (`req.Tools` declarations + replayed assistant `tool_calls`) via `sanitizeToolName` and resolves the returned name back to the real catalog name on dispatch via `resolveDeclaredToolName` — transparently, no operator/catalog/API change; the 107c/107d/133 tests used a scripted LLM so never hit provider name-validation, the §17.8 hazard; a deterministic declaration→projection round-trip unit test is the regression guard; validated live against a real provider for both a dotted built-in and a dotted scaffolded custom tool; also fixes the `embed-runonce` worked example to declare a `ModelProfiles` entry so it RUNS, not just compiles; D-270) | internal/planner/react + examples | §6.2, §6.4 | 107c, 107d, 133 | n/a | Shipped (V1.8) |
|142 | External tool-credential provisioning: the `tokenexchange` OAuth driver (an external credential authority — a fleet orchestrator, an enterprise token vault, an STS — holds a user's downstream integration credentials centrally and a runtime PULLS them at token-miss time via an RFC-8693-shaped exchange, instead of every runtime independently OAuth-acquiring + sealing its own copy (today: N consents, N encrypted copies); a new non-interactive driver on the D-095 OAuth flow-strategy registry (`tools.oauth_providers[].driver: tokenexchange`, broker knobs on the reserved `extra` map — no config-schema change), keyed on the VERIFIED ctx identity triple (D-219 posture), brokered tokens TTL-cached in memory only + single-flighted and NEVER `TokenStore.Put` (no shadow copy of the broker's truth — D-061 southbound), broker failure fails the run loudly with NO silent interactive fallback, a broker `consent_required` refusal surfaces the existing typed `*auth.ErrAuthRequired` and parks on the unified pause primitive (§7 rule 4 — one pause path) with resume re-driving `Token()`, interactive-flow methods return the new typed `ErrNonInteractive`, every actual exchange emits the new canonical `tool.credential_exchanged` SafePayload event (+ protocol-docs regen); push-style credential injection over the Protocol is REJECTED as §7 credential passthrough, recorded in D-271 so it is not re-litigated; D-271) | internal/tools/auth/drivers/tokenexchange + internal/tools/auth + internal/config | §6.4, §3.3 | 30, 50, 64a | 85% | Shipped (V1.9) |
|143 | Run-level structured output: the `WithOutputSchema` run option (the run-level typed-output mechanism — an opt-in `assemble.WithOutputSchema(schema) RunOption` on `Stack.RunOnce` threading a caller-supplied final-answer JSON Schema through `runctx` → `planner.RunContext` into the React driver's terminal completion, riding the shipped-but-nearly-consumerless LLM substrate (`CompleteRequest.ResponseFormat` + the `Validator` retry-with-feedback wrapper bounded by `ModelProfile.MaxRetries` + the `OutputMode` strategy/downgrade chain — no new toggle, brief 03); final validation is RUNTIME mechanism at the RunOnce edge for every planner (no `Supports*` ceremony — the deterministic planner's structured `Finish` payload just validates), schema-invalid after the budget → typed `planner.ErrOutputInvalid`, never silent unvalidated text (§13); the validated answer lands on the ADDITIVE `answer_payload` envelope key (pinned bytes untouched, `Answer` keeps the string rendering, golden test extended); `WithStream` composes — `tool_dispatched`/`step` stream as today, assistant-content `token` chunks are suppressed for a schema-constrained run and the answer arrives once, validated (the Claude-Agent-SDK/LangGraph buffered pairing with a retry loop — D-272; partial-object streaming is the NAMED follow-up, not silence); mixed-traffic N≥100 D-025 test (distinct schemas, no bleed); `sdk/assemble`/`sdk/planner` re-exports; embed recipe + example updated same-PR (§18); D-272) | internal/runtime/assemble + internal/runtime/runctx + internal/planner + internal/planner/react + sdk | §6.5, §6.2, §3.6 | 35, 36, 110a, 132, 132-stream | 80% | Shipped (V1.9) |
|144 | Typed embed binding: `RunTyped[T]` + the shared schema-derivation home (the typed sugar over 143 — a generic free function `assemble.RunTyped[T any](ctx, stack, goal, id, opts...) (T, planner.AnswerEnvelope, error)` deriving the output schema from `T`, running with `WithOutputSchema`, unmarshaling the validated `answer_payload` into `T`; the Go-type→JSON-Schema derivation is PROMOTED from the inproc tool driver into the neutral `internal/tools/schema` package (§13 forbids importing a concrete driver — one implementation, golden-pinned byte-identical, both `RegisterFunc` and `RunTyped` consume it); the `sdk/assemble.RunTyped` forward is the facade's SECOND documented generic-func carve-out amending D-205 item 1 (identical "Go has no generic function values" rationale; the no-behavior smoke flips to an enumerated two-func allow-list and fails on any third); deliberately NOT named `Agent` (noun taken twice: `harbortest.Agent` + the Agent Registry entities, D-059) and NO stateful binding object (the bind-once surface remains `config`+`Assemble`→`Stack`); unsupported `T` fails loud at call time before LLM spend; mixed-`T` N≥100 D-025 test; scaffold/example consumer through the external-module compile gate + pkg.go.dev `Example_runTyped`; fallback named up front — if the amendment is rejected, 143 alone ships typed output with two caller-side lines and 144 slips without blocking the wave; D-273) | internal/tools/schema + internal/runtime/assemble + internal/tools/drivers/inproc + sdk | §3.6, §6.2, §6.4 | 143, 26, 112a | 85% | Shipped (V1.9) |
|145 | Governance attempt-level cost accounting (closes D-272's "Known accounting gap": governance composes OUTSIDE retry — D-043/D-044, unchanged — so `CostAccumulator.PostCall` sees only the FINAL `(resp, err)` of the retry-with-feedback loop × downgrade chain; worst case `(MaxRetries+1)×3` uncounted provider calls per turn, live since 143 made the `Validator` loop production-real; fix = a synchronous in-band **attempt-cost tap** in `internal/llm`: `governance.Wrap` installs a per-call ctx-carried accumulator after `PreCall` permits, the retry wrapper reports each validator-rejected NON-final attempt and the downgrade wrapper reports every errored attempt (its error paths discard the attempt's resp entirely), `PostCall` drains once and folds tap total + final `resp.Cost` under the existing identity-triple key — the propagate-or-report invariant makes accounting exactly-once and compose-order-independent; NOT an `llm.cost.recorded` subscriber (would re-litigate the settled in-band rationale + §13 parallel-implementation); PreCall short-circuit semantics, `resp.Cost` semantics, compose order all unchanged; attempt spend accumulates even when the outer call errors (fail-loud, §13); exactness test with distinct per-attempt costs across success/retry-exhausted/downgrade-exhausted terminals; ceiling test where intermediate attempts trip the NEXT PreCall; N≥100 shared-chain D-025 stress; stale bifrost "subscriber" doc comment corrected same-PR; D-275) | internal/governance + internal/llm + internal/llm/retry + internal/llm/output | §6.15, §6.5 | 33, 35, 36, 36a, 143 | 85% | Shipped (v1.10) |
|146 | Per-task structured output: the `answer_payload` per-task producer (closes the D-272 reservation recorded at `dispatch.go::taskOutcomeObservation` + `tasks.go::TaskResult` — "per-task Protocol runs do not yet produce it"; a new ADDITIVE `output_schema` field on the `start` wire request (`types.StartRequest`, per-run granularity — deliberately NOT agent config, D-234 is per-agent next-turn desired state) rides request → `tasks.SpawnRequest` → the persisted `Task` record → both per-task RunLoop drivers (production + devstack twin, §17.6/D-094), which compile ONCE at run start via `planner.CompileOutputSchema` (the edge also compile-rejects with `CodeInvalidRequest` before Spawn), set `RunSpec.Base.OutputSchema` so the React per-turn steering engages with zero planner change, and validate the terminal Finish through ONE promoted shared `runctx` envelope builder (the RunOnce-edge validation + `capturePayloadJSON` move there; RunOnce re-bases, goldens byte-identical — `steering.RunLoop.Run` deliberately does not validate, so each run-edge caller invokes the same builder, §13); validated payload lands as `answer_payload` on the task envelope → `tasks.get` `result_inline` + the AwaitTask parent observation (generic parse, D-026 `projectForLLM` offload already applies and is test-pinned); schema-invalid after the retry budget → task fails LOUD with new terminal code `planner.TaskErrorCodeOutputInvalid`, never a schemaless success; token deltas suppressed on schema tasks (D-272 posture mirrored); full D-223 lockstep (manifest regen + typed-client `outputSchema` opt) + D-209 docs regen; §13 consumer = the v1.9-audit AwaitTask round-trip E2E; N≥100 mixed distinct-schema D-025 stress; NON-goals recorded: planner-emitted SpawnTask schemas (sealed Decision sum, D-047), config-level defaults, partial-object streaming (#444); D-276) | internal/protocol + internal/tasks + internal/runtime/runctx + internal/runtime/assemble + cmd/harbor + harbortest + web/console | §6.5, §6.2, §6.8, §5.2 | 143, 110a, 118, 54, 73d, 87, 107e | 80% | Shipped (v1.10) |
|147 | Events conformance suite home + fold (the v1.9 audit's deferred NIT 7 paid down as a phase — build `internal/events/conformancetest` mirroring the identity/state/memory precedent EXACTLY (package `conformancetest`, exported `Run(t, factory)` — NOT `RunConformance`) and fold the verified duplicated per-driver scenarios into it: fence drop-late/empty-history + after-close (the D-274-hardened erasure fence's bus-side contract), Bounds/Window history reads, replay cursor semantics (head-cursor nil, cross-identity isolation, cursor-too-old, replay-disabled, empty-filter), subscribe scope validation, close lifecycle — 20 pinned scenarios with a binding no-coverage-loss mapping table; the `Factory` returns a memory-style `Harness` of three MANDATORY constructors (default / replay-disabled / bounded-retention) because the scenarios span bus CONFIGURATIONS — the one genuine divergence (durable-mode retention is unbounded; only best-effort ring mode can return `ErrCursorTooOld`) is parameterized as configuration, never a `Supports*` flag (§4.4), and capability presence (`Replayer`/`HistoryReplayer`/`Fencer`) is asserted fail-loud, never skipped; identity-scoping scenarios are unconditional suite members (§6 rule 10); PURE test refactor — zero production change, both drivers invoke the suite from their own `_test.go` in the same PR (§13 consumer by construction); driver-specific tests stay put (durable recovery/restart D-255, OpenWith shared-store, persist-failure, cancellation-bounds, per-driver D-025 reuse tests); the drop-policy/redaction/reaper/admin-audit near-duplicate pairs are the NAMED second tranche, deferred deliberately; D-277) | internal/events | §6.13, §6.9 | 05, 06, 57, 125, 130 | n/a | Shipped (v1.10) |
|148 | MCP southbound per-identity OAuth bearer + `_meta` provenance (a non-secret `oauth_provider` name binding a declared `tools.oauth_providers[]` entry — the D-095 registry — on `config.MCPServerConfig` + the agentcfg/wire MCP connection descriptors; every identity-stamped per-call MCP RPC fetches `prov.Token(ctx, source)` and injects `Authorization: Bearer` on THAT request only via a context-aware RoundTripper (token rides the per-call ctx, D-025 — the connect-frozen static header map stays for connect-time auth; static `Authorization` + binding on one connection is rejected at validation); a bound provider whose `Token()` fails NEVER falls back to an unauthenticated call, and `consent_required` parks on the unified pause primitive via the existing typed `*auth.ErrAuthRequired` (§7 rule 4); `buildIdentityMeta` additionally stamps `agent_id` (provenance via a new `tools.WithInvokingAgent` ctx seam — NEVER an isolation principal, §6/D-059) + operator-declared non-secret `meta_annotations` merged verbatim (reserved keys `tenant`/`user`/`session`/`agent_id`/`traceparent`/`tracestate` + the `io.modelcontextprotocol/` prefix rejected at validation); wire change runs the full D-223 TS lockstep + D-209 docs regen; the injection seam is the ONE mechanism 85b/92l reuse when they land; D-278) | tools/mcp + tools/auth + config + agentcfg + protocol | §6.4, §3.3, §6.16 | 142, 92f, 28, 30, 64a, 50, 118 | 80% | Shipped (v1.10) |
|149 | HTTP-manifest boot loader wiring (the `tools.http_manifests[]` knob goes live: `assembleCatalogBand` loads each declared UTCP-style manifest via the shipped Phase 27 `LoadManifest`/`RegisterManifest` pair and registers its tools by name — after built-ins, BEFORE the catalog Builder applies `tools.entries[]`, so the EXISTING by-name `entries[].oauth` binding wraps manifest tools with zero new OAuth machinery, closing the "no config-declarable tool can exercise catalog OAuth wrapping end-to-end" gap and giving adopters a black-box vehicle for D-271 token exchange; `config.Validate` flips from REJECTING a populated list (the SDK-friction-audit dead-knob guard) to VALIDATING it, with `config.Load` resolving relative entries against the config directory under the §7 rule 5 Clean+prefix posture; boot fails loud naming file + config key on missing/unparseable/`ErrManifestInvalid` manifests and on tool-name collisions — never a silent skip; one wiring home (`Assemble`) serves binary + devstack per D-196/D-197; NO manifest-level `oauth` field (the by-name path is THE binding home, §13) and NO change to `WrapWithOAuth` pre-check semantics (southbound injection is Phase 148's, for MCP); D-279) | internal/runtime/assemble + internal/config + internal/tools/drivers/http + examples | §6.4, §3.4 | 26, 27, 64a, 110d, 142 | 80% | Shipped (v1.10) |
|150 | Run-completion hook: transcript egress through the tool catalog (the runtime's first run-lifecycle hook — memory/audit/analytics sinks need the full conversation at completion for runs no client observes (background + disconnected runs have no observer to pull it), and NO generic run-completion signal exists today (a plain foreground `Stack.RunOnce` completing emits nothing; only `task.completed`/`task.failed` fire from the tasks engine, so a bus subscriber cannot cover all run types); fix = `RunSpec.CompletionHook` fired EXACTLY ONCE at `RunLoop.Run`'s single terminal boundary via a deferred fire over the named returns — every terminal outcome (goal / no_path / constraints_conflict incl. REJECT + pause-timeout / cancelled incl. cancel-while-paused / error) with the outcome IN the payload, never mid-run, never on pause — the one seam ALL run types terminate through (`runonce.go:305`, `cmd_dev_runloop.go:972`); egress = a NAMED CATALOG TOOL dispatched through the existing `spec.ToolExecutor.ExecuteDecision` path (provenance, identity, per-tool policy retries, and args-free `tool.*` audit events come FREE; a bespoke HTTP egress client is the REJECTED §13 parallel implementation), under `context.WithTimeout(context.WithoutCancel(runCtx), timeout)` so the cancelled-run case fires with identity values preserved (the `internal/tools/auth` precedent); a hook failure NEVER alters the settled run outcome — `run.hook_failed` on the bus + Warn, success emits `run.hook_dispatched` (SafePayloads: metadata only, never transcript content); payload = typed golden-pinned `RunCompletionPayload` (`format_version: 1`) assembling the ordered conversation from LIVE run state (initial goal, steering USER_MESSAGE/REDIRECT entries accumulated as per-run stack-local state with step indices — today they are consumed per-step and durably lost, assistant preambles + final answer, D-274 counts; the dispatch does NOT touch `OnToolDispatched`/`ToolCallsSeen`/trajectory); config = yaml `runtime.hooks.run_completion.{tool,timeout}` (the reserved `RuntimeConfig` slot's first body) paired with a new versioned agentcfg `hooks` section riding the EXISTING `set_revision`/`get`/`diff`/`rollback` surface (next-run projection per D-234; `projection.ActiveRunCompletionHook` twinned cmd/devstack per §17.6; wire impact types-only → `AgentConfigHooks` + diff arm + D-223 TS mirror/manifest + protocol-docs regen, NO new verb); §13 consumer E2E: a run with a hook targeting a real fixture tool receives the full ordered transcript incl. a steering-injected mid-run user message, identity asserted AT the receiving tool, hook-error + cancelled-run failure legs, N≥10 concurrent no-transcript-bleed `-race`; 148's per-identity MCP bearer applies automatically via ctx identity (same-wave consumer); D-280) | internal/runtime/steering + internal/agentcfg + internal/runtime/agentcfg/projection + internal/config + internal/protocol/types + cmd/harbor | §6.17, §6.3, §6.4, §6.13, §6.16 | 53, 83i, 92a, 132, 148, 149 (soft) | 80% | Shipped (v1.10) |
|151 | Runtime loading-mode control on tool exposure (extends the ONE exposure section — `agentcfg.ToolExposure` gains `server_loading_modes` (per-`ToolSourceID` default, tool-form descriptors only via the additive `Tool.Form` classification) + `tool_loading_modes` (per-name, exact) riding the existing `agent_config.set_tool_exposure` verb, no new verb; ONE pinned precedence order — exposure per-tool > exposure per-server > boot `tools.entries[].loading_mode` > driver default (the mcp.go `LoadingAlways` tool hardcode becomes the overridable default; resources/prompts stay driver-deferred) — closing the two-knobs-undefined-precedence §13 smell; applied NEXT-turn at the shared `projection.ActivePlannerCatalogView` seam via a new `tools.LoadingOverrideView` (List filters on the EFFECTIVE mode, Resolve never filters — the D-167 two-turn `tool_search` discovery cycle is untouched; disable stays strictly stronger: hidden from List AND Resolve), in-flight runs keep their snapshot per D-025/D-234; admin tier only — loading is not capability-narrowing (`VisibleNames` spans both modes; the app-call gate ignores it) and the D-256/D-258 narrow-only tiers gain no map fields; unknown value → loud 400 `invalid_request`, no revision, no event; `DiffToolExposure` gains structured loading arms (audit = `agent.config.revised` + diff, no new event); `tools.describe` gains optional `agent_id` reporting the projected EFFECTIVE `loading_mode` (absent = boot-effective, byte-compatible); additive wire changes with full D-223 lockstep + D-209 docs regen same-PR; integration test on the real MCP stdio fixture (flip → prompt-list excludes / `tool_search` surfaces / Resolve returns; flip-back; invalid-mode failure; cross-agent + cross-tenant isolation) + N≥100 flip-under-load D-025 stress; lands after 148 merges (same files — coordination, not semantic); D-281) | agentcfg + internal/tools + internal/runtime/agentcfg + internal/protocol | §6.4, §6.16 | 92a, 92d, 107c, 110a, 118 | 85% | Shipped (v1.10) |
|152 | agentcfg section-setter hooks carry-forward + rebuild-completeness guard (all FIVE section-scoped setters — `set_tool_exposure`, `add_mcp_connection`, `set_skills`, `set_prompt_layers`, `set_llm_params` — drop the D-280 `Hooks` section when rebuilding the payload, so any section edit silently erases a pinned run-completion hook (§13 silent degradation on the subsystem's own "bidirectional section-merge invariant"); fix = carry `Hooks` forward in all five + a reflection-backed rebuild-completeness guard (seed constructor populates EVERY `ConfigPayload` section and reflect-asserts full field coverage, then each setter must preserve every non-target section byte-identically — a future section addition fails `go test` naming the field); no new verb (hooks stay `set_revision`-only), no wire change, no TS/docs impact; Protocol-level regression E2E (`set_revision` hooks → `set_tool_exposure` → `get` hook intact) + `projection.ActiveRunCompletionHook` resolves post-edit; D-283) | internal/runtime/agentcfg/protocol | §6.16, §6.17 | 150, 151, 92d, 92f, 92m | 85% | Shipped (v1.11) |
|153 | Admin-widened fleet enumeration for `tasks.list` + `agents.list` (today both project ONLY the caller's own triple — a fleet observer's synthetic session sees nothing, unlike `sessions.list` which widens to `(tenant, user)` / admin-named tenants; fix = the reserved "aggregating projector" behind the EXISTING per-subsystem `Projector` interfaces: explicit tenant-scoped enumeration on the task-registry seam (a separate method with an explicit tenant argument — NEVER an optional/blank session on the identity-scoped read, no identity-downgrading knob §13) with conformance parity across inprocess/durable drivers, + the agents analogue over the Agent Registry; gate = the ONE existing `auth.ScopeAdmin` claim (no new "fleet scope" vocabulary), widened-without-claim → loud `ErrScopeMismatch` (never silent narrowing), every widened call emits `audit.admin_scope_used`; rows carry full identity attribution; additive wire fields + full D-223 lockstep + D-209 docs regen; §6 rule 10 isolation E2E (2 tenants × 2 users × 2 sessions; non-admin never widens, admin-A never sees tenant B) + N≥10 concurrent mixed listers `-race`; cross-RUNTIME federation stays coordinator-side, as with sessions; D-284) | internal/tasks + internal/runtime/registry + internal/protocol + web/console | §6.8, §6.16, §5.2 | 87, 53a, 118, 130 | 85% | Shipped (v1.11) |
|154 | OAuth provider credential source: env or coordinator-served pull (a `tools.oauth_providers[]` client credential is env-resolved ONCE at boot — a broker credential minted post-boot can never reach a running runtime, forcing the one-reboot provisioning step; fix = a §4.4 `credential_source` seam on the provider entry: `env` (today's boot-time fail-loud resolution, the DEFAULT — existing configs byte-compatible §10) or `remote` (authenticated PULL of `client_id`/`client_secret` from a coordinator endpoint via the runtime's service token env, at first need — memory-only TTL cache, single-flight, strict `format_version`-ed parse); fail-loud everywhere: boot validates declared-source shape, fetch failure = typed sentinel + SafePayload event, NEVER fallback to env/unauthenticated/interactive (one mode per source §13); declaring both sources on one entry = validation error; hot-reload/admin-verb push REJECTED (credential passthrough per D-271 + post-exec env immutability); no catalog re-wrap (provider instance stays boot-constructed, only cred resolution is late); §17.8 fixture credential server E2E incl. zero-env boot → first-need pull → tokenexchange succeeds + rotation leg; new canonical fetch events + D-209 regen, no Protocol wire change; defense-in-depth: broker secret never enters runtime env; D-285) | internal/tools/auth + internal/config + internal/drivers/prod + examples | §6.4, §3.3 | 142, 148, 30, 149 | 85% | Shipped (v1.11) |
|155 | Session-erasure audit integrity (issues #409/#410 from the v1.7 band-end review: the `session.erased` record-of-fact (D-262) is emitted best-effort AFTER the irreversible clear — one bus/redactor failure loses the only audit record while the call reports success, and re-invoke returns `not_found`; retried mid-cascade erasures also under-report deletion counts (idempotent re-runs find fewer records); fix = the ordering invariant becomes binding: never (data gone AND no durable record AND success) nor the inverse — record/emit failure fails `sessions.delete` LOUD with a typed sentinel and the session still re-invokable, re-invoke converges (fault-injection tested: bus fails once then heals); deletion counts accumulate across converging attempts (the #410 "document the undercount" alternative REJECTED — compliance counts must be accurate); no wire-shape change (field docs state cumulative semantics, `protocol-ts-gen-check` proves no manifest impact); D-286) | internal/sessions + internal/protocol/types | §6.9, §6.13 | 130 | 85% | Shipped (v1.11) |
|156 | `agent_config.remove_mcp_connection` + detach-on-reconcile (supersedes D-240 decision 5's deferral via its OWN recorded revisit clause — "a removal need pause cannot serve" emerged: a coordinator delete flow must actually remove a runtime-added MCP connection, which pause cannot express (descriptor persists; resume resurrects); the verb = a new revision dropping the named descriptor AND pruning that server's tool-exposure residue atomically, all sibling sections (incl. Hooks) carried forward under the D-283 completeness guard which this setter JOINS; unknown name / boot-declared (yaml) name = distinct loud typed errors (the verb governs revisioned state only); run-start reconciliation gains the detach leg: declared-vs-attached diff deregisters undeclared servers from catalog + MCP registry and closes the transport at the NEXT-turn projection boundary (never mid-run; in-flight snapshots stable per D-025/D-234) — rollback past an add detaches through the SAME reconcile path (one mechanism §13), closing D-240's deferred rollback gap; agent-bound sealed tokens NOT deleted on remove (re-add reuses consent; revocation is provider-side; documented); new canonical `mcp.connection.removed` event; full D-223 lockstep + D-209 regen; real-stdio-fixture E2E (add → remove → next-turn catalog/registry/transport all clear; rollback leg; re-add-reuses-token leg) + remove-under-load `-race` stress + goroutine-baseline teardown proof; D-287) | internal/runtime/agentcfg + internal/agentcfg + internal/tools + internal/protocol + web/console | §6.16, §6.4, §3.3 | 152, 92m, 92n, 92o, 118 | 85% | Shipped (v1.11) |
|157 | Session title: record field + `sessions.set_title` + Console rename (sessions display as raw ids everywhere — the record carries no human-readable name and no verb sets one; a coordinator consumer must not shadow-store the attribute (D-061), so it lands framework-side: `Title` + `TitleSource` (`unset\|auto\|manual`) on the session record — additive JSON round-trip through the existing `session.lifecycle` kind, zero migration, erased with the session by the existing `DeleteScope` cascade; new `sessions.set_title` method writes `manual` ONLY (`auto` is not wire-expressible — Phase 158's internal path is its sole producer, so manual-wins is unforgeable), empty clears, > 200-rune/multi-line input fails loud 400 (never a silent clamp §13); write scope = the owning `(tenant, user)` (the scope `sessions.list` reads at; metadata-only, no elevation knob, no admin widening); `SessionRow.title`/`title_source` additive; content-free `session.title_changed` SafePayload (the title is user-derived content and NEVER rides an event/log/audit payload — consumers refetch); §13 in-repo consumers same-wave: Sessions-page truncated-title display + inline rename AND Playground switcher `title \|\| session_id` + active-session rename with event-driven list refresh; full D-223 lockstep + D-209 regen; registry D-025 stress extended N≥100 `-race`; D-288) | internal/sessions + internal/protocol + web/console | §6.9, §5.2, §6.13, §7 | 73c, 106, 118, 130, 155 | 85% | Shipped (v1.12) |
|158 | Session auto-naming: opt-in policy + terminal-boundary titling call (OPT-IN, DEFAULT OFF — zero-config behavior byte-identical to v1.11: no counters, no LLM calls, no events; a new `naming` agent-config section riding `set_revision` (the D-280/D-283 hooks precedent — NO new verb, additive wire types only, joins the D-283 completeness guard across all six setters) over a yaml `runtime.naming` fleet default, precedence agentcfg › yaml › off resolved once at run start (D-234): `auto`/`after_turns`(≥1, default 1)/`repeat_every`(0 = once)/`max_repetitions`(REQUIRED ≥ 1 when repeating, default 5, counts the first call — NO unlimited value exists, unbounded periodic re-naming is unrepresentable)/`model`("" = run's effective model, else ModelProfiles-validated)/`max_title_len`(default 80); mechanism = a SIBLING of the run-completion hook at the run loop's terminal boundary (fires after `(fin, err)` settle, never alters them, `recover()`-contained, detached-but-bounded ctx preserving identity — riding the hook would overload D-280's single tool-egress; a spawned task pollutes `tasks.list`; memory-subsystem transcripts are silently inert under default `strategy: none` §13) making ONE governed `Complete` on the run's WRAPPED LLM client (governance outermost keys ctx identity — a ceiling block SKIPS naming with `governance_blocked`, run untouched; spend is deliberately the tenant's, cheap-`model` is the mitigation) over a ≤ 4 KiB completion-boundary transcript digest (D-026 `ErrContextLeak` unreachable by construction); write via internal `SetTitleAuto` — refuses `manual` (typed `ErrManualTitle`), clamps auto output deterministically (trusted-internal asymmetry vs 157's reject documented), bumps `AutoNameCount`+`LastAutoNamedTurn` in ONE record save; `TurnCount`/counters written only when a policy is active (counts start at enablement — no write amplification for the naming-off fleet); failure NEVER alters the run and is NEVER silent: `session.naming_failed` SafePayload with stable error class (`llm_error`/`timeout`/`empty_title`/`governance_blocked`/`manual_title`/`internal`), never content; scripted-LLM E2E (83l pattern: enable → title lands `source=auto`; manual halts; clear re-arms; cadence + cap honored; governance leg) + N≥10 concurrent-sessions no-bleed `-race` + goroutine baseline; D-289) | internal/agentcfg + internal/runtime/agentcfg + internal/runtime/steering + internal/sessions + internal/config + internal/protocol/types | §6.9, §6.17, §6.16, §6.5, §6.15 | 157, 150, 152, 92a, 118, 83l | 85% | Shipped (v1.12) |
|159 | Serve-band promotion: the config→listener composition leaves `package main` (`harbor serve` serves the Protocol from stock yaml, but a scaffolded agent carrying compiled in-process Go tools cannot — `bootDevStack` + `devBootOptions` + the `devStack` serve/close lifecycle are trapped in `cmd/harbor`; promote that band into ONE importable internal package `internal/runtime/serve` — `internal/server` is already the protocol-server package, do NOT collide; the promoted constructor REQUIRES a non-nil auth-validator factory (nil = loud error; identity is mandatory §6) and mounts ONLY the surfaces every caller shares — the dev signer NEVER promotes; dev-only surfaces (bootstrap-token endpoint, dev mint/print, draft scaffolding, the dev key-rotate surface, Console mount, mock-LLM snapshot override, post-boot fixture seeding) are composed CALLER-SIDE by `cmd/harbor` through explicit injection seams on the promoted Options/Handle (extra pre-CORS routes, the transports auth-surface option, an LLM snapshot override, a post-boot hook with subsystem handles); dev-only POLICY stays cmd-side: the mock gate (D-089), hot-reload supervisor (D-099), dev signer/mint, drafts, Console embedding (D-091); `harbor serve`/`harbor dev`/`harbor console` become thin callers; SECOND CONSUMER same-wave (§13): `harbortest/devstack` re-wired onto the promoted band, deleting its hand-mirrored transports/mux block (~877–1310) — the same move D-197 made for assembly, with the kit's mux GAINING the options its mirror omitted (WithAgentsService/WithAuthSurface/WithGovernanceService/WithGovernanceKeyRotate — closing that drift IS the point); an honestly-enumerated NEW options/handle seam surface but ZERO wire changes, no new Protocol methods, no `ProtocolVersion` bump; D-025 served-handle concurrent-reuse N≥100 `-race` + goroutine-baseline teardown; D-291) | internal/runtime/serve + cmd/harbor + harbortest/devstack | §5.6, §5.4, §5.5, §6.1, §8 | 64, 110d, 118 | 85% | Shipped |
|160 | `sdk/server` facade + `harbor scaffold --with-server` + parity gate (the promotion's EXTERNAL consumer: a curated `sdk/server` facade — `server.Open(ctx, cfg, Options{RegisterCatalog func(tools.ToolCatalog) error})` → a handle with `Serve`/`Close`, D-204 alias/forward over `internal/runtime/serve` NOT raw protocol re-exports — so a scaffolded agent with compiled in-process Go tools serves the Protocol at parity with stock `harbor serve`; PRODUCTION-ONLY posture, non-negotiable: `Open` ALWAYS builds the JWKS validator from `cfg.Identity` and fails loud (named field) when absent — NO dev-signer, NO mock knob; local-dev loop documented via `harbor token keygen` → `identity.jwks_file` → `harbor token mint`; `Open` re-runs `Validate` on programmatic configs so there is no validation bypass; the registrar mechanism is a NEW optional `assemble.Options.RegisterCatalog func(tools.ToolCatalog) error` invoked at the existing pre-policy `PreRegisterTools` application point — BEFORE builtin registration and the `tools.entries` Builder wrapping — so compiled tools receive declared approval/OAuth/policy wrapping, an adapter over the seam NEVER a second registration path (post-assembly `Catalog.Register` does NOT get wrapping — the named trap, §13); `harbor scaffold --with-server` (opt-in; default scaffold stays headless RunOnce) generates `cmd/<agent>/main.go` (loads yaml via `--config`/`--bind`, blank-imports `sdk/drivers/prod`, passes `agent.RegisterTools` to `server.Open`, serves); `harbor serve` calls the promoted constructor directly with a nil registrar (not via the facade); PARITY GATE scoped per leg: BOTH binaries from the SAME base config — (a) manifest-driven method-status parity (from `methods.Methods()`; script-side from the `wire-manifest.gen.json` methods key), (d) dev-only surfaces 404 on BOTH, (e) §17.3 real drivers + identity/401 + N≥10 stress + `-race`; SCAFFOLDED BINARY ONLY — (b) generated-tool discovery + dispatch through the catalog, (c) approval-gate wrap FIRES (proves pre-policy registration; the `tools.entries[]` block naming the generated tool lives in the scaffolded binary's config OVERLAY — a stock serve booted against it fails loud `ErrToolNotRegistered`, the deliberate fail-closed behavior, assertable as a negative; a wrap-fires mirror on both MAY use a builtin tool); CI/live split §17.8: the (b)/(c) mechanics gate is an in-module `test/integration` scripted-LLM test (83l/158 precedent) under `-race`; the wire-level subprocess end-to-end is an env-gated `HARBOR_LIVE_*` live leg (131d precedent) run as Stage 2 live verification; NO new Protocol wire types — no D-223/D-209 churn; §18 same-PR: `scaffold-a-harbor-agent` + `add-an-in-process-tool` + `configure-production-identity` + `use-the-harbor-protocol` + `embed-harbor-headless` recipe companion + docs/site stubs/nav + README pointer; D-292) | sdk/server + internal/runtime/assemble + cmd/harbor + test/integration | §3.6, §5.6, §5.5, §6.4, §8 | 159, 112a, 112b, 133, 131d, 118 | 85% | Shipped |
|161 | Session rehydration carries per-turn metadata (LIVE-TEST FINDING, operator-confirmed: reopening a Playground session hydrates message content but loses per-turn tokens/cost/latency — header shows "no turns yet" — the TOOL CALLS badges, and the model chip; root cause VERIFIED producer-side, not read-path: `state.history` read-back strips nothing (payload pass-through at `state_history_handler.go:297`; empirically probed — chunk + `planner.decision{DecisionKind,Tool}` payloads intact in the page), but (b1) `llm.cost.recorded` (the only `Model`/`Cost`/`Usage` carrier) is emitted ONLY inside the bifrost driver — mock/other drivers emit nothing; (b2) `tool.invoked/completed/failed` are emitted ONLY by the inproc transport driver — MCP/HTTP/A2A tools emit NO lifecycle events; (b3) even inproc's emits stamp an empty envelope RunID (triple-only quadruple, `inproc.go:185/:212`) with no payload TaskID — attribution-dead on BOTH live SSE (`taskIDOf`→'' drops the frame) and replay; FIX = one driver-neutral emit seam per producer on the one bus (brief 06 §5 two-channel-split lesson): the cost emit promotes to the MANDATORY LLM-edge safety wrapper (`llm.Open` wraps every driver, `registry.go:438-460`; bifrost's internal emit deleted — one emit per DRIVER-LEVEL completion, i.e. per attempt under retry, today's bifrost cadence, aligned with the attempt-cost governance tap) and the tool lifecycle emits land at the CATALOG-BUILD DESCRIPTOR-WRAP seam (`catalog.Register`, `catalog.go:68`, wraps every descriptor's `Invoke` in ONE universal shell — `desc.Invoke` has FOUR production call sites: the single executor `dispatch.go:238`, the parallel-executor branches `parallel.go:538` (the default for native N>1; ≥2 `tool.invoked` per parallel turn per `phase-107d.sh:97`), the MCP-Apps proxy `apps.go:270`, and the declarative-action re-invoke `declarative_action.go:236` — all four inherit the emit by construction, full run quadruple on the envelope; inproc's per-driver emits + the orphaned `tools.WithBus` option deleted; `wave7a_test.go` migrates) — which also fixes the latent LIVE attribution bug §17.6; payloads stay CONTENT-FREE (tool NAME+transport+status+attempts+duration, usage/cost/model figures ONLY — never args/results §7, sentinel-redaction test pins it; redactor publish path mandatory); read path UNTOUCHED (D-254 flat-events + client-side reduction + `MatchesScoped` scoping preserved; works on inmem within process lifetime — no durable-only feature); ZERO wire changes — no new method/types/event types, no D-223/D-209 churn (zero-diff proven on both gates); §13/D-062 consumer same phase: `HistoryTurn` gains tokens/cost/model/duration/toolCalls, `reduceHistoryTurns` folds cost sums + planner-decision tool rows resolved by tool lifecycle + duration (its doc comment finally true), `hydratePastTurns` populates header stats + per-message badges + TOOL CALLS + model chip — leave-and-return renders IDENTICAL to the live view (the acceptance centerpiece); stream-vs-readback key-equivalence test; MCP leg vs the real stdio fixture §17.8; N≥100 per-run attribution no-bleed `-race`; NON-goals: session-pinned-JWT enumeration posture, Overview Recent Activity, the phantom top_p fix (carried by the v1.13 wave-end §17.5 checkpoint punch list); D-293) | internal/llm + internal/runtime/dispatch + internal/tools + web/console | §6.13, §5.2, §6.4, §6.5, §7 | 125, 157, 118, 124 | 85% | Shipped |
|162 | `events.list`: durable, time-ranged, cross-session raw-event read (a second Protocol consumer needs "the raw events from 7 days ago to now, scrollable" — no surface answers it: `events.subscribe` is forward-only live tail, `events.aggregate` is counts-only, `state.history` is per-session + sequence-windowed (`state.ts:20-26`), `search.events` is text search not window enumeration; the wire `EventFilter` ALREADY carries `since`/`until` (`types/events.go:46-51`) with no row read consuming them; ADD additive `events.list`: the existing `EventFilter` + `limit` + a tail-first SEQUENCE-based cursor mirroring `state.history`'s `next_cursor`/`has_more` grammar, rows = the EXISTING flat `StateEvent` projection (same `payloadWireValue` pass-through + artifact-ref seeding — no new row shape); scope: non-admin = own verified triple only, fleet widening = verified `auth.ScopeAdmin` derived SERVER-SIDE never the body (§6 item 5) + `audit.admin_scope_used` once per request; honest `truncated` at the retention edge; substrate = the `HistoryReplayer` seam BOTH drivers implement (D-254 precedent — durable real windows over the persisted global-sequence log; inmem = ring contents + truncated, honest degradation, NO capability ceremony §4.4/§9); redaction + D-026 by-reference unchanged (rows stay the bus-redacted projection); D-062 consumer same phase: the Console Events page — whose empty state already hints at durable read-back (`events/+page.svelte:249-258`) — drives `events.list` from the existing `WINDOW_SPEC` picker (historical rows + scroll-up paging; live SSE tail unchanged; retention-gap notice); operator latitude exercised as NO additions (inspect-by-id: rows self-contained; type catalog: derivable from aggregate buckets; link reads: identity already on rows); full D-223 lockstep + D-209 regen; D-294) | internal/events + internal/protocol + web/console | §6.13, §5.2, §4, §7 | 124, 125, 72, 72a, 108h, 118 | 85% | Shipped |
|163 | Windowed-reads honesty pair: `flows.runs.list` since/until + retention horizons as data (TWO bundled asks: (1) LOW — `FlowRunsListRequest` filters only by `flow_id`/`tenants`/`page`/`page_size` (`types/flows.go:303-320`), the one run-history read with no time bound though rows carry+sort by `StartedAt` (`:289-290`,`:323`); ADD optional `since`/`until` RFC-3339 inclusive-lower/exclusive-upper mirroring `TaskFilter.Since/Until` EXACTLY (`types/tasks.go:211-214`) — additive, absent ⇒ unbounded, bounds before pagination, `until<since` → 400, scope rules unchanged; consumer same phase: the flow detail page's run-history table (`flows/[flow_id]/+page.svelte:147`) gains a server-side date filter; (2) MEDIUM — retention horizons as Protocol DATA: additive `retention` block on `runtime.health` with per-surface OBSERVED `oldest_retained_at` for events/tasks/sessions — the ask's "surface the existing retention config" premise was FALSE against the tree (NO retention knob exists; the durable log is "gap-free and untrimmed in V1" `durable.go:776`; `EventsConfig` `config.go:814-823` carries none; inmem horizon = the `ReplayBufferSize` ring), so the honest v1 shape is the observed head timestamp (configured-retention field additive later IF a pruning knob ever ships); placement `runtime.health` (polled operational surface) over `runtime.info` (identity/build-shaped), with rationale; pairs with the at-read `truncated` flag (125 + 162); consumer: Events-page window-edge honesty banner ("retains only back to X") composing with 162; the counters/metrics TSDB re-recorded as decided-NO (snapshots stay now-only `posture.ts:41/:131`; trends derive from the durable log; metrics scrape out-of-band RFC §6.14) so it is not re-opened; full D-223/D-209 regen; D-295 + D-296) | internal/protocol + internal/events + web/console | §5.2, §6.1, §6.13, §6.14, §7 | 26a, 108p, 72f, 108h, 162, 118, 125 | 85% | Shipped |
|164 | MCP OAuth requirement discovery, surfaced as data (a second consumer brokers downstream credentials centrally — the runtime PULLs at call time via `tokenexchange` (D-271) and never holds a credential; provisioning today needs a HAND-DECLARED provider descriptor, yet the MCP authorization spec (2025-06-18) makes servers ADVERTISE it: `401` + `WWW-Authenticate resource_metadata` → RFC 9728 protected-resource metadata → `authorization_servers[]` → RFC 8414/OIDC metadata (issuer, authorization/token endpoints, scopes, PKCE, optional RFC 7591 registration endpoint, RFC 8707 resource); DETECT at the MCP http(s) transport edge (net-new — no 401/WWW-Authenticate handling exists in the driver today, grep-verified) or on `mcp.servers.probe` (never a background crawler); DISCOVER via ONE chain walker composing the existing RFC 8414 `Provider.resolveEndpoints` (`provider.go:858`) — RFC 7591 `ensureClient` (`provider.go:924`) stays UNUSED, the registration endpoint is reported never invoked; SURFACE verbatim + provenance as an additive `oauth_requirement` on `MCPServerView` (`types/mcp_servers.go:51-79`), projected by list/get/probe; HARD boundaries: Harbor NEVER runs the flow / holds / refreshes tokens (custody stays consumer-side, D-271 stays PULL), discovered metadata is INERT UNTRUSTED data (report don't follow; a proposal an operator confirms, never auto-applied config); SSRF guardrails PER HOP (RFC 9728 hop = same-origin-as-server default + explicit origin allowance; the RFC 8414 authorization-server hop is inherently cross-origin so it ALWAYS requires the explicit allowance — most real discoveries need an operator grant, partial-surface without it; allowed cross-origin fetches also refuse private-range/IP-literal hosts; bounded redirects, timeout, size cap, https-only off-loopback, NO credentials — each negative-tested); §17.8 fixtures = RFC 9728 §3.2 + RFC 8414 §3.2 example documents committed as testdata (wrong-field mutation must fail); `mcp.servers.probe` triggers discovery (its `MCPProbeRow` return unchanged; requirement read via `get`/`list`); D-062 consumer same phase: the MCP Connections page (108m) renders the discovered requirement marked unverified; sibling reconciliation §13 (one mechanism, N consumers): the ready 85b + the parked 92p (reserved D-246) each reuse this single-homed discovery chain and add only their flow legs (Phase 148 precedent), pointer notes in both plan files (85b's this PR); no new method/event; full D-223/D-209 regen; D-297) | internal/tools/auth + internal/tools/drivers/mcp + internal/protocol + web/console | §6.4, §5.2, §6.15, §7 | 28, 30, 108m, 118 | 85% | Shipped |
|165 | Structured reasoning-steps rehydration (Phase 161/D-293 rehydrates per-turn stats + FLAT reasoning text + tool-call badges on reopen, but the STRUCTURED reasoning steps — the ordered per-ReAct-step native thinking the live view renders as a `ReasoningAccordion`, interleaved in order with the tool calls each step preceded — do NOT survive reopen: the reopened message shows only 161's flat `reasoningText`, not ordered `reasoningSteps`; VERDICT ZERO-WIRE Console-only, verified live probe + code trace 2026-07-11: the live path's steps come from the tasks.get ENRICHER trajectory projection which is IN-MEMORY-ONLY (`internal/tasks/protocol/registry_projector.go:205` → `Enricher.Trajectory` reads `trajectoryFn(taskID)`, returning nil when the trajectory is "unavailable (evicted …)", `internal/runtime/serve/enricher.go:49-56`) — on reopen the task record survives but the trajectory projection does NOT (`tasks.get` carries no trajectory field once the in-memory trajectory is reaped), so the enricher can never serve a reopened run; the ONLY durable source is the event stream, which ALREADY carries `planner.decision` (one per trajectory step, ordered by `sequence`, each with `ReasoningTrace` + `DecisionKind` + `Tool`) + `tool.invoked/completed/failed`, all in `state.history` read-back (D-254) and already read by 161's reducer for the badges — it merely ignores the `ReasoningTrace` key; byte-equivalence by construction: `emitDecision(rc,final,resp.Reasoning)` (`react.go:720`) and `rc.OnReasoning(resp.Reasoning)` feed the SAME value into the event's `ReasoningTrace` and `trajectory.Step.ReasoningTrace` (`runloop.go:724-725/:921`), enricher projects it verbatim; redaction is a no-op (DecisionPayload is `SafeSealed` → the bus skips the redactor, `inmem.go:369-374` — persisted trace = raw, byte-identical to the enricher's); corrected source model: `reasoning_trace` is NOT a ReAct Thought scratchpad (the textual `Reasoning` action field was REMOVED, `decision.go:36-38`) — it is native `llm.CompleteResponse.Reasoning` bucketed per step; FIX (corrected index rule): `reduceHistoryTurns` folds `planner.decision.ReasoningTrace` into `HistoryTurn.reasoningSteps {index,reasoning_trace}` ONLY for step-appending `DecisionKind ∈ {CallTool,CallParallel,SpawnTask,AwaitTask}` (the runloop appends a `traj.Step` only in its switch `default` branch, `runloop.go:917-923`; `Finish`/`RequestPause` return without a step, `:795-798`, yet still emit a `planner.decision` — a reasoning-bearing `Finish` or mid-run `RequestPause` would phantom-add/shift steps), incrementing the per-run STEP ordinal only on those and emitting only non-empty, `hydratePastTurns` sets `message.reasoningSteps` (MessageBubble already prefers steps over flat text `:176-178` → `ReasoningAccordion`); empty-step run falls back to 161's `reasoningText` (no regression); acceptance centerpiece: reopen renders the ordered reasoning↔tool interleaving IDENTICAL to the live view; Console vitest (ordered reconstruction + byte-equivalence-vs-`parseReasoningSteps` + page-boundary no-dup/reorder) + rehydration regression; NON-goals: the flat `reasoningText` path (161 ships it), the Anthropic-native-thinking-empty question (RESOLVED, separate bifrost-layer), tasks.get/trajectory/enricher changes (reconstruct from history, not per-run tasks.get); ZERO wire — no method/type/event, no D-223/D-209 churn (zero-diff proven); D-298) | web/console (Console-only) | §6.13, §5.2, §7, §6.2 | 161, 125, 107a, 118 | Console vitest bar | Shipped |
|166 | Credential-sink hardening (shipped-code security fix, NO new Protocol surface; the base the v1.14 wave stands on). Two adversarial reviews of the first-cut v1.14 shape proved the "no env-var NAMES on the wire" rule a SYMPTOM rule and returned NO-GO; the generalized invariant (D-300) is **no admin-writable field may determine where a credential is sent**. Three exfil paths, all reachable in SHIPPED Harbor (92f add path + D-278 southbound binding), fixed IN-BAND (§17.6): (1) `token_url` on the writable descriptor is where `tokenexchange.go:582` POSTs the org's real `client_id`/`client_secret` — an admin naming a legitimate broker + an attacker `token_url` receives the secret; (2) the CONNECTION `url` is where the exchanged bearer is injected and `resolveOAuthBinding` (`attach.go:288-320`) constrains NO host, while the provider name is the default token audience (`tokenexchange.go:214-217`) so the caller picks audience+scopes; (3) the token-exchange client is a bare `&http.Client{Timeout:30s}` (`tokenexchange.go:242-245`) that FOLLOWS redirects (Go replays the body on 307/308, re-POSTing the `client_secret`). FIX: a boot-declared `ToolOAuthProviderConfig.AllowedDownstreamHosts` allow-list enforced in `resolveOAuthBinding` (fail-closed on empty for a bindable provider; covers the `oauth2` binding too); a boot audience/scope ceiling (audience NOT derived from the caller-chosen name; scopes intersected); the token-exchange client hardened to the discovery client's bar (`net.Dialer.Control` private/loopback refusal `discovery.go:260-281`, `Proxy:nil`, `CheckRedirect` refuse — the `credsource/remote` `remote.go:127-134` precedent); AND the audit-ordering LIE fixed — `handleSetRawHTMLTrust` (`mcp.go:843-875`) applies-then-emits while its godoc claims fail-closed, so 168/169's audit ACs would be unsatisfiable; corrected to a genuinely fail-closed ordering + a shared helper 168/169 reuse. No new method/type/event; config-schema + internal only; CHANGELOG migration note for the mandatory allow-list; D-300) | internal/tools/auth + internal/tools/drivers/mcp + internal/config + internal/protocol | §6.4, §6.15, §7 | 142, 148, 154, 28 | 85% | Shipped |
|167 | Owner-scoped reconcile for runtime-added connections + providers (NARROW shipped-code correctness fix; EXTENDS D-287, does NOT reverse it). The first-cut v1.14 draft proposed keying the MCP registry + provider set by the `(tenant,user,session)` triple; two delta reviews returned NO-GO and CONVERGED, confirmed against code: (1) it BREAKS the common deployment — boot MCP servers attach ONCE under one `mcpDefault` identity (`assemble.go:929`) but are read under many session triples, so triple-keying fragments deployment-wide servers into per-session buckets and they vanish from the Console/posture; (2) it does NOT isolate — dispatch + bearer injection go through the process-global BARE-NAME catalog (`catalog.go` — `byName`, `Resolve(name)`, not identity-filtered) which 169 declines to widen, so keying METADATA leaves same-named runtime-adds colliding (`ErrToolDuplicateName`) or cross-serving; (3) it silently REVERSES D-287 (settled + PR-464-hardened: catalog+registry shared across sessions, refcount/drain rejected). CORRECTED MODEL (operator): boot infra stays PROCESS-GLOBAL (D-287 preserved — do NOT key the boot registry or catalog); runtime-ADDED connections + Protocol-installed providers carry an OWNER tag `(tenant,agent)` used for exactly (a) agent-config revision ownership (already `ConfigScopeAgent`) + (b) RECONCILE-VIEW SCOPING so a run-start reconcile touches only ITS OWN owner's runtime-adds — never boot servers, never another owner's adds (exactly what the two NOTEs `projection.go:125-132`/`mcp_detacher.go:77-87` asked for: a per-agent reconcile VIEW, NOT full-triple keying; §6 says agent_id is not an isolation key). The wave claims NO hard cross-tenant isolation of runtime-added tool DISPATCH in a shared runtime — a shared runtime trusts co-tenant admins (a name collision fails loud); hard isolation = ONE-RUNTIME-PER-TENANT (which gets full isolation for free). NO false safety property (the prior draft's FAIL-A). The two NOTEs are REWRITTEN (not deleted) to describe the deliberate process-global boot behaviour + the owner-scoped reconcile. Fixed AC: `TestRegistry_BootServerVisibleToEverySession` + `TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner` (the discredited `TestRegistry_IdentityKeyed_NoCrossTenantOverwrite` used two tenant-differing triples that couldn't distinguish tenant-key from full-triple-key — FAIL-B). No new Protocol surface; single-tenant behaviour-identical; unblocks 168/169. D-301) | internal/tools/drivers/mcp + internal/tools/auth + internal/runtime/serve + internal/runtime/agentcfg + internal/mcpconsole | §6.4, §6.9, §6.16, §7 | 166, 28, 92f, 156, 92a | 85% | Shipped (v1.14) |
|168 | Live MCP OAuth discovery-allowance write (re-homed from the first-cut v1.14 Phase 166; lands ON the owner-scoped reconcile (167) + the sink hardening (166); the MCP registry stays process-global bare-name — D-287). Phase 164/D-297 shipped MCP OAuth-requirement discovery, but its RFC 8414 authorization-server hop is inherently cross-origin and needs a per-connection origin allowance existing only as restart-required yaml (`config.go:1308`), unreachable to a Protocol consumer — and discovery is INERT for every runtime-added connection. **CORRECTION (was a false "shipped bug drops the field" claim):** nothing upstream CARRIES the allowance (`AttachRequest`, the descriptor, the wire type all lack it) — the mechanism is a §17.1 cross-package WIRING GAP (164 shipped the walker, 92f the add path, neither joined them); the discriminator is a within-phase round-trip guard, NOT a pre/post-attacher comparison (which was unachievable). FIX: the descriptor gains the non-secret `OAuthDiscoveryAllowedOrigins` (spine for free, D-283); the wiring is closed descriptor→`AttachRequest`→`config.MCPServerConfig`→the process-global bare-name registry; ONE narrow admin verb `agent_config.set_mcp_discovery_origins` FULL-REPLACE writes the revision under `lockAgent` AND applies live via `Registry.SetOAuthDiscoveryOrigins` (bare-name, identity-mandatory for auth); REVOKE is live+symmetric (prunes the recorded `oauth_requirement`'s AS entries fetched from the revoked origin, building a FRESH requirement and swapping the pointer under the lock — the registry hands it out by pointer `registry.go:605`, WARN 10); ROLLBACK takes effect live via an OWNER-scoped run-start allowance-reconcile leg doing a FULL IDEMPOTENT re-prune (FAIL 7 — no revision-vs-runtime half-write; WARN 11 — the re-prune bounds the mid-walk self-heal); NO allowance-generation counter; allowance ≠ SSRF bypass (post-DNS dial guard still refuses a granted private/loopback origin, named test); shared `config.ValidateDiscoveryOrigin`; admin-gated by omission from the exception maps (D-219, D-284); audit via 166's corrected fail-closed helper; D-062 consumer — write single-homed on `/agent-config`, `/mcp-connections` deep-links (resolving connection→agent from the agent-config registry first; boot-declared/unowned → honesty copy, no link, item 12), and Phase 156's caller-less `remove_mcp_connection` gets its first Svelte caller; 92m collision RULED (its parallel add-request OAuth block must route through 169; pointer notes written into 92k/92m); §17.8 = 164's spec fixtures; full D-223/D-209 regen; D-302) | internal/agentcfg + internal/runtime/agentcfg + internal/tools/drivers/mcp + internal/config + internal/protocol + web/console | §6.4, §6.16, §5.2, §6.15, §7 | 167, 166, 164, 92f, 156, 152, 92h, 92i, 108m, 118 | 85% | Shipped (v1.14) |
|169 | Protocol-installed OAuth provider (ZERO-URL broker-pull shape) + the connection→provider binding (re-homed + DE-SCOPED from the first-cut v1.14 Phase 167; lands on 166 + 167). The binding is ALREADY Protocol-writable end to end; the provider must exist and the boot-built provider list is invisible to a Protocol client. **The writable descriptor carries ZERO URLs** — `{name, credential_broker, scopes?}` — honouring D-300's invariant (no admin-writable field determines a credential sink): the token endpoint, allowed downstream hosts, and audience/scope ceiling are pinned at boot on the named credential broker (166). A reflective test asserts NO field is a URL/env-var name, and `DisallowUnknownFields` rejects `token_url`/`auth_url`/`client_id_env`/`client_secret_env`/`remote` BY NAME (no decoy fields — the field cannot exist, resolving WARN 17); empty `credential_source` is a loud reject (WARN 16). Backing it: `auth.ProviderSet` — a NEW `providers.go` type (NOT `registry.go`, the OAuth DRIVER registry — WARN 9; NOT a §4.4 driver seam, it holds instances, no `drivers/prod` registration — WARN 20), bare-name for RESOLUTION (D-287) + owner-tagged for reconcile scoping (167), seeded from `BuildProviders`, consulted by the MCP attach path; the catalog builder keeps its boot map (D-292). INSTALL+UNINSTALL together: `agent_config.set_oauth_provider` (upsert+`Install`) + `agent_config.remove_oauth_provider` (drop+`Uninstall`→`Close`, verified `tokenexchange.go:360`→`mcp.go:1166-1182`), so a bound connection's next call fails LOUD; rollback runs the SAME uninstall through the reconcile seam; uninstall is deliberately breaking, defensible ONLY because 167 OWNER-scopes the reconcile (a tenant-B reconcile never closes tenant-A's provider — FAIL 6); boot-declared name collision refused; a shared-runtime cross-owner install-name collision fails loud (167's bounded guarantee). THE BINDING HALF is Console-only — the wire carries `oauth_provider`, the Console drops it (`state.svelte.ts:912-923`); 169 stops dropping it (a SELECT from the installed list) + pins the round-trip. NO credential VALUE over the wire (D-271 PULL); NO auto-install from discovered metadata (D-297); sentinel-redaction proves no secret/sink in any revision/diff/event/response; §17.8 = RFC 8693 / captured transcript; 92k/92m pointer notes written; full D-223/D-209 regen; departs from brief 09 (RFC 7591 / operator-burden) — recorded in D-303; D-303) | internal/tools/auth + internal/runtime/agentcfg + internal/agentcfg + internal/tools/drivers/mcp + internal/protocol + web/console | §6.4, §6.16, §5.2, §6.15, §7 | 166, 167, 168, 142, 154, 148, 92f, 92h, 152, 118 | 85% | Pending |

---

|170 | Same-origin MCP OAuth-discovery dial (HA-19 — reproduced live: Phase 164/D-297's report-only discovery walker can NEVER complete against an MCP server on localhost / a container-compose network / a private VPC — the ordinary self-hosted posture; it dies one hop EARLIER than the correct allowance boundary, on the RFC 9728 protected-resource-metadata fetch back to the very server the runtime already dials successfully for tool calls; ROOT CAUSE = two SSRF guards that DISAGREE: the per-hop policy `validateHop` (`internal/tools/auth/discovery.go:427`) correctly PERMITS the same-origin protected-resource hop even to a private address (`sameOrigin` at `:435`; `case StepProtectedResource` at `:442` refuses only cross-origin; cross-origin IP-literal refusal fires only `if !sameOrigin` at `:455`), but the dial-time backstop `net.Dialer.Control` in `NewDiscoverer` (`:260-281`) UNCONDITIONALLY refuses every resolved private/loopback IP via `isPrivateIP` (`:276-278`) with NO same-origin exemption and NO knowledge of which hop it serves, so the hop `validateHop` just approved dies at connect; the only relaxation `WithPrivateNetworkAccessForTest` (`:232`) PANICS outside a test binary, so there is NO production path to the working dial — and every positive-path chain-walk test uses that test-only escape, which is exactly why the gap shipped green (§17.8); FIX = align BOTH dial-time guards to the per-hop policy: swap `Control`→`ControlContext` (Go 1.20+; module is 1.26) so the dialer runs post-DNS-resolution AND reads a per-fetch `dialPin` from ctx naming the step, plus the connection's operator-declared boot-config `ServerURL` resolved ONCE at walk start into a resolved-IP SET `pinnedIPs` + `pinnedPort` (trusted origin, NOT the attacker-influenceable `resource_metadata` URL); permit a private resolved address for EXACTLY the same-origin protected-resource hop when `resolvedIP ∈ pinnedIPs && resolvedPort == pinnedPort`, and NOTHING else; SECOND guard aligned — `validateHop`'s https-off-loopback refusal (`discovery.go:449-452`; loopback = localhost/127.x/::1 only) is EXTENDED so a plain-HTTP NON-loopback compose/k8s MCP server (two of HA-19's four postures, config permits plain-HTTP MCP URLs) discovers on the same pinned predicate (non-pinned/cross-origin plain-HTTP still `ReasonNotHTTPS`); `DisableKeepAlives: true` so every dial (incl. redirects) re-enters the gate; CRUX = DNS rebinding: same-origin is by ORIGIN STRING but SSRF defence is by RESOLVED IP, so an attacker-influenced `resource_metadata` URL or same-origin-by-string redirect resolving to a DIFFERENT private IP must STILL be refused — resolved-IP-set membership is the gate; PORT is part of the pin (refuses intra-host `302→{pinnedIP}:22`/`:6379` port SSRF); PRESERVED (each a named test): cross-origin hops keep the private-IP refusal + rebinding defence; relaxation gated by `step == StepProtectedResource` (an AS hop whose target IP is IN the pin still refused); a redirect off the pinned target refused at its dial by `ControlContext` (NOT a `validateHop` re-entry); the `oauth_discovery_allowed_origins` allowlist still gates EVERY authorization-server hop (`needs_allowance` stays the intended halt — HA-15/168's surface; a granted allowance still can't auto-fetch a private-address AS); bounded redirects/8 KiB size cap/5 s timeout/no-proxy/credential-strip-on-redirect all unchanged; RFC 7591 registration REPORTED never INVOKED (D-297 report-don't-follow); runtime NEVER runs the flow / holds a token (D-271); NO production `allowPrivate` knob — `WithPrivateNetworkAccessForTest` stays test-only; §17.8 tests exercise the PRODUCTION construction path (no test-only escape) against the Phase 164 spec-derived RFC 9728/8414 fixtures across ALL THREE self-hosted postures (loopback-hostname + IP-literal + plain-HTTP non-loopback service name) — same-origin private hop COMPLETES, cross-origin private hop refused, AS-to-pinned-IP refused, rebind-to-different-private-IP refused, different-port refused, redirect-off-target refused; SECURITY-SENSITIVE relaxation planned with the rebind crux + preserved-property list binding; runtime-internal dial policy — NO wire/config/CLI/Console change, `ProtocolVersion` unbumped, no D-223/D-209 churn; §18 exempt (operator steps unchanged; the previously-blocked data path merely becomes reachable — `observe-with-the-console` MCP-Connections prose already describes the now-restored behavior); D-304) | internal/tools/auth | §6.4, §5.2, §7 | 164, 28, 30 | 85% | Shipped (v1.14) |

---

|171 | `events.aggregate` durable-driver parity + conformance-matrix closure (HA-18 + HA-20, the D-283 pattern — fix the instance AND close the class same PR; v1.14 Track B). HA-18: `events.aggregate` 500s on the durable driver EVERY call — the aggregator replays via the per-session `Replayer.Replay` path with a session-less `Filter{Admin:true}` the durable driver correctly refuses (`ErrIdentityScopeRequired`, "durable replay requires a SessionID"), while inmem honours `Admin:true` and fans in, so it works in dev and 500s in prod; `events.list` ALREADY does the session-less admin fan-in on durable via `listWindowDurable` (the `ListKind` maintenance head-scan). FIX: source the window snapshot from the same `HistoryReplayer` cross-session fan-in (ONE read ⇒ ONE `audit.admin_scope_used` on the widened path; the effective `Since/Until` threaded onto the substrate query; a generous aggregation bound keeps memory equal to today's whole-ring materialization; a window past the bound returns PARTIAL buckets with the additive `EventAggregateResponse.Truncated = true` UNIFORMLY on both drivers, NEVER a 400 — the earlier `ErrAggregateWindowTooLarge → 400` was the review F1 FAIL: it forked 400-on-durable / 200-on-inmem for the same request, the whether-it-works divergence HA-20 kills), threading the handler's SERVER-DERIVED `widened` decision (D-299) computed on the RAW pre-fold filter as a Go input never a wire field (also fixes a latent audit-integrity bug: the hardcoded `Admin:true` emits `admin_scope_used` TODAY on inmem for EVERY aggregate incl. non-admin own-session reads, `inmem.go:640`); the new substrate also EXCLUDES bus-internal notice types the old `Replay`+`MatchWire` path counted (a latent-bug fix owned in D-305); aggregate + list now agree by construction; `Replayer.Replay` UNCHANGED (its per-session SSE-reconnect refusal is correct). HA-20: close the matrix hole — a driver-parametrized `Aggregate_Admin_SessionLess_FansInAcrossSessions` scenario over a MULTI-TENANT fixture (≥2×2×2) with an explicit ISOLATION assertion (parity ≠ isolation) + a four-method parity leg (`events.aggregate`/`events.list`/`events.subscribe`/`state.history` against every registered driver → same answer OR same named sentinel) + a registry gate that BLANK-IMPORTS `internal/drivers/prod` (a `RegisteredDrivers()` entry with no conformance run fails the build); the thesis made executable — a driver difference may change WHAT (depth via `truncated`, observed horizon) never WHETHER a method works, differences stay DATA/named sentinels (`ErrReplayUnavailable`, `truncated`) never a 500 never a status fork never normalized (do NOT make inmem durable, do NOT normalize retention); ONE additive wire field `EventAggregateResponse.Truncated` (`ProtocolVersion` stays 0.1.0; full D-223/D-209 lockstep + §18 SKILL.md); §17.1 integration test = 200-not-500 on durable over a real StateStore (§17.8) + identity propagation + ≥1 failure mode; D-305) | internal/events + internal/protocol/types + internal/protocol/transports/stream + web/console | §6.13, §5.2, §6.5, §4, §7 | 72a, 162, 124, 125, 118 | 85% | Shipped |
|172 | `events.aggregate` origin-anchored (epoch-aligned) bucket grid (HA-16; v1.14 Track B). The aggregator lays its grid from the wall-clock instant at handler entry (`windowStart := now.Add(-req.Window)`); `Window % Bucket == 0` constrains bucket-series LENGTH but never ORIGIN, so two calls at two instants return two different boundary sets — a `bucket_start` is not addressable twice and no consumer can cache a bucket. ADD an OPTIONAL `anchor` on `EventAggregateRequest` (zero ⇒ today's clock-anchored grid); when set, boundaries floor onto `anchor + k·Bucket` so two calls with the same anchor+window+bucket share boundary instants (a bucket is re-requestable/cacheable) and a cold N-bucket fill is ONE call; the Unix epoch yields a globally-shared grid. Chosen over explicit `{since,until,bucket}` as the smallest additive surface composing with `Window`/`Bucket` and not duplicating the `Filter.Since/Until` clamp. NO change to bucket contents/redaction/retention/identity axes; `ProtocolVersion` stays 0.1.0 (additive); full D-223 lockstep + D-209 regen + §18 `use-the-harbor-protocol` SKILL.md same PR; D-306) | internal/protocol/types + internal/events + web/console | §6.13, §5.2, §7 | 171, 118 | 85% | Shipped |
|173 | `events.aggregate` per-tenant attribution for admin-widened reads (HA-17; v1.14 Track B). An aggregate bucket is a bag of scalars (`{"tool.invoked":7}`) with NO tenant attribution, so — unlike a ROW read (`sessions.list`/`tasks.list`/`events.list`) where each row carries its tenant and a consumer post-filters the merged result against its entitled set — a consumer CANNOT verify an admin-widened count against the `Filter.TenantIDs` it asked for; for an aggregate the runtime's honouring of the filter IS the entire tenant boundary, a single point of enforcement with NO downstream check. ADD opt-in `by_tenant` → `EventBucket.CountsByTenant` (tenant → event_type → count) alongside the totals, returned ONLY for admin-widened reads (verified admin/console:fleet, server-derived per D-299); attribution keys ⊆ the authorized (named-or-folded) `Filter.TenantIDs`, with `Counts` and `CountsByTenant` scoped to the IDENTICAL set by construction (NO per-tenant entitlement mechanism — `ScopeAdmin`/`ScopeConsoleFleet` are GLOBAL binary fan-in grants: the body SELECTS tenants, the scope AUTHORIZES the fan-in; an unelevated caller gains attribution for NOTHING new); the per-bucket invariant `Σ counts_by_tenant == counts` proves it is a pure re-projection not a looser read path; per-bucket (not a response rollup) so it composes with the time series + 172's grid. Makes the isolation boundary independently verifiable on aggregates the way it already is on rows (§6 defence-in-depth); NO payloads/new identity axes; `ProtocolVersion` stays 0.1.0; full D-223 lockstep + D-209 regen + §18 SKILL.md same PR; D-307) | internal/protocol/types + internal/events + internal/protocol/transports/stream + web/console | §6.13, §5.2, §6.5, §4, §7 | 171, 172, 118 | 85% | Shipped |
|175 | Fleet-scoped retention horizons (HA-23 — the scope gap in the retention-horizon surface HA-14/D-296 shipped; v1.14 Track B). `runtime.health`'s `retention[]` block reports the three durable surfaces at THREE DIFFERENT scopes — `events` runtime-wide/identity-free (`RetentionReporter.OldestRetainedAt(ctx)`, CORRECT), `tasks` scoped to the caller's full TRIPLE (its session, via `TaskRegistry.List`), `sessions` scoped to the caller's TENANT (`SessionLister.ListSnapshots`) — and the provider OMITS an absent surface, so the SAME wire shape (no entry) means BOTH "surface retains nothing" AND "caller has nothing in scope," indistinguishable on the wire. The one caller that exists to observe the fleet — a coordinator polling under a dedicated `svc:` service identity that owns no sessions/tasks — therefore receives ONLY the `events` horizon; the `tasks`+`sessions` horizons are structurally empty and its cross-session windowed view silently undercounts (a session aged out of the sessions surface while its events remain → enumeration misses it → the correct "mark buckets incomplete when the sessions horizon < window" guard is INERT because the sessions horizon is unobservable at fleet scope). FIX (both): (1) a verified admin/`console:fleet` caller reads `tasks`+`sessions` at RUNTIME-WIDE scope (the same identity-free scope `events` already uses), server-derived from the verified session per D-299 (NEVER the request body), riding `runtime.health` (NO new method, NO new capability bit, NO ordinary-caller scope relaxation — that fold stays a fail-closed control); the runtime-wide read goes through an optional identity-free `OldestRetainedAt` reader on the tasks registry + session lister (mirroring `events.RetentionReporter`, type-asserted, NO `Supports*` ceremony); the widened path emits one fail-loud `admin_scope_used`. (2) absence made REPRESENTABLE — an additive `RetentionHorizon.scope` (`runtime`/`tenant`/`session`) + always-emit-for-the-three-known-surfaces so a consumer distinguishes "unobservable at your scope" from "surface retains nothing" and degrades honestly (the HA-18/20/21/22/23 through-line; D-311 class rule cross-ref). Composes with the D-308 events-fold work (Phase 172 — the `if !widened` guard on `events.list`+`events.aggregate` closing HA-16+HA-21): that work makes the session-less cross-session enumeration REACHABLE, this makes its completeness VERIFIABLE (orthogonal partner, not a hard dep). NO change to the `events` horizon/retention itself (Harbor has no retention knob)/redaction/counters+metrics TSDB (D-296 decided-NO); ONE additive wire field, `ProtocolVersion` stays 0.1.0; full D-223 lockstep + D-209 regen + §18 SKILL.md same PR; D-310) | internal/protocol/types + internal/protocol + internal/runtime/posture + internal/tasks + internal/sessions + web/console | §5.2, §5.5, §6.1, §6.13, §6.14, §6.16, §7 | 163, 118 | 85% | Shipped |
|176 | Session reopen: re-activate a closed session so a consumer chat resumes (AMENDS a Settled RFC decision — §6.9 "reopen-after-close is forbidden. Clients open a new session." becomes "a closed session — explicit OR GC-reaped — MAY be reopened"; the consumer-chat / white-label model needs conversations always-resumable, a user returning days later on the SAME conversation; reopen is CLEAN because close/GC reap the session RECORD not the DATA — `Registry.Close`/the GC sweep mark `Closed=true` + drop the live `openSessions` entry but leave the durable events/state/memory intact (the GC guards against resurrecting the record), the durable event log is gap-free + UNTRIMMED in V1 (no TTL/cap), the StateStore has no retention sweep; `Open`/`EnsureOpen`/`start` on a `Closed=true` record RE-ACTIVATES it — clears `Closed`/`ClosedAt`/`ClosedReason`, preserves the IMMUTABLE identity + `OpenedAt` (a reopen under a DIFFERENT tenant is still `ErrSessionIDReuse`, invariant 3 unchanged), stamps `LastReopenedAt`, refreshes `LastSeen`, re-adds to `openSessions`+catalog, lifts the erasure fence, emits a NEW content-free `session.reopened` SafePayload — history resumes intact; FAIL-1 (in-phase, not a follow-up): the GC hard cap now measures from `max(OpenedAt, LastReopenedAt)` (was `OpenedAt` alone, which would re-reap a reopened old conversation within one SweepInterval on every deployment) — `OpenedAt` is NOT refreshed (it is the erasure lifecycle discriminator, hence the separate stamp); THE ONE terminal exception = an ERASED session (Phase 130 `session.erase`): reopen fails loud `ErrReopenAfterErase` (NEW sentinel; never a silent empty-start — §5 fail-loud, §7 right-to-erasure), detected via an O(1) point-`Load` of a pending erasure LEDGER (in-flight/interrupted) OR a NEW durable content-free erasure TOMBSTONE (`erasureTombstoneKindPrefix`, written in `completeErasure`'s success criteria + RETAINED — sibling of the `session.erased` record-of-fact, same content-free shape so no new retained info; REQUIRED because the pending ledger is deleted on erasure success so it alone can't make a converged erasure terminal; a fail-closed StateStore point-read, NOT the fail-open/bounded/prunable `session.erased` event scan — WARN-2); FAIL-2: the erased gate fires on BOTH the closed-record branch AND the not-found/fresh-create fall-through (a converged erase removes the record, so a naive reopen would mint a fresh empty session — silent resurrection); race-safe by write-happens-before-delete (WARN-1) — the tombstone `Save` (SUCCESS-CRITICAL: a failure fails the erasure loud, never proceeding to `deleteLedger`) completes before the ledger delete, so `isErased == ledger ∨ tombstone` holds with no gap under the shared `r.mu` (the tombstone is NOT atomic with `DeleteScope` — different scope, written in step 6 — and needn't be); ALL §6 isolation preserved — identity-mandatory + immutable (`ErrIdentityMismatch` on stored-vs-ctx mismatch), no identity-downgrade knob; old blanket `ErrReopenAfterClose` retired from the reopen path (`Touch`-on-closed keeps a loud read-only guard renamed `ErrSessionClosed`); `protocol.ErrSessionReopenAfterErase` → a NEW machine-branchable wire code `CodeSessionErased` (`"session_erased"`, HTTP 409; §8 codes-added-there-only, NOT a `ProtocolVersion` break) — WARN-A, so a consumer chat branches on `code` not the advisory `Message` to route "conversation deleted — start fresh"; adding the code makes the D-209 regen FIX now-false prose the lockstep gate can't catch (the generator's `CodeInvalidRequest` join + the `auth-and-identity.md` choreography both drop the stale "reopen-after-close is forbidden" — the re-review docs-integrity FAIL); tombstone `Save` is UNCONDITIONAL per terminal `completeErasure` (outside the `recordAlreadyEmitted` skip guard, WARN-B) and `isErased` fails CLOSED on a non-NotFound Load error (WARN-C); §13 same-wave consumer — Playground/Sessions resume a closed session (a `start` on a closed id now succeeds), refresh on `session.reopened`, branch on `session_erased`; ONE new event + ONE new error code = full D-223 lockstep + D-209 events.md/errors.md regen; §17.1 integration test (real durable+inmem state/events/memory + real `CascadeEraser`: close→reopen→history intact on DURABLE; erase→reopen fails loud; cross-tenant + identity-mismatch reject; ≥1 failure mode; `-race`) + registry D-025 stress extended N≥100 incl. the reopen-vs-erase race; D-312) | internal/sessions + internal/protocol + internal/runtime/serve + web/console | §6.9, §5.2, §6.13, §7 | 130, 155, 125, 73c, 106, 118 | 85% | Shipped |
|177 | Projection-completeness gate + the populate/remove surfaces (HA-24; v1.14; the CLASS that HA-22/174 is one instance of — declared-but-never-assigned wire fields with filters/aggregates over them (false absence), on THREE more surfaces + a mechanical class-closer. TASKS: `TaskRow.HasPendingApproval` is READ by the `tasks.list` filter (`list.go`) but NEVER assigned by the sole producer `projectRow` (`registry_projector.go`) → `has_pending_approval:true` returns an EMPTY page on a fleet with open gates (the sharp one) → populate at projection time from the approval/pause registry; `BackgroundAcknowledged` (no filter) → represent/populate; ALSO the WIRED tasks `serve.Enricher` (`enricher.go`) is a STUB — `ParentSession` returns a zero `TaskParentSessionRef{}`, `Cost` a zero `TaskCostRollup` — so even the surface Harbor got STRUCTURALLY right ships zeros → un-stub the parent-session card (session registry) + cost rollup (cost events, coordinated with 174). FLOWS: `budgetConsumption` (`catalog.go`) sets `RequestsUsed`+`CostUSDUsed` but NEVER `TokensUsed` (non-omitempty) → fabricated 0 on `FlowSummary`/`FlowDetail` → add `RunRecord.Tokens` (symmetric with `CostUSD`) + sum it. MEMORY: producer never sets `AgentID`/`ExpiresAt`; V1 memory has NO TTL (`ExpiresAt` zero=no TTL) → `filter.has_ttl_expiring` + BOTH `expiring_in_1h` fields (`MemoryAggregates` :217 AND `MemoryHealthAggregate` :346, neither omitempty) are STRUCTURALLY DEAD → REMOVE (breaking wire-shape change, D-223/D-209, RFC §8 exemption stated: always-empty → no live consumer + the 0.1.0 within-version precedent, 171); `filter.agent_ids` → V1 mechanism is LOUD-REJECT (`ConversationTurn` carries no producer identity, so nothing to populate from; populate deferred to a follow-up that adds it), never a false-empty page (D-311). SESSIONS: this phase ADDS the sessions registration (174 predates `projectioncheck`; serialized AFTER 174, edits its `lister_projector.go`). TOOLS: split to 178 — no production `Annotator` exists (only a `fakeAnnotator` test double, §17.8), so Phase 177 honestly GATES the annotator-backed surface behind ONE annotator-wired capability toggle: facet filters loud-reject when unwired; the response-riding catalog aggregates carry an explicit `aggregates_partial` marker (Console renders "unavailable," never a silent 0) pending 178. THE GATE (centerpiece, the class-closer) — TWO halves because the class has two variants: a registry-gated projection-completeness check (`internal/protocol/projectioncheck`) where every surface self-registers a `ProjectionContract` (probe + filtered/sorted/aggregated field-set + reason-carrying honest-omission allow-list + a prod-wiring-test name). HALF A (never-assigned): reflect each probe, FAIL when a filtered/sorted/aggregated field is left zero and not allow-listed (and on an empty allow-list reason). HALF B (never-wired — the variant that motivated this band): each surface MUST register a prod-wiring test exercising the projector as assembled through real `mux` wiring, so a forgotten `WithX` (passes Half A's fake-backed probe, ships zeros in prod — the tools bug) FAILS; a surface with no prod-wiring test FAILS. A surface-coverage check asserts every known surface is registered — mirroring the events `RegisteredDrivers()` conformance gate (D-305). Honest exclusions NOT allow-listed (populated or not operated-over, so no entry): `FlowBudget.TokenCap`, `MemoryItem.AgentID`/`.ExpiresAt` ROW fields, `TaskParentSessionRef.SessionID`. The gate is the primitive; the surface fixes + 174's session fix + the tools interim-gating are its consumers so it lands green (§13). No `ProtocolVersion` bump. D-313) | internal/protocol/projectioncheck + internal/tasks/protocol + internal/sessions/protocol + internal/runtime/serve + internal/runtime/flow + internal/memory/protocol + internal/protocol/types + web/console | §5.2, §6.1, §6.4, §6.6, §6.8, §7 | 174, 08, 54, 60, 73c, 107a | 90% | Pending |
|178 | Tools production Annotator (HA-24; v1.14; the tools leg + second member of the D-313 band). The tools catalog projector reads OAuth/approval/last-used/metrics/content-stats/display-modes through the optional `Annotator` seam (`WithAnnotator`), but NO production `Annotator` is ever wired — `mux.go` `NewCatalogProjector` supplies ONLY `WithLoadingResolver`, and the sole implementer is a `fakeAnnotator` test double (§17.8) — so in prod `filter.oauth_statuses`/`filter.approval_policies`/the `Name+" "+Version` search axis/the catalog aggregates (`Active`/`PendingApproval`/`AwaitingOAuth`) all operate over structural defaults (never-wired variant, confirmed). ASSEMBLE a production `Annotator` (a §4.4-shaped concrete behind the shipped seam — no new interface) reading each annotation from its owning subsystem: OAuth from `tools/auth`, approval from `tools/approval`, last-used/metrics/content-stats read-time from the events stream, DisplayModes from MCP negotiation; WIRE it at `mux.go` via `WithAnnotator(...)` exactly as `WithLoadingResolver` (the one-line seam; the weight is the assembly); FLIP the Phase-177 annotator-wired capability on so the gated facets/search/aggregates go live; populate `Tool.Version` (or honestly-empty name-only search where a transport carries no version); and LIGHT UP the inert admin write path (`tools.set_approval_policy`/`tools.revoke_oauth`, which returned `ErrAdminUnsupported` because no annotator implemented the `ApprovalPolicySetter`/`OAuthRevoker` seams the projector already delegates to) — writes route back through `tools/approval`/`tools/auth` with audit, never a Console shadow store (D-061). With the annotator wired, the D-313 gate now ENFORCES the tools fields (their allow-list entries removed). No new Protocol method, no `ProtocolVersion` bump — the fields are already declared (177 gated them); the capability flips unwired→wired. D-314) | internal/tools/protocol + internal/runtime/serve + web/console | §5.2, §6.4, §6.15, §7 | 177, 28, 60 | 85% | Pending |

---

|174 | Session-projection enrichment / honest counters (HA-22; v1.14 Track C — the projection leg of HA-20, cross-refs D-179). A structurally-proven defect: `SessionRow` (`internal/protocol/types/sessions.go`) declares 18 fields but the sole producer `projectRow` (`internal/sessions/protocol/lister_projector.go:154`, the ONLY non-test `SessionRow{}` literal) assigns just 10 — EIGHT (`agent_id`, `agent_name`, `tasks_count`, `events_count`, `total_cost_cents`, `total_tokens`, `has_pending_intervention`, `has_failed_task`) are declared, typed, shipped on the wire, and NEVER assigned ⇒ permanently zero on every row. The harm is NOT cosmetic: the runtime ships facets/sort/cursor over those zeros returning FALSE ABSENCE — `filter.cost_above_cents` (`filter.go:38`) excludes EVERY row for any threshold ("show sessions over $5" ⇒ empty on a fleet full of them), `filter.agent_ids` / `has_failed_task` / `has_intervention` return empty-or-match-all, `sort=cost_desc` (`protocol.go:548`) silently degrades to id-ascending, the agent-name `query` axis (`filter.go:55`) never matches, and the Console ALREADY renders the "Most expensive" sort option (`sessions/+page.svelte:438`) + the `cost_above_cents` facet chip (`SessionFacetChips.svelte:169`) over this zero data. The test suite is green because `protocol_test.go`'s `sampleRows`/`mk` fixture assigns exactly the four fields the production projector never does (HA-20's class, polarity flipped; §17.8). FIX (option a, operator-preferred): give `sessions.Projector` the SAME read-time `Enricher` SEAM `tasks.Projector` ships (`internal/tasks/protocol/registry_projector.go:52-71`, honest-zero doc at `:39-40`) — but ONLY the seam is inherited: the tasks PRODUCTION enricher returns a ZERO rollup (`internal/runtime/serve/enricher.go:36-40`, cost deferred to the event stream), so the per-session SUM is NET-NEW code with its own correctness surface. The aggregation reads raw data owned one package over (`llm.cost.recorded` for cost/tokens; the task registry for `tasks_count`/`has_failed_task`; the durable substrate via `HistoryReplayer.ListWindow` for `events_count`; the pause registry for `has_pending_intervention`) and sums per session, NO shadow store. WARN-1 truncation honesty (a D-311 instance, NOT perf): `ListWindow` is a BOUNDED scan returning `HasMore`/`Truncated` (`aggregate.go:230` `scanBound`) — a truncated per-session scan undercounts SILENTLY (the class recursing), so the enricher names its bound + surfaces `SessionCounters.Partial` → additive `SessionRow.CountersPartial` (honest lower bound, cost_desc/cursor over a partial key non-authoritative). WARN-3: sessions runs SERVER-SIDE facets/sort over the counters (tasks does not), so honest-zeros-when-unwired is INSUFFICIENT — the unwired build reproduces the defect; gate the numeric facets/sort behind an enricher-wired capability OR prove production always wires (deps present at `mux.go:371`). The two agent fields have NO single-valued session→agent binding today (the registry keys by the triple; a session may run multiple agents) — so they take the class rule (D-311): absence made representable + `filter.agent_ids` ONLY fails LOUD (`invalid_request`); WARN-4: the multi-field `query` (OR over session_id/agent_name/agent_id/user_id, feeds the Console search box `+page.svelte:81`) is NEVER failed whole — it matches populated sub-fields, honestly never-matches agent sub-terms; WARN-5: `SessionSort` has NO agent axis, so no agent sort exists to reject. Delivers D-179's deferred "V1.3 evolution" per-row cost aggregate (EXTENDS, not supersedes, D-179 — its Console Cost-History tab stays valid). Console: the shipped cost sort + facet become TRUTHFUL (not un-hidden — already rendered `+page.svelte:438` / `SessionFacetChips.svelte:169`); the agent fields have NO lying control (no `agent_ids` facet in the route; Agent column already degrades to `—` `+page.svelte:604`). Additive wire signals (`counters_partial`; agent nullable `*string omitempty` OR a `session_agent_binding` capability bit) — `ProtocolVersion` stays 0.1.0, full D-223/D-209 lockstep (incl. regenerated `docs/site/protocol/types.md`) + §18 `observe-with-the-console` + `use-the-harbor-protocol` SKILL.md same PR; §17.1 integration test = a populated counter reaches the wire over real drivers (§17.8 fixture pinned against the real producer) + identity propagation + `agent_ids`-fails-loud + truncation-honesty failure modes; D-309 (+ D-311 shared class rule) | internal/sessions/protocol + internal/protocol/types + cmd/harbor + web/console | §6.9, §5.1, §7 | 08, 73c, 107a, 54, 72a | 85% | Shipped |

V1 critical path: phases 01–82 + 26a + 36a + 36b (85 phases beyond skeleton). Post-V1 follow-ups: phases 83–84, 86–100, plus the lettered bands 83a–e (ReAct prompt depth + reasoning-channel decoupling) and 85a–j + 85m (MCP client/host compliance — the prioritised first post-V1 work; 85k is the separate Harbor agent-builder skills phase). The integer phase 85 (Skills Portico provider driver) was removed; the 85-band is now MCP compliance. Per the MCP 2026-07-28 RC re-plan (2026-05-28) the 85-band re-shapes: 85a / 85b / 85f are ready now; 85d / 85m revisit after SDK-RC (≈ Aug 2026); 85g / 85j revisit after RC-final (2026-07-28); 85c / 85e / 85h / 85i are cut. Governance is 91–96, Multimodal-output 97–99, Recipe loader 100. The next release tag is V1.1.x — both the hygiene + positioning + UX band (101–104 + 108) and the Playground-depth band (105 + 106 + 107 + 107a + 107c + 107d) roll up under it; the previously-sketched V1.2 / V1.3 splits collapse. Phases 105–107c ship with this release: Console first-attach UX (105), Playground real assistant response (106), the streaming completion pipeline (107), reasoning trace projection (107a), and native tool-calling + deferred tools/skills + search meta-tools (107c) — the four built-in `*_search`/`*_get` meta-tools plus the optional `declarative_action` escape-hatch tool preserving brief 07's prompt-engineered path for weaker models. The 107b streaming answer extractor was deliberately superseded by 107c (one cutover instead of stop-gap-then-replace); the file at `docs/plans/phase-107b-streaming-answer-extractor.md` is kept as historical context. Phase 107d (shipped) is the native-tool-calling follow-up that closes 107c's documented serialization carve-out: it wires the already-shipped `internal/runtime/parallel.Executor` (Phase 47 / D-056) into the dev `ToolExecutor`, flips the React planner to native `CallParallel` emission for N>1 tool-calls, and pins the `JoinKind`-collapses-to-`JoinAll`-on-native semantic (D-169). Phase 107e (pending) closes the last `ErrDecisionShapeUnsupported` carve-out the dev `ToolExecutor` carries: it wires `planner.SpawnTask` + `planner.AwaitTask` dispatch through the already-shipped `tasks.TaskRegistry` (Phase 47 / D-056) and teaches the per-task RunLoop driver to drive `KindBackground` tasks (closing the D-097 dead-task gap for the background kind), bounded by a new `planner.absolute_max_spawn_depth` recursion cap; on the synchronous V1.1.x runloop a retain-turn spawn blocks in-decision and a non-retain-turn spawn is joined by an explicit `AwaitTask` (eager push wake-on-resolution is a documented steering-runloop follow-up). SpawnTask + AwaitTask dispatch land together per §13 (D-170). Phase 108 starts a 14-round page-by-page visual-polish series (one phase per Console page, anchored to `docs/design/console/page-*.md` + `docs/design/console/CONVENTIONS.md`) and is the largest piece still pending under V1.1.x. Background context for the native-tool-calling cutover: research brief 15. **Immediately after Phase 108, the three-phase "MCP Apps host" wave 109a–c lands (D-172):** 109a (MCP Apps runtime + Protocol surface — `_meta.ui.resourceUri` parse, `ui://` projection, `mcp.servers.read_resource`, real DisplayMode negotiation, app-tool-call proxy), 109b (Console sandboxed-iframe host + the official `ext-apps` AppBridge in manual-handler mode + inline DisplayMode), 109c (fullscreen-tab + pip-split DisplayMode layout). This wave **deprecates and supersedes Phase 85g**, pulling MCP Apps forward from the post-V1 85-band: Apps is a stable independent extension (`io.modelcontextprotocol/ui`), not gated on the July RC, and ships an official host bridge that removes 85g's hand-rolled-bridge risk. The architectural invariant is D-173 — the AppBridge runs in manual-handler mode and every app→host call is Protocol-proxied through the Runtime, never a direct MCP connection, so an in-iframe app stays inside the `(tenant, user, session)` isolation boundary and the unified approval/OAuth gates. The 14-round page-polish series continues from the next free integer after the 109 band; the band precedes it in execution order, it does not displace it. **Live Runtime reframe (2026-06-01, D-177):** after 108d shipped the topology-first Live Runtime page, an operator review found it low-value and Playground-overlapping on the dominant planner/RunLoop runtime (no engine graph). Phase 108e supersedes the topology-first composition (D-126) with a single-runtime **capability-adaptive cockpit** — the runtime's advertised `runtime.info` capabilities compose the page (an always-present spine + capability-gated topology / health / cost panels), so it is full on a planner runtime and richer on engine/multi-agent shapes with no rebuild. Plan: `docs/plans/phase-108e-live-runtime-capability-cockpit.md`. **Protocol auth-hardening sequence (114–116, D-219):** a planning + adversarial review of the Protocol surface found a steering-control privilege escalation — `dispatchControl` derived caller scope + tenant from the request *body* instead of the verified context identity, so a caller could assert `scope:"admin"` in the body and the cross-tenant gate could never fire. Phase 114 (shipped) closes it: the control surface now reads authority from `identity.From(ctx)` + the JWT scope claims, fails closed when no verified identity is present, and a non-admin caller can steer only runs it owns (admin for cross-tenant). 114 is the prerequisite hardening for the lesser-privileged-token work: Phase 115 adds production JWKS verification + a `harbor serve` auth path (giving the inert `JWKSURL`/`JWKSFile` config fields a consumer), and Phase 116 introduces the non-admin session-scoped token contract — the consumer that makes 114's derivation load-bearing and the seam where the `session_user` tier becomes safe to grant. Independently, Phase 117 hardens the chat module's encapsulation boundary (D-091) so it renders self-contained — its own theming contract, font-family inheritance, and host/theme parameterization — with no Console look-and-feel leakage, and Phase 118 builds the long-tracked `protocol-ts-gen-check` gate as a field-level lockstep VERIFICATION of the hand-maintained per-page TS client against a committed, Go-generated wire manifest (`cmd/harbor-protocol-ts-lockstep`) — a D-093 deviation (D-223): the "generate" half (per-domain generated TS type modules) is a deferred future phase and the `cmd/harbor-gen-protocol-ts` name stays reserved for it.

`Shipped*` (Phase 73): the phase was **dissolved** — its surface was decomposed across the Console page phases that consumed each slice; the methods with no V1 consumer are deferred post-V1. See the Phase 73 detail block and D-133.

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
**§4.3 deviation (shipped).** The master-plan line "TokenStore (InMem + SQLite + Postgres drivers)" was implemented as a typed wrapper over the existing `state.StateStore` §4.4 seam (D-027) — the same approach Phase 50 (D-067) and Phase 53a (D-068) took for their persistence layers. Driver pluralism (in-mem / SQLite / Postgres) is inherited from the `StateStore` triad; the Phase 30 conformance suite runs the same `TokenStore` assertions against every `StateStore` driver to prove parity. This avoids the §13 two-parallel-implementations smell. Documented in D-083.

### 31 — Tool-side approval gates (RFC §6.4, §3.3)

**Goal.** Synchronous "approve this tool call" gates using the same pause/resume primitive — distinct from OAuth, simpler payload shape.
**Acceptance.** APPROVE/REJECT round-trip via the protocol; reject path raises typed `tool.rejected` events.
**Tests.** Integration.
**Deps.** 30.
**§4.3 deviation (shipped).** The master-plan row's owning-subsystem `tools/auth` was the right home for "approval as another consumer of the OAuth machinery." The implementation chose a SIBLING package `internal/tools/approval` under `internal/tools/` so the approval gate has zero OAuth baggage (no `TokenStore`, no `Sealer`, no PKCE / RFC 7591 / discovery surface — none of which an HITL approval gate needs). The two siblings (`auth/` + `approval/`) share the Coordinator + bus + redactor seams via the public `pauseresume` / `events` / `audit` packages; nothing else. The master-plan row's subsystem column was updated `tools/auth → tools/approval` in the same PR. Documented in D-086 §1 ("the approval-gate package is a SIBLING of `internal/tools/auth`, not a subpackage").
**Settled decisions:** D-086.
**See also.** `docs/plans/phase-31-tool-approval-gates.md`.

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
**Deviations (§4.3, see D-076).** (1) `NodeName` / `Producer` are realised as the reserved `Event.Extra["node"]` / `Event.Extra["producer"]` keys — not new `events.Event` struct fields — because the Phase 05 `Event` doc already reserves `Extra` for "Phase 56's bounded low-cardinality metric labels"; no `events.Event` shape change. (2) The static cardinality-lint flags `attribute.*` calls only when nested inside `metric.WithAttributes(...)` — a span's `attribute.String("run_id", …)` inside `trace.WithAttributes` is legitimate (D-073) and is left alone; the rule is metric-labels-only. (3) The `/metrics` endpoint ships as the standalone `telemetry.PrometheusHandler` `http.Handler` constructor; the live Runtime server that mounts it at `/metrics` is the Phase 60+ bootstrap (there is no `internal/server/` yet). (4) The master-plan "§11 Q-5" citation: RFC §11's Q-5 is the skill-versioning question; the metrics-exporter question is brief 06 Q-2, resolved by RFC §6.14 — "§11 Q-5" is read as "the §11-tracked metrics-exporter question is settled".

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

**Goal.** SSE stream for events; REST/JSON for control surface. Identity-scope enforcement at edge. **Q-1 RESOLVED 2026-05-14 — SSE + REST** (owner sign-off given; RFC §5.4 + §11 Q-1 updated). Phase 60 is now a normal implementation phase, not a decision gate. WebSocket remains an additive alternate transport for a later phase via the `internal/protocol/transports/` seam — not a fork of this phase.
**Acceptance.** Console can stream events and submit control over SSE+REST; smoke covers both directions.
**Tests.** Integration; full duplex stress.
**Deps.** 58, 05.
**Risks.** Q-1 resolved — the load-bearing decision is settled. Remaining risk is ordinary implementation risk (SSE keepalive/reconnect discipline, identity-scope enforcement at the edge).

### 61 — Protocol auth + identity-scope enforcement (RFC §5.5, §4)

**Goal.** JWT (asymmetric only); `(tenant, user, session)` in claims; admin/console:fleet scopes for elevated subscriptions.
**Acceptance.** Missing claim rejected with audit; HS\*/`none` algorithms rejected at parser level.
**Tests.** Unit + integration; security suite.
**Deps.** 58, 60, 01.
**Status.** Shipped — D-079. `internal/protocol/auth` ships the transport-agnostic `Validator` (asymmetric-algorithm allowlist enforced via `jwt.WithValidMethods` at parse time — HS\* and `alg:none` are structurally impossible, the keyfunc is belt-and-braces with a non-asymmetric-key shape rejection); `Middleware` is the `net/http` decorator (`Authorization: Bearer <jwt>` → identity in `r.Context()` via `identity.With` + scopes via `WithScopes`); the eight typed sentinels (`ErrTokenMissing` / `ErrTokenMalformed` / `ErrAlgNotAllowed` / `ErrSignatureInvalid` / `ErrTokenExpired` / `ErrTokenNotYetValid` / `ErrUnknownKey` / `ErrIdentityClaimMissing`, plus `ErrAudienceMismatch` / `ErrIssuerMismatch`) cover every rejection. The new `CodeAuthRejected` Protocol error code lands in `internal/protocol/errors/` (single-source preserved); `transports.NewMux` gains a `WithValidator` option that wraps both Phase 60 handlers in the middleware (additive — the Phase 60 trust-based posture is preserved verbatim when no validator is supplied). The control handler's `assertBodyMatchesAuthedIdentity` is the defence-in-depth check (a body claiming a different `(tenant, user, session)` than the JWT is rejected 401 before `Dispatch` runs); the SSE handler's `?admin=1` query param is gated on the verified `ScopeAdmin` / `ScopeConsoleFleet` scope (rejected 403 without). The `golang-jwt/jwt/v5` library was promoted from indirect to direct (no new module — already pulled by `aws-sdk-go-v2/credentials`). `test/integration/phase61_auth_test.go` exercises every rejection mode end-to-end against a real ES256-keypair-signed bearer + the real `ControlSurface` + the real `events.EventBus` behind `httptest.Server`; the security suite covers algorithm-confusion, alg:none, scope-escalation, kid-substitution, expired-token, and tampered-body attacks; D-025 concurrent-reuse pinned at N=128 with goroutine-baseline assertion. Coverage: auth 90.1%, errors 100%, transports 94.3%, control 89.5%, stream 86.6% (all ≥ targets).

### 62 — Protocol conformance suite (RFC §5)

**Goal.** A single conformance suite the protocol surface passes; covers every method, every error code, every event filter.
**Acceptance.** `go test ./internal/protocol/conformance/...` exits 0; smoke runs the same suite against `harbor dev`.
**Tests.** The suite itself.
**Deps.** 58, 60, 61.
**Status note.** Shipped at 81.2% statement coverage (master-plan target 85%) per the documented §4.3 deviation in `docs/plans/phase-62-protocol-conformance.md` — matches the precedent set by Phase 49's `internal/planner/conformance` (70.8% under the same target). Conformance-suite coverage is dominated by `t.Fatalf` rollback branches that fire only on assertion failure; the assertion *density* (10 methods × 2 transports; 8 error codes × ≥1 failure path; every event-filter shape; the version handshake; the auth pipeline; an N=100 D-025 stress) is the load-bearing surface. The suite ships paired with `test/integration/wave10_test.go` — the Wave 10 wave-end E2E that consumes the same suite from a different consumer profile against the assembled real-driver Wave 10 surface.

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

### Phase 64 — `harbor dev` v1 (pre-plan scoping note — BINDING when the plan is authored)

Phase 64 is the moment `cmd/harbor/main.go` stops being a driver-registration stub and starts instantiating an LLM-backed runtime for the first time. Before this phase, no production code path resolves the LLM client — every "test stub as default" call (the `mock` LLM driver, `EchoSummarizer`, `staticSummariser`) is dormant. Phase 64 is the moment they go live.

The §13 entry **"Test stubs as production defaults on operator-facing seams"** is pre-settled for this phase. The plan author MUST satisfy the constraints below — they are not re-litigable inside the phase plan:

1. **Default LLM driver is `bifrost`, not `mock`.** Phase 64 flips `llm.DefaultDriver` from `"mock"` to `"bifrost"` (`internal/llm/registry.go:172`) and updates `examples/*.yaml` so `driver: bifrost` is the demonstrated path. The `mock` driver subpackage (`internal/llm/mock/`) moves under a `harbor_testfixtures` build tag (or to a `testfixtures/` subdirectory) so it is unreachable from `cmd/harbor/main.go`'s blank-import block in a normal build. Production tests that need a deterministic LLM consume it via the build-tagged path or via `*_test.go`-local fixtures.

2. **Boot fails loudly when no LLM provider is configured.** Missing API key, missing `bifrost` provider section, or an empty `llm:` block → `harbor dev` prints a one-line error that names the missing config key (e.g. `config.llm.providers[0].api_key: required when driver=bifrost`) and points to `examples/dev.yaml`, then exits non-zero. Silent fallback to the mock is forbidden — this is the §13 "fail loudly at boot" consequence.

3. **LLM-backed defaults for `memory.Summarizer` and `planner.Summariser`.** When `memory.strategy: rolling_summary` is configured and no custom `Summarizer` is injected, Phase 64 (or a same-wave sibling phase) provides a default LLM-backed `Summarizer` that composes an `llm.LLMClient` with a versioned compaction prompt template. Same shape for `planner.Summariser` consumed by `CompressionRunner`. `EchoSummarizer` and `staticSummariser` move to `testfixtures` and are no longer reachable from the production wiring path. If the author chooses to split this into a sibling phase (e.g. Phase 64a), that phase MUST ship in the same wave as Phase 64 — the §13 primitive-with-consumer rule applies recursively: a `harbor dev` that defaults to `rolling_summary` but has no Summarizer wired is the same failure mode one layer down.

4. **Dev-only escape hatch is explicit and banner'd.** A `--mock` flag on `harbor dev` (or `HARBOR_DEV_ALLOW_MOCK=1` env var — Phase 64's plan picks ONE and pins the choice in a `D-NNN` decisions entry) is the ONLY path to the mock LLM at runtime. When the escape hatch fires, every boot prints a stderr banner: `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]`. The README's quickstart MAY use this path but must label it as a dev shortcut, not the production install — `examples/dev.yaml` shows the production-shaped config and the README's "5-minute quickstart" demonstrates the escape-hatch path with a one-line note.

5. **`scripts/smoke/phase-64.sh` exercises the LLM seam, not just `/healthz`.** A smoke that only checks `GET /healthz` is insufficient — the phase exists to wire the LLM, so the smoke MUST exercise the LLM. The script boots `harbor dev` against a recorded bifrost fixture (no live network — use `httptest.Server` or a recorded-cassette pattern), submits one task over the Phase 60 REST handler, and asserts the SSE stream emits a planner Decision derived from a real `LLMClient.Complete` call. A second smoke assertion: boot with no provider configured and assert the non-zero exit with the expected error message.

6. **The §18 mirror invariant applies in spirit.** Phase 64 introduces a binary that real users will run. The README's `## Status` table, `cmd/harbor`'s godoc, and any "Quick start" prose are updated in the same PR — no aspirational claims like "harbor dev boots the Console" that land before the Console-boot phases (72–75) ship. If §3's "Harbor CLI" bullet describes a command that doesn't yet exist, the bullet says so in future tense with a phase reference.

7. **Tool catalog wires Phase 30 (OAuth, D-083) + Phase 31 (approval gates, D-086) primitives from operator config** ([issue #104](https://github.com/hurtener/Harbor/issues/104)). Both phases shipped runtime-side primitives whose only consumers today are tests — `internal/tools/auth.OAuthProvider` and `internal/tools/approval.ApprovalGate` reach the runtime, but the tool catalog (`internal/tools/catalog/`) doesn't know about either. Phase 64 (or a same-wave sibling per the §13 primitive-with-consumer rule) extends the catalog so a tool registration can declare an `ApprovalPolicy` and/or an OAuth `BindingScope` via operator config (`tools.<name>.approval: <policy>`, `tools.<name>.oauth: <provider>` or equivalent shape). The catalog auto-wraps the registered `Tool` with an `ApprovalGate` and/or an OAuth-aware invocation wrapper. Operators get HITL approval AND tool-side OAuth out of the box without writing Go wiring code. The Wave 11 wave-end E2E exercises APPROVE/REJECT via the real `transports/control` HTTP handler (closing the Protocol-wire round-trip half of issue #104); the catalog-wiring half lands in Phase 64. ✅ shipped in Phase 64a / D-090.

**Mandatory reading before authoring this plan** (per §16): RFC §5 (Protocol surface), RFC §6.5 (LLM client), RFC §6.6 (Memory + Summarizer), `docs/research/brief-02-trajectory-compression.md`, `docs/research/brief-04-memory-strategies.md` (or whichever brief indexes summariser design — `docs/research/INDEX.md` resolves), `docs/decisions.md` (D-026 LLM-edge safety, D-035 rolling summary, D-044 latent governance, D-055 trajectory compression rendering rule), the shipped `internal/llm/registry.go` (the default-driver flip site) and `internal/memory/strategy/` (the Summarizer wiring site).

**Pre-assigned decisions slot:** Phase 64's plan claims a `D-NNN` number when dispatched and records: (a) the `mock` → `bifrost` default flip; (b) the chosen escape-hatch mechanism (`--mock` flag vs env var); (c) the LLM-backed default `Summarizer` location (in-package vs new `internal/llm/summarizer/` subpackage); (d) any deliberate carve-out from the §13 entry above (requires an RFC PR — bake the carve-out into the RFC, then reference it here).

**First production consumer of Phase 55's W3C carriers.** Phase 64 is the first production consumer of `telemetry.InjectHTTP` / `telemetry.ExtractHTTP` (the HTTP carrier helpers Phase 55 shipped as standalone functions — see issue [#94](https://github.com/hurtener/Harbor/issues/94)). The plan threads `traceparent` through `tools/drivers/http` on outbound calls and extracts on inbound — `internal/protocol/transports/control` + `tools/drivers/mcp` follow the same shape. This is the §13 primitive-with-consumer obligation closed for the Phase 55 carriers; before Phase 64 they are dormant helpers exercised only by unit tests.

**Departures from this note require an RFC PR.** This note is binding, not advisory — it encodes a Wave 10 audit finding (the §13 amendment above) that future plan-authors do not have visibility into. Treat it as the equivalent weight of an RFC section.

### 65 — `harbor dev` hot-reload (RFC §8)

**Goal.** fsnotify watcher; graceful-drain restart on Go-source change; configurable retain-in-flight policy.
**Acceptance.** File change triggers drain; in-flight runs cancel cleanly; new code picked up.
**Tests.** Integration with file mutation.
**Deps.** 64.

**§4.3 shape decision (D-099).** In-process `bootDevStack` rebuild, NOT binary re-exec. Re-exec was considered and rejected for V1: it requires an out-of-process supervisor (the binary cannot re-exec itself without losing live http.Server connections), it costs a Go build per cycle (~5s on a warm machine — the developer feedback loop is the load-bearing UX here), and an operator iterating on YAML config does NOT need a binary rebuild. The in-process rebuild satisfies the "new code picked up" acceptance for every config / scaffold change; operators changing Go source rebuild + re-launch the binary manually (the same cycle they'd run today without hot-reload). A future opt-in `policy: rebuild` can layer binary-rebuild semantics on without changing the supervisor's shape.

### 66 — `harbor dev` draft-save scaffolding (RFC §8)

**Goal.** Project-local `.harbor/drafts/` scratchpad endpoint; iterate on agent without committing scaffold; "save" promotes to `harbor scaffold`-emitted layout.
**Acceptance.** Draft round-trip: edit → preview run → save → resulting scaffold passes `harbor validate`.
**Tests.** Integration + golden.
**Deps.** 64.
**Status.** Shipped — D-100. `internal/devdraft` package ships the filesystem-backed `Store` + the `http.Handler` mounted at `/v1/dev/drafts/` on the `harbor dev` mux behind the Phase 61 JWT validator. On-disk layout is `<root>/<tenant>/<user>/<session>/<draft_id>/` so concurrent operators sharing the same `.harbor/drafts/` root cannot collide (CLAUDE.md §6 applied to a filesystem-backed store). Five endpoints: `POST /` (create + seed via the Phase 67 scaffold engine), `GET /{id}` (list files + content for the Console editor), `PATCH /{id}/files/{path}` (path-traversal-safe per §7 rule 5), `POST /{id}/preview` (validation-only dry-run via `internal/config.Load`), `POST /{id}/save` (promote to operator-supplied output dir; refuses with `ErrValidationFailed` when the rendered `harbor.yaml` fails the validator), `DELETE /{id}` (idempotent discard). Five SafePayload bus events land per round-trip — `dev.draft.{created,updated,previewed,saved,discarded}` — registered with `internal/events`'s exhaustive registry at init(). `harbortest/devstack/devstack.go::Assemble` mirrors the production wiring per D-094 (always constructs a `DraftStore`; mounts the handler when transports are enabled). `test/integration/phase66_draft_save_test.go` exercises the round-trip through the devstack helper with a real Bearer token, observes the five bus events, exercises path-traversal + missing-bearer failure modes, and runs an N=10 concurrency stress under `-race`. `internal/devdraft/concurrent_test.go` runs the D-025 N=128 concurrent-reuse test against one shared Store. `scripts/smoke/phase-66.sh` drives the round-trip against the live binary; the 404/405/501 → SKIP convention keeps the smoke harmless on builds that pre-date Phase 66. Coverage on `internal/devdraft`: ≥80% (master-plan target 75%).

### 67 — `harbor scaffold` (RFC §8)

**Goal.** Generate a new agent skeleton from a template (default = "minimal-react"). Templates discoverable; output passes `harbor validate`.
**Acceptance.** `harbor scaffold my-agent` creates a buildable project; `harbor validate` returns 0.
**Tests.** Golden output.
**Deps.** 63.

**§4.3 deviation (D-087).** Phase 67 was dispatched in parallel with Phase 68 (`harbor validate`) per CLAUDE.md §17.7 step 3. At scaffold-time, `harbor validate` is still a Phase 63 stub — calling it would exit non-zero with `not_implemented` regardless of the scaffolded config's validity. Phase 67's acceptance criterion is therefore verified against `internal/config.Load + Validate` directly (the shipped subsystem the future `harbor validate` will call), via `cmd/harbor/scaffold/scaffold_test.go::TestScaffold_RenderedConfig_PassesConfigValidate`. The cross-phase CLI integration smoke step (running `harbor validate ./harbor.yaml` after a scaffold, asserting exit 0) lands in Phase 68's PR per §17.6. The §13 primitive-with-consumer rule is satisfied — the consumer-of-the-config-validator is a real shipped subsystem (`internal/config`), not a future CLI surface.

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
>
> **Console-wave deployment + shared-library posture (BINDING — D-091 / D-092 / D-093).** Companion to the page-decomposition note above; this note locks in the *how it's deployed* and *how it's built* answers a future Console plan-author cannot relitigate. Departures from any item below require an RFC PR, not a phase-plan footnote.
>
> 1. **`harbor console` is the Console's deployment surface, not `harbor dev`.** The full Console SvelteKit build is baked into `cmd/harbor` via `embed.FS` and served by a new `cmd/harbor/cmd_console.go` subcommand (a phase to be slotted at re-decomposition time). `harbor dev` (Phase 64, shipped) is and stays headless — embedding the Console into `harbor dev` is rejected (couples developer iteration to operator observability; wrong scope). A future packed dev UI for single-agent development reuses the Console's chat/playground components via a shared library; post-V1. Decision: **D-091**.
> 2. **Svelte 5 + runes mode only.** `web/console/svelte.config.js` ships with `compilerOptions: { runes: true }`; `package.json` pins `"svelte": "^5.0.0"`. Legacy Svelte 4 reactivity (`$:`, top-level `let` as state, `export let` props, store auto-subscription in scripts) is rejected by `svelte-check --fail-on-warnings`. Decision: **D-092**.
> 3. **Protocol TypeScript client is generated, not hand-written.** `cmd/harbor-gen-protocol-ts/` reads `internal/protocol/singlesource.CanonicalWireTypes` and emits `web/console/src/lib/protocol.ts` with a `// CODE GENERATED ... DO NOT EDIT.` header. A `make protocol-ts-gen-check` target asserts `git diff --exit-code` is clean in CI. Hand-rolled `fetch` in `.svelte` files is still rejected (§13). Decision: **D-093**.
> 4. **Stylelint enforces the no-raw-literals rule mechanically.** The first Console phase that creates `web/console/` lands `web/console/.stylelintrc.cjs` that disallows hex / rgb() / named colors and arbitrary `px` / `rem` / `em` outside the token surface (`tokens.css`). `npm run lint` fails CI on raw literals; reviewers no longer hunt for them by eye.
> 5. **Shared chat module — encapsulate first, extract on second consumer.** The chat + playground + MCP-Apps renderer + file-upload + trace-toggle components ship as a self-contained module at `web/console/src/lib/chat/`. The introducing phase enforces: (a) no imports of other Console internals from the chat module; (b) a typed `ProtocolClient` interface the caller injects, not a Console singleton; (c) the MCP-Apps renderer registry lives at `web/console/src/lib/chat/renderers/`. The future packed dev UI extracts to `web/shared/chat/` via `git mv` when its phase plan lands.
> 6. **Mockup inventory is complete for V1 (as of 2026-05-18).** All 13 V1 sidebar pages plus the session-level Playground surface have canonical mockups at `docs/rfc/assets/console-<slug>-page.png` (14 PNGs; Evaluations excluded per D-064). Each `docs/design/console/page-<slug>.md` spec carries a `§12. Mockup-aligned refinements (2026-05-18)` section that reconciles its mockup against §3-§7. Each Console page phase plan MUST reference the canonical mockup for the view(s) it ships AND consume the §12 reconciliation directly — the §12 component table is the binding source for any `[wave-13-extends]` Protocol-surface additions. The superseded legacy `docs/research/console-mockup-runtime-view.png` is retained as a research artifact only; the canonical Live Runtime mockup is `docs/rfc/assets/console-live-runtime-page.png`.
> 7. **§17.7 dispatch-prompt forcing function.** Every Console-wave dispatch prompt MUST name in its mandatory reading list: Brief 11, Brief 12, every `docs/rfc/assets/console-*-page.png` asset (the legacy `docs/research/console-mockup-runtime-view.png` is superseded — agents should not consume it), CLAUDE.md §4.5 + §13 frontend bullets, and the three decisions above (D-091, D-092, D-093). This note is binding, not advisory.
> 8. **Per-page Console specs live at `docs/design/console/page-<slug>.md`.** The 14-page IA is decomposed into one self-contained spec per page (Overview, Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background Jobs, Flows, Memory, MCP Connections, Artifacts, Settings, Playground) — each carries an eleven-section template with a `[shipped]` / `[wave-13-extends]` / `[deferred]` functionality matrix. These specs are the authoritative per-page mockup-authoring source for Wave 13 and MUST appear in every per-page agent's mandatory reading list alongside Brief 11, Brief 12, and the relevant mockup asset. The directory's `README.md` is the index.

### 72 — Console subscription protocol surface (RFC §5.2, §7)

**Goal.** Read-only event subscription scoped by identity triple; admin/console:fleet scope for cross-session/tenant.
**Acceptance.** Console can subscribe to a session's events; cross-tenant call rejected unless scoped admin.
**Tests.** Integration.
**Deps.** 60, 05, 06.
**Plan file.** `docs/plans/phase-72-console-subscription-scope.md` (shipped — D-105).

### 72a — `events.subscribe` filter extensions + `events.aggregate` (RFC §5.2, §6.13)

**Goal.** Extend the `events.subscribe` Protocol surface with a wire `EventFilter` struct (event-type / tenant / user / session / run / time-window) and add a new `events.aggregate` Protocol method returning time-bucketed event-type counts. Both methods use the closed two-scope set (`auth.ScopeAdmin` + `auth.ScopeConsoleFleet`) for cross-tenant fan-in per D-079 — NO new `events.crosstenant` scope.
**Acceptance.** `EventFilter` + `EventBucket` + `EventAggregateRequest` + `EventAggregateResponse` ship in `internal/protocol/types/events.go`; `events.aggregate` route mounted on the wire; cross-tenant requests without the closed-set scope claim return 403 + `CodeIdentityScopeRequired`; bucket arithmetic deterministic (Window % Bucket == 0 or 400); concurrent-reuse pin under `-race` (N≥100).
**Tests.** Unit (filter matrix, aggregate bucket arithmetic, concurrent-reuse) + integration (`test/integration/events_filter_aggregate_test.go` — real bus + real auth + real transports, scope-claim happy + reject paths, concurrent-reuse over the wire) + smoke (`scripts/smoke/phase-72a.sh`).
**Deps.** 60, 61, 72.
**Plan.** See `docs/plans/phase-72a-events-filter-aggregate.md`.

### 72e — `pause.list` snapshot Protocol method (RFC §5.2, §6.3)

**Goal.** Add the `pause.list` Protocol method (route `POST /v1/pause/list`) — a paginated, identity-scope-filtered snapshot of currently-paused tasks / sessions, projected from the shipped Phase 50 Pause/Resume Coordinator's in-memory registry. Read-only: it consumes the Coordinator state, it does not mutate the registry or call `Resume`. It is the snapshot half of the Console intervention-queue contract; live deltas continue to flow through `events.subscribe` on the `pause.requested` / `pause.resumed` topics. The Overview-page intervention queue (Phase 73a) is the UI consumer.
**Acceptance.** `MethodPauseList` + the `PauseSnapshot` / `PauseFilter` / `PauseListRequest` / `PauseListResponse` / `PauseArtifactRef` wire types ship in `internal/protocol/{methods,types}`; the `Coordinator.List` interface extension + `internal/runtime/pauseresume/list.go` implementation; identity-mandatory (401 `CodeIdentityRequired`); cross-tenant filter without `auth.ScopeAdmin` → 403 `CodeIdentityScopeRequired` (D-079 closed-scope reuse, no new scope); the D-026 heavy-content bypass routes oversized pause payloads through the `ArtifactStore` and emits `pause.payload_artifact_routed`; pagination (`PageSize` default 50, max 200, out-of-range → 400, never silently clamped); concurrent-reuse pin under `-race` (N=128).
**Tests.** Unit (`list_test.go` — filter combinations + pagination math + status semantics; `pause_list_handler_test.go` — identity / scope-claim / malformed / heavy-bypass; `list_concurrent_test.go` — D-025 N=128) + integration (`test/integration/pause_list_test.go` — real Coordinator + real transport + real auth, two-tenant scope, cross-tenant reject, admin-claim accept, heavy-payload bypass, concurrency stress, all `-race`) + smoke (`scripts/smoke/phase-72e.sh`).
**Deps.** 50, 60, 61, 17 (all shipped). 73c / 73d for pagination-shape consistency only — same wave.
**Plan.** See `docs/plans/phase-72e-pause-list-snapshot.md` (shipped — D-110).

### 72g — `governance.posture` + `llm.posture` (RFC §5.5, §6.15)

**Goal.** Two read-only posture Protocol methods feeding the Console Settings page (Phase 73m). `governance.posture` returns the D-081 `IdentityTiers` view (per-tier `BudgetCeilingUSD` + token-bucket `RateLimit` + `MaxTokens`) plus `DefaultTier` + the caller-resolved tier. `llm.posture` returns the bound LLM provider/model/region + a `MockMode` boolean — `true` iff the runtime booted with `HARBOR_DEV_ALLOW_MOCK=1` (D-089). The two methods EXTEND the Phase 72f `PostureSurface` (one surface, not two — §13). Both are identity-mandatory; cross-tenant reads require `auth.ScopeAdmin` (D-079). Read-only — no mutation method.
**Acceptance.** `MethodGovernancePosture` / `MethodLLMPosture` registered in `internal/protocol/methods` + folded into `IsPostureMethod`; wire types in `internal/protocol/types/{governance,llm}.go`; the Phase 72f `PostureSurface` dispatcher routes both new methods through the control transport via the same `IsPostureMethod` branch; cross-tenant non-admin → 403 `CodeScopeMismatch`; missing identity → 401; cross-tenant governance/llm admin reads emit a `*.posture_read_admin` audit event; `MockMode` reflects the D-089 boot-time capture; concurrent-reuse pin under `-race` (N≥100).
**Tests.** Unit (posture providers, posture surface, control posture handler, concurrent-reuse) + integration (`test/integration/phase72g_posture_test.go` — real governance + llm + transports + ES256 auth, MockMode round-trip across two boot modes, cross-tenant reject, N≥10 stress) + smoke (`scripts/smoke/phase-72g.sh`).
**Deps.** 36a, 36b, 64, 72f.
**Plan.** See `docs/plans/phase-72g-governance-llm-posture.md` (shipped — D-112).

### 72h — Console DB local schema + SvelteKit scaffold (RFC §7)

**Goal.** Land the Console-local IndexedDB schema (per D-061 — Console-local state ONLY, never a shadow source of truth for runtime entities) AND introduce the `web/console/` SvelteKit scaffold (audit-resolved A5) every Stage-2 Console page rides on. Eight V1 tables: `saved_filters`, `saved_views`, `profiles`, `runtime_registry`, `auth_profiles`, `pat_store`, `notifications_routing`, `keybindings`.
**Acceptance.** `web/console/src/lib/db/` ships as a self-contained TypeScript module behind a `ConsoleDB` driver interface (V1 default driver: IndexedDB); per-operator row scoping is structural (`[operator_id, id]` compound key); `auth_profiles` / `pat_store` blobs are AES-GCM ciphertext with a PBKDF2-derived KEK (`crypto.ts`); forward-only migrations; the §13 / D-061 carve-out is mechanically scanned (`schema-carveout.spec.ts` + smoke); the SvelteKit scaffold pins Svelte 5 runes (D-092) + ships the generated `protocol.ts` stub (D-093).
**Tests.** Vitest unit (`crypto.spec.ts`, `schema.spec.ts`, `schema-carveout.spec.ts`, `migrations.spec.ts`) + in-package integration (`tests/integration.spec.ts` — real IndexedDB driver via `fake-indexeddb`, real WebCrypto, eight-table round-trip, cross-operator isolation, encrypted-blob round-trip, wrong-key fail-loud) + smoke (`scripts/smoke/phase-72h.sh`, static-only).
**Deps.** 60 (Protocol auth for PAT identity scoping).
**Plan.** See `docs/plans/phase-72h-console-db-schema.md`. Decision: D-113.

### 73 — Console state inspection surface (RFC §5.2, §7)

**Status.** `Shipped*` — **dissolved during Wave 13** (D-133). Phase 73 never landed as a standalone phase; its surface was decomposed across the Console page phases that consumed each slice. Shipped: `sessions.inspect` (Phase 73c), `tasks.get` (Phase 73d), `artifacts.list` / `artifacts.put` / `artifacts.get_ref` (Phase 73l, D-120). Deferred post-V1 (no V1 consumer — §13 no-primitive-without-consumer): `state.history`, `state.list_trajectories`, `state.load_planner_checkpoint`, `artifacts.get`, `artifacts.delete` — each lands additively with the first Console surface that consumes it.
**Goal.** `sessions.inspect`, `tasks.get`, `state.history`, `state.list_trajectories`, `state.load_planner_checkpoint`, `artifacts.list`, `artifacts.get`, `artifacts.get_ref`, `artifacts.delete` — all scope-checked, redacted on emit.
**Acceptance.** Each method enforces identity; redaction applied; pagination defined.
**Tests.** Integration + scope mismatch.
**Deps.** 60, 07, 17.
**Cross-reference.** Phase 73l (Console Artifacts page) is the page-side consumer — it extends `artifacts.list`'s filter shape and adds `artifacts.put` + the `artifacts.get_ref` presigned-URL resolver in the same wave (D-120).

### 73l — Console Artifacts page (RFC §5.2, §6.10, §7)

**Goal.** The Console Artifacts page — catalog + preview surface over the runtime's content-addressed artifact store — plus its feeding Protocol additions: the `artifacts.list` filter extensions (mime / source / size / created / tags), the `artifacts.put` upload pipeline (Brief 11 §PG-2), and the `artifacts.get_ref` presigned-URL resolver (D-022 / D-026). Ships the canonical renderer-registry SKELETON at `web/console/src/lib/chat/renderers/` (dispatch table + six MIME renderers) — Phase 73l is the registry's first in-staging consumer; Phase 73n extends it.
**Acceptance.** The three `artifacts.*` methods route through a sibling `ArtifactsSurface`; identity-mandatory + D-079 cross-tenant gating; `artifacts.get_ref` fails loud with `CodePresignUnsupported` on a non-S3 driver; the page dispatches previews through the canonical registry with no bespoke per-mime renderer; mutation surfaces render disabled-with-tooltip. See `docs/plans/phase-73l-console-artifacts-page.md`.
**Tests.** Unit (`internal/protocol/artifacts_test.go`), concurrent-reuse N=100 (D-025), integration (`test/integration/artifacts_page_test.go` — in-mem + SQLite + fs drivers + real wire transport), renderer-registry Vitest, Playwright per-page spec.
**Deps.** 73 (artifacts base methods), 75 (Playwright harness).
**Deviations (D-120).** The surface lands at `internal/protocol/artifacts.go` (the codebase has no `handlers/` sub-package — it follows the `SearchSurface` / `PostureSurface` convention); `web/console/src/lib/protocol.ts` is hand-extended (the `cmd/harbor-gen-protocol-ts` generator binary has not yet landed — Phase 72h committed `protocol.ts` as a hand-shaped stub). Both are recorded in the phase plan.

### 73j — Console Memory page (Protocol + UI) (RFC §5.2, §6.6, §7)

**Goal.** Bundle the Memory-page Protocol surface and UI into one Stage-2.1 phase (Wave 13 decomposition §5). Three read-only Protocol methods land — `memory.list` (paginated, identity-scope-filtered memory records + aggregate counters), `memory.get` (one record's full detail; heavy values routed through `artifacts.get` by reference per D-026), `memory.health` (aggregate counters + per-scope driver mapping). The methods compose over the shipped `MemoryStore.Snapshot` surface (Phases 23–25) + the `events.aggregate` 24h counters (Phase 72a). The UI is the SvelteKit Memory page (`/memory`) — catalog table + right-rail status cards (Memory health / Recent identity rejections / Recovery dropouts / Selected-item detail) + the disabled-with-tooltip bulk-action toolbar (V1 is view-only; the memory mutation surface is deferred to Phase 73 / post-V1). The page IS the consumer (§13 satisfied trivially); it also consumes `memory.identity_rejected` (D-033) + `memory.recovery_dropped` (D-035) events.
**Acceptance.** `MethodMemoryList` / `MethodMemoryGet` / `MethodMemoryHealth` registered in `internal/protocol/methods` + folded into the new `IsMemoryMethod` predicate; wire types in `internal/protocol/types/memory.go`; the three routes (`POST /v1/memory/{list,get,health}`) mounted via `transports.WithMemory`; identity-mandatory (401 `CodeIdentityRequired`); cross-tenant filter without `auth.ScopeAdmin` → 403 `CodeIdentityScopeRequired` — NO new memory scope (audit B1; D-079 closed-set reuse); the D-026 heavy-value bypass routes oversized values through the `ArtifactStore` and `memory.get` ships `ValueArtifact` (never inline bytes); a constructed-driver negative test fails loud with `ErrContextLeak`; concurrent-reuse pin under `-race` (N≥100); the Memory page renders against the mockup with design-token-only styling; per-page Playwright spec `web/console/tests/memory-page.spec.ts`.
**Tests.** Unit (`internal/memory/protocol` — `list_test.go` / `get_test.go` / `health_test.go` / `leak_internal_test.go` / `concurrent_reuse_test.go`; `internal/protocol/transports/stream/memory_handler_test.go`) + integration (`test/integration/memory_page_test.go` — real `MemoryStore` + real transport + real ES256 auth + real artifact store + real events bus; happy path, cross-tenant reject, identity-required fail-loud with the D-033 bus assertion, D-026 heavy-value round-trip, N≥10 two-tenant concurrency stress, all `-race`) + Console-side Vitest (`saved_filters_memory.spec.ts`, `protocol-memory.spec.ts`) + Playwright (`memory-page.spec.ts`) + smoke (`scripts/smoke/phase-73j.sh`).
**Deps.** 23, 24, 25, 60, 61, 72a, 72h, 73 (artifacts.get), 75 (all shipped or same-wave).
**Plan.** See `docs/plans/phase-73j-console-memory-page.md` (shipped — D-118).

### 73i — Console Flows page (Protocol + UI) (RFC §5.2, §6.1, §7)

**Goal.** Ship the Console Flows page as a single Wave 13 Stage-2.1 phase: six NEW `flows.*` Protocol methods (`flows.list` with aggregate metrics, `flows.describe` engine-graph payload, `flows.runs.list`, `flows.runs.describe`, `flows.run`, `flows.metrics`) + the read-only Flows-page UI (catalog table + Flow Metrics card + the shared read-only engine graph canvas + per-flow Budget meter + run-history table + selected-run summary panel) + the per-page Playwright spec. Authoring is OUT of V1 per D-063 — the page is view-only with `flows.run` as the only mutating action, gated on `auth.ScopeAdmin` (D-079).

**Acceptance.** Six method names declared in `internal/protocol/methods/methods.go`; wire types in `internal/protocol/types/flows.go`; all six identity-mandatory + cross-tenant gated on `auth.ScopeAdmin`; `flows.run` gated on the same admin claim and degrades to 403 without it; `flows.runs.describe` ships heavy outputs via `FlowArtifactRef` (D-026); the shared `EngineGraphCanvas` + typed `GraphInput` interface published for Phase 73b; no authoring affordances render (D-063).

**Deviations (D-117).** The `flows.run` mutating gate reuses `auth.ScopeAdmin` (D-079 closed two-scope set — no new scope minted). The runtime side introduces a new `flow.Registry` subsystem as the source-of-truth (registered flows + bounded run-history ring). The typed Console client lives at `web/console/src/lib/flows/client.ts` as the hand-authored mirror of the flows.* surface until `cmd/harbor-gen-protocol-ts` (D-093) is extended to emit it — `protocol.ts` itself is not hand-edited.

**Tests.** Unit (`flow/protocol/*_test.go` — surface + catalog + invoker; `flows_handler_test.go` — identity / scope / decode; `concurrent_reuse_test.go` — D-025 N≥100) + integration (`test/integration/flows_page_test.go` — real registry + real transport + real auth, two-tenant scope, cross-tenant reject, `flows.run` reject without claim, D-026 heavy-output bypass, concurrency stress, all `-race`) + Console Vitest (`format.spec.ts`, `layout.spec.ts`, `client.spec.ts`) + Playwright (`web/console/tests/flows-page.spec.ts`) + smoke (`scripts/smoke/phase-73i.sh`).

**Plan.** See `docs/plans/phase-73i-console-flows-page.md` (shipped — D-117).

### 73g — Console Events page (RFC §5.2, §6.13, §7)

**Goal.** Ship the Console Events page — the runtime event-bus stream as a full-screen, query-driven investigative surface. This is a composition-only page phase: it ships NO new Protocol method. It consumes the shipped `events.subscribe` (`GET /v1/events` SSE table feed — Phase 72), `events.aggregate` (`POST /v1/events/aggregate` sparkline feed — Phase 72a), and `artifacts.get_ref` (heavy-payload `Open artifact` resolver — Phase 73l). The page IS the consumer Phase 72a's primitives waited for (§13 satisfied trivially). The UI is the SvelteKit Events page (`/events`) — faceted filter chips + Console-DB-backed saved-view chips + event-rate sparkline + virtualised event table + right-rail Event Details card + Pause-stream toggle + Export ▾ — built on the D-121 design-system foundation.

**Acceptance.** Route under `(console)/events/` (no `/console/` URL prefix — CONVENTIONS.md §1); the `EventsNamespace` joins the unified `HarborClient`; saved views persist in the shipped `saved_filters` Console DB table scoped to `page='events'` (no new table — D-061); the Pause-stream toggle is a Console-local render gate distinct from the runtime `pause` method; heavy payloads route through `artifacts.get_ref`, never inlined (D-026); cross-tenant `Tenant ▾` gated on the D-079 closed scope set (no `events.crosstenant` minted); four-state `PageState`. See `docs/plans/phase-73g-console-events-page.md`.

**Deviations (D-125).** No new Protocol method (composition-only). The route ships at `web/console/src/routes/(console)/events/` and the page components at `web/console/src/lib/components/events/` — the phase plan (authored before D-121) named `console/events/` and `lib/events/components/`; CONVENTIONS.md §1/§3 (D-121) is the binding cross-cutting authority and yields the corrected paths (CLAUDE.md §15).

**Tests.** Console Vitest (`filters.test.ts`, `sparkline.test.ts`, `export.test.ts`, `taxonomy.test.ts`, `saved_filters_events.spec.ts`, `EventsNamespace` cases in `harbor-client.spec.ts`) + integration (`test/integration/events_page_test.go` — real inmem bus + real SSE/aggregate handlers + real artifacts surface, subscribe filter narrowing, aggregate sparkline correctness, cross-tenant isolation, the truncated-payload `artifacts.get_ref` identity-rejection failure mode, N≥16 concurrent-subscriber stress, all `-race`) + Playwright (`web/console/tests/events-page.spec.ts`) + smoke (`scripts/smoke/phase-73g.sh`).

**Plan.** See `docs/plans/phase-73g-console-events-page.md` (shipped — D-125).

### 73a — Console Overview page (composition-only UI) (RFC §5.2, §6.13, §6.15, §7)

**Goal.** Ship the Console Overview page — the operator's at-a-glance hub and the default route on a fresh attach. This is a composition-only page phase: it ships NO new Protocol method. It composes the SHIPPED `runtime.counters` / `runtime.health` (Phase 72f), `pause.list` (Phase 72e), `events.subscribe` SSE (Phase 60 / 72), and the Phase 54 `approve` / `reject` control verbs into the 4-card counter row + sub-header health-chip strip + cost-rollup card + intervention queue + recent-activity feed + 2×3 Quick Links grid + the `+ New` quick-create menu. The UI is the SvelteKit Overview page (`/overview`) built on the D-121 design-system foundation.

**Acceptance.** Route under `(console)/overview/` (no `/console/` URL prefix — CONVENTIONS.md §1); the `RuntimeNamespace` + `PauseNamespace` join the unified `HarborClient`; the counter sparklines / recent-activity feed / cost rollup fold client-side off the `events.subscribe` cursor (no new Protocol method — page-overview.md §12); the intervention queue's Approve / Reject invoke the SHIPPED Phase 54 control verbs and degrade to disabled-with-tooltip without the admin control-scope claim (D-066 / §13 — no parallel implementation); the Quick Links grid is exactly six tiles with no Evaluations tile (D-064); saved views persist in the shipped `saved_filters` Console DB table scoped to `page='overview'` (no new table — D-061); four-state `PageState` with nested `PageState` per panel. See `docs/plans/phase-73a-console-overview-page.md`.

**Deviations (D-127).** No new Protocol method, no new Go-side surface (composition-only — `internal/` is unchanged). The route ships at `web/console/src/routes/(console)/overview/` — the phase plan (authored before D-121) named `web/console/src/routes/overview/` and the smoke probed `/console/overview`; CONVENTIONS.md §1 (D-121) is the binding cross-cutting authority and yields the corrected unprefixed `(console)`-group paths (CLAUDE.md §15).

**Tests.** Console Vitest (`aggregations.test.ts`, `activity.test.ts`, `cost.test.ts`, `saved_filters_overview.spec.ts`, `RuntimeNamespace` / `PauseNamespace` cases in `harbor-client.spec.ts`) + Playwright (`web/console/tests/overview-page.spec.ts` — depth-bar shell, counter row, scope-gated intervention actions, Quick Links navigation, `+ New` deep-links, the Disconnected PageState) + smoke (`scripts/smoke/phase-73a.sh`). No Go-side integration test — Phase 73a adds no `internal/` seam; the cross-stack integration assurance is the Playwright spec against a live `harbor console` plus the upstream 72e/72f integration tests.

**Plan.** See `docs/plans/phase-73a-console-overview-page.md` (shipped — D-127).

### 73c — Console Sessions page (Protocol + UI) (RFC §5.2, §6.9, §7)

**Goal.** Ship the Console Sessions page as a single Wave 13 Stage-2.1 phase: two NEW `sessions.*` Protocol methods (`sessions.list` — paginated + filtered SessionRegistry projection with the full filter set; `sessions.inspect` — full per-session snapshot) + the SvelteKit Sessions list/detail route + the per-page Playwright spec. Read-only — the bulk Cancel / Pause toolbar actions iterate the shipped per-row control methods (D-072) and render disabled-with-tooltip (D-066). The page IS the first consumer of `sessions.list` (§13 primitive-with-consumer).

**Acceptance.** Two method names declared in `internal/protocol/methods/methods.go`; nine wire types in `internal/protocol/types/sessions.go`; both identity-mandatory + cross-tenant gated on `auth.ScopeAdmin` (D-079); `sessions.list` emits `Truncated bool` not a silent total (D-026); the Sessions-page Identity column renders Phase 72b's `IdentityScope` impersonation triplet; no `Priority` surface (D-065); saved filters Console-DB-local (D-061); the page clears the `CONVENTIONS.md` §5 depth bar.

**Deviations (D-122).** The wire handler lands at `internal/protocol/transports/stream/sessions_handler.go` (the codebase has no `internal/server/` package — the plan's path is stale; the handler follows the Phase 73f / 73i precedent). `sessions.inspect` ships whole, not as an additive extension of a Phase 73 parent method that has not landed. `web/console/src/lib/protocol.ts` is NOT hand-edited — the Sessions wire types live at `web/console/src/lib/sessions/types.ts` with a typed `SessionsProtocol` wrapper over the unified `HarborClient`, following the Phase 73i Flows-page precedent until `cmd/harbor-gen-protocol-ts` (D-093) lands.

**Tests.** Unit (`sessions/protocol/protocol_test.go` — Service filter/cursor/scope; `concurrent_test.go` — D-025 N≥100; `sessions_handler_test.go` — identity / scope / decode) + integration (`test/integration/sessions_page_test.go` — real registry + real transport + real auth, two-tenant scope, cross-tenant reject + audit emit, malformed cursor, N≥10 SSE-subscriber concurrency stress, all `-race`) + Console Vitest (`sessions/tests/format.spec.ts`, `db/tests/saved_filters_sessions.spec.ts`) + Playwright (`web/console/tests/sessions-page.spec.ts`) + smoke (`scripts/smoke/phase-73c.sh`).

**Plan.** See `docs/plans/phase-73c-console-sessions-page.md` (shipped — D-122).

### 74 — Console topology projection events (RFC §5.2, §6.13, §7.1)

**Goal.** `topology.snapshot` Protocol method + `topology.changed` event over the canonical engine-scoped `TopologyProjection` (static graph + live per-edge queue depth); the event emits on engine construction, the method serves on-demand cold-start.
**Acceptance.** A Protocol client renders a topology view from the canonical projection alone (no internal access); identity-mandatory; cross-tenant requires `auth.ScopeAdmin` (D-079). See `docs/plans/phase-74-console-topology.md`.
**Tests.** Unit (`internal/protocol/types`, `internal/runtime/engine`), concurrent-reuse N≥128 (D-025), integration (`test/integration/phase74_topology_test.go` — real engine + real bus + real wire transport).
**Deps.** 05, 09.
**Deviations (D-114).** The `ControlSurface` topology accessor wires via the `WithTopologyAccessor` functional option (not a positional `NewControlSurface` argument — keeps the Phase 54 signature stable); the nil-accessor / engine-less path returns `CodeUnknownMethod` (no `CodeMethodNotSupported` code exists); `harbor dev` hosts no engine-graph so its surface leaves the accessor nil; the decision number is `D-114` (the plan's pre-assigned `D-106` collided with a parallel Wave 13 phase).

### 75 — Console e2e Playwright harness baseline (RFC §7)

**Goal.** Playwright **harness baseline** under `web/console/tests/` — config, fixtures, page-object base class, helpers, the meta-test, and the `frontend-e2e` CI hook. The harness runs against `harbor console` (D-091) — NOT `harbor dev`; the original master-plan wording is corrected per D-091 + Brief 12 (the Console static build is served exclusively by `harbor console`). Per the binding rule: every operator-facing flow shipped in a phase has a matching `.spec.ts`. Wave 13 (`docs/plans/wave-13-decomposition.md` §12 item 7) narrows this phase to **baseline-only**: per-page specs land alongside each Stage-2 page phase (73a–73n); the wave-end aggregator suite is Phase 75a (Stage 3). See D-115.
**Acceptance.** A baseline harness exists at `web/console/tests/` (config + fixtures + page-object base + helpers + meta-test); the `frontend-e2e` CI job runs it and skips gracefully when `web/console/` is absent (directory-missing → SKIP); future Console page phases hook their per-page specs into it.
**Tests.** Playwright meta-test (`harness.spec.ts`) — boots `harbor console`, asserts the index serves + the SvelteKit app hydrates; SKIPs cleanly before the `harbor console` subcommand (Phase 73m) and the SvelteKit scaffold (Phase 72h) land.
**Deps.** 60, 72. (Narrowed from `64, 72, 73` per the Wave 13 decomposition §4 — per-page Protocol additions move into each Stage-2 page phase; 64 is transitively assumed via 60.)

### 75a — Console e2e Playwright wave-end suite (RFC §7)

**Goal.** The Wave 13 wave-end aggregator Playwright suite (`web/console/tests/wave13.spec.ts`) — full IA navigation across all 14 V1 Console pages, scope-claim degradation regression, cross-page identity isolation, saved-view persistence, notification routing end-to-end. Bundled with the final Stage-2 PR per CLAUDE.md §17.5. Includes `test/integration/wave13_test.go` (Go-side wire-type round-trip + cross-page identity isolation + N≥10 concurrent SSE subscriber stress). Enumerates the 14-page IA and asserts a matching `<slug>-page.spec.ts` exists for each — a missing page-spec pair is a build break (operator §12 item 7 binding amendment).
**Acceptance.** Every one of the 14 V1 Console pages has a matching per-page spec; the aggregator walks them all; the page-coverage check (`make wave13-coverage-check`) is green.
**Tests.** `wave13.spec.ts` + `test/integration/wave13_test.go`.
**Deps.** 75, 73a-73n.
**Shipped notes (D-131).** Three things landed beyond the original plan: (1) a §17.6 cross-phase fix of a Phase 73m build-pipeline gap — the `frontend-e2e` CI job now runs `make console-build` before `make build` so `harbor console` embeds the real SvelteKit bundle (it was embedding an empty `consoledist/`); (2) a dev-only runtime-entity fixture seeder (`cmd/harbor/devseed.go`, gated by `HARBOR_DEV_SEED_FIXTURES=1`) so the per-page Playwright specs render real rows — the 25 `SEED_DEPENDENT` per-page skips were un-skipped and pass; (3) six per-page tests (Live Runtime tab content ×2, Playground chat ×3, Events pause-toggle ×1) carry a documented §17.6 deferral skip — they need run-trajectory fixtures (a live `topology.snapshot` / chat history / SSE subscription), a larger seam than registry seeding, tracked as a follow-up.

### 76 — Cross-tenant isolation conformance harness (RFC §4.3)

**Goal.** A master conformance harness asserting cross-tenant + cross-session isolation across StateStore / ArtifactStore / MemoryStore / SkillStore / TaskRegistry / EventBus. 100 sessions × random ops under `-race`.
**Acceptance.** Final invariant: every read's identity matches the caller's identity exactly; CI runs the harness on every PR.
**Tests.** The harness is the test.
**Deps.** 07, 17, 23, 37, 20.
**Risks.** This is the integrity gate. A regression here is a security bug.
**Shipped notes (D-134).** The harness lives at `test/integration/isolation_conformance_test.go` (package `integration_test`; no new top-level directory — AGENTS.md §3 / §17.2). Three shipped tests: `TestE2E_Isolation_ConformanceHarness` (the 100-session randomized soak), `TestE2E_Isolation_CrossScopeReadIsBlind` (targeted positive proof across the cross-session + cross-tenant boundaries), `TestE2E_Isolation_FailClosedOnMissingIdentity` (the §17.3 failure mode — every subsystem rejects an incomplete triple). Soak-window split (D-134): the every-PR default is a fast ~3 s window (100 workers × thousands of op-cycles still catch a leak with overwhelming probability); the master-plan 30 s soak is opt-in via `HARBOR_ISOLATION_SOAK=<go-duration>`, and `-short` forces the fast window. All six subsystems are opened through their production registry factories — no mocks at the seam; SkillStore runs against its only V1 driver, `localdb` SQLite (`:memory:` DSN). The dedicated `isolation` CI job runs the fast window on every PR.

### 77 — Goroutine leak conformance harness (RFC §5 Go conventions)

**Goal.** Harness wrapping every long-lived component asserting `runtime.NumGoroutine` returns to baseline after `Stop()`.
**Acceptance.** All Runtime components pass; CI runs on every PR.
**Tests.** The harness is the test.
**Deps.** 10, 13, 50.
**Shipped.** `test/integration/phase77_goroutine_leak_test.go` ships the table-driven `TestE2E_Phase77_GoroutineLeakConformance` — `leakCases` is a slice of `{name, exercise}` rows, one per long-lived Runtime component (`Engine`, inmem + durable `EventBus`, `sessions.Registry`, inprocess `TaskRegistry`). Each row constructs the real component with real drivers, runs 12 construct → exercise → teardown cycles, and asserts `runtime.NumGoroutine()` returns to baseline via a bounded eventually-poll (deadline + interval, never an instant snapshot — CLAUDE.md §17.4). A warm-up cycle precedes baseline capture; the suite is not `t.Parallel` (`NumGoroutine` is process-global). Passive registries with no background goroutines (`pauseresume.Coordinator`, steering `Registry`/`Inbox`/`RunLoop`) are deliberately not rows — they have no teardown seam to leak from; the Phase 50 dependency is satisfied by the pause primitive being exercised inside the Engine run lifecycle. A dedicated `leak-harness` CI job runs the suite under `-race` on every PR. All five V1 component rows pass on first run — no leaks found. See D-135.

### 78 — Chaos / fault injection harness

**Goal.** Kill mid-run, drop messages, simulate provider quirks, simulate StateStore disconnect, force pause-deserialize failures. Used in integration tests; not on hot path.
**Acceptance.** Each failure mode produces the documented event + recovery path.
**Tests.** Chaos suite.
**Deps.** 76, 77.
**Shipped.** `test/integration/phase78_chaos_fault_injection_test.go` ships the table-driven `TestE2E_Phase78_ChaosFaultInjection` — `chaosCases` is a slice of `{name, inject}` rows, one per master-plan failure mode. Each row wires the real Runtime component through its production factory / constructor (`engine.New`, `events.Open`, `state.Open`, `pauseresume.New`, `retry.Wrap`), injects one fault, and asserts BOTH the documented loud error / event AND the documented recovery path. The five rows: **kill-mid-run** (a run held in-flight by a blocking node is cancelled — asserts the engine's `RunCancelledHandler` seam fires, `FetchByRun` observes `ErrRunCancelled`, `Engine.Stop` tears down cleanly within a bounded deadline, no goroutine leak); **drop-messages** (a tiny-buffered subscription is saturated past the bus's drop-oldest backpressure — asserts the typed `bus.dropped` event carries a non-empty dropped sequence range); **provider-quirks** (a quirk LLM driver returns malformed output, wrapped in the real `retry.Wrap` retry-with-feedback layer with a rejecting `Validator` — asserts the `llm.retry_with_feedback` event fires + the call exhausts with `llm.ErrRetryExhausted`, plus a recovery sub-case that succeeds after one retry); **statestore-disconnect** (a fault-injecting decorator over the real in-mem `StateStore` returns a transport error — asserts the error surfaces loudly out of `Save`/`Load`, then the reconnect recovery path works); **pause-deserialize-failure** (a `PauseRequest` whose trajectory carries a live channel fails `Coordinator.Request` loud with `trajectory.ErrUnserializable` naming a non-empty field path — the D-069 / RFC §3.4 fail-loud contract, never a half-persisted checkpoint, plus a clean-trajectory recovery sub-case). Faults are injected by THIN DECORATORS over the real components (`test/integration/phase78_faults_test.go`) — they decorate, never replace, and live in `*_test.go` files, never registered as a driver default (the §17.3 "real drivers at the seam" pattern with a fault overlay, not the §13 "test stub as production default" anti-pattern — see D-137). Every row asserts the fault is SURFACED loudly; no silent degradation (CLAUDE.md §13). A dedicated `chaos` CI job runs the suite under `-race` on every PR. All five failure-mode rows pass under `-race`. `scripts/smoke/phase-78.sh` (`static-only`) asserts the harness + decorators files exist, declare the conformance test, are table-driven, and the `chaos` CI job is wired. See D-137.

### 79 — Performance benchmarks

**Goal.** Engine throughput (envelopes/sec under N runs); bus fan-out (subscribers vs latency); memory-strategy latency (truncation vs rolling_summary).
**Acceptance.** Baseline numbers committed; perf regression threshold gates PRs (e.g. > 10% slowdown blocks).
**Tests.** `go test -bench`.
**Deps.** 10, 12, 05.
**Status.** Shipped (D-136 — `test/benchmarks/` suite over engine / bus / memory against real components; `docs/perf/baseline.txt` committed; `scripts/perf/check-regression.sh` `benchstat` gate wired into CI as the `perf-regression` job — fails on a statistically-significant slowdown past a noise-tolerant 30% threshold, an empirical calibration of the master plan's illustrative "10%"; `make bench` / `make bench-check`; phase plan `phase-79-performance-benchmarks.md`).

### 80 — Documentation hygiene polish

**Goal.** Every package has a doc comment; every exported symbol has godoc; example agents in `examples/`; recipe docs (`docs/recipes/`).
**Acceptance.** `golangci-lint`'s `revive exported` and `package-comments` clean; `examples/` builds end-to-end.
**Tests.** Lint + example builds in CI.
**Deps.** All V1 phases.
**Status.** Shipped (D-138 — the `revive` `exported` / `package-comments` documentation lint gate is now ENFORCED in CI: the `lint` job installs `golangci-lint v1.64.8` and runs `make lint-revive`, which uses the dedicated `.golangci-revive.yml` config — previously `make lint` silently skipped because the binary was never installed. The `exported` rule keeps godoc-presence enforcement but gains `disableStutteringCheck` so the ~20 cross-package type renames the stutter sub-check would force stay out of a docs phase; the genuine doc gaps the rule surfaced — eight detached package comments, two malformed package comments, a handful of un-commented `const`/`var` blocks — are all fixed. `examples/` gains worked, buildable code — `examples/agents/echo/` (a `harbortest.Agent` + test) and `examples/tools/weather/` (an `inproc.RegisterFunc` tool + register→resolve→invoke test) — exercised by a new CI `examples` job. `docs/recipes/` ships five real-API-grounded how-to guides. The broader `make lint` backlog (~1000 issues across ~20 linters, accumulated while the gate silently skipped) is deliberately left to a separate release-hardening effort. Phase plan `phase-80-documentation-hygiene-polish.md`).

### 81 — Release engineering (versioning, changelog) (RFC §12)

**Goal.** Semver tagging, `CHANGELOG.md`, build provenance (SLSA-style attestations as a stretch).
**Acceptance.** `git tag v1.0.0-rc.1` produces a release artifact; CHANGELOG covers all V1 phases.
**Tests.** Release dry-run.
**Deps.** All V1 phases.
**Status.** Shipped (D-139 — the product release version is stamped into the `harbor` binary at link time: `cmd/harbor.HarborVersion` becomes a `var` (a `const` cannot be `-ldflags -X` overridden), and `scripts/release-build.sh` — the single home of the build incantation — stamps it via `go build -ldflags="-s -w -X 'main.HarborVersion=…'"` from a `git describe --tags`-derived version, falling back to the `v0.0.0-dev` sentinel for an un-tagged build. The product release version is kept STRICTLY distinct from the Harbor Protocol version (`internal/protocol/types.ProtocolVersion`, RFC §5.3) — `harbor version` already prints both as separate fields; the two are versioned independently. `CHANGELOG.md` lands at the repo root in Keep-a-Changelog format, grouped by delivery wave / subsystem, covering every V1 phase (01–81 + the lettered phases). `.github/workflows/release.yml` triggers on a `v*` tag push — builds the CGo-free static binary, emits a SHA-256 checksum, attaches SLSA-style build provenance via GitHub's native `actions/attest-build-provenance` (the master-plan stretch — landed, not deferred, because the first-party action adds no framework dependency), and publishes a GitHub Release; a `workflow_dispatch` path runs the dry-run. `scripts/release-dryrun.sh` (the `make release-dryrun` target) is the master-plan "release dry-run" test — it exercises the exact release-build path with a synthetic version and asserts the artifact + checksum + version stamp, all without pushing a tag. Phase 81 creates NO `v*` tag — tagging is the operator's job in Phase 82. Phase plan `phase-81-release-engineering.md`.)

### 82 — V1 cut (RFC §1, §12)

**Goal.** `v1.0.0` tag; release notes; migration notes (if any); blog/announcement scaffold.
**Acceptance.** `harbor version` returns `v1.0.0`; preflight green; protocol conformance suite green; cross-tenant + leak harnesses green.
**Tests.** Full preflight.
**Deps.** 81.

### Post-V1 follow-ups (83–90)

Listed for tracking. Not on the V1 critical path.

- **83 — Auto-sequence detection.** Skip the LLM call on deterministic single-tool transitions. Off by default. RFC §12. Deps: 45.
- **83a — ReAct prompt structured sections.** Refactor `defaultBuilder` to assemble the twelve XML-tagged sections from brief 13 §2.1 (`<identity>`, `<output_format>`, `<action_schema>`, `<finishing>`, `<tool_usage>`, `<parallel_execution>`, `<reasoning>`, `<tone>`, `<error_handling>`, `<available_tools>`, `<additional_guidance>`, `<planning_constraints>`); add `WithSystemPromptExtra` Option + `PlannerConfig.ExtraGuidance` config key; golden-fixture the default prompt. **Foundation phase** — 83b/c/d build on its section anchors. RFC §6.2. Deps: 45. See `docs/plans/phase-83a-react-prompt-structured-sections.md`.
- **83b — ReAct tool schema injection (catalog rendering).** Extend `tools.Tool` with `Examples []ToolExample` (tag-ranked `minimal > common > edge-case`); upgrade `<available_tools>` rendering to emit `args_schema`, `side_effects`, and curated examples per tool. Closes the args-validation-failure cascade caused by name+description-only catalog rendering. RFC §6.2, §6.4. Deps: 83a, 26. See `docs/plans/phase-83b-react-tool-schema-injection.md`.
- **83c — ReAct dynamic repair guidance + planning hints.** Add per-run `RepairCounters{FinishRepair, ArgsRepair, MultiAction}` on `RunContext`; render escalating `reminder → warning → critical` hints per turn when counters trip; wire `RunContext.PlanningHints` into `<planning_constraints>`. Closes the across-step feedback loop Phase 44 (per-step repair) leaves open. New decisions entry **D-145** scopes counters to `RunContext` (not the planner struct) per D-025 concurrent-reuse contract. RFC §6.2. Deps: 83a, 44, 05. See `docs/plans/phase-83c-react-dynamic-repair-guidance.md`.
- **83d — ReAct skills + memory injection (UNTRUSTED framing).** Render `RunContext.MemoryBlocks` and `RunContext.SkillsContext` into the system prompt as separate `llm.ChatMessage` entries with the five-line anti-prompt-injection rule list from brief 13 §2.3. Distinct `<read_only_external_memory>` / `<read_only_conversation_memory>` wrappers preserved per tier; `<skills_context>` for pre-retrieved skill bodies. Serialisation failures fail loudly via `ErrMemoryBlockUnserializable`. RFC §6.2, §6.6, §6.7. Deps: 83a, 23, 37. See `docs/plans/phase-83d-react-skills-and-memory-injection.md`.
- **83e — ReAct reasoning channel decoupling (capture-vs-replay).** Drop `Reasoning` from `Decision_CallTool`; extend `llm.CompleteResponse` with `Reasoning string`; bifrost driver reads `BifrostChatResponse.Choices[0].Message.ReasoningDetails` — closing both the unary-path gap (today `OnReasoning` is streaming-only) and the Gemini-direct black hole (today bifrost populates `reasoning_details[]` on the message but Harbor drops it). Reasoning persists on `TrajectoryStep.ReasoningTrace`; replay is operator-controlled per agent via `PlannerConfig.ReasoningReplay` enum (`never` default for ALL models, `text` opt-in). No `provider_native` mode in V1 (Bifrost docs don't cover thinking-block round-trips). New decisions **D-147** (schema narrowing) + **D-148** (replay knob shape — two enum values, defer `provider_native`). RFC §6.2, §6.5. Deps: 45, 32, 33, 44. See `docs/plans/phase-83e-react-reasoning-channel-decoupling.md`.
- **83f — Dev RunLoop populates the 83-band RunContext (D-149).** Closes the Wave 15 §17.5 audit's W3/W4 (issue #208). `cmd/harbor/cmd_dev_runloop.go::runOne` now fetches the task's `Query`, session-scoped memory via `MemoryStore.GetLLMContext`, session-scoped skills via `SkillStore.Search`, allocates a per-run `*RepairCounters`, and projects operator-supplied `planner.PlanningHints` from the new `planner.skills_context_max` + `planner.planning_hints` YAML keys. Memory/skills fetch errors fail loud with `MarkFailed(code=runtime_fetch_error)`; the LLM is never called on a degraded run. RFC §6.2, §6.6, §6.7. Deps: 83c, 83d, 23, 37, 20. See `docs/plans/phase-83f-react-prompt-band-runtime-consumers.md`.
- **83g — MCP southbound consumer in `harbor dev` (D-150).** Closes the parallel consumer gap for the Phase 28 MCP driver, surfaced during the 83f operator validation. `cmd/harbor/cmd_dev.go::bootDevStack` now iterates `cfg.Tools.MCPServers[]`, spawns each via `mcpdrv.New` + `Connect`, discovers tools via `Discover`, and registers each `ToolDescriptor` on the tool catalog. Boot fails loud (`mcp[<name>]: <stage>: <err>`) on any connect / discover / register failure; the operator sees the error before the binary starts serving. The MCP Registry is constructed and populated so a small follow-up phase mounts the Console MCP-page surface without re-spawning. Devstack mirror per D-094; integration test spawns a real stdio subprocess via the `cmd/harbor-mcptest-stdio` test fixture. RFC §6.4. Deps: 28, 26. See `docs/plans/phase-83g-mcp-dev-consumer.md`.
- **83h — Dev-binary fixes (D-151).** Two hard-block bugs from the v1.1 operator validation: V1 — `cmd/harbor/cmd_dev_hot_reload.go::shouldTrigger` reboot-looped the binary every ~700ms on SQLite WAL/SHM/journal sidecars (fixed: extend with a `dbSidecarSuffixes` ignore list). V2 — `internal/llm/safety.go` rejected every real-bifrost request with `CompleteRequest.Model is empty` because the react planner never sets `Model` (fixed: default `req.Model = c.cfg.Model` before structural validation). The mock LLM driver used in every existing integration test does not enforce `Model`, which is why the gap escaped Wave 13/14/15 audits. Together unblock real-bifrost dev-binary runs. RFC §6.5, §8. Deps: 83g, 64, 32. See `docs/plans/phase-83h-dev-binary-fixes.md`.
- **83i — RunContext wiring closure (D-152).** Closes the four root causes of the Wave 17 operator-validation "64 steps, 0 tool calls" failure mode: (1) the steering RunLoop's `default:` case dropped every `CallTool` decision — fixed with a new `steering.ToolExecutor` seam + `RunSpec.ToolExecutor` field + trajectory-append on every dispatched step; (2) `RunContext.Catalog` was never populated — fixed with `runtimeCatalogView` projecting the production `tools.ToolCatalog` through a per-run identity filter; (3) heavy tool results (1.5 MB MCP responses) leaked verbatim into the LLM prompt — fixed with D-026-shaped artifact-store promotion in the dev binary's `devToolExecutor` (results above `cfg.Artifacts.HeavyOutputThresholdBytes` get stored + a small summary `{tool, size_bytes, truncated, preview, artifact_ref}` is rendered into the LLM observation); (4) `MemoryStore.AddTurn` had no production caller — fixed in `runOne` on `FinishGoal`. The runOne also populates `RunContext.Emit` so the planner's `planner.decision` / `planner.finish` / `planner.repair_guidance_injected` events reach the bus. Live validation: 2 LLM calls end-to-end against `mcp-youtube`. Devstack mirror per D-094. RFC §6.2, §6.6, §6.8. Deps: 83f, 83g, 83h, 26, 23. See `docs/plans/phase-83i-runcontext-wiring.md`.
- **83n — `harbor init` + tiered yaml + docs/CONFIG.md + built-in tools (D-153).** Introduces `harbor init` — the operator-facing entry point that drops a tiered, commented `harbor.yaml` plus `AGENTS.md` / `CLAUDE.md` / `README.md` companion files into a fresh directory. The yaml has three tiers: REQUIRED (identity + four commented LLM-provider examples — OpenRouter, Anthropic, OpenAI, NVIDIA NIM — all reachable through bifrost), COMMON KNOBS (memory, planner, tools, skills, governance) all commented with sensible defaults, and ADVANCED with a pointer to `docs/CONFIG.md`. Companion files explain the workflow (init → validate → scaffold → dev). Ships `docs/CONFIG.md` — the full operator-facing knob reference with one `### <yaml.path>` heading per leaf field on `Config{}` — plus a Go test (`internal/config/doc_drift_test.go`) that fails CI when a new config field lands without documentation. Also ships the first two opt-in built-in tools at `internal/tools/builtin/`: `clock.now` (current UTC time) and `text.echo` (echo input verbatim). The new `tools.built_in []string` yaml field registers built-ins by name; the validator mirrors `builtin.KnownNames()` per the §4.4 seam pattern. `bootDevStack` calls `builtin.Register(toolCat, cfg.Tools.BuiltIn)` between catalog construction and the catalog-wiring step; devstack mirrors per D-094. RFC §8, §6.4. Deps: 67, 63, 26. See `docs/plans/phase-83n-harbor-init.md`.
- **83u — Console DB chicken-and-egg fix (D-163).** Closes round-2 walkthrough F3: the Connected Runtimes add-form on Settings called `console_db::addRuntime` → `runtimes.upsert(...)` on a DB that required an active `RuntimeConnection` to derive its per-operator AES key. Operator without a Runtime → no connection → DB stays closed → form threw "Console DB not open — attach to a Runtime first". The form was reachable (Phase 83p) but structurally non-functional. Fix: new `attachConnection(baseURL, opts)` helper in `web/console/src/lib/connection.ts` writes the `harbor.runtime.*` localStorage keys first (the operator's primary intent — "make the Console talk to this Runtime"); `console_db.svelte.ts::addRuntime` calls `attachConnection()` first, then attempts the DB upsert and degrades to a non-fatal warning if the DB is still locked. On the post-attach page reload, a new private `#catchUpAddressBook()` invoked from `load()` inserts the active connection into the address book if it's not already there (`is_default: 1`). Playwright test follows the disconnected-boot → form → reload → connected flow end-to-end. RFC §5, §7. Deps: 73m, 73p, 83p. See `docs/plans/phase-83u-console-db-chicken-and-egg.md`.
- **83v — Runtime CORS allowlist (D-162).** Closes round-2 walkthrough F4 — the showstopper that broke the D-091 multi-process Console+Runtime posture at the wire. The pre-83v `grep -rn 'Access-Control\|cors' --include='*.go'` returned zero matches anywhere in the Go codebase; cross-origin requests from a Console (`:18790`) to a remote Runtime (`:18080`) were blocked at preflight. Fix: new `internal/protocol/transports/cors/` package with `Wrap(next http.Handler, cfg Config) http.Handler` middleware; new `ServerConfig.AllowedOrigins []string` + `ServerConfig.CORSDevAllowAny bool` yaml fields. Default deny (empty list = no CORS headers = same-origin only). Per-origin echo of the request's `Origin` header after exact-match allowlist check (never `*` in production). `Access-Control-Allow-Credentials: true` (incompatible with `*` per CORS spec, which forces the per-origin shape). Validator rejects `*` in the allowlist unless `server.cors_dev_allow_any: true` is also set; when the dev flag fires, every boot prints a stderr banner `[DEV-ONLY CORS WILDCARD — DO NOT USE IN PRODUCTION]`. Middleware wraps both REST + SSE handlers in `cmd/harbor/cmd_dev.go::bootDevStack`; devstack mirror per D-094. Integration test exercises cross-origin preflight end-to-end against an httptest origin. `docs/CONFIG.md` documents both fields with the production-security note. RFC §5, §7. Deps: 60. See `docs/plans/phase-83v-runtime-cors.md`.
- **83w — Wire-surface gaps (D-164).** Closes round-2 walkthrough F5 + F6 — two wire-surface gaps surfacing as scary red ERROR PageStates on the operator's most-used debugging surfaces. F5 (Console side): `topology.snapshot` returns `unknown_method` on a planner/RunLoop runtime (no engine graph); the Live Runtime + Playground pages routed the error through PageState's red ERROR branch with a Retry button that always failed. Fix: new `'info'` branch added to PageState's `PageStatus` union (additive to disconnected/loading/error/empty/ready); new `isUnknownMethod(err)` helper in `web/console/src/lib/protocol/errors.ts`; both pages special-case `unknown_method` → route to PageState's info branch with "Topology view not available on this Runtime — planner/RunLoop runtime, not engine-graph" copy + no Retry button (retry is meaningless for not-applicable surfaces). F6 (Go side): `mcp.servers.list` was missing from the Runtime's wire surface even though the `*mcp.Registry` was already constructed at boot (Phase 83g) and Tools page rendered six youtube tools fine — just no method handler. Fix is wiring-only: `cmd/harbor/cmd_dev.go::bootDevStack` constructs the Phase 73k `MCPSurface` from the boot-time registry + threads it into `transports.NewMux` via `transports.WithMCPSurface(mcpSurface)`; new `mcpconsole.NoOAuthAccessor` provides the read-only access pattern for V1 `harbor dev` (no OAuth providers); OAuth-flow methods fail loudly with `ErrNoOAuthConfigured` per §13. Devstack mirror per D-094. Integration test asserts the wire surface returns 200 (not unknown_method). RFC §5, §6.4, §7. Deps: 83g, 83m, 73k. See `docs/plans/phase-83w-wire-surface-gaps.md`.
- **83x — Real-data layout polish (D-165).** Closes round-2 walkthrough W4-W11 + N11-N14 — the "every page has a paper cut" backdrop. Twelve items spanning the Console (memory key ellipsis W4; artifacts grid layout 1fr × right-rail W5; tasks kanban Complete column W7; events empty-state names `events.driver: durable` W9; live-runtime session status derived from strip aggregate W10; agents synthetic-default copy W11; overview "(now)" suffix N11; tools "In-flight (now)" relabel N12; tools reliability column width token N13; live-runtime pillar labels "(now)" N14) plus two cross-stack Go-side fixes (§17.6): W6 — `Artifact.created_at` was the Go zero value `0001-01-01T00:00:00Z` because two call sites populating the storage Source map silently omitted the timestamp — fixed at `cmd/harbor/cmd_dev_executor.go::projectForLLM` (heavy-tool promotion, `time.Now().UTC()`) + `internal/protocol/artifacts.go::handlePut` (artifacts.put upload, `s.clock()` so unit tests with injected clock stay deterministic). W8 — SessionRegistry held zero rows under the dev token so the Console Sessions page rendered "No sessions match these filters" even mid-task; fixed by `bootDevStack` opening the dev session right after constructing the registry, swallowing `ErrSessionAlreadyOpen` for idempotency. RFC §5, §6.4, §6.6, §6.10, §6.13. Deps: 73m, 73p, 83i, 83m. See `docs/plans/phase-83x-real-data-layout-bugs.md`.
- **83s — Saved-views label + per-page footer dedup (D-161).** Closes Nits N2 + N7 from the post-83k visual walkthrough. Settles on the canonical pair `"Save view"` (button) + `"Save current as…"` (input placeholder) across every page that surfaces a saved-view save gesture (eight pre-83s phrasings drifted to two). Removes every per-page inline `Disconnected · no Runtime attached` indicator — the viewport-fixed `ConnectionFooter` is the single source of truth; pages now show ONE disconnected indicator per viewport instead of two stacked ones. RFC §5. Deps: 73m, 73p. See `docs/plans/phase-83s-savedviews-and-footer-dedup.md`.
- **83r — Disconnected-state hygiene (D-160).** Closes Walkthrough Bugs W1 + W2 + W3 + Nits N4 + N5 + N8 + N9 + N10. New `isDisconnected()` predicate + `DISCONNECTED_TOOLTIP` constant in `web/console/src/lib/connection.ts` — every page composes it via `$derived(connection === null)` locally. Action buttons + filter controls reach the same predicate (disabled + tooltip when disconnected; W2/W3). The Overview Cost Rollup card stops rendering synthetic `$0.00` data when disconnected (W1). The Tools page renders ONE empty-state message instead of two (N5). Agents KPIs use `—` matching Tools (N4). MCP Connections status chips desaturate via a new `desaturated` prop that flips `data-kind` to `neutral` (N8). Artifacts subtitle reads "— no Runtime attached" when disconnected (N9). `<PageState>` adds vertical-centring CSS (`min-height: 40vh`) so empty-state placeholders centre in the viewport instead of hugging the top (N10). New `web/console/tests/disconnected-state.spec.ts` Playwright spec covers the cluster. Bundled §17.6 fix: pre-83r ESLint break in `settings/+page.svelte:94` (Phase 83p placeholder) — fixed inline. RFC §5. Deps: 73m, 73p, 83p. See `docs/plans/phase-83r-disconnected-state-hygiene.md`.
- **83q — Playground sidebar nav + breadcrumb (D-159).** Closes Bug F2 + Nit N1 from the post-83k visual walkthrough. The Console's `(console)/+layout.svelte` defines the `NAV` constant (cluster → items) AND derives the breadcrumb's `crumbLabel` from the same NAV by URL-segment match — so adding `{ label: 'Playground', href: '/playground' }` to the EXECUTION cluster fixes F2 (Playground unreachable from sidebar) AND N1 (lowercase `playground` breadcrumb) in one stroke. Also rewrites `docs/design/console/CONVENTIONS.md` §2 which explicitly declared "Playground is NOT a sidebar entry" (a stale Phase 73n design call). Playwright tests bump cardinality from ≥13 to ≥14 sidebar links + explicit Playground assertion. RFC §5, §7. Deps: 73n. See `docs/plans/phase-83q-playground-sidebar-nav.md`.
- **83p — Settings two-group layout (D-158).** Closes Bug F1 from the post-83k visual walkthrough: the Settings page wrapped its WHOLE cards loop in `<PageState>`, so the disconnected state short-circuited every section to the "Not connected — Attach one in Settings" placeholder — hiding the Connected Runtimes add-form an operator needs to fix the disconnection. `SettingsState.load()`'s docstring already documented the intended split ("Console-local sections do NOT depend on the runtime posture"); the template ignored it. Fix: each `SETTINGS_SECTIONS` entry now carries a `group: 'console-local' | 'runtime-posture'` discriminator; the page template renders console-local sections (Connected Runtimes, Per-Runtime Auth, API Tokens, Appearance, Time & Locale, Keybindings, Notifications Routing) unconditionally and routes only runtime-posture sections (Runtime Info, Governance Posture, Storage Drivers, LLM-Provider Posture, About) through `<PageState>`. Playwright test extension asserts the add-form is reachable + the input fields render in the disconnected state. RFC §5, §8. Deps: 73m, 73p. See `docs/plans/phase-83p-settings-add-runtime-form.md`.
- **83k — Console release embed (D-157).** Closes the operator-validation gap where `cmd/harbor/consoledist/*` is gitignored (only `.gitkeep` committed) — a fresh `git clone` + `go build ./cmd/harbor` (or `go install github.com/.../cmd/harbor@latest`) produces a binary that embeds an EMPTY Console bundle and serves the synthesized placeholder page. Fix: (1) `make build` now runs `make console-build` as a prerequisite (operators who `git clone && make build` get a working binary the first time); (2) a new `make build-fast` preserves today's no-Console-rebuild path for iterative dev work; (3) `scripts/release-build.sh` rebuilds the Console before `go build`, so tagged-release artifacts always carry a fresh Console; (4) a new `scripts/check-console-bundle.sh` staleness gate (wired into the CI `frontend-e2e` job) asserts two consecutive `make console-build` runs produce byte-identical outputs (catches non-determinism in the build); (5) the placeholder page copy is reworded with the exact rebuild commands + a `go install` workaround + a pointer to `harbor init` for first-time operators + a link to `docs/CONFIG.md`. The "first-run reach" polish half (favicon, brand tokens, empty-state copy) is intentionally deferred to a post-walkthrough follow-up — shipping release-embed first means the visual walkthrough runs against the fixed binary. RFC §5, §8. Deps: 73m, 81, 83n. See `docs/plans/phase-83k-console-release-embed.md`.
- **83m — WARN cleanup band (D-156).** Closes the eight WARN-tier items the §17.5 audit + Wave 17 operator validation surfaced. Item 1: MCP `Config.DefaultIdentity` reuse across pushes is now a fallback — the driver prefers `identity.From(ctx)` per call (multi-isolation footgun closed). Item 2: hot-reload watcher's `dbSidecarSuffixes` extended with `.sqlite` + `.db` main files (the reboot-loop was slower without it, not absent). Item 3: `bootDevStack` appends `agentRegistry.Close` + `draftStore.Close` to the closer chain (goroutine + file-handle leak on shutdown closed). Item 4: `extractSkillKeywords` helper drops English stopwords + 1-char tokens + dedupes before `skills.Search` (FTS5 ranker now sees keyword-shaped queries, not full sentences). Item 5: `internal/llm/safety.go::Complete` prefers `c.cfg.Timeout` over the 5-minute default (operator's `harbor.yaml` timeout was being silently ignored). Item 6: new `tools.granted_scopes []string` yaml field replaces the runloop's hard-coded `nil` pass to `newRuntimeCatalogView` (catalog filter now actually applies operator-declared scopes). Item 7: `tasks.Task.ToolCount` field + `TaskRegistry.IncrementToolCount(ctx, id) error` method (with conformance + N=128 D-025 concurrent-reuse test) + `projectRow` projection + runloop wiring closes the dead `prototypes.Task.ToolCount` wire field (Console rendered 0 forever before). Item 8: `RunContext.OnReasoning func(string)` callback (option b design — keeps the Decision sum sealed, treats reasoning as per-step observation rather than per-step instruction) + runloop's per-step closure capture + `Step.ReasoningTrace` copy on trajectory append makes Phase 83e's `ReasoningReplay=text` mode structurally effective in production for the first time. Shipped via two parallel worktree-agent buckets per §17.7 cadence + coordinator integration. Devstack mirror per D-094 for items 1, 3, 4, 6, 7, 8. RFC §6.2, §6.4, §6.5, §6.8, §8. Deps: 83g, 83h, 83i, 83l. See `docs/plans/phase-83m-warn-cleanup.md`.
- **83l — Real-bifrost integration tests + production-bug fix (D-155).** Closes the audit lesson D-151 named verbatim: every existing dev-binary integration test used the mock LLM, which masked two real-bifrost+real-stack bugs through Wave 13/14/15. Ships `test/integration/phase83l_real_bifrost_test.go` — two tests + a scripted OpenAI-compatible `httptest.NewServer` helper (`scriptedLLMServer`) — exercising the full `cmd/harbor`-shape stack (bifrost driver, safety/correction/retry wrapper chain, react planner, steering RunLoop, ToolExecutor seam, trajectory append, memory writeback). The first run of the tests immediately surfaced a latent production bug: `cmd/harbor/cmd_dev.go::bootDevStack` constructed the `llm.ConfigSnapshot` WITHOUT `cfg.LLM.CustomProviders`, `cfg.LLM.NetworkDefaults`, or `cfg.LLM.Corrections` — an operator who declared a custom provider (NIM / vLLM / ollama / in-house gateway) would pass config validation but fail at boot with `bifrost: invalid provider … declared custom: (none)`. Fix lands in this PR per §17.6 (three new projection helpers + the snapshot wiring; D-094 mirror in `harbortest/devstack/devstack.go`). Exactly the failure mode D-151 predicted; the mock LLM hid it; the integration test caught it on first contact. RFC §6.5. Deps: 33, 33a, 45, 83h, 83i. See `docs/plans/phase-83l-real-bifrost-tests.md`.
- **83o — scaffold reads operator yaml + per-custom-tool Go stubs + `--patch` (D-154).** Closes the operator workflow Phase 83n opened. `harbor scaffold` now reads the operator-edited `harbor.yaml` (explicit `--from-config <path>` or auto-detected `./harbor.yaml`), copies it verbatim into the output project (operator's comments + uncommented LLM block survive), and fans out one `tools/<name>.go` + matching `_test.go` per entry under a new `tools.custom` yaml field. Each custom tool stub carries typed Input/Output structs derived from the operator's flat `field: type` declarations (`string` / `integer` / `number` / `boolean` / `[]string`) plus a `TODO: implement` Handle. The generated `agent.go` includes a `RegisterTools(cat tools.ToolCatalog) error` function that registers each custom tool's Handle so the operator's binary bootstrap is one call. **As-built correction (v1.13.1):** it originally ALSO registered each built-in (from `tools.built_in`), which made any `--with-server` scaffold declaring a built-in unbootable — the runtime registers `tools.built_in` itself at boot (with the backing SkillStore / ArtifactStore / Bus / Redactor), so the second registration hit `ErrToolDuplicateName` (`duplicate tool name: clock.now`). Built-ins are config-driven and the runtime owns them; the generated registrar carries this module's COMPILED tools only, and the yaml entry is the whole opt-in. The generated `go.mod` likewise now pins the scaffolding binary's own release (was the unresolvable `v0.0.0-dev`). Gate: `scripts/smoke/phase-160.sh` declares a built-in alongside the custom tool and BOOTS the scaffolded binary. See the corrections on D-154 / D-267 + the as-built sections of `phase-83o-scaffold-from-yaml.md`, `phase-133-scaffold-tools-execution.md`, `phase-160-sdk-server-scaffold-parity.md`. A new `--patch` flag relaxes the refuse-overwrite default: existing files are skipped (Skipped slice on Result), only new tools materialise. The validator rejects name collisions between `tools.custom` and `tools.built_in` and rejects unknown shorthand types. `docs/CONFIG.md` documents `tools.custom`; the doc-drift gate caught the missing heading mid-implementation. RFC §8, §6.4. Deps: 67, 83n, 26. See `docs/plans/phase-83o-scaffold-from-yaml.md`.
- **84 — Reflection / critique loop.** Optional per planner. Self-critique before Finish. RFC §12. Deps: 45.
- **84b — Multimodal attachment disposition policy (D-189).** Turns the hardcoded MIME→disposition `switch` in `materializeOne` into declared policy: an `AttachmentDisposition` enum (`ref` / `inline` / `provider_native` / `tool:<name>`) resolved per-attachment caller hint > per-agent policy map > runtime default (`ref`) — the layers are semantic, the carriers (Protocol hint, `harbor.yaml`) are thin adapters over a planner-homed policy core (`DispositionPolicy` + the exported pure `ResolveDisposition`), so a headless library consumer authors the same policy directly. The default is byte-for-byte unchanged from today — so the existing developer-controllable `ArtifactStub`+`Fetch.Tool` path stays first-class for Playground, Protocol, and third-party clients, and 84c's provider-native upload becomes opt-in rather than forced. Ships no provider mechanism and no embeddings. §13 consumer: the materializer + the same-wave 84c. RFC §6.4, §6.5, §6.10. Deps: F11/D-166, 107c. Same wave as 84c. See `docs/plans/phase-84b-multimodal-disposition-policy.md`.
- **84c — Provider-native multimodal mechanism (D-190).** Implements the `provider_native` disposition: the bifrost driver hands an over-threshold attachment to the provider's own understanding via `FileUploadRequest`→`file_id` (already on `core@v1.5.15`), performed *inside* `Complete` so `LLMClient` stays one method. Priority order is deliberate — **image/audio/video first** (regain vision/audio/video capability the stub path loses), **PDF/documents last** (the `ref`/`tool`+84d route is preferred for docs). Adds the part-level `ProviderNative` flag (settable by any `CompleteRequest` builder — the driver is the ONLY seam; the run loop never pre-uploads), optional `ProviderFileID`/`DocumentType` content fields, a driver-owned identity-scoped `file_id` cache + lifecycle (TTL/evict + `Close`-time cleanup; observability via the `llm.provider_file.uploaded` event), and the streaming-with-multimodal residual that Phase 107's row forward-referenced (in 107's `req.Stream`+`llm.completion.chunk` vocabulary). `ArtifactStub` stays the universal degradation. Opt-in via 84b; never the default. Shipped deviation (§4.3, D-190): the optional run-loop cancel hook is not wired — the wrapped `LLMClient` chain would need a forwarding method through five wrapper layers; the driver-owned lifecycle (TTL/LRU evict + `Close` sweep, with per-key fill coalescing) is the authority, per the SDK-lens C3 guidance. RFC §6.5, §6.10, §11Q3. Deps: 84b, 107, 32. Same wave as 84b. See `docs/plans/phase-84c-provider-native-multimodal.md`.
- **84d — Embedding client + semantic retrieval (D-191).** Adds Harbor's first embeddings capability — an `Embedder` §4.4 seam wired to bifrost's `EmbeddingRequest` — with its §13 in-wave consumers being **semantic memory retrieval and semantic skill retrieval** (the direction set by the owner; not a standalone RAG tool). Both are opt-in modes composing with (not replacing) rolling_summary memory + token-savvy skill retrieval; vectors persist in the existing stores (brute-force similarity at V1 scale; ANN deferred). This is the primitive that makes 84b's `ref`/`tool` document path powerful. Requires a §6.5 RFC addendum (the `Embedder` seam) landed in the same PR. RFC §6.5, §6.6, §6.7. Deps: 32, 23, F11/84b. Follows the 84b+84c wave. See `docs/plans/phase-84d-embedder-semantic-retrieval.md`. *Shipped as planned with four recorded §4.3 deviations (see D-191): the seam lives in `internal/embeddings` with the driver blank-imported via the D-196 `internal/drivers/prod` aggregator (the plan's pre-110c "blank-import at `cmd/harbor`" wording); the interface carries a lifecycle `Close` alongside `Embed`; the skills-side injection resolved to the store seam (`skills.Deps.Embedder` + localdb's semantic Search path) rather than a tool-constructor seam; the §6.5 addendum pre-landed with the D-189 plans PR, so this PR's RFC delta is the D-191 contract sentence + the §6.6/§6.7 consumer text.*
- **84e — Semantic memory consumption in the run loop (D-211).** Closes the gap 84d (D-191) left by design: `MemoryStore.SearchTurns` shipped with store/SDK consumers only — nothing in the run loop called it, so the agent never semantically recalled earlier conversation turns and the planner prompt's memory injection (the 83d path) stayed rolling-summary-only. 84e makes the run loop, when `memory.retrieval: semantic` is on (the ONLY switch — no second knob), search the session's embedded turns with the task query and inject the top-k recalled turns into the prompt's `<read_only_external_memory>` tier — the `planner.MemoryBlocks.External` slot that was nil on every production path since 83d, so the planner gains zero new surface and recall inherits the UNTRUSTED framing for free. Composes with `rolling_summary` (the Conversation tier is byte-untouched); mode off → byte-for-byte prompt parity, zero embedder traffic. The fetch+recall step is promoted to ONE home, `runctx.FetchMemoryBlocks`, consumed by both `cmd/harbor`'s runOne and the devstack mirror — the per-step `GetLLMContext` mirror copies collapsed. Knobs ride the existing memory block (`retrieval_top_k` reused; new `retrieval_min_score` similarity floor, default 0.0, range [-1,1], validated) via a 110c-shaped `memory.RecallFromConfig` exporter with field-parity test. Degradation posture is fail-loud: a recall error fails the run (`runtime_fetch_error`, LLM never called) — never a silent fall-back to rolling-summary-only. Recalled turns are text-only and per-turn capped (2 KiB per side; D-026 guard stays the backstop). Records the deferred sibling: a `memory.search` Protocol method must precede any Console memory-search page (D-062 ordering) — parked for post-109 planning. RFC §6.2, §6.5, §6.6. Deps: 84d, 83d, 83f, 110b, 110c, 107c. *Shipped as planned. See `docs/plans/phase-84e-semantic-memory-runloop.md`.*

#### 110-band — Wave B SDK re-homing (production semantics out of `cmd/harbor`)

The 2026-06-09 SDK friction audit (`docs/notes/sdk-friction-audit.md`; program entry
D-193) found a **package-main stratum** of production semantics that lives only in
`cmd/harbor` with an already-diverged D-094 devstack mirror (two shipped/live
silent-field-drop bugs — D-155, B3 — plus a degraded executor, a silently-dropped MCP
ToolPolicy projection, and missing Emit/OnChunk/envelope wiring on the official test
surface). The 110-band promotes that stratum into reusable `internal/` packages and
collapses the mirror to thin callers — every promotion deletes a devstack copy in the
same phase (§13 primitive-with-consumer + §17.6 fix-both-sides). Staging: **Stage 1 =
110a ∥ 110c** (independent), **Stage 2 = 110b ∥ 110d** (after Stage 1 merges). The
band is module-internal; the external-module facade (the audit's Wave D) is a future
RFC-level program for which 110d is the named prerequisite.

- **110a — Tool-executor promotion (SHIPPED — D-194).** Promotes the only production
  `steering.ToolExecutor` (`cmd/harbor/cmd_dev_executor.go`, ~660 lines: catalog
  dispatch, D-026 heavy-result artifact promotion, `CallParallel` via
  `internal/runtime/parallel`, SpawnTask/AwaitTask with depth caps) to
  `internal/runtime/dispatch.NewToolExecutor(catalog, artifacts, tasks, opts...)`.
  Also exports the Phase-106 answer envelope `{answer, finish_reason,
  tool_calls_seen}` + terminal task error-code constants as `planner.AnswerEnvelope`
  et al. (home picked by import direction — tasks stays planner-free), re-homes the
  catalog→planner view as `tools.NewPlannerView` (structural satisfaction of
  `planner.ToolCatalogView`; tools cannot import planner), re-points
  `internal/planner/react/prompt.go`'s shape-contract citation away from `cmd/harbor`,
  switches the D-192 HITL E2E off its test-local executor shim onto the real promoted
  executor, and DELETES the devstack degraded executor (capability drift closed).
  D-025 concurrent-reuse test (N≥100, `-race`) mandatory. RFC §6.4, §6.5, §6.2. Deps:
  D-192 fix (merged), 107d, 107e, 83i. Stage 1, parallel with 110c. See
  `docs/plans/phase-110a-tool-executor-promotion.md`.
- **110b — RunContext population + event-closure promotion (SHIPPED — D-195).**
  Promotes the five RunContext-population helpers duplicated cmd↔devstack into
  `internal/runtime/runctx` (direction-safe: runtime imports planner/memory/skills;
  planner gains NO memory import): `ProjectMemoryBlocks`, `ProjectSkillsContext`,
  `ExtractSkillKeywords` + stopwords (the D-156 FTS5 query shaping — a third copy
  existed; godoc carries the "scheduled for deletion by Phase 111d (D-201); add no
  new consumers" notice per the owner's 2026-06-09 scope amendment),
  `ExtractAssistantAnswer`, and the D-166 `ResolveInputArtifacts` policy —
  following the `planner.BuildArtifactManifest` precedent. Adds
  `events.IdentityStampingEmitter(bus, q, logger)` for `RunContext.Emit` and
  `llm.NewChunkPublisher(bus, q, taskID, logger)` for `OnChunk` (the closures whose
  identity-envelope trap once produced 280+ bus-rejected chunks per task). cmd +
  devstack become callers; devstack ADDITIONALLY gains missing parity: Emit/OnChunk
  wired in its RunSpec and `MarkComplete` carrying the 110a answer envelope instead of
  an empty `TaskResult{}` (pinned by `test/integration/phase110b_runctx_parity_test.go`).
  Also wires the D-196 call-4 handoff one-liner: dispatch's spawn-depth default now
  references `config.DefaultSpawnDepthCap`. RFC §6.2, §6.5, §6.13. Deps: 110a, 83f,
  83i, 83m, 107. Stage 2, parallel with 110d. See
  `docs/plans/phase-110b-runcontext-population-promotion.md`.
- **110c — Config-projection exporters (SHIPPED — D-196).** The five config→snapshot
  projections become exported helpers on the OWNING packages (settled direction:
  subsystem imports `internal/config` additively; config stays a leaf; the snapshot
  decoupling is preserved because `FromConfig` is optional sugar, never a required
  path): `llm.SnapshotFromConfig` (absorbing the four private `copy*` helpers —
  closing the D-155 recurrence class), `memory.SnapshotFromConfig`,
  `skills.SnapshotFromConfig`, `planner.ConfigFromOperator` (fixing the LIVE devstack
  drift B3 — four planner knobs silently dropped today, pinned by a reflection
  field-parity test), `governance.ConfigFromOperator`. Plus: `config.Defaults()`
  exported (hand-built configs start defaulted), planner-adjacent knob projections
  re-homed (`skills_context_max` default, `planner.HintsFromConfig`, spawn-depth
  default deduped), a headless validation profile (`ValidateCore` — config-without-
  binary stops demanding JWT identity fields), and ONE blank-import aggregator
  (`internal/drivers/prod`) imported by `main.go` and devstack — also closing
  devstack's missing-LLM-wrapper trap (no corrections/downgrade/retry on its chain
  today). cmd + devstack consume every projection; all duplicates deleted. Shipped
  as planned, plus one §17.6 cross-fix the parity gate surfaced: both
  `copyModelProfiles` copies silently dropped the per-model `cost_overrides:` /
  `corrections:` yaml (a third D-155-class drop); `llm.SnapshotFromConfig` maps
  both, pinned by the sub-struct parity tests. The spawn-depth constant is exported
  as `config.DefaultSpawnDepthCap`; the executor-side clamp (110a's
  `internal/runtime/dispatch`) references it at Stage 1 merge (parallel worktrees).
  RFC §6.5, §6.6, §6.7, §9, §10. Deps: 83l, 83f, 107d, 107e. Stage 1, parallel with
  110a. See `docs/plans/phase-110c-config-projection-exporters.md`.
- **110d — Assembly promotion (D-197).** SHIPPED. Promoted devstack's `tryAssemble`
  shape into the exported, error-returning
  `assemble.Assemble(ctx, *config.Config, Options) (*Stack, error)` in
  `internal/runtime/assemble`; `bootDevStack` and `devstack.Assemble(t, ...)` are
  thin wrappers — the last of the D-094 subsystem-wiring mirror is collapsed.
  Promoted the remaining cmd-local assembly legs: `mcpdrv.Attach` next to the driver
  INCLUDING the config→`ToolPolicy` projection (the devstack silent drop is closed —
  regression pinned in `phase83g`'s real-stdio-fixture E2E + the attach unit E2E),
  `auth.BuildProviders` (OAuth KEK→sealer→tokenstore→provider chain; per §4.3 it
  returns the provider map only — approval gates remain the catalog Builder's
  output, which the assembly invokes), and
  `events.OpenWith(ctx, cfg, redactor, Deps{State})` + `events.RegisterWithDeps` so
  the durable event driver shares the runtime's StateStore through the factory path
  (recorded reconciliation: the assembly opens State BEFORE the bus so the shared
  store outlives it — production's pre-110d bus-first order swapped, behaviour
  preserved). The per-task run-loop *driver* (the `task.spawned` subscriber) stays
  per-caller (110b's seam); headless embedders drive `Stack.RunLoop.Run` directly.
  Ships `docs/recipes/embed-harbor-headless.md`, acceptance-gated by
  `test/integration/phase110d_assemble_test.go` (recipe path on real drivers +
  durable-store sharing + identity propagation + 2 failure modes + N=10
  Assemble/Close cycles + N=100 concurrent runs on one stack). RFC §6.4, §6.13, §9,
  §10. Deps: 110a, 110b, 110c, 64, 83g, 30, 57. Stage 2, parallel with 110b. See
  `docs/plans/phase-110d-assembly-promotion.md`.

#### 111-band — Wave C: finish (or formally defer) the half-shipped primitives (SDK friction audit §3)

The 2026-06-09 SDK friction audit (`docs/notes/sdk-friction-audit.md`) found a band of shipped primitives with **zero production consumers anywhere** — not even the dev binary — several behind config knobs that validate cleanly and then silently do nothing. These are standing §13 violations (primitive-without-consumer; test-stubs-as-defaults one layer up: seams never exercised under real call sites). The 111 band gives each its first production consumer or a recorded disposition. Staging: the band parallelizes freely after Wave B Stage 1 (110a + 110c) merges; 111a soft-depends on 110c; all six phases are mutually independent. D-numbers D-198–D-203 are reserved per phase (logged when each ships).

- **111a — Governance enforcement assembly (D-198).** SHIPPED. Closed the audit's headline gap: `governance.SetFactory`'s only caller was a test; populated `governance.identity_tiers` drove the posture provider only — clean validation, zero enforcement. Shipped exported `governance.NewSubsystemFromConfig(cfg, store, bus)` (Compound in the documented MaxTokens→RateLimiter→CostAccumulator order; nil Subsystem on empty tiers preserving the D-044 latent default; `ErrInvalidConfig` on nil store/bus with tiers), called via `SetFactory` from the production assembly (`assemble.Assemble`, eager build → fail-loud boot → factory installed BEFORE `llm.Open`; empty tiers clear the factory and `Stack.Close` clears it again); consumes 110c's `governance.ConfigFromOperator`; documents `governance.Wrap` as the multi-runtime headless escape (SetFactory-vs-per-Open evaluated + decided: keep the global seam for the binary); removed the Wave A posture-only boot warning (§4.3 correction: the warning lived in `assemble.go` post-110d, not `internal/config/validate.go` — `validateGovernance` never carried one); E2E proves a configured tier actually rejects/limits (cost + rate + MaxTokens each exercised against the real assembled stack, with identity-propagated `governance.*` events, cross-session isolation, and D-025 concurrency). RFC §6.15, §6.5, §6.11. Deps: 32, 36a, 36b, 110c (soft). See `docs/plans/phase-111a-governance-enforcement-assembly.md`.
- **111b — Tool-OAuth completion leg (D-199).** SHIPPED. `auth.CallbackHandler` (state→`PendingFlow`→`CompleteFlow`→typed 404/410/400/502 error mapping; no secret material in responses or logs) is `CompleteFlow`'s first production caller, mounted by `harbor dev` at `GET /v1/tools/oauth/callback` (devstack mirrors; both over `assemble.Stack.OAuthProviders` — D-197) and mountable headless on any mux. The full choreography E2E (`test/integration/phase111b_oauth_completion_test.go`): gated tool → `tool.auth_required` + `pause.requested` → authorize → 302 redirect onto the handler → `CompleteFlow` → `pause.resumed{Decision: resume}` → run re-enters and the tool succeeds USING the minted token; failure modes: expired flow (410 + pause parked), replayed callback (404 by consumption). §4.3 refinements recorded in D-199: `PendingFlow` returns `(PendingFlowInfo, bool)`; `DenyFlow` added (upstream denial → `DecisionReject` resume); both on the `OAuthProvider` interface; the run-level re-entry rides the existing steering RESUME surface (the recipe documents the honesty note). Recipe: `docs/recipes/steer-and-resume-a-run.md`. Closed the `InitiateFlow`/`CompleteFlow` primitive-without-consumer pair (§13). RFC §6.4, §3.3, §6.3. Deps: 30, 50, 31, the D-192 steering fix. See `docs/plans/phase-111b-tool-oauth-completion.md`.
- **111c — Durable pauses + pause lifecycle (D-200).** `WithCheckpointStore` has zero production consumers (both assemblies construct the Coordinator storeless with the StateStore in scope); `requestPause` persists `Trajectory: nil`; no pause GC exists — `DecisionTimeout` has no producer and cancel-while-paused orphans records forever. Threads the run's Trajectory into `requestPause`; wires `WithCheckpointStore(stateStore)` in both assemblies; pause→new-Coordinator-over-same-store→Resume→trajectory-restored E2E (+ the §11 `ErrUnserializable` fail-loud test); ships `WithMaxParkDuration` + the exported `pauseresume.RunSweeper` emitting `pause.resumed` with `DecisionTimeout` — its first producer — started config-gated by the one merged `assemble.Assemble` site (cmd + devstack inherit as thin callers). Shipped deviation (§4.3, recorded in the plan + D-200 §5): the sweeper's SCAN is package-internal over the Coordinator's registry rather than `Coordinator.List` — List is §6-identity-scoped with no all-tenants wildcard (the plan's Risks anticipated exactly this); every MUTATION still goes through the public `Resume` under the pause's own scope. Timeout is terminal: the parked run finishes `Finish{ConstraintsConflict}` via the bus-event wake + `Status.Decision` fallback. RFC §3.3, §6.3, §6.11. Deps: 50, 51, the D-192 steering fix. See `docs/plans/phase-111c-durable-pause-lifecycle.md`.
- **111d — Skills canonical surface + ingestion (SHIPPED — D-201).** Three intertwined gaps closed: the rich Phase-38 planner tools (capability filter, redaction, budgeter) + Phase-41 generator were registered nowhere while production registered a thinner parallel builtin implementation (the §13 two-implementations smell); Skills.md ingestion had no shipped path; the Phase-39 Directory had only test consumers. Shipped: builtin `skill_search`/`skill_get` delegate to the exported Phase-38 handlers (duplicate bodies deleted; filter/redaction/budgeting on production; the capability envelope is server-computed from the run's visible-tool set via `tools.VisibleNames`, never LLM-supplied); `skill_list` + `skill_propose` register through the same carrier (`skill_propose` opt-in rides the existing `tools.built_in` names list — the plan's sketched `…skill_propose.enabled` key was dropped as a second parallel enablement mechanism, §4.3 deviation in the plan); `harbor skill import`/`rm` ship over exported `importer.ImportAndStore` (+ §18 SKILL.md restoration in the same PR); the Directory was WIRED per the recorded owner decision (2026-06-09) as the `<skills_context>` producer — `runctx.ExtractSkillKeywords` deleted per its D-195 deprecation notice; `skills.directory.{pinned,max_entries,selection}` config block added. Note: the plan's "stale `cmd/harbor/main.go:76-90` promises" had already been rewritten by 110c — the surviving stale text lived in `internal/skills/tools`' package doc + `internal/drivers/prod`'s honesty notes and was replaced there. RFC §6.7, §8. Deps: 37–41, 107c. See `docs/plans/phase-111d-skills-canonical-surface.md`.
- **111e — Trajectory compression consumer (SHIPPED — D-202).** `planner.Summariser` has only test implementations; `MaybeCompress` has zero call sites; `Budget.TokenBudget` is dead on every path — while the consumer half (the React prompt's `Summary != nil` branch) is already wired. Ships a real LLM-backed `TrajectorySummariser` (in `internal/llm/summarizer`, distinct from the unrelated `memory.Summarizer` — do not conflate), the `MaybeCompress` integration in `steering.RunLoop`'s step loop gated on `TokenBudget > 0`, the `planner.token_budget` config → `RunSpec.Budget` production wiring, and the godoc un-dormanting. Long-trajectory E2E: compression fires, the prompt shrinks, the run completes correctly on summary-carried context. Scoped tight: one compression per run, no auto-cascade. RFC §6.2, §6.5. Deps: 46, 35, 107, the D-192 steering fix. See `docs/plans/phase-111e-trajectory-compression-consumer.md`. *Shipped (D-202).* One recorded §4.3 deviation: the compaction payload renders the trajectory's planner-facing projection (per-step action + `LLMObservation`, per-fragment capped) rather than the raw `Serialize` bytes — raw observations may carry heavy content that must never reach the LLM edge (D-026 / `ErrContextLeak`); the budget estimator still measures the full serialized trajectory.
- **111f — Telemetry assembly + approval-gate authorizer seam (SHIPPED — D-203).** Two halves, both closed. Telemetry: pre-111f, `telemetry.New` (redactor-mandatory, identity-attribute, bus-paired Logger) had zero production callers — cmd booted bare slog; `engine.WithRunErrorHandler` had no production caller; metrics got `BridgeBusToMetrics` but traces got no bridge and `NewTracer` was never constructed despite the blank-imported exporters. Shipped: `telemetry.New` + the `Stack.RunErrorHandler` wired into the production assembly (`assemble.Assemble`; cmd + devstack inherit as thin callers), `telemetry.BridgeBusToTracer` started symmetric with the metrics bridge (lifecycle-pair span model, `DefaultTraceBridgeFilter()` volume guard, both on the closer chain), and `docs/recipes/observe-an-embedded-runtime.md`. Approval: pre-111f, `ResolveApproval` hard-required `internal/protocol/auth` scopes and the runtime's own steering bridge self-elevated to pass its own gate — wire-layer auth vocabulary in an in-process control path. Shipped: the injected `GateDeps.Authorizer` seam (runtime identity/control-scope default `approval.NewIdentityAuthorizer()`; protocolauth via the server-side `ProtocolScopeAuthorizer` adapter), the protocol/auth import removed from `internal/tools/approval` (the steering bridge's self-elevation deleted outright), and the direction rule recorded: runtime may import protocol TYPES, never protocol auth/methods/transports (see D-203's 2026-06-10 addendum for the `<area>/protocol` adapter carve-out + the named `internal/search` standing violation). Three recorded §4.3 deviations (D-203): (a) `assemble.Options` gains `TelemetryOptions` / `TracerOptions` / `ApprovalAuthorizer` (the MetricsOptions precedent — the union of real-caller needs); (b) the Protocol-side adapter injects via `assemble.Options` rather than the plan's "server-side gate assembly" because gate assembly lives in the ONE D-197 fan-out (the adapter stays owned by `internal/server`); (c) `engine/options.go`'s godoc rewritten, not merely "now true" (the Wave A honesty text had said "no production assembly installs one today"). RFC §6.14, §6.4, §5.1. Deps: 03, 04, 05, 31, 55, 56, the D-192 steering fix. See `docs/plans/phase-111f-telemetry-assembly-approval-seam.md`.

#### 112-band — Wave D: the public SDK facade (D-204)

RFC §3.6 settles the design (alias-based `sdk/` tree; curation over moves; D-204 records the rationale). The wave's §13 pairing: 112a ships the facade, 112b is its consumer in the same wave.

- **112a — The public SDK facade (SHIPPED — D-205).** The `sdk/` tree of alias-based re-exports per RFC §3.6's inventory; the facade-integrity test runs the headless recipe via `sdk/` imports only (grep-gated zero `internal/` imports; deterministic-planner override over an offline custom-provider bifrost client so the path runs in CI without network or the mock driver); `sdk/drivers/prod` parity with the internal aggregator by construction (its only content is the internal aggregator's blank import); AGENTS/CLAUDE §3 amendment. No moves, no mechanism — forwards only, with ONE documented carve-out (`sdk/tools/inproc.RegisterFunc`, a generic wrapper Go cannot express as a `var` forward; smoke-gated as the sole `func` in the tree). See `docs/plans/phase-112a-sdk-facade.md`.
- **112b — External consumers + the compile gate (SHIPPED — D-206).** Scaffold templates emit `sdk/` imports (the tool-declaring output compiles AND tests green as an external module — the audit's headline external break, now gated by `scripts/smoke/phase-112b.sh`: scaffold → replace directive → `go build`, bounded + self-tested, plus an external harbortest `go test` probe); harbortest vocabulary externally satisfiable via the aliases with signatures unchanged and zero kit constructors; the five consumer-facing recipes + README flipped to `sdk/` paths (grep-gated). Two recorded §4.3 calls (D-206): (a) phase-67's smoke keeps the toolless build-check and this gate owns the tool-declaring shape (no duplication); (b) the conversions flushed out additive facade extensions — `sdk/{audit,telemetry,telemetry/eventbus,governance,tools/auth,skills/{importer,tools,generator}}` + `sdk/tools.ErrorClass` (RFC §3.6 item 3 amended) — while `sdk/pauseresume` was deliberately NOT added (D-205's curation; the steer recipe reworked to the config-driven assemble shape). Wave D and the SDK re-homing program close here. See `docs/plans/phase-112b-external-consumers.md`.

#### 113-band — the Protocol adoption track on the docs site (D-209 / D-210 reserved)

`docs/notes/protocol-docs-proposal.md` (owner-approved; merged as PR #305) is the binding design: the Protocol is Harbor's ecosystem surface (RFC §5.1 — "the same surface powers a remote attach, a third-party dashboard, or an IDE/TUI client"), but a client author today must read Go source to answer *what methods exist, what events arrive with what payloads, what an error looks like, how auth works, what a version bump means*. The band serves the proposal's four audiences (evaluator / client builder / event integrator / control integrator) with a docs-site track whose center of gravity is a **generated, gen-check-gated contract reference** — the house single-source discipline applied to adopter docs. The owner resolved the proposal's open questions per its recommendations: **Q1** event catalog is registry-read at gen time (the generator imports `internal/drivers/prod` and reads the populated `events.EventTypes()` registry, payload shapes via the `CanonicalWireTypes`-style reflection + lockstep-test treatment); **Q2** OpenAPI emission deferred (recorded as a stretch in 113a's non-goals); **Q3** the conformance suite is documented as the certification path in 113b but its sdk-export waits for a real third-party ask; **Q4** versioned docs deferred to the first breaking Protocol change (recorded in both plans' risks). §13 pairing: 113a ships the generator + gate, and its own choreography guides + executed quickstart are the consumers in the same phase; 113b consumes 113a's reference pages (lockstep greps) and closes the track.

- **113a — the floor (D-209, logged at ship; Shipped).** `cmd/harbor-gen-protocol-docs` emits `methods.md` / `events.md` / `errors.md` / `types.md` into `docs/site/protocol/` from the canonical sources (`methods.go` + the transports' `*RoutePattern` tables + `IsControlMethod`/cluster predicates + auth scopes; the Q1 registry-read event catalog; `errors.go`; `CanonicalWireTypes`) under generated-file headers; `make protocol-docs-gen-check` (git diff --exit-code) wired into the docs workflow — the gate shape D-093 specified, built here for a generator that actually exists (the TS generator stays deferred per D-132 / issue #179; no dependency); the "Speak Protocol in 15 minutes" quickstart whose curl steps the smoke EXECUTES against the preflight dev server (the recipe-cannot-lie pattern); choreography guides 1–3 (auth & identity incl. the D-171 session-blank model; streaming semantics; task control); the Protocol nav section + README Docs-table row; the §18 amendment putting the generated reference under the same-PR regeneration rule (AGENTS+CLAUDE, mirror-gated). See `docs/plans/phase-113a-protocol-reference-and-quickstart.md`. Shipped 2026-06-10 with two recorded §4.3 deviations (D-209 calls 3–4): `control.HTTPStatus` exported so the generated error page reads the wire transport's own code→status binding, and the executed quickstart's steering step accepts both documented wire outcomes (200 accepted / 404 not_found on a terminal run — the deterministic mock-path result, doubling as the §17.3 failure-mode leg).
- **113b — the closer (D-210; Shipped).** Choreographies 4–5: the pause model (`pause.requested` → approve / reject / OAuth-callback / plain resume; durable pauses across restarts; `DecisionTimeout` reaps — the wire view of RFC §3.3's unified primitive) and versioning & compatibility (RFC §5.3 made adopter-facing, incl. unknown-field tolerance and unknown-method 404/405 handling — the smoke SKIP convention promoted to adopter contract); the build-a-client guide around a ~150-line worked event-viewer at `examples/protocol-clients/` (compile-gated in the smoke) with the hand-maintained TS wire-type module + the Console as reference implementations; the conformance-certification page (how to run `internal/protocol/conformance`, what passing claims — NO sdk-export per Q3). Deps: 113a. See `docs/plans/phase-113b-protocol-choreographies-and-certification.md`. Shipped 2026-06-11 with one recorded §4.3 deviation (D-210 call 2): the OAuth callback route lockstep-greps against the exported `auth.CallbackPath` source constant instead of the generated reference — the callback is a provider-redirect mount, deliberately not a canonical Protocol method, so it has no `methods.md` row. The pause guide's approve/reject/timeout wire examples are captured from a production-driver devstack assembly; the OAuth leg is transcribed from the handler + its tests and says so (D-210 call 1). A §17.6-posture docs fix rode along: `task-control.md` no longer claims `task.paused`/`task.resumed` fire on the live pause path (nothing calls MarkPaused/MarkResumed in production — a parked run's task stays `running`; `pause.list` is the authoritative park read).

#### 85-band — MCP client/host compliance (prioritised first post-V1 work)

The integer Phase 85 (Skills Portico provider driver) is **removed**: Portico is an MCP gateway and speaks MCP like any server, so the generic MCP client driver consumes it — a Portico-specific driver would duplicate the driver and couple Harbor to one ecosystem tool. The 85-band closes Harbor's MCP-client-compliance gap (audit + decomposition in **brief 14**). This band is the **first post-V1 work** — ahead of 83/84 in execution priority.

**MCP 2026-07-28 RC re-plan (effective 2026-05-28).** The MCP Foundation published a release candidate locked 2026-05-21; final spec drops 2026-07-28; Tier-1 SDKs ship RC support within a 10-week window (≈ late July–early August 2026). The RC reshapes the 85-band:

- **Roots, Sampling, Logging are deprecated** in the RC (annotation-only — functional 12+ months — but on death row). Phases that build operator-facing surface against them are cut.
- **Tasks moves from experimental core to an extension**, redesigned: `tasks/list` removed; new method set is `tools/call` returns a task handle, then `tasks/get` / `tasks/update` / `tasks/cancel`. 85h's hand-transcription against 2025-11-25 would lock in the wrong shape.
- **Session handshake (`initialize`/`initialized` + `Mcp-Session-Id`) is removed**, Streamable HTTP requires new `Mcp-Method` / `Mcp-Name` headers, error code `-32002` flips to `-32602`, server-to-client requests restructure into `InputRequiredResult`. These cross-cutting changes land as a new sub-phase **85m**.
- **Authorization hardens** with six new SEPs (`iss` validation, DCR `application_type`, issuer-bound credentials, refresh-token docs, scope accumulation, `.well-known` clarification). 85b absorbs them; scope grows.
- **Cut phases** (`85c`, `85e`, `85h`, `85i`) keep their plan files as historical context — do not delete — but their Status reads `Cut`. The `docs/decisions.md` entry recording this re-plan is the canonical reference.
- **Lettering note:** 85k (skills) already exists. The new RC-adoption sub-phase is **85m** (skipping `l` to avoid `l`/`I`/`1` ambiguity next to existing `85i`).

Per-phase RC verdict + readiness:

- **85a — MCP client core-compliance fixes.** Pagination-truncation fix, `*ListChanged` handlers, resource `Unsubscribe`-on-close. The honest-empty `roots` capability advertisement is now **permanent** (not a stopgap; 85e is cut). RFC §6.4. Deps: 28. **Ready now** — uses go-sdk v1.6.0 surface that exists today; nothing the RC removes is in scope. See `docs/plans/phase-85a-mcp-client-core-compliance.md`.
- **85b — MCP HTTP OAuth (scope ↑).** Wire `auth.Provider` into the MCP driver; RFC 9728 protected-resource-metadata discovery; `WWW-Authenticate` 401 step-up; RFC 8707 resource indicators. **Adds the six RC auth SEPs**: SEP-2468 (`iss` validation per RFC 9207), SEP-837 (DCR `application_type`), SEP-2352 (credential binding to issuer + re-register on migration), SEP-2207 (OIDC refresh-token docs), SEP-2350 (scope accumulation during step-up), SEP-2351 (`.well-known` suffix). Also: token-store keying moves from session-scoped to per-request `_meta` since the RC removes `Mcp-Session-Id`. RFC §6.4, §3.3. Deps: 28, 30, 50, 85m (for the per-request keying). **Ready now** — OAuth flow is Harbor-side; SDK exposes the wire transport and `WWW-Authenticate` already; the per-request keying mechanic ships with 85m but the plan can be authored against the new shape now. See `docs/plans/phase-85b-mcp-http-oauth.md`.
- **~~85c — MCP sampling provider~~ (CUT).** RC deprecates `sampling/createMessage`; replacement is "direct LLM provider API integration" — which is what `llm.LLMClient` already is. Building a `CreateMessageHandler`, pause-gated review surface, `modelPreferences` resolution and tool-enabled sampling would ship operator-facing surface for a 12-month-EOL feature. Servers needing an LLM bring their own provider per the RC's guidance. Plan file kept as historical context. **No revisit.**
- **85d — MCP elicitation provider.** Form vs URL mode and the secret-rejection rule survive; the **wire mechanic does not**. RC replaces the SSE-based wait with `InputRequiredResult` (`inputRequests`, `requestState`) + client retries the original call with `inputResponses`. The plan as written targets SSE — must be rewritten before implementation. The pause/resume primitive integration is still conceptually right. RFC §6.4, §3.3. Deps: 28, 50, 85m. **Revisit after SDK-RC** (≈ late Jul–Aug 2026). See `docs/plans/phase-85d-mcp-elicitation-provider.md`.
- **~~85e — MCP roots provider~~ (CUT).** RC deprecates roots; replacement is "tool parameters, resource URIs, or server configuration." 85a's honest-empty advertisement is now the permanent posture. Plan file kept as historical context. **No revisit.**
- **85f — MCP remaining server features (slim).** Ship **completions** (`completion/complete`), **resource templates** (`resources/templates/list`), and **progress** (`_meta.progressToken` + `notifications/progress`). **Drop the logging slice** — RC deprecates `logging/setLevel` + `notifications/message`; replacement is stderr / OpenTelemetry, both of which Harbor already has. RFC §6.4. Deps: 28, 85a. **Ready now** — all three retained features are in go-sdk v1.6.0. See `docs/plans/phase-85f-mcp-remaining-server-features.md`.
- **~~85g — MCP Apps host~~ (DEPRECATED → superseded by 109a–c, D-172).** The original premise — "revisit after RC-final because Apps is experimental and the RC may reshape `_meta.ui.resourceUri`" — was overturned: MCP Apps is a stable, independently-versioned extension (`io.modelcontextprotocol/ui`, the `ext-apps` repo), NOT gated on the July RC, and it ships an official framework-agnostic host bridge (`@modelcontextprotocol/ext-apps` AppBridge) that removes the hand-rolled-bridge risk this plan carried. A code audit also found 85g's "purely Console-side" non-goal factually wrong (the MCP driver doesn't parse `_meta.ui.resourceUri`, `tool.completed` carries no content, and `ReadResource` isn't exposed on the Protocol — so there is real runtime + Protocol work). Pulled forward into V1.1.x as the three-phase **109a–c "MCP Apps host" wave**, scheduled immediately after Phase 108. Plan file kept as historical context, marked deprecated. RFC §6.4, §7. See D-172, D-173, and `docs/plans/phase-109a-mcp-apps-runtime-protocol.md` / `phase-109b-console-mcp-apps-host.md` / `phase-109c-mcp-apps-displaymode-layout.md`.
- **109a — MCP Apps runtime + Protocol surface.** The runtime/Protocol enablement layer the deprecated 85g plan wrongly assumed already existed. Parse `_meta.ui.resourceUri` on MCP tool results; recognise `ui://`-scheme resources; project the app reference (resourceUri + negotiated DisplayMode + `RawHTMLTrusted`) onto the tool-result Protocol surface; add `mcp.servers.read_resource` (identity-scoped, D-026 heavy-content aware) to fetch the `ui://` HTML; negotiate `DisplayModes` from the server's `io.modelcontextprotocol/ui` capability (replacing the static `registry.go` placeholder); add an app-initiated-tool-call proxy that routes through the existing approval/OAuth/identity tool-safety path. §13 same-wave consumer: 109b. RFC §6.4, §6.5, §7. Deps: 28, 85a, 84a. See `docs/plans/phase-109a-mcp-apps-runtime-protocol.md`.
- **109b — Console MCP Apps host.** Sandboxed-iframe renderer in the shared chat module (`web/console/src/lib/chat/renderers/mcp-app.svelte`, D-091); strict CSP; `postMessage` origin validation; the official AppBridge wired in **manual-handler mode** (D-173) — every app→host call Protocol-proxied through 109a, never a direct MCP connection; honours `RawHTMLTrusted` → sandbox strictness; the **inline** DisplayMode via the renderer registry. Adds `@modelcontextprotocol/ext-apps` + `@modelcontextprotocol/sdk` to `web/console` (RFC §10 dependency-addition prerequisite). RFC §6.4, §7. Deps: 109a, 73n, 108. See `docs/plans/phase-109b-console-mcp-apps-host.md`.
- **109c — MCP Apps DisplayMode layout.** The Playground page-level layout state machine for **fullscreen** (app replaces chat + composer; multi-tab) and **pip** (50/50 resizable split, right rail hidden by default + toggle); `inline` already shipped in 109b; `onrequestdisplaymode` drives runtime transitions. Distinct from PG-6 two-agent comparison (post-V1, D-064). RFC §7. Deps: 109b. See `docs/plans/phase-109c-mcp-apps-displaymode-layout.md`.
- **109d — Inline MCP-app discovery (D-215).** Closes the dead seam the 109 wave's §17.5 audit pinned: the chain "a planner-initiated MCP tool result carrying `_meta.ui.resourceUri` → a chat message that mounts the 109b renderer → 109c's layout activates" was never wired, so the renderer + entire layout were unreachable in production. Three breaks closed: (1) the runtime emits a new canonical SafePayload event `mcp.app_available` at the MCP provider's invoke site whenever a tool result declares a `ui://` app (carrying the server source id + resource URI + display-mode hint + run/identity correlation), registered alongside `mcp.resource_offloaded`; (2) the single-sourced wire `MCPAppRef` gains a `server_id` field (also populated on the app-tool-call proxy response) so the renderer resolves which server to read the `ui://` document from; (3) the Console `ChatMessage` gains an `app`/`serverID` field, `MessageBubble` dispatches it under `MCP_APP_INLINE_MIME` to mount the real renderer, and the Playground page attaches the decoded `mcp.app_available` SSE event to the run's agent bubble. The §13 same-wave consumer is the discovery path itself; an inline app's `onrequestdisplaymode` (granted by the page's full available-mode set) opens the app through 109c's already-shipped layout reducer. The wave-end W3 weak synthetic-DOM Playwright test (which re-implemented the clamp) is replaced by a real-component Vitest guard that mounts the shipped `MessageBubble` / `McpAppRenderer` / `AppPanel` and fails if the discovery→render wiring is reverted. RFC §6.4, §6.5, §7. Deps: 109a, 109b, 109c. See `docs/plans/phase-109d-inline-mcp-app-discovery.md`.
- **109e — MCP App discovery reads the tool-DEFINITION `_meta.ui` (D-216).** A spec-conformance fix a live test against a real `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) surfaced: the 109 wave parsed the `_meta.ui.resourceUri` app reference from the tool RESULT (`CallToolResult._meta`), but the canonical SEP-1865 dialect (vendored `McpUiToolMetaSchema`: "UI-related metadata for tools") places it on the tool DEFINITION. A real ext-apps server binds the `ui://` UI resource per tool in `tools/list` and returns an empty result `_meta`, so the result-parse found nothing and `mcp.app_available` never fired — the renderer (109b) + layout (109c) were unreachable against real servers, and every 109a–d test passed only because its hand fixture put `_meta.ui` on the RESULT (matching the buggy code, not the spec). The fix: `buildToolDescriptor` captures the tool-definition `_meta.ui` at discovery (immutable closure capture, D-025); `callTool` reconciles that binding with any optional per-result display-mode hint and fires `mcp.app_available` from the result, feeding BOTH the discovery event AND the app-tool-call proxy projection (`mcpconsole/apps.go`, which had the identical result-only bug — fixed in the same change per §17.6). DisplayMode defaults to inline when none is negotiated/declared (go-study-mcp advertises no UI capability; the Console renderer already mounts on a bare `{resourceUri, serverID}`). The fixtures are corrected to the canonical placement (tool-def `_meta.ui`, empty result `_meta`) and a `HARBOR_LIVE_MCP`-gated probe drives the real go-study-mcp binary over stdio (CI-skipped, verified green in dev). This PR also adds CLAUDE.md/AGENTS.md §17.8: external-protocol conformance fixtures must derive from the real spec, never a hand-built one. RFC §6.4, §6.5, §7. Deps: 109a, 109d. See `docs/plans/phase-109e-mcp-app-tool-def-discovery.md`.
- **109f — Render heavy MCP App documents + operator "pop to side-by-side" affordance (D-217).** Closes two gaps a live test against the real go-study-mcp ext-apps server surfaced. **Gap A:** go-study-mcp's `ui://go-study-mcp/studio/index.html` is 86.4 KB; the default heavy-content threshold is 32 KiB, so 109a's `mcp.servers.read_resource` correctly offloads the document to the ArtifactStore by reference (D-026) and returns an `artifactRef` instead of inline `content`. The 109b renderer treated that as a FATAL "server bug" and refused to render — which hits nearly every real App, since Svelte/React bundles routinely exceed 32 KiB. The renderer now resolves the by-reference stub to a presigned URL via a new injected `MCPAppHostClient.resolveArtifact` seam, fetches the bytes at the iframe edge, and loads them into the SAME sandboxed `srcdoc` (same CSP, sandbox tokens, `wrapAppDocument`, origin guard) the inline path uses — only the content source changes; the offload stays correct (heavy bytes never inline through the context plane). The real `resolveArtifact` impl lives in the Console adapter `makeMCPAppHostClient` (over `artifacts.get_ref`), OUTSIDE the chat module, so the renderer keeps zero `$lib/` imports (D-091). A §17.6 bug-twin is fixed in the same PR: the playground `ChatProtocolClient.resolveArtifact` read the absent `resp.url` (the wire field is `presigned_url`), silently breaking every chat-bubble artifact preview. **Gap B:** a host-side operator "expand ⤢" affordance on the inline app frame pops the app to the 109c side-by-side (`pip`) / fullscreen layout WITHOUT the app asking, dispatched through the EXISTING injected `onDisplayModeRequest` seam → the 109c layout reducer (no parallel display-mode path; no chat-module reach into the page). Always-on Vitest guards: a heavy-document fetch test (realistic >32 KiB App fixture, §17.8) that fails if the artifactRef branch reverts to the error path, plus an inline-path regression and a Gap-B affordance→reducer test. Console-only — no Runtime endpoint or Protocol method. RFC §6.4, §6.5, §7. Deps: 109a, 109b, 109c, 109d. See `docs/plans/phase-109f-heavy-app-doc-render.md`.
- **109g — MCP App documents render inline on every artifact driver (D-218).** A spec-correctness fix a live test against the real go-study-mcp ext-apps server surfaced: the 109 MCP Apps host gated a `ui://` App document on the D-026 LLM-context heavy-output threshold (32 KiB) in `internal/mcpconsole/apps.go::ReadResource`. go-study-mcp's studio App HTML is ~86 KB, so `mcp.servers.read_resource` offloaded it to the ArtifactStore by reference and returned an `artifactRef` — which the Console can only fetch via a presigned URL, and the read-side resolver fails loud (`CodePresignUnsupported`) on every non-S3 driver. So the App never rendered on the inmem / fs / sqlite / postgres stores. Root cause: the heavy-output threshold exists to keep bulky bytes OUT of the LLM context window, but a `ui://` App document NEVER enters the LLM context — the tool result carries only the tiny `_meta.ui.resourceUri` reference; the HTML is fetched ONLY by the Console and rendered in a sandboxed iframe. The fix re-scopes the threshold OUT of App documents: `ReadResource` checks `mcp.IsUIResourceURI(resourceURI)` and rides a `ui://` document inline up to a dedicated `appDocumentInlineCap` (2 MiB) instead of the 32 KiB heavy threshold, so the common case (all real apps) renders on EVERY driver with no presigning. Above the cap, the existing D-026 offload→artifactRef path (the loud `mcp.resource_offloaded` bypass) is preserved for pathologically large apps. An ordinary (non-`ui://`) resource keeps the heavy threshold. The tests use a REAL inmem ArtifactStore on the seam (109f's fetch test stubbed the resolver and so never hit the presign-unsupported driver — the §17.8 failure mode): the below-cap revert-guard reads an 86 KiB `ui://` doc and asserts it rides inline with no offload event (it fails if reverted to the 32 KiB gate — verified); the above-cap test asserts a >2 MiB doc still offloads + fires the event; a `HARBOR_LIVE_MCP`-gated probe drives the real go-study-mcp studio doc through `ReadResource` and asserts it returns inline. No Protocol wire-shape change — `ReadMCPResourceResponse.Content` already carries inline bytes. RFC §6.5, §7. Deps: 109a. See `docs/plans/phase-109g-app-doc-inline-read.md`.
- **109h — MCP Apps UI-host capability advertisement (D-224).** Closes brief 14's "Extension negotiation — Absent: `ClientCapabilities.Extensions` never populated" gap on the MCP southbound driver. The 109 wave shipped the READ side — `negotiateDisplayModes` reads a server's `io.modelcontextprotocol/ui` capability — but the driver never advertised its OWN: `ClientCapabilities.Extensions` shipped empty, so a spec-conformant ext-apps server could not learn the Harbor host renders apps and could not tailor the app references it returns. 109h adds the symmetric WRITE side: the driver advertises the `io.modelcontextprotocol/ui` extension carrying the host's renderable `displayModes` during the initialize handshake (`hostCapabilities` / `filterHostDisplayModes` in `mcp.go`, reusing the existing `uiExtensionKey` + closed `validDisplayModes` set), sourced from a new deployment-level `tools.mcp_app_host.display_modes` config field (`MCPAppHostConfig` + `ToolsConfig.MCPAppHostDisplayModes()`, defaulting to the inline baseline `[inline]`) threaded once through `AttachDeps.HostDisplayModes` — which is also the programmatic SDK seam an embedder sets without YAML. **The roots-regression trap (brief 14 §2 row 4 / §3):** the go-sdk advertises `{"roots":{"listChanged":true}}` by default when `ClientOptions.Capabilities` is nil; setting `Capabilities` to add the UI extension OVERRIDES that default AND the SDK ignores the deprecated `Roots` field in favour of `RootsV2`, so the code MUST replicate the current roots advertisement (`RootsV2.ListChanged=true`) or opting into the extension silently drops roots. This phase PRESERVES current roots behaviour exactly — it does NOT fix the roots honesty defect (brief 14 §3 / the separate 85a stopgap). Sampling/elicitation stay inferred from their handlers, unaffected. The integration test (§17.8) builds two providers from one resolved config value, pairs them to real SDK in-memory transports, and asserts each server's captured `InitializeParams.Capabilities` echoes the configured modes AND still advertises roots — the fixture derives from the SDK's real `InitializeParams` shape, not a hand blob; an opt-out provider advertises roots with no UI extension (the failure mode). No new inbound Protocol method or REST endpoint — the capability is an OUTBOUND client→server advertisement on the MCP handshake; the smoke is static-only. RFC §6.4, §7. Deps: 109a. See `docs/plans/phase-109h-mcp-apps-host-capability.md`.
- **109i — MCP Apps tool-context capture + `mcp.apps.tool_context` (D-225).** The BACKEND half of the MCP Apps "Data Delivery" lifecycle. The 109 wave lets the Console discover (`mcp.app_available`), fetch (`mcp.servers.read_resource`), and render a `ui://` MCP App in a sandboxed iframe — but a rendered app had no way to read the tool context (the input + the lowered result) that produced it. This phase captures that context at the tool-invocation site (`internal/tools/drivers/mcp/mcp.go::callTool`, the same site that emits `mcp.app_available`) whenever a result declares a `ui://` app, and exposes a new identity-scoped Protocol read method, `mcp.apps.tool_context`. Capture rides the EXISTING `StateStore` — all three persistence drivers + identity isolation come free, NO new driver and NO new migration — keyed by the caller's identity triple (with empty RunID; session-scoped) under `kind = "mcp.apps.tool_context/<serverID>/<toolCallID>"`; the input and result are heavy-content-aware at WRITE (a payload ≥ the heavy threshold offloads to the ArtifactStore by reference through the SAME loud-bypass path the resource read uses, refactored into a shared `offloadHeavy` helper). The `tool_call_id` is a deterministic content hash of `run | server | tool | args` (NO mutable `Provider` field — D-025); it is stamped on the `mcp.app_available` event (alongside `tool_name`; the payload stays `SafeSealed` — ids/names are not content), on the wire `MCPAppRef`, and on the app-tool-call proxy projection, so a client correlates a discovered app to its captured context. The new method routes through the AppsSurface dispatcher (`IsMCPAppsMethod`); an unknown or cross-identity `(server_id, tool_call_id)` fails with `CodeNotFound` (existence never revealed across identities — proven by a ≥2-identity isolation test). A capture failure is logged loudly but never fails the tool call (the planner's result is the source of truth); a missing identity fails closed. The capturer is wired into every MCP Provider in `internal/runtime/assemble` (mirrored in `harbortest/devstack` + `cmd/harbor`), and the read seam onto the `AppsAccessor`. New wire types (`ToolContextRequest` / `ToolContextPayload` / `ToolContextResponse`) + the new method are single-sourced, hand-mirrored into `web/console/src/lib/protocol/mcp.ts`, and the generated Protocol docs + wire manifest regenerated. The §13 same-wave consumer is the read path itself (capture → read, exercised end-to-end in Go); the Console UI consumption lands in 109j. Concurrent-reuse tests (N=128) on the shared Provider + AppsAccessor + ToolContextStore pass under `-race`; a `HARBOR_LIVE_MCP`-gated probe drives a real ext-apps server through capture → read. RFC §6.4, §6.5, §7. Deps: 109a, 109d, 109g. See `docs/plans/phase-109i-mcp-apps-tool-context.md`.
- **109j — Console pushes tool-input/tool-result into the app (D-226, Reverted in #346 — re-land tracked in #347).** The Data Delivery Console half (Stage 2 of the spec-compliance wave), consuming the 109i `mcp.apps.tool_context` surface now on main. After the sandboxed app sends `ui/notifications/initialized`, the host fetches the originating tool's context through the injected `MCPAppHostClient` and pushes it via the official AppBridge `sendToolInput()` then `sendToolResult()` (the SDK requires `initialized` before `sendToolResult`). Heavy-aware (resolves an `artifact_ref` to bytes at the iframe edge like 109f, else a faithful by-reference stub — never silently empty); a missing/evicted context (`CodeNotFound`) mounts with no push and no thrown error. The `tool_call_id` flows event → ChatMessage app ref → renderer. No new sandbox/CSP/origin change; the no-direct-transport invariant (D-173) holds — the push uses only the injected client. **Status: the Console data-delivery push was reverted to v1.4 in #346 because it broke the `ui/initialize` handshake (handshake regression). The BACKEND tool-context surface (109i) is unaffected and remains Shipped. Re-landing the Console push is tracked in #347.** RFC §6.4, §7. Deps: 109i, 109b. See `docs/plans/phase-109j-mcp-apps-data-delivery-push.md`.
- **109k — MCP Apps spec-conformance hardening (D-227, Shipped V1.1.x).** Closes the wave-end adversarial spec-review's findings — two conformance-breaking FAILs (green vs Harbor's own fixtures, broken vs a real ext-apps server — the D-216 class) plus host-obligation gaps. **FAIL-1:** the UI capability is advertised as the spec `mimeTypes: ["text/html;profile=mcp-app"]` (the field a conformant server gates on via `getUiCapability(caps).mimeTypes`), NOT the hand-rolled `displayModes` 109h shipped (not a `McpUiClientCapabilities` field). **FAIL-2:** an app→host `tools/call` (bare server tool name) resolves against the calling app's `<serverID>_` namespace, so it hits the right catalog tool AND an app is confined to its own server's tools. Also: the non-spec `displayModes` read off `ServerCapabilities` is removed; display modes move to the spec slot (`ui/initialize` host-context `availableDisplayModes`, sourced from the 109h `display_modes` config via `runtime.info`); and the host honours `ui/notifications/size-changed` (iframe height), graceful `ui/resource-teardown` on unmount, live Console theme + `host-context-changed`, host-context `toolInfo`/`containerDimensions`, and `resources/templates/list`. Sanctioned deviations (D-173 bridge-proxy, D-224 deployment-declaration intent, D-225, D-218) are preserved. The FAIL revert-guards are `HARBOR_LIVE_MCP` probes against a real ext-apps server that gates on `mimeTypes` + exposes a callback tool. **Before merge, the orchestrator live-tests the full MCP Apps surface against the test agent + Console (regression guard — it worked pre-109).** RFC §6.4, §7. Deps: 109a, 109b, 109h, 109i, 109j. See `docs/plans/phase-109k-mcp-apps-conformance-hardening.md`.
- **~~85h — MCP Tasks wire types~~ (CUT).** RC redesigns Tasks (moved to extension; `tasks/list` removed; new method set; new lifecycle around `tools/call` returning a task handle). Hand-transcribing the 2025-11-25 shape now locks in code that the extension SEP and Dockyard's port will both diverge from. Plan file kept as historical context. **Revisit when** Tasks extension SEP stabilizes + Dockyard ports + SDK adds support — refile as a new band, not 85h. **No revisit on this slot.**
- **~~85i — MCP Tasks client~~ (CUT).** Same reasoning as 85h. Polling loop, `tasks/list` consumption, `input_required` → elicitation composition all targeted the old shape. **No revisit on this slot.**
- **85j — MCP client conformance (target: RC).** Conformance harness + scoped, substantiated compliance statement at `docs/design/mcp-compliance.md`. Statement target bumps from **MCP 2025-11-25** to **MCP 2026-07-28 (RC)** and ultimately the final spec. Wording obligation: never "fully compliant" unqualified; the scoped sentence enumerates exactly what's wired. Drops the cut areas (sampling, roots, logging, original Tasks) from the claim; adds the 85m transport / auth / schema / cache / trace items. RFC §6.4. Deps: 85a, 85b, 85d, 85f, 85g, 85m. **Revisit after RC-final** (2026-07-28) and after the dependent phases land. See `docs/plans/phase-85j-mcp-client-conformance.md`.
- **85m — MCP 2026-07-28 RC adoption (NEW).** Absorbs the RC's cross-cutting breaking changes the other phases can't carry on their own:
  - Remove `initialize` / `initialized` handshake plumbing and `Mcp-Session-Id` header dependence from `internal/tools/drivers/mcp` and all transports; client info moves to per-request `_meta`.
  - Streamable HTTP: set `Mcp-Method` and `Mcp-Name` on every outbound request; assert the server's reject-on-mismatch behaviour in tests.
  - Error code flip: every `-32002` (resource-not-found) callsite → `-32602` (Invalid Params).
  - Server-to-client request restructuring: server-initiated requests only issuable while server is actively processing a client request; SSE elicitation polling removed (composes with 85d's rewrite).
  - JSON Schema 2020-12 (SEP-2106): full draft support in tool / resource-template schema validation (composition, conditionals, `$ref`).
  - Cache directives (SEP-2549): respect `ttlMs` and `cacheScope` on list / resource reads.
  - W3C Trace Context propagation (SEP-414): wire Harbor's existing OTel `traceparent` / `tracestate` / `baggage` into MCP `_meta`.
  - Capability discovery via `server/discover` (replaces handshake-time advertisement).
  RFC §6.4. Deps: 28, 85a. **Revisit after SDK-RC** (≈ late Jul–Aug 2026) — every item above needs go-sdk RC support; Harbor's plan can be authored now (transcribe the RC SEPs into a phase plan) so implementation can start the day the SDK lands. New plan file: `docs/plans/phase-85m-mcp-rc-2026-07-28.md` (to author).
- **86 — Durable distributed bus driver.** NATS / Redis Streams / Postgres-as-queue behind `MessageBus`. RFC §12. Deps: 22.
- **87 — Durable TaskService backend.** Background tasks survive restart. RFC §12. Deps: 20, 22.
- **88 — Episodic memory tier.** Durable summaries promoted from session → user/tenant scope. RFC §11 Q-4. Deps: 24, 25.
- **89 — A2A northbound.** Expose Harbor as an A2A server. RFC §11 Q-2. Deps: 29.
- **90 — Additional planner concretes.** PlanExecute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval. RFC §12. Deps: 49.
- **91 — Console-driven key rotation (Protocol).** `governance.rotate_key` Protocol method; `Account` impl atomically swaps the live key set; bifrost picks up the new key on the next `Account.GetKeysForProvider` lookup (no `ReloadConfig` race). RFC §6.15, D-019. Deps: 36a, 60 (Protocol transport), 73 (Console-attaching).
- **92 — Console-driven mid-session model swap.** Two scopes, both next-turn (a D-025 run-start snapshot; never mid-flight) so the model changes live with no redeploy: **session-level** extends the existing `runs.set_overrides` next-turn override with a `Model` field (owner-scoped; reuses the proven projection + verified-id/cross-session-scope auth + Console Playground consumer); **tenant-level admin default** is the new admin-scoped `governance.swap_model` Protocol method (the RFC §6.15 `ModelOverride` governance seam; StateStore-backed tenant default; audited) — the "an admin selects a new default model for the whole tenant without a deploy" workflow. Effective model resolves session › tenant › `cfg.LLM.Model`; the planner sees it via `RunContext`. Unknown model (no `ModelProfile`) fails loud at swap time. Versioning + prompt/tools/skills are 92a. RFC §6.15, §6.5. Deps: 36a, 60, 73. D-231.
- **92a — Agent-config control plane (extends 91/92).** Generalises the 91/92 "mutate desired-state via Protocol, reconcile into the runtime" pattern from governance config to AGENT-DEFINITION config: live, audited control of (a) MCP server-connection enablement — pause / resume / remove — plus per-individual-tool policy (the `active` / `deferred` / disabled `loading_mode` vocabulary from 107c + 26b, made mutable per `<source>_<tool>`); (b) the skill set, over the existing `SkillStore` (Phase 37); and (c) a layered system prompt — an operator-owned base layer plus an optional session-scoped instruction layer composed above it, respecting the 83a–f structured prompt sections. The unifying primitive is a durable, identity-scoped, VERSIONED desired-state registry on the StateStore: each edit is an immutable revision (content-addressed, parent pointer), the active config is a revision pointer, **rollback = repoint**, and a server-side **diff** between revisions is exposed as a read method plus an `agent.config.revised` event. **Next-turn-only, snapshot-immutable semantics (the D-025 alignment):** a change affects ONLY the next run — in-flight / concurrent runs keep the immutable view they snapshotted at run-start; the runtime projects the per-run tool/config view from the registry by extending `tools.NewPlannerView` (Phase 110a) to read desired-state instead of boot config, so there is no mid-flight mutation, no draining, and no forcible teardown (a paused connection's transport may stay warm — pause is a projection-time decision). Two decisions to settle when the band is expanded under §16: (1) app→host `tools/call` callbacks from a rendered MCP App (109i, D-173) are gated against CURRENT desired-state — a paused server rejects them and the host surfaces a "paused by an administrator" advisory — while in-flight PLANNER calls keep their snapshot (an intentional asymmetry); (2) the authorization-scope matrix — base-prompt edits, connection add/remove, and the per-tool allowlist are tenant/deployment-level capability changes requiring an elevated (fleet / tenant-admin) scope plus audit (adding a stdio server is approval-gated / allowlist-only), while session-scoped callers get only the safe subset (the user instruction layer, enable/disable among already-allowed sources, ephemeral skills). Adding a brand-new connection (async dial + `initialize` + OAuth via the unified pause/resume primitive) is the separable hard sub-phase. Decomposes under §16 into: registry + Protocol surface + diff/rollback → skills control → layered prompt → MCP connection pause/resume + per-tool policy → (separable) add-connection. RFC §6.15, §6.16. Deps: 86, 87, 91, 92, 53a, 37, 28, 26b, 110a, 109i.
- **92j — Agent-config: per-agent LLM parameters.** Adds a versioned per-agent LLM-parameters section (model / temperature / max-tokens / reasoning-effort) to the agent-config `ConfigPayload`, correcting the scope of the 92/92b tenant-default override (which set those defaults tenant-wide, one spec for every agent in the tenant). The per-agent layer sits BETWEEN the session override and the tenant-wide baseline, so the effective per-run resolution becomes **session › per-agent › tenant-wide baseline › config default**. The section rides the existing revision machinery for free (content-hash, `set_revision` full-payload + a sibling-preserving `set_llm_params` convenience verb, the server-side `diff` with a new LLM-params arm, `rollback`, the `agent.config.revised` event). Run-start resolution gains a shared `projection.ActiveLLMOverrides` helper called by BOTH the production run loop and the devstack twin (D-094), and `ComposeLLMOverrides` folds the per-agent layer in per-field. Admin-scoped writes only (the D-235 capability tier); the tenant-wide override is RETAINED as the baseline (the operator's decision — additive, not a rescope). A pinned model with no `ModelProfile` fails loud at run-start (parity with the Phase 92 tenant swap), never a silent fallback. RFC §6.15, §6.16, §6.5. Deps: 92a, 92, 92b, 92d, 92e, 110a. D-238.
- **92i — Console: agent-config revision UX.** A pure-Console hardening of the 92h panel's revision experience over already-shipped Protocol surface (92a's `list_revisions` / `diff` / `set_revision` / `rollback`), answering the two operator questions the band surfaced: a **derived per-revision change summary** (which sections changed vs the parent revision, computed client-side from the payloads `list_revisions` already returns — a rollback revision is recognised and labelled, no new round-trips and no stored free-text label on the backend); a **diff-before-rollback** safety gate (rollback renders the structured `agent_config.diff` active→target for explicit confirmation, never a blind repoint); and an **atomic multi-section Save-all** (edits across prompt / skills / MCP policy / model & sampling commit as ONE `set_revision` revision instead of N single-section convenience-verb revisions — "change prompt + temperature + model in one revision"). Built against the D-121 conventions (shared `ui/` inventory, four-state `<PageState>`, tokens-only, typed `AgentConfigNamespace`). Build order: after 92j (so the summary + Save-all + diff-preview cover the LLM-params section). RFC §7, §6.16. Deps: 92a, 92h, 92j. D-239.
- **92k–92q — Runtime MCP tool-side OAuth wave (PLANNING — parked; completes 92f / issue #375).** Closes the two unfinished halves of `agent_config.add_mcp_connection` the faithful way: a runtime-added MCP server authenticates through Harbor's EXISTING agent-bound tool-side OAuth primitive (`internal/tools/auth` — `ScopeAgent`, PKCE, RFC 7591/8414, the sealed agent-bound `TokenStore`, `InitiateFlow`/`CompleteFlow`, and `CompleteFlow`'s already-built pause-resume) rather than a parallel auth path. The wave is staged 4 deep; the umbrella decision is **D-240** (per-phase D-241..D-247 reserved, logged on ship). Full decomposition, staging, and reading lists: `docs/plans/wave-mcp-oauth-decomposition.md`. **This is a planning deliverable — no phase below has begun; all rows are `Pending`.**
  - **92k — `auth.Provider` runtime config registration seam.** `RegisterConfig`/`UnregisterConfig` move the immutable `configs` map behind a documented internally-synchronised mutex (D-025 preserved); the boot path migrates to the seam (the §13 in-phase consumer). RFC §6.4, §3.3. Deps: 30, 50. D-241 (reserved). See `docs/plans/phase-92k-auth-provider-runtime-config.md`.
  - **92l — MCP transport agent-bound OAuth + typed `ErrAuthRequired`.** The MCP driver resolves an agent-bound token via `provider.Token` and injects `Authorization`; a missing token surfaces a TYPED `auth.ErrAuthRequired` (replacing the `looksLikeAuthRequired` string heuristic). Static operator headers still win when present. RFC §6.4, §3.3. Deps: 28, 30, 92k. D-242 (reserved). See `docs/plans/phase-92l-mcp-transport-oauth.md`.
  - **92m — `add_mcp_connection` OAuth config + `InitiateFlow` parking.** The add request gains an optional agent-bound `OAuth` block; the service registers the config (92k), drives the attach (92l), and replaces `parkForAuth` with `provider.InitiateFlow` on a typed `ErrAuthRequired`. Secrets never persisted (§7). RFC §6.4, §6.16, §3.3. Deps: 92f, 92k, 92l. D-243 (reserved). See `docs/plans/phase-92m-add-connection-oauth-flow.md`.
  - **92n — Resume-completes-attach bridge (closes #375 gap 1).** A long-lived agent-config `pause.resumed` subscriber re-drives the attach (under the per-`(tenant, agentID)` write lock) so the server comes `online`; fail-loud on attach failure. Re-instates the "a resume completes the attach" claim in D-237 §2 + the godoc + the 92f plan. RFC §3.3, §6.16. Deps: 92m, 50. D-244 (reserved). See `docs/plans/phase-92n-resume-completes-attach.md`.
  - **92o — Run-start connection reconciliation (closes #375 gap 2).** A shared `ReconcileConnections` projection (both run-loop drivers, D-094) attaches a declared-but-absent connection at run-start, reading the agent-bound token. **Detach-on-rollback is deferred** (pause/resume is the revoke path — D-240). RFC §6.16, §6.4. Deps: 92m. D-245 (reserved). See `docs/plans/phase-92o-connection-reconciliation.md`.
  - **92p — Spec-faithful MCP OAuth discovery.** 401 → `WWW-Authenticate` → RFC 9728 protected-resource metadata → RFC 8414 AS discovery → RFC 7591 dynamic registration → PKCE, so operator OAuth config is optional. Brief 09 + 14. RFC §6.4, §3.3. Deps: 92l, 92m. D-246 (reserved). See `docs/plans/phase-92p-mcp-oauth-discovery.md`.
  - **92q — Console advisory + wave-end live E2E + §17.5 audit.** The Console renders the "awaiting authorization" advisory (extends 92h; pure Protocol client); the wave-end deliverable bundles `test/integration/wavemcpoauth_test.go` (real drivers, identity propagation, `DenyFlow` failure mode, N≥10 stress) + an env-gated `HARBOR_LIVE_*` probe against a real OAuth MCP server (§17.8) + the §17.5 checkpoint audit. RFC §6.16, §6.4. Deps: 92k–92p. D-247 (reserved). See `docs/plans/phase-92q-console-advisory-wave-e2e.md`.
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

## Open architectural follow-ups feeding next-wave scoping

The Wave 11 §17.5 audit (PR #117) surfaced four architectural gaps tracked as GitHub issues. Three closed in Wave 11.5 (issues #112, #113, #114, #115 via PRs #119, #120, #121, #122; the wave-end E2E now exercises production end-to-end). Issue #116 (`tools.oauth_providers[]` operator config) shipped in PR #119 alongside Wave 11.5 Stage A. One open follow-up remains:

- **[#123 — task FSM bridge: translate RunLoop `Finish` into `TaskRegistry.Mark{Complete,Failed}`](https://github.com/hurtener/Harbor/issues/123)**. Surfaced by PR #122 (D-097). Closed in Wave 12 Stage 1 via PR #128 (D-098).
- **[#134 — wire memStore into ControlSurface](https://github.com/hurtener/Harbor/issues/134)**. Surfaced by Wave 12 §17.5 audit N2. `cmd/harbor/cmd_dev.go::bootDevStack` constructs a MemoryStore and currently discards it via `_ = memStore`; when a Protocol method (or RunLoop hook) needs memory, the consumer phase closes the seam.
- **[#135 — preflight wall time: parallelize phase smokes + ephemeral ports](https://github.com/hurtener/Harbor/issues/135)**. Surfaced by Wave 12 audit Recommendations + operator feedback ("preflight is more waiting than dev time"). Four-step plan: random port allocation (unblocks parallel-worktree preflight), classify smokes (`live-server | static-only | unit-tests`), parallel driver for the static batch, CI matrix sharding. Targets ≥50% wall-time reduction. **Recommend scheduling early in Wave 13** — every wave that lands without this added another 10–20s to the gate.

This section accumulates audit-surfaced follow-ups that warrant tracking issues but haven't been promoted to phase plans yet. When the next wave scopes, this is the first list to reconcile against `docs/plans/README.md`'s pending-phase block.

---

## V1 cut line

**V1 ships phases 01–82 + 36a + 36b + 53a.** The follow-ups (83–100) are intentionally deferred to post-V1: the original band (83, 84, 86–90 — integer 85 was removed, see below), six Governance (91–96), three Multimodality follow-ups (97–99) for media-input/output tool wrappers and vision-aware memory summarization, and the Recipe loader (100). Two lettered bands sit inside this range: 83a–e (ReAct prompt depth + reasoning-channel decoupling) and **85a–j + 85m (MCP client/host compliance — the prioritised first post-V1 work; 85k is the separate Harbor agent-builder skills phase)**. The 85-band was re-shaped on 2026-05-28 against the MCP 2026-07-28 RC (sampling / roots / logging deprecated, Tasks redesigned to an extension, session handshake removed); see the 85-band detail block for the per-phase verdict. Multimodal **inputs** ship in V1 (RFC §6.5 + D-021); only multimodal **outputs** and richer memory handling are post-V1. The Evaluations subsystem and code-mode (Starlark) are also post-V1 — see RFC §12.

The cut line is justified by RFC §12 (Out of Scope for V1):

- **Auto-sequence + reflection (83, 84)** — explicit RFC §12 entries: "optional optimization, off by default" and "optional per concrete; not on V1's critical path." Shipping the planner without them does not weaken the swappable-planner property; both can land as planner-internal upgrades without runtime change.
- **MCP client/host compliance (85-band, 85a–j + 85m)** — post-V1 by deferral, not by architecture: the V1 MCP southbound driver (Phase 28) is core-functional; the 85-band raises it to feature-complete. Prioritised as the first post-V1 work. The integer Phase 85 (Skills Portico provider driver) was removed — Portico speaks MCP like any server, so the generic MCP client driver is its consumer; no Portico-specific driver is built. **Per the MCP 2026-07-28 RC re-plan (2026-05-28),** the band scopes as: HTTP OAuth (now covering six RC auth SEPs), elicitation (RC `InputRequiredResult` shape), the surviving server features (completions / templates / progress), MCP Apps host, conformance (target: RC), and a new 85m absorbing the RC's cross-cutting transport / session / error / schema / cache / trace changes. Sampling, roots, the original Tasks pair (85h/85i) are cut.
- **Durable distributed bus + durable TaskService backend (86, 87)** — RFC §6.12 settles "V1 ships contracts only; in-process default." A durable backend is a driver phase, not a runtime-architecture phase. **Phase 87 SHIPPED (D-228):** a `durable` `TaskRegistry` driver (`internal/tasks/drivers/durable`) persists task/group/patch records through the shared `StateStore` (per-record slots, replayed on open) so they survive a restart, with an open-time recovery sweep that fails a crash-left `Running` task to `Failed{runtime_restarted}`. Two recorded deviations from the merged plan: the task lifecycle was extracted into a shared `internal/tasks/engine` package (`inprocess` + `durable` are thin wrappers over one state machine) rather than duplicated; and the driver reuses the runtime's shared `StateStore` (no `StateDriver`/`StateDSN` config fields — `tasks.Open` already passes the store). Opt-in via `tasks.driver: durable`; fail-loud when no `StateStore` is wired. Single-instance restart-survival of records only (no auto-re-drive); the queue-backed / distributed driver remains future work behind the unchanged seam. **Phase 86 SHIPPED (D-229):** a `durable` `MessageBus` driver (`internal/distributed/drivers/durable`) persists every `BusEnvelope` through the shared `StateStore` and projects it onto the local `events.EventBus`, with a background poller that delivers cross-instance envelopes + replays persisted history after a restart (at-least-once; consumers dedupe on `(TaskID, Edge, EventID)`). StateStore-backed (Postgres-as-queue on a shared Postgres store); NATS / Redis Streams deferred (new deps → RFC §10 PR first). The bus-projection contract (`EventTypeDistributedBusEnvelope` / `BusEnvelopePayload`) was promoted from the loopback driver into the `distributed` package so both drivers share it. Opt-in via `distributed.bus_driver: durable`; `loopback` stays the default; fail-loud when no `StateStore` is wired. The `MessageBus` seam itself is still contracts-only in production (no `OpenBus` consumer yet, like `loopback`), so the driver is registered + conformance/integration-tested, ready for a future bus consumer.
- **Distributed task dispatcher (86a)** — the **consumer** that makes the Phase 86 durable bus load-bearing: it wires `OpenBus` into the runtime, publishes task-lifecycle envelopes (`task.spawned` + terminal) to the bus, and runs a fleet RunLoop driver that **claims** a spawned task (a StateStore compare-and-swap lease, so exactly one worker drives it despite at-least-once fan-out) and drives it — turning the durable bus into the fleet work queue (a task spawned on any worker is driven on any worker and survives a restart). It is the distributed evolution of the single-instance per-task RunLoop driver (`cmd/harbor/cmd_dev_runloop.go`). Carries the high-level **multi-worker deployment** topology (N stateless Harbor workers behind a shared Postgres `StateStore` + durable bus + durable tasks; EKS / multi-container). Deps: 86, 87. Without it, the durable bus is registered-but-unconsumed in production — 86a closes the "primitive without a consumer" gap (§13). RFC §6.8 + §6.12, D-230.
- **Episodic memory tier (88)** — RFC §11 Q-4 leans post-V1 unless V1 user feedback demands it.
- **A2A northbound (89)** — RFC §11 Q-2 leans V1.1 unless an early adopter demands it.
- **Additional planner concretes (90)** — RFC §12 explicitly: "wait on V1 evidence that the interface holds." V1 ships React + Deterministic; the rest land as evidence accrues.

If under calendar pressure, **phase 19 (ArtifactStore S3-style)** and **phase 75 (Playwright CI gate)** are the most reasonable V1 → V1.1 slip candidates inside the V1 list, in that order.

### Runtime hardening & observability (119–123)

Origin: a read-only profiling/leak/refactor audit of the Go runtime (2026-06) found the concurrency, D-025 concurrent-reuse, ctx-first, and fail-loudly claims hold up under inspection — no send-on-closed, double-close, WaitGroup misuse, or unguarded per-run mutable artifact state — but surfaced two confirmed unbounded-growth maps and a complete absence of in-process profiling/leak-detection on the *running* binary. An adversarial review of the first-draft plans then caught that the runtime-health Protocol surface already ships (Phase 72f / D-111: `runtime.health`, `runtime.counters`, `metrics.snapshot`, with an existing `live-runtime/health-panel.svelte`), so the originally-planned 121a/121b "new method + new panel" were collapsed into a single Phase 121 that EXTENDS the shipped surface. Sequenced: 119 (correctness) → 120 (observability, must follow 119's bounded maps) → 121 (surface the gauges through the shipped `metrics.snapshot` in the existing panel) → 122 (opportunistic refactor).

- **119 — Runtime retention + ctx hardening.** Reaps the engine streaming-capacity maps when a run *truly* ends — keyed off the real streaming-completion signal or a `completedAt` TTL sweep mirroring the existing cancellation-map sweeper, NOT off `markRunDone` (a per-invocation refcount that cycles 1→0→1→0 as an envelope hops nodes; the struct comment itself defers to "a future run-end signal"); fixes the governance cost + rate-limit caches, which key by `RunID` with no `.Delete` — RFC §6.15 scopes both to *identity*, so dropping `RunID` (from the in-memory map AND the persisted StateStore key) is the RFC-mandated correctness fix, not just a leak patch; makes the rolling-summary recovery loop's summariser call cancellable (it passes `context.Background()`, so a hung summariser that doesn't honour ctx can wedge `Close()` forever) and adds the §11-mandated `NumGoroutine` leak test that was missing; folds in two low-severity cleanups (the no-op `break`-inside-`select` in `MapConcurrent`, the per-iteration `time.After` in the engine `readAny` poll loop). No new operator surface. RFC §6.1, §6.6, §6.15, D-248/D-249/D-250. Deps: 12, 13, 24, 36a, 36b. Plan: `docs/plans/phase-119-runtime-retention-and-ctx-hardening.md`.
- **120 — Runtime observability foundation.** Registers `NewGoCollector` + `NewProcessCollector` on the per-instance registry (built fresh for isolation but with the standard collectors never registered, so `go_goroutines` / `go_memstats_*` / `go_gc_duration_seconds` are absent from the one continuous-monitoring path) — via a NEW registration seam in `internal/telemetry/metrics.go`, since the registry is private with only a read-only `Gatherer` exposed today; adds Harbor runtime gauges (preferably as OTel observable instruments so they also reach OTLP and the shipped `metrics.snapshot`) so Phase 119's bounded maps are observable and regression-guarded; adds `go.uber.org/goleak` to the RFC §10 stack table (dependency gate) then `goleak.VerifyTestMain` in the goroutine-heavy packages; exposes `runtime/pprof` behind a gated loopback `HARBOR_DEBUG_ADDR` listener **deliberately kept off the authenticated Protocol/Console mux** (its own `*http.Server`, not `DefaultServeMux`); reconciles benchmarks with the existing `test/benchmarks/engine_bench_test.go`. RFC §6.14, §10, D-251. Deps: 04, 119, 111f, 72f. Plan: `docs/plans/phase-120-runtime-observability-foundation.md`.
- **121 — Surface runtime gauges in the Console Live Runtime health panel.** The runtime-health Protocol surface ALREADY ships (Phase 72f / D-111) — `runtime.health` (`methods.go:198`), `RuntimeHealth` (a per-subsystem readiness rollup), `runtime.counters`, `metrics.snapshot` — and the Console already has `live-runtime/health-panel.svelte` registered via the 108e capability→panel registry. So this phase does NOT add a method or a panel: it routes the Phase 120 gauges through the shipped `metrics.snapshot` (zero new Protocol method; go-liveness fields, if needed, extend an existing posture type with a §8 version bump) and EXTENDS the existing panel to render them with a trend sparkline. Scope follows the implemented posture gate (`posture.go:301-312` / RFC §5.5), not an invented session-vs-fleet view. RFC §5.2, §7.1, §7.2, D-252. Deps: 72f, 120, 108e, 117, 118, 113a. Plan: `docs/plans/phase-121-console-surface-runtime-gauges.md`.
- **122 — Persistence + driver-registry dedup (opportunistic).** Extracts a shared `internal/persistence/sqlmigrate` runner from the **four conformant** SQLite drivers + the Postgres advisory-lock runner across three (the fifth SQLite copy, `searchcache`, is materially divergent — own table, no `BeginTx`, silent bad-filename skip — and is conformed-first or scoped out, never erased), and a generic `internal/driverreg.Registry[T]` across the fifteen `(registered:)` factories that yields one canonical message FORMAT while wrapping each subsystem's exported `ErrUnknownDriver` sentinel (`errors.Is` still holds). No behaviour change — the per-driver conformance suites are the regression gate; migrations are not edited (§13). Lowest urgency; do when next touching migrations. RFC §9, D-253. Deps: 15, 16, 18, 25, 37, 41, 110c. Plan: `docs/plans/phase-122-persistence-and-driver-registry-dedup.md`.

### Session hydration & user-scope agent-config (124–127)

A V1.6 band that hardens durable-runtime resumability and opens a durable USER-scope agent-config tier for generic Protocol clients. Sequenced in two stages: stage 1 lands the durable-bus resumability fix (124), the windowed event-replay hydration surface that depends on it (125), and the durable user-scope agent-config tier + revision store (126a); stage 2 lands the two projection-only consumers of 126a (126b prompt layer, 126c tool-policy overlay) and the stretch wire-surface digest (127). All additive — `ProtocolVersion` holds at 0.1.0 across the band.

### Phase 124 — Durable event-bus sequence rehydration on restart

**Subsystem:** `internal/events/drivers/durable` (+ a small additive
`internal/protocol/transports/stream` framing change).
**RFC:** §6.13 (gap-free, resumable-across-restart sequence numbering +
the `id:`/`Last-Event-ID` resume cursor), §6.11 (the `ListKind`
maintenance scan used to read the head records).
**Deps:** 06 (replay + cursor), 57 (durable driver), 60 (SSE transport),
12/13 (runtime producers). **Coverage:** 85%. **Decision:** D-255.

Fixes a correctness bug for generic Protocol clients: the durable driver's
in-memory `nextSeq` counter was never rehydrated from the persisted log,
so after a Runtime restart the first `Publish` re-used pre-restart
sequence tokens and a client reconnecting with a high `Last-Event-ID` had
every post-restart event silently skipped by `Replay`. Phase 124
rehydrates `nextSeq` at construction from the per-session head records via
`ListKind(ListScope{MaintenanceScoped: true}, "events.durable.head")`,
taking the global max across sessions, fail-loud on scan/decode error, and
recording the cross-identity scan via structured `slog` (the pause-sweeper
posture). It also closes the same skip class for transient notices:
`publishInternal` assigns the non-replayable sentinel `Sequence == 0` and
stops advancing `nextSeq`, and `stream.encodeEvent` omits the SSE `id:`
line for `Sequence == 0`, so no reconnect cursor can anchor on a transient
tick. `New` gains a leading `ctx` (construction now does I/O). Binding
regression tests: publish AFTER a simulated restart, and make a transient
notice the highest pre-restart emission, asserting no collision and no
skip. No Protocol surface change; `ProtocolVersion` held at 0.1.0
(additive SSE refinement). **Risks:** O(sessions) boot read (bounded;
global-max checkpoint is the noted follow-up); ctx bridged with
`context.Background()` at the ctx-free `events.Register` factory boundary
(threading it through the factory contract is the tracked follow-up).

### Phase 125 — Session state-history windowed event-replay surface

**Subsystem:** internal/protocol + internal/events + web/console
**RFC:** §5.2 (State snapshots), §6.5/§6.10 (heavy-output by reference),
§6.9 (Sessions), §6.13 (durable event log).
**Deps:** 124 (tight — the gap-free durable stream), 72/72a (SSE + durable
event-log driver + `events.Replayer`), 73 (Sessions/Playground surface +
`auth.HasScope`), 58 (single-source), 60 (wire transport), 113a
(protocol-docs gen), 118 (TS lockstep gate).
**Decision:** D-254.

Ships the Pending `state.history` method from RFC §5.2's State-snapshots
row as a TAIL-FIRST WINDOWED read of the durable event stream (NOT memory —
the default `StrategyNone` makes `AddTurn` a no-op, so memory holds no
transcript). Adds the `events.HistoryReplayer` seam (`Bounds` discover
head/tail + `Window` bounded backward read) implemented by the durable +
inmem drivers, and a `state.history` Protocol handler that returns a page of
flat `StateEvent` (heavy content by ROUTABLE `StateArtifactRef`, not
`ArtifactRefSummary`) plus a scroll-up cursor. Reduction stays client-side.
Single-sourced across `methods` / `types`; advertised via a new
`CapStateSnapshots` capability with NO ProtocolVersion bump (additive
minor-class per the `version.go` taxonomy). The §13 first consumer lands in
the same wave through the real boot path: the open-source Console
session-reopen hydration is rewired off its full-load `tasks.list` +
N×`tasks.get` reconstruction to tail-first windowed + scroll-up, wired
through both `cmd/harbor dev` and `harbortest/devstack.Assemble`, with the
LIVE preflight returning OK for the windowed round-trip incl. a routable
artifact ref (resolving to a presigned URL on S3, or the typed
`CodePresignUnsupported`/501 on the default inmem/fs store).

**Risks:** substrate gap-freedom (Phase 124's contract; window reads the
gap-free persisted sequence list, fails loud on a missing entry); retention
vs completeness (surfaced via `Truncated`, not hidden); cross-identity
existence leak (return `CodeNotFound` — pinned to 404 exactly, never 403 or
`CodeScopeMismatch`; admin authority from verified ctx only — D-219); ref
dereference on the default CGo-free stores (inmem/fs are NON-Presigner, so
`artifacts.get_ref` returns the typed 501 — the gate asserts the id ROUTES
well-formed, with the 200-resolves leg gated behind `HARBOR_LIVE_*` S3 env
per §17.8). Status: Shipped (V1.6).

> **Phase 126a — USER-scope agent-config tier (D-256).** Adds the missing
> middle tier of the agent-config authorization matrix and the band's ONE
> durable user-scope write surface: a durable, versioned per-user config
> variant that spans a non-admin caller's own sessions, sitting between the
> admin/tenant durable config (above) and the ephemeral session overlay
> (below). Three pieces land together (primitive + consumer per §13): (1) a
> new closed-set authority scope `agent_config:user` in
> `internal/protocol/auth/scopes.go`; (2) a user-keyed durable revision
> store — the `agentcfg.Registry` gains an `agentcfg.ConfigScope`
> discriminator so the one implementation keys agent-level config under the
> existing synthetic slot (`ConfigScopeAgent`, `agentcfg.*` kinds) and the
> per-user variant under the caller's REAL `(tenant, user)` with `agent_id`
> in the session slot (`ConfigScopeUser`, DISTINCT `agentcfg.user.*` kinds +
> a `__agentcfg__` sentinel rejection so the two key spaces can never alias),
> never widening the isolation tuple; and (3) the in-phase consumer — a
> versioned `agent_config.user.*` verb family
> (get/set_revision/list_revisions/diff/rollback) gated on the new scope,
> with diff/rollback parity to the admin registry and a structurally-bounded
> safe-subset payload (`AgentConfigUserPayload`) carrying the band-complete
> field set (`user_prompt` + `disabled_servers`/`disabled_tools` +
> `personal_skills`) so a user caller cannot widen capabilities or edit the
> operator base. The `ConfigScope` parameter breaks all existing `Registry`
> callers (projection ×4, the sibling protocol verbs, mcpconsole apps); all
> are migrated to pass `ConfigScopeAgent` in the same PR so the tree builds
> green. The two sibling phases are PROJECTION-ONLY consumers of this payload
> (126b projects `user_prompt`; 126c projects the disable set), with no write
> verb of their own. Adding an MCP connection, editing the base prompt,
> widening the allowlist, and model swap stay admin-only and fail-closed.
> Deps 92a, 92g, 116, 61. No `ProtocolVersion` bump (additive methods + wire
> types — Minor-class per `internal/protocol/types/version.go`).

#### Phase 126b — USER-scope durable prompt-layer projection

- **Subsystem:** `internal/runtime/agentcfg/projection` (the run-start
  prompt-layer projection only — no new package).
- **RFC:** §6.16 (agent config read back at run start; `agent_id` is a key,
  not an isolation principal), §5.5 (verified identity keys the read; the
  durable layer was already gated at write time by 126a).
- **Deps:** 126a (the durable USER-scope revision + `ConfigScopeUser` read +
  the `user_prompt` field this projects), 92e (the admin layered prompt +
  `composeUserLayer`), 92g (the session overlay this composes above).
- **Decision:** D-257.
- **What it delivers:** the run-start CONSUMER of 126a's durable `user_prompt`.
  PROJECTION-ONLY — no new store, verb, method, or wire type.
  `ApplyPromptLayers` reads the caller's active `ConfigScopeUser` revision via
  126a's existing registry read and threads `user_prompt` into the existing
  lower-trust `<user_instructions>` block as the MIDDLE segment, in precedence
  order admin Base > admin User > USER-durable > session User;
  `composeUserLayer` goes from two segments to three. One writer (126a's
  `set_revision`), one reader (this projection) — closing the §13
  two-writers/one-reader trap an earlier cut would have created.
- **Risks:** the durable read must key by the run's identity triple
  (`agent_id` in the session slot, never an isolation filter); the §17.6 seam
  is the shared `ApplyPromptLayers` (no signature change, so both twins reach
  the new behaviour through the one function).
- **Status:** Shipped (V1.6).

#### Phase 126c — USER-scope tool-policy run-start projection

- **Subsystem:** agentcfg (run-start tool-exposure projection)
- **RFC:** §6.16 (agent-level capability surface; agent_id is not an isolation principal)
- **Deps:** 126a (durable user-scope tier + ConfigScopeUser read + disable-set fields — consumed), 92d (MCP pause/resume + per-tool policy projection — extended)
- **Decision:** D-258
- **Delivers:** a PROJECTION-ONLY extension of the run-start tool-exposure
  projection (`ActivePlannerCatalogView`): it reads the active USER-scope
  revision via 126a's `reg.Active(..., ConfigScopeUser)` and unions its
  narrow-only disable set into the exclusion set
  (`admin ∪ user ∪ session`, order-independent, grow-only). No new store, no
  new verb, no new authority scope, no binary rewiring — the user disable set
  is persisted + audited at 126a's user tier.
- **Boundary preserved:** adding a NEW MCP connection stays admin-only +
  fail-closed; `agent_id` is a record/key discriminator on the user read,
  never an isolation `WHERE` filter (isolation stays the run's
  `(tenant, user)`).
- **Coverage:** `internal/runtime/agentcfg/projection` ≥ 85% (maintained).
- **Status:** Shipped (V1.6)

### Phase 127 — Protocol wire-manifest consumability (runtime.info digest) — STRETCH

- **Subsystem:** internal/protocol/wiresurface (new sub-package) +
  internal/protocol (additive RuntimeInfo field + handler) +
  cmd/harbor-protocol-ts-lockstep (manifest digest) + web/console
  (app-shell status-bar connect-time consumer).
- **RFC:** §5, §5.2, §5.3 (§5.3 cited only for "bumping the version is an
  RFC change"; the additive-vs-breaking taxonomy is version.go).
- **Deps:** 58 (single-source + CanonicalWireTypes), 118 (lockstep gate /
  D-223), 60 (wire transport), 72f (PostureSurface / runtime.info). NO dep
  on 126a — staged into wave stage 2 for cadence only.
- **What it delivers:** a canonical `wiresurface.Digest()` over the
  name-level wire surface (version + methods + errors + capabilities +
  wire-type names — EXCLUDES field shapes + event-type names); the digest
  returned as an additive `runtime.info.wire_surface_digest` field AND
  stamped into the committed `wire-manifest.gen.json`; a Console app-shell
  status-bar connect-time drift check (the consumer) that surfaces a loud
  signal on a mismatch and an informational note on an absent (old-runtime)
  digest. NO new method, NO version bump, NO field-shape exposure over the
  wire. Generated `types.md` regenerated (D-209).
- **Why STRETCH:** the vendor-and-gate interim needs zero Harbor code, so
  no client is blocked; this adds connect-time (vs build-time) drift
  detection and closes the lockstep mechanism's missing runtime consumer.
  First in the band to cut if V1.6 capacity is tight (recorded cut, D-259).
- **Decision:** D-259.
- **Status:** Shipped (V1.6).

### Phase 128 — Advertise the agent-config control plane as a Protocol capability

- **Subsystem:** internal/protocol/types (the `CapAgentConfig` constant +
  registration) + internal/protocol (the additive `PostureDeps` flag +
  conditional `runtime.info` advertisement) + cmd/harbor + harbortest/devstack
  (the boot-path consumer wiring) + internal/protocol/conformance (the
  handshake set 5→6).
- **RFC:** §5.2 (capabilities are how a client negotiates the §5.2
  surfaces), §5.3 (cited only for "bumping the version is an RFC change";
  the additive-vs-breaking taxonomy is version.go), §6.16 (the agent-config
  control plane this advertises).
- **Deps:** 58 (single-source capability registry), 72f (PostureSurface /
  runtime.info / the `topology_snapshot` conditional-advertisement pattern),
  92a (the agent-config control plane), 127 (the wire-surface digest +
  lockstep gate the manifest regen rides).
- **What it delivers:** a canonical `CapAgentConfig = "agent_config"`
  capability; `runtime.info.capabilities` advertises it IFF the
  agent-config surface is mounted (wired to `WithAgentConfigService` via
  one source-of-truth boolean); a Protocol client gates the `agent_config.*`
  surfaces at attach instead of method-probing. NO new method, error code,
  wire type, or version bump; only the wire-surface digest regenerates.
- **Consumer (§13, same phase):** the runtime-side conditional
  advertisement wired to the real surface mount + the conformance/handshake
  test (advertised iff mounted). A Console capability-gate is the natural
  follow-up.
- **Decision:** D-260.
- **Status:** Shipped (V1.7).

---

### Phase 129 — JWKS max-stale / revocation ceiling

- **Subsystem:** internal/protocol/auth (the JWKSKeySet ceiling + the
  Validator consumer + the wire-reason arm) + internal/config (the
  `identity.jwks_max_stale` field + validation).
- **RFC:** §5.5 (Authentication — the verified-identity edge; the ceiling
  is the fail-closed backstop when the key source is unreachable past the
  bound).
- **Deps:** 115 (production JWKS keyset + `harbor serve` + from_config), 55
  / 56 (JWT core + Validator/KeySet interfaces), 61 (Protocol auth
  middleware mapping).
- **What it delivers:** a configurable max-stale ceiling on
  `auth.JWKSKeySet` — past the ceiling, with refreshes failing, `KeyByID`
  fails closed with a distinct `ErrJWKSStale` instead of serving a
  possibly-revoked key indefinitely; the Validator (same-phase consumer)
  surfaces it as the `jwks_stale` wire reason; a new `identity.jwks_max_stale`
  config field with a documented safe default (1h) + fail-loud validation;
  loud, rate-bounded operator signal via an escalated `slog.Error` + the
  existing `auth.rejected` audit/bus emit. Proven by a controllable-clock
  test (stale → rejected; successful refresh → reset). Bounds — not makes
  instantaneous — revocation; rotation guidance documented.
- **Wire impact:** NONE — no new method / type / error code / capability;
  reuses `CodeAuthRejected`; `ProtocolVersion` untouched; no manifest /
  generated-docs regen.
- **Decision:** D-261.
- **Status:** Shipped (V1.7).

---

### Phase 130 — Session erasure Protocol method (data-lifecycle deletion)

- **Subsystem:** internal/protocol (additive method + error code +
  capability + wire types) + internal/sessions (Registry.Erase + the
  three-store cascade) + internal/state (DeleteScope cascade primitive,
  conformance parity) + web/console (hand-mirrored TS types; Console
  delete-chat UI is a follow-on consumer).
- **RFC:** §5.2 (the erasure surface alongside `artifacts.delete`), §6.9
  (the GC never-reap-running invariant mirrored as a fail-loud refusal +
  the hard-deleted session record), §6.11 (the StateStore cascade
  primitive), §6.13 (the redacted `session.erased` event), §7 (the audit
  trail). §5.3 cited in prose only for "bumping the version is an RFC
  change" (this stays additive).
- **Deps:** 08 (sessions + RunningProbe), 11 (StateStore + conformance), 17
  (memory Flush), 18 (artifacts List/Delete), 58 (single-source +
  CanonicalWireTypes), 60 (wire transport), 61 (auth scopes).
- **What it delivers:** the additive identity-scoped `sessions.delete`
  method that deletes a session and cascades deletion of its scoped State +
  Memory + Artifacts; a fail-loud `session_running` (409) refusal on a
  RUNNING task; an own-session-only scope contract (a caller erases only
  their own verified triple; no admin / cross-tenant path); a new
  `StateStore.DeleteScope` primitive with in-mem/SQLite/Postgres conformance
  parity; a redacted audited `session.erased` event (content-free, emitted
  under the actor scope, not re-persisted under the erased identity); a
  negotiable `CapSessionLifecycle` capability. Consumer = the real three-store
  cascade handler exercised by an E2E test (delete → inspect-404 + cross-store
  erasure + foreign-target 401 + running-task 409).
  NO ProtocolVersion bump. Generated Protocol docs regenerated (D-209).
- **Decision:** D-262.
- **Status:** Shipped (V1.7).

---

### Phase 139 — Public-site honesty sweep

- **Subsystem:** docs/site (the VitePress marketing landing surface
  `landingSpec.ts`) + cmd/harbor (a `_test.go` godoc comment). Docs-only —
  no runtime, Protocol, or config surface.
- **RFC:** §7.1 (the runtime-lens principle — the Console / landing surface is
  a Protocol client; the figures it renders track the real canonical Protocol
  surface), §7.3 (Console binding conventions — the published surface stays in
  lockstep with the canonical method/event/error sets).
- **Deps:** 138 (the `.go`-reload honest-WARN fix that makes the
  config-reload qualification accurate). Lands first in the wave.
- **What it delivers:** corrects three stale claims on the public marketing
  landing surface and one stale godoc reference. (1) `landingSpec.ts` Protocol
  section + stats grid advertise **110** canonical methods (was 109), with the
  `at v1.6` qualifier dropped — the count is current, pinned by
  `internal/protocol/methods/methods_test.go` and `docs/site/protocol/methods.md`
  (the +1 is `sessions.delete`, shipped v1.7.0). (2) The `harbor dev`
  hot-reload claim is qualified to config/YAML reload (the genuine
  capability) — `.go` reload is honest-WARN-only after phase 138. (3) The
  cosmetic, never-printed `3 drivers registered` dev-banner fragment is
  removed. (4) `cmd/harbor/cmd_dev_hot_reload_test.go` godoc is repointed at
  the in-package real-bus tests
  (`TestHotReloadSupervisor_FileChangeTriggersRebuild` /
  `…RebuildEmitsCompletedOnNewBus`) instead of the non-existent
  `test/integration/phase65_hot_reload_test.go` (`cmd/harbor` is `package
  main`, so the real-bus coverage stays in-package per §17.2). Dropped (strings
  verified absent): no "in under a second" softening, no README hot-reload edit,
  no README serve change. Gate = `scripts/smoke/phase-139.sh` static honesty
  greps + the VitePress build.
- **Decision:** — (docs-only; no decision entry).
- **Status:** Shipped (V1.8).

---

### Phase 131a — Production identity setup guide

- **Subsystem:** docs/site/protocol (the new adopter-path guide) + docs/skills
  (the §18 same-PR skill forward-pointer). Docs-only: no Go package, no
  Protocol surface, no config schema touched.
- **RFC:** §5.5 (Authentication — the Protocol's JWT verification surface; the
  identity triple lives in the claims; the Protocol rejects any request without
  an identity scope), §4.2 (mandatory identity — the triple fails closed), §8
  (CLI layer — `harbor serve` is the production verifier; the no-IdP
  `harbor token` on-ramp this guide forward-references is a CLI subcommand).
- **Deps:** 130 (the v1.7 band closing before the wave; the auth/JWKS surface
  this guide documents is shipped). No code dependency on sibling 131 phases.
- **What it delivers:** `docs/site/protocol/production-identity-setup.md` — the
  P0 that leads the v1.8.0 Adopter-Path wave, closing the serve-attach cliff
  documentation-first. It documents the claim shape `serve`'s verifier enforces
  (lifted from the authoritative parser `internal/protocol/auth/auth.go`, not
  the illustrative godoc comment): the mandatory `(tenant, user, session)`
  triple + optional `scopes`, plus the `iss`/`aud` exact-match contract `serve`
  hard-rejects on mismatch (`Config.Validate` mandates a non-empty
  issuer/audience for the serve profile, so the verifier's optional iss/aud
  checks become mandatory at runtime). It walks OIDC app registration with
  Auth0 / Okta / Keycloak / Cognito custom-claim snippets, a mint-and-test
  loop (obtain → decode-and-verify → `runtime.info` handshake → attach), and
  **both** attach on-ramps: a real IdP (this guide + the 131c worked OIDC
  client) and the no-IdP **`harbor token`** self-issuing path (131d) with an
  honest single-issuer-grade callout. Same-PR wiring: the VitePress nav entry +
  the `phase-103.sh` page-mapped assertion; links from `auth-and-identity.md`
  and `build-a-client.md`; and the §18 forward-pointer in the
  `use-the-harbor-protocol` skill's token step. **`harbor serve`'s verifier is
  untouched — D-220 is preserved**, not superseded; first-attach is solved by
  two documented on-ramps, never by minting inside serve.
- **Decision:** D-263.
- **Status:** Shipped (V1.8).

### Phase 131b — configure-production-identity skill

- **Subsystem:** docs/skills (the operator skill) + docs/site/skills (the
  Phase-103 include stub + nav). Docs-only: no Go package, no Protocol surface,
  no config schema touched.
- **RFC:** §5.5 (Authentication — the JWT verification surface the skill teaches
  operators to satisfy), §4.2 (mandatory identity — the `(tenant, user, session)`
  triple, reflected in the skill's claim-shape table), §8 (CLI layer — `harbor
  serve` is the production verifier; the `harbor token` on-ramp the skill
  documents is a CLI subcommand).
- **Deps:** 131a (the production identity setup guide this skill operationalizes
  and links to as the authority). No code dependency on sibling 131 phases —
  131c (worked OIDC client) and 131d (`harbor token`) are forward-referenced.
- **What it delivers:** `docs/skills/configure-production-identity/SKILL.md`
  (`metadata.surface: protocol`) — the Claude-Code-style operator playbook that
  turns the 131a guide into a fast, copy-pasteable path: the `serve`
  verification contract, the `(tenant, user, session)` + `scopes` claim shape,
  the `iss`/`aud` exact-match contract (the most common production 401), OIDC app
  registration, a mint-and-test loop, and **both** attach on-ramps — a real IdP
  and the no-IdP **`harbor token`** self-issuing path with an honest
  single-issuer-grade callout. Same-PR wiring: the `docs/skills/INDEX.md` entry,
  the `docs/site/skills/configure-production-identity/` include stub, the
  VitePress nav entry, and `scripts/smoke/phase-131b.sh` (static-only) pinning
  presence + frontmatter + on-ramps + claim shape + site mirror. The Phase-103
  skill→page-mapping gate (`phase-103.sh`) and the VitePress dead-link gate are
  the binding checks; the §18 skill-frontmatter audit runs under
  `make drift-audit`. Docs-only — no runtime change.
- **Decision:** None (operator skill — no D-NNN).
- **Status:** Shipped (V1.8).

---

### Phase 132 — Embed production runner (`RunOnce`) + `NewRunContext` factory

- **Subsystem:** internal/runtime/runctx (the shared RunContext factory),
  internal/runtime/assemble (the `Stack.RunOnce` runner), sdk
  (facade aliases), examples (the checked-in worked example).
- **RFC:** §3.6 (the public SDK facade — `sdk/assemble` + `sdk/runctx`
  aliases), §6.2 (Planner interface + `RunContext` population), §6.4
  (the Runtime owns the run loop / tool dispatch).
- **Deps:** 110d (`assemble.Assemble` + the `Stack` shape), 110c (the
  production driver aggregator).
- **What it delivers:** the production one-call goal runner
  `Stack.RunOnce(ctx, goal, identity, opts ...RunOption)` — a SINGLE
  blocking method (no sync/async split, no `Sync` suffix) that turns a
  goal + identity into the canonical `planner.AnswerEnvelope`, closing
  the embed adopter path's ~15–27-line per-run ceremony. Its primitive
  (same phase, §13) is `runctx.NewRunContext(ctx, src, quad, goal,
  opts...)`, the ONE shared factory that COMPOSES the existing run-loop
  projection helpers (`FetchMemoryBlocks`, `ProjectSkillsDirectory` over
  `Directory.View`, `ResolveInputArtifacts`, the bus chunk publisher) —
  never a third hand-rolled construction site — proven by a parity test
  asserting field-equality with each helper called directly. The
  dev-driver bodies are NOT rewritten (they thread control-plane
  projections the headless factory omits) but call the same helpers.
  Identity is mandatory and fails loud; a non-runnable stack returns
  `ErrNotRunnable`. `sdk/assemble` + `sdk/runctx` facade aliases. An
  N≥100 concurrent-reuse `-race` test against one shared `Stack` pins
  no context bleed / cross-cancellation / goroutine leak (D-025).
  `docs/recipes/embed-harbor-headless.md` gains step 4a (the shorthand;
  the manual 4b path stays); `examples/embed-runonce/` is the
  smoke-compiled worked example. The `WithStream` streaming sink is the
  sibling 132-stream phase (D-266); `RunOption` is functional-option-
  shaped so it lands without a signature change. Gate: extends
  `scripts/smoke/phase-112b.sh` (no new smoke file).
- **Decision:** D-265.
- **Status:** Shipped (V1.8).

---

### Phase 138 — Hot-reload Go-source honesty

- **Subsystem:** cmd/harbor (the `harbor dev` hot-reload supervisor;
  classification + WARN routing only — no Protocol / config / wire surface).
- **RFC:** §8 (`harbor dev` and the CLI surface).
- **Deps:** 65 (the hot-reload supervisor this corrects).
- **What it delivers:** the watcher now CLASSIFIES every fsnotify event. A
  Go-source (`.go`) change routes to a WARN-and-guide path — it logs "Go source
  change detected — run `make build` and restart" and does NOT reboot or emit
  `dev.hot_reload.completed`. Previously a `.go` edit drove an in-process
  `bootDevStack` rebuild that re-read config without recompiling the binary yet
  emitted `dev.hot_reload.completed{Success=true}` — a loud false-success
  (the inverse of the §13 no-silent-degradation rule). Config / YAML / scaffold
  changes keep the in-process rebuild path unchanged. The dangling
  `cmd_dev_hot_reload.go` package-doc sentence ("This is documented in.") is
  completed to name the dev-loop recipe, which gains the `.go`-changes caveat.
  The optional `policy: rebuild-binary` (auto `go build` + re-exec) is
  **deferred** — WARN + guidance only. Gate: `scripts/smoke/phase-65.sh`
  performs a live `.go` edit against the running preflight dev server and
  asserts the WARN fires and no rebuild fires (a static `strings` grep cannot
  distinguish this — the `completed` string stays in the binary for the YAML
  path). The in-package `cmd_dev_hot_reload_test.go` pins both halves (live
  `.go` WARN-no-rebuild + YAML-still-rebuilds).
- **Decision:** D-268.
- **Status:** Shipped (V1.8).

---

### Phase 131c — Worked OIDC client + serve round-trip smoke

- **Subsystem:** examples/protocol-clients (the new worked client) +
  test/integration (the binding round-trip + the reusable mock-OIDC
  fixture) + scripts/smoke. No Go package other phases depend on, no
  Protocol surface, no config schema touched.
- **RFC:** §5.5 (Authentication — the JWT verification surface the worked
  client's token is checked against; the identity triple lives in the
  claims), §5.4 (Wire transport — the `POST /v1/control/{method}` REST
  surface `runtime.info` is called over), §8 (CLI layer — `harbor serve`
  is the production verifier the example attaches to).
- **Deps:** 131a (the production-identity setup guide whose IdP on-ramp
  this example realizes). Code-wise it depends only on the already-shipped
  `harbor serve` JWKS verifier and the control transport.
- **What it delivers:** the load-bearing P0 **consumer** of the wave —
  `examples/protocol-clients/oidc-client-example/`, a SDK-free,
  stdlib-only worked client that runs the OAuth2 client-credentials grant
  (`client_secret_basic`) against an IdP token endpoint, obtains a JWT,
  decodes the granted scopes for the operator, and calls `runtime.info`
  against `harbor serve` — a 200 proving the production JWKS path is live.
  The binding gate `scripts/smoke/phase-131c.sh` drives the full
  production leg: a hermetic in-test ES256/JWKS mock-OIDC issuer mints a
  token the REAL parser accepts (it signs with the same `golang-jwt`
  library and publishes a JWK in the exact shape
  `internal/protocol/auth/jwks.go` parses — CLAUDE.md §17.8, not a
  self-consistent lookalike), `harbor serve` boots pointed at that JWKS,
  the example obtains a token and `runtime.info` returns OK with the
  granted scope, and a failure-mode leg presents a token signed by a key
  the JWKS does not publish and asserts the verifier rejects it. The smoke
  also mechanically enforces the SDK-free guarantee (`go list -deps`
  carries no `sdk/` or `internal/` Harbor package). **`harbor serve`'s
  verifier is untouched — D-220 is preserved**; the mock-OIDC issuer drops
  to a test fixture (not a user-facing quickstart — that role is 131d's
  `harbor token`).
- **Decision:** reaffirms D-263 (no new decision).
- **Status:** Shipped (V1.8).

---

### Phase 131d — `harbor token` bring-your-own-issuer subcommand

- **Subsystem:** cmd/harbor (the new `token` subcommand + its keygen/mint
  crypto), plus RFC §8 (subcommand enumeration), examples/serve.yaml, and
  docs/site/protocol (the 131a guide's On-ramp B body). No change to
  `internal/protocol/auth` or `cmd/harbor/cmd_serve.go`.
- **RFC:** §5.5 (Authentication — the minted JWT carries the mandatory
  `(tenant, user, session)` triple the verifier rejects a token without, signed
  with an asymmetric algorithm on the allowlist), §8 (CLI layer — a new
  top-level subcommand; the §8 enumeration is updated in the same PR per the
  binding RFC-surface rule). Security posture follows CLAUDE.md §7.
- **Deps:** 131a (the production-identity guide ships the claim-shape docs and
  the On-ramp B placeholder this phase fleshes out).
- **What it delivers:** the no-IdP on-ramp of the serve-attach resolution. A new
  `harbor token` subcommand with two verbs: `keygen` generates an asymmetric
  keypair (ES256 default; RS256 opt-in — both on the §5.5/§7 allowlist), writes
  `private.pem` (mode 0600, parent dir 0700; refuses overwrite without
  `--force`; stderr "keep this out of version control" warning) and a public
  RFC-7517 `jwks.json` whose `kid` is the RFC-7638 JWK thumbprint (hand-written
  stdlib emitter — the `internal/protocol/auth` JWKS surface is consumer-only);
  `mint` self-issues a Harbor JWT with the claim shape the authoritative parser
  (`internal/protocol/auth/auth.go`) enforces, signed with the keypair, with
  `--issuer`/`--audience` **mandatory** and required to equal the operator's
  `identity.issuer`/`identity.audience` (serve 401s a mismatch). Least-privilege
  defaults: **no** scopes unless `--scopes`, short `--ttl` (1h) echoed to
  stderr. The signer reuses **only** the dev path's JWT claim shaper
  (`harborClaims`); it is otherwise a distinct persistable,
  issuer/audience-parameterized signer with net-new keygen / PEM I/O / JWK-emit
  / RS256 branch (the dev signer has none of these and never touches disk). The
  operator points `identity.jwks_file` at the emitted JWK Set and attaches.
  **`cmd/harbor/cmd_serve.go` is unmodified — serve still mints nothing
  (D-220 preserved, not superseded)**: it trusts the self-issued key only
  because the operator configured `jwks_file`, identical to pointing at an
  external IdP. The §17.8 gate is a real-parser round-trip: the keygen-produced
  JWK Set is loaded by the REAL `auth.JWKSKeySet` + `auth.NewValidator`, which
  accepts a matching minted token and rejects a mismatched-`iss`/`aud` token
  (the edge's 401). Honesty callout: single-issuer / eval grade — graduate to a
  real IdP for multi-user SSO.
- **Decision:** D-264 (cross-references D-220 and names the tension: the
  self-issuing single-key posture is bounded to an explicit operator opt-in — a
  subcommand run deliberately, never a silent runtime default — so §13's
  no-stub-default / dev-only-escape-hatch rules are satisfied without a runtime
  banner).
- **Status:** Shipped (V1.8).

---

### Phase 132-stream — `WithStream` sink on `RunOnce`

- **Subsystem:** internal/runtime/assemble (the `WithStream` option +
  sink wiring on `Stack.RunOnce`), sdk (facade re-exports).
- **RFC:** §3.6 (the public SDK facade — `sdk/assemble` re-exports
  `WithStream` + `StreamEvent`), §6.2 (Planner interface + `RunContext`
  population — the `OnChunk` streaming callback the sink taps), §6.4 (the
  Runtime owns the run loop / tool dispatch — the `OnToolDispatched` hook
  the sink taps).
- **Deps:** 132 (`Stack.RunOnce` + `runctx.NewRunContext` + `RunOption` /
  `runOnceConfig`).
- **What it delivers:** `assemble.WithStream(func(StreamEvent)) RunOption`
  — ONE streaming sink on the SAME blocking `Stack.RunOnce`. `RunOnce`
  still blocks and returns the terminal `planner.AnswerEnvelope`; the
  sink receives `StreamEvent`s — token deltas, planner-step boundaries,
  and tool dispatches — as they occur. The fix the phase exists for: *an
  agent framework without first-class streaming is a 2026 adoption
  blocker.* The sink rides the EXISTING run-loop seam, fired
  SYNCHRONOUSLY on the run goroutine: `planner.RunContext.OnChunk` (token
  deltas + the per-LLM-call `done=true` step boundary) and
  `steering.RunSpec.OnToolDispatched` (successful tool dispatch). Because
  both fire inline before `RunOnce` returns, "chunks arrive before the
  final envelope" is DETERMINISTIC, not racy. The wiring is additive — it
  WRAPS any bus-backed chunk publisher `NewRunContext` already installed,
  so a Console on the run's event bus and an embed sink both observe the
  chunks. A new minimal public `StreamEvent` type carries three sealed
  kinds (token / tool_dispatched / step); `sdk/assemble` re-exports
  `WithStream` + `StreamEvent` + the kind constants. NO separate
  `internal/runtime/streaming` package on the `RunOnce` path, NO
  sync/async method split (a second `RunOnceStream` would be a parallel
  implementation of the same feature — §13 forbids it), NO signature
  change (the functional-option `RunOption` absorbs the knob). §13
  primitive-with-consumer: an end-to-end test asserts ordered token +
  step chunks arrive before the envelope; the N≥100 concurrent-reuse
  `-race` test is extended so each run gets its own sink and asserts no
  cross-run chunk bleed. Gate: extends `scripts/smoke/phase-112b.sh` (the
  streaming leg) — `scripts/smoke/phase-132-stream.sh` is a thin
  delegating skip pointing at it.
- **Decision:** D-266.
- **Status:** Shipped (V1.8).

---

### Phase 134 — `sdk` `Example_` functions

- **Subsystem:** sdk (the public facade — `_test.go` example files only;
  no production code changes).
- **RFC:** §3.6 (the public SDK facade — these are the facade's first
  godoc-rendered worked examples, the adopter's first contact on
  pkg.go.dev), §6.2 (Planner interface — the swappable-planner driver
  registry the `sdk/planner` example exercises), §6.3 (steering
  authn/authz — the scoped control the `sdk/steering` example submits),
  §6.4 (the Runtime owns the run loop — the `sdk/assemble` examples drive
  the assembled `Stack.RunOnce`).
- **Deps:** 132 (`Stack.RunOnce` + `RunOption` + `ErrNotRunnable` + the
  `sdk/assemble` aliases), 132-stream (`WithStream` + `StreamEvent` +
  `StreamEventKind`).
- **What it delivers:** the FIRST `_test.go` files under `sdk/` —
  runnable, deterministic, offline `Example_*` functions across the four
  primary adopter surfaces so the curated facade stops rendering an
  empty first-contact page on pkg.go.dev. `sdk/assemble` ships the golden
  one-call path (`Assemble` → `Stack.RunOnce`, `// Output: goal`) and a
  `WithStream` variant whose sink reassembles the streamed content tokens
  to the terminal answer (`// Output: true`). `sdk/config` shows
  `Defaults` → `ValidateCore` (the headless validation profile a Go
  embedder uses when it never serves the Protocol edge, `// Output:
  inmem`). `sdk/planner` shows the swappable-planner driver registry
  (`RegisteredDrivers` after blank-importing `sdk/planner/react`, plus
  the canonical `FinishReason` vocabulary, `// Output: [react]` then
  `true`). `sdk/steering` shows the per-run control inbox round-trip
  (`NewRegistry` → `Open` → `Enqueue` a scoped `CANCEL` → `Drain`,
  `// Output: CANCEL`). The examples are mock-LLM-backed for determinism
  and zero network; every example BODY imports only the public `sdk/`
  facade (+ stdlib) — the dev-only mock LLM (D-089) is the single allowed
  test-file blank-import exception, in `sdk/assemble`, mirroring
  `internal/runtime/assemble/runonce_test.go`. Gate: a thin static
  `scripts/smoke/phase-134.sh` pins the example functions present, the
  non-assemble example files internal-free, and `go test ./sdk/... -run
  Example` green under `-race`.
- **Decision:** none — examples carry no design decision.
- **Status:** Shipped (V1.8).

---

### Phase 137 — Conformance worked example (in-tree)

- **Subsystem:** examples/protocol-clients (the `conformance-fork` worked
  example), docs/site/protocol (the certification-page pointer).
- **RFC:** §5.1 (the Console/Protocol decoupling rule the suite certifies a
  fork still honours), §5.2 (what the Protocol exposes — the surface the suite
  exhaustively exercises across both consumer profiles), §5.3 (versioning — the
  version + capability handshake the suite asserts).
- **Deps:** 62 (the Protocol conformance suite — `Factory` / `Stack` /
  `RunSuite`), 113b (the conformance-certification page this example is pointed
  at; D-210).
- **What it delivers:** a runnable, in-tree worked example of certifying a
  Protocol-server fork (or embedder assembly) against Harbor's conformance
  suite. The fix the phase exists for: *there is no runnable worked example of
  wiring a custom `Factory` + `RunSuite`* — the cert page shows only the
  one-line `NewDefaultFactory` gate, leaving a fork to reverse-engineer the
  seam. **Scope correction:** the conformance suite stays under
  `internal/protocol/conformance` — it is **deliberately not externally
  importable** (D-210), and `RunSuite` is `*testing.T`-bound, so the example is
  a **`go test`-compiled `_test.go` harness**, NOT a runnable client binary
  like the SDK-free `event-viewer`. `examples/protocol-clients/conformance-fork/`
  ships a `doc.go` narrative + a `_test.go` that wires a CUSTOM
  `conformance.Factory` — assembling its own real-driver runtime surface (event
  bus, state store, task registry, control surface, wire mux, ES256 validator,
  the four token-minting closures) — and hands it to `conformance.RunSuite`.
  The suite running green over that custom assembly IS the gate (§17.8): a
  mis-wired surface fails it, never silently. The certification page
  (`docs/site/protocol/conformance-certification.md`) gains a pointer to the
  worked example explaining why it is a test harness, not a binary. It
  **reinforces** the already-documented in-tree Factory-seam posture (D-210
  honest) — it does **not** make the suite externally importable (that would be
  a §3-layout / RFC change this wave does not propose). Gate:
  `scripts/smoke/phase-137.sh` — file presence + custom-Factory pins +
  cert-page pointer + the `go test ./examples/protocol-clients/conformance-fork/...`
  execution leg.
- **Decision:** Reaffirms D-210 (no new D-NNN).
- **Status:** Shipped (V1.8).

---

### Phase 136 — MCP agent-calls-tool integration test

- **Subsystem:** test/integration (the new E2E), scripts/smoke (the
  name-pinning + no-match-fails guard). Test-only; no production code
  changes.
- **RFC:** §6.4 (Tool catalog and transports + code-level tool dispatch
  — the runtime, not the provider, parses the planner's reply, dispatches
  the tool, and formats the observation back into the next prompt; the MCP
  southbound is one transport behind the same `Tool` abstraction), §6.2
  (Planner interface / Trajectory / RunContext — the agent-call loop the
  test drives).
- **Deps:** 83g (MCP southbound wired into the dev boot path; the
  `cmd/harbor-mcptest-stdio` fixture + devstack MCP-server config), 83l
  (the scripted-bifrost devstack harness — real LLM driver against a
  scripted OpenAI-compatible server — this test reuses).
- **What it delivers:** the missing verification that the **agent-call
  leg** works for an MCP-sourced tool. The existing 83g test
  (`…ReachTheCatalog`) proves a configured stdio MCP server is spawned at
  boot and its tools reach the catalog — the discovery leg. The existing
  executor-level test (`phase83l_real_bifrost_test.go`) drives a goal
  through planner → executor → trajectory but dispatches an in-process
  BUILTIN (`text.echo`), not an MCP tool. Neither proves a planner can
  decide to call a tool discovered from a real MCP server and have the
  runtime dispatch it through the executor to the live subprocess. The
  net-new `TestE2E_Phase83g_MCPAgentCallsTool` boots the real
  `cmd/harbor-mcptest-stdio` server (its `echo` tool lands as
  `mcptest_echo`), assembles a devstack with it in config, and drives a
  goal whose scripted LLM decides to call `mcptest_echo`. The
  dispatch-through-executor signal is the echoed sentinel appearing in the
  CallTool step's OBSERVATION (the executor's result) — NOT its input args
  — and in the SECOND LLM prompt (the observation round-trip); a
  catalog-listing test produces no observation and so cannot pass. Real
  drivers everywhere on the seam (bifrost LLM driver, EventBus, StateStore,
  Coordinator, tools catalog, `mcpdrv.Provider` against the real stdio
  subprocess; §17.8 — a real server over the real wire, not a hand
  fixture). Identity propagates end-to-end: the MCP driver fails closed
  (`ErrIdentityMissing`) without the triple, so a successful echo proves
  the triple reached the MCP `_meta`; a cross-identity `Tasks.Get`
  rejection (`ErrNotFound`) pins the isolation half. ≥1 failure mode: a
  wrong-typed argument is dispatched, rejected server-side, surfaced
  through the executor as an error observation, and the planner re-plans.
  `-race` is the gate. `scripts/smoke/phase-136.sh` pins the EXACT test
  name and runs `go test -list '^TestE2E_Phase83g_MCPAgentCallsTool$'`,
  failing loud unless EXACTLY one test matches — closing the original
  `…Phase83g.*Call`-matched-ZERO false-green (a SKIP that should have been
  an OK).
- **Decision:** none (test-only phase).
- **Status:** Shipped (V1.8).

---

### Phase 135 — TS wire-type generator + `event-viewer-ts`

- **Subsystem:** cmd/harbor-protocol-ts-types (the generator),
  examples/protocol-clients/event-viewer-ts (the worked consumer),
  Makefile (the gen targets), docs/skills + docs/site (the §18 surfaces).
- **RFC:** §5 (the Harbor Protocol — the canonical contract the generated
  module projects), §5.3 (versioning — the module pins `PROTOCOL_VERSION`
  and carries the `WIRE_SURFACE_DIGEST` for skew detection), §3.6 (the
  public SDK / adopter-facing client surface).
- **Deps:** 113b (the `examples/protocol-clients/` worked-client pattern +
  the Go `event-viewer` this mirrors), 118 / D-223 (the manifest gate +
  reserved name this is deliberately distinct from and must not disturb).
- **What it delivers:** the missing typed-TS on-ramp for a non-Console
  client. `cmd/harbor-protocol-ts-types` reflects over
  `internal/protocol/singlesource.CanonicalWireTypes` (and the canonical
  method / error-code / event-type sets) and emits a single self-contained,
  dependency-free external-client TypeScript module —
  `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts`: one
  `interface` per canonical wire type, `HarborMethod` / `HarborErrorCode` /
  `HarborEventType` string-union types, and the pinned `PROTOCOL_VERSION` /
  `WIRE_SURFACE_DIGEST` constants. It carries **types only, no client
  logic**, so a third-party client copy-vendors the one file. New make
  targets `protocol-ts-types-gen` / `protocol-ts-types-gen-check`
  (regenerate + `git diff --exit-code` + the Go lockstep tests) gate it,
  ADDITIVE to and NON-interfering with the D-223 Console gate
  (`cmd/harbor-protocol-ts-lockstep`, `wire-manifest.gen.json`,
  `protocol-ts-gen[-check]`) — none of which this phase touches. The
  reserved `cmd/harbor-gen-protocol-ts` name stays reserved for the FULL
  Console-`protocol.ts` generator, which stays deferred (D-132); this phase
  PARTIALLY retires D-132 for external clients only. §13
  primitive-with-consumer: the worked `event-viewer-ts` client (the
  TypeScript sibling of the Go `event-viewer`, runs under Node
  `--experimental-strip-types`, vendors the generated module, SDK-free)
  round-trips `runtime.info` against the dev runtime in the phase smoke.
  §18 same-PR doc drift: `use-the-harbor-protocol/SKILL.md` (the three
  TS-generation assertions, lines 17 / 286 / 354) and
  `docs/site/protocol/build-a-client.md` (the "doors up" section) are
  updated to reflect the new external-client emitter while noting the full
  Console generator stays deferred. Gate: `scripts/smoke/phase-135.sh`
  (static surface + non-mutating gen-check + manifest non-interference +
  live `event-viewer-ts` probe).
- **Decision:** D-269.
- **Status:** Shipped (V1.8).

---

### Phase 140 — Wave E2E + v1.8.0 checkpoint audit

- **Subsystem:** test/integration (the composing wave-end E2E),
  scripts/smoke (the static name-pinning gate). Audit-only / test-only —
  ships no production code.
- **RFC:** §5.4 (the wire transport the `harbor serve` Protocol leg
  exercises), §5.5 (authentication — the JWKS-verified attach both on-ramps
  prove), §6.4 (the tool catalog + transports the MCP dispatch leg drives),
  §8 (the CLI layer — `harbor serve` / `harbor token` the Protocol legs
  boot).
- **Deps:** 131c (the hermetic mock-OIDC issuer + `bootServe` round-trip
  harness), 131d (the `harbor token` keygen/mint CLI), 132 + 132-stream
  (`Stack.RunOnce` + the `WithStream` sink), 133 (scaffold-with-tools
  execution — gated by its own smoke), 135 (the TS generator — gated by its
  own smoke), 136 (the MCP agent-call harness this reuses), 137 (the
  conformance worked example — gated by its own smoke), 138 (hot-reload
  honesty — gated by its own smoke). The import graph reuses 131c / 131d /
  132 / 132-stream / 136; the docs-/audit-only siblings (131b, 134, 139) are
  gated by §6 staging, not by 140's imports.
- **What it delivers:** the §17.7-step-5 wave-end composing E2E,
  `test/integration/wave_v18_test.go` (named for the `wave_v17_test.go`
  precedent), proving the three advertised adopter paths are alive TOGETHER
  on a single suite with real drivers on every seam, under `-race`. The
  legs: **EMBED** — a real assembled `Stack` runs a goal through
  `Stack.RunOnce`, plus a `WithStream` variant asserting ordered streamed
  chunks arrive BEFORE the final envelope (the synchronous-seam ordering
  contract, D-266). **PROTOCOL** — BOTH on-ramps that close the v1.8.0
  serve-attach cliff, each against a real `harbor serve` subprocess whose
  JWKS verifier is untouched: the **binding** hermetic ES256/JWKS mock-OIDC
  issuer → serve → `runtime.info` OK with the JWT-decoded granted scope
  (131c), AND the **bring-your-own** leg — `harbor token keygen` → serve's
  `identity.jwks_file` → `harbor token mint` (matching `--issuer` /
  `--audience`) → `runtime.info` OK (131d) — plus an asserted **401** on a
  mismatched-`iss` token. **MCP TOOL DISPATCH** — an in-process goal drives
  the planner to invoke `mcptest_echo` THROUGH the executor to a real stdio
  MCP subprocess, the echoed sentinel proving the dispatch (136). The
  mandatory §17.3/§17.4 coverage: ≥1 failure mode per edge (mismatched-iss
  401, garbage-token 401, missing-identity 401) + the **D-220 invariant**
  (`harbor serve` mints nothing) re-asserted at the binary surface;
  identity propagation asserted end-to-end (the embed run is goal/identity
  scoped, the Protocol token's verified triple is accepted while a
  mismatched/absent one is rejected, the MCP echo is impossible without the
  triple reaching the MCP `_meta`) + a cross-identity `Tasks.Get` isolation
  assertion; and an N≥16 concurrency stress — N concurrent `RunOnce` against
  ONE shared `Stack` with distinct triples + per-run sinks, asserting no
  answer bleed, no cross-run chunk bleed, and goroutine baseline restored
  after teardown. The file is `package integration` (not the external
  `integration_test` the wave_v17 precedent uses) precisely so it can reuse
  the in-package mock-OIDC issuer, the `harbor serve` boot harness, and the
  scripted-bifrost + MCP harness directly rather than re-rolling them.
  `scripts/smoke/phase-140.sh` is a thin static gate: it pins the file
  present + the top test defined, with a `go test -list` no-match-fails
  guard on `TestE2E_WaveV18` (mirroring 136) so the gate can never silently
  match zero tests. The phase then runs the read-only §17.5 checkpoint audit
  over the 131–139 band and lands one `chore(checkpoint): v1.8.0 audit
  fixes` PR that flips the 131a…139 `Status` rows + the root `README.md`
  Status table to Shipped and authors the `[1.8.0]` CHANGELOG entry. It
  gates the next band's scoping.
- **Decision:** none (audit/test phase — reuses the wave's shipped D-263…
  D-269 surfaces, introduces no new decision).
- **Status:** Shipped (V1.8).

---

### Phase 141 — Native tool-name sanitization for provider-safe tool-calling

- **Subsystem:** internal/planner/react (the native tool-calling projection)
  and examples (the embed-runonce worked-example runtime fix). No Protocol,
  config, or public-API surface.
- **RFC:** §6.2 (the Planner interface / `RunContext` tool view), §6.4 (the
  Runtime owns the executor / tool dispatch).
- **Deps:** 107c / 107d (native `tool_calls` emission + projection), 133 (the
  scaffold-with-tools path whose LIVE verification surfaced this).
- **What it delivers:** the React planner sanitizes every tool name it sends
  the LLM — the `req.Tools` declarations (`toolToDeclaration`) and the
  replayed assistant `tool_calls` history (`renderNativeStepPair` /
  `renderNativeParallelStep`) — to the provider-safe form
  `^[a-zA-Z0-9_-]{1,64}$` via `sanitizeToolName`, and resolves a
  provider-returned name back to the real catalog name via
  `resolveDeclaredToolName` before building the `CallTool` decision (single,
  parallel, serialized paths). Harbor's dotted convention (`clock.now`,
  `inventory.check`) was otherwise rejected by OpenAI-compatible providers
  with a `400` — found in live verification, missed by the scripted-LLM
  107c/107d/133 tests (§17.8). The catalog name stays the canonical key;
  declarations dedup on the sanitized name. A deterministic round-trip unit
  test (`tool_name_sanitize_test.go`) is the regression guard; the fix was
  validated live against a real provider for a dotted built-in AND a dotted
  scaffolded custom tool. Also folds the `examples/embed-runonce` runtime
  fix (declare a `ModelProfiles` entry so the worked example runs).
- **Decision:** D-270.
- **Status:** Shipped (V1.8).

### Phase 142 — External tool-credential provisioning: the `tokenexchange` OAuth driver

- **Subsystem:** internal/tools/auth (the D-095 OAuth flow-strategy driver
  registry) — new driver `internal/tools/auth/drivers/tokenexchange/`, plus a
  per-driver validation branch in internal/config and the D-196 blank import
  in internal/drivers/prod.
- **RFC:** §6.4 (tool-side OAuth; the "External tool-credential provisioning
  (D-271)" paragraph), §3.3 (the unified pause primitive the
  broker-declined-consent path parks on).
- **Deps:** 30 (tool-side OAuth subsystem — `OAuthProvider` / `TokenStore` /
  `ErrAuthRequired`, D-083), 50 (unified pause/resume), 64a (catalog
  `WrapWithOAuth` — the §13 consumer path, D-090), the D-095 registry
  (Wave 11.5 Stage A, PR #119).
- **What it delivers:** the pull-based external-credential acquisition
  strategy. A fleet orchestrator (or any external credential broker) holds a
  user's downstream integration credentials (M365, Google Workspace) in ONE
  place; a runtime obtains them non-interactively at token-miss time via an
  RFC-8693-shaped token exchange — `grant_type=…:token-exchange`,
  `subject_token` = the VERIFIED ctx identity triple (Harbor-defined
  `subject_token_type` URN; impersonation semantics, trust model documented
  honestly), the runtime authenticated by an env-indirected broker
  credential. Brokered tokens are TTL-cached in memory only, single-flighted,
  and never persisted (`TokenStore.Put` never called). Fail-loud: broker
  outage → typed `ErrExchangeFailed`, never a silent interactive fallback; a
  `consent_required` refusal → the existing typed `*auth.ErrAuthRequired`
  parking the run on the unified primitive, with resume re-driving `Token()`.
  Interactive-flow methods return the new typed `ErrNonInteractive`. Every
  actual exchange emits the new canonical `tool.credential_exchanged`
  SafePayload event. Push injection of credentials over the Protocol is
  REJECTED (not deferred) as §7 credential passthrough; MCP-layer-only
  custody is rejected as the general answer (identity-blind for shared
  servers) but documented as a recipe for per-user stdio servers. Composes
  with the 92k–92q band unchanged (92l's typed-`ErrAuthRequired` park must
  also branch on `ErrNonInteractive`). Fully additive — no wire-type,
  interface, or config-schema change. See
  `docs/plans/phase-142-tool-credential-exchange.md`.
- **Decision:** D-271.
- **Status:** Shipped (V1.9).

### Phase 143 — Run-level structured output: the `WithOutputSchema` run option

- **Subsystem:** internal/runtime/assemble (the run option + runtime-edge final
  validation), internal/runtime/runctx + internal/planner (threading + the
  additive `answer_payload` envelope key + `ErrOutputInvalid`),
  internal/planner/react (terminal-completion `ResponseFormat`/`Validator`
  wiring), sdk re-exports.
- **RFC:** §6.5 (structured output strategies + retry with feedback — the
  substrate; the "Run-level structured output (planned — D-272)" paragraph),
  §6.2 ("schema mode" among the runtime-level run options — this phase
  implements that slot), §3.6 (facade re-export path).
- **Deps:** 35 (OutputMode + downgrade chain, D-043), 36 (the
  `Validator`-keyed retry-with-feedback wrapper, D-043), 110a
  (`AnswerEnvelope`, D-194), 132 (`Stack.RunOnce`, D-265), 132-stream
  (`WithStream` + the ordering guarantee, D-266).
- **What it delivers:** the run-level typed-output mechanism. An embedder asks
  a run for a schema-conforming final answer in one line and receives a
  VALIDATED payload or a loud typed error. Opt-in with zero default-path
  change (no schema → byte-identical v1.8 behavior; the golden envelope test
  re-pins it). Validation is runtime mechanism at the RunOnce edge for EVERY
  planner — no capability ceremony; the React driver additionally constrains
  the terminal completion via the profile's EXISTING `OutputMode` strategy and
  engages the bounded corrective retry. Streaming posture (D-272,
  survey-validated): `WithStream` composes, `tool_dispatched`/`step` stream as
  today, assistant-content `token` chunks are suppressed for a
  schema-constrained run — the terminal answer arrives once, validated, on the
  additive `answer_payload` key (the buffered pairing every surveyed framework
  with a validate-and-retry loop chose; partial-object streaming is the named
  follow-up). This is also the §13 consumer that puts the idle
  `Validator`/`ResponseFormat` retry substrate into production. Coordination
  note: lands AFTER the pre-wave envelope-semantics fix PR (`ToolCallsSeen`),
  which touches the same files. See
  `docs/plans/phase-143-run-level-structured-output.md`.
- **Decision:** D-272.
- **Status:** Shipped (V1.9). Terminal-turn strategy: the React driver sets
  `ResponseFormat{FormatJSONSchema}` + a tool-call-aware `Validator` every turn
  and lets the profile's `OutputMode` decide the wire shaping (candidate A under
  Native; the existing envelopes under Tools/Prompted) — no new toggle. The
  schema compiles once at the runctx edge and the same compiled validator serves
  both the planner steering and the runtime-edge final validation.

### Phase 144 — Typed embed binding: `RunTyped[T]` + the shared schema-derivation home

- **Subsystem:** internal/tools/schema (the promoted Go-type→JSON-Schema
  derivation package), internal/runtime/assemble (`RunTyped`),
  internal/tools/drivers/inproc (re-based on the shared package), sdk/assemble
  (the generic forward).
- **RFC:** §3.6 (the facade + the carve-out pattern being amended), §6.2
  (consumed via 143's run option), §6.4 (the derivation machinery's origin).
- **Deps:** 143 (the mechanism this sugars), 26 (the inproc schema-derivation
  machinery, D-024), 112a (the facade + the one-carve-out precedent, D-205).
- **What it delivers:** the `output_type T` ergonomics on the embed path —
  `assemble.RunTyped[T](ctx, stack, goal, id)` derives the schema from `T`,
  runs schema-constrained, and returns the validated payload unmarshaled into
  `T`. The derivation is promoted to the neutral `internal/tools/schema`
  package (one implementation, §13-compliant home, golden-pinned
  byte-identical; both `RegisterFunc` and `RunTyped` consume it). The `sdk/`
  forward is the facade's SECOND documented generic-func carve-out — D-273
  amends D-205 item 1 with the identical rationale, and the no-behavior smoke
  flips to an enumerated two-func allow-list. Deliberately NOT named `Agent`
  and NO stateful binding object (D-273). Fallback named up front: if the
  amendment is rejected in review, 143 alone ships typed output with two
  caller-side lines and this phase slips without blocking the wave. See
  `docs/plans/phase-144-typed-embed-binding.md`.
- **Decision:** D-273.
- **Status:** Shipped (V1.9). The amendment landed as designed: the
  facade's no-behavior smoke (`scripts/smoke/phase-112a.sh`) enumerates
  the exact two-func allow-list and fails on any third. The promotion
  surfaced (and this phase fixed, per §17.6) a pre-existing §13 seam
  violation: `internal/runtime/flow` imported the concrete inproc
  driver directly for `DeriveSchema`; it now depends on the neutral
  `internal/tools/schema` package instead, same as the driver and
  `RunTyped`.

### Phase 145 — Governance attempt-level cost accounting: the in-band attempt-cost tap

- **Subsystem:** internal/governance (`Wrap` tap install + `CostAccumulator`
  drain-and-fold), internal/llm (the attempt-cost tap primitive),
  internal/llm/retry + internal/llm/output (the report sites).
- **RFC:** §6.15 (governance — PostCall cost accumulation, per-identity
  ceilings), §6.5 (the LLM-edge retry/downgrade layers whose internal
  attempts become accounting-visible).
- **Deps:** 36a (the CostAccumulator being made attempt-accurate, D-044),
  36 (the retry-with-feedback wrapper — the dominant leak site, D-043),
  35 (the downgrade chain — the secondary leak site), 33 (bifrost cost
  reporting), 143 (the first production `Validator` consumer — what took
  the gap from latent to live, D-272).
- **What it delivers:** closes the "Known accounting gap" recorded in D-272
  and at `internal/governance/wrap.go`: governance composes OUTSIDE retry
  (D-043/D-044, deliberately unchanged), so every intermediate corrective
  re-ask and downgrade attempt is a real provider call invisible to the
  `CostAccumulator` — worst case `(MaxRetries+1)×3` uncounted calls per
  planner turn. Fix: a synchronous in-band **attempt-cost tap** —
  `governance.Wrap` installs a per-call ctx-carried accumulator after
  `PreCall` permits; the retry wrapper reports each validator-rejected
  non-final attempt, the downgrade wrapper reports every errored attempt;
  `PostCall` drains once and folds tap total + final `resp.Cost` under the
  existing identity-triple key. The propagate-or-report invariant (each
  inner outcome is either propagated to the caller or consumed-and-reported,
  never both, never neither) makes accounting exactly-once and
  compose-order-independent. Deliberately NOT an `llm.cost.recorded`
  subscriber — the in-band rationale pinned at the accumulator ("the next
  PreCall sees the latest total without a bus-delivery race") is settled,
  and a subscriber accumulator would be a §13 second parallel
  implementation. PreCall short-circuit semantics, `resp.Cost` semantics,
  identity keying, and the event taxonomy are all unchanged; attempt spend
  accumulates even when the outer call ultimately errors (fail-loud, §13).
  Exactness test drives distinct per-attempt costs through retry ×
  downgrade across all three terminal shapes; a ceiling test proves
  intermediate attempts trip the NEXT PreCall; N≥100 shared-chain D-025
  stress; the stale bifrost "governance subscribes" doc comment is
  corrected in the same PR. See
  `docs/plans/phase-145-governance-attempt-accounting.md`.
- **Decision:** D-275.
- **Status:** Shipped (v1.10). Deviation (§4.3): the integration test composes
  governance via the documented headless `governance.Wrap(inner, sub)` path
  rather than the process-global `SetFactory` seam, to stay parallel-safe
  against sibling integration suites that toggle that global — the seam under
  test (a `Subsystem` factory's product consumed by `Wrap`) is identical. The
  same stale "governance subscribes" claim found on `internal/llm/llm.go`'s
  `Cost` godoc (beyond the planned bifrost `cost.go` one) was corrected in the
  same PR.

### Phase 146 — Per-task structured output: the `answer_payload` per-task producer

- **Subsystem:** internal/protocol (the `start` wire field + edge validation),
  internal/tasks (`SpawnRequest`/`Task` plumbing + conformance),
  internal/runtime/runctx (the promoted shared envelope builder),
  internal/runtime/assemble (RunOnce re-based on it), cmd/harbor +
  harbortest/devstack (the twin per-task RunLoop drivers), web/console
  (typed-client parity + manifest regen).
- **RFC:** §6.5 (run-level structured output — the per-task producer for the
  same mechanism), §6.2 ("schema mode" as a runtime-level run option),
  §6.8 (the task answer-envelope contract, extended additively), §5.2 (the
  `start` task-control method; `tasks.get` surfacing the payload), §5.3
  (additive field — no version bump).
- **Deps:** 143 (the compile/steer/validate mechanism + `AnswerPayload`,
  D-272), 110a (the canonical `planner.AnswerEnvelope`, D-194), 118 (the
  D-223 TS lockstep gate this wire change must satisfy), 54 (the `start`
  Protocol control surface), 73d (`tasks.get` + `result_inline`), 87 (the
  durable TaskService backend the persisted field rides), 107e
  (SpawnTask/AwaitTask dispatch + `taskOutcomeObservation`, D-170).
- **What it delivers:** closes the run-level/per-task asymmetry Phase 143
  deliberately left (the `answer_payload` key is documented "reserved" at
  `internal/runtime/dispatch/dispatch.go::taskOutcomeObservation` and
  `internal/tasks/tasks.go::TaskResult`). A new additive `output_schema`
  field on `types.StartRequest` (per-run granularity — deliberately not
  agent config: D-234's next-turn projection is per-agent desired state)
  flows request → task record → the per-task drivers, which compile once
  at run start (`planner.CompileOutputSchema` — the edge also rejects a
  bad schema with `CodeInvalidRequest` before any task spawns), set
  `RunSpec.Base.OutputSchema` so the React driver's existing per-turn
  steering engages unchanged, and validate the terminal Finish through ONE
  promoted `runctx` envelope builder shared with `RunOnce` (§13 — the
  RunLoop deliberately does not validate; every run-edge caller invokes
  the same implementation). The validated payload lands as `answer_payload`
  on the task envelope, surfacing via `tasks.get` `result_inline` and the
  AwaitTask parent observation (whose D-026 `projectForLLM` offload
  already applies — test-pinned). Schema-invalid after the retry budget →
  the task fails loud with the new `output_invalid` terminal code, never a
  schemaless success; token deltas are suppressed on schema-constrained
  tasks (the D-272 buffered posture, mirrored). Full D-223 lockstep dance
  and D-209 docs regen. The §13 consumer is the AwaitTask round-trip E2E the
  v1.9 wave-end audit specified: a parent run awaits a schema-constrained
  task and receives the validated payload in its observation, with the
  schema-invalid-after-budget failure mode asserted. Non-goals recorded:
  planner-emitted `SpawnTask` schemas (sealed Decision sum, D-047),
  config-level default schemas, partial-object streaming (#444),
  `TaskDetail` schema exposure (no consumer yet). See
  `docs/plans/phase-146-per-task-structured-output.md`.
- **Decision:** D-276.
- **Status:** Shipped (v1.10).

### Phase 147 — Events conformance suite: the shared multi-driver home + the duplicated-scenario fold

- **Subsystem:** internal/events (the new `internal/events/conformancetest`
  suite package + both driver packages' `_test.go` files).
- **RFC:** §6.13 (the typed event bus — the one contract both drivers
  implement), §6.9 (sessions — the erasure cascade whose bus-side fence
  contract is among the folded scenarios).
- **Deps:** 05 (inmem bus + taxonomy), 06 (replay + ring + cursor), 57 (the
  durable driver), 125 (the `HistoryReplayer` Bounds/Window surface, D-254),
  130 (session erasure — the cascade the `Fencer` serves, D-262; its
  fail-loud hardening is D-274 item 2).
- **What it delivers:** the CLAUDE.md §11 conformance-suite rule applied to
  the one multi-driver subsystem that never got its home — the v1.9 wave
  audit's deferred NIT 7. `internal/events/conformancetest` mirrors the
  identity/state/memory precedent exactly (package `conformancetest`,
  exported `Run(t, factory)`); the `Factory` returns a memory-style
  `Harness` of three mandatory constructors (default replay-capable /
  replay-disabled / bounded-retention) because the folded scenarios span bus
  configurations — the one genuine driver divergence (only best-effort ring
  mode can return `ErrCursorTooOld`) is parameterized as configuration,
  never a `Supports*` flag (§4.4). Folds the verified duplicated per-driver
  scenario pairs — fence (drop-late + empty-history, after-close),
  Bounds/Window, replay cursors, subscribe scoping, close lifecycle — into
  20 pinned scenarios under a binding no-coverage-loss mapping table; six
  cells are coverage GAINS on the driver that lacked the scenario.
  Identity-scoping scenarios are unconditional members (§6 rule 10). Pure
  test refactor: zero production change; both drivers consume the suite in
  the same PR (§13 consumer by construction). Durable recovery/restart,
  OpenWith, persist-failure, cancellation-bounds, and every per-driver
  D-025 reuse test stay driver-specific; the timing-sensitive
  drop-policy/redaction/reaper/admin-audit pairs are the named second
  tranche. See `docs/plans/phase-147-events-conformance-suite.md`.
- **Decision:** D-277.
- **Status:** Shipped (v1.10). All 20 pinned scenarios (plus a
  `Capabilities_ReplayerHistoryReplayerFencer_Present` fail-loud capability
  gate) pass against both drivers with ZERO production code change — every
  coverage-GAIN cell passed cleanly, so the named risk of a same-PR
  production fix never materialized. Coverage moved ~93.5–93.8%→~93.8–94.3%
  (inmem; run-to-run noisy from timing-dependent branches, no pairing
  regresses) and 88.3%→88.6% (durable, stable) — no regression.

### Phase 148 — MCP southbound per-identity OAuth bearer + `_meta` provenance enrichment

- **Subsystem:** internal/tools/drivers/mcp (per-call bearer injection +
  `buildIdentityMeta` enrichment), internal/tools (the agent-provenance ctx
  seam), internal/tools/auth (interface consumer only — no driver change),
  internal/config + internal/agentcfg + internal/protocol/types (the
  connection-surface binding fields), web/console (D-223 TS mirror).
- **RFC:** §6.4 (the MCP southbound transport; the D-271
  external-provisioning paragraph this makes wire-real), §3.3 (the unified
  pause primitive the `consent_required` path parks on), §6.16 (the
  registration `agent_id` stamped as provenance).
- **Deps:** 142 (the `tokenexchange` driver + D-271 — the credential carried
  to the wire), 92f (`add_mcp_connection` — the runtime descriptor + attach
  surface extended), 28 + the 85-band (the MCP driver), 30 + 64a/D-095
  (`auth.OAuthProvider` + the named-provider registry), 50 (unified
  pause/resume), 118 (the D-223 lockstep gate the wire change satisfies).
- **What it delivers:** the missing consumer between Phase 142's brokered
  credential and the MCP wire — a second consumer needs per-identity
  southbound credentials on SHARED MCP servers, and today the MCP call path
  has no auth beyond connect-frozen static headers (the catalog
  `WrapWithOAuth` pre-check deliberately discards the token it fetches).
  Four parts. (1) A NON-SECRET `oauth_provider` name binding a declared
  `tools.oauth_providers[]` entry lands on `config.MCPServerConfig`, the
  agentcfg `MCPConnectionDescriptor`, and the wire descriptor + add-request
  (a name is not secret material — the descriptor's never-carry-secrets
  invariant holds); unknown name fails loud listing registered providers
  (§4.4), stdio+binding is rejected, and a static `Authorization` header
  alongside a binding is rejected (one auth mode per connection). (2) Every
  identity-stamped per-call RPC (tool calls, resource reads,
  subscribe/unsubscribe, prompt gets) resolves `prov.Token(ctx, source)` and
  injects `Authorization: Bearer` on THAT request only via a context-aware
  RoundTripper — the token rides the per-call ctx (D-025; no mutable
  transport state); a bound provider whose `Token()` fails NEVER falls back
  to an unauthenticated call, and `consent_required` parks on the unified
  primitive via the existing typed `*auth.ErrAuthRequired` (§7 rule 4).
  (3) `buildIdentityMeta` additionally stamps `agent_id` — PROVENANCE via
  the new `tools.WithInvokingAgent` ctx seam (produced by the run loop + its
  devstack twin), never an isolation principal (§6/D-059): servers must not
  filter by it and Harbor keys nothing by it. (4) Operator-declared
  non-secret `meta_annotations` merge verbatim into every call's `_meta`;
  reserved keys (triple + `agent_id` + the D-073 trace carriers + the
  `io.modelcontextprotocol/` prefix) are rejected at validation. The
  injection seam is deliberately the ONE mechanism the Pending interactive
  phases (85b discovery, 92l agent-bound) reuse when they land — a second
  injection transport is the §13 parallel-implementation violation. Wire
  change runs the full D-223 lockstep + D-209 docs regen. Isolation gate:
  N≥100 concurrent calls through one shared Provider with distinct triples,
  the fixture server asserting per-request bearer↔`_meta`-triple match (no
  token bleed); the integration test reuses Phase 142's RFC-8693 broker
  fixture plus a go-sdk streamable-HTTP fixture server (§17.8). See
  `docs/plans/phase-148-mcp-southbound-oauth.md`.
- **Decision:** D-278.
- **Status:** Shipped (v1.10).

### Phase 149 — HTTP-manifest boot loader wiring

- **Subsystem:** internal/runtime/assemble (the `assembleCatalogBand` load+register
  loop), internal/config (validate flip + loader-side path resolution),
  internal/tools/drivers/http (the shipped-but-boot-consumer-less
  `LoadManifest`/`RegisterManifest` pair gains its production caller), examples +
  docs/CONFIG.md (the operator surface goes from "not wired yet" to working).
- **RFC:** §6.4 ("HTTP tool definitions: both inline … and out-of-process via
  UTCP-style manifest. Inline is the dev-loop ergonomic; manifest is the operator
  deployment shape" — this phase makes the second half true at boot; plus the
  tool-side OAuth paragraphs the consumer exercises), §3.4 (fail-loudly boot
  posture).
- **Deps:** 26 (catalog + `ErrToolDuplicateName`), 27 (HTTP driver + manifest
  types, D-036), 64a (catalog Builder + `tools.entries[]` + `WrapWithOAuth`,
  D-090/D-095), 110d (`assemble.Assemble` — the one config→stack home, D-196/
  D-197), 142 (`tokenexchange` + the §17.8 RFC-8693 broker fixture — the test
  vehicle, D-271).
- **What it delivers:** the boot path for `tools.http_manifests[]`. Before this
  phase, the knob was documented, exemplified, and REJECTED at validate time
  because no production path called the loader (`internal/config/config.go`
  documented the guard; the SDK friction audit §1 pinned it as the canonical
  dead knob).
  `assembleCatalogBand` now walks the list — after `builtin.RegisterWith`,
  BEFORE the catalog Builder's `Apply` — loading each manifest and registering
  its tools by name, so the EXISTING `tools.entries[].oauth` by-name binding
  wraps manifest tools with zero new OAuth machinery: config in, brokered-
  credential pre-check + HTTP round-trip out. This closes the "no
  config-declarable tool can exercise catalog OAuth wrapping black-box
  end-to-end" gap. `config.Validate` flips reject→validate (structural checks
  only — the validator stays I/O-free); `config.Load` resolves relative entries
  against the config file's directory with the `path_safety.go` Clean+prefix
  posture (§7 rule 5; escapes fail loud naming `tools.http_manifests[i]`);
  absolute entries (the documented `/etc/harbor/tools/*.yaml` shape) are Cleaned
  and accepted. Boot failure modes are loud, naming file + config key: missing/
  unparseable/`ErrManifestInvalid` manifests, and tool-name collisions
  (`tools.ErrToolDuplicateName` propagates). Both the binary and
  `harbortest/devstack` inherit the wiring through the ONE `Assemble` home —
  no second projection. Non-goals fenced in D-279: no `oauth` field on
  `ManifestTool` (the by-name path is THE binding home — a manifest-level field
  would be a §13 second implementation), no MCP/A2A manifest loaders, no hot
  reload (boot-only, restart-required per §10), no change to `WrapWithOAuth`
  pre-check-and-discard semantics (southbound injection is Phase 148's concern,
  for MCP). Ships with the E2E integration test (fixture HTTP server +
  tokenexchange broker fixture, identity + provenance asserted, ≥2 failure
  modes, `-race`), the D-025 N≥100 concurrent-reuse test on the shared catalog,
  the examples/CONFIG.md/godoc honesty sweep, and the §18 skills/recipes grep.
  See `docs/plans/phase-149-http-manifest-boot-loader.md`.
- **Decision:** D-279.
- **Status:** Shipped (v1.10).

### Phase 150 — Run-completion hook: transcript egress through the tool catalog

- **Subsystem:** internal/runtime/steering (the `RunSpec.CompletionHook` seam,
  transcript assembly, `run.hook_*` events), internal/agentcfg + the run-start
  projection (the versioned `hooks` section), internal/config (the yaml
  `runtime.hooks` block), internal/protocol/types (the `AgentConfigHooks` wire
  section), cmd/harbor + harbortest/devstack (run-start resolution wiring).
- **RFC:** §6.17 (the new "Run-completion hook" subsection — the RFC amendment
  rides the same PR as this plan), §6.3 (the run-loop seam + steering payload
  bounds), §6.4 (the tool-catalog egress + audit posture), §6.13 (the bus
  events), §6.16 (agent-config content; `agent_id` as metadata).
- **Deps:** 53 (the RunLoop, D-071), 83i (the `ToolExecutor` seam, D-152),
  92a (the agent-config registry/projection primitive, D-234), 132
  (`Stack.RunOnce`, D-265), 148 (same-wave — per-identity MCP OAuth binding;
  the hook is its non-planner consumer), 149 (soft — HTTP tool targets).
- **What it delivers:** the runtime's first run-lifecycle hook. Motivation:
  memory/audit/analytics sinks need the full conversation at completion for
  runs no client observes — background and disconnected runs have no observer
  to pull it — and no generic run-completion signal exists (a plain foreground
  `RunOnce` completing emits nothing on the bus, so a subscriber cannot cover
  all run types; the hook is therefore a RunLoop-level mechanism at the ONE
  seam every run type terminates through). `RunLoop.Run` fires the hook
  exactly once at its terminal boundary — all terminal outcomes, outcome in
  the payload, never mid-run or on pause — and dispatches a typed,
  golden-pinned `RunCompletionPayload` transcript (initial goal, steering
  user messages/redirects captured from live run state in order, assistant
  steps, final answer, run metadata) to an operator-named catalog tool through
  the existing executor path: provenance, identity, policy retries, and
  args-free audit events come free; a bespoke HTTP egress client is the
  rejected §13 alternative. A hook failure never alters the run outcome
  (`run.hook_failed` + Warn; success emits `run.hook_dispatched`); the
  cancelled-run case fires under a bounded `WithoutCancel` ctx that preserves
  identity values. Config pairs yaml `runtime.hooks.run_completion` with a
  versioned agentcfg `hooks` section on the existing revision surface (no new
  Protocol verb; types-only D-223 lockstep + protocol-docs regen). See
  `docs/plans/phase-150-run-completion-hook.md`.
- **Decision:** D-280.
- **Status:** Shipped (v1.10). The deferred terminal fire over `RunLoop.Run`'s
  named returns covers every terminal exit; steering `USER_MESSAGE` / `REDIRECT`
  text is captured in the drain-apply loop with the trajectory index it
  precedes. `projection.ActiveRunCompletionHook` resolves agentcfg › yaml ›
  none in one place (both driver twins call it; a small §4.3 deviation from the
  plan's 4-arg signature so the precedence is pinned by one table test). Wire
  impact was additive (`AgentConfigHooks` + `AgentConfigHooksDiff` + the
  `AgentConfigDiff` hooks arm), no new verb; D-223 TS lockstep + D-209
  protocol-docs regen ran in the same PR. `TranscriptEntry.At` is a
  `*time.Time` so untimestamped entries omit it in the golden JSON. The
  adversarial review round added: embed coverage — `Stack.RunOnce` resolves
  the hook from the stack's static config via the shared
  `projection.RunCompletionHookFromConfig` (also now applied by the devstack
  twin) with a `WithCompletionHook` RunOption; a wire-edge set-time
  `ErrInvalidHooks` rejection of a negative `timeout_ms` (yaml parity); a
  `recover()` in the fire so a panicking sink can never replace the settled
  run result; the `deadline_exceeded` error-classification arm; and the
  documented outcome-fidelity boundary (the hook outcome is the run-loop
  terminal outcome — the post-run schema backstop and driver task status are
  not reflected).

### Phase 151 — Runtime loading-mode control on tool exposure: the loading-override layer on the ONE exposure section

- **Subsystem:** internal/agentcfg (the `ToolExposure` loading maps + diff arms),
  internal/tools (`LoadingOverrideView` + the additive `Tool.Form`
  classification), internal/runtime/agentcfg (edge validation + the run-start
  projection), internal/tools/protocol + internal/protocol/types (the
  effective `loading_mode` read surface).
- **RFC:** §6.4 (tool catalog — `Tool.Loading`, `CatalogFilter.LoadingModes`,
  the MCP southbound driver), §6.16 (the agent-config control plane the
  section extends).
- **Deps:** 92a (the revisioned desired-state registry, D-234/D-235), 92d
  (the ToolExposure section + `set_tool_exposure` + `ExclusionView` + the
  run-start projection — extended), 107c (the deferred-loading engine +
  `tool_search` meta-tools, D-167), 110a (`tools.NewPlannerView` — the view
  seam), 118 (the D-223 TS lockstep gate the wire changes must satisfy).
  Coordination (not semantic): lands after 148 merges — same files
  (`mcp.go`, `agentconfig.go` wire types, `agentconfig.ts`).
- **What it delivers:** closes the verified v1.9 gap pair — the MCP driver
  pins injected TOOLS `LoadingAlways` (`mcp.go:539`; its resources/prompts
  already register deferred) and the runtime exposure surface has no loading
  field — by extending `agentcfg.ToolExposure` with `server_loading_modes`
  (per-`ToolSourceID`, applying to tool-form descriptors only via the new
  additive `Tool.Form` classification — a server-level `always` must not
  blanket-surface wrapped resources/prompts) and `tool_loading_modes`
  (per-name, exact, unconditional), values `always|deferred`, riding the
  existing `agent_config.set_tool_exposure` verb. ONE pinned precedence
  order — exposure per-tool > exposure per-server > boot
  `tools.entries[].loading_mode` > driver default — kills the
  two-knobs-undefined-precedence §13 smell (the bottom two layers are
  already materialized into `Tool.Loading` at boot by the catalog Builder).
  Application is NEXT-turn at the one shared projection seam
  (`projection.ActivePlannerCatalogView`, called by both run-loop drivers
  per D-094): a new `tools.LoadingOverrideView` filters `List()` on the
  EFFECTIVE mode while `Resolve()` never filters on loading — so an
  overridden-to-deferred tool drops out of the prompt-time catalog but
  stays `tool_search`-surfaceable and callable through the D-167 two-turn
  discovery cycle; `ExclusionView` composes outside unchanged (disable
  stays strictly stronger — hidden from List AND Resolve). Loading is not
  capability-narrowing (`VisibleNames` spans both modes; the D-234 app→host
  gate ignores it), so overrides live in the ADMIN tier only — the
  D-256/D-258 narrow-only user/session tiers gain no map fields. Fail-loud
  edges: an unknown mode value → 400 `invalid_request` before any registry
  write (no revision, no event); normalization keeps content hashes stable;
  `DiffToolExposure` gains structured loading arms (audit =
  `agent.config.revised` + the diff; no new event type). Read surface:
  `tools.describe` gains optional `agent_id` and reports the projected
  effective `loading_mode` via an injected resolver on the
  `CatalogProjector` (absent `agent_id` = boot-effective, byte-compatible).
  All wire changes are additive (no ProtocolVersion bump) with the full
  D-223 lockstep (manifest regen + TS mirror + gates) and D-209 docs regen
  in the same PR; `docs/skills/use-the-harbor-protocol` updated per §18.
  Consumer + proof: `test/integration/agentcfg_loading_exposure_test.go`
  against the real MCP stdio fixture (`cmd/harbor-mcptest-stdio`, §17.8) —
  flip to deferred → next run's prompt-visible catalog excludes it while
  `tool_search` still surfaces it; flip back → visible; invalid-mode
  failure; identity propagation + cross-agent/cross-tenant isolation (§6
  rule 10); plus an N≥100 concurrent-runs-during-admin-flips D-025 stress
  (no torn per-run snapshots). RFC §6.4, §6.16. D-281. See
  `docs/plans/phase-151-tool-loading-exposure.md`.
- **Decision:** D-281.
- **Status:** Shipped (v1.10).

---

### Phase 152 — agentcfg section-setter hooks carry-forward + rebuild-completeness guard

- **Subsystem:** internal/runtime/agentcfg/protocol (the five section-scoped
  setters + the new guard test).
- **RFC:** §6.16 (the agent-config control plane), §6.17 (the hooks section
  whose loss this fixes).
- **Deps:** 150 (the `Hooks` section), 151 (the loading maps on
  `set_tool_exposure` — the setter the headline bug rides), 92d/92f (the
  exposure section + revision registry), 92m (add-connection).
- **What it delivers:** all five section-scoped setters (`set_tool_exposure`,
  `add_mcp_connection`, `set_skills`, `set_prompt_layers`, `set_llm_params`)
  rebuild the `ConfigPayload` by hand and none carries the D-280 `Hooks`
  section forward — any section edit silently erases a pinned run-completion
  hook (§13 silent degradation against the subsystem's own section-merge
  invariant). Fix: carry `Hooks` in all five, plus a reflection-backed
  rebuild-completeness guard — a seed constructor populates every payload
  section and reflect-asserts full field coverage (a new section fails the
  seed first, naming the field), then each setter must preserve every
  non-target section byte-identically. No new verb, no wire change.
  Regression E2E at the Protocol service layer + the run-start projection
  (`projection.ActiveRunCompletionHook`) resolving after an interleaved
  edit. See `docs/plans/phase-152-agentcfg-hooks-carry-forward.md`.
- **Decision:** D-283.
- **Status:** Shipped (v1.11).

---

### Phase 153 — Admin-widened fleet enumeration for `tasks.list` + `agents.list`

- **Subsystem:** internal/tasks (registry seam + engine + conformance +
  protocol projectors), internal/runtime/registry/protocol (the agents
  analogue), internal/protocol/types + web/console (additive wire).
- **RFC:** §6.8 (tasks), §6.16 (Agent Registry), §5.2 (the Protocol read
  surface).
- **Deps:** 87 (durable task driver — conformance parity), 53a (Agent
  Registry), 118 (D-223 lockstep gate), 130 (the sessions admin/audit
  precedent shape).
- **What it delivers:** the reserved aggregating projector. `tasks.list` /
  `agents.list` today project only the caller's own triple — a fleet
  observer sees nothing. This phase adds explicit tenant-scoped enumeration
  on the task-registry seam (a separate method with an explicit tenant
  argument, never an optional session — no identity-downgrading knob, §13)
  with conformance parity across both task drivers, and the agents analogue
  over the Agent Registry; both behind the EXISTING per-subsystem
  `Projector` interfaces. Gate = the one existing `auth.ScopeAdmin` claim,
  exactly like sessions: widened-without-claim fails loud
  (`ErrScopeMismatch`, never silent narrowing); every widened call emits
  `audit.admin_scope_used`; rows carry full identity attribution.
  Cross-runtime federation stays coordinator-side over per-runtime reads.
  Additive wire fields with full D-223 lockstep + D-209 docs regen; §6
  rule 10 isolation E2E + N≥10 concurrent mixed listers under `-race`.
  See `docs/plans/phase-153-fleet-scoped-tasks-agents.md`.
- **Decision:** D-284.
- **Status:** Shipped (V1.11). Delivered the `ListTenant` enumeration seam
  on both the `TaskRegistry` (shared engine impl, conformance parity across
  inprocess + durable) and the `AgentRegistry` (StateStore maintenance-scan,
  no new index), the tasks + agents aggregating projectors behind the
  unchanged `Projector` interfaces, `Service.List` widening routed on
  `filter.tenant_ids` + the verified `auth.ScopeAdmin` claim (widened-
  without-claim → loud `ErrScopeMismatch`), `audit.admin_scope_used` on
  every widened call, per-row `identity` attribution (added to the agents
  `Agent` wire row; tasks already carried it), full D-223 TS lockstep +
  D-209 docs regen, and the §6-rule-10 isolation E2E (2×2×2 matrix, both
  task drivers, admin/non-admin, N≥10 concurrent stress under `-race`).
  `tasks.get` / `agents.get` deliberately stay caller-scoped: the widened
  LIST attributes each row's `(tenant, user, session)`, so a coordinator
  drills in with a normally-scoped `get` under the owning identity — no
  admin leg on detail reads (which preserves cross-tenant existence-hiding).

---

### Phase 154 — OAuth provider credential source: env or coordinator-served pull

- **Subsystem:** internal/tools/auth (the new `credsource` seam + drivers +
  `BuildProviders` threading), internal/config (schema + validation),
  internal/drivers/prod (blank imports), examples + docs site (the
  documented fetch contract).
- **RFC:** §6.4 (the D-271 paragraph's additive credential-source sentence),
  §3.3 (fail-loud posture).
- **Deps:** 142 (`tokenexchange` — the motivating consumer), 148
  (per-identity southbound binding), 30 (tool-side OAuth), 149
  (config-declared manifest tools — the E2E's black-box OAuth vehicle).
- **What it delivers:** zero-touch broker provisioning. Today a provider's
  client credential is env-resolved once at boot, so a coordinator-minted
  credential can never reach a running runtime (the one-reboot step). A
  §4.4 `credential_source` seam on the provider entry: `env` (default,
  byte-compatible) or `remote` — an authenticated pull of
  `client_id`/`client_secret` from a coordinator endpoint via the runtime's
  service token, at first need, memory-only TTL cache, single-flight,
  strict `format_version`-ed parse. Fail-loud everywhere; no fallback to
  env / unauthenticated / interactive; both-sources-on-one-entry is a
  validation error. Hot-reload / an admin verb carrying the secret is
  REJECTED (D-271 credential passthrough + post-exec env immutability).
  No catalog re-wrap: the provider instance stays boot-constructed; only
  credential resolution is late. §17.8 fixture credential server E2E
  (zero-env boot → first-need pull → token exchange succeeds; rotation
  leg). Defense-in-depth: the broker secret never enters the runtime's
  environment. See `docs/plans/phase-154-broker-credential-source.md`.
- **Decision:** D-285.
- **Status:** Shipped (v1.11). The `credsource` §4.4 seam (interface +
  factory + `env`/`remote` drivers), `BuildProviders` threading, the
  `credential_source` + `remote` config schema/validation, the two
  canonical fetch events + `ErrCredentialSourceUnavailable`, the committed
  coordinator fixture (`credsourcetest`), and the §17.8 E2E all landed.
  §4.3 deviations: (a) the fetch events + sentinel live in the
  `credsource` package (not `internal/tools/auth/events.go` as the plan
  sketched) to keep the seam a leaf — `internal/tools/auth` imports
  `credsource`, so a reverse import would cycle; (b) `remote` is restricted
  to the `tokenexchange` driver — the interactive `oauth2` flow bakes its
  credential at construction and cannot attach a lazy remote pull to an
  identity-bearing ctx for the mandatory audit event, so `oauth2` consumes
  the seam via `env`/static only. The two new canonical event names touch
  the D-223 wire manifest's event catalog (regenerated); no Protocol
  method or request/response wire-type changed.

---

### Phase 155 — Session-erasure audit integrity

- **Subsystem:** internal/sessions (the erasure cascade),
  internal/protocol/types (field docs only).
- **RFC:** §6.9 (sessions), §6.13 (the audited event).
- **Deps:** 130 (the erasure method + cascade, D-262).
- **What it delivers:** issues #409/#410 from the v1.7 band-end review. The
  `session.erased` record-of-fact is emitted best-effort AFTER the
  irreversible clear — one bus/redactor failure loses the only audit record
  while the call reports success, with no second chance (re-invoke returns
  `not_found`); and retried mid-cascade erasures under-report deletion
  counts. Fix: the ordering invariant becomes binding — never (data gone
  AND no durable record AND success returned), nor the inverse; a
  record/emit failure fails `sessions.delete` loud with a typed sentinel
  and the session still re-invokable; counts accumulate across converging
  attempts (the "document the undercount" alternative is rejected —
  compliance counts must be accurate). Fault-injection tested
  (bus-fails-once-then-heals; interrupt-mid-cascade-then-converge). No
  wire-shape change. Mechanism shipped: a durable erasure-ledger
  checkpoint (StateStore-backed, keyed under the actor's observability
  scope so it survives the erased triple's own `DeleteScope` clear) is
  persisted after every destructive step, strictly before
  `StateStore.DeleteScope`'s irreversible clear; the final
  `session.erased` emit reads only from the ledger and is now part of
  `Erase`'s success criteria (redactor refusal / bus-publish failure both
  fail loud with the new `ErrErasureRecordFailed`, mapped through
  `sessions/protocol.ErrErasureRecordFailed` to HTTP 500). Concurrent
  `sessions.delete` calls for the SAME session serialize via a striped
  in-process lock so exactly one call ever runs the cascade — the other
  observes the genuine not-found path, never a double event. See
  `docs/plans/phase-155-erasure-audit-integrity.md`.
- **Decision:** D-286.
- **Status:** Shipped (v1.11).

---

### Phase 156 — `agent_config.remove_mcp_connection` + detach-on-reconcile

- **Subsystem:** internal/runtime/agentcfg (verb + projection detach leg),
  internal/agentcfg (connections diff arm), internal/tools (catalog + MCP
  registry deregistration), internal/protocol + web/console (wire).
- **RFC:** §6.16 (the control plane), §6.4 (the catalog), §3.3 (pause vs
  removal semantics).
- **Deps:** 152 (the completeness guard this setter joins), 92f (the shipped
  add-connection verb), 118 (D-223). **As-built:** the plan originally also
  cited 92m/92n/92o — that 92k–92q MCP-OAuth band is still PARKED
  (planning-only, unshipped); 92o's run-start reconciliation never existed in
  code. Phase 156 BUILT run-start reconciliation from scratch, DETACH-ONLY
  (plus the primitives beneath it: MCP-registry Deregister/SourceIDs, provider
  Close, catalog source-deregistration, the driver-agnostic
  ConnectionDetacher seam + both D-094 twin concretes, wired into both
  run-loop drivers); the attach leg (restart-survival) lands with the future
  band. See D-287's as-built note.
- **What it delivers:** supersedes D-240 decision 5's deferral through its
  own recorded revisit clause — a removal need pause cannot serve emerged
  (a coordinator's delete flow; pause leaves the descriptor forever and
  resume resurrects the server). The verb records a new revision dropping
  the named descriptor AND pruning that server's tool-exposure residue
  atomically (sibling-safe: entries also claimed by a remaining server's
  `<name>_` prefix are never pruned), carrying all sibling sections (incl.
  Hooks) forward under the D-283 guard; unknown / boot-declared names fail
  loud with distinct typed errors. Run-start reconciliation (built here,
  detach-only): declared-vs-attached diff deregisters undeclared servers from
  the catalog + MCP registry and closes the transport at a run-start
  reconcile — never in the middle of the run that triggered it; exposure is
  next-turn, teardown is process-global (a cross-session in-flight caller of
  a detached server fails loudly with a typed error — test-pinned, D-287 call
  2 as amended) — and rollback past an add detaches through the SAME
  reconcile path (one mechanism, §13), closing D-240's deferred rollback
  gap. Agent-bound sealed tokens are NOT deleted on remove (re-add reuses
  consent; revocation is provider-side). New canonical
  `mcp.connection.removed` event; full D-223 + D-209; real-stdio-fixture
  E2E + remove-under-load `-race` stress + goroutine-baseline teardown
  proof. See `docs/plans/phase-156-remove-mcp-connection.md`.
- **Decision:** D-287.
- **Status:** Shipped (v1.11). Detach-only reconcile mechanism built here
  (the parked 92o attach leg stays deferred; the live add verb is the attach
  path) — see D-287's as-built note.

---

### Phase 157 — Session title: record field + `sessions.set_title` + Console rename

- **Subsystem:** internal/sessions (record field + registry setter + event),
  internal/protocol (method + wire types + stream handler), web/console
  (Sessions page + Playground consumers).
- **RFC:** §6.9 (the session model + the D-288 settled block), §5.2 (the
  method), §6.13 (the content-free event), §7 (the Console lens).
- **Deps:** 73c (sessions.list/inspect + Sessions page), 106 (Playground),
  118 (D-223 lockstep), 130 (the delete-path identity discipline this
  mirrors), 155 (erasure suites stay green).
- **What it delivers:** sessions stop displaying as raw ids. `Title` +
  `TitleSource` (`unset | auto | manual`) land on the canonical session
  record (additive JSON round-trip, zero migration, erased with the
  session); `sessions.set_title` writes `manual` ONLY (`auto` is not
  wire-expressible — the Phase 158 internal path is its sole producer, so
  manual-wins is structurally unforgeable); empty clears; over-bound input
  fails loud 400, never a silent clamp. Write scope is the owning
  `(tenant, user)` — the scope `sessions.list` already reads at —
  metadata-only, no elevation knob, no admin widening. The title string
  never rides an event payload: `session.title_changed` is a content-free
  SafePayload and consumers refetch. §13 consumers ship same-wave: the
  Sessions page renders the truncated title (tooltip full title + id, id
  fallback) with inline rename, and the Playground switcher renders
  `title || session_id` with an active-session rename + event-driven list
  refresh. Full D-223 lockstep + D-209 regen; registry D-025 stress
  extended (N≥100, `-race`). See `docs/plans/phase-157-session-title.md`.
- **Decision:** D-288.
- **Status:** Shipped (v1.12).

---

### Phase 158 — Session auto-naming: opt-in policy + terminal-boundary titling call

- **Subsystem:** internal/agentcfg + internal/runtime/agentcfg (the `naming`
  section + validation + projection), internal/runtime/steering (the
  terminal-boundary trigger), internal/sessions (counters + the auto write
  path), internal/config (yaml default), internal/protocol/types (additive
  wire types).
- **RFC:** §6.9 (the D-289 settled block), §6.17 (the additive-hook-point
  clause this walks through), §6.16 (agent-config revisions), §6.5 (the one
  wrapped LLM chain), §6.15 (governance).
- **Deps:** 157 (field + verb + `auto` source), 150 (the terminal boundary +
  transcript assembly this siblings), 152 (the D-283 guard this extends),
  92a (agent-config revisions), 118 (D-223), 83l (the scripted-LLM harness
  pattern).
- **What it delivers:** the runtime titles sessions itself — opt-in,
  DEFAULT OFF (zero-config behavior byte-identical to v1.11; a test pins
  zero counters / zero LLM calls / zero events). Policy = a `naming`
  agent-config section riding `set_revision` (hooks precedent: no new verb,
  additive wire types only, joins the D-283 guard) over a yaml
  `runtime.naming` default, resolved once at run start (D-234):
  `after_turns`, `repeat_every`, `max_repetitions` (required ≥ 1 whenever
  repeating — no unlimited value exists), `model` (empty = the run's
  effective model), `max_title_len`. Mechanism = a sibling of the
  run-completion hook at the run loop's terminal boundary making ONE
  governed `Complete` call on the run's wrapped LLM client over a ≤ 4 KiB
  bounded transcript digest; the write goes through the internal
  `SetTitleAuto`, which refuses manual titles and updates title + counters
  in one record save. A governance block SKIPS naming loudly
  (`governance_blocked`) and the run is untouched; every failure emits
  `session.naming_failed` (stable error class, never content). Scripted-LLM
  E2E covers the set_revision-enable / bare-`auto:false`-opt-out /
  manual-halts / clear-re-arms / cadence-and-cap / governance-block legs
  (as built, the governance leg blocks the naming PreCall via a one-shot
  rate-limit tier rather than a budget ceiling — a deterministic ceiling
  breach needs synthetic cost accounting the scripted provider cannot
  supply; §4.3-documented in the plan's As-built section); N≥10 concurrent
  sessions prove no title bleed under `-race`. See
  `docs/plans/phase-158-session-auto-naming.md`.
- **Decision:** D-289.
- **Status:** Shipped (v1.12).

---

### Phase 159 — Serve-band promotion: the config→listener composition leaves `package main`

- **Subsystem:** internal/runtime/serve (the promoted band), cmd/harbor (thin
  callers + the dev-only policy that stays), harbortest/devstack (the second
  consumer).
- **RFC:** §5.6 (the decided external-serving contract this promotion enables),
  §5.4 (the wire transport the band mounts), §5.5 (the auth posture seam),
  §6.1 (the runtime layer above assembly), §8 (the CLI subcommands that become
  thin callers).
- **Deps:** 64 (`harbor dev` v1 — the `bootDevStack` this promotes), 110d
  (D-197 `assemble.Assemble` — the layer the serve band sits above), 118
  (D-223 lockstep — stays green; no wire changes).
- **What it delivers:** `harbor serve` serves the Protocol from stock yaml, but
  a scaffolded agent carrying compiled in-process Go tools cannot — the serve
  composition (`bootDevStack`, `devBootOptions`, the `devStack` serve/close
  lifecycle) is trapped in `cmd/harbor` (`package main`), unreachable to any
  importer. This phase promotes that band into ONE importable internal package
  `internal/runtime/serve` (naming: `internal/server` is already the
  protocol-server package — do not collide). The promoted constructor REQUIRES
  a non-nil auth-validator factory (nil = loud error; identity is mandatory
  §6) and mounts ONLY the surfaces every caller shares — the dev signer NEVER
  promotes. Dev-only surfaces (bootstrap-token endpoint, dev mint/print, draft
  scaffolding, the dev key-rotate surface, Console mount, mock-LLM snapshot
  override, post-boot fixture seeding) are composed CALLER-SIDE by
  `cmd/harbor` through explicit injection seams on the promoted
  Options/Handle: extra pre-CORS routes, the transports auth-surface option,
  an LLM snapshot override, and a post-boot hook receiving subsystem handles.
  Dev-only POLICY stays cmd-side: the mock gate (D-089), the hot-reload
  supervisor (D-099), the dev signer + dev-token mint, drafts, and Console
  embedding (D-091). `harbor serve` / `harbor dev` / `harbor console` become
  thin callers. The §13 same-wave second consumer is `harbortest/devstack`,
  re-wired onto the promoted band with its hand-mirrored transports/mux block
  (~877–1310) deleted — the same re-homing move D-197 made for
  `assemble.Assemble` — with an owned behavior change: the kit's mux GAINS the
  options its mirror omitted (`WithAgentsService` / `WithAuthSurface` /
  `WithGovernanceService` / `WithGovernanceKeyRotate`); closing that drift IS
  the point of single-homing. The re-homing adds an honestly-enumerated NEW
  options/handle seam surface (the injection seams above) but ZERO wire
  changes: no new Protocol methods, no `ProtocolVersion` bump. The served
  `Handle` is a compiled artifact — a D-025 N≥100 concurrent-request `-race`
  test + goroutine-baseline teardown pin it. See
  `docs/plans/phase-159-serve-band-promotion.md`.
- **Decision:** D-291.
- **Status:** Shipped. As-built: promoted to `internal/runtime/serve`
  (`Boot`/`Options`/`Handle` + the single-homed `BuildMux` fan-out + the
  promoted run-loop driver / MCP attacher+detacher / session-ensurer /
  enricher). `harbor serve` / `dev` / `console` are thin callers; the dev-only
  policy composes caller-side in `cmd/harbor/devcompose.go` through the
  Options seams (`BuildLLMSnapshot`, `BuildAuthSurface`, `ExtraRoutes`,
  `PostBoot`). `harbortest/devstack` is the second consumer — its mux mirror
  and driver/glue mirror files are deleted; the kit gained the previously
  omitted agents / auth-rotate / governance-override / governance-key-rotate
  surfaces. THREE §4.3 deviations (full text in the phase plan's as-built
  section): (1) the plan's "LLM snapshot override" seam landed as a
  `BuildLLMSnapshot(cfg) (*snapshot, error)` BUILDER folding the fail-loud
  provider gate + mock override into one seam (avoids a double
  `config.Load`); (2) the dev signer is built ONCE caller-side and reused
  across hot-reload reboots — the validator keeps accepting earlier tokens,
  and the supervisor's onReboot hook re-mints + re-prints a fresh token (and
  the mock banner) per reboot; (3) the kit composes the promoted BUILDING
  BLOCKS (`BuildMux` + the promoted driver/glue) rather than routing its whole
  assembly through `serve.Boot`, preserving its stable `AssembleOpts` /
  `Skip*` public API for its 40+ consumers. Promotion-found bug fixed in-PR:
  the bind-address production/dev discriminator collapsed when the factory
  became mandatory (a dev boot could inherit a non-loopback config
  `bind_addr`); fixed via the explicit `Options.PreferConfigBindAddr` opt-in
  only `harbor serve` sets, with in-package + caller-level + live-listener
  regression pins. Named follow-up: a driver-options parity pin for the
  residual Boot↔devstack composition band (the mounted-surface half is pinned
  by the anti-drift integration test; the options-field half is tracked for
  the post-160 checkpoint audit).

---

### Phase 160 — `sdk/server` facade + `harbor scaffold --with-server` + the parity gate

- **Subsystem:** sdk/server (the curated facade), internal/runtime/assemble
  (the additive `RegisterCatalog` option), cmd/harbor (the `--with-server`
  scaffold flag + template), test/integration (the parity gate).
- **RFC:** §3.6 (the SDK facade gains `sdk/server`), §5.6 (the external-serving
  contract), §5.5 (the production JWKS posture), §6.4 (the tool catalog + the
  pre-policy registration seam), §8 (the scaffold subcommand's new flag).
- **Deps:** 159 (the promoted `internal/runtime/serve` — hard dep), 112a/112b
  (D-205/D-206 the `sdk/` facade tree + external-compile gate this extends),
  133 (D-267 scaffold-with-tools execution gate), 131d (D-264 `harbor token` —
  the production JWKS posture + local-dev loop), 118 (D-223 — stays green; no
  wire changes).
- **What it delivers:** a curated `sdk/server` facade over
  `internal/runtime/serve` — `server.Open(ctx, cfg, Options{RegisterCatalog})`
  → a handle with `Serve`/`Close` (D-204 alias/forward, NOT raw protocol
  re-exports) — so a scaffolded agent with compiled in-process Go tools serves
  the Protocol at parity with stock `harbor serve`. Production-only by
  construction: `Open` ALWAYS builds the JWKS validator from `cfg.Identity` and
  fails loud (named field) when absent — no dev-signer, no mock; the local-dev
  loop is `harbor token keygen` → `identity.jwks_file` → `harbor token mint`.
  `Open` re-runs `Validate` on programmatic configs (no validation bypass).
  The registrar mechanism is a NEW optional
  `assemble.Options.RegisterCatalog func(tools.ToolCatalog) error` invoked at
  the existing `PreRegisterTools` application point — before builtin
  registration and the `tools.entries` Builder wrapping — so compiled tools
  receive declared approval/OAuth/policy wrapping; it is an adapter over that
  one seam, never a second registration path (post-assembly `Catalog.Register`
  does NOT get the wrapping — the named trap, §13). `harbor scaffold
  --with-server` (opt-in; default scaffold stays headless RunOnce) generates
  `cmd/<agent>/main.go` (loads yaml, `--config`/`--bind` trio mirroring
  `harbor serve`, blank-imports `sdk/drivers/prod`, passes
  `agent.RegisterTools` to `server.Open`, serves); `harbor serve` itself calls
  the promoted constructor with a nil registrar via the internal package
  directly. The wave's acceptance centerpiece is the parity gate, scoped per
  leg: BOTH binaries from the SAME base config — (a) a manifest-driven
  method-status parity probe (in-module from `methods.Methods()`; script-side
  from the `wire-manifest.gen.json` methods key), (d) dev-only surfaces 404 on
  BOTH, (e) §17.3 real drivers + identity propagation + ≥1 failure mode
  (401) + N≥10 stress + `-race`; SCAFFOLDED BINARY ONLY — (b) generated-tool
  discovery + dispatch through the catalog, (c) the approval-gate wrap FIRING
  (proving pre-policy registration, D-292) — the `tools.entries[]` block
  naming the generated tool lives in the scaffolded binary's config OVERLAY,
  and a stock serve booted against it fails loud `ErrToolNotRegistered` (the
  deliberate fail-closed behavior, assertable as a negative; a wrap-fires
  mirror on both binaries MAY use a builtin tool). The CI/live split (§17.8):
  the (b)/(c) mechanics gate is an in-module `test/integration` scripted-LLM
  test (the 83l/158 precedent) under `-race`; the wire-level end-to-end
  against the real scaffolded subprocess binary is an env-gated
  `HARBOR_LIVE_*` live leg (the 131d precedent) run as Stage 2's
  live-verification step. No new Protocol wire types — no D-223/D-209 churn
  (stated explicitly). §18 same-PR surfaces: `scaffold-a-harbor-agent` +
  `add-an-in-process-tool` + `configure-production-identity` (+
  `use-the-harbor-protocol` checked), `embed-harbor-headless` recipe
  companion, docs/site stubs + nav, README pointer. See
  `docs/plans/phase-160-sdk-server-scaffold-parity.md`.
- **As-built (§4.3):** shipped as specified with three recorded realizations.
  (1) The facade forwards to an internal sibling package
  `internal/runtime/serve/external` (the production `Open` — config
  re-validation, the shared JWKS factory, the registrar adaptation, build
  identity from Go build info — plus a `Handle` wrapper whose
  `Close(ctx) error` matches the plan's public API); `sdk/server` stays
  alias/forward + the ONE `Options` adapter, and the load-from-path
  convenience is an `Options.ConfigPath` field (not a separate
  `OpenFromConfigFile`). The production JWKS factory + instance-id shape are
  single-homed as `serve.NewJWKSAuthValidatorFactory` / `serve.InstanceID`,
  reused by `cmd_serve.go` and the external band. (2) The parity gate boots
  the scaffolded composition through `external.Open` (the shipped path) and
  covers leg (b) DISPATCH in CI with the scripted-LLM pattern (canned
  tool-call → the compiled tool dispatches through the catalog; the terminal
  envelope carries `tool_calls_seen ≥ 1` and the handler's fixture marker
  round-trips into the follow-up prompt) and leg (c) behaviorally (the
  deny-all gate FIRES on the dispatched compiled tool —
  `tool.approval_requested` with a pause token — plus the structural
  assembly-seam `Gates` pin); the fail-closed negative asserts
  `errors.Is(err, catalog.ErrToolNotRegistered)` naming the tool. The
  script-side probe exists in `scripts/smoke/phase-160.sh`: stock
  `harbor serve` + the scaffolded binary boot from the same probe yaml and
  every `wire-manifest.gen.json` method is status-class-compared across both.
  (3) The env-gated live leg (`HARBOR_LIVE_SERVE=1`, needs a real
  `OPENROUTER_API_KEY`) runs the FULL wire choreography in-test: build the
  CLI, `token keygen`, scaffold `--with-server`, implement the generated
  stub, external build (replace directive), boot the subprocess, mint, drive
  `control.start` → poll `tasks.get` → assert the fixture answer +
  `tool_calls_seen ≥ 1`. New env vars: `HARBOR_LIVE_SERVE` (live-leg gate);
  the generated `main.go` honors `HARBOR_BIND` (documented in its README),
  mirroring `harbor serve`.
- **Decision:** D-292.
- **Status:** Shipped.

---

### Phase 161 — Session rehydration carries per-turn metadata

- **Subsystem:** internal/llm (the driver-neutral cost emit),
  internal/runtime/dispatch (the transport-agnostic tool lifecycle emit),
  internal/tools (the superseded per-driver emits), web/console (the reducer +
  Playground rehydration consumers).
- **RFC:** §6.13 (the one bus both channels reduce), §5.2 (the state-snapshots
  read this reconstructs from), §6.4 (the tool dispatch seam), §6.5 (the LLM
  edge), §7 (the Console lens).
- **Deps:** 125 (the `state.history` windowed read — D-254), 157 (session
  titles — the sibling reopen-surface work), 118 (D-223 lockstep — stays
  green; this phase proves a zero wire diff), 124 (the gap-free durable
  substrate, via 125).
- **What it delivers:** closes the operator-confirmed live-test regression
  where reopening a Playground session hydrates message content but loses
  per-turn tokens/cost/latency (header shows "no turns yet"), the TOOL CALLS
  badges, and the model chip. Root cause is producer-side, verified against a
  live dev boot: the read path strips nothing (payload pass-through;
  `planner.decision{DecisionKind, Tool}` and chunk payloads probe intact in
  read-back), but `llm.cost.recorded` — the only carrier of
  `Model`/`Cost`/`Usage` — is emitted only inside the bifrost driver, the
  `tool.*` lifecycle events are emitted only by the inproc transport (MCP /
  HTTP / A2A tools emit none), and even those stamp an empty envelope run id
  (attribution-dead live AND on replay). The fix is one driver-neutral emit
  seam per producer on the one bus: the cost emit promotes to the MANDATORY
  LLM-edge safety wrapper `llm.Open` composes around every driver (bifrost's
  internal emit deleted — one emit per driver-level completion, per attempt
  under retry, today's bifrost cadence) and the tool lifecycle emits land at
  the catalog-build descriptor-wrap seam — `catalog.Register` wraps every
  descriptor's `Invoke` in ONE universal shell, so all FOUR `desc.Invoke`
  production call sites (single executor, parallel-executor branches, the
  MCP-Apps proxy, the declarative-action re-invoke) inherit the emit by
  construction, with the full run quadruple on the envelope (inproc's
  per-driver emits + the orphaned `tools.WithBus` option deleted;
  `wave7a_test.go` migrates to the wrap seam) — which also fixes the latent
  live attribution bug in the same PR (§17.6). Coverage pins: ≥2
  quadruple-stamped `tool.invoked` per native parallel turn, and a
  direct-invoke non-executor pin covering the proxy/declarative shapes.
  Payloads stay CONTENT-FREE
  (tool name + transport + status + attempts + duration, usage/cost/model
  figures only — never args/results; a sentinel-redaction test pins it; §7).
  The read path is untouched (D-254 flat events, client-side reduction,
  by-id identity scoping; works on inmem within process lifetime). ZERO wire
  changes — no new methods, wire types, or canonical event types; no
  D-223/D-209 churn. The §13/D-062 consumer ships in the same phase:
  `HistoryTurn` gains the stats fields, `reduceHistoryTurns` folds them (its
  doc comment finally true), and `hydratePastTurns` populates header stats +
  per-message badges + TOOL CALLS + model chip — leave-and-return renders
  IDENTICAL to the live view (the acceptance centerpiece), backed by a
  stream-vs-readback key-equivalence test and the MCP leg against the real
  stdio fixture (§17.8). See
  `docs/plans/phase-161-session-rehydration-metadata.md`.
- **Decision:** D-293.
- **Status:** Pending.

---

### Phase 162 — `events.list`: durable, time-ranged, cross-session raw-event read

- **Subsystem:** internal/events (the `HistoryReplayer` seam + both drivers),
  internal/protocol (method + wire types + handler), web/console (the Events
  page historical window).
- **RFC:** §6.13 (the one bus this reads), §5.2 (the Protocol's
  streaming-events surface this completes), §4 (the identity/elevated-scope
  rules the widened read follows), §7 (the Console lens).
- **Deps:** 124 (gap-free durable log), 125 (`state.history` — paging
  grammar, row projection, `HistoryReplayer` seam; D-254), 72/72a
  (subscribe + aggregate siblings), 108h (the Events page), 118 (D-223).
- **What it delivers:** the third leg of the events surface. A second
  Protocol consumer needs "the raw events from 7 days ago to now,
  scrollable"; today `events.subscribe` is the forward-only tail,
  `events.aggregate` is counts-only, `state.history` is per-session and
  sequence-windowed, and `search.events` is text search — so a pure client
  would need the shadow store it must not build. `events.list` takes the
  EXISTING wire `EventFilter` (which already carries `since`/`until`,
  `types/events.go:46-51`) plus a tail-first sequence-based cursor mirroring
  `state.history`'s `next_cursor`/`has_more`, and returns the EXISTING flat
  `StateEvent` row projection — no new row shape, redaction and D-026
  by-reference untouched. Scope per §6 item 5: own-triple for non-admin;
  fleet widening only via the verified admin claim, derived server-side,
  audited once per request. Substrate: the `HistoryReplayer` seam both V1
  drivers implement — durable serves real windows; inmem serves its ring +
  an honest `truncated` (no capability ceremony). The D-062 consumer ships
  same-phase: the Console Events page (whose empty state already hints at
  durable read-back) drives the read from its existing window picker with
  scroll-up paging and a retention-gap notice; the live tail is unchanged.
  Operator latitude on adjacent reads was exercised as NO additions (each
  candidate is already answerable from the rows or the aggregate). Full
  D-223 lockstep + D-209 regen. See
  `docs/plans/phase-162-events-list-durable-read.md`.
- **Decision:** D-294.
- **Status:** Shipped.

---

### Phase 163 — Windowed-reads honesty pair: `flows.runs.list` time filter + retention horizons as data

- **Subsystem:** internal/protocol (flows + posture wire types and
  projectors), internal/events (the horizon derivation), web/console (flow
  detail date filter + Events window-edge banner).
- **RFC:** §5.2 (the read surfaces), §6.1 (flows), §6.13 (the durable
  substrate), §6.14 (the out-of-band metrics posture the decided-NO
  preserves), §7 (the Console lens).
- **Deps:** 26a (flows + run history), 108p (flows page), 72f (posture
  surfaces), 108h + 162 (the Events page window work the banner composes
  with), 125 (the `truncated` flag this pairs with), 118 (D-223).
- **What it delivers:** two bundled honesty asks. (1) `FlowRunsListRequest`
  gains optional `since`/`until` mirroring `TaskFilter.Since/Until` exactly
  (additive; absent ⇒ unbounded; bounds on `StartedAt` before pagination;
  scope rules unchanged) — the flow detail page's run-history table gains
  the server-side date filter as the same-phase consumer. (2) Retention
  horizons surface as Protocol data: an additive `retention` block on
  `runtime.health` carrying the OBSERVED `oldest_retained_at` per durable
  surface (events/tasks/sessions). The filed ask's premise — "surface the
  existing retention config" — was verified FALSE (no retention knob
  exists; the durable log is "gap-free and untrimmed in V1",
  `durable.go:776`), so the plan corrects to the observed horizon, which is
  strictly more honest and derivable today; a configured field becomes
  additive if a pruning knob ever ships. Pairs with the at-read `truncated`
  flag; the Events page gains the window-edge honesty banner. The
  counters/metrics TSDB is re-recorded as a decided-NO (snapshots stay
  now-only; trends derive from the durable log; metrics scrape out-of-band
  per RFC §6.14) so it is not re-opened. Full D-223/D-209 regen. See
  `docs/plans/phase-163-windowed-reads-honesty.md`.
- **Decision:** D-295 (flows time filter) + D-296 (retention horizons + the
  TSDB decided-NO).
- **As-built deviations:** (a) the `flows.runs.list` upper bound is
  EXCLUSIVE (`until` excludes a run at exactly `until`), per the plan's
  explicit inclusive-lower/exclusive-upper semantics — the FIELD SHAPE
  mirrors `TaskFilter` exactly (optional `omitempty` `time.Time`,
  `until<since` → `CodeInvalidRequest`) while the boundary handling is the
  stated half-open window (`TaskFilter`'s own upper bound is inclusive).
  (b) The retention seam is an events optional-capability interface
  (`events.RetentionReporter`, both drivers implement it, type-asserted at
  the wiring seam — the `DroppedCounter` precedent, not a `Supports*`
  flag). (c) The events horizon is runtime-wide (a bare timestamp); the
  tasks/sessions horizons are the oldest retained within the read's scope,
  mirroring `CountersProvider`'s scoping (tasks per-session, sessions
  per-tenant) rather than a cross-tenant maintenance scan.
- **Status:** Shipped.

---

### Phase 164 — MCP OAuth requirement discovery, surfaced as data

- **Subsystem:** internal/tools/auth (the discovery chain walker),
  internal/tools/drivers/mcp (the 401-challenge capture + registry state),
  internal/protocol (the additive connection-view field), web/console (the
  MCP Connections requirement card).
- **RFC:** §6.4 (the MCP southbound edge), §5.2 (surfaced as Protocol
  data), §6.15 (the audit/security posture on the discovery fetches), §7
  (the Console lens).
- **Deps:** 28 (MCP southbound driver), 30 (tools/auth — the RFC 8414
  machinery this composes), 108m (the MCP Connections page), 118 (D-223).
  Related, NOT deps: the parked 92p (reserved D-246) and the ready 85b —
  the flow-executing siblings that reuse this phase's single-homed
  discovery chain.
- **What it delivers:** near-zero-config credential onboarding as DATA. When
  a connected MCP server presents the MCP-auth-spec challenge (`401` +
  `WWW-Authenticate` `resource_metadata` — nothing captures it today,
  grep-verified) or an operator probes (`mcp.servers.probe`), Harbor walks
  the metadata chain — RFC 9728 protected-resource metadata →
  `authorization_servers[]` → RFC 8414/OIDC metadata (endpoints, scopes,
  PKCE, optional RFC 7591 registration endpoint, RFC 8707 resource) —
  reusing the existing `Provider.resolveEndpoints` for the 8414 half and
  REPORTING (never invoking) the registration endpoint. The chain surfaces
  verbatim + provenance as an additive `oauth_requirement` on
  `MCPServerView`, projected by list/get/probe. Hard boundaries: Harbor
  never runs the OAuth flow, never holds/refreshes a token (custody stays
  consumer-side; D-271 stays PULL); discovered metadata is inert untrusted
  data — a proposal an operator confirms, never auto-applied config. SSRF
  guardrails bound the discovery fetches (same-origin default + explicit
  allowance, redirect/timeout/size caps, https-only off-loopback, no
  credentials attached), each negative-tested. §17.8 fixtures derive from
  the real spec artifacts (a wrong-field mutation must fail). The D-062
  consumer ships same-phase: the MCP Connections page renders the
  discovered requirement marked unverified. One discovery mechanism, N
  consumption postures: the ready Phase 85b (RFC 9728 discovery + the 401
  step-up + flow execution) and the parked 92p (reserved D-246) each reuse
  this chain and add only their flow legs — the Phase 148 precedent; both
  sibling plan files gain pointer notes (85b's in this PR). SSRF guardrails
  are per-hop (the RFC 8414 authorization-server hop is inherently
  cross-origin and requires the explicit per-connection origin allowance;
  allowed fetches also refuse private-range/IP-literal hosts). §17.8
  fixtures are RFC 9728 §3.2 + RFC 8414 §3.2 example documents committed as
  testdata. `mcp.servers.probe` triggers discovery (its `MCPProbeRow`
  return unchanged); the requirement is read via the `mcp.servers.get`/
  `list` view. Full D-223/D-209 regen; no new method or event. See
  `docs/plans/phase-164-mcp-oauth-discovery-surfacing.md`.
- **Decision:** D-297.
- **Status:** Shipped.
- **As-built note (§4.3):** the RFC 8414 PARSE shape (`discoveredMetadata`) is
  single-homed and shared with the interactive-flow resolver, but the report-
  only walker uses a NEW guardrailed fetch path (`internal/tools/auth/
  discovery.go`) rather than composing `Provider.fetchDiscovery` directly —
  the existing fetch has no SSRF guardrails and caches into flow-execution
  state, which report-only discovery must not touch. This strengthens the
  single-homing claim: 85b / 92p reuse this guardrailed walker, not the
  ungated flow fetch. A per-connection `oauth_discovery_allowed_origins`
  config field carries the SSRF cross-origin allowances (IP-literal / non-https
  origins rejected at config load). The DNS-rebinding backstop is a
  `net.Dialer.Control` hook running POST-DNS-resolution against the resolved
  IP (a pre-resolution `DialContext` guard missed hostnames that resolve to
  private addresses — caught in adversarial review, fixed with a
  fail-without/pass-with test); an aggregate walk budget caps the
  authorization-server fan-out.

---

### Phase 165 — Structured reasoning-steps rehydration

- **Subsystem:** web/console (Playground session-reopen reducer + hydration) —
  Console-only, no Go/wire touch.
- **RFC:** §6.13 (the one bus both the live view and the reopen reduce), §5.2
  (the `state.history` read the reconstruction is over), §7 (the Console
  lens), §6.2 (the planner reasoning channel the steps project).
- **Deps:** 161 (D-293 — the rehydration foundation this extends), 125 (D-254 —
  the `state.history` rows), 107a (the `ReasoningStep` type +
  `parseReasoningSteps` + the enricher trajectory projection this reconstructs
  the reopen equivalent of), 118 (D-223 — stays a no-op; zero wire diff proven).
- **What it delivers:** on session reopen, the ordered reasoning-step accordion
  interleaved with the tool calls each step preceded — not 161's flat
  reasoning blob. **Verdict: ZERO-WIRE, Console-only** (verified live probe +
  code trace, 2026-07-11): the live path's reasoning steps come from the
  tasks.get enricher projection over the in-memory trajectory, which is
  in-memory-only (the enricher can never serve a reopened run — the task
  record survives but its trajectory projection is reaped, `enricher.go:49-56`),
  so the ONLY durable source is the
  event stream. `state.history` already carries `planner.decision` (one per
  trajectory step, ordered by `sequence`, each with `ReasoningTrace` +
  `DecisionKind` + `Tool`) plus `tool.*` lifecycle, and 161's reducer already
  reads `planner.decision` for the tool-call badges — it merely ignores the
  `ReasoningTrace` key. Byte-equivalence is by construction (`emitDecision`
  and `rc.OnReasoning` feed the same `resp.Reasoning` into the event and the
  trajectory step; the enricher projects it verbatim). Corrected source model
  (a fix to the motivating brief): `reasoning_trace` is native
  `llm.CompleteResponse.Reasoning` bucketed per ReAct step, NOT a removed
  "Thought:" scratchpad. `reduceHistoryTurns` folds the trace into
  `HistoryTurn.reasoningSteps {index, reasoning_trace}` (index = the per-run
  step ordinal over step-appending decision kinds only, matching the
  enricher's `i`-over-`traj.Steps`),
  and `hydratePastTurns` sets `message.reasoningSteps` — which the message
  bubble already prefers over the flat text, rendering the accordion identical
  to live; an empty-reasoning run falls back to 161's `reasoningText`. The
  index rule folds ONLY step-appending decision kinds
  (CallTool/CallParallel/SpawnTask/AwaitTask) — `Finish`/`RequestPause` emit a
  `planner.decision` but append no `traj.Step`, so folding them would
  phantom-add/shift steps (the mandated fixture carries a reasoning-bearing
  `Finish` + a mid-run `RequestPause`). Console
  vitest (ordered reconstruction + byte-equivalence vs `parseReasoningSteps` +
  page-boundary safety) + the rehydration regression. Zero wire/runtime
  change; no D-223/D-209 churn. Non-goals: the flat text path (161), the
  Anthropic-native-thinking-empty question (resolved, separate), and any
  `tasks.get`/enricher change. See
  `docs/plans/phase-165-reasoning-steps-rehydration.md`.
- **Decision:** D-298.
- **Status:** Shipped.

---

### Phase 166 — Credential-sink hardening (shipped-code security fix)

- **Subsystem:** internal/tools/auth (the token-exchange driver + the provider
  ceiling), internal/tools/drivers/mcp (`resolveOAuthBinding` downstream-host
  guard), internal/config (the allow-list + ceiling), internal/protocol (the
  `handleSetRawHTMLTrust` audit-ordering fix).
- **RFC:** §6.4 (the tool-side OAuth seam being hardened), §6.15 (the audit
  posture), §7 (no credential passthrough / no unbounded egress).
- **Deps:** 142 (D-271 — the `tokenexchange` driver), 148 (D-278 — the
  southbound binding whose sink this bounds), 154 (D-285 — the credential-source
  seam whose `remote` redirect-refusal is the precedent), 28 (the MCP driver +
  `resolveOAuthBinding`).
- **What it delivers:** the base the v1.14 wave stands on. Two adversarial
  reviews of the first-cut v1.14 shape returned NO-GO and proved the "no env-var
  NAMES on the wire" rule a SYMPTOM rule; the generalized invariant (D-300) is
  **no admin-writable field may determine where a credential is sent.** Three
  exfil paths — all reachable in SHIPPED Harbor (92f's add path + D-278's
  binding), none an HA-15 regression — are fixed in-band (§17.6): `token_url` is
  where the org's `client_id`/`client_secret` are POSTed (`tokenexchange.go:582`);
  the connection `url` is where the exchanged bearer is injected and
  `resolveOAuthBinding` constrained no host, while the provider name was the
  default audience; and the token-exchange client followed redirects, replaying
  the `client_secret` form. The fix: a boot-declared `AllowedDownstreamHosts`
  allow-list enforced in `resolveOAuthBinding` (fail-closed on empty for a
  bindable provider; covers the interactive `oauth2` binding too); a boot
  audience/scope ceiling (audience not derived from the caller-chosen name;
  scopes intersected); the token-exchange client hardened to the discovery
  client's bar (`net.Dialer.Control` private/loopback refusal, `Proxy: nil`,
  refuse-redirects — the `credsource/remote` precedent); and the
  `handleSetRawHTMLTrust` audit-ordering lie corrected to a genuinely fail-closed
  ordering + a shared helper Phases 168/169 reuse. No new Protocol surface —
  config-schema + internal only, with a CHANGELOG migration note for the
  mandatory allow-list. See
  `docs/plans/phase-166-credential-sink-hardening.md`.
- **Decision:** D-300.

---

### Phase 167 — Owner-scoped reconcile for runtime-added connections + providers

- **Subsystem:** internal/tools/drivers/mcp (the owner tag on runtime-added
  entries + the owner-filtered reconcile view), internal/tools/auth (the owner
  tag on the installed provider set), internal/runtime/serve and
  internal/runtime/agentcfg (the owner-scoped attacher/detacher/reconcile),
  internal/mcpconsole (the accessor threading).
- **RFC:** §6.4 (the registry — process-global model unchanged), §6.16 (the
  reconcile seam being owner-scoped), §7 (the HONEST bounded posture).
- **Deps:** 166 (the wave builds bottom-up), 28 (the MCP registry), 92f (the add
  path + `AttachRequest.Identity`), 156 (D-287 — the reconcile seam being scoped,
  EXTENDED not reversed), 92a (the agent-config revision ownership the tag
  mirrors).
- **What it delivers:** a NARROW shipped-code correctness fix, corrected after
  two delta reviews returned NO-GO on the first-cut "identity-key the registries
  by the triple" draft. That draft was wrong three ways (all confirmed against
  code): it BREAKS the common deployment (boot servers attach under one
  `mcpDefault` identity, `assemble.go:929`, but are read under many session
  triples → triple-keying fragments them per session); it does NOT isolate
  (dispatch goes through the process-global bare-name catalog, `catalog.go`,
  which 169 declines to widen → keying metadata leaves same-named runtime-adds
  colliding or cross-serving); and it silently REVERSES D-287 (shared
  catalog/registry; refcount/drain rejected). The corrected model
  (operator-chosen): boot infra stays PROCESS-GLOBAL — D-287 is PRESERVED, not
  reversed. Runtime-ADDED connections + Protocol-installed providers carry an
  OWNER tag `(tenant, agent)` used for exactly (a) the agent-config revision
  ownership they already have and (b) RECONCILE-VIEW scoping, so a run-start
  reconcile touches only ITS OWN owner's runtime-adds — never boot servers,
  never another owner's adds. This is precisely what the two NOTEs asked for (a
  per-agent reconcile VIEW, NOT full-triple keying; §6 says `agent_id` is not an
  isolation key); the NOTEs are REWRITTEN, not deleted. The wave claims NO hard
  cross-tenant isolation of runtime-added tool DISPATCH in a shared runtime — a
  shared runtime trusts co-tenant admins (a name collision fails loud), and hard
  isolation is a one-runtime-per-tenant deployment (which gets full isolation
  for free). No false safety property (the prior draft's FAIL-A). Fixed AC:
  `TestRegistry_BootServerVisibleToEverySession` +
  `TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner` (the discredited
  `TestRegistry_IdentityKeyed_NoCrossTenantOverwrite` used two tenant-differing
  triples that could not distinguish tenant-key from full-triple-key — FAIL-B).
  Single-tenant behaviour-identical; no new Protocol surface; unblocks 168/169.
  See `docs/plans/phase-167-owner-scoped-reconcile.md`.
- **Decision:** D-301.

---

### Phase 168 — Live MCP OAuth discovery-allowance write

- **Subsystem:** internal/agentcfg (the descriptor field + diff arm),
  internal/runtime/agentcfg (the service method + the wiring carry + the
  allowance reconcile leg), internal/tools/drivers/mcp (the bare-name mutator
  plus the fresh-requirement prune), internal/config (the exported validator),
  internal/protocol (the method + wire types + route), web/console (the
  `/agent-config` editor + the `/mcp-connections` deep-link).
- **RFC:** §6.4, §6.16, §5.2, §6.15, §7.
- **Deps:** 167 (the owner-scoped reconcile this rides for rollback — `lockAgent` alone
  gives zero cross-tenant protection, FAIL 5), 166 (the corrected audit
  ordering), 164 (D-297 — the walker + the `needs_allowance` status), 92f, 156
  (D-287 — the diff arm, the boot-declared refusal, `remove_mcp_connection`),
  152 (D-283), 92h + 92i, 108m, 118 (D-223).
- **What it delivers:** the discovery-allowance write half of HA-15, re-homed
  from the first-cut Phase 166. It makes the allowance a first-class,
  revisioned, diffable, rollback-able descriptor field; adds ONE narrow admin
  verb `agent_config.set_mcp_discovery_origins` (FULL REPLACE) that writes the
  revision under `lockAgent` AND applies it to the process-global live registry
  (D-287, bare-name) via `SetOAuthDiscoveryOrigins`; makes REVOKE live and
  symmetric (prunes the recorded `oauth_requirement`'s AS entries fetched from
  the revoked origin, building a FRESH requirement and swapping the pointer under
  the lock — WARN 10); makes ROLLBACK take effect live through a run-start
  allowance-reconcile leg (FAIL 7 — no revision-vs-runtime half-write); reuses
  the shared `ValidateDiscoveryOrigin`; keeps allowance ≠ SSRF bypass (the
  post-DNS dial guard still refuses a granted private/loopback origin); is
  admin-gated by omission from the exception maps (D-219). **Correction (WARN
  8):** the first-cut "shipped bug drops the field" claim was false — nothing
  upstream CARRIES the field (a §17.1 wiring gap); the discriminator is a
  within-phase round-trip guard. Console (D-062): the write is single-homed on
  `/agent-config`, `/mcp-connections` deep-links to it (resolving connection→agent
  from the agent-config registry first — item 12), and Phase 156's caller-less
  `remove_mcp_connection` gets its first Svelte caller. The 92m collision is
  ruled (its parallel add-request OAuth block routes through 169; pointer notes
  written into 92k/92m). Full D-223/D-209 regen; `ProtocolVersion` unbumped. See
  `docs/plans/phase-168-mcp-discovery-allowance-write.md`.
- **Decision:** D-302.
- **Status:** Shipped (v1.14).

---

### Phase 169 — Protocol-installed OAuth provider (zero-URL) + the connection→provider binding

- **Subsystem:** internal/tools/auth (the owner-tagged `ProviderSet`),
  internal/runtime/agentcfg (the two service methods + the reconcile leg),
  internal/agentcfg (the providers section), internal/tools/drivers/mcp (the
  attach-path set lookup), internal/protocol (the methods + wire types + routes),
  web/console (the provider card + the binding SELECT).
- **RFC:** §6.4, §6.16, §5.2, §6.15, §7.
- **Deps:** 166 (the named broker pins every sink so the descriptor needs no
  URL; the corrected audit ordering), 167 (the owner-tagged provider set +
  owner-scoped reconcile —
  makes uninstall cross-tenant-safe, FAIL 4/6), 168 (the sibling editor surface),
  142 (D-271), 154 (D-285), 148 (D-278), 92f, 92h, 152, 118. Related, NOT a dep:
  the parked 92k.
- **What it delivers:** the provider-install half of HA-15, de-scoped from the
  first-cut Phase 167. The writable descriptor carries ZERO URLs —
  `{name, credential_broker, scopes?}` — honouring D-300's invariant: the token
  endpoint, allowed downstream hosts, and audience/scope ceiling are pinned at
  boot on the named broker (166). A reflective test asserts no field is a
  URL/env-var name, and `DisallowUnknownFields` rejects `token_url` / `auth_url`
  / `client_id_env` / `client_secret_env` / `remote` by name (no decoy fields —
  WARN 17); empty `credential_source` is a loud reject (WARN 16). Backing it:
  `auth.ProviderSet` — a NEW `providers.go` type (NOT the OAuth driver registry
  `registry.go` — WARN 9; NOT a §4.4 driver seam, it holds instances, no
  `drivers/prod` registration — WARN 20), bare-name resolution + owner-tagged
  reconcile (167), consulted by the
  MCP attach path; the catalog builder keeps its boot map (D-292). Install and
  uninstall ship together (`set_oauth_provider` / `remove_oauth_provider`);
  uninstall CLOSES the provider so a bound connection's next call fails LOUD
  (verified `tokenexchange.go:360` → `mcp.go:1166-1182`); rollback runs the same
  uninstall through the reconcile seam; the deliberately-breaking uninstall is
  defensible ONLY because 167 keys the set per triple (a tenant-B reconcile never
  closes tenant-A's provider — FAIL 6). The binding half is Console-only: the
  wire already carries `oauth_provider`, the Console drops it
  (`state.svelte.ts:912-923`) — 169 stops dropping it and pins the round-trip.
  Departs from brief 09 (RFC 7591 / operator-burden) — recorded in D-303. 92k/92m
  pointer notes written. Full D-223/D-209 regen; `ProtocolVersion` unbumped. See
  `docs/plans/phase-169-oauth-provider-install-binding.md`.
- **Decision:** D-303.

---

---

### Phase 170 — Same-origin MCP OAuth-discovery dial (HA-19)

- **Subsystem:** internal/tools/auth (the discovery walker's dial policy) —
  no wire, config, CLI, or Console touch.
- **RFC:** §6.4 (the MCP southbound edge), §5.2 (the discovered requirement
  surfaced as Protocol data), §7 (the Console lens that renders it).
- **Deps:** 164 (D-297 — the discovery walker this fixes), 28 (the MCP
  southbound driver whose operator-declared `ServerURL` is the pinned dial
  target), 30 (tools/auth home). Related, NOT structural: 168 (HA-15 — the
  `oauth_discovery_allowed_origins` allowance WRITE surface this preserves)
  and the flow-executing siblings 85b / parked 92p (reserved D-246) that reuse
  the corrected walker.
- **What it delivers:** discovery that COMPLETES in the ordinary self-hosted
  posture. Phase 164's report-only discovery walker can never complete against
  an MCP server on localhost / a container-compose network / a private VPC: it
  dies one hop earlier than the correct allowance boundary — on the RFC 9728
  protected-resource-metadata fetch back to the very server the runtime already
  dials successfully for tool calls. Root cause: the walker enforces SSRF
  policy in two places that disagree. The per-hop policy (`validateHop`,
  `internal/tools/auth/discovery.go:427`) correctly PERMITS the same-origin
  protected-resource hop even to a private address (`sameOrigin` at `:435`;
  `case StepProtectedResource` at `:442` refuses only cross-origin; the
  cross-origin IP-literal refusal fires only `if !sameOrigin` at `:455`). But
  the dial-time backstop — `net.Dialer.Control` in `NewDiscoverer`
  (`:260-281`) — unconditionally refuses every resolved private/loopback IP
  via `isPrivateIP` (`:276-278`), with no same-origin exemption and no
  knowledge of which hop it serves, so the hop `validateHop` just approved
  dies at connect. The only relaxation, `WithPrivateNetworkAccessForTest`
  (`:232`), panics outside a test binary — there is NO production path to the
  working dial, and every positive-path chain-walk test uses that test-only
  escape, which is exactly why the gap shipped green (§17.8). The fix aligns
  the backstop to the per-hop policy: `Control` → `ControlContext` (Go 1.20+;
  the module is 1.26) so the dialer runs post-DNS-resolution AND reads a
  per-fetch `dialPin` from ctx naming the step, the connection's
  operator-declared `ServerURL` host (a trusted origin, NOT the
  attacker-influenceable `resource_metadata` URL), resolved ONCE at walk start
  into a resolved-IP SET (`pinnedIPs`) + `pinnedPort`; a private resolved
  address is permitted for EXACTLY the same-origin protected-resource hop when
  `resolvedIP ∈ pinnedIPs && resolvedPort == pinnedPort`, and nothing else.
  **The crux is DNS rebinding:** "same-origin" is computed on the origin
  STRING, but the SSRF defence is by resolved IP — so an attacker-influenced
  `resource_metadata` URL (from the `WWW-Authenticate` challenge) or a
  same-origin-by-string redirect whose host resolves to a DIFFERENT private IP
  must still be refused; resolved-IP-set membership is the gate. The PORT is
  part of the pin to refuse intra-host port SSRF (a same-origin
  `302 → {pinnedIP}:22`/`:6379`). A SECOND guard is also aligned:
  `validateHop`'s https-off-loopback refusal (`discovery.go:449-452`; loopback =
  localhost/127.x/::1 only) is extended so a plain-HTTP NON-loopback MCP server
  (compose service name / k8s in-cluster address — two of HA-19's four named
  postures, which config permits) discovers on the same pinned predicate; a
  non-pinned / cross-origin plain-HTTP target still returns `ReasonNotHTTPS`.
  The transport sets `DisableKeepAlives: true` so every dial (incl. redirects)
  re-enters the gate. Preserved (each a named test): cross-origin hops keep the
  private-IP refusal + rebinding defence; the relaxation is gated by
  `step == StepProtectedResource` (an AS hop whose target IP is IN the pin is
  still refused); a redirect off the pinned target is refused at its dial by
  `ControlContext` (NOT a `validateHop` re-entry); the
  `oauth_discovery_allowed_origins` allowlist still gates every
  authorization-server hop (`needs_allowance` stays the intended halt — HA-15 /
  Phase 168's surface); bounded redirects / 8 KiB size cap / 5 s timeout /
  no-proxy / credential-strip-on-redirect unchanged; RFC 7591 registration
  reported never invoked (D-297 report-don't-follow); the runtime never runs
  the flow or holds a token (D-271); and NO production `allowPrivate` knob is
  introduced — `WithPrivateNetworkAccessForTest` stays test-only. §17.8 tests
  exercise the PRODUCTION construction path (no test-only escape) against the
  Phase 164 spec-derived RFC 9728/8414 fixtures across ALL THREE self-hosted
  postures (loopback-hostname + IP-literal + plain-HTTP non-loopback service
  name): same-origin private hop completes, cross-origin private hop refused,
  AS-hop-to-pinned-IP refused, rebind-to-a-different-private-IP refused,
  different-port refused, redirect-off-target refused. A
  SECURITY-SENSITIVE relaxation, planned with the rebind crux and the
  preserved-property list binding. Runtime-internal dial policy — no wire,
  config, CLI, or Console change; `ProtocolVersion` unbumped; no D-223 / D-209
  churn. §18 exempt: operator steps are unchanged (the previously-blocked data
  path merely becomes reachable; the `observe-with-the-console` MCP-Connections
  prose already describes the now-restored behavior). See
  `docs/plans/phase-170-same-origin-discovery-dial.md`.
- **Decision:** D-304.
- **Status:** Shipped (v1.14). Deviation (§4.3): the dial gate is extracted to a
  pure `evalDialGate(host, port, allowPrivate, *dialPin)` so the full pin
  decision table is unit-covered without DNS, and a TEST-ONLY
  `WithResolverForTest(*net.Resolver)` option (panics outside a test binary,
  relaxes no guardrail) injects the name resolver used by BOTH the walk-start
  pin resolution and the dialer, so the plain-HTTP non-loopback compose posture
  is exercised on the production dial path. Both are internal/test-only; no
  exported production surface changed.

---

### Phase 171 — `events.aggregate` durable-driver parity + conformance-matrix closure (HA-18 + HA-20)

- **Subsystem:** internal/events (the aggregator, the `HistoryReplayer` fan-in,
  the shared conformance suite, both drivers), internal/protocol/types (the one
  additive `Truncated` field), internal/protocol/transports/stream (the aggregate
  handler threads the server-derived `widened` decision), web/console (the wire
  mirror).
- **RFC:** §6.13 (the one bus this reads), §5.2 (the Protocol read surface),
  §6.5/§4 (the identity + elevated-scope rules), §7 (the Console lens).
- **Deps:** 72a (`events.aggregate` + the aggregator), 162 (`events.list` /
  the `HistoryReplayer.ListWindow` fan-in this reuses; D-294), 124 (gap-free
  durable log), 125 (`state.history` / `HistoryReplayer` seam; D-254), 118 (D-223).
- **What it delivers:** the instance AND its class in one phase (the D-283
  pattern). **HA-18:** `events.aggregate` 500s on the durable driver on every
  call — the aggregator replays through the per-session `Replayer.Replay` path
  with a session-less `Filter{Admin:true}` the durable driver correctly refuses
  (`ErrIdentityScopeRequired`), while inmem honours it, so the method works in
  dev and 500s in prod. Fix: source the window snapshot from the same
  `HistoryReplayer` cross-session fan-in `events.list` uses (one read ⇒ one
  `audit.admin_scope_used` on the widened path; the effective `Since/Until`
  threaded onto the substrate query; a generous bound keeps memory equal to
  today's whole-ring materialization; a window past the bound returns PARTIAL
  buckets with the additive `EventAggregateResponse.Truncated = true` UNIFORMLY
  on both drivers, NEVER a 400 — the earlier `ErrAggregateWindowTooLarge → 400`
  was the review's F1 FAIL, a 400-on-durable / 200-on-inmem fork), threading the
  handler's server-derived `widened` decision (D-299) computed on the RAW
  pre-fold filter as a Go input — never a wire field — which also fixes a latent
  audit-integrity bug (the hardcoded `Admin:true` emits `admin_scope_used` TODAY
  on inmem for every aggregate); the new substrate also EXCLUDES bus-internal
  notice types the old path counted (a latent-bug fix owned in D-305).
  `events.aggregate` and `events.list` now agree by construction on the
  session-less admin read; `Replayer.Replay` is unchanged. **HA-20:** close the
  conformance-matrix hole that let HA-18 ship — a driver-parametrized aggregate
  scenario over a multi-tenant fixture (≥2×2×2) with an explicit isolation
  assertion (parity ≠ isolation) + a four-method parity leg
  (`events.aggregate`/`events.list`/`events.subscribe`/`state.history` against
  every registered driver → same answer or same named sentinel) + a registry
  gate that blank-imports `internal/drivers/prod` (a self-registered driver with
  no conformance run fails the build). The thesis, made executable: a driver
  difference may change WHAT a method returns (retention depth via `truncated`,
  an observed horizon) but NEVER WHETHER it works — differences stay DATA/named
  sentinels, never a 500, never a status fork, never normalized. ONE additive
  wire field (`EventAggregateResponse.Truncated`; `ProtocolVersion` stays 0.1.0;
  full D-223/D-209 lockstep + §18 SKILL.md). §17.1 integration test:
  `events.aggregate` 200-not-500 on durable over a real StateStore, identity
  propagation, ≥1 failure mode, truncated-at-bound. See
  `docs/plans/phase-171-events-aggregate-durable-parity.md`.
- **Decision:** D-305.
- **Status:** Shipped.

---

### Phase 172 — `events.aggregate` origin-anchored (epoch-aligned) bucket grid (HA-16)

- **Subsystem:** internal/protocol/types (additive `anchor` field),
  internal/events (the aggregator's boundary flooring), web/console (the
  aggregate consumer passes the anchor + caches by coordinate).
- **RFC:** §6.13, §5.2, §7.
- **Deps:** 171 (the aggregate must work on the durable driver before its grid
  matters), 118 (D-223 lockstep machinery).
- **What it delivers:** the aggregator lays its grid from the wall-clock instant
  at handler entry, so `Window % Bucket == 0` constrains bucket-series LENGTH but
  never ORIGIN — two calls at two instants return two different boundary sets, a
  `bucket_start` is not addressable twice, and no consumer can cache a bucket.
  Add an OPTIONAL `anchor` on `EventAggregateRequest` (zero ⇒ today's
  clock-anchored grid); when set, boundaries floor onto `anchor + k·Bucket`, so
  two calls with the same anchor share boundary instants (a bucket is
  re-requestable) and a cold N-bucket fill is one call. Passing the Unix epoch
  yields a globally-shared grid. Chosen over explicit `{since,until,bucket}` as
  the smallest additive surface that composes with `Window`/`Bucket` and does
  not duplicate the `Filter.Since/Until` clamp. No change to bucket contents,
  redaction, retention, or identity axes; `ProtocolVersion` stays 0.1.0; full
  D-223 lockstep + D-209 regen + §18 `use-the-harbor-protocol` SKILL.md in the
  same PR. See `docs/plans/phase-172-events-aggregate-epoch-grid.md`.
- **Decision:** D-306.
- **Status:** Shipped.

---

### Phase 173 — `events.aggregate` per-tenant attribution for admin-widened reads (HA-17)

- **Subsystem:** internal/protocol/types (additive `by_tenant` +
  `counts_by_tenant` fields), internal/events (the second accumulator + the
  entitled-set guard), internal/protocol/transports/stream (pass-through with
  the server-derived widened decision), web/console (the fleet-view breakdown).
- **RFC:** §6.13, §5.2, §6.5, §4, §7.
- **Deps:** 171 (the aggregate must work on durable and carry the server-derived
  `widened` decision before attribution rides it), composes with 172 (same
  response), 118 (D-223).
- **What it delivers:** an aggregate bucket is a bag of scalars with NO tenant
  attribution, so — unlike a row read where each row carries its tenant — a
  consumer cannot verify an admin-widened count against the `Filter.TenantIDs`
  it asked for; the runtime's honouring of the filter IS the entire tenant
  boundary, a single point of enforcement with no downstream check. Add opt-in
  `by_tenant` → `EventBucket.CountsByTenant` (tenant → event_type → count)
  alongside the totals, returned ONLY for admin-widened reads (verified
  admin/console:fleet, server-derived per D-299); attribution keys ⊆ the
  authorized (named-or-folded) `Filter.TenantIDs`, `Counts` and `CountsByTenant`
  scoped to the IDENTICAL set by construction (no per-tenant entitlement
  mechanism — the scope grants are global binary fan-in authorizations; the body
  selects tenants, the scope authorizes the fan-in); an unelevated caller gains
  attribution for nothing new. The per-bucket invariant `Σ counts_by_tenant ==
  counts` proves it is a pure re-projection, not a looser read path. Makes the isolation boundary
  independently verifiable on aggregates the way it already is on rows (§6
  defence-in-depth). No payloads, no new identity axes; `ProtocolVersion` stays
  0.1.0; full D-223 lockstep + D-209 regen + §18 SKILL.md in the same PR. See
  `docs/plans/phase-173-events-aggregate-tenant-attribution.md`.
- **Decision:** D-307.
- **Status:** Shipped.

---

### Phase 175 — Fleet-scoped retention horizons (HA-23)

- **Subsystem:** internal/protocol/types (the additive `RetentionHorizon.scope`
  field), internal/protocol (the `handleHealth` server-derived `widened`
  decision + the widened-path audit), internal/runtime/posture (the
  `RetentionProvider` scope-aware read), internal/tasks + internal/sessions (an
  optional identity-free `OldestRetainedAt` reader), web/console (the fleet-lens
  horizon).
- **RFC:** §5.2, §5.5, §6.1, §6.13, §6.14, §6.16, §7.
- **Deps:** 163 (HA-14 / D-296 — the retention block + `RetentionProvider` this
  phase extends), 118 (D-223 lockstep). Composes with the D-308 events-fold work
  (Phase 172 — the `if !widened` guard on `events.list`+`events.aggregate`
  closing HA-16+HA-21; reachable vs verifiable — orthogonal, either order).
- **What it delivers:** `runtime.health`'s `retention[]` block (HA-14/D-296)
  builds its three horizons at three DIFFERENT scopes — `events` runtime-wide and
  identity-free (correct, unchanged), `tasks` scoped to the caller's full triple
  (its session), `sessions` scoped to the caller's tenant — and OMITS an absent
  surface, so the same no-entry wire shape means BOTH "surface retains nothing"
  AND "caller has nothing in scope." A fleet coordinator polling under a
  dedicated `svc:` service identity (owns no sessions/tasks) therefore observes
  ONLY the `events` horizon; the `tasks`+`sessions` horizons are structurally
  unobservable at fleet scope — so a cross-session windowed view silently
  undercounts (a session aged out of the sessions surface while its events remain
  is missed by the enumeration, and the correct "mark buckets incomplete when the
  sessions horizon < window" guard is inert because the sessions horizon can't be
  read at fleet scope). This phase makes the `tasks`+`sessions` horizons
  OBSERVABLE at runtime-wide scope to a verified admin / `console:fleet` caller
  (server-derived from the verified session per D-299, NEVER the request body),
  riding `runtime.health` — no new method, no new capability bit, no relaxation
  of the ordinary caller's per-tenant/per-session fold (which stays a fail-closed
  control). The runtime-wide read goes through an optional identity-free
  `OldestRetainedAt` reader on the tasks registry + session lister (mirroring
  `events.RetentionReporter`, discovered by type assertion — no `Supports*`
  ceremony); the widened path emits one fail-loud `admin_scope_used`. Second, it
  makes absence REPRESENTABLE: an additive `RetentionHorizon.scope`
  (`runtime`/`tenant`/`session`) plus always-emitting an entry for the three
  known surfaces, so a consumer distinguishes "unobservable at your scope" from
  "surface retains nothing" and degrades honestly (the HA-18/20/21/22/23
  through-line — never let an unobservable scope masquerade as an empty result;
  the shared class rule is recorded at D-311 if the sibling HA-22 plan authored
  it, else stated inline here). Composes with the D-308 events-fold work
  (Phase 172, closing HA-16+HA-21): that work makes the session-less
  cross-session enumeration REACHABLE, this makes its completeness VERIFIABLE.
  No change to the `events` horizon, retention itself (Harbor has no retention
  knob), redaction, or the D-296 decided-NO counters/metrics TSDB. One additive
  wire field; `ProtocolVersion` stays 0.1.0; full D-223 lockstep + D-209 regen +
  §18 SKILL.md in the same PR. See
  `docs/plans/phase-175-fleet-retention-horizons.md`.
- **Decision:** D-310.

### Phase 176 — Session reopen: re-activate a closed session so a consumer chat resumes

- **Subsystem:** internal/sessions (the reopen re-activation branch on
  `Open`/`EnsureOpen`; the `ErrReopenAfterErase` sentinel + `session.reopened`
  event; the durable erasure tombstone in the cascade), internal/protocol (the
  retired reopen-after-close mapping → `ErrSessionReopenAfterErase`),
  internal/runtime/serve (the ensurer-adapter sentinel translation), web/console
  (the resume affordance + `session.reopened` list refresh).
- **RFC:** §6.9 (amended — D-312), §5.2, §6.13, §7.
- **Deps:** 130 (`session.erase` — the terminal exception + the erasure
  ledger/tombstone scope reopen checks against), 155 (the erasure-audit-integrity
  cascade the tombstone folds into), 125/D-254 (`state.history` — the read path a
  reopened conversation reduces from, and the integration probe proving history
  intact), 73c (Sessions page consumer), 106 (Playground consumer), 118 (D-223).
- **What it delivers:** AMENDS a Settled RFC decision. RFC §6.9 was
  "reopen-after-close is forbidden — clients open a new session"; the
  consumer-chat / white-label model needs conversations always-resumable (a user
  returns days later on the SAME conversation). Reopen is clean because close/GC
  reap the session RECORD, not the DATA: `Registry.Close` + the GC sweep mark the
  record `Closed=true` and drop the live `openSessions` entry but leave the
  durable events/state/memory intact (the GC explicitly guards against
  resurrecting the record), the durable event log is gap-free + UNTRIMMED in V1
  (no TTL/cap), the StateStore has no retention sweep. `Open`/`EnsureOpen`/`start`
  on a `Closed=true` record RE-ACTIVATES it — clears `Closed`/`ClosedAt`/
  `ClosedReason`, preserves the immutable identity AND `OpenedAt` (a reopen under
  a DIFFERENT tenant is still `ErrSessionIDReuse`, invariant 3 unchanged), stamps
  `LastReopenedAt`, refreshes `LastSeen`, re-adds to `openSessions`+catalog, lifts
  the erasure fence, emits a new content-free `session.reopened` SafePayload —
  history resumes intact. The GC hard cap now measures from
  `max(OpenedAt, LastReopenedAt)` (was `OpenedAt` alone), so a reopened old
  conversation is not re-reaped on the next sweep (FAIL-1 — `OpenedAt` is NOT
  refreshed, it is the erasure lifecycle discriminator). THE ONE terminal
  exception: an ERASED session (`session.erase`) fails loud `ErrReopenAfterErase`
  (new sentinel; never a silent empty-start — §5, §7), detected via an O(1)
  point-`Load` of a pending erasure ledger (in-flight) OR a new durable
  content-free erasure tombstone (retained — sibling of the `session.erased`
  record-of-fact; required because the pending ledger is deleted on erasure
  success so it alone can't make a converged erasure terminal; a StateStore
  point-read fails closed where an event-history scan would fail open). The gate
  fires on BOTH the closed-record branch AND the not-found / fresh-create
  fall-through — a converged erase removes the record so a naive reopen would mint
  a fresh empty session (FAIL-2). Race-safe by write-happens-before-delete: the
  tombstone `Save` (success-critical — a failure fails the erasure loud, never
  proceeding to `deleteLedger`) completes before the ledger delete, so
  `isErased == ledger ∨ tombstone` holds with no gap under the shared `r.mu`.
  The tombstone `Save` is UNCONDITIONAL per terminal `completeErasure` (outside
  the `recordAlreadyEmitted` skip guard, WARN-B); `isErased` fails CLOSED on a
  non-NotFound Load error (WARN-C). All §6 isolation preserved (identity-mandatory
  and immutable, `ErrIdentityMismatch`; no downgrade knob). Reopen-after-erase maps
  to a NEW machine-branchable wire code `CodeSessionErased` (`"session_erased"`,
  HTTP 409; NOT a `ProtocolVersion` break, WARN-A) so a consumer chat branches on
  `code` for the deleted-conversation path; adding the code drives the D-209 regen
  that FIXES the now-false "reopen-after-close is forbidden" prose in the errors
  generator + the `auth-and-identity.md` choreography (the re-review docs FAIL).
  One new event + one new error code = full D-223 lockstep + D-209
  events.md/errors.md regen. §13 same-wave consumer: Playground/Sessions resume a
  closed session + refresh on `session.reopened` + branch on `session_erased`.
  §17.1 integration test (real durable+inmem drivers +
  `CascadeEraser`: close→reopen→history intact on durable; erase→reopen loud;
  cross-tenant/identity-mismatch reject; ≥1 failure mode; `-race`) + registry
  D-025 stress extended N≥100 incl. the reopen-vs-erase race. See
  `docs/plans/phase-176-session-reopen.md`.
- **Decision:** D-312.

### Phase 177 — Projection-completeness gate + the populate/remove surfaces (HA-24)

- **Subsystem:** internal/protocol/projectioncheck (the new gate + registry),
  internal/tasks/protocol (`HasPendingApproval` / `BackgroundAcknowledged`),
  internal/runtime/serve (the tasks `serve.Enricher` un-stub),
  internal/runtime/flow + internal/runtime/flow/protocol (`RunRecord.Tokens` +
  `TokensUsed`), internal/memory/protocol (dead TTL facet/aggregate removal +
  `agent_ids` loud-reject), internal/sessions/protocol (register the 174 surface
  into the gate — serialized after 174), internal/protocol/types (memory wire
  removal), internal/tools/protocol (interim honest-gating), web/console.
- **RFC:** §5.2, §6.1, §6.4, §6.6, §6.8, §7.
- **Deps:** 174 (HA-22 — the sessions instance + D-311; the gate must cover the
  sessions surface 174 fixes, and the tasks enricher cost work coordinates with
  174's session-cost aggregation), 08, 54, 60/72a, 73c/107a. All shipped/landing
  in the same wave.
- **What it delivers:** HA-24 — the CLASS that HA-22/174 is one instance of, on
  three more surfaces plus a mechanical class-closer. Declared-but-never-assigned
  wire fields with filters/aggregates over them return FALSE ABSENCE. TASKS:
  `TaskRow.HasPendingApproval` is read by the `tasks.list` filter but never
  assigned by `projectRow`, so `has_pending_approval:true` returns an empty page
  on a fleet with open gates — populated at projection time from the
  approval/pause registry; `BackgroundAcknowledged` (no filter) represented; the
  WIRED tasks `serve.Enricher` is a STUB returning a zero parent-session ref +
  zero cost rollup (even the structurally-right surface ships zeros) — un-stubbed.
  FLOWS: `budgetConsumption` never sums `TokensUsed` (a non-omitempty field) —
  `RunRecord.Tokens` added (symmetric with `CostUSD`) and summed. MEMORY: V1 has
  NO TTL, so `has_ttl_expiring` and BOTH `expiring_in_1h` fields (on
  `MemoryAggregates` and `MemoryHealthAggregate`) are structurally dead — REMOVED
  (breaking wire-shape change, D-223/D-209, RFC §8 exemption stated: always-empty
  → no live consumer, plus the 0.1.0 within-version precedent); `agent_ids`
  LOUD-REJECTS in V1
  (`ConversationTurn` carries no producer identity; populate deferred to a
  follow-up). SESSIONS: this phase adds the sessions registration (174 predates
  `projectioncheck`; serialized after 174). TOOLS: split to 178 — no production
  `Annotator` exists, so this phase honestly gates the annotator-backed surface
  behind ONE capability toggle (facet filters loud-reject; catalog aggregates
  carry an `aggregates_partial` marker, never a silent 0) pending 178. THE GATE
  (the class-closer) — TWO halves: a registry-gated projection-completeness check
  where every surface self-registers a `ProjectionContract` (probe +
  filtered/sorted/aggregated field-set + reason-carrying honest-omission
  allow-list + a prod-wiring-test name). Half A (never-assigned) reflects each
  probe and FAILS on a filtered field left zero and not allow-listed (and on an
  empty allow-list reason); Half B (never-wired — the variant that motivated the
  band) requires each surface to register a prod-wiring test through real `mux`
  wiring, so a forgotten `WithX` (passes Half A's fake-backed probe, ships zeros
  in prod — the tools bug) FAILS. A surface-coverage check asserts every known
  surface is registered — mirroring the events `RegisteredDrivers()` gate (D-305).
  The gate is the primitive; the surface fixes + 174's session fix + the tools
  interim-gating are its consumers so it lands green (§13). Fields that are
  populated or not operated-over (`FlowBudget.TokenCap`, the
  `MemoryItem.AgentID`/`.ExpiresAt` ROW fields, `TaskParentSessionRef.SessionID`)
  need NO allow-list entry. No `ProtocolVersion` bump. See
  `docs/plans/phase-177-projection-completeness-gate.md`.
- **Decision:** D-313.
- **Status:** Pending.

---

### Phase 178 — Tools production Annotator (HA-24)

- **Subsystem:** internal/tools/protocol (the new production `Annotator` +
  filter/projector un-gating), internal/runtime/serve (the `WithAnnotator`
  wiring), web/console.
- **RFC:** §5.2, §6.4, §6.15, §7.
- **Deps:** 177 (D-313 — the projection-completeness gate + the annotator-wired
  capability this phase flips on; the tools surface is registered by 177), 28
  (MCP registry / DisplayMode negotiation), the `tools/auth` + `tools/approval`
  subsystems, 60/72a (events read-side). All shipped.
- **What it delivers:** the tools leg of HA-24 and the second member of the D-313
  band. The tools catalog projector reads OAuth/approval/last-used/metrics/
  content-stats/display-modes through the optional `Annotator` seam, but no
  production `Annotator` is ever wired — `mux.go` supplies only
  `WithLoadingResolver` and the sole implementer is a `fakeAnnotator` test double
  (§17.8) — so in prod the tools facets/search/aggregates operate over structural
  defaults (the never-wired variant). This phase assembles a production
  `Annotator` (a §4.4-shaped concrete behind the shipped seam) reading each
  annotation from its owning subsystem, wires it at `mux.go` via
  `WithAnnotator(...)`, flips the Phase-177 annotator-wired capability on so the
  gated facets go live, populates `Tool.Version`, and lights up the inert admin
  write path (`tools.set_approval_policy` / `tools.revoke_oauth`, which returned
  `ErrAdminUnsupported` because no annotator implemented the setter/revoker seams
  the projector already delegates to) — writes route back through
  `tools/approval` / `tools/auth` with audit, never a Console shadow store
  (D-061). With the annotator wired, the D-313 gate now enforces the tools fields
  (their allow-list entries removed). No new Protocol method, no `ProtocolVersion`
  bump — the fields are already declared; the capability flips unwired→wired. See
  `docs/plans/phase-178-tools-annotator.md`.
- **Decision:** D-314.
- **Status:** Pending.

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
