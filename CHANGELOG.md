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

### Added — in-process tools can take content by reference

- **A Go tool can now declare an artifact-reference PARAMETER and read the
  stored bytes without the content ever passing through the model.** Declare a
  field of type `sdk/tools.ArtifactRef`; the derived JSON Schema renders it to
  the model as a plain string, so the model supplies an artifact id — the same
  id the runtime already quotes to it in a truncated tool result or the session
  artifact block — and the runtime resolves it at dispatch and hands the tool
  the bytes via `in.Doc.Bytes()`.

  ```go
  type SummarizeArgs struct {
      Doc      tools.ArtifactRef `json:"doc"`
      MaxWords int               `json:"max_words"`
  }
  ```

  **Why it matters.** Harbor's heavy-content safeguard was one-directional: a
  large tool RESULT routes to the artifact store so it never reaches the
  context window, but a tool that needed to READ something large could only be
  given it through its arguments — which meant through the model. This closes
  that leg. Bytes flow store → tool.

  **What the runtime guarantees.** The resolved value is dispatch-local: it
  does not appear in the argument JSON, the trajectory, the observation the
  next prompt renders, any event payload, an audit payload, or a log. The
  reference resolves under the run's own `(tenant, user, session)`, so a tool
  reaches exactly what its run reaches — an out-of-scope id answers not-found
  through the same soft path `artifact_fetch` uses, with no way to tell "not
  yours" from "does not exist". Every failure is loud: an unresolvable
  reference is the step's error, and reading a reference the argument never
  carried returns `tools.ErrArtifactRefUnresolved` rather than an empty slice a
  tool would measure as an empty artifact.

  **Scope.** In-process tools only. A tool behind a process boundary (HTTP,
  MCP, A2A) cannot be handed a Go value, and handing it something
  dereferenceable instead is a separate design that has deliberately not been
  built — it needs an externally-reachable address Harbor does not have, a
  grant whose single-use semantics a presigned URL cannot provide, and an
  answer for a grant URL being a bearer capability to the content.

  Worked example: `examples/tools/artifactstats/`. Guidance: the
  `add-an-in-process-tool` skill.

### Changed — the LLM-edge heavy-content guard also inspects tool-call arguments

- **A tool call whose arguments exceed the heavy-output threshold now fails
  loudly instead of reaching the provider.** The guard that already refused
  oversize tool RESULTS and oversize inline binary content now applies the same
  byte check to `tool_calls[].arguments` — the field the prompt builder replays
  turn over turn and the provider drivers map onto the wire. The failure is the
  existing one (`ErrContextLeak` plus a `llm.context_leak` event naming the
  offending call by index); nothing else about the guard moves — same
  threshold, same exemption for ordinary conversation text, same error, same
  event.

  **Who this affects.** Almost nobody: a tool call is an id and a few
  parameters. If it fires, it is naming a real bug — a producer putting a
  payload into an argument that should have been a reference, which is now
  something Harbor supports directly (see above).

### Changed — artifact reads now resolve on the session, and two stores migrate

- **An artifact is read by `(tenant, user, session)` plus its id; the producing
  task is provenance.** `artifacts.get_ref` and `artifacts.delete` — and the
  `artifact_fetch` built-in the model uses — no longer require the caller to
  name the same `task` the artifact was stored under. Naming a different one,
  or omitting it, reaches the same artifact.

  **Why it changes.** The session-artifact block the planner injects lists
  every artifact in the session and invites the model to fetch any of them,
  while the three writers that produce them stamped three different task
  shapes. So the model was shown references a read then answered "not found"
  for, and it had no way to tell that from a deleted artifact. Nothing crosses
  a session, a user or a tenant: the session was always the innermost isolation
  scope, and this makes the key match it.

  **The provenance stamp becomes first-writer-wins.** Identical bytes stored by
  two tasks in one session were two records, one stamp each; they are now ONE
  record carrying the first writer's stamp. A `task` filter on `artifacts.list`
  therefore answers "which artifacts is this task the recorded producer of",
  not "which did it write". This is inherent to a content-addressed store — the
  id is derived from the bytes, so the question has no single answer once two
  tasks produce them.

  **`artifacts.list` now requires a tenant.** A filter with no tenant is
  refused at the store instead of listing across tenants. Empty `user`,
  `session` and `task` remain wildcards within the tenant, so no existing
  request shape changes: the Protocol edge already refused a tenant-less list.

- **ACTION: the SQLite and Postgres artifact stores run a schema migration on
  first start after upgrade.** `0002_read_key_is_the_triple.sql` re-keys
  `artifacts_blobs` onto `(tenant, user, session, namespace, id)`. It is
  forward-only and transactional, and pre-existing duplicate rows (the same
  bytes stored by two tasks in one session) collapse onto the row with the
  lowest `task`. On a large artifacts table the rebuild is not instantaneous —
  size the restart window rather than meeting it. No action is needed for the
  in-memory or filesystem stores.

- **ACTION: the S3-compatible artifact store writes a new object key layout.**
  Objects are now stored at `<prefix>/<tenant>/<user>/<session>/<namespace>/<id>`
  — the task segment is gone, because an object store has no compare-and-set
  and keying on it let concurrent writers of identical bytes create one object
  each. Objects written by earlier versions stay readable and deletable: the
  driver falls back to a session-prefix scan and nothing is rewritten in place,
  so a bucket migrates by being read. The cost is one extra listing on a read
  that misses the new key, and one per delete. Buckets whose lifecycle rules or
  external tooling match on the old key shape need those rules updated.

### Changed — ACTION REQUIRED for two configurations

- **MCP server ids must be separator-safe; an ambiguous pair now refuses to
  boot.** Two MCP server names where one is an underscore-extension of the
  other (`github` and `github_enterprise`; also `a` and `a_b`) are no longer
  allowed to coexist. A server's tools are keyed `<name>_<tool>` in the shared
  tool catalog, so such a pair makes that key ambiguous — a key built by
  prefixing the shorter name can resolve to a tool owned by the longer one,
  which silently weakens any scoping that relies on the prefix (notably the
  MCP-Apps host's per-App tool scoping).

  **Impact.** A deployment already running such a pair will FAIL TO START after
  upgrade: the registry refuses the second registration and assembly aborts
  with `mcp: ambiguous server id`. The failure is loud, deterministic and names
  the offending id. `harbor validate` now reports the same condition, so run it
  against your config BEFORE upgrading.

  **Remediation is a rename, and a rename is not free.** Changing a server's
  name changes every `<name>_<tool>` catalog key that references it — agent
  YAML tool allow-lists, `disabled_tools`, `paused_servers`, and any persisted
  agent-config revision that pinned those keys. Update them in the same change,
  or the tools will read as missing. Names that merely share a prefix without a
  `_` boundary (`github` and `githubby`) are unaffected.

- **MCP Apps: a rendered app must send the BARE server-side tool name.** The
  MCP-Apps host now qualifies an app-supplied tool name with the app's own
  server id before dispatching, instead of passing it through verbatim. An app
  written against Harbor's `<source>_<tool>` catalog keys now double-qualifies
  and receives a typed not-found; send `get_forecast`, not
  `weather_get_forecast`. This is the spec-correct behaviour — a conformant MCP
  App knows only its own server-side names — and it is what scopes an app to
  its own server's tools.

### Fixed

- **MCP not-found responses are classified by sentinel, not by error text.**
  `mcp.*` methods previously decided `not_found` by substring-matching the
  error chain, which carries a southbound MCP server's message verbatim — so a
  server whose transport failure happened to read like a not-found could turn a
  transient failure into a typed `not_found` that clients treat as permanent.
  The same applied to the paused-server / disabled-tool refusal. Both now
  classify on an explicit sentinel the accessor sets.

- **An MCP App that reports its content size now resizes its inline frame.**
  `ui/notifications/size-changed` was ignored, so content-bearing apps were
  pinned to a fixed height with their own nested scrollbar. The frame now
  tracks the reported height, bounded by the host between a minimum and a
  maximum. Apps that report no size are unchanged. The fullscreen and
  side-by-side panels are unaffected by that bound.

- **MCP Apps host obligations that were recorded as shipped but were absent.**
  Graceful `ui/resource-teardown` on unmount, the app-initiated
  `request-teardown`, host-context `toolInfo` / `containerDimensions`, and
  `resources/templates/list` are now implemented. They were reverted alongside
  an unrelated regression and never re-landed.

## [1.18.1] — 2026-07-24

Patch: test-only hardening — eliminate a class of load-sensitive flaky
goroutine-leak tests. No Runtime, Protocol, or Console behaviour changes.

### Fixed

- **Load-sensitive goroutine-leak tests hardened to a bounded drain-poll.**
  A class of concurrent-reuse and leak tests sampled `runtime.NumGoroutine()`
  a SINGLE instant right after their workers joined — after a fixed
  `time.Sleep`, a lone `runtime.GC()`, or a tight `runtime.Gosched()` spin —
  and compared against a baseline. Under the full `go test -race ./...` matrix
  (and `make preflight`) shared background teardown goroutines (event/notification
  consume-loops, HTTP handler unwinds, SSE keepalives, server-close readers) can
  still be draining at that instant, so the count reads transiently over
  tolerance and the test FAILs on one platform while passing on the other and in
  isolation — a spurious failure that forced CI re-runs on otherwise-clean PRs.
  Each such site now polls a real-time bounded eventually-assertion (AGENTS.md
  §17.4): re-sample after `runtime.GC()` (which reaps finished-but-unscheduled
  goroutines and parks the poller so exiting goroutines get scheduler time under
  load) until the count settles within tolerance or a deadline elapses, failing
  only if it never drains — the leak-detection intent is preserved. Twenty-three
  single-instant sites across the protocol, protocol/transports, protocol/client,
  tui/app, tui/conversation, runtime/engine, runtime/pauseresume, search (+ four
  index sub-packages), tools/drivers/mcp, and the `test/integration` wave suites
  were converted; the two `protocol/client` sites' `runtime.Gosched()` spin (which
  can starve the very goroutines it waits on under load) was replaced with the
  same GC-parked poll. Test files only.

## [1.18.0] — 2026-07-23

Minor: runtime-time MCP connection management + per-user credential delivery — an idempotent live re-attach, a dev-gated wire-carried OAuth-provider descriptor, and per-user credential injection for receiver-style MCP servers.

### Added

- **`agent_config.add_mcp_connection` is idempotent at the live layer.** A
  same-name attach against a still-live in-memory registration now
  synchronously REPLACES it — deregister the old server's tools + close its
  transport, then register the new one — an atomic, owner-scoped upsert,
  instead of failing with a duplicate-tool-name collision. This closes the
  synchronous-attach / deferred-detach asymmetry that otherwise stranded a
  coordinator re-establishing a connection on a running runtime with ZERO tools
  until a process restart. A same-name collision owned by a DIFFERENT tenant /
  agent is rejected loud (`ErrConnectionNameOwnerConflict`) and never tears
  down another owner's live registration; the fix also closes a transport leak
  in the registry's same-name overwrite path.
- **A dev-gated, wire-carried OAuth-provider descriptor.**
  `agent_config.set_oauth_provider` / `add_mcp_connection` may carry a new
  server's OAuth binding (`token_url`, `audience`, `scopes`, naming a
  boot-declared credential broker) so a new OAuth-fronted MCP server can be
  connected at runtime without a static `tools.oauth_providers[]` block and a
  redeploy — but ONLY behind a fail-closed, boot-only opt-in
  (`tools.allow_wire_oauth_descriptor`, or the `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR`
  boot env; default off). With the opt-in off (all production), a descriptor
  carrying any credential-sink field is rejected exactly as before. When an
  operator opts in, the downstream allow-list is DERIVED from the connected
  server's own URL (never wire-chosen), the wire token endpoint is dialed
  through the same SSRF backstop as the boot path (private / link-local /
  unspecified refused, redirects refused, no proxy), and the runtime's own
  credential custody stays entirely boot-declared — no secret ever rides the
  wire.
- **Per-user credential injection for receiver-style MCP servers.** The
  southbound MCP driver can source the acting principal's credential from the
  broker per outbound tool call and inject it in a server's declared form
  (arbitrary headers, `Authorization: Basic`, or MCP `_meta`), reaching a
  server that RECEIVES its credential directly instead of pulling it via
  RFC 8693. The injection mapping is non-secret connection config; the pulled
  value is per-user, fetched-not-held, and never logged — the audit redactor
  covers every injected form. Injection is mutually exclusive with the bearer
  OAuth mode (one auth mode per connection).

## [1.17.4] — 2026-07-23

Patch: fix the Console's posture-edit 400 and a silent-park on an unresolvable MCP OAuth binding.

### Fixed

- **The Console's Governance-Posture edit card 400'd on every submit.** The
  shared Console transport folds the identity triple into every request body,
  but `governance.set_posture` decodes an identity-LESS request type with
  `DisallowUnknownFields`, so the folded `identity` key was rejected with
  `unknown field "identity"` (HTTP 400) before the validator ran. `setPosture`
  now opts out of the body fold (identity still rides the `X-Harbor-*`
  headers); a corrected JSDoc no longer claims the transport skips the fold.
- **A runtime-added MCP connection whose `oauth_provider` did not resolve
  parked silently at `auth_required` instead of failing loud.** The
  auth-required classifier is a message heuristic that matches the marker
  `oauth`; a binding/config error (`mcp: invalid oauth_provider binding: …`)
  contains `oauth_provider`, so an unknown-provider / missing-or-mismatched
  downstream-allow-list error was misclassified as auth-required and parked,
  hiding the diagnostic. The attacher now checks the typed `ErrOAuthBinding`
  sentinel first, so a binding misconfiguration surfaces its real cause loudly
  (§13). The genuine transport auth-required path is unchanged.

## [1.17.3] — 2026-07-23

Patch: dev-only, fail-closed opt-in to permit a private-IP `tokenexchange` dial.

### Added

- **Dev-only opt-in to reach a private-IP credential broker.** The
  `tokenexchange` credential-bearing RFC-8693 exchange POST is hardened with a
  post-DNS dial backstop that refuses private-range / link-local resolved
  addresses (the DNS-rebinding defence). That backstop also refused the ordinary
  containerized local-dev topology — a coordinator behind a **private-IP** TLS
  sidecar whose operator-declared `token_url` resolves into RFC 1918 space. A new
  **fail-closed, boot-only** opt-in relaxes ONLY the private / link-local / ULA
  branch of the guard, scoped to that provider's own boot-declared `token_url`:
  the per-provider `tools.oauth_providers[].allow_private_token_url` config flag,
  or the global `HARBOR_DEV_ALLOW_PRIVATE_EXCHANGE=1` boot env (effective posture
  is the OR of the two, default off). When the env hatch fires, every boot prints
  a `[DEV-ONLY PRIVATE-IP TOKEN EXCHANGE — DO NOT USE IN PRODUCTION]` stderr
  banner. It is never Protocol-writable or derived from a discovered / wire
  descriptor.

### Changed

- **Production posture is unchanged.** The dial backstop stays default-armed; the
  opt-in is off unless an operator explicitly sets the config flag or the banner'd
  env. The unspecified-address block (`0.0.0.0` / `::`) stays refused
  unconditionally, the loopback carve-out stays allowed, and the token-endpoint
  redirect refusal (the credential-form-replay defence) stays absolute — none of
  them are touched by the opt-in (see D-300, D-338).

## [1.17.2] — 2026-07-21

Patch: fix `tool_search` output validation when a discovered tool has no tags.

### Fixed

- **`tool_search` failed its own output schema whenever a searchable MCP tool
  had no tags.** An MCP-discovered tool with `tags: null` surfaces in Go as a
  nil slice, which JSON-marshals to `null`; but the `tool_search` result schema
  requires `tags` to be an array, so the whole result failed validation
  (`/tools/N/tags: got null, want array`) — breaking `tool_search` for any agent
  whose catalog held an MCP tool without tags. The `tool_search` result builder
  now normalizes a nil tag slice to an empty array (`[]`) at the emit boundary.

## [1.17.1] — 2026-07-21

Patch: fix runtime-added MCP connections on SDK-facade servers.

### Fixed

- **A compiled `sdk/server` agent could not accept a runtime-added MCP
  connection.** The SDK facade exposes no knob for `MCPDefaultIdentity`, so it
  was left empty; the runtime-add attacher then passed that empty triple to the
  MCP provider, which rejected it at construction (`DefaultIdentity must be
  fully populated`). Every runtime-added connection on such a server died before
  dialing. `serve.Boot` now fills an unset `MCPDefaultIdentity` with the same
  fully-populated `assemble.DefaultMCPIdentity` fallback the boot path already
  applies to config-declared servers. The default only stamps transport-side
  events; per-call isolation continues to ride the inflight caller identity, so
  multi-isolation is unchanged. Agents launched via `harbor serve` / `harbor
  dev` / the test devstack were unaffected (they set the identity explicitly).

## [1.17.0] — 2026-07-21

The control-plane admin-write release. Harbor's admin/control plane gains
WRITE surfaces that were previously read-only or absent, plus the
parallel-intent primitives that complete the v1.16 task-control story: a
planner can now steer, pause, and resume the children it spawned; operators
can rewrite the governance identity-tier policy live; and the LLM credential
plane learns to pull provider keys from a broker and orchestrate failover —
all fail-closed, all identity-scoped.

Every new surface is additive on the wire — one new canonical event
(`governance.failover`), two new Protocol methods (`governance.set_posture`,
`agent_config.set_llm_provider`), new optional wire fields, and new config
keys — so the Harbor Protocol holds at `0.1.0` (no methods removed, no
breaking changes). Nine decisions land: D-329 (task-group-cancelled
conversational mirror), D-330 (planner steer/pause/resume of a spawned
child), D-331 (per-tool OAuth on resource/prompt paths + owner-scoped
uninstall), D-332 (`governance.set_posture` tier-policy write), D-333
(inference broker-pull credential source), D-334
(`agent_config.set_llm_provider`), D-335 (broker-pulled Harbor-orchestrated
failover), D-336 (ephemeral inference-provider rebind), D-337
(`governance.failover` as a canonical event).

### Added — control-plane admin-write surfaces

- **`notification.task_group_cancelled`** conversational mirror with a
  cancel-origin (operator / cascade / fail-fast); operator-driven cancels
  are suppressed, only unprompted cancels surface (D-329).
- **Planner-facing steer / pause / resume** of a spawned child
  (`_steer_task` / `_pause_task` / `_resume_task`), dispatched onto the
  existing per-sub-run steering inbox and the unified pause/resume
  primitive; descendant-scoped, human-supremacy-preserving (D-330).
- **Per-tool OAuth binding** extended to MCP resource/prompt paths, plus
  **owner-scoped provider uninstall** (defense-in-depth on D-303) (D-331).
- **`governance.set_posture`** — admin identity-tier **policy write**
  (per-tier budget / max-tokens / rate + default tier), fail-closed against
  any budget-widening write, `admin`-scope only, hot-swapped into live
  enforcement with no restart (D-332).
- **Inference-plane broker-pull** credential source (per-runtime, fail-loud,
  no stale-key-past-refresh) plus a zero-URL admin
  **`agent_config.set_llm_provider`** install/rebind (D-333, D-334).
- **Broker-pulled Harbor-orchestrated failover** — on a retryable provider
  error, advance the chain, emit `governance.failover`, re-run governance
  `PreCall` (budget is never bypassed), re-issue through the one-method
  `LLMClient`; the provider SDK's native fallback array stays unused
  (D-018) (D-335, D-337).

## [1.16.0] — 2026-07-18

The parallel-intent release. Harbor's React planner and runtime gain the
ability to express and execute *concurrent* intent in a single turn, a
task-management control surface with a real cancel hierarchy, honest
background-wake and turn-failure signals on the conversation surfaces,
prompt-cache telemetry, a default-agent row, and three additive OAuth
broker legs. The wave closes a long-standing limitation from the native
tool-calling migration: a planner could no longer say "do these things at
once."

Every new surface is additive on the wire — new event classes, new
optional wire fields, new config keys — so the Harbor Protocol holds at
`0.1.0` (no methods removed, no breaking changes). Seven decisions land:
D-322 (Batch decision), D-323 (Batch executor), D-324 (task-management
meta-tools + cancel hierarchy), D-325 (background wake + failure honesty),
D-326 (cache-token capture), D-327 (default-agent row), D-328 (OAuth
broker legs).

### Added — parallel intent + task management

- **The `Batch` decision (D-322).** A fourth sealed `planner.Decision`
  shape, `Batch{Tools, Spawns, Join}`, lets a single planner step dispatch
  multiple tool calls and/or spawn multiple background tasks at once. The
  fossilized "`_spawn_task` must stand alone" rule (a leftover from the
  pre-native-tool-calling era) is lifted: only the genuinely terminal /
  blocking controls (`_finish`, `_await_task`, `_task_status`,
  `_cancel_task`) remain standalone; `_spawn_task` is now batchable
  alongside ordinary tool calls. `SpawnTask` gains an additive `CallID`.
- **The Batch executor with auto-grouping (D-323).** The runtime dispatch
  path executes a `Batch` with non-atomic per-branch semantics (a branch
  failure is that branch's error result, not a whole-run abort); two or
  more unbound spawns are auto-grouped so the planner can await them as a
  unit. A `planner.max_batch_spawns` config key (default `5`) caps spawn
  breadth per batch.
- **Task-management meta-tools + a cancel hierarchy (D-324).** New reserved
  controls `_task_status` and `_cancel_task` (sealed `TaskStatusQuery` /
  `CancelTask` decisions) let a run inspect and cancel the tasks it spawned
  — descendant-scoped, so a run can never observe or cancel a sibling
  run's tasks. A model-expressible `propagate_on_cancel: isolate` lands in
  the same phase as its brake. The cancel hierarchy is explicit and tested
  end-to-end: **operator > agent (own descendants) > cascade** — there is
  no uncancellable task; a human always has the last word.
- **Background-wake notifications + turn-failure honesty (D-325).** Two new
  conversational-mirror event classes, `notification.task_group_resolved`
  and `notification.task_completed`, surface background resolution on the
  conversation surface (ref-shaped member outcomes; the typed `WatchGroup`
  planner path is untouched). The TUI renders muted lifecycle one-liners (a
  new conversational `notification` block kind) and a dedicated
  `× Turn failed · <ErrorCode>` status-strip line for a failed foreground
  turn that previously went silently idle. The Console Sessions and Tasks
  docks render the same family.
- **A default-agent row on `agents.list` (D-327).** The agent registry
  projection synthesises a default-agent row (`Agent.IsDefault`), so a
  zero-registration Runtime still presents an agent to operators; a real
  registration suppresses the synthetic row.
- **OAuth broker legs (D-328).** Three additive, report-not-act legs on the
  broker-pull spine: (HA-26) a downstream 403 +
  `WWW-Authenticate: insufficient_scope` becomes typed
  `tools.ErrInsufficientScope` on the tool-result error path and
  `MCPServerView.LastScopeShortfall` on the connection view, and
  `ClassifyError` now returns `ErrClassPermanent` for it (closing a
  retry-storm where a scope shortfall was retried); (HA-27a) an RFC 8707
  `resource_indicator` rides the RFC 8693 exchange with best-effort
  audience verification; (HA-27b) a per-tool `tool_oauth_providers` binding
  overrides the connection-level `oauth_provider` for a shared MCP server
  fronting multiple downstream resources; (HA-28) an
  `include_actor_token` opts the run's verified acting principal into the
  exchange as an RFC 8693 `actor_token`. Custody, acquisition, refresh, and
  consent stay coordinator-side; no leg widens a binding or runs a flow,
  and D-300's boot-declared credential-sink invariant is preserved.

### Changed

- **Prompt-cache telemetry (D-326).** The bifrost translator no longer
  drops the provider's cache accounting: `llm.Usage` gains
  `CacheReadTokens` / `CacheWriteTokens`, threaded through the cost surface
  and its consumers (the TUI reducer, the Console run-events reader, and
  the sessions enricher). Cache tokens are informational first — governance
  ceiling math is intentionally untouched this wave.
- **New configuration keys (all additive, optional, restart-required):**
  `planner.max_batch_spawns`; and on a `tokenexchange`-bound MCP provider,
  `resource_indicator`, `include_actor_token`, and the per-server
  `tool_oauth_providers` map. Documented in `examples/harbor.yaml`.

### Fixed

- **Native-path structural rejections are now repairable, not fatal.** A
  native tool-calling response the React projector cannot turn into an
  actionable decision — malformed tool-call args JSON, a `retain_turn`
  spawn riding a multi-call batch, a terminal/blocking control co-occurring
  with other calls, an out-of-enum spawn arg — previously killed the run,
  while the same class of failure raised at dispatch was fed back to the
  planner and re-planned. The runloop now feeds a projector structural
  rejection back as a repairable step observation (bounded by a
  consecutive-failure budget so a persistently-malformed model still
  terminates loudly). Genuinely-fatal errors (context cancellation, missing
  identity, unserializable state, LLM transport failure) stay fatal. A real
  streaming model emitting imperfect JSON now recovers instead of dying.

### Known follow-ups

- A cascade / fail-fast-cancelled batch-spawned task has no
  conversational mirror while its successful siblings wake — tracked in
  [#532](https://github.com/hurtener/Harbor/issues/532) (a
  `notification.task_group_cancelled` class), accepted-by-plan per D-325's
  scope for this wave.

## [1.15.0] — 2026-07-17

The native terminal client release. Harbor gains a first-party TUI — a pure
Protocol client that attaches over authenticated REST/SSE and never holds a
Runtime handle — and the distribution surfaces to launch it. On top of the
foundational wave (the Go Protocol client, the typed projection reducer, the
Bubble Tea terminal foundation, and the conversation / runtime-inspection /
control surfaces), a deep level-up pass rebuilt the conversation to product
quality: message hierarchy, a full-screen alternate-screen session surface with
line-granular scrolling, honest chrome sourced from canonical events, a 76×
faster layout path, and a correctness pass over the interactive command/modal/
session workflows.

The TUI reads only canonical Protocol events, state snapshots, topology,
artifacts, traces, and metrics — never a Runtime internal — so the Harbor
Protocol holds at `0.1.0` (no new methods, events, or breaks). Two operational
decisions land: D-320 (TUI distribution) and D-321 (dev-only `.env` loading).

### Added — the native terminal client (D-320, D-321)

- **`harbor tui --attach`** — the native terminal client. A pure Protocol
  client (no Runtime handle) carrying the full `(tenant, user, session)`
  identity through authenticated REST/SSE, with the runtime routes
  (tasks / tools / artifacts / events / posture / interventions / diagnostics)
  behind F-keys and the F9 action matrix for canonical control
  (cancel / pause / resume / redirect / inject / user-message / prioritize,
  and intervention approve / reject / resume through the unified pause
  primitive). TTY discovery falls back to `/dev/tty` when stdin is redirected.
- **`harbor dev --tui`** co-launches the local Runtime + the terminal client in
  one process, one terminal — the one-command dev loop. `harbor dev` also
  loads a dev-only `./.env` at boot (D-321): a names-only stderr line,
  environment-wins precedence, no secret ever echoed in an error, `--no-env-file`
  to opt out.
- **`harbor serve --tui`** co-launches the native terminal client after the
  server is ready. The TUI attaches through authenticated REST/SSE — it
  receives no Runtime handle. The operator supplies the token
  (`HARBOR_TOKEN` or `~/.harbor/token`); there is no anonymous loopback,
  automatic token minting, or mock fallback. Quitting the TUI drains the
  owned server. Runtime logs go to a captured sink so Bubble Tea frames
  are never overwritten; on server failure the terminal is restored before
  the captured log is printed.
- **`sdk/tui.Run(ctx, Options)`** — a curated connection-only facade with
  `BaseURL`, `Token protocolclient.TokenSource`, and `Session`. No
  Runtime/stack/event-bus handle. The facade forwards to the shared
  `internal/tui/entry` attach flow (`harbor tui --attach` and
  `harbor serve --tui` share one implementation).
- **`(*server.Handle).WaitReady(ctx) (string, error)`** — race-safe
  one-shot readiness through `sdk/server`. Returns the actual bound
  address or the bind/cancellation error. No polling, no second listener
  lifecycle.
- **`harbor scaffold --with-server --with-tui`** generates a binary whose
  opt-in `--tui` flag co-launches the TUI via `sdk/server` + `sdk/tui`.
  Flagless behavior remains headless and unchanged.
- **A dependency-free, streaming-stable markdown renderer**
  (`internal/tui/markdown`) with a span API (`RenderSpans`) that emits plain
  styled runs for the cell-grid canvas — deferred delimiter matching keeps a
  mid-stream token from flickering formatting.
- **The Harbor lighthouse mark and reconciled palette** in the session banner,
  with a real version readout (`v1.15.0-dev` on un-stamped builds, never
  `v0.0.0`).
- **Wave-end PTY E2E** (`test/integration/wave_v115_tui_test.go`) covers
  standalone attach, stock co-launch, and generated co-launch with
  identity propagation, session isolation, conversation, shutdown
  ordering, goroutine baseline, and N≥10 concurrent cycles under `-race`.

### Changed — the conversation surface level-up

- **Rebuilt around message hierarchy.** The session surface owns the whole
  alternate screen — banner at the top, the conversation flowing top-down, the
  composer pinned at the bottom — with line-granular scrolling that follows the
  tail and never yanks the reader back, reflow-stable across resizes, and the
  mouse wheel translated to scroll without capturing the mouse so native text
  selection keeps working over the conversation only.
- **Honest chrome.** The per-turn duration and the context / cost / token
  readout come only from the canonical `llm.cost.recorded` stream (never
  estimated); the operator's own message is echoed on a live turn; honesty
  states surface as chrome. The conversation carries the conversation —
  audit records, cost accounting, planner decisions, and session/task lifecycle
  stay on the diagnostics and events routes, never interleaved with the answer.
- **76× faster transcript layout** — a per-block layout cache keyed on a content
  signature plus a structured projection clone replacing a JSON round-trip, so
  scrolling a long conversation costs map lookups instead of a full re-render.
- **Interactive-workflow correctness.** A single command-dispatch path with a
  submit-time slash guard keeps `/command` drafts out of the model (they execute
  or toast, never send as chat) and makes shell-owned commands reachable from the
  composer; interrupting a run takes a deliberate double-esc; `ctrl+c` quits from
  every input mode. Free-text dialogs (rename, attach, credential, action input)
  now carry real labels, masked credential echo, and inline validation instead of
  a mislabeled "Search:" prompt. Start Fresh mints a fresh session instantly
  (with per-session dev-token minting under `harbor dev`), a failed session
  switch is non-destructive (the previous session stays live), mid-run steering
  targets the active run from the conversation, an explicit theme choice locks
  against terminal auto-detect, and closing search always clears its filter.

### Fixed

- **Streaming transcript segmentation.** A turn's assistant text and reasoning
  were keyed per run, so a turn's preamble and its post-tool answer merged into
  one block anchored at its first appearance — reordering the intervening
  reasoning and tool blocks. Streaming blocks now key per `(kind, run, step)`,
  advancing on each stream's `Done` terminator (content and reasoning carry
  independent terminators), so every message and reasoning segment is a distinct,
  correctly-ordered block while consecutive same-step deltas still merge.

(Next up: the run-start connection reconcile landed DETACH-ONLY in v1.11
(D-287) — only the attach / OAuth-resume leg of issue
[#375](https://github.com/hurtener/Harbor/issues/375) remains parked with the
92k–92q MCP-OAuth band; identity-scoped `StateStore` enumeration to bound the
user-scope revision scan — issue
[#396](https://github.com/hurtener/Harbor/issues/396); and the shared
chat-module extraction to `web/shared/chat/` — the D-091 follow-on. New this
wave: hardening the Protocol-installed OAuth provider's `Uninstall(owner)` blast
radius — issue [#507](https://github.com/hurtener/Harbor/issues/507); the tools
Annotator's read-side metrics path may grow a cache off the durable bus if the
per-projection events scan proves expensive (D-314), and its `DisplayModes`
reader awaits per-MIME MCP negotiation. A `HARBOR_LIVE_LLM`-gated smoke guards
Anthropic reasoning-chunk surfacing; the `internal/runtime/dispatch`
parallel-cancel test flakes under full-suite `-race` CPU oversubscription —
issue [#480](https://github.com/hurtener/Harbor/issues/480).)

## [1.14.0] — 2026-07-14

The credential-plane + Protocol-edge-honesty release. Two arcs land together.

**The credential plane gets an invariant, and MCP OAuth becomes
Protocol-drivable.** An adversarial review of the v1.14 credential surface found
three exfiltration paths in *shipped* Harbor and generalized the rule that closes
them: no admin-writable field may determine where a credential is sent — every
credential sink (token endpoint, allowed downstream hosts, token audience, scope
ceiling) is now boot-declared. On top of that hardened edge, an operator can now
grant an MCP server's OAuth-discovery origins live, install a zero-URL broker-pull
OAuth provider over the Protocol, and bind it to a connection — the discovery
walker shipped in v1.13 is now reachable on the ordinary self-hosted (localhost /
compose / private-VPC) posture it could never complete against before.

**The Protocol read edge stops lying.** A recurring silent-absence defect class —
a surface that declares a wire field, ships a facet/sort/aggregate over it, never
populates it, and returns believable-but-false emptiness — is closed across
`events.aggregate`, `sessions.list`/`inspect`, `runtime.health` retention, and the
tools catalog, and a build-time projection-completeness gate now makes the class
mechanically un-reintroducible. `events.aggregate` works on the durable driver for
the first time (it 500'd on every call), gains an addressable/cacheable bucket
grid and opt-in per-tenant attribution, and widened admin reads finally fan in
across the axes they were authorized to read instead of folding to an empty
own-scope result.

Nearly all additive: the Harbor Protocol holds at `0.1.0` — three new methods, one
new event, one new error code (`session_erased`), and a handful of optional wire
fields, none of them a break. The one wire *removal* — a structurally-dead memory
TTL facet that was always empty — is a deliberate within-`0.1.0` removal justified
under RFC §8 (no live consumer). Thirteen phases plus a checkpoint audit; fifteen
new decisions (D-300…D-314).

### Security

- **Credential-sink hardening (D-300) — the v1.14 credential-plane
  invariant: no admin-writable field may determine where a credential is
  sent.** Three exfiltration paths reachable in shipped Harbor are closed
  in-band: (1) a boot-declared `tools.oauth_providers[].allowed_downstream_hosts`
  allow-list is now enforced in the MCP southbound binding
  (`resolveOAuthBinding`) — a binding whose connection host is not listed is
  refused fail-closed, covering both the `tokenexchange` and `oauth2`
  drivers; (2) the token-exchange HTTP client (which POSTs the org's OAuth
  `client_id`/`client_secret`) is hardened to refuse private-range/link-local
  dials post-DNS (the DNS-rebinding backstop; loopback stays allowed for a
  boot-declared localhost-sidecar broker), disable the proxy, and refuse
  every redirect (Go replays the POST body on 307/308); (3) the MCP bearer client re-validates every
  redirect target against the provider's allow-list so an exchanged
  downstream bearer never egresses via a redirect. A boot-declared
  audience/scope ceiling decouples the token audience from the caller-chosen
  provider name and intersects requested scopes. The
  `mcp.servers.set_raw_html_trust` audit ordering is corrected to a
  genuinely fail-closed (apply-then-emit-then-compensate) posture.
- **MCP OAuth discovery now works on private networks without weakening the
  SSRF posture (D-304).** The v1.13 discovery walker had two SSRF guards that
  *disagreed*: the per-hop policy permitted a same-origin protected-resource
  fetch, but the dial-time backstop unconditionally refused every private/loopback
  IP with no production override — so discovery could never complete against an
  MCP server on `localhost`, a compose network, or a private VPC (the ordinary
  self-hosted posture), dying one hop before the operator could even be asked to
  grant an origin. The dial backstop is aligned to the per-hop policy, relaxed for
  **exactly** the same-origin protected-resource hop and pinned to the
  connection's operator-declared dial target (resolved IP set + port) — the
  DNS-rebinding vector stays closed by pinning to the *resolved IP*, not the origin
  string, and plain-HTTP is permitted only to that pinned same-origin target
  (compose/k8s service names). Cross-origin and authorization-server hops keep the
  full private-IP refusal; there is **no** production private-network knob.
- **Run-start connection reconcile is owner-scoped in a shared runtime
  (D-301).** In a deployment running one runtime for many tenants, a run-start
  reconcile enumerated the process-global MCP registry and could detach
  boot-declared servers or *another* owner's runtime-added connections. Runtime-
  added connections and Protocol-installed providers now carry an owner tag
  `(tenant, agent)`, and a reconcile only ever touches its own owner's runtime-
  adds. The owner tag is a reconcile-*view* filter, never an isolation principal
  or a storage `WHERE` key (the boundary stays `(tenant, user, session)`); the
  shared catalog, bare-name resolution, and dispatch model are unchanged (extends
  D-287, does not reverse it). The bounded honesty this states plainly: a shared
  runtime does **not** claim hard cross-tenant isolation of runtime-added tool
  *dispatch* — a name collision fails loud, and a deployment needing hard isolation
  runs one-runtime-per-tenant.

### Added

- **`agent_config.set_mcp_discovery_origins` — live MCP OAuth discovery-origin
  grant (D-302).** The per-connection origins that the discovery walker's
  cross-origin authorization-server hop needs were reachable only as a
  restart-required yaml field, unusable to a Protocol-driven console. One narrow
  admin verb now writes them (full replace) as revisioned agent-config state —
  diffable, rollback-able — *and* applies them live to the process-global registry.
  Revoke is symmetric and live (dropping an origin refuses the next hop and prunes
  the recorded requirement's entries from that origin); rollback / `set_revision`
  take effect through the owner-scoped run-start reconcile. A granted origin is
  still refused at dial if it resolves private/loopback (D-304). Server-derived
  admin authority, no new scope. The Console `/mcp-connections` `needs_allowance`
  affordance deep-links to the `/agent-config` editor.
- **Protocol-installable OAuth providers, zero-URL broker-pull shape (D-303).**
  `agent_config.set_oauth_provider` (upsert + live install) and
  `agent_config.remove_oauth_provider` (drop + live uninstall) let an admin manage
  OAuth providers over the Protocol without a restart. Honoring D-300, the writable
  descriptor carries **zero URLs** — `{name, credential_broker, scopes?}`; the
  token endpoint, allowed downstream hosts, and audience/scope ceiling are all
  pinned at boot on a named credential broker the descriptor references by name
  (a `client_secret_env`/`token_url`/`remote` field is rejected *by name* via
  strict decoding). Uninstall closes the provider and fails its bound calls loud;
  rollback past an install runs the same uninstall through the owner-scoped
  reconcile (safe cross-tenant only because of D-301). The Console Add-connection
  card gains an `oauth_provider` select from the installed set, closing a silent
  binding drop.
- **`events.aggregate` addressable bucket grid — the `anchor` field (D-306).**
  The aggregator laid its bucket boundaries from the wall-clock instant of the
  call, so a `bucket_start` was never re-requestable and no consumer could cache a
  bucket. An optional `anchor` (a `*time.Time`; absent ⇒ today's clock-anchored
  behavior) floors boundaries onto the fixed grid `anchor + k·bucket`; passing the
  Unix epoch yields a globally shared grid, so two calls at two instants with the
  same anchor/window/bucket share boundary instants and a cold N-bucket backfill
  becomes one cacheable call.
- **`events.aggregate` per-tenant attribution — the `by_tenant` flag (D-307).**
  An aggregate bucket was a bag of scalars with no tenant attribution, so an
  admin-widened count could not be verified against the `tenant_ids` the caller
  asked for. Opt-in `by_tenant` returns `counts_by_tenant` (tenant → event_type →
  count) alongside the totals, **only** for admin-/`console:fleet`-widened reads
  (server-derived), scoped by construction to the authorized tenant set
  (`Σ counts_by_tenant == counts`). Existing callers see byte-identical responses.
- **Session reopen — a closed conversation resumes with its history intact
  (D-312).** RFC §6.9's "reopen-after-close is forbidden" is amended: a `start` /
  `EnsureOpen` on a session whose record is closed or GC-reaped now re-activates it
  in place — the immutable identity and `OpenedAt` are preserved, a
  `LastReopenedAt` is stamped (and the GC hard cap now measures from it, so an old
  conversation stays resumable), and because close/GC never reaped the durable
  events/state/memory, the conversation resumes with its full history. A new
  content-free `session.reopened` event fires. The one terminal exception is an
  **erased** session: reopen fails loud with a new machine-branchable
  `session_erased` code (HTTP 409), gated fail-closed on both the closed-record and
  fresh-create paths against a durable erasure tombstone — a deleted conversation
  is never silently resurrected. The Console Playground/Sessions pages resume a
  closed session and branch on `session_erased` for the deleted-conversation path.
- **Tools facets and aggregates carry real per-tool data (D-314).** The catalog
  projector read OAuth status, approval policy, last-used, error-rate, and
  content-size through an `Annotator` seam that shipped with *no* production
  implementation — so in prod `filter.oauth_statuses` / `filter.approval_policies`,
  the version search axis, and the catalog aggregates all operated over structural
  defaults (D-313 honestly gated them behind a capability toggle in the interim).
  A production `Annotator` is now assembled and wired, reading identity-scoped raw
  data from the owning subsystems (OAuth from `tools/auth`, policy from
  `tools/approval`, metrics/last-used/content-stats from the events stream). This
  also lights up the previously-inert admin write path: `tools.set_approval_policy`
  and `tools.revoke_oauth` now persist (routed back through the owning subsystems
  with audit, never a Console shadow store). `DisplayModes` reads honestly empty
  pending per-MIME MCP negotiation.

### Fixed

- **`events.aggregate` returned HTTP 500 on the durable driver, on every call
  (HA-18/HA-20, D-305).** The aggregator replayed through a per-session path with
  a session-less admin filter that the durable driver correctly refuses — so the
  method worked in dev (in-memory) and 500'd in prod. It now reads through the same
  cross-session windowed fan-in substrate `events.list` uses; a window too wide to
  count within the memory bound returns the partial buckets with an additive
  `truncated` flag (uniformly on both drivers), never a status-code fork. The
  events-driver conformance matrix that let this ship — it never exercised the
  session-less admin read and never covered `events.aggregate` at all — gains a
  driver-parametrized aggregate scenario and a four-method parity leg, plus a
  build gate that fails if any registered driver has no conformance run wired.
- **Widened admin reads folded away the axes they were authorized to read
  (D-308/HA-21).** Both `events.aggregate` and `events.list` unconditionally
  folded every elided identity axis onto the caller's own triple — so a real
  admin/`console:fleet` read naming `tenant_ids:[T2]` with elided user/session
  narrowed to the caller's own (empty) scope, authorizing and auditing a fan-in the
  fold immediately defeated (a silently-blank Console fleet sparkline). The fold is
  now asymmetric: a widened, scope-gated, audited read leaves the elided
  user/session axes wildcard and fans in; only a non-widened own-scope read folds.
  The tenant axis stays name-to-widen (D-284 parity). This also closes the
  session-less fan-in case (HA-21).
- **`sessions.list` / `sessions.inspect` counters were declared but never
  assigned (HA-22, D-309).** Eight `SessionRow` fields — cost, tokens, task/event
  counts, failed-task and pending-intervention flags, agent id/name — shipped on
  the wire permanently zero, while the Service ran facets, a sort, and a keyset
  cursor over them: "sessions over $5" returned empty on a fleet full of them, and
  "most expensive first" silently degraded to an id tiebreak. The six numeric
  counters are now populated at the source through a read-time `Enricher` seam
  (identity-scoped, no shadow store), with an honest `counters_partial` marker when
  a bounded scan truncates — never a plausible-but-low number. The two agent fields
  (no single-valued session→agent binding exists in V1) take the class rule
  instead: representable absence, and `filter.agent_ids` over the unpopulated
  binding loud-rejects rather than returning a false-empty page.
- **`runtime.health` retention horizons were invisible to the one caller that
  observes the fleet (HA-23, D-310).** The `tasks` and `sessions` retention
  horizons were measured at the caller's own triple/tenant scope, so a fleet
  coordinator polling under a service identity (owning no sessions/tasks) received
  only the `events` horizon and silently under-trusted its windowed view. A
  verified admin/`console:fleet` caller now reads both horizons at runtime-wide
  scope (server-derived), and each horizon carries a `scope` marker so an
  unobservable scope is distinguishable from a genuinely empty surface — the
  consumer degrades honestly instead of trusting an absent horizon as
  runtime-wide truth.

### Changed

- **Migration — the downstream-host allow-list is MANDATORY for a bound
  OAuth provider (D-300).** Any `tools.oauth_providers[]` entry that a
  `tools.mcp_servers[]` connection binds via `oauth_provider` MUST now declare
  a non-empty `allowed_downstream_hosts` that lists the connection's host
  (default-port equivalence and case-insensitivity apply). A previously-valid
  config that bound a provider without listing its downstream host now fails
  at **boot** (not at first call), naming the missing field. Add
  `allowed_downstream_hosts: ["<connection-host>"]` to the provider entry.
  A new boot-declared `tools.oauth_credential_brokers[]` list pins named
  credential sinks (token endpoint + allowed downstream hosts + broker
  credential env); it is additive and the inline `oauth_providers[].remote`
  block stays valid. See `examples/dev.yaml` and `docs/CONFIG.md`.
- **A build-time projection-completeness gate, plus one dead memory wire field
  removed (D-311, D-313).** The silent-absence class is now closed mechanically: a
  registry-gated gate (`internal/protocol/projectioncheck`) fails the build when a
  filtered/sorted/aggregated wire field is never assigned by its production
  projector (never-assigned) *or* when the production constructor never wires the
  seam that populates it (never-wired), and asserts every projection surface is
  registered. Fixing the surfaces it caught, the structurally-dead memory
  `has_ttl_expiring` filter and **both** `expiring_in_1h` aggregate fields
  (`MemoryAggregates`, `MemoryHealthAggregate`) are **removed** from the wire —
  V1 memory has no TTL, so these were always empty. This is a within-`0.1.0` wire
  removal taken under an explicit RFC §8 exemption (always-empty fields, no live
  consumer), not a silent drop. A client reading either field should stop; no
  replacement is needed.

## [1.13.1] — 2026-07-13

A scaffold patch release. Both bugs were reported by an external adopter
integrating the `harbor scaffold --with-server` path — the first is a broken
golden path (the scaffolded binary could not boot), the second made the
generated project unbuildable as emitted.

### Fixed

- **`--with-server` scaffold died at boot when `harbor.yaml` declared any
  `tools.built_in` entry.** The generated `RegisterTools` registered the
  declared built-ins at the pre-policy catalog seam; the runtime then registered
  the same names from `tools.built_in` (with their backing SkillStore /
  ArtifactStore / Bus / Redactor), and the catalog rejected the duplicate:
  `open server: tools/builtin: builtin: failed to register built-in tool:
  "clock.now": tools: duplicate tool name`. Built-ins are **config-driven and
  the runtime owns them** — the generated registrar now carries this module's
  **compiled** tools (`tools.custom`) and nothing else, which is the registrar
  seam's whole purpose. Listing a built-in in the yaml is the complete opt-in;
  no Go wiring accompanies it. This also closes a latent second defect: the
  generated `builtin.RegistryContext` was Catalog-only, so a stateful built-in
  (`artifact_fetch`, the `skill_*` set) registered that way would have been
  store-less. A project declaring only built-ins now gets a `RegisterTools` that
  returns `nil` — the seam stays emitted so `cmd/<name>/main.go` keeps a stable
  shape. Corrections appended to D-154 and D-267.
- **The generated `go.mod` was unbuildable as emitted.** It required
  `github.com/hurtener/Harbor v0.0.0-dev` with the `replace` directive commented
  out, and told the reader Harbor "has not yet published a tagged module
  release" — untrue since v1.0.0. The scaffold now emits a real, resolvable
  `require github.com/hurtener/Harbor vX.Y.Z` — the release version of the
  `harbor` binary that scaffolded the project (link-stamped version → the
  binary's embedded build info → the last published release), so
  `go mod tidy && go build ./...` works with no manual edit. A clearly-labelled
  **commented** `replace` remains for contributors building against a local
  Harbor checkout. The version is threaded through **both** seeding paths —
  `harbor scaffold` and the `harbor dev` draft store (`devdraft`) — so a
  promoted draft pins the same release a scaffolded project does. Corrects
  D-087 item 6, which deferred this pinning to release-engineering and shipped
  `v0.0.0-dev` in the meantime. `make drift-audit` now gates the pin: it must
  name a released CHANGELOG section, and may trail the newest by at most one
  release (the deliberate merge→tag window, since a release's CHANGELOG section
  lands before its tag is cut).

### Testing

- **The scaffold-serve smoke now boots a scaffolded binary that declares a
  built-in.** Its probe config carries `clock.now` **alongside** the custom
  tool, so the scaffold → build → boot leg exercises the mixed configuration,
  and the authenticated discovery probe asserts BOTH tools reach the served
  catalog. This is the end-to-end gate whose absence let the bug ship; it is
  verified to FAIL against the pre-fix template with the adopter's exact error.
  The serve-parity integration test declares a built-in on the scaffolded side
  too, in both the in-process parity legs and the live (`HARBOR_LIVE_SERVE`)
  leg.
- `cmd/harbor/scaffold` — new unit gates: the rendered `agent.go` for a config
  declaring both built-ins and custom tools must not reference
  `builtin.RegisterWith` (and must still register the custom tool), and the
  rendered `go.mod` must name a published release with the `replace` commented.
  The scaffold-template and serve-facade smokes gain matching absence pins, so
  a built-in registration cannot creep back into the emitted registrar.
- `internal/devdraft` — new gates on the `harbor dev` draft-seeding path: a
  stamped binary's release version reaches the seeded `go.mod`, and an
  un-stamped one falls back to a published release (never the dev sentinel).
- `make drift-audit` — a new mechanical check on `scaffold.FallbackModuleVersion`
  (verified to bite on both a phantom pin and a two-release trail), so the
  release-time bump is prompted rather than left to prose.

## [1.13.0] — 2026-07-11

The adopter-serving + observability-history release. Two arcs land together.

**Any agent can now serve the Protocol.** An external Go module that assembles
its own runtime with compiled in-process tools was previously headless-only —
it could `RunOnce` but could not mount the northbound Protocol surface, so it
was not a first-class Protocol server without forking the runtime. That gap is
closed: the serve composition (config → subsystems → auth → transports →
listener), formerly trapped in `package main`, is promoted to one importable
constructor; `harbor serve` / `dev` / `console` become thin callers of it, and
the dev-only surfaces (dev-token mint, bootstrap endpoint, draft scaffolding,
Console embedding) compose caller-side through explicit injection seams. A
curated, production-only `sdk/server` facade exposes it, and
`harbor scaffold --with-server` emits a `cmd/<agent>/main.go` that serves the
full Protocol with the project's own compiled tools — at parity with
`harbor serve`, enforced by a both-binary integration gate that boots each from
one config and compares the whole method surface. The compiled-tool registrar
rides the existing pre-policy catalog seam, so an in-process tool inherits every
declared approval / OAuth / policy wrapping.

**The Console stops losing history, and fleet observability gets a durable
read.** Reopening a Playground session no longer drops what a run produced:
per-turn tokens / cost / latency / model and the tool-call badges rehydrate,
and the ordered reasoning-step ↔ tool-call interleaving is reconstructed from
the durable event stream — both by relocating the cost and tool-lifecycle
emissions to driver-neutral seams so the read-back carries what the live stream
always did. A new durable, time-ranged, cross-session `events.list` lets a
Protocol client render historical raw events over a `{since, until}` window
without holding a shadow copy; `flows.runs.list` gains the same time window; and
each durable surface reports its observed retention horizon so a windowed view
degrades honestly at the edge. The MCP southbound driver discovers a connected
server's advertised OAuth requirement (RFC 9728 → RFC 8414) and surfaces it as
inert data on the connection view — the runtime never runs the flow or holds a
token; discovery fetches are SSRF-guarded with a post-resolution dial check.

All additive: the Harbor Protocol holds at `0.1.0` (no error-code or version
change), and existing configs stay byte-compatible (a config-free runtime is
byte-identical to v1.12). Seven phases, eight new decisions (D-291…D-298).

### Added

- **Serve-band promotion — one importable serve constructor.** *(runtime)*
  The `bootDevStack` composition that turned a validated config into a running
  Protocol server was reachable only from `package main`, so no external module
  or test kit could mount it. It is promoted to `internal/runtime/serve` with a
  required auth-validator factory (identity is mandatory; a nil factory fails
  loud); the dev signer never promotes, and dev-only routes/surfaces are
  composed caller-side via `ExtraRoutes` / `BuildAuthSurface` /
  `BuildLLMSnapshot` / `PostBoot` seams. `harbor serve` / `dev` / `console` and
  `harbortest/devstack` all become callers of the one constructor, deleting the
  kit's hand-mirrored mux (net −900 lines). Purely internal wiring; no wire
  change. (D-291)
- **`sdk/server` facade + `harbor scaffold --with-server` + parity gate.**
  *(sdk)* A curated `sdk/server.Open(ctx, cfg, {RegisterCatalog})` mounts the
  promoted server behind a **production-only** JWKS posture — it always builds
  the operator's verifier from `identity` and fails loud when absent; no dev
  signer, no mock knob, no dev surface is reachable. `harbor scaffold
  --with-server` (opt-in; the default scaffold stays headless) emits a
  `cmd/<name>/main.go` that loads yaml, blank-imports the production driver
  aggregator, passes the project's `RegisterTools` to the facade, and serves.
  The compiled-tool registrar fires at the catalog's pre-policy point so an
  in-process tool is wrapped by declared `tools.entries` policy exactly like any
  other. A `test/integration` gate boots stock `harbor serve` and a scaffolded
  binary from one config and asserts method-surface parity + custom-tool
  dispatch; the wire-level leg runs env-gated against a real LLM. The
  headless-embed path stays the default (D-091); external Protocol serving is a
  decided contract with a deprecation window (RFC §5.6). (D-292)
- **Session rehydration carries per-turn metadata.** *(runtime, console)* A
  reopened Playground session recovered message text but lost every per-turn
  stat, the model chip, and the tool-call badges — because `llm.cost.recorded`
  was emitted only by the bifrost driver and `tool.*` only by the in-process
  transport, so the durable read-back was missing them. The cost emission moves
  to the mandatory LLM-edge safety wrapper (one event per driver completion,
  every driver) and the tool-lifecycle emission moves to a catalog-build
  descriptor shell (every transport, every dispatch path — single, parallel,
  MCP-Apps, declarative), each carrying the full run quadruple (closing a latent
  empty-`RunID` attribution bug). The Console reducer folds the now-present
  metadata so reopen renders identically to live. Content-free throughout
  (D-026 / §7 preserved). Zero wire change. (D-293)
- **`events.list` — durable, time-ranged, cross-session raw-event read.**
  *(protocol)* A Protocol client rendering historical fleet observability had
  only a forward-only live tail and a counts-only aggregate — no way to read the
  actual event rows over a `{since, until}` window without a forbidden shadow
  store. The additive `events.list` reuses the existing `EventFilter` (its
  `since`/`until` axes finally consumed) plus a tail-first sequence cursor
  mirroring `state.history`, returning the same bus-redacted rows the SSE
  projects. Fleet-widening derives server-side from the verified session and
  requires the closed `admin` OR `console:fleet` scope set; the per-read
  `truncated` flag marks the retention edge. Both event drivers implement the
  windowed read (in-memory honest-ring, durable real-window). The Console Events
  page gains the historical window as its same-phase consumer. Full lockstep +
  smoke. (D-294)
- **`flows.runs.list` time window + observed retention horizons.**
  *(protocol)* `flows.runs.list` gains optional `since`/`until` (inclusive,
  mirroring `TaskFilter` for cross-method consistency), bounded on `StartedAt`
  before pagination. `runtime.health` gains a `retention` block reporting each
  durable surface's **observed** oldest-retained timestamp (surfacing what is
  actually retained — V1 has no prune knob — via an optional cross-driver
  capability), so a consumer promising "the last N days" degrades honestly at
  the window edge, pairing with `events.list`'s at-read `truncated` flag.
  Counters/metrics deliberately do NOT become a time-series substrate. (D-295,
  D-296)
- **MCP OAuth requirement discovery, surfaced as data.** *(tools)* When the MCP
  southbound driver meets the MCP-auth-spec challenge (401 +
  `WWW-Authenticate resource_metadata`) or an operator probe, the runtime walks
  the RFC 9728 protected-resource-metadata → RFC 8414 authorization-server chain
  and surfaces the discovered endpoints/scopes **verbatim, as inert data** on
  the MCP-connection view — a proposal an operator confirms, never auto-trusted
  config. The runtime never runs the OAuth flow, never holds or refreshes a
  token (the broker-pull custody model, D-271, is preserved); RFC 7591
  registration is reported, never invoked. Discovery fetches are SSRF-guarded:
  same-origin default on the resource hop, explicit per-connection allowance +
  private-range/IP-literal refusal at **post-resolution dial time** (DNS-rebind
  defence), bounded redirects, size/time caps, https-only, no credentials.
  Conformance fixtures derive from the real RFC example documents. The one
  discovery chain is single-homed for the parked flow-execution siblings to
  reuse. (D-297)
- **Structured reasoning-step rehydration on session reopen.** *(console)* The
  ordered reasoning ↔ tool-call interleaving (which thinking preceded which
  tool call) survived only in the live stream; reopen collapsed it to a flat
  blob. The Console history reducer now reconstructs the per-step structure from
  the same durable `planner.decision` + `tool.*` events, folding a reasoning
  step only for trajectory-appending decision kinds
  (`CallTool` / `CallParallel` / `SpawnTask` / `AwaitTask`) so the reopened index
  is byte-identical to the live enricher's — `Finish` / `RequestPause` emit a
  decision but no step and are correctly excluded. Zero wire change; the
  reasoning trace rides a `SafePayload` and is persisted raw. (D-298)

### Fixed

- **The Playground Controls panel silently broke every override.** *(console)*
  The panel composed a `top_p` field the runtime's `RunOverrides` never had, so
  the strict `runs.set_overrides` decoder rejected the whole request — reasoning
  effort, temperature, max-tokens, and system-prompt overrides all failed. The
  unsupported field is removed and `RunOverrides` is promoted to a named,
  lockstep-gated TS interface so a phantom key is now a compile error rather
  than a silent runtime rejection.
- **A latent trajectory data race** between the serve enricher and the steering
  run loop's step append (predating this wave) is closed by a per-run mutex +
  snapshot, surfaced and fixed under the rehydration work.
- **A DNS-rebinding hole** in the MCP OAuth discovery guard (the private-IP
  check ran on the pre-resolution hostname) is closed with a post-resolution
  `net.Dialer.Control` check.
- **A cross-user event-disclosure gap** — `events.list` / `events.aggregate` /
  `events.subscribe` gated the tenant axis but not a foreign same-tenant user —
  is closed in the shared filter path (§6 multi-isolation).
- **~35 internal phase-number references** in godoc-visible source are rewritten
  to name the feature, and the drift-audit pattern is tightened to catch the
  hyphenated form that let them through (§13).

## [1.12.0] — 2026-07-09

The name-your-sessions release: sessions stop displaying as raw ids. The
canonical session record gains an optional human-readable title with
provenance, a new Protocol verb lets a caller rename any session they own —
with the Console Sessions page and Playground switcher shipping the rename
affordance in the same wave — and the runtime can now title a session
itself: an opt-in, default-off auto-naming policy that makes one governed
LLM call at a run's terminal boundary and can never overwrite a human's
name. A latent lost-update race on the session record, predating this wave,
is closed by serializing every whole-record writer. One new Protocol
method, two new canonical events, one new config surface; all additive —
the Harbor Protocol holds at `0.1.0` (no error-code or version change), and
existing configs stay byte-compatible (a config-free runtime is
byte-identical to v1.11).

### Added

- **Session titles — `Title` + `TitleSource` on the record,
  `sessions.set_title`, Console rename.** *(protocol)* Sessions displayed
  as raw ids everywhere, and a coordinator consumer must not shadow-store a
  human-readable name (D-061) — so it lands framework-side. The session
  record gains `Title` plus a `TitleSource` provenance mark
  (`unset | auto | manual`), persisted as an additive JSON round-trip
  through the existing `session.lifecycle` kind (zero migration) and erased
  with the record by the existing `DeleteScope` cascade. The new
  `sessions.set_title` verb ALWAYS writes `manual` — `auto` is not
  expressible over the wire, so the internal auto-namer is its sole
  producer and manual-wins is structurally unforgeable. The write is scoped
  to the owning `(tenant, user)` — the scope `sessions.list` reads at — so
  a Console-style "rename any of my sessions" flow needs no elevation;
  metadata-only, no admin widening, and a cross-identity target is
  `not_found` (existence is never revealed). Empty-after-trim clears;
  over-bound (> 200 runes), multi-line, or invisible-only
  (format-character) input fails LOUD with `400 invalid_request` — never a
  silent clamp. The title is user-derived content and never rides an
  event, log, or audit payload: `session.title_changed` is content-free
  (`{session_id, source}`), and consumers refetch — `sessions.list` /
  `sessions.inspect` project `title` / `title_source` on every row. The
  §13 consumers ship same-wave: the Sessions page renders the truncated
  title (full title + id in the tooltip, id fallback) with an inline
  rename, and the Playground switcher renders `title || session_id` with
  an active-session rename and event-driven list refresh. (D-288.)
- **Session auto-naming — opt-in policy + terminal-boundary titling.**
  *(runtime)* Opt-in and DEFAULT OFF: with no `naming` config anywhere the
  runtime is byte-identical to v1.11 — zero counters, zero LLM calls, zero
  events (test-pinned). Policy home pairs a versioned agent-config
  `naming` section riding the existing `set_revision` (no new verb; joins
  the D-283 rebuild-completeness guard) with a yaml `runtime.naming` fleet
  default — precedence agentcfg › yaml › off, resolved once at run start
  (next-turn projection). A PRESENT section is authoritative either way: a
  bare `{auto: false}` revision is an explicit per-agent opt-out that wins
  over a yaml-on fleet default (section presence is the signal — never
  dropped as inert). Knobs: `after_turns` (default 1), `repeat_every`
  (0 = name once), `max_repetitions` (required ≥ 1 whenever repeating —
  no unlimited value exists, so unbounded periodic re-naming is
  unrepresentable; default 5 for programmatically built policies),
  `max_title_len` (default 80; auto output is deterministically clamped —
  the manual verb, by contrast, rejects; the trusted-internal vs
  untrusted-boundary asymmetry is intentional), and `model` (empty = the
  run's effective model; point it at a cheap profile — the spend is
  deliberately the tenant's). The mechanism is a sibling of the
  run-completion hook at the run loop's terminal boundary — it fires after
  `(fin, err)` settle and never alters them, with the hook firing FIRST so
  a slow naming call can never inflate the hook payload's timing — making
  ONE governed `Complete` call on the run's already-wrapped LLM client
  (governance outermost: a ceiling/rate block SKIPS naming loudly and the
  run is untouched) over a ≤ 4 KiB bounded transcript digest, then writing
  through the internal `SetTitleAuto` path, which refuses `manual` titles
  and updates title + counters in one record save. Failure is loud but
  contained: `session.naming_failed` carries a stable error class
  (`llm_error` / `timeout` / `empty_title` / `governance_blocked` /
  `manual_title` / `internal`), never content — and a failure does not
  burn the cap: a still-due title retries on every completed run until one
  succeeds (the naming call is bounded by a fixed 10s runtime timeout;
  worst-case post-run latency is hook + naming timeouts, serialized).
  (D-289.)

### Fixed

- **Lost-update race on the session record.** *(runtime)* The registry's
  whole-record writers — the pre-existing `Touch` and GC-reap, joined by
  the new `SetTitle` — did load→mutate→save without holding the registry
  lock across all three steps, so a write racing `Close` could re-persist
  `Closed=false` (resurrecting a closed session as a GC-invisible zombie,
  violating reopen-after-close), and one racing `sessions.delete` could
  re-persist the record after the irreversible clear (empirically
  confirmed pre-fix). Every whole-record writer (`Touch` / `Close` /
  `SetTitle` / the GC reap) now flows through ONE serialized
  read-modify-write path, and the erasure cascade's `DeleteScope` runs
  under the same mutex — pinned by interleave tests verified to fail
  against the pre-fix code. Surfaced by D-288's verb widening the writer
  set; the fix covers the pre-existing writers too. (D-288.)
- **Lost-update race on the session-discovery catalog.** *(runtime)* The
  per-`(tenant, user)` discovery catalog — the index that lets
  `sessions.list` survive a restart, since the StateStore has no List — did
  its own read-modify-write WITHOUT the registry lock held across all three
  steps: an erasure's catalog remove racing a concurrent `Open` of a
  sibling session could save a stale set-difference that dropped the
  freshly-opened session's catalog entry. The session's record survived but
  became invisible to `sessions.list` and was never re-discovered after a
  restart (nor GC-swept). Both catalog writers (the `Open` add and the
  erasure remove) now flow through ONE serialized read-modify-write path,
  and the erasure's catalog remove is folded into its in-memory-clear
  critical section under the registry lock — pinned by an Erase-vs-Open
  interleave test asserting a fresh registry over the same store
  re-discovers every surviving session. Surfaced by D-288's `set_title`
  widening the writer set alongside the record race above. (D-288.)
- **Auto-naming re-arm after a manual clear.** *(runtime)* Clearing a
  session's title via `sessions.set_title` reset the title + provenance but
  NOT the naming counters, so once any auto-name had landed a clear→re-arm
  never fired under a name-once policy (`repeat_every: 0`) — the documented
  unqualified re-arm was a silent dead-end. A manual clear now zeroes the
  naming counters (`AutoNameCount` / `LastAutoNamedTurn`) in the same record
  save, opening a fresh arming cycle; the `max_repetitions` cap is therefore
  per-cycle, not per-session-lifetime. A no-op `set_title` (clearing an
  already-unset title, or an identical manual re-set) now also suppresses
  the redundant `session.title_changed` event. (D-289.)
- **Agent-config `hooks`-section presence footgun.** *(runtime)* A
  `set_revision` carrying a bare `hooks: {}` (or a run-completion section
  with an empty tool) returned 200 but was silently normalised away, so a
  per-agent opt-out of run-transcript egress fell through to the yaml fleet
  hook and kept dispatching. A PRESENT hooks section is now authoritative
  (mirroring the naming-section presence rule): an empty section is the
  explicit per-agent NO-HOOK that overrides the yaml default, `NormalizePayload`
  preserves any non-nil section, and `agent_config.diff` gains a presence
  dimension — carried to the wire as the additive
  `AgentConfigHooksDiff.section_present_*` fields (TS mirror, wire manifest,
  and generated Protocol reference regenerated) — so the opt-out is a
  visible revision end to end. The request shape is unchanged. (D-290.)

### Internal

- One additive Protocol method (`sessions.set_title`) with its
  request/response wire types; two additive canonical events
  (`session.title_changed`, `session.naming_failed`); the additive
  `SessionRow.title` / `title_source` projections and the
  `AgentConfigNaming` section + its diff arm. The generated Protocol
  reference and the TS wire manifest were regenerated for every wire
  change — all additive, so `ProtocolVersion` holds at `0.1.0`, and no
  error code was added (an invalid naming policy maps to the existing
  `invalid_request`/400 class). The wave-end checkpoint additionally closes
  the pre-existing session-discovery-catalog lost-update, re-arms
  auto-naming after a manual clear (per-cycle `max_repetitions`), and makes
  an agent-config `hooks`-section presence authoritative (D-290) — the only
  checkpoint wire change is the additive
  `AgentConfigHooksDiff.section_present_*` diff fields, so `ProtocolVersion`
  still holds at `0.1.0` — and lands the wave E2E
  (`test/integration/wave_v112_test.go`) composing the title lifecycle,
  identity-scoped event discipline, own-vs-foreign refusal, governance
  interplay, live-revision precedence, and a cross-tenant concurrency stress
  under `-race`.

## [1.11.0] — 2026-07-05

The fleet-and-lifecycle release: the control plane a coordinator drives over
many runtimes grows the surfaces it was missing — an admin observer can now
enumerate tasks and agents across every session on a runtime, a broker
credential minted after boot reaches a running runtime without a reboot, a
runtime-added MCP connection can actually be removed (not just paused), and the
right-to-erasure audit record becomes part of erasure's success criteria
instead of a best-effort afterthought. A latent silent-degradation bug in the
agent-config revision model — any section edit erased a pinned run-completion
hook — is closed and made mechanically impossible to reintroduce. One new
Protocol method, three new canonical events, one new config surface; all
additive — the Harbor Protocol holds at `0.1.0` (no error-code or version
change), and existing configs stay byte-compatible.

### Added

- **Admin-widened fleet enumeration for `tasks.list` + `agents.list`.**
  *(protocol)* Both list methods projected only the caller's own
  `(tenant, user, session)` triple, so a coordinator's synthetic observer
  session saw nothing. An additive `filter.tenant_ids` selector widens the
  read to every task / agent across all sessions of the named tenants —
  riding the SAME verified `auth.ScopeAdmin` claim as `sessions.list` (no new
  "fleet" scope vocabulary). A `tenant_ids` request without the claim fails
  LOUD with `403 scope_mismatch`, never a silent narrow; every widened row
  carries full per-`identity` attribution and every widened call emits
  `audit.admin_scope_used`. The widening rides a SEPARATE explicit
  tenant-scoped enumeration seam on each registry (`ListTenant`, with
  conformance parity across the inprocess and durable drivers) — never an
  optional/blank session on the identity-scoped read, so no
  identity-downgrading knob (§13). Cross-runtime federation stays
  coordinator-side, the same division as sessions/events. This lands the
  "future cross-runtime aggregating projector" the tasks projector godoc
  reserved, behind the unchanged `Projector` interface. (D-284.)
- **OAuth provider credential source — `env` or coordinator-served pull.**
  *(tools)* A `tools.oauth_providers[]` client credential was resolved once at
  boot from the process env, so a broker credential a coordinator mints AFTER
  a runtime booted could never reach it — forcing a one-reboot provisioning
  step. A §4.4 `credential_source` seam on the provider entry closes it:
  `env` (today's boot-time, fail-loud resolution — the DEFAULT, existing
  configs byte-compatible) or `remote` (the runtime PULLS
  `client_id`/`client_secret` from a coordinator endpoint at first need,
  authenticated by its own service token, memory-only TTL cache, single-flight,
  strict `format_version`-ed parse). Fail-loud everywhere: boot validates the
  declared source's shape; a fetch failure fails the tool call with a typed
  sentinel plus a SafePayload audit event — NEVER a fallback to env, to an
  unauthenticated call, or to the interactive flow. The `remote` endpoint must
  be https (loopback the one carve-out) and redirects are refused. Declaring
  both sources on one entry is a validation error; `remote` is valid only for
  the non-interactive `tokenexchange` driver. Push over the Protocol stays
  rejected (the credential-passthrough shape D-271 forbids). Defense-in-depth:
  with `remote`, the broker secret never enters the runtime's environment at
  all. Two new canonical events (`tool.provider_credential_fetched` /
  `tool.provider_credential_fetch_failed`); no Protocol wire-type change.
  (D-285.)
- **`agent_config.remove_mcp_connection` + detach-on-reconcile.** *(protocol)*
  Pause could disable a runtime-added MCP connection but never remove it (the
  descriptor persists forever; resume resurrects the server) — so a
  coordinator's delete flow had no path. A new admin revision verb drops the
  named descriptor AND prunes that server's tool-exposure residue atomically
  (sibling-safe: an entry a remaining server's `<name>_` prefix also claims is
  never pruned); unknown-name and boot-declared-yaml-name each fail loud with
  distinct typed errors. Run-start reconciliation gains the detach leg:
  the declared-vs-attached diff deregisters an undeclared server from the
  catalog + MCP registry and closes its transport at a run-start reconcile
  boundary — never mid-run; a rollback past an add detaches through the SAME
  path (one mechanism, §13). Honest in-flight semantics: exposure correctness
  is next-turn per session; teardown is process-global, so a different
  session's in-flight run that next calls the detached server fails LOUDLY
  (typed catalog-not-found / closed-transport) — never a hang or silent
  success. Agent-bound sealed tokens are NOT deleted on remove (re-add reuses
  completed consent; revocation is provider-side). New canonical
  `mcp.connection.removed` event. This supersedes D-240 decision 5's
  detach-on-rollback deferral via that entry's own recorded revisit clause;
  the 92k–92q MCP-OAuth band stays parked, so the run-start reconcile
  mechanism ships DETACH-ONLY (the attach leg remains deferred with the
  parked band). (D-287; supersedes D-240 §5.)

### Fixed

- **Agent-config section setters no longer erase a pinned run-completion
  hook.** *(runtime)* Each of the five section-scoped setters
  (`set_tool_exposure`, `add_mcp_connection`, `set_skills`,
  `set_prompt_layers`, `set_llm_params`) rebuilds the `ConfigPayload` by hand,
  enumerating the sibling sections known when it was written — so when the
  `Hooks` section landed (v1.10, D-280), none of them carried it forward, and
  any tool-exposure / connection / skills / prompt-layer / LLM-params edit
  silently erased a pinned hook (deterministic, same-surface, no error — the
  §13 silent-degradation shape a coordinator pinning an auto-save hook trips on
  its golden path). All five now carry `Hooks` forward, and a
  reflection-backed rebuild-completeness guard makes the invariant mechanical:
  a seed constructor populates every payload section, reflection-asserts full
  field coverage, then asserts each setter preserves every non-target section
  byte-identically — adding a future section without extending every setter
  fails `go test`, naming the field. The omission class is closed, not just
  this instance. No wire change. (D-283.)
- **Session-erasure audit record is now part of erasure's success criteria
  (closes #409, #410).** *(runtime)* `session.erased` (the right-to-erasure
  record-of-fact, D-262) was emitted best-effort AFTER the irreversible clear:
  one bus/redactor failure lost the only audit record while the call reported
  success, and a re-invoke returned `not_found` — no second chance. Separately,
  a retried mid-cascade erasure re-ran idempotent deletes that found fewer
  records, under-reporting deletion counts. Now the ordering invariant is
  binding — a durable compliance checkpoint (an erasure ledger) is persisted
  BEFORE the irreversible clear, and the final record-of-fact is part of the
  success gate: a record/emit failure fails `sessions.delete` LOUD with the
  new typed sentinel (`ErrErasureRecordFailed` → HTTP 500) with the session
  still re-invokable, and a re-invoke converges (skips every destructive step,
  re-attempts only the record). Deletion counts accumulate across converging
  attempts, so the response and event report true totals — the #410
  "document the undercount" alternative was rejected (compliance counts must be
  accurate, not documented-as-inaccurate). No wire-shape change; field docs
  state the cumulative semantics. (D-286.)

### Internal

- Three additive canonical events (`mcp.connection.removed`,
  `tool.provider_credential_fetched`, `tool.provider_credential_fetch_failed`);
  one additive Protocol method (`agent_config.remove_mcp_connection`) with its
  request/response wire types; the additive `filter.tenant_ids` selector on
  `tasks.list` / `agents.list`. The generated Protocol reference and the TS
  wire manifest were regenerated for every wire change — all additive, so
  `ProtocolVersion` holds at `0.1.0` and no error code was added.

## [1.10.0] — 2026-07-02

The reach-the-wire release: the typed answers and brokered credentials v1.9
shipped for the embed path now reach the Protocol and the MCP wire, and the
runtime grows its first run-lifecycle egress. Any task can return a
schema-validated answer, a shared MCP server receives a per-identity bearer
plus call provenance on every request, the `tools.http_manifests` knob goes
from documented-but-rejected to live at boot, an operator-named catalog tool
receives every run's full transcript at completion, and tool loading modes
become runtime-controllable per agent. Governance also stops undercounting:
every retry and downgrade attempt now reaches the cost ceilings — see Fixed.
Additive wire types + two new canonical events; the Harbor Protocol holds at
`0.1.0` (no method, error, or version change this release).

### Added

- **`output_schema` on `start` — per-task structured output.** *(protocol)*
  The run-level mechanism v1.9 shipped for `Stack.RunOnce` gains its Protocol
  producer: one additive field on the `start` request asks any task for a
  schema-conforming final answer. The schema is compile-rejected at the edge
  (`400 invalid_request`, before any task spawns), compiled once at run
  start, steers the React driver through the existing per-turn mechanism
  with zero planner change, and the terminal answer is validated through the
  ONE shared envelope builder `RunOnce` also uses — the validated object
  lands as the task envelope's `answer_payload`, readable via `tasks.get`'s
  `result_inline` and a parent run's AwaitTask observation (the heavy-output
  offload applies unchanged). A schema-invalid answer after the correction
  budget fails the task loud with the new `output_invalid` terminal code —
  never a schemaless success — and schema-constrained tasks suppress
  assistant token deltas (the validated answer arrives once, on completion).
  A reused idempotency key carrying a different schema is a loud
  `ErrIdempotencyConflict`. Per-run granularity by design — deliberately not
  agent config. (D-276.)
- **MCP southbound per-identity OAuth bearer + `_meta` provenance.**
  *(tools)* A non-secret `oauth_provider` name on an MCP connection (yaml,
  agent-config descriptor, and wire) binds a declared
  `tools.oauth_providers[]` entry — the v1.9 `tokenexchange` broker
  credential included — and every identity-stamped per-call RPC injects a
  fresh per-identity `Authorization: Bearer` via a context-aware
  RoundTripper: the token rides the per-call ctx, so one shared transport
  serves N concurrent identities with no bleed. Fail-closed is the
  load-bearing invariant — a bound provider whose token fetch fails aborts
  the call with NO wire request, never an unauthenticated fallback; a
  `consent_required` refusal parks on the unified pause primitive. `_meta`
  gains provenance: the registration `agent_id` (attribution metadata, never
  an isolation principal) plus operator-declared non-secret
  `meta_annotations`, with reserved keys rejected at validation. A static
  `Authorization` header alongside a binding, or a binding on a connection
  that would select stdio, is rejected at validation. (D-278.)
- **`tools.http_manifests` goes live — the HTTP-manifest boot loader.**
  *(tools)* The documented-but-dead knob is wired: assembly loads each
  UTCP-style manifest at boot and registers its tools by name — after
  built-ins, before `tools.entries[]` middleware — so the existing by-name
  `oauth` / `approval` / `loading_mode` bindings apply to manifest tools
  with zero new machinery. This is the first config-only, black-box path
  through catalog OAuth wrapping (token exchange included): config in,
  brokered-credential pre-check + HTTP round-trip out. `harbor validate`
  flips from rejecting a populated list to validating it; relative entries
  resolve against the config file's directory under the path-safety posture;
  a missing, unparseable, or invalid manifest — or a tool-name collision —
  fails boot naming the file and the config key, never a silent skip.
  Boot-only (restart-required). (D-279.)
- **The run-completion hook — transcript egress through the tool catalog.**
  *(runtime)* The runtime's first run-lifecycle hook (new RFC §6.17):
  `RunLoop.Run` fires it exactly once at its terminal boundary — every
  terminal outcome, carried in the payload; never mid-run, never on pause —
  dispatching a typed, versioned `RunCompletionPayload` transcript (initial
  goal, mid-run steering messages in arrival order, assistant steps, final
  answer, identity + the true tool-invocation count) to an operator-named
  catalog tool through the existing executor path, so provenance, identity,
  per-tool policy retries, and args-free audit events come free (a bespoke
  webhook subsystem is the rejected parallel implementation). A hook failure
  never alters the settled run outcome — `run.hook_failed` + a Warn; success
  emits `run.hook_dispatched` (both metadata-only, never transcript
  content). Cancelled runs fire under a bounded detached ctx with identity
  values preserved. Config pairs yaml
  `runtime.hooks.run_completion.{tool,timeout}` with a versioned
  agent-config `hooks` section on the existing revision surface (no new
  verb); embedders get a per-call `WithCompletionHook` RunOption. One seam
  covers embed, foreground, and background runs uniformly — including the
  runs no client observes. (D-280.)
- **Runtime loading-mode control on tool exposure.** *(protocol)*
  `agent_config.set_tool_exposure` gains `server_loading_modes` (per MCP
  source id, tool-form descriptors only via the new additive `Tool.Form`
  classification) and `tool_loading_modes` (exact per-tool name), valued
  `always` / `deferred`, with ONE pinned precedence order — per-tool >
  per-server > boot `tools.entries[].loading_mode` > driver default.
  Applied next-turn at the shared run-start projection via a new
  `LoadingOverrideView`: a deferred tool drops out of the prompt-time
  catalog but stays `tool_search`-discoverable and callable (disable stays
  strictly stronger — hidden from prompt AND dispatch); in-flight runs keep
  their snapshot. Admin tier only — loading is not capability-narrowing. An
  unknown mode value fails `400 invalid_request` before any revision is
  recorded; `agent_config.diff` renders structured loading arms.
  `tools.describe` gains an optional `agent_id` reporting the projected
  effective `loading_mode`. (D-281.)

### Fixed

- **Governance now accounts every LLM attempt — the in-band attempt-cost
  tap.** *(governance)* Governance composes outside the retry wrapper
  (deliberate: the ceiling check must run before any spend), so every
  intermediate corrective re-ask and downgrade attempt was a real provider
  call invisible to the `CostAccumulator` — worst case `(MaxRetries+1)×3`
  uncounted calls per planner turn, live since v1.9's validator loop went
  production-real (the gap D-272 recorded). `governance.Wrap` now installs a
  per-call attempt-cost tap after `PreCall` permits; the retry and downgrade
  wrappers synchronously report each attempt they consume; `PostCall` drains
  the tap once and folds it with the final response's cost under the
  existing identity key — exactly-once by the propagate-or-report invariant,
  with the compose order, `PreCall` short-circuit semantics, and `resp.Cost`
  meaning all unchanged. Operators take note: recorded spend now includes
  retry/downgrade attempts, so per-identity totals rise and ceilings trip
  sooner — by design; attempt spend accumulates even when the outer call
  ultimately errors. (D-275.)

### Internal

- The events subsystem gained its conformance-suite home
  (`internal/events/conformancetest` — 20 pinned scenarios plus a fail-loud
  capability gate, spanning fence semantics, Bounds/Window history, replay
  cursors, subscribe scoping, and close lifecycle), and both bus drivers
  consume it from their own tests, folding the hand-copied per-driver twins;
  six scenario cells are coverage gains on the driver that lacked them. Zero
  production change. (D-277.)
- The distributed durable-bus conformance flood test is bounded to the
  subscriber buffer, deflaking `Concurrent_Publish_NoRace`.
- Two additive canonical events (`run.hook_dispatched` / `run.hook_failed`);
  the generated Protocol reference and the TS wire manifest regenerated for
  every wire change (`start.output_schema`, the MCP descriptor's
  `oauth_provider` / `meta_annotations`, `AgentConfigHooks` + its diff arm,
  the exposure loading maps, `tools.describe.agent_id`) — all additive, so
  `ProtocolVersion` holds at `0.1.0`.

## [1.9.0] — 2026-07-02

The typed-output-and-brokered-credentials release: the embed path now answers
in your types, and a fleet can hold tool credentials in one place instead of
N runtimes each acquiring their own. `Stack.RunOnce` gains an opt-in run-level
JSON Schema whose validated answer lands on a new envelope key, `RunTyped[T]`
derives that schema from a Go type and hands back the unmarshaled struct, and
the new `tokenexchange` OAuth driver pulls downstream tool credentials from an
external broker at token-miss time — never persisting them. Two pre-wave
integrity fixes land with it, one of which changes the *meaning* (not the
shape) of an SDK-visible envelope field — see Changed. Additive public API +one
new canonical event; the Harbor Protocol holds at `0.1.0` (no method, error, or
wire-type change this release).

### Added

- **`WithOutputSchema` — run-level structured output.** *(embed)* One run
  option asks `Stack.RunOnce` for a schema-conforming final answer: the
  terminal payload is validated at the run edge for every planner (no
  capability ceremony), delivered as the additive `answer_payload` envelope
  key (`AnswerEnvelope.AnswerPayload`; the pinned three-key byte shape is
  untouched and `Answer` keeps the string rendering), and a schema-invalid
  answer after the profile's bounded retry budget is a typed
  `planner.ErrOutputInvalid` — never silent unvalidated text. The React driver
  additionally constrains the terminal completion through the existing
  `OutputMode` strategy and the `Validator`-keyed retry-with-feedback loop —
  no new knob. `WithStream` composes: `step` / `tool_dispatched` events stream
  as today; token chunks are suppressed on a schema-constrained run (a
  validate-and-retry loop cannot retract streamed tokens), and the validated
  answer arrives once, in the envelope. Partial-object streaming is the named
  follow-up. (D-272.)
- **`RunTyped[T]` — the typed embed binding.** *(embed)* A generic free
  function, `assemble.RunTyped[T](ctx, stack, goal, id, opts...)`, that
  derives the output schema from `T` (via the same reflection deriver
  `RegisterFunc` uses, promoted to a shared neutral package), runs
  schema-constrained, and returns the validated answer already unmarshaled
  into `T`. All failure modes loud: an unsupported `T` fails at call time
  before any LLM spend; a caller-supplied `WithOutputSchema` alongside it is
  `ErrRunTypedSchemaConflict`; a validated-but-unmarshalable payload is
  `ErrRunTypedUnmarshal`. Deliberately a free function over the shared
  immutable `Stack` — no stateful binding object; the `sdk/assemble` forward
  is the facade's second (and gate-enumerated) generic-func carve-out. (D-273.)
- **`tokenexchange` — pull-based external tool credentials.** *(tools)* A new
  non-interactive driver on the OAuth flow-strategy registry
  (`tools.oauth_providers[].driver: tokenexchange`): at token-miss time the
  runtime performs an RFC-8693-shaped exchange against an operator-configured
  credential broker (a fleet orchestrator, a token vault, an STS), presenting
  its own env-indirected broker credential plus the **verified** ctx identity
  triple — so one central grant serves N runtimes instead of N consents and N
  sealed copies. Brokered tokens are TTL-cached in memory only, single-flighted,
  and never persisted (`TokenStore.Put` is never called — the broker stays the
  single source of truth). Broker failure fails the run loudly (typed
  `ErrExchangeFailed`, no silent interactive fallback); a `consent_required`
  refusal parks on the unified pause primitive via the existing typed
  `ErrAuthRequired`; interactive-flow methods return the typed
  `ErrNonInteractive`. Every actual exchange emits the new canonical
  `tool.credential_exchanged` audit event (zero token bytes). Push-style
  credential injection over the Protocol is rejected as credential
  passthrough — recorded so it is not re-litigated. (D-271.)

### Changed

- **`AnswerEnvelope.ToolCallsSeen` now counts true tool invocations —
  embedders take note.** *(embed)* The field previously reported the
  trajectory step count: a parallel tool call (N branches) counted 1, and a
  task spawn/await counted 1 despite dispatching no tool. It now sums real
  dispatches per decision (`CallTool` = 1, `CallParallel` = N, spawn/await = 0)
  via the shared `planner.CountToolInvocations`. The JSON key, type, and
  envelope byte shape are unchanged — only the value's meaning — but the field
  is SDK-visible, so embedders who assumed step-count semantics should re-check
  consumers. The same rule now drives the Console task `tool_count` and
  `WithStream`, which emits one `tool_dispatched` event per dispatched tool
  (N events for a parallel call, none for spawn/await); the two counters
  deliberately diverge on failures (`ToolCallsSeen` counts attempted
  invocations, `tool_count` successful dispatches). (D-274.)

### Fixed

- **Session erasure fails loud when the event-bus fence fails.** *(protocol)*
  `sessions.delete`'s cascade eraser documented "a Fence error fails the
  erasure loud" but actually logged and reported unqualified success with the
  late-event window still open. A fence error now fails `Erase` before the
  first destructive step runs — nothing is deleted, and a retry after the
  fault clears converges to a full erasure. A bus that doesn't implement
  fencing at all remains the documented warn-and-proceed capability downgrade.
  Both branches gained their first tests. (D-274.)

### Internal

- The Go-type→JSON-Schema deriver moved from the in-process tool driver to the
  neutral `internal/tools/schema` package (golden-pinned byte-identical), also
  fixing a pre-existing seam violation where the flow engine imported the
  concrete driver directly.
- One additive canonical event (`tool.credential_exchanged`) — the generated
  Protocol reference regenerated; no method, error, or wire-type change, so
  `ProtocolVersion` holds at `0.1.0`.
- Wave-end checkpoint: composing E2E (`test/integration/wave_v19_test.go`)
  across the wave's surface — schema-constrained runs dispatching real tools
  through the OAuth-wrapped catalog, park/resume under an output schema,
  per-branch invocation counting on the envelope, and N-way mixed typed/plain
  concurrency against one shared Stack.

## [1.8.0] — 2026-06-27

The adopter-path release: make all three advertised ways into Harbor — embed,
CLI, and Protocol — work end to end, and make the public surface honest about
what ships. v1.7 advertised the paths; only one was honest end-to-end. This
release closes the serve-attach cliff with two on-ramps, delivers a production
one-call runner with first-class streaming, makes a scaffolded tools agent
actually invoke a tool against a real provider, and ships discoverable examples
plus a vendorable TypeScript wire-type generator. Purely additive public API +
docs; the Harbor Protocol holds at `0.1.0` (no method, error, event, or wire-type
change this release).

### Added

- **`Stack.RunOnce` — the production one-call runner.** *(embed)* One blocking
  call turns a goal + the `(tenant, user, session)` identity into the terminal
  answer envelope, replacing ~15–27 lines of hand-built `RunContext` / `RunSpec`
  ceremony per run. It is built on a shared `runctx.NewRunContext` factory that
  composes the same memory / skills / artifact / streaming projections the dev
  drivers use (parity-tested, never a third construction site), with
  `sdk/assemble` + `sdk/runctx` facade aliases and an N≥100 concurrent-reuse
  guarantee. (D-265.)
- **`WithStream` — first-class streaming on `RunOnce`.** *(embed)* One run
  option observes token / tool / step events as they happen, on the **same**
  blocking method — wired to the synchronous planner `OnChunk` + steering
  `OnToolDispatched` seam, so streamed chunks deterministically precede the
  final envelope. New public `StreamEvent` type. (D-266.)
- **`harbor token` — the no-IdP self-issuing on-ramp.** *(protocol)* A new CLI
  subcommand for an operator with no identity provider: `harbor token keygen`
  generates an asymmetric keypair (ES256 default / RS256 opt-in) and the
  matching public JWK Set (its `kid` is the RFC-7638 thumbprint); `harbor token
  mint` self-issues the JWTs `harbor serve` verifies. serve's verifier is
  unchanged — it trusts the key only because you point `identity.jwks_file` at
  the emitted JWK Set, identical to pointing at a real IdP, so serve still mints
  nothing. Least-privilege defaults (no scopes unless `--scopes`, a short ttl),
  `private.pem` written `0600`, and mandatory `--issuer` / `--audience` that
  must match `serve.yaml` or attach 401s. (D-264.)
- **Production-identity guide, skill, and worked OIDC client.** *(protocol)*
  `docs/site/protocol/production-identity-setup.md` documents both attach
  on-ramps (a real IdP and the `harbor token` self-issuing path), lifting the
  JWT claim shape — including the `iss` / `aud` exact-match contract serve
  hard-rejects — from the authoritative parser; the `configure-production-identity`
  operator skill operationalizes it; and
  `examples/protocol-clients/oidc-client-example/` is an SDK-free worked client
  that obtains a JWT via the OAuth2 client-credentials grant and attaches. (D-263.)
- **External-client TypeScript wire-type generator.** *(protocol)*
  `cmd/harbor-protocol-ts-types` reflects over the canonical Protocol surface
  and emits a vendorable, dependency-free TypeScript wire-type module for
  third-party clients (consumed by a worked `event-viewer-ts`), under its own
  `make protocol-ts-types-gen[-check]` targets — distinct from, and
  non-interfering with, the reserved Console generator and its lockstep gate.
  Partially retires the TypeScript-generation deferral for external clients. (D-269.)
- **Runnable `sdk` examples and an in-tree conformance worked example.** The
  first `Example_` functions under `sdk/` — the facade's first contact on
  pkg.go.dev — and a `go test`-compiled `conformance-fork` harness wiring a
  custom `Factory` + `RunSuite`.

### Changed

- **`harbor dev` is honest about `.go` edits.** *(cli)* A `.go` change now warns
  and guides a manual rebuild instead of driving an in-process reboot that
  reported `dev.hot_reload.completed{Success=true}` without recompiling the
  binary — a loud false success. Config / YAML reload still rebuilds in place.
  (D-268.)
- **The scaffolded tools agent actually invokes a tool.** *(cli)* When an agent
  declares tools, the scaffold's golden test now registers **and** dispatches a
  declared tool through the executor (not merely that `RegisterTools` is
  defined), closing a compile-only false-green. (D-267.)
- **Native tool-calling sanitizes catalog tool names.** *(runtime)* The React
  planner maps a tool name to the provider-safe form (`^[a-zA-Z0-9_-]{1,64}$`)
  for native tool-calling and resolves it back on dispatch, so Harbor's dotted
  convention — every built-in (`clock.now`) and the default scaffolded custom
  name (`inventory.check`) — no longer 400s against OpenAI-compatible providers.
  Transparent: the catalog name, the config, and the public API are unchanged.
  (D-270.)
- **A public surface that tracks the shipped reality.** The marketing surface
  now states the current canonical method count, the genuine config-reload
  capability, and the v1.8.0 release surfaces.

### Fixed

- **Dotted tool names against real providers.** Any built-in or default-named
  custom tool previously failed the first LLM call on an OpenAI-compatible
  provider with a `400`, because the catalog name was sent verbatim as the
  function name. The 107c/107d native-tool-calling tests and the scaffold gate
  all used a scripted LLM, which bypasses provider name-validation — surfaced by
  building a real scaffolded agent against a live provider. (Fixed by the
  native-tool-calling change above.)
- **The `embed-runonce` worked example runs, not just compiles.** It now
  declares a `ModelProfiles` entry, so an operator who copies it gets an answer
  instead of a runtime error. Surfaced by live verification.

### Internal

- A composing wave-end end-to-end test (`wave_v18_test.go`) exercises all three
  adopter paths together on real drivers under `-race`: embed `RunOnce` +
  `WithStream`, both Protocol on-ramps (the hermetic mock-OIDC issuer and
  `harbor token`) against a real `harbor serve` with a mismatched-`iss` 401,
  in-process MCP tool dispatch, identity propagation, and an N≥16 concurrency
  stress. A net-new integration test also proves a planner invoking an
  MCP-sourced tool through the executor.
- Three pre-existing, load-induced CI flakes were hardened (a `t.Parallel`
  goroutine-leak check, a durable-bus drain deadline, and a spawn/await settle
  window), and two long-committed root build artifacts were removed.
- Purely additive public API + docs: no Protocol method / error / event / type
  change, so `ProtocolVersion` and the committed wire-surface digest both hold.

## [1.7.0] — 2026-06-26

Protocol-edge hardening: capability negotiation, key-revocation safety, and
data-lifecycle erasure. This release lets a generic Protocol client learn which
conditionally-mounted surfaces a runtime serves at attach (instead of probing
for a `404`), bounds how long the JWKS validator will honor a possibly-revoked
key during an IdP outage, and adds the Protocol's first identity-scoped session
**erasure** verb with a real three-store cascade. The Harbor Protocol's wire
surface grows (two new capabilities + one method + one error code + wire types)
but stays semver `0.1.0` — every change is additive or corrective, and no prior
INTENDED behavior changes.

### Added

- **Agent-config capability negotiation.** `runtime.info` now advertises an
  `agent_config` capability — but only when the agent-config control plane is
  actually mounted — so a Protocol client (Console, IDE/TUI, SDK) gates the
  `agent_config.*` surfaces at attach instead of method-probing for a `501` /
  `unknown_method`. The advertisement is wired from the same source-of-truth as
  the mount in every boot path, so it can never claim a surface the runtime does
  not serve. (D-260.)
- **Session erasure — `sessions.delete`.** A new identity-scoped,
  **own-session-only** Protocol method that erases a session and cascades
  deletion of its scoped State, Memory, and Artifacts — Harbor's first canonical
  right-to-erasure surface, satisfiable through the wire contract instead of a
  privileged back-door. It refuses fail-loud with `session_running` (409) when
  the session has a running task (mirroring the GC never-reap-running
  invariant), emits a redacted, content-free `session.erased` audit event under
  the actor's observability scope (never the erased identity), and advertises a
  `session_lifecycle` capability when an eraser is wired. Backed by a new
  mandatory `StateStore.DeleteScope` cascade primitive with in-memory / SQLite /
  Postgres conformance parity. (D-262.)
- **JWKS max-stale / revocation ceiling.** A configurable `identity.jwks_max_stale`
  bound on the production JWKS validator: past the ceiling, with refreshes
  failing, it fails **closed** with a distinct `jwks_stale` rejection reason
  instead of serving a possibly-revoked signing key indefinitely during an
  identity-provider outage. The bound is identity-agnostic (it gates before a
  verified identity exists), has no opt-out knob, and defaults to a safe 1h. No
  new wire type — it reuses the existing `401` auth-rejected envelope. (D-261.)

### Fixed

- **Erasure is durable under concurrency.** A session erased while a task was
  still publishing lifecycle events could leave orphaned durable events readable
  via `state.history` (the events landed on the durable bus after the cascade's
  scope-delete). A new identity-scoped event **fence** — taken by the cascade
  before the sweep and lifted when the session id is reused — closes the race:
  a late publish is either swept or dropped, so a post-erasure `state.history`
  for the erased session is genuinely empty. Surfaced by live testing.

### Internal

- A composing wave-end end-to-end test exercises all three surfaces together
  with real drivers — both capabilities coexisting in one `runtime.info`
  projection (a self-consistent 7-capability universe), the full erasure
  lifecycle, the JWKS staleness fail-closed-then-recover path, cross-tenant
  isolation, and an N≥10 concurrency stress — under the race detector.
- Only the wire-surface digest in the committed manifest moves (it hashes the
  capability / method / error / type names); `ProtocolVersion` holds at `0.1.0`,
  consistent with the precedent that capabilities and additive methods are
  advertised, not version-gated.

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

[1.10.0]: https://github.com/hurtener/Harbor/releases/tag/v1.10.0
[1.0.0]: https://github.com/hurtener/Harbor/releases/tag/v1.0.0
