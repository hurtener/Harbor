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

## [Unreleased]

(Next up: completing the runtime add-connection OAuth-resume attach + connection
run-start reconciliation — issue [#375](https://github.com/hurtener/Harbor/issues/375);
identity-scoped `StateStore` enumeration to bound the user-scope revision scan —
issue [#396](https://github.com/hurtener/Harbor/issues/396); and the generated
per-domain Protocol wire-type modules + the shared chat-module extraction — the
D-093 / D-091 follow-ons.)

## [1.6.0] — 2026-06-25

Session hydration and user-scope agent configuration. This release adds a
windowed session-state history surface backed by a durable event bus that
rehydrates across a Runtime restart, a per-user durable agent-configuration tier
that layers between the admin config and the ephemeral session overlay, and a
connect-time wire-surface drift signal for Protocol clients. The Harbor
Protocol's wire surface grows (a new `state.history` surface and an additive
`runtime.info` field) but stays semver `0.1.0` — every change is additive or
corrective, and no prior INTENDED behavior changes.

### Added

- **Session-state history.** A new Protocol surface (`POST /v1/state/history`)
  for windowed, identity-scoped replay of a session's event history: tail-first
  paging that scrolls back to the head, with offloaded heavy payloads surfaced
  by artifact reference. Cross-tenant reads return `404` (no existence leak) and
  an unidentified read returns `401`.
- **Durable event-bus rehydration across restart.** The durable EventBus
  rehydrates its monotonic sequence counter from the persisted log on
  construction, so a Runtime restart continues a session's event sequence above
  its pre-restart high-water mark instead of resetting to 1 — the foundation
  that lets `state.history` return a gap-free, strictly-monotonic window across
  a restart boundary.
- **User-scope agent configuration.** A per-user durable config-variant tier
  layered between the admin/agent config and the ephemeral session overlay. A
  user owns a standing variant via `agent_config/user/set_revision`
  (set / list / diff / rollback), keyed by the real `(tenant, user)` and
  isolated per user — never by `agent_id`, which is a key, not an isolation
  principal.
  - **Prompt layers** compose at run start with the precedence admin Base >
    admin User > user-durable > session User.
  - **Tool exposure** projects as a grow-only three-set union
    (admin ∪ user ∪ session): a user can disable more tools/servers, never
    re-widen past the admin-provisioned palette.
- **Wire-surface drift detection.** `runtime.info` now returns a
  `wire_surface_digest` — a coarse, stable `sha256:` fingerprint of the
  Protocol's name-level wire surface (version + method / error / capability /
  wire-type names; field shapes and event-type names are deliberately excluded).
  The committed wire manifest is stamped with the same digest, so a Console (or
  any client that vendors the manifest) raises a loud drift signal at
  connect-time when it was built against a different Protocol surface. Additive
  field; the Harbor Protocol stays `0.1.0`.

### Changed

- **Agent-config write serialisation is memory-bounded.** The per-owner
  read-modify-write lock is striped across a fixed shard array instead of an
  unbounded per-owner map, so the lock memory stays constant regardless of how
  many users ever write a durable variant. Same-owner writes still serialise.

### Fixed

- **Agent-config keying fails closed on an unrecognised scope.** The config
  scope discriminator is matched explicitly; an out-of-range value now returns
  an error instead of defaulting to the more-privileged agent tier.

### Internal

- A composing wave-end end-to-end test exercises the durable-bus-restart →
  `state.history` rehydration seam together with the user-scope projection (real
  drivers across every seam, identity propagation, ≥1 failure mode per track,
  and an N≥48 concurrency stress under the race detector) — closing the one
  cross-phase seam no per-phase test covered.
- godoc hygiene on operator-visible surfaces, a scan-cost note on the user-scope
  `ListRevisions` path (tracked for an identity-scoped enumeration in
  [#396](https://github.com/hurtener/Harbor/issues/396)), and test coverage
  tightened to the phase-plan claims (prompt-layer subsets, the tool-exposure
  union's order-independence, and the Console wire-drift match branch).

## [1.5.1] — 2026-06-23

Runtime hardening, observability, and a memory-context fix. This release bounds
the runtime's long-lived state, adds first-class observability (pprof + standard
collectors + runtime gauges, surfaced in the Console), and fixes a memory
context-budget bug that could poison a long-lived session. Every change is
additive or corrective: the Harbor Protocol stays at `0.1.0`, all new
configuration is optional, and no prior INTENDED behavior changes.

### Added

- **Runtime observability foundation.** Standard Go + process collectors on a
  per-instance Prometheus registry (`/metrics`), plus Harbor runtime gauges
  (`harbor_runtime_{active_runs,engine_capacity_entries,governance_cache_entries,events_dropped}`)
  registered on the `MetricsRegistry` so they reach `/metrics`, OTLP, AND the
  already-shipped `metrics.snapshot` projection — no new Protocol method. A pprof
  debug listener is available for live profiling, gated to a loopback address via
  `server.debug_addr` / `HARBOR_DEBUG_ADDR` and NEVER mounted on the
  Protocol/Console surface.
- **Console runtime gauges.** The Live Runtime health panel surfaces the runtime
  gauges through the existing `metrics.snapshot` (typed Protocol client; no new
  method or page).
- **Operator-tunable memory compaction.** `memory.recent_turns` sets the verbatim
  recent-turn window for `rolling_summary` (default 4); `memory.summarizer.model`
  runs compaction on a chosen model (unset → the main LLM); `memory.summarizer.prompt`
  appends operator instructions to the baseline summariser prompt (extends, never
  replaces it — so the role framing and conciseness guarantees are preserved).

### Changed

- **`rolling_summary` now enforces its configured `budget_tokens`.** The strategy
  keeps the assembled context within the token budget — on write it compacts
  oldest-first into the rolling summary (threading the prior summary so it is never
  discarded), and on read a deterministic clamp guarantees the emitted patch fits
  the budget even if the summariser is degraded. `budget_tokens: 0` preserves the
  prior unbounded behavior. (D-242.)
- **The context-window safety net's byte check is scoped to offloadable content.**
  The 32 KiB heavy-content check now governs tool/MCP results and binary inputs
  (which offload to an `ArtifactStub`); legitimate conversation text — including a
  long rolling summary — is governed by the token-window guard instead, not the
  byte threshold. (D-241.)
- **Governance cost ceilings and rate limits are keyed by identity, not per-run**
  (RFC §6.15) — closing a per-run ceiling-bypass and bounding the governance cache.
  (D-249.)

### Fixed

- **Long-lived sessions no longer poison themselves.** A `rolling_summary`
  session's accumulated context could cross the heavy-content byte threshold and
  then fail EVERY subsequent run at planner step 0 with a context-leak error; the
  budget enforcement + safety-net scoping above resolve it. Surfaced by runtime
  profiling.
- **Bounded runtime retention.** The engine's per-run streaming-capacity map and
  the governance cache are now reaped on a true run-end idle-TTL sweep instead of
  growing for the process lifetime. (D-248.)
- **Clean shutdown under a hung summariser.** The `rolling_summary` recovery loop
  runs under a cancellable context cancelled on `Close()`, so a hung or degraded
  summariser can no longer pin runtime shutdown. (D-250.)

### Internal

- The seven per-driver SQL migration runners (SQLite + Postgres across state,
  memory, artifacts, skills) are consolidated behind one shared
  `internal/persistence/sqlmigrate` runner — no behavior change; migrations stay
  forward-only and append-only. (D-253.)

## [1.5.0] — 2026-06-22

The **agent-config control plane**: live, audited, versioned control of an
agent's configuration over the Protocol — prompt, skills, MCP policy,
connections, and model/sampling — plus the governance LLM-control surface it
builds on (provider key rotation, tenant-default and per-agent LLM overrides)
and the durable runtime infrastructure underneath (a StateStore-backed
distributed bus and a durable TaskService). Every change is additive: the
Harbor Protocol stays at `0.1.0`, all new configuration is optional, and no
prior behavior changes. The defining property is **next-turn, snapshot-immutable
semantics** — a config edit affects only an agent's NEXT run; in-flight runs
keep the immutable view they snapshotted at run start (the D-025 concurrent-reuse
contract), so there is no mid-flight mutation, draining, or forcible teardown.

### Added

- **Agent-config control plane (the headline).** A durable, identity-scoped,
  VERSIONED desired-state registry: every edit is an immutable, content-addressed
  revision with a parent pointer; the active config is a revision pointer;
  **rollback is a repoint** (never a mutation); and a server-side **diff**
  between revisions is a read method plus an `agent.config.revised` event. The
  admin Protocol family `POST /v1/agent_config/*` covers `get` / `set_revision`
  / `list_revisions` / `diff` / `rollback` and the per-section convenience verbs
  — each verb replaces only its own section and preserves its siblings
  (the bidirectional section-merge invariant). All writes are admin-scoped and
  identity-mandatory; authority derives from the verified ctx, never the request
  body. Persisted across the in-memory / SQLite / Postgres driver triad with
  conformance parity.
- **Layered system prompt.** An operator-owned `base` layer plus an optional
  session-scoped `user` layer that composes ABOVE the base without weakening it
  (`agent_config.set_prompt_layers`) — the composition order is the security
  boundary.
- **MCP pause/resume + per-tool policy.** Pause / resume an agent's MCP servers
  and disable individual tools as next-turn projection (`agent_config.set_tool_exposure`);
  the live transport stays warm while paused. A paused server's App callbacks
  are rejected against CURRENT desired state (the planner-snapshot /
  app-call-current asymmetry), with the operator-legible "paused by an
  administrator" advisory driven by the canonical event.
- **Runtime add-connection.** Add a NEW MCP server connection live
  (`agent_config.add_mcp_connection`) — async dial → `initialize` handshake →
  discover → register, recorded as a non-secret connection revision; a
  half-attached server is never registered (fail loud). Adding a stdio server
  (an RCE surface) is allowlist-gated beyond admin, argv-form only. (An
  OAuth-required server currently PARKS on the unified pause/resume primitive;
  completing the resume-to-online attach + connection run-start reconciliation
  is tracked in [#375](https://github.com/hurtener/Harbor/issues/375).)
- **Per-agent LLM parameters.** A versioned `model` / `temperature` /
  `max_tokens` / `reasoning_effort` section (`agent_config.set_llm_params`),
  resolved at run start as **session › per-agent › tenant-wide baseline ›
  config**. A pinned model with no configured `ModelProfile` is rejected at set
  time, never silently.
- **Skills control.** Manage an agent's skill set over the Protocol
  (`agent_config.skills.{list,upsert,delete}`) — each membership change is a
  versioned revision (so skills inherit diff + rollback); a pack-origin skill is
  never silently overwritten.
- **Session-user safe subset.** A non-admin, session-scoped lower tier
  (`agent_config.session.*`): set the user prompt layer, NARROW (never widen)
  source/tool enablement within the admin-allowed set, and manage ephemeral
  personal skills — the tier is derived from the verified scope, fail-closed.
- **Governance LLM controls.** Live, no-redeploy admin control of LLM behavior:
  `governance.rotate_key` rotates the provider API key (the new key is a secret
  carried only on the request leg; events carry a fingerprint), and tenant-default
  overrides (`governance.{set,get}_tenant_overrides`) set a tenant's default
  model / sampling / additive instructions — composed UNDER the per-agent and
  per-session layers.
- **Console agent-config panel.** A consolidated `/agent-config` control panel
  (admin-gated, four-state `<PageState>`, typed Protocol client) with: a derived
  **per-revision change summary** (client-side, no extra round-trip), **diff-before-rollback**
  (a rollback renders the structured diff for explicit confirmation, never a
  blind repoint), an **atomic multi-section "Save all"** (commit edits across
  every area as ONE revision), and a per-agent **"Configure"** entry point on the
  Agents list.
- **Durable distributed bus.** A StateStore-backed `MessageBus` driver with
  cross-instance fan-out — the multi-replica substrate for the control plane.
- **Durable TaskService.** The TaskService backend over an extracted shared task
  engine, recovering in-flight tasks across a runtime restart.

## [1.4.1] — 2026-06-18

MCP Apps backend spec-conformance and a Console rollback. The Harbor Protocol
stays at `0.1.0`; the one new method and the new capability advertisement are
additive, and the reverted Console surface returns the Playground to its
working v1.4 behavior. No breaking changes.

### Added

- **MCP Apps backend spec-conformance.** The runtime advertises the
  `text/html;profile=mcp-app` UI-host capability to MCP servers during
  initialization, so a spec-conformant `ext-apps` server knows the host can
  render its `ui://` documents. `runtime.info` surfaces the host's supported
  display modes (inline / fullscreen / pip), and a new Protocol method
  `mcp.apps.tool_context` exposes the tool-context the app needs after mount
  — identity-mandatory and Protocol-proxied like every other app→host call,
  so it never escapes the `(tenant, user, session)` boundary.

### Reverted

- **Console MCP-Apps data delivery rolled back to v1.4.** The post-1.4
  Console renderer/host that pushed tool input/result into a mounted app,
  live-re-themed a running app, and wired the operator pop-to-side-by-side
  display-mode panel broke the `ui/initialize` handshake — a rendered app
  timed out before it could mount. The Console MCP-Apps surface is restored
  to its working v1.4 behavior; re-landing the data-delivery push is tracked
  in issue [#347](https://github.com/hurtener/Harbor/issues/347). The backend
  conformance above is unaffected and remains present.

## [1.4.0] — 2026-06-16

Production adoption: interactive MCP Apps in the Console, multimodal input,
a production authentication path, and a privilege-escalation fix on the
steering control surface. No breaking changes — the Harbor Protocol stays
at `0.1.0` and all new configuration fields are optional.

### Added

- **MCP Apps host** — a server's tool can declare a `ui://` resource
  (`io.modelcontextprotocol/ui`) that the Console renders as an interactive,
  sandboxed app inline in the Playground. Ships the runtime + Protocol
  surface (`mcp.servers.read_resource`, `ui://` projection, the app-tool-call
  proxy), a Console host built on the official `ext-apps` AppBridge in
  manual-handler mode (every app→host call is Protocol-proxied — an in-app
  call never escapes the `(tenant, user, session)` isolation boundary),
  three DisplayModes (inline / fullscreen tab / pip split), and inline
  discovery via the `mcp.app_available` event. App documents render on every
  artifact driver, not only S3.
- **Multimodal input** — agents accept image, audio, and video attachments.
  Provider-native upload happens inside the LLM driver; an attachment
  disposition policy (mechanism → policy, reference by default) keeps heavy
  bytes out of the context window. An `Embedder` seam adds opt-in semantic
  memory + skill retrieval, consumed by the run loop.
- **Production authentication** — `harbor serve`, the headless production
  sibling of `harbor dev`, verifies JWTs against a JWKS source
  (`identity.jwks_url` / `jwks_file`; asymmetric algorithms only) and boots
  with no dev-only surfaces (no bootstrap-token endpoint, no Console, no mock
  LLM — it fails loud at boot when a real provider or JWKS source is
  missing). Non-admin session-scoped and owner-scoped tokens are now
  authorized correctly across the steering control surface.
- **Protocol TS lockstep gate** — `make protocol-ts-gen-check` verifies the
  Console's hand-maintained TypeScript wire client against the Go single
  source (`CanonicalWireTypes`) and fails CI on any drift (a new/renamed/
  retyped wire field that the client did not mirror). It immediately caught
  and fixed real latent client drift.
- A binding **`frontend` CI job** (`svelte-check` + stylelint + eslint +
  vitest + build) and a Console chat-module **encapsulation guard** that
  keeps the chat module renderable as a self-contained library.

### Changed

- **Chat module encapsulation hardening** — the Console chat module is now
  self-contained (its own typography + theming, an injectable host identity
  and theme, a documented design-token contract) so it can render outside
  the Console shell, mechanically enforced.
- **Godoc hygiene** — internal phase / decision numbering stripped from
  operator-visible Go doc comments (the `pkg.go.dev` surface now reads as
  product API docs).
- CI runners moved to Node 24; dependency bumps (`pgx` v5.10.0,
  `aws-sdk-go-v2` S3, others via Dependabot).

### Fixed

- Image (and other) attachments now reach the agent — a stub-fetch tool-name
  mismatch dropped them silently.
- MCP App discovery reads the tool **definition**'s `_meta.ui` (spec-correct),
  so discovery fires against real ext-apps servers; heavy app documents that
  exceed the inline threshold now render via the offloaded artifact.
- A notification-producer startup race (subscriber registered after assembly
  returned) is closed.
- The preflight gate no longer silently aborts when a parallel smoke fails.

### Security

- **Steering control surface privilege escalation closed.** Caller authority
  for cancel / pause / resume / redirect / inject / approve / reject /
  prioritize is now derived from the **verified** request-context identity
  and JWT scope — never from the request body. A request can no longer
  assert its own privilege tier, the steering surface fails closed when no
  verified identity is present, and cross-tenant steering requires the admin
  scope (the cross-tenant check is now actually reachable). Latent before
  this release because only admin-scoped dev tokens existed; closed before
  any lesser-privileged token (now supported) could exploit it.

## [1.3.1] — 2026-06-11

The Protocol adoption track: the docs site gains a complete, drift-proof
Protocol surface for third-party client authors.

### Added

- **The Protocol adoption track** — the published docs site gains a complete
  top-level Protocol section for third-party client authors: a four-page
  **generated contract reference** (methods / events / errors / types)
  emitted by `cmd/harbor-gen-protocol-docs` from the canonical Protocol
  sources and drift-gated in CI by `make protocol-docs-gen-check`; the
  **executed quickstart** ("Speak Protocol in 15 minutes"), whose curl steps
  the preflight smoke runs against a live dev server on every commit; and
  **five choreography guides** — auth & identity, streaming semantics, task
  control, **the pause model** (the full intervention choreography:
  approve/reject, the OAuth callback leg, plain resume, durable pauses
  across restarts, timeout reaps — wire examples captured from a
  production-driver assembly), and **versioning & compatibility** (what the
  Protocol version promises, what a client should pin, and the
  unknown-field / unknown-method tolerance rules).
- **Build a client + conformance certification** — a worked **event-viewer
  client** at `examples/protocol-clients/event-viewer/`: ~150 lines of
  stdlib-only Go (no Harbor import) that authenticates, handshakes via
  `runtime.info`, and tails the SSE event stream, compile-gated in preflight
  so the published walkthrough cannot rot. The new
  **conformance-certification page** documents how to run the in-repo
  Protocol conformance suite against a runtime build under test and
  precisely what a pass claims.

### Fixed

- **The methods reference's Auth column now states the deployed scope gates
  exactly** — several rows over-claimed `console:fleet` cross-tenant fan-in
  on admin-only (or no-fan-in) read surfaces, and the posture rows omitted
  the note where it genuinely applies. The column is derived from a
  per-method machine-readable policy and pinned by a test that drives
  admin-only and fleet-only tokens against every noted row on a live wire,
  so the cell cannot silently drift again. The auth-and-identity scope
  table now states `console:fleet`'s full real grant set, and the
  pause-model page's wire-capture provenance header is scoped honestly
  around its transcribed OAuth leg.
- The smoke-script fleet's dead-server probes uniformly report curl's
  honest `000` and SKIP instead of failing with a confusing `000000` —
  the inline fallback shape behind it is swept tree-wide.

(Next up: the MCP Apps host — interactive, sandboxed `ui://` resources in the Console — the remaining Console polish rounds, godoc hygiene, and the resilient-flows positioning work.)

## [1.3.0] — 2026-06-10

The release where the SDK story becomes real: Harbor is now a `go get`-able
runtime for external modules, not only a binary.

### Added

- **The public SDK facade (`sdk/`)** — a curated, alias-based re-export tree
  (RFC §3.6): 20+ packages spanning identity, events, config,
  tools, llm, memory/state/artifacts/skills, planner, tasks, steering,
  dispatch, runctx, and the one-call `assemble.Assemble` stack fan-out.
  External-module importability is enforced by a standing preflight gate that
  scaffolds and compiles a tool-declaring agent outside the module.
- **`assemble.Assemble`** — the exported, error-returning, dependency-ordered
  runtime assembly (D-197); `bootDevStack` and `harbortest/devstack` are thin
  callers of the one implementation.
- **`harbor skill import` / `harbor skill rm`** — CLI ingestion for Skills.md
  playbooks over the exported `importer.ImportAndStore`.
- **Governance enforcement** — populated `governance.identity_tiers` now
  enforces cost ceilings, rate limits, and max-tokens caps; the
  latent-by-default posture is preserved.
- **Durable pauses** — pause checkpoints carry the run trajectory and survive
  a Runtime restart; a max-park sweeper reaps expired and crash-orphaned
  pauses (`DecisionTimeout`, `StateStore.ListKind`).
- **Tool-OAuth completion** — `auth.CallbackHandler` closes the
  pause→callback→resume choreography.
- **Trajectory compression** — long runs compress under
  `planner.token_budget` via the LLM-backed summariser.
- **Production telemetry assembly** — the redactor-mandatory Logger, the
  engine RunErrorHandler, and `BridgeBusToTracer` are wired by the assembly.
- **The published docs site** — VitePress on GitHub Pages, built from the
  canonical in-repo docs.
- The skills `<skills_context>` prompt block is produced by the
  capability-filtered, redacted virtual Directory with functional operator
  pinning.

### Changed

- The runtime's production semantics were re-homed out of `cmd/harbor` into
  exported packages (`internal/runtime/dispatch`, `runctx`, `assemble`; five
  per-subsystem `FromConfig` projections; the `internal/drivers/prod`
  aggregator). The devstack mirror is collapsed to thin
  callers.
- The approval gate's privilege check is an injected authorizer; the runtime
  no longer imports Protocol auth (the Protocol import-direction rule now has zero
  violations).
- Scaffold templates emit `sdk/` import paths; `harbortest`'s full vocabulary
  is externally satisfiable; the root README tells the embeddable-SDK story.

### Fixed

- A planner-dispatched approval-gated tool no longer deadlocks the run loop —
  APPROVE/REJECT drain mid-step.
- Session GC can no longer reap RUNNING sessions (the `RunningProbe` is wired).
- The bifrost driver's `Close` now shuts down the provider worker pool
  (previously leaked ~1000 goroutines per stack close).
- The pause park's subscribe-after-publish wake window is closed; sqlite
  `:memory:` stores no longer collide across subsystems; the durable
  event bus honours publish-context cancellation; per-model
  `cost_overrides`/`corrections` YAML is no longer silently dropped.

## [1.1.6] — 2026-05-26

A release-engineering hotfix that finishes what v1.1.5 started. v1.1.5
trimmed the LICENSE but the trimmed text still carried three substantive
deviations from the canonical Apache-2.0 — pkg.go.dev's license detector
(google/licensecheck, ~75% confidence threshold) saw the deviations and
kept reporting "License: UNKNOWN" + ✗ Redistributable. This release ships
the byte-identical canonical text so the badge can finally flip.

### Fixed

- **LICENSE is now byte-identical to apache.org's canonical Apache-2.0
  text** (`https://www.apache.org/licenses/LICENSE-2.0.txt`). The three
  fixed deviations:
  - Missing leading blank line at line 1.
  - §6 "Trademarks" was missing the phrase "reasonable and customary
    use in" — non-standard wording.
  - §9 used the old "Accepting Warranty or Support" wording instead of
    the canonical "Accepting Warranty or Additional Liability"
    (both heading and body's closing phrase).
- Effect: pkg.go.dev will detect `License: Apache-2.0` and flip the
  Redistributable badge from ✗ to ✓ on its next module fetch.

### Changed — release pipeline now ships the full cross-compile matrix

- The release workflow (`.github/workflows/release.yml`) cross-compiles
  six binaries per release (`linux`/`darwin`/`windows` × `amd64`/`arm64`)
  via a matrix strategy, attests SLSA build provenance per binary, and
  publishes them all in a single GitHub Release.
- `scripts/release-build.sh` now appends the `.exe` suffix automatically
  when `GOOS=windows` so the Windows artifact behaves like every other
  Windows CLI a user might download.
- Each release carries an aggregate `checksums.txt` alongside the
  per-binary `.sha256` sidecars. Downloaders verify with the standard
  `sha256sum -c checksums.txt` two-column form.
- Pre-release tags (`-rc` / `-beta` / `-alpha`) keep the existing
  pre-release marking; no behavior change there.

### Notes

- v1.1.5's binary attached to GitHub Releases is still valid and runs
  fine; this release does not deprecate it. The pkg.go.dev license
  display, however, is per-version cached — v1.1.5's UNKNOWN status
  will not retroactively heal. v1.1.6 is the first version on which
  pkg.go.dev's detector should succeed.

## [1.1.5] — 2026-05-25

A pure docs-and-hygiene release — no Runtime, Console, or Protocol behavior changes. Adds Harbor's first cut of **operator skills**: ten Claude-Code-style playbooks covering the agent-builder loop end-to-end, plus a mechanical drift-prevention rule that keeps them honest.

### Added — operator skills (`docs/skills/`)

- Ten focused `docs/skills/<slug>/SKILL.md` playbooks for building Harbor agents, with Dockyard-style frontmatter (`name` / `description` carrying "Use when" framing / `license: Apache-2.0` / `metadata.framework: harbor` / `metadata.surface` / `metadata.verbs`):
  - **Start a project**: `scaffold-a-harbor-agent`, `define-the-agent-yaml`.
  - **Build the agent**: `add-an-in-process-tool`, `wire-the-llm-provider`, `configure-memory-and-skills`.
  - **Drive it interactively**: `run-the-dev-loop`, `drive-the-playground`.
  - **Observe + debug**: `observe-with-the-console` (the 14-page Console tour).
  - **Ship**: `validate-and-package`.
  - **Build a custom frontend**: `use-the-harbor-protocol` — Bearer-JWT + identity-triple headers + the typed wire surface + `events.subscribe` SSE + `topology.snapshot` capability + artifact upload, with a 30-LoC TypeScript chatbot reference. Ships a working chat UI against a real Runtime in a day.
- `docs/skills/INDEX.md` groups the skills by agent-author stage (start → build → drive → observe → ship → frontend) and pins the first-five-minutes adoption chain (`scaffold-a-harbor-agent` → `run-the-dev-loop` → `drive-the-playground`).
- `README.md` Documentation table now points at `docs/skills/INDEX.md`.
- Glossary entry distinguishes **skill (operator)** — `docs/skills/` adoption playbooks — from **skill (runtime)** — the `internal/skills/` token-savvy planner subsystem. Same word, different consumers; the glossary pins the boundary so future contributors don't conflate them.

### Added — same-PR drift prevention rule

- New §18 in `CLAUDE.md` (mirrored verbatim in `AGENTS.md`): a change that mutates a documented surface (a `harbor` CLI verb, a Harbor Protocol method / wire-shape field / capability advertisement / event payload key, a Console route or page or `<PageState>` branch, a `harbor.yaml` config field, a canonical artifact a skill quotes verbatim) MUST update the matching skill in the **same PR**. Affected skill is findable by greping `docs/skills/` for matching `surface:` frontmatter lines. Closes the failure mode where docs drift erodes the first-five-minutes adoption guarantee.
- Mechanical frontmatter audit at `scripts/skills/check-frontmatter.sh` invoked by `make drift-audit`: every `docs/skills/<slug>/SKILL.md` is validated for `name` (matching directory slug), `description` (containing "Use when"), `license: Apache-2.0`, `metadata.framework: harbor`, `metadata.surface` in the canonical set (`cli` / `agent-yaml` / `tools` / `mcp` / `llm` / `memory` / `playground` / `console` / `tasks` / `protocol`), and `metadata.verbs` key presence. Content drift remains human-reviewed — frontmatter shape only is mechanical.
- New static-only smoke script asserts every required slug ships its SKILL.md, the INDEX references them all, the frontmatter helper passes, §18 is present in CLAUDE.md, and the glossary carries both skill clarifications.

### Notes

- `attach-an-mcp-server` is deliberately deferred to V1.2 — its surface depends on MCP wire shapes still stabilising; shipping it here would lock prose against a moving target. Per §18 it will land in the same PR that finalises the MCP wire.
- Distinct from Dockyard's MCP-server-focused skills repo — the two products share the convention but cover separate adoption surfaces (Dockyard: building MCP servers; Harbor: building agents).

## [1.1.0] — 2026-05-25

The V1.1 cut, focused on **Playground multimodal input** and **1:1 Console↔Runtime feature parity**. Harbor Protocol stays at `0.1.0` — `topology_snapshot` is an additive capability, `StartRequest.InputArtifactIDs` is opt-in via `omitempty`.

### Added — Playground multimodal artifact input

- **`StartRequest.InputArtifactIDs []string`** — opt-in wire field on the canonical `start` request. Text-only spawns elide the field entirely (`omitempty` honours the existing wire shape). Operator-uploaded artifacts attach to a foreground task's first planner turn; the runtime resolves each id, materializes the appropriate `Content.Parts` shape, and routes per MIME. `tasks.SpawnRequest.InputArtifactIDs` folds into the idempotency-key content hash so "same key, different attachments" surfaces as `ErrIdempotencyConflict`.
- **`Tool.HandlesMIME []string`** — new tool-descriptor field declaring which MIME types a tool consumes. The planner's multimodal materializer populates `ArtifactStub.Fetch.Tool` from the first matching descriptor — explicit "use this tool for this ref" hint to the LLM, no catalog-discovery guesswork. `Tool.MatchesMIME(mime string)` helper supports exact + `type/*` wildcard matching (no full-`*/*` to keep operator-declared MIMEs predictable).
- **Planner per-MIME dispatcher** (`internal/planner/multimodal.go`) — pure-function `MaterializeInputContent(goal, []InputArtifactView, ToolCatalogView) → llm.Content`. Routes: `image/*` inlines bytes as `ImagePart{DataURL}` so vision-capable providers see the image directly; `application/pdf` → `FilePart{Artifact}` (Anthropic native PDF; others see the canonical ArtifactStub-JSON text); `audio/*` → `AudioPart{Artifact}`; everything else → `ArtifactStub` text the LLM routes via the catalog with the `Fetch.Tool` hint.
- **Run-loop pre-fetch** — `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` reads `task.InputArtifactIDs`, calls `ArtifactStore.GetRef` for metadata + `Get` for bytes when MIME starts with `image/`, and pre-resolves the `InputArtifactView` slice the planner consumes synchronously. Cleared from `RunSpec.Base.InputArtifacts` after the first step so subsequent turns never re-inline bytes. Mirrored in `harbortest/devstack` so test fixtures and production share the same path.
- **Console Playground multimodal end-to-end** — `ControlNamespace.start(query, {inputArtifactIDs})` typed method; `buildChatClient.sendMessage` plumbs the chat-attach uploads through. Fixed the chat-attach upload adapter which previously shipped the wrong body (`{filename, mime, size_bytes}` vs `{scope, bytes, opts: {mime_type, filename}}`) and read `resp.id` instead of `resp.ref.id` — every upload had been silently producing empty artifact ids. New `fileToBase64` helper, correct request body, fail-loud on missing `resp.ref.id`.

### Added — Playground queue-vs-steer when a run is active

- Chat composer (`web/console/src/lib/chat/ChatComposer.svelte`) gains a `running` prop and a two-radio mode picker that appears only while a foreground task is in flight: **Queue after current run** (default) stashes the message and dispatches it via `start` as soon as the active task reaches a terminal state; **Steer current run** dispatches the SHIPPED `user_message` control verb to inject the message into the running task's next planner turn.
- Playground page hooks an `EventSource` lifecycle subscription to `task.completed` / `task.failed` / `task.cancelled` and drains the FIFO queue when `activeTaskID` clears. The SSE envelope's `payload.TaskID` (capital T) is the load-bearing read; an initial draft looked for `task_id` and the queue never drained — caught on a live wire-tap before shipping.

### Added — Runtime capability gate + session aggregates

- **Per-instance capability advertisement** — `runtime.info.capabilities` now reflects what THIS runtime instance has actually wired. `topology_snapshot` is registered in the canonical capability set (the handshake universe) and advertised by a runtime IFF the engine-graph projection accessor is wired. `harbor dev` against a planner/RunLoop agent yaml correctly omits it; a future engine-graph runtime advertises it by setting `PostureDeps.TopologyAvailable=true`. Mirrored in `harbortest/devstack`.
- **Console capability gate** — `HarborClient.capabilities()` lazy-fetches the runtime's advertised set at attach time and memoises a frozen `ReadonlySet<string>`. Live Runtime + Playground + Playground's trace toggle gate their `topology.snapshot()` calls behind `caps.has('topology_snapshot')`; on planner/RunLoop runtimes the browser fetch never fires, the friendly info banner renders directly, and the operator's DevTools console stays clean.
- **Console-side session counter enrichment** — Session detail's `tasks_count` + `events_count` are now truthful. The page fetches `tasks.list` (filtered locally by session id) + `events.aggregate` (30-day session-scoped window, single bucket summed across all event types) after `sessions.inspect` and merges into the snapshot. `sessions.inspect` itself stays a pure registry projection — the Console computes the aggregates. `total_tokens` + `total_cost_cents` remain at zero pending the V1.3 `cost.aggregate` follow-up; a TODO comment on the page calls out the gap.

### Fixed

- **Sessions catalog empty across reboots**. `Registry.Open` did not hydrate `idIndex` when the StateStore returned an existing-record sentinel. On a `harbor dev` boot against a SQLite state store with a pre-existing dev session, the Sessions page rendered "No sessions match these filters" and `runtime.counters.sessions_active` was zero even with a live session in the store. Fix: hydrate `idIndex` (and `openSessions`, for an open record) before returning `ErrSessionAlreadyOpen` / `ErrReopenAfterClose`. Tests cover both branches.
- **`tasks.get` crashed the Cost-breakdown rail**. `cost.per_step` returned `null` against a TS contract that declares `TaskCostStep[]`. Go projector normalizes empty `PerStep` to `[]TaskCostStep{}` so the wire honours its shape; the Console rail uses `?.length ?? 0` for defence in depth. Test pin asserts `"per_step":[]` appears in JSON output and `"per_step":null` does not.
- **Playground composer hidden on empty stream**. `PageState`'s `empty` snippet was swallowing the `ChatPanel` children. `status` now always goes to `'ready'` on a successful load; ChatPanel owns its own empty-state copy + composer. Two previously-skipped Playwright specs un-skipped.
- **sendMessage shipped the wrong wire body**. `dispatch('user_message', sessionID, ...)` treated sessionID as a run id AND used the steering-verb body shape; `start` has a flat shape. New typed `control.start(query, opts)` method ships the correct wire body; both sendMessage + restartRun route through it.
- **Memory page rendered Go zero-time `0001-01-01`** for nullable `expires_at`. `shortTime` helper now returns `"—"` when the ISO starts with `0001-01-01`, matching the Tools page's existing guard.
- **Multimodal conformance probe used a malformed 1×1 PNG** below OpenAI's image-API minimum-pixel threshold, surfacing as a generic "Provider returned error" 400 that looked like a wire-shape bug for months. Swap to a 132-byte 64×64 solid-red PNG (`internal/llm/drivers/bifrost/conformance_test.go::runLiveMultimodal`). All six providers in the live matrix + multimodal subtests now pass under `HARBOR_LIVE_LLM=1`. Playground multimodal end-to-end is verified: operator uploads a PNG via the chat composer → bifrost → OpenRouter → vision model returns "Red".

### Protocol additions

- **Capability `topology_snapshot`** — registered in `internal/protocol/types/version.go::canonicalCapabilities`. Per-instance advertisement in `runtime.info.capabilities` is conditional via the new `PostureDeps.TopologyAvailable` flag; the canonical handshake set is unconditional. RFC §5.3 minor-class additive change — no Protocol version bump.
- **`StartRequest.InputArtifactIDs []string`** — additive field on the existing `start` Protocol method (`internal/protocol/types/control.go`). `omitempty` keeps the wire shape backward-compatible for text-only callers. Round-trip test pins the wire shape.

### Decisions logged

- **D-166** — Playground multimodal artifact input. Three settled calls: (a) runtime inlines image bytes via DataURL rather than pushing materialization down into each LLM driver; (b) per-MIME dispatcher lives in the planner package, not the run loop or LLM driver; (c) `HandlesMIME` is an opt-in descriptor field with bounded `type/*` wildcards, not a global registry.

### Roadmap pointers

- **V1.2** — MCP wave. Plans already on the master plan.
- **V1.3** — bifrost extended multimodal: provider-native file uploads for over-threshold images, native PDF / audio / video / document parts, streaming-with-multimodal, per-MIME conformance probe matrix, `cost.aggregate` follow-up that completes the session counters' `total_tokens` / `total_cost_cents` slots. Plan file staged at `docs/plans/`.

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
