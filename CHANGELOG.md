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

## [1.28.6] — 2026-08-15

### Fixed

- Session auto-naming now exposes a naming-only `reasoning_mode` with the
  compatibility values `off` (the default) and `provider_default`. The latter
  omits provider reasoning controls without inheriting the selected model
  profile's planner default, so providers that reject reasoning controls can
  still produce titles while the 64-token bound remains unchanged (D-289).

## [1.28.5] — 2026-08-15

### Fixed

- Session auto-naming now explicitly disables private reasoning so a selected
  model profile cannot consume the fixed 64-token title allowance before
  emitting visible content. The digest, timeout, one-call shape, output bound,
  and persisted title clamp are unchanged (D-289).

## [1.28.4] — 2026-08-15

### Fixed

- Durable conversation turns now retain their canonical planner-derived
  reasoning, tool activity, and MCP App references when the stock Runtime's
  `task.spawned` event omits RunID and its later run-scoped events use the
  canonical `RunID=TaskID` binding. The exact task-bound relation is persisted
  and replay/restart remains byte-identical; arbitrary run ids and explicit
  binding mismatches remain fail-closed, and raw reasoning/tool content and App
  callback authority remain absent (D-425).
- Durable conversation-turn projection now acknowledges a strictly later
  contradictory terminal lifecycle event for an already-sealed legacy turn
  without rewriting the immutable first terminal row. Its session checkpoint
  advances so one incompatible historical tail cannot stop later turns;
  same-event conflicts and identity, task, and run binding mismatches remain
  fail-closed (D-425).

## [1.28.3] — 2026-08-14

### Fixed

- Durable conversation-turn projection now treats optional historical task
  failure messages containing invalid UTF-8 or control characters as
  unavailable instead of letting advisory display text stop the global
  projector. The lifecycle event remains canonical; required snapshot fields
  and identity, task, and run bindings remain fail-closed (D-425).

## [1.28.2] — 2026-08-14

### Fixed

- Durable conversation-turn projection no longer stalls behind incompatible
  optional failure metadata on a historical task record. Persisted lifecycle
  events remain canonical; disagreeing snapshot failure metadata and
  over-bound optional terminal messages are represented as unavailable while
  identity, task, and run binding conflicts remain fail-closed (D-425).

## [1.28.1] — 2026-08-14

### Fixed

- Compiled `sdk/server` hosts can now set a paired Harbor framework version and
  immutable commit on `server.Options.Framework`. `runtime.info` now reports
  them in additive `framework_version` / `framework_commit` fields while its
  existing `build_*` fields continue to identify the host application. Leaving
  the option empty omits the new fields; partial identities fail loud. Stock
  release binaries stamp the same pair from their release checkout and expose
  it through `harbor serve` / `harbor dev`.

## [1.28.0] — 2026-08-14

### Fixed

- Typed MCP error classification now survives the runloop's step recording and
  appears in the actual next ReAct prompt: the classified outcome, retry-policy
  result, bounded provider message, and retained bounded MCP result content are
  preserved, a generic `Step.Error` never masks the classified observation, raw
  arguments are redacted on every failed-replay shape, and canonical task/tool
  events agree with the planner observation on the terminal error (D-410,
  HA-54 planner-replay amendment).
- MCP Apps embedded/durable reopen now uses a fresh stateless, integrity-
  protected render admission sealed with the deployment-shared
  `tools.oauth_token_kek_env` KEK, minted only after the current
  authorization, generation, and resource checks (verified identity, signed
  effective-agent reach, retirement, erasure, current session/agent exposure,
  exact server, exact current `ui://` resource, paused/disabled state, and the
  deterministic replica-stable current provider/catalog generation). The
  reopen order is durable App reference → successful `mcp.apps.tool_context`
  replay → the explicit admission-requesting `ui://` read → iframe/AppBridge
  mount → same-server app-only callback through the existing wrapped
  invocation; a failed/unavailable/evicted/foreign replay mints no authority,
  ordinary and AppBridge-secondary resource reads never mint, and the fresh
  admission is distinct from, never aliases, and never coexists with the
  legacy live binding. The surface is strictly opt-in via
  `tools.mcp_app_render_admission.enabled` (default `false`) — sealer
  availability alone never enables it — and an enabled surface without a
  valid shared KEK fails readiness loud (D-412, HA-56 fresh render-admission
  amendment).

### Added

- Verified-caller two-phase `SKILL.md` package import:
  `agent_config.user.skills.import_validate` runs the one production
  importer/validator and performs ZERO writes of any kind — no proposal-ledger
  write — returning a bounded opaque versioned sealed proposal token;
  `import_commit` reauthenticates, re-resolves, and revalidates the token,
  forces `ScopeUser` + the effective agent, and durably installs the reviewed
  package plus membership in one conditional write, with durable idempotency
  state (a token-derived commit ledger) beginning only in the commit phase.
  Supporting files are copied into the durable package addressed by immutable
  `skillpkg://<PackageHash>/<path>` references behind one mandatory authorized
  resolver (D-422).
- `skill_create_draft`, an ordinary disabled-by-default tool that turns
  bounded intent plus optional feedback into a caller-scoped `SKILL.md` draft
  artifact through the governed authoring path's safety-wrapped LLM adapter.
  It has zero mutation authority; installation of a draft is exclusively the
  import validate/commit workflow (D-423).
- `sessions.list` / `sessions.inspect` gain an additive
  `projection: "lifecycle"` selector returning lifecycle metadata only with
  ZERO enrichment, using the closed counter-availability state
  `current | partial | not_requested | unavailable` (lifecycle counters are
  `not_requested`, never zero-as-not-computed); counter filters/sorts paired
  with it fail as a typed invalid request, and the full projection remains the
  default (D-424).
- `sessions.turns.list` / `sessions.turns.get`, a dedicated runtime-owned
  durable tail-paged conversation-turn projection with indexed keyset paging,
  per-component availability, inline Activity, and restart/erasure fences.
  Chat open is two reads (one lifecycle read + one turn page), then a
  page-before-subscribe snapshot-to-live handoff: the Console folds the
  durable page and establishes bounded running/paused membership before
  opening the EventSource with `live_resume_seq` as the initial `resume_seq`;
  the server replays events strictly newer than that snapshot, a browser
  reconnect `Last-Event-ID` takes precedence, one terminal event causes one
  `sessions.turns.get`, and a page retry rebuilds stale live membership from
  freshly read authoritative rows without duplicating bubbles or re-admitting
  a terminal row (D-425).
- Durable observability rollups behind `observability.query`: an indexed,
  rebuildable projection of best-effort aggregates over successfully
  persisted canonical events (never billing-exact), fixed UTC minute storage
  buckets with exactly the tenant/user/session/model dimensions, existing
  source-backed measures only, explicit freshness/completeness on every
  response, projection-backed session counters with an honest fallback, and
  the narrow D-296 amendment — a general-purpose TSDB and identity-labelled
  OTel metrics remain rejected (D-426).
- A boot-declared, resource-free operator skill baseline
  (`skills.boot_agent_packs`): a config-file-relative strict eager immutable
  loader running before readiness, exact `(tenant, boot_agent_id)` binding,
  strict merge with the active durable revision into one combined operator
  tier (`boot|revision|both` dedupe, 256-item cap), boot-owned mutation/remove
  guards, and a deterministic set hash in the run snapshot and the read-only
  composition preview. `harbor composition-preview` renders the preview from
  the CLI; production and devstack share the single loader path; headless
  `RunOnce` is explicitly unsupported and fails loud when the baseline is
  configured (D-427).

## [1.27.0] — 2026-08-13

### Added

- Typed MCP tool-result failure classification now preserves bounded provider
  results through retry policy, planner observations, Apps callbacks, and
  truthful lifecycle accounting without inferring authority from error text
  (D-410).
- Governed per-agent skill packs support policy-bound model proposals, exact
  reviewed-hash publication, crash-safe CAS recovery, and next-run-only
  activation while direct operator upserts remain LLM-free (D-411).
- MCP Apps now use per-server app-only callback catalogs and opaque,
  runtime-authored render bindings; app-only tools stay outside planner and
  generic tool surfaces while callbacks re-enter the normal identity, reach,
  OAuth, approval, audit, and redaction gates (D-412).
- Max-step exhaustion can pause a live run at a bounded step tranche and resume
  the same cumulative trajectory with a private, identity-bound token. A
  process restart reports typed `restart_unavailable` rather than masquerading
  as a new task (D-417/D-418).
- Virtual child profiles are frozen into task receipts and can only narrow the
  parent agent's model, instructions, limits, skills, tools, and MCP servers;
  they do not mint a new isolation principal (D-419).
- Child tasks can receive scoped artifact references and return schema-checked
  output. Artifact IDs never grant authority, and model-facing contracts carry
  references rather than bytes, URLs, credentials, or schemas (D-420).
- Runtime-owned task progress is available to ReAct and the SDK through a
  task-bound reporter, with durable snapshot/event convergence, ordered
  terminal behavior, redaction, and additive Protocol projections (D-421).

## [1.26.12] — 2026-08-13

### Added

- Optional signed `session_reach` JWT claim (D-409): a bearer may be pinned
  to a bounded set of session IDs it may select per request. Absent, D-171's
  dynamic per-request session selection is preserved exactly; present, the
  effective session — resolved from `X-Harbor-Session`, the SSE `?session=`
  projection, or the token's `session` default — must be a member or the
  request fails closed `403 scope_mismatch` before any handler side effect.
  Malformed, duplicate, blank, or over-limit claims reject authentication;
  an explicitly empty set grants no session. The claim is bearer authority
  only — never a storage filter or request-body member — and carrier-identity
  mode cannot manufacture it. `harbor token mint` gains `--session-reach`.

## [1.26.11] — 2026-08-05

### Fixed

- MCP Apps resource listings, resource reads, and app-initiated tool calls now
  retain the effective agent configuration that produced the App. The
  runtime-authored discovery reference carries an optional `agent_id`; the host
  echoes it, and Harbor
  re-runs signed reach before tenant-local resolution and before seating
  pair-owned credentials or reading current per-agent tool exposure. Omission
  still resolves to the configured default for v1.26.10 clients. This restores
  signed over-the-wire MCP Apps for named agents without weakening provider
  signature or exact-pair validation.

## [1.26.10] — 2026-08-05

### Fixed

- Signed OAuth MCP desired state now retains multiple independently signed
  provider capabilities for one agent instead of replacing the existing pair.
  The first pair keeps the v1.26.9 `signed_oauth_mcp_pair` storage and content
  hash shape; later pairs use the provider-keyed `signed_oauth_mcp_pairs`
  collection. Each pair keeps its own immutable operation, activation fence,
  replay, restart-recovery, and retirement identity. A pending pair candidate
  remains hidden until its fence commits, while a definitive concurrent CAS
  loser releases only its unpublished fence so the same signed authority can
  retry against the new active revision. Removal targets the exact requested
  provider and preserves siblings; omitting `provider_name` remains compatible
  only when the selected revision contains exactly one pair.

## [1.26.9] — 2026-08-04

### Added

- Signed MCP capability registration now supports receiver-style, per-user API
  key injection without enabling the development-only `allow_wire_injection`
  gate. The signed connection binds the complete non-secret injection mapping;
  its provider must equal the pair's private provider, its downstream host is
  still derived from the canonical connection URL and checked against the
  boot-declared broker ceiling, and header/meta targets must remain covered by
  the shared credential redactor. The provider pulls the acting user's
  credential per call, is never published into the shared provider set, and is
  closed with the connection across removal, replay, and restart reconciliation.

## [1.26.8] — 2026-08-04

### Fixed

- An exact replay of a published signed OAuth MCP capability now restores its
  process-local provider, connection, and tools before returning success. This
  lets an authenticated control-plane convergence pass recover a restarted
  runtime without waiting for an unrelated user run, while retaining the same
  committed-fence, immutable-pair, active-revision, operation-generation, and
  publisher-epoch checks as run-start recovery. Changed, foreign, removed, or
  incompletely fenced pairs remain fail-closed. No Protocol wire or runtime
  configuration surface changed.

## [1.26.7] — 2026-08-04

### Fixed

- Signed OAuth MCP restart reconciliation now reattaches an immutable pair after
  an unrelated agent-config edit carried that pair into a newer active sibling
  revision. The committed activation fence remains anchored to its original
  candidate and authorizes the current sibling only when both revisions carry
  the exact same pair and durable operation receipt. Changed pairs, foreign
  operations, stale active pointers, missing candidates, and non-committed
  fences remain fail-closed. No Protocol wire or runtime configuration surface
  changed.

## [1.26.6] — 2026-08-03

### Added

- Signed OAuth MCP capability registration now accepts the same trusted
  artifact-egress declaration as a generic HTTP MCP connection:
  `artifact_byte_eligible` plus `artifact_params` bounded to 32 methods, 8
  parameters per method, 128 bytes per name, and 8 KiB canonical JSON. Both
  fields are covered by the signed immutable binding, replay fingerprint, persisted
  revision, applied echo, and restart reconciliation. Mappings are rejected
  atomically when unsigned, tampered, over-bound, or inconsistent with the
  server's discovered string input schema. The closed descriptor still accepts
  no headers, arbitrary hosts, credential sinks, or secrets. This is an
  additive Protocol field change; `ProtocolVersion` remains `0.1.0`.
  A schema-rejected committed candidate is durably compensated and terminally
  non-replayable across restart. Existing signed pairs with both fields omitted
  preserve their prior replay fingerprint across upgrade and restart.

## [1.26.5] — 2026-08-03

### Fixed

- Identity-scoped OAuth MCP connections no longer open the optional
  connection-level standalone SSE stream, which could retain a short-lived
  preparation bearer beyond its lifetime. Streamable JSON-RPC calls continue
  resolving fresh tenant/user credentials per invocation. Per-entry-only OAuth
  now injects those credentials on calls and refuses redirects when no single
  provider can authorize the hop. Unbound and static-header MCP connections
  retain standalone SSE. No Protocol wire or runtime configuration surface
  changed.

## [1.26.4] — 2026-08-03

### Fixed

- Run-start connection reconciliation now preserves an active signed OAuth MCP
  capability instead of treating it as an undeclared generic MCP connection.
  Generic stale-connection cleanup remains intact, while recovery of a missing
  signed connection stays exclusively with the dedicated signed lifecycle.
  No Protocol wire or runtime configuration surface changed.

## [1.26.3] — 2026-08-03

### Fixed

- Signed OAuth MCP capability dispatch now keeps immutable registrar identity
  for the broker actor assertion, removal, and audit while exchanging and
  caching for the verified live run subject. Reconciliation reattaches through
  the frozen owner receipt for a later same-tenant subject. An authenticated durable
  `control.start` admission receipt now gates the exact effective agent across
  restart and child spawns, so denied, tampered, or bare SDK-created tasks
  cannot use a cached bearer. No Protocol wire or runtime configuration
  surface changed.

## [1.26.2] — 2026-08-03

### Fixed

- `sessions.list` now applies lifecycle-only filters, ordering, and cursor
  pagination before counter enrichment. A `last_activity_desc` request with
  `limit=50` enriches only its returned rows instead of scanning counters for
  the full matching catalog; counter-dependent filters and `cost_desc` retain
  their exact results with bounded (eight-worker) enrichment concurrency. No
  Protocol wire change.

## [1.26.1] — 2026-08-03

This patch restores authority-enabled serving composition for signed OAuth MCP
capability recovery. The Protocol version remains `0.1.0`; there is no wire
surface change.

### Fixed

- Signed OAuth MCP capability authority is now wired through the shared serving
  composition. A valid boot-declared authority no longer fails before recovery
  because the run-loop projection detacher was selected in place of the MCP
  attacher's exact signed-pair teardown seam. This restores authority-enabled
  boot for `harbor serve`, dev/Console compositions, and the production server
  facade without changing the Protocol version or making any credential,
  verifier, or sink field writable.

## [1.26.0] — 2026-08-02

This release strengthens the authority and lifecycle boundaries for
agent-addressed Runtime work: bearer reach is explicit and bounded, OAuth MCP
capabilities have a durable production registration path, and agent-config
retirement is terminal and recoverable. The Protocol version remains `0.1.0`;
new methods, fields, and error codes are additive, but the deployment and
client actions below are required for a safe upgrade.

### Before you deploy

1. **Mint bounded signed bearer reach for every agent-addressed data-plane
   request.** `control.start`, the session and user `agent_config` methods,
   and an agent-named `tools.describe` now require a valid `agent_reach` claim.
   Add `--agent-reach <id>[,<id>...]` when using `harbor token`; missing or
   empty reach is denied and malformed reach fails authentication. The default
   `harbor dev` bearer reaches only its boot agent. Update any issuer or client
   that previously relied on an unbounded bearer before deploying.
2. **Plan the session-personal skill cutover.** New session-personal skill
   mutations default to `session_skill_cutover_pending` (HTTP 409) while a
   tenant is in `dual_read`; eligible legacy skills remain readable. An
   operator must declare the tenant's bounded static cutover, drain older
   writers, and allow Harbor's resumable verification to finish at
   `state_only`. Do not retry the refusal by writing a legacy shared
   SkillStore body.
3. **Treat retirement outcomes as part of normal client handling.** The
   additive `agent_config.retire` operation makes an agent unavailable for new
   runs while retaining immutable revision history and resumable cleanup. A
   retired agent now returns the additive `agent_retired` (HTTP 409) outcome;
   a conflicting retirement operation returns `agent_retirement_conflict`.
   Clients that distinguish error codes should handle both before enabling
   retirement automation.

### Security

- Stable-JTI signed OAuth MCP registration now durably admits expiry before
  compensation and can resume the same exact operation with a later freshly
  verified envelope. Recovery preserves the original registrar and receipt,
  restores only the frozen prior/absent authority, and cannot reopen published,
  removal, removed, or widened bindings.

- Signed OAuth MCP capability registration now recovers only exact durable
  signed pairs at boot and run start. Recovery verifies the pair descriptor,
  owner, operation receipt, and activation fence before it can attach or
  withdraw a connection; real SQLite and two-runtime Postgres restart paths
  are covered.

- Agent-addressed data-plane calls now require signed, bounded
  `agent_reach` authority. This applies to `control.start`, the session and
  user `agent_config` data-plane methods, and `tools.describe` only when it
  names an `agent_id`. Add `--agent-reach <id>[,<id>...]` when minting a
  bearer with `harbor token`; the default `harbor dev` bearer reaches only its
  boot agent. Missing or empty reach denies these calls, while malformed reach
  rejects authentication. Omitted `tools.describe.agent_id` remains the
  boot-effective projection.

- Session-personal skill mutation now defaults to a deliberate read-only
  `dual_read` deployment posture. Existing eligible legacy session skills stay
  readable, but new/update/delete requests return
  `session_skill_cutover_pending` (HTTP 409) until an operator declares the
  tenant's bounded static cutover, drains every older writer, and Harbor
  completes a fresh durable verification pass to `state_only`. This is not a
  transparent fallback and clients must not retry by writing a legacy shared
  SkillStore body. The declaration requires a configured SkillStore; malformed,
  duplicate, over-bound, or unwired declarations fail boot loud.

### Fixed

- The completed, independently reviewed authority and lifecycle checkpoint
  composes
  signed-agent reach, conditional configuration writes, signed-capability
  publisher takeover and removal, session erasure, inflight-run retirement
  drain, cleanup restart, reasoning durability, and mixed-identity cancellation
  isolation. Isolated-schema Postgres races cover the conflicting authority,
  lifecycle, overlay, and personal-record transitions. This is checkpoint
  evidence; tag, release assets, checksums, and provenance publication are
  recorded separately after they complete.

- Bifrost reasoning now preserves the selected response's exact observed
  bytes. Any non-nil raw reasoning delta, including an empty one, makes the raw
  stream authoritative; details-only responses coalesce fragments by stable
  block identity without trimming or rewriting whitespace. Only choice index 0
  contributes content, reasoning, tool calls, or callbacks. The same bytes are
  verified through live callbacks, completed responses, planner decisions,
  task trajectory, durable restart history, and Console history rendering.
  This is an internal fidelity correction with no
  Protocol type, method, event, error, manifest, or version change.

## [1.25.0] — 2026-08-01

One release-closure slice: the original **prompt-composition surface**, the
transaction and declared-tool corrections found by adversarial review, and the
external-oracle/reliability gates needed to trust the candidate. No
Protocol wire-version change (`ProtocolVersion` stays `0.1.0`) and no
migration. Every wire *addition* is an optional field or an additive error
code; **two changes are nonetheless breaking for a deployed client**, and they
are the first thing to read below.

**The defect this wave closes is not the one that was reported.** The upstream
ask was "Harbor offers no additive path for contributing prompt content." It
does — two of them, both shipped and both documented. What was true is narrower
and worse: **the safe paths were not reachable over the Protocol, so the surface
steered consumers toward the unsafe one.** `planner.MemoryBlocks` carries
recalled content behind an anti-prompt-injection preamble and appeared nowhere
in `internal/protocol/`; `planner.LLMOverrides.ExtraInstructions` renders
additively into a trusted position and was absent from `RunOverrides`. A
consumer needing either reached for `system_prompt_override`, which replaces the
whole base+user spine, silently suppresses the operator's durable user layer,
and seats caller content in the **trusted** base position.

### Before you deploy

**Two changes break clients that worked against 1.24.** Both are in *Action
required* at the end of this entry with the full reasoning; this is the short
form, and neither is optional reading.

1. **The control transport now decodes strictly, so an unknown request member
   is a `400` instead of a silent drop** (D-374). If your client folds extra
   keys into request bodies, it will start failing. **Seven request types
   across eleven methods declare no `identity` field** — the five
   `artifacts.*`, `SearchRequest` (shared by the five `search.*`), and
   `governance.set_posture` — and an `identity` object sent to any of them is
   now refused. This is not hypothetical: **Harbor's own Console and its own
   smoke corpus both broke on it**, and both are fixed in this release.
2. **`GovernancePostureRequest.tenant_id` and `LLMPostureRequest.tenant_id`
   are removed from the Protocol** (D-374). The Runtime never read either
   field. A request carrying `tenant_id` to `governance.posture` or
   `llm.posture` used to return a `200` with the **wrong tenant's** posture;
   it now returns a `400`. **Drop the field and set `identity.tenant`
   instead** — the cross-tenant read has always lived there.

**Three further changes need a decision from you, not a code change** — the
conditional-write guarantee is exact within one Runtime process and **absent
across processes**; `extra_instructions` remains available to ordinary users
for same-session personalization but is now escaped and structurally separated
from operator guidance; and **every model-visible tool name longer than 44 bytes is now
rewritten** (D-377), so anything that string-matches tool names outside a live
run — stored transcripts, eval fixtures, dashboards — sees new strings.

**Upgrade recommended for all deployments.** This release also strengthens the
containment of untrusted memory and turn text in the prompt (see *Security*
below).

### Added — a caller can hand a run its own recalled memory, in the UNTRUSTED tier

- **`start` takes an optional `caller_memory`.** It is a `json.RawMessage`
  (`StartRequest.CallerMemory`, `caller_memory,omitempty`) composed into the
  run's **External** memory tier — the one the ReAct planner renders as a
  system message behind a five-line anti-prompt-injection preamble, in the
  documented most-stable-first order that preserves KV-cache prefixes. This is
  the slot that already existed for exactly this content; it is now reachable
  from a Protocol client. An omitted field produces a byte-identical wire shape
  and byte-identical run behaviour, golden-compared rather than asserted.

- **The caller names no key.** It supplies a value; the runtime writes it under
  one fixed runtime-owned map key, `caller_supplied`
  (`runctx.CallerSuppliedKey`). There is therefore no reserved-key deny-list to
  maintain and no future collision surface: runtime producers may add sibling
  keys forever and can never collide with a caller, and a caller can never
  shadow, rename or displace a runtime key. Runtime semantic recall
  (`recalled_turns`) and caller-supplied content COMPOSE in the same tier
  rather than competing for it.

- **The `Conversation` tier is NOT caller-writable.** The runtime writes that
  slot unconditionally whenever the memory patch is non-empty, so a caller
  writing it would be two producers on one slot with silent last-writer-wins.
  The reported need is served by External; widening buys nothing and costs the
  collision.

- **The bound sits at the Protocol edge, before a task exists.** A payload over
  **32 KiB** is refused `invalid_request` **and no task is created** — the
  refusal precedes `Spawn`. (It does not precede session creation: the `start`
  handler ensures the session row before it validates `caller_memory`. Only
  the named-agent check is ordered ahead of the ensurer.) The refusal text names
  `caller_memory`, which is load-bearing: the control transport caps the whole
  body at 64 KiB with the same error code, so a field cap that ever rose to
  meet the envelope cap would become unreachable dead code while every
  status-code test kept passing. The ordering is pinned mechanically. An
  explicit `"caller_memory": null` is refused rather than read as absent, and a
  malformed document is refused rather than dropped.

  **That cap is a resource bound and a wire-size guard, not a security
  boundary** (D-375). It exists because nothing downstream re-checks these
  bytes — the LLM-edge leak guard byte-exempts system-role text — so without it
  an oversized document reaches the token-budget guard and fails the whole run
  late instead of costing one cheap refusal. Nothing may be inferred from it
  about how much content a caller can put in front of the model: the same
  principal can send more through the uncapped `query` (landing in the
  *unframed* conversation position) and through the claim-free
  `agent_config.session.set_user_prompt` (1 MiB body, landing *inside* the
  system prompt). What contains this payload is positional, never its size.

  It is still deliberately **not** an operator knob, on the operational
  argument rather than a security one: a byte dial buys nothing an operator can
  act on, and a configurable value is a foot-gun against the cap-ordering
  invariant above. If a legitimate caller is ever refused, the answer is an
  additive optional config key defaulting to the constant and validated
  strictly below the envelope cap.

- **Negotiate `caller_memory` before you rely on it, and unknown members are
  now refused rather than dropped** (D-374). A runtime that predates the field
  would have discarded it and answered **200** — the run then proceeds without
  the memory the caller believes it supplied, and nothing says so. Two changes
  close that: `runtime.info.capabilities` now advertises `caller_memory` (the
  ninth Protocol capability, and the first to advertise an additive optional
  request *field* rather than a method cluster — a client reads its **absence**
  to identify an older runtime), and the **control transport now decodes
  strictly**, so a member no wire type declares comes back `400
  {"code": "invalid_request"}` with the member **named**. Every other Harbor
  Protocol handler already decoded strictly; the control transport was the
  outlier. No deprecation window applies — `Deprecation` can only describe a
  method, error code, wire field or capability being retired, and unknown-member
  tolerance is none of those. **Action for clients:** strip members Harbor does
  not declare. In particular the `artifacts.*` methods scope by `scope`, and a
  stray `identity` object beside it is now a 400 rather than a silent drop.

  **Seven request types, reached by eleven methods, declare no `identity`
  field** — the five `artifacts.*` types (which scope by `scope`), the single
  `SearchRequest` shared by all five `search.*` methods (which scopes by
  `filter`), and `GovernanceSetPostureRequest`. **Audit all eleven methods if
  your client folds identity into request bodies**, as Harbor's own Console and
  smoke corpus both did:

  - The **Console** sent it on all five `artifacts.*` and on `search.query` —
    its transport folds the triple *below* the typed client surface, where
    caller-side typing cannot see it. Those call sites now suppress the fold,
    and a lockstep check (`npm run lint`) enforces the rule in both directions:
    a call site suppresses the fold **iff** its request type has no `identity`.
  - The **smoke corpus** sent it to four `search.*` methods. A second guard
    (`scripts/check-smoke-body-identity.sh`, run by `make drift-audit`) covers
    that corpus.

  Clients built on the Go Protocol client are unaffected: they marshal typed
  wire structs, which have no `identity` field to populate.

- **Removed: `GovernancePostureRequest.tenant_id` and
  `LLMPostureRequest.tenant_id` — a Protocol field the Runtime never read.**
  Both request types were orphans: the posture family is decoded into the
  shared `RuntimeInfoRequest` envelope, so `tenant_id` was discarded and **an
  admin naming another tenant silently received its OWN tenant's posture with a
  200** — wrong data on an admin audit path, with no signal. The strict decode
  turned that into a loud 400, which is how it surfaced.

  **The cross-tenant read is not lost — it never lived here.** The selector is
  `identity.tenant`: a body tenant differing from the verified tenant requires
  the `admin` / `console:fleet` claim and emits `governance.posture_read_admin`.
  That gate is implemented, audited and tested in both directions. The field was
  a second, never-implemented spelling of it, so it was removed rather than
  implemented — two selectors for one concept would have to answer "what if they
  disagree?" for no benefit.

  **If you send `tenant_id` to these methods, drop it and set
  `identity.tenant` instead.** Both types are gone from the Protocol reference
  and the generated TS wire types; a new `TestManifest_NoOrphanWireTypes` gate
  fails the build if a wire type is ever again published without a method that
  decodes it.

- **`memory.caller_block_admitted` — a new canonical event recording the FACT
  of an admission, never the content.** It carries `bytes`, `tier` and `key`
  and no fragment of the payload. `bytes` is the exact pre-redaction wire
  length, even when the persisted redacted representation has a different
  size. It fires at admission, which precedes
  planning, so it lands whether or not the run subsequently succeeds. A Console
  that cannot tell caller-asserted memory from runtime-retrieved memory can
  audit neither.

- **A documentation correction that changes where the bound had to live.**
  `internal/runtime/runctx/memory_fetch.go` claimed the LLM-edge context-leak
  guard was "the authoritative backstop" for an oversized memory tier. **It
  never has been.** `findContextLeak` applies its byte check to text only when
  the message is `RoleTool`, and memory tiers render as `RoleSystem`. The real
  backstop is the token-budget guard, which fires after the whole prompt is
  built and fails the run late. The comment is corrected, and that correction
  is why this bound sits at the Protocol edge rather than being left to a
  downstream guard that does not cover it. Both halves are pinned by greps, so
  a future author who re-adds the claim — or who changes the exemption the
  argument rests on — is forced to re-derive rather than silently invalidate it.

- **The honest residual, stated rather than implied.** This makes it possible
  to put untrusted content in front of a model over the wire. That is the
  point; the mitigation is **positional** (the untrusted tier and its framing),
  not filtering. **An operator who pipes third-party content through
  `caller_memory` without redacting it has a data-leakage path no prompt
  wrapper closes.** The 32 KiB cap is the only thing that re-checks these
  *bytes* before the token-budget guard — it is not the only path by which a
  caller's content reaches the model, and it bounds one block rather than the
  aggregate: a caller block plus a large `retrieval_top_k` plus a long
  trajectory can still exhaust a context window, and there is no per-tenant rate
  or volume accounting on admitted caller memory — spend is metered at the LLM
  edge, admission is not.

### Added — `extra_instructions` on run overrides, contained as normal-user personalization

- **`runs.set_overrides` takes an optional `extra_instructions`.** The
  product capability is not admin-only: a verified non-admin user can
  personalize the next run in their own session. What was missing is reach —
  `RunOverrides` exposed `session_id`,
  `reasoning_effort`, `temperature`, `max_tokens`, `system_prompt_override` and
  `model`, and no additive field, so the only producer was the admin-gated
  tenant-override record. The wire addition needs no new error code or Protocol
  version move.

- **The run-level value is structurally separate from the tenant value and can
  NEVER clear or impersonate it.** The admin-owned tenant contribution remains
  in `<additional_guidance>`; the session contribution lands in the escaped,
  runtime-labelled `<user_personalization>` tier. Prompt ordering is not an
  authorization boundary: tool exposure, identity, governance, and policy
  checks remain authoritative. The personalization tier survives a
  `system_prompt_override` without being promoted into the replacement spine.

- **There is no run-level clear, and an empty value is accepted rather than
  refused.** An empty or whitespace-only run-level value contributes nothing
  and returns the tenant value untouched, so a run with no session contribution
  resolves byte-identically to before. That is what stops "clear" being
  reachable by the back door of "set it to empty".

- **The two contributions remain distinguishable.** Runtime-owned section
  framing makes their provenance explicit while the durable per-agent surface
  below provides name-addressed attribution for admin-owned blocks.

- **The event carries a FLAG, never the text.**
  `events.RunOverridesSetPayload` gains `SetExtraInstructions bool`. The value
  is caller-supplied free text and never rides the bus.

- **Binding non-goal — this is NOT the home for recalled memory.** The field is
  for user-authored preferences that personalize a response. Recalled or
  retrieved content belongs in `start.caller_memory`, with its stronger
  read-only external-memory framing.

### Added — ordered, named prompt blocks on the agent-config payload

- **A new agent-config payload section, `extra_system_blocks`**, carrying an
  ORDERED list of `{name, body}` blocks, written by one admin verb
  (`agent_config.set_extra_system_blocks`) as a **whole-section desired-state
  replace**. The prompt surface was two flat `*string`s (`Base`, `User`), so N
  independent capability sources that each want to contribute one block
  collapsed into one opaque string and removing one contributor's text meant
  re-deriving the whole composition from prose. Blocks exist so each
  contributor can find and replace exactly its own.

- **It lands on the durable payload, not on the per-run bundle** — which is
  where the upstream ask put it. A per-run bundle is reconstructed by whoever
  assembles the next request, so a per-run block list inherits the same "who
  reconstructs the rest" problem it is meant to solve. On the config payload
  the list is durable state the registry owns, readable by name through
  `agent_config.get`, and mutable by a name-addressed read-modify-write that
  the expected-revision token below makes safe against a concurrent second
  contributor. That token is why this ships **one** section-replace verb rather
  than per-item upsert and delete verbs.

- **Order is the declared slice order, and normalization MUST NOT sort it.**
  This is a deliberate asymmetry with two sibling sections: skill names are
  sorted and de-duplicated, and OAuth providers are sorted by name, both
  because a re-ordering of a SET must not perturb the content hash. For blocks
  a re-ordering DOES change the rendered prompt, so it MUST change the hash,
  mint a real revision, and appear in the diff — which gains a `Reordered` flag
  its sorted siblings have no analogue for. **The carrier is a slice, never a
  map** — map iteration order is not a composition order — and nothing on the
  write → normalize → hash → project → render path sorts it. (Local `seen` maps
  do exist on that path, for duplicate-name detection; they are membership sets
  over an already-ordered slice and determine nothing about order.)

- **Admitted block bodies are byte-faithful.** Blank detection may inspect a
  trimmed view, but normalization, hashing, persistence, projection, and prompt
  rendering preserve every byte of a nonblank body, including surrounding
  whitespace. This matters for operator-authored structured prompt fragments.

- **Rendered VERBATIM, in declared order, each behind a plain-text `[name]`
  label**, into the same `<additional_guidance>` position — after the binary's
  baked operator guidance and before the additive extra-instructions. Blocks
  survive a session `system_prompt_override`, for the same structural reason
  `extra_instructions` does. Names are unique and charset-restricted
  (`^[A-Za-z0-9._-]{1,64}$`); a duplicate is refused with `invalid_request`
  naming **both** offending positions rather than silently de-duplicated,
  because uniqueness is what makes remove-by-name well defined.

- **Trust is argued from the WRITE DOOR, not assumed.** The section has exactly
  one *dedicated* write verb, and every door that reaches it —
  `set_extra_system_blocks`, plus `set_revision` and `rollback`, which write or
  republish the whole payload — sits in the admin method set: the same tier that
  writes `PromptLayers.Base`, which is already verbatim and strictly more
  powerful. Escaping a block while leaving `Base` unescaped would defend
  against a writer who can already replace the entire prompt, and would mangle
  an operator's angle brackets. **The obligation that creates is stated rather
  than engineered away: a capability that wants to surface user-authored or
  model-authored text MUST NOT put it in a block** — it uses the untrusted
  memory tier above, or `PromptLayers.User`, which is escaped precisely because
  it has a lower-tier write path. Two tested guards make a future reopening
  loud.

- **The `[name]` label is for a human reading a transcript. It is not a
  security boundary and must not be described as one.** To the model, two
  blocks from two capabilities are one contiguous run of trusted guidance. A
  `<block name=…>` prompt tag was rejected: the attribution the ask needs is a
  data-model property, and minting a tag from config data would make the
  prompt's structural taxonomy a function of caller input.

- **Absent ⇒ byte-identical.** A stored revision written before this release
  unmarshals to nil, normalizes out of the canonical form entirely, and does
  not perturb its content hash; the renderer returns nothing, so the composed
  system prompt is byte-equal to before. Pinned by a byte-equality test rather
  than by inspection. No `harbor.yaml` key, no new error code.

- **No cap on block count or body size, deliberately.** The two real bounds are
  the agent-config wire door's 1 MiB body cap
  (`maxAgentConfigBodyBytes`) and the LLM edge's token-budget guard. The
  byte-leak check does **not** cover this content — verified rather than
  assumed, because the obvious assumption is wrong: system-role text is exempt
  from the leak detector. A per-section cap would be operator policy with no
  consumer asking for it, and `PromptLayers.Base` — unbounded on the same
  surface today — would make it pure asymmetry.

### Added — an optional precondition on every agent-config write

- **`expected_content_hash`, one optional string on all seventeen
  spine-writing request types.** Every write onto the durable agent-config
  revision spine was unconditional last-writer-wins with no conflict detection
  anywhere on the path: two writers composing into one agent's config silently
  reverted each other and **both were told `200`**. Present and matching ⇒ the
  write proceeds. Present and not matching, or present with no active revision
  ⇒ refused and **nothing is persisted** — no revision record, no
  active-pointer move, no `agent.config.revised` event. Absent ⇒ byte-for-byte
  the previous behaviour on every door.

  The count is asserted, not documented. The section shipped at sixteen doors
  and the ordered-blocks verb above made it seventeen — caught by the
  exact-count guard, which is what it is for. A future eighteenth spine writer
  added without the field fails three ways, including a behavioural table that
  drives each door with a stale token (the only one of the three that catches a
  door which declares the field and then drops it on the floor).

- **A content hash, not a revision id.** `agent_config.rollback` repoints the
  active revision **without necessarily changing the content**, so rolling back
  to the content a writer already read leaves the content identical and the
  revision id different. A revision-id token would refuse that write — turning
  "the operator restored exactly what you read" into a false positive on
  Harbor's own recovery path. Accepting *either* token was rejected: two fields
  answering one question means four combinations to specify and a client that
  can silently pick the weaker one.

- **`revision_conflict` — a new error code, mapped to HTTP 409.** Nothing
  existing fits: the body was well-formed (so not `invalid_request`) and the
  server did not fault (so not `runtime_error`, which would additionally make
  the conflict unbranchable — a client could not tell "re-read and retry" from
  a server bug). No `data` field is added to the error envelope; a conflicted
  client re-reads `agent_config.get`, which already returns both `revision_id`
  and `content_hash`.

- **The evaluation order is load-bearing.** The precondition runs **before**
  the existing idempotent-re-set short-circuit. The other order would convert a
  stale token into a `200` whenever the caller's payload happens to equal the
  current content — a success that misleads the caller into believing its base
  was still valid. A transposition is the one mutation that leaves every
  grep-for-presence guard green, so it is pinned twice.

- **The token is a PRECONDITION, never an AUTHORITY.** It is compared strictly
  after the identity and scope gates, it can only ever cause a write to be
  refused, and no value of it widens what a caller may write. A valid token
  with an incomplete identity triple is refused by the identity gate; a valid
  token with no admin scope is refused by the scope gate; neither is reported
  as `revision_conflict`.

- **The first write is protectable too — `expected_content_hash: "-"`**
  (`agentcfg.ExpectNoActiveRevision`, D-370). The composition protocol had no
  expressible form at its own base case: on an agent with no config the only
  token a first contributor could send was the empty one, which means
  unconditional, so two contributors composing a fresh agent silently reverted
  each other and both were told `200`. The reserved sentinel succeeds **only**
  while the agent has no active revision and is refused `revision_conflict` the
  moment one exists, on both the content-write door and the pointer-move door.
  A real token is 64 lowercase hex characters, so `"-"` can never collide with
  one — asserted by a conformance row rather than argued. Three conformance
  rows carry it (one of which reproduces the lost update and then shows it
  closed), and the conditional-write smoke's exact-count guards moved 4 → 7
  with them.

- **What the token still does not do, named rather than discovered later.** An
  unconditional writer can still clobber a conditional one — the token
  constrains the writer that supplies it, not the ones that do not.

### Fixed — the preflight inert-smoke gate, and the classifier that measured it

- **`scripts/smoke/inert-baseline.txt` drains to zero, and thirteen of its
  twenty-four entries were the measuring instrument's own false positives.**
  The v1.24 release switched on a gate: a smoke belonging to a **shipped** surface
  that reports `OK: 0` and `FAIL: 0` asserted nothing. Twenty-four scripts
  violated it and were parked as declared debt. Measurement showed the count
  itself was wrong — the shipped/not-shipped classifier failed two independent
  ways, and **thirteen of the twenty-four did not belong to shipped surfaces.** Its
  row regex could see 233 of 339 master-plan rows and treated the other 106 as
  shipped; its status vocabulary named two of the eight not-shipped words in
  use and defaulted the rest to shipped. **The correction can only relax, never
  tighten**, and that is verified rather than argued: both classifiers were run
  over all 360 plan entries — 345 unchanged, 15 relaxed, 0 tightened.

- **The eleven genuinely-inert smokes got real assertions**, and the helper
  they use closes a trap. `go test -run NoSuchTest ./pkg` prints "no tests to
  run" and exits **zero**, so a smoke that names a test and asserts only the
  exit code reports OK forever after that test is renamed or deleted. The new
  `assert_go_tests_pass` helper greps a `--- PASS:` line **per name**, so a
  rename is a FAIL. Every one of the eleven repairs was mutation-verified, and
  the captured output shows `go test` exiting 0 in each case — the exit-code
  guard would have been a false OK on all eight of the pure-Go repairs.

- **Three residual holes in the gate itself, each closed rather than deferred.**
  A smoke that exits 0 without printing a summary was invisible to **both**
  gates and is now a FAIL naming the missing call. A baseline line naming a
  deleted script rotted forever and is now asserted as a property of the file.
  An unparseable master-plan row was indistinguishable from a missing one and
  is now reported by name — scoped to the classification call site rather than
  to every smoke, because reporting the twenty-one rowless smokes on every run
  would be noise an operator learns to scroll past, which is the failure mode
  this exists to close rather than reproduce.

- **`scripts/drift-audit.sh` no longer writes its markdownlint output to a
  fixed `/tmp` path.** `make preflight` runs the audit internally, so two
  concurrent audits clobbered each other's diagnostic and an operator could
  read someone else's violations. The verdict was never wrong — it comes from
  the exit code — but the file the failure message *named* was.

- **The drift-audit's own guards are now mutation-verified** (D-376).
  `scripts/drift-audit.sh` is the mechanical instrument behind the
  contributor workflow and half the rejection-on-sight list, and **nothing
  verified the instrument** — its guards had been mutation-verified by hand,
  with the results recorded in code comments that no automated check re-ran. A
  guard that cannot fire is indistinguishable from a corpus with no violations.
  The mutation harness now builds a throwaway corpus per guard, applies the
  defect that guard names, runs the **real** audit against it, and asserts
  **that guard's own** FAIL line — never merely the exit code, which would
  report "caught" when a *different* guard fired. 18 guard units, 18 covered,
  22 mutations, and a census that fails when a new guard ships with no case.

  It found two live defects on its first run, both fixed here: **`brief NN`
  reference resolution could not fail** (an unmatched glob under `nullglob`
  degenerated to a bare `ls`, exit 0 — every brief citation in every implementation plan
  had been unverified), and **a smoke with no `PREFLIGHT_REQUIRES` header
  aborted the whole audit** under `set -euo pipefail`, silently skipping six
  later guards.

### Security — untrusted memory and turn text are structurally contained

- **Content inside an untrusted prompt section cannot escape that section.**
  The JSON-encoded memory tiers (`read_only_external_memory`,
  `read_only_conversation_memory`), the skills-context block and the
  session-artifacts manifest are embedded inside tag-framed wrappers the model
  reads **positionally**. Harbor now guarantees structurally that no byte of
  the content inside those wrappers can be read as the wrapper's own framing:
  the JSON tiers encode `<`, `>` and `&` to their `\u003c` / `\u003e` /
  `\u0026` JSON escapes (loss-free, JSON-native, and deterministic, so the
  compact-JSON KV-cache discipline is unaffected), and the plain-text sections
  run through the same
  neutralisation. Untrusted content therefore stays untrusted and can never
  open a trusted position — `<additional_guidance>`, the operator base layer,
  or the admin-written named blocks.

- **This covers content nobody had to mark as untrusted.** It is not only about
  the new `caller_memory` field: `recent_turns[].user` and
  `recalled_turns[].user` are **ordinary user turn text** that the runtime
  itself recalls into these tiers, and an uploaded artifact's filename, MIME
  type and provenance string land in the session-artifacts manifest. Every one
  of those is contained by the same mechanism.

- **Trusted positions still render VERBATIM, deliberately.** `PromptLayers.Base`
  and the admin-written `extra_system_blocks` are authored by principals who can
  already replace the entire prompt; neutralising them would defend against
  nobody and would mangle an operator's angle brackets. The asymmetry is the
  design, and it is tested in both directions.

- **Pinned end-to-end** — over the real wire, through the real
  `runctx.ComposeCallerMemory` → planner render path — for every wrapper in the
  prompt, plus the inverse assertion that trusted positions are unaffected.
  Recorded as D-386, with the property named in the encoder's godoc as
  load-bearing rather than incidental, so a future author cannot switch it off
  as a formatting preference.

  **Upgrading is recommended for every deployment.** The containment is
  structural and needs no configuration.

### Changed — model-visible tool names are bounded, and a dropped declaration is announced

- **A tool's model-visible name is now bounded at 44 bytes and shortened
  TAIL-FIRST** (D-377). Catalog keys are `<sourceID>_<tool>` and the catalog is
  flat and process-global, so a source id must be globally unique — operators
  reach for long, high-entropy ids to get that, and the id is then paid on
  **every turn, twice per tool** (the `req.Tools[]` declaration and the
  `<available_tools>` prompt section). Over-budget names now render as
  `<retained tail>_<8-hex digest of the full sanitized name>`. The catalog key
  does not move and no isolation property changes. Each run constructs one
  immutable projection table from the declarations it actually exposed;
  reverse resolution accepts only a declared winner and never treats model
  input as a raw catalog key. The projection is a **pure** function of the
  declared catalog snapshot, which is why a short
  per-source alias was rejected (an alias assigned from catalog composition
  shifts when the catalog grows mid-run, and the catalog grows mid-run by
  design).

  **Measured effect** on a 30-tool server, declared-name bytes per turn across
  both surfaces: a 6-byte key is unchanged (in-budget names pass through
  byte-for-byte), a 36-byte key drops 3124 → 2640 (−15%), a 64-byte key drops
  3840 → 2640 (−31%). Characters are measured exactly; **the token figure is an
  estimate and is labelled as one** — no tokenizer is vendored, and dividing the
  character deltas brackets the long-key saving at roughly 300–480 tokens per
  turn. Do not quote that range as measured.

  **Operator-visible consequence:** anything that string-matches a model-visible
  tool name outside a live run — stored transcripts, eval fixtures, dashboards
  — sees new strings for names that were over 44 bytes. Live runs are
  unaffected: every surface re-derives the name from the catalog key each turn.

- **`<available_tools>` was showing the model a name it could not call**, and
  that is fixed in the same change. The prompt section rendered the RAW catalog
  key while `req.Tools[]` declared the sanitized form (`clock.now` listed,
  `clock_now` declared). Both surfaces now go through one transform.

- **Builtin discovery, lookup, and declarative dispatch use that same
  projection.** `tool_search`, `tool_get`, and `declarative_action` no longer
  resolve a model-authored name as a raw catalog key. Reserved planner controls
  claim the namespace first, collision losers are neither advertised nor
  callable, and authorization scopes are applied before the projection is
  built. An unknown or raw alias fails before dispatch; a rejected serialized
  batch leaves no partial pending-call state. This closes #654 without renaming
  the internal catalog.

- **`planner.tool_declaration_collision` — a new canonical event** (D-378).
  Two catalog tools that collapse onto one model-visible name are ambiguous to
  provider-side dispatch, so one declaration is dropped. The drop was correct;
  its failure mode was not — it was a bare `continue` with no error, no event
  and no log. Combined with the old head truncation that meant, measured on a
  64-byte source id with a 30-tool server, **30 catalog tools in, 1 declaration
  out**, while `<available_tools>` still listed all 30. The drop is kept and
  made observable: a typed `SafePayload` names the run identity, the colliding
  model-visible name, the tool that KEPT the declaration and the tool that was
  DROPPED, because the remedy is operator-side. An **event and not an error**,
  because failing every run of an agent with two ambiguously-named tools would
  escalate a config problem into a total outage.

  A benign re-discovery of an already-loaded tool stays quiet — `seen` maps the
  model-visible name to the real catalog name that claimed it, so the drop can
  tell "same tool" from "lost surface". Residual collisions remain reachable by
  construction (`clock.now` and `clock/now` both sanitize to `clock_now` at any
  length), which is why the diagnostic is load-bearing rather than theatre.

### Fixed — agent-config mutations preserve one coherent visible state

- **Runtime-added MCP connections now prepare, persist, then activate**
  (D-390, closing #653). Dial, authentication, handshake, and discovery happen
  privately; no provider, catalog entry, live registration, or `online` state
  is published before desired state is durable. Activation holds a private,
  reversible registry reservation while direct reads continue reaching the
  prior provider, then uses the catalog swap as the dispatch linearization
  point. A collision restores the exact prior registry and catalog; displaced
  resources close only after successful publication.

  A write error no longer triggers unconditional detach. An exact active-pointer
  read distinguishes confirmed landing, confirmed absence, and an unreadable
  answer. Confirmed landing activates while returning the storage error loud;
  absence closes only unpublished resources; unreadable state preserves the
  existing live connection and reports the ambiguity. An exactly landed
  auth-required write returns `auth_required` with the reread revision and the
  producer-owned pause token rather than rejecting a durable continuation over
  a lost acknowledgement. Inline OAuth is likewise private until activation
  and carries an exact install receipt for rollback.

  Auth-required preparation is restart-safe through the production callback,
  not only through a fake continuation. A mandatory typed `FlowStore` seals the
  pending PKCE verifier, client material, identity, expiry, and pause token over
  the existing StateStore/KEK seam. Reconstructed providers claim the
  high-entropy state exactly once, validate its identity and provider owner,
  and resume the same durable pause without a second exchange. Retryable
  failures release the claim; a spent code whose token cannot be persisted
  records a durable terminal-rejection stage and rejects the pause through
  bounded uncancelled cleanup. A transient rejection failure retains a retry
  path without re-exchanging the spent code. The access token, refresh token,
  and encrypted exact flow marker persist in one atomic StateStore record.
  Before pause resume or destructive cleanup, a second sealed record keyed by
  the exact OAuth state preserves the original identity, source, subject, pause,
  expected decision, and retry horizon. Callback retry therefore remains exact
  even after a later flow replaces the current credential or a cleanup delete
  lands without its acknowledgement. Partial cleanup reclaims residual PKCE and
  claim records; later flows prune only expired tombstones for their exact
  identity, with the tombstone deleted last. Already-resumed cleanup verifies
  the exact terminal decision. Upstream OAuth response bodies and redirect
  error text never enter callback logs, HTTP detail, pause records, or canonical
  events: seven standard denial codes are accepted and every other value becomes
  static `authorization_denied`. Mixed resume decisions share one first-winner
  gate, so reject/timeout cannot overtake accepted continuation work.

- **Conditional skill mutations coordinate their body and revision effects**
  (D-388). The expected-content check runs under the owner lock before all four
  admin/user SkillStore doors. A later revision failure restores the exact prior
  body or deletes only a body created by that call; a stale expectation changes
  no body, revision, pointer, or success event.

- **Ambiguous revision saves clean only proven orphans** (D-388, superseding
  the affected D-373/D-380 behavior). When a revision-record save reports an
  error, an exact scoped point-read may delete only a byte-identical,
  unreferenced candidate. A missing record needs no cleanup; a mismatched or
  unreadable answer is retained and reported. Compensation uses a bounded
  uncancelled context and preserves both the original and cleanup errors. This
  is coordinated compensation, not cross-store ACID or cross-process CAS.

### Fixed — caller memory is redacted at rest, like its siblings

- **`caller_memory` was persisted verbatim while `description` and `query` were
  redacted** (D-370). Proven against the real `patterns` redactor: the durable
  driver's whole-record marshal — the bytes that land in the StateStore on disk
  — contained the raw secret. The wire godoc documented the prompt-injection
  residual and said nothing about at-rest persistence, so an operator who saw
  `query` redacted would reasonably infer the same of `caller_memory`. **The
  inconsistency is the part that could not survive.**

- **Redaction was chosen on measured evidence, not assumption.** Driving the
  canonical rule set over representative payloads first: objects, arrays,
  numbers, booleans, `null` and nesting all survive structurally intact, and
  only secret-shaped keys and inline `Bearer …` / `Basic …` values are
  replaced. It is not a text redactor on this path — `redactRawJSON` decodes,
  walks the value and re-encodes, which is the path the engine already takes
  for a structured tool result.

- **Two consequences handled rather than absorbed.** The idempotency compare
  against the stored bytes would now raise a false conflict on an honest retry,
  so it is removed — the content hash already folds the **pre-redaction** bytes
  and is strictly stronger. And a malformed document is now refused **loudly**
  (`tasks.ErrInvalidRequest`, naming the field) before the marshal, rather than
  being re-quoted into a valid-but-unusable row whose failure surfaces at
  whatever reads it later.

- **The residual is restated honestly rather than dropped.** The audit redactor
  is a **pattern** redactor, not a sanitiser: it does not detect PII, does not
  detect a credential that looks like prose, and cannot make hostile text safe.
  An operator piping third-party content through `caller_memory` still has a
  leakage path neither a prompt wrapper nor a pattern redactor closes.

### Fixed — two isolation and availability defects on already-shipped surfaces

- **The spawn idempotency index is keyed by the full identity triple** (D-371).
  It was keyed on `(SessionID, IdempotencyKey)` — `tenant_id` and `user_id`
  appeared in none of the six sites, including the hydration rebuild that
  decides whether the shape survives a restart. **Stated at its true size:** this
  was not a handle disclosure, because the divergence compare rejects a foreign
  entry; the reachable damage is a **cross-tenant denial plus an existence
  oracle**, and session ids are high-entropy so it is not reachable by guessing.
  It is fixed anyway, because an isolation boundary is held by the *shape* of
  the key and not by the entropy of one component.

  **No migration.** The index is derived state that is never written to any
  store — the engine rebuilds it at `Hydrate` from each persisted task's own
  `Identity` + `IdempotencyKey`, and `Task.Identity` already carries
  `TenantID`. Existing rows are byte-identical; an upgrade rebuilds the
  narrower key from what is already on disk. `RunID` deliberately does **not**
  join the key: a run-scoped key would defeat dedup across exactly the retries
  it exists to collapse.

- **The pending-override slot map is bounded** (D-372). `runs.set_overrides`
  records a one-shot next-message override into an in-process map keyed by the
  identity triple; a session that records one and never sends a message left its
  slot behind. The package godoc called that "dropped" — nothing dropped it.
  With no TTL, no eviction and no size cap, any authenticated caller reached
  unbounded growth by recording an override under a fresh session id in a loop.
  The map now holds at most 4096 entries and **evicts the oldest-recorded slot**
  at capacity — evict rather than refuse, because a capacity refusal lets one
  caller deny the surface to every other tenant, which is strictly worse.

  Eviction is **loud, once per eviction, with no first-in-window suppression**:
  below the bound an eviction is impossible, so a line is never routine. There
  is deliberately **no TTL** — the slot already has a lifetime ("until the next
  message") and a second time-based axis would be a second mechanism answering
  one question. A non-positive bound is *ignored* rather than honoured, so
  there is no way to configure the bound away. The same append-only shape in the
  agent-config session-overlay store is fixed in the same change, refcounted
  rather than capped, because patching one and leaving its twin is how a defect
  class survives its own fix.

### Verified — a settled decision gains the instrument it never had

- **HA-47 (keying the live MCP registry by `(owner, name)`) is REFUSED, and
  D-301 is reaffirmed unchanged** (D-379). The re-key is **inert for its own
  motivating case**: the token tax is levied on the CATALOG key, not the
  registry key, and with the registry conflict removed by construction two
  owners attaching `github` still fail at `catalog.Register("github_add")`. It
  would also move a clean pre-dial refusal to **after** the transport is live.

  What lands instead is the missing instrument. D-301's namespace half — "a
  collision fails loud", the bounded guarantee traded for the process-global
  catalog — was pinned nowhere, so anyone re-attempting this had to rebuild the
  probe to learn what the decision already knew. It is now pinned as
  *behaviour*: the refusal is loud, typed, and **pre-dial**; it leaves the first
  owner's catalog and registration untouched; a distinct-name control arm proves
  the harness measures the name; and the catalog is pinned as the independent
  second gate.

- **A latent credential-plane hazard is recorded rather than quietly fixed.**
  The tool token cache Kind carries no agent component under `ScopeUser`, so two
  agents in one tenant sharing a user and a connection name would share one
  bearer cache row. It is unreachable today **only because** the same-name
  refusal above prevents the precondition. Any future work that relaxes
  cross-owner connection-name uniqueness must address this first; it is filed as
  issue #638 rather than fixed in a test-only change.

### Fixed — release oracles and scoped convergence

- **Canonical event names now have a handwritten external oracle.** The oracle
  is bidirectional over the entire canonical registry; it is not generated from
  the emitter constants it checks. Adding, removing, or renaming an event
  requires an explicit reviewable oracle edit. The current registry contains
  143 canonical names.

- **A shipped smoke that exits zero without a canonical summary is red in a
  black-box fixture.** The mutation harness derives its declared
  cases and guard signatures from one registry, so deleting a whole registered
  case leaves either an unexecuted declaration or an unclaimed audit guard.

- **Docs PR validation no longer competes in the global Pages concurrency
  group.** Validation supersedes only an older run for the same workflow/ref;
  the main-only deploy job alone owns the global `pages` singleton.

- **Agent-config revision enumeration is identity-scoped inside every
  StateStore driver.** The mandatory `ListKindForIdentity` interface method is
  implemented by in-memory, SQLite, and Postgres drivers; `ListRevisions` no
  longer performs a maintenance-wide scan followed by Go-side filtering.

- **A stale erasure ledger converges before it is discarded.** The older
  lifecycle's completion is published before checkpoint deletion;
  publish/delete failures retain retryable state and never touch the current
  lifecycle. Retried delivery is best-effort deduplicated from retained
  history, with duplicates preferred to lost compliance records when that
  oracle cannot verify.

### Fixed — deterministic reliability gates

- Tool conformance failure decisions are per invocation rather than derived
  from a scheduler-shared counter; OAuth refresh callers synchronize behind one
  observable single flight; parallel-cancel start signals are buffered; and
  cancelled Protocol-client calls cannot race a successful server response.

- The OAuth choreography now distinguishes the tool OAuth pause from the
  planner's run pause and waits until both tokens exist before completing the
  callback. Callback completion resumes the tool pause after durable token
  persistence; one steering `RESUME` then releases the distinct run pause. This
  removes the fast-callback race without sleeps or retries.

- The authenticated PTY workflow is unquarantined. It sends real Kitty CSI-u
  function-key codepoints, establishes persisted or canonical state before a
  full visual repaint, and keeps a newer action modal open when an older
  same-scope inspection completes. It also sends one failed-followup retry
  command; a duplicate while dispatch is in flight cannot start again or mutate
  queued intent. Failure diagnostics report child liveness and capture SIGQUIT
  goroutine stacks. The frozen correction passed 10 local race repetitions, a
  20-run two-CPU Linux calibration, and then 100/100 race-instrumented workflows
  in the same Go 1.26 Linux profile (`ok`, 392.617s).

### Changed — dependencies

- **`github.com/maximhq/bifrost/core` `v1.5.21` → `v1.7.4`**, tracking the
  upstream `SecretVar` rename. No Harbor-facing API change.

### Action required

The first two items **break a client that worked against 1.24**. The rest ask
for a decision, not a code change.

- **BREAKING — strip request members Harbor does not declare.** The control
  transport now decodes strictly (D-374): a member no wire type declares is
  refused `400 {"code": "invalid_request"}` with the member **named** in the
  message. Every other Harbor Protocol handler already decoded this way at this
  same `ProtocolVersion 0.1.0`; the control transport was the outlier, and the
  asymmetry was an omission rather than a policy. **No deprecation window
  applies** — `Deprecation` can only describe a method, error code, wire field
  or capability being *retired*, and unknown-member tolerance is none of those;
  a window would also be a grace period on a data-loss bug.

  **What to do today:** audit any place your client adds keys to a request body
  below its typed surface. The concrete trap is identity folding — **seven
  request types reached by eleven methods declare no `identity` field** (the
  five `artifacts.*`, `SearchRequest` shared by the five `search.*`, and
  `GovernanceSetPostureRequest`). Harbor's own Console folded the triple below
  its typed client on six call sites, and Harbor's own smoke corpus sent it to
  four more; both are fixed here and both are now held by a lockstep guard.
  Clients built on the **Go** Protocol client are unaffected — they marshal
  typed wire structs with no `identity` field to populate.

- **BREAKING — drop `tenant_id` from `governance.posture` and `llm.posture`,
  and set `identity.tenant` instead.** `GovernancePostureRequest` and
  `LLMPostureRequest` are removed (D-374). Nothing ever decoded them: the whole
  posture family decodes into the shared `RuntimeInfoRequest` envelope, so
  `tenant_id` was discarded and **an admin naming another tenant silently
  received its OWN tenant's posture with a 200** — wrong data on an admin audit
  path, with no signal. The strict decode turned that into a loud 400, which is
  how it surfaced.

  **The cross-tenant read is not lost — it never lived here.** The selector is
  `identity.tenant`: a body tenant differing from the verified tenant requires
  the `admin` / `console:fleet` claim and emits `governance.posture_read_admin`.
  That gate is implemented, audited and tested in both directions. The removed
  field was a second, never-implemented spelling of it. A new
  `TestManifest_NoOrphanWireTypes` gate now fails the build if a wire type is
  ever again published without a method that decodes it.

- **The conditional-write guarantee is exact within ONE Runtime process and
  ABSENT across processes. Do not read this release as "Harbor now prevents
  lost updates."** The atomicity comes from one thing: the agent-config
  service's 256-way striped per-owner write lock, taken by every door as its
  first act after identity validation and held across its whole
  read-modify-write. It does **not** come from the store, and the store says so
  in its own interface godoc — it stores and returns a version integer and
  enforces no compare-and-set. **Two Runtime processes sharing one Postgres or
  SQLite StateStore can still silently lose an update, with both writers told
  they succeeded.**

  That residual is written into the field's godoc, the error code's godoc, the
  generated Protocol reference row and the operator skill, and it is **pinned
  by a test that asserts it as absent**: two independently-constructed
  registries over one real file-backed SQLite store, one writer suspended
  between its precondition read and its save, the other completing an entire
  conditional write under the same token — and the lost update still occurs. If
  a future change ever does make the write cross-process safe, that test fails
  and all four texts must be corrected. The fix that would close it is named
  rather than hinted: a conditional-write primitive on the `StateStore`
  interface across the in-memory / SQLite / Postgres triad, with conformance
  rows. That is its own release.

  **What to do today:** if you run more than one Runtime process against a
  shared store, treat `expected_content_hash` as a defence against concurrent
  writers *within* a process and not as a distributed lock. Single-process
  deployments get the full guarantee.

- **`extra_instructions` remains a normal-user personalization capability.**
  Any verified caller of `runs.set_overrides` can personalize its own session;
  no admin-only gate was added. The runtime now escapes that prose inside a
  dedicated `<user_personalization>` section and keeps the admin-owned tenant
  guidance in `<additional_guidance>`. A user cannot clear the tenant tier or
  forge its structural boundary. **What to do today:** treat the field as
  preferences, not recalled data; use `caller_memory` for retrieved content.

- **Model-visible tool names longer than 44 bytes are rewritten** (D-377). A
  live run is unaffected — every surface re-derives the declared name from the
  catalog key each turn, and the transform is a pure function, so a name cannot
  shift mid-run. What changes is any string comparison made **outside** a live
  run: stored transcripts, eval fixtures that assert on a tool name, dashboards
  that group by it. **What to do today:** if you pin tool names anywhere
  durable, re-capture them after upgrading. Names already within 44 bytes pass
  through byte-for-byte and need nothing.

- **Everything else in this release is additive and needs no action.** Every
  remaining wire change is an optional field or an additive error code; an
  omitted field reproduces the previous behaviour byte-for-byte on every
  affected path, and each of those claims is pinned by a golden or
  byte-equality test rather than asserted. The at-rest redaction of
  `caller_memory`, the idempotency-key rekey and the override-slot bound all
  land without a migration and without a config change.

## [1.24.0] — 2026-07-30

Seven phases: artifact read-path byte correctness, a heavy-content threshold
split by purpose, the MCP arm of pass-by-reference routing, caller-named agent
selection, run-start connection re-establishment, `_meta` path nesting for MCP
annotations, and a search user-axis isolation gate. No Protocol wire-version
change (`ProtocolVersion` stays `0.1.0`) and no migration anywhere in the
release.

**Upgrading — four behaviour changes an operator can experience without
touching a config file.** Two of them narrow what a caller sees and can read as
a regression; both are deliberate. See *Action required* at the end of this
entry, and read the `heavy_output_threshold_bytes` and search entries below in
full before upgrading.

### Added — an MCP tool can be handed artifact BYTES without them entering the model's context

- **A mapped parameter on an MCP tool call is resolved from the artifact store
  and sent as base64, so a large document reaches a remote tool without ever
  transiting the model's context.** When the model authors an artifact id into a
  parameter the operator has mapped, the runtime resolves the artifact itself
  and places the bytes into the outbound tool-call body (RFC 4648 §4 standard
  base64, padded). No address is published, no grant is minted, and no reusable
  handle exists. This is the MCP counterpart of the in-process artifact-reference
  parameter that shipped in 1.23.0.

  The raw argument JSON is never rewritten, so the substituted bytes reach none
  of the seven places a tool call is otherwise recorded: the observation, the
  LLM observation, the serialised trajectory, event payloads, audit payloads and
  logs, the per-invocation content hash, and the durable MCP-App tool-context
  record.

- **Two additive, optional connection fields configure it** — on
  `tools.mcp_servers[]` in `harbor.yaml` and on the
  `agent_config.add_mcp_connection` / `agent_config.set_revision` descriptor:

  - `artifact_byte_eligible` (bool, **default false**) — the containment
    boundary. With it unset, declaring `artifact_params` is **refused at the
    door**, not silently ignored.
  - `artifact_params` (`map[string][]string`) — this server's **bare**
    server-side tool names mapped to the parameter names that carry artifact
    bytes, in the same shape `tool_policies` and `tool_oauth_providers` use.

  Both are **http-only and refused on stdio** (explicit, or an `auto` transport
  carrying only a `command`). Each mapped parameter is validated **at attach
  against the server's own discovered `inputSchema`** — it must be declared
  there and declared string-typed. An absent or non-string parameter fails the
  attach loudly rather than failing the next call silently. The check is
  point-in-time: a server that mutates its schema without emitting
  `tools/list_changed` drifts out from under a validated mapping and surfaces as
  a server-side argument-validation error on the wire.

- **`tools.mcp_artifact_egress_max_bytes` (default 8388608 — 8 MiB)** bounds one
  substituted parameter. It is deliberately independent of
  `heavy_output_threshold_bytes`: substituted bytes never enter a model's
  context, so the budget is network and memory, not tokens. An oversize artifact
  **fails loudly naming the artifact, its size and the ceiling — it is never
  truncated**, because a partial document delivered to a remote ingester is a
  corruption rather than a bounded read. A negative value is refused at config
  load rather than read as unbounded. Restart-required.

- **`mcp.artifact_egressed` — a new canonical event recording the FACT of a
  substitution, never the bytes.** It carries the identity quadruple, the server
  id, the tool name, and one content-free record per substituted parameter
  (artifact id, parameter name, byte count, `sha256:` digest). It is
  **fail-closed, not best-effort**: a publish failure, a missing bus or a missing
  identity aborts the call *before* any wire request is issued. A substitution
  that could not be recorded does not happen.

  **Read the security posture, not just the feature.** Byte-eligibility is
  wire-configurable and admin-writable — deliberately *not* the fail-closed boot
  gate its siblings `tools.allow_wire_oauth_descriptor` and
  `tools.allow_wire_injection` take, because those gate where a *credential* is
  sent and this gates where a *user's own content* is sent. Stated plainly: a
  tenant admin can attach a server they control, map a parameter, declare it
  byte-eligible, and receive a user's artifact bytes on the next run that names
  an id. Artifacts are stored as authored, unredacted, so a byte-eligible
  connection can move a secret. That sits inside the co-tenant-admin trust
  boundary Harbor already accepts and documents, whose remedy is one runtime per
  tenant. What does *not* widen is the reachable artifact **set** — resolution
  runs under the dispatching run's own `(tenant, user, session)`, the same
  seated resolver the in-process arm uses.

  Known limit: an MCP App's tool callback (`mcp.apps.call_tool`) cannot use a
  byte-mapped parameter. There is no run, so no resolver is seated, and the call
  fails loudly rather than degrading.

### Added — a caller can name the agent a run should use

- **The `start` method takes an `agent_id`.** It was an explicit argument on roughly
  thirty request types — the whole `agent_config.*` family, the user-layer and
  skills families, `tools.describe` — and absent from exactly one surface: the
  one that STARTS a run. So a caller could write a config revision under a new
  agent id, read it back, diff it, roll it back, and no run would ever use it.
  Orphan configs were the normal outcome of driving the control plane as
  documented.

  The field is additive and optional (`agent_id`, omitted by default) on
  `StartRequest` and on the internal spawn path, and the resolved value is
  persisted on the **task** and surfaced as `agent_id` on the task row.

- **A named agent is accepted on two cheap checks and is NEVER silently
  substituted.** It is accepted if it equals the runtime's configured default
  agent id, or if a config revision exists for `(caller's tenant, agent id)`.
  Anything else is refused at the Protocol edge **before** the session ensurer
  and before the task is spawned, so no session row and no task materialise. A
  caller that named A, silently got B, and was told it succeeded is the exact
  defect this closes.

  An unresolvable id answers `invalid_request`; a resolver *error* answers
  `runtime_error` — never a fall-through to the default. The refusal text is one
  constant that names neither the rejected id nor the reason, so the edge is not
  a cross-tenant existence oracle. On a Runtime built without an agent resolver,
  a non-empty `agent_id` is refused rather than ignored; an omitted one behaves
  exactly as before.

- **The credential plane is untouched.** The RFC 8693 acting principal and the
  MCP `_meta.agent_id` provenance stamp continue to derive from the boot value.
  A caller-named agent changes prompts, tools, skills and the other
  configuration projections only. Two carriers, different provenance,
  deliberately not conflated.

- **An absent `agent_id` on a task row means DEFAULTED, not "unknown".** Every
  historical row predates the field and every such run bound the runtime's
  default; render absence as "the runtime default". The session row deliberately
  carries no agent — a session may run several — so the agent is persisted on
  the task only.

  **ACTION for anyone using idempotency keys.** The named agent is folded into a
  task's content identity, so **reusing an idempotency key while naming a
  different agent is now a loud conflict** rather than a silent adoption of the
  original task's agent. A spawn that names no agent hashes byte-identically to
  one made before the field existed.

### Added — a declared MCP connection is re-established at run start

- **Connection reconciliation attaches as well as detaches.** It was
  detach-only, and nothing on the run path attached, so a connection an active
  config revision DECLARED but the live registry did not carry never came back —
  not after a restart, not after a rollback that re-declared one. The only way
  back was an admin re-running `agent_config.add_mcp_connection` by hand.

  At every run start, for the reconciling `(tenant, agent)` owner, a declared
  connection the registry is missing is now re-attached through the **same**
  attach lifecycle the admin verb drives — never a second implementation — so
  every gate is re-applied against current boot policy, including the
  fail-closed stdio command allowlist and the fail-closed credential-injection
  opt-in. Detach still runs first; the attach pass re-reads the owner view
  afterwards.

- **The claim is bounded, and the bound matters.** It is not "connections
  survive restarts". It is: *a declared connection whose descriptor is
  self-sufficient is re-established at run start.* A connection whose live
  transport depended on operator-supplied static `Headers` is **not**
  restart-survivable — headers are secret and are never persisted — so the
  re-dial receives a `401` and is reported as `transport_failed`.

  The credential plane is untouched structurally: the attach path has no token
  step, so this leg mints, holds, refreshes and exchanges nothing, never
  initiates a consent flow, and **never parks a run**. An auth shortfall
  surfaces where it always did — on the first tool call, through the existing
  typed auth-required pause cycle.

- **Two additive canonical events**, `mcp.connection.reattached` and
  `mcp.connection.reattach_failed`. The failure event carries a stable class
  from a closed set of six: `transport_failed` (the only retryable one),
  `stdio_not_allowed`, `injection_disabled`, `oauth_binding`, `owner_conflict`,
  `ambiguous_server_id`. A reconcile is distinguishable from an admin add by the
  author's run id — an admin add carries none.

  Failure is loud, non-fatal and bounded: a refused or unreachable third party
  never fails the run and never aborts the sweep. Retries back off per
  `(owner, name)`, the first failure emits, suppressed attempts are counted and
  the count rides the next event, and an operator edit to the descriptor resets
  the window. The window is in-memory, so a crash-looping deployment re-dials a
  dead server once per boot.

### Changed — the heavy-content threshold rises to 128 KiB, and the config key's meaning NARROWS

**Read this one even if you have never set the key.** Two things move
independently and both are visible.

- **`artifacts.heavy_output_threshold_bytes` now defaults to 131072 (128 KiB),
  up from 32768 (32 KiB).** An ordinary 40 KiB JSON tool result now reaches the
  planner inline instead of being routed to the artifact store and read back on a
  second turn — which was the point. The consequence to size: leaving the key
  unset now permits roughly 32k tokens of a 200k context window for a single
  32–128 KiB tool result. On a small-context model that is a real regression
  risk. Setting the key explicitly to `32768` restores the previous behaviour of
  this arm.

- **ACTION: the key no longer governs Console-facing Protocol replies.** It now
  governs **the LLM-context arm only** — the tool dispatcher's promote-to-stub
  boundary, the LLM-edge leak guard and auto-materialization, the
  trajectory-compaction payload budget, and the `tools.content_stats`
  `heavy_threshold_bytes` figure that reports it.

  Six Protocol-visible sites now select inline-versus-reference at a **pinned
  32 KiB** that deliberately does not track the key: `pause.list`, `memory.get`
  and `memory.list`, the flow catalog, and the
  `mcp.servers.read_resource` / `mcp.apps.call_tool` / `mcp.apps.tool_context`
  reads. Two further arms that were never Protocol replies also pin their own
  32 KiB and stop tracking the key: the search preview's ref-versus-inline
  selection and the terminal-UI scrollback fold.

  **Who this bites.** On a default configuration the pins are a byte-for-byte
  no-op — 32 KiB before, 32 KiB after. The narrowing is only observable to an
  operator who had set an **explicit non-default** value: their Console-facing
  bounds now decouple from it and revert to 32 KiB. There is deliberately **no
  compatibility flag**; the reopening path, if it is ever wanted, is an additive
  `artifacts.console_inline_payload_bytes` key rather than re-coupling two
  different questions to one number. The rationale is that those payloads are
  rendered by a browser and never placed in a prompt, and that the arm a reply
  selects is Protocol-visible: raising this key should widen what a planner sees
  inline and leave every browser-facing reply unchanged.

  `docs/CONFIG.md` documents the split in full.

- **`tools.content_stats.heavy_count` is not comparable across this upgrade.**
  It counts historical offload events against the *current* threshold, so the
  number drops discontinuously after upgrade without anything having changed.

- **One residual, named rather than buried.** `tool_calls[].arguments` has no
  offloader anywhere in the tree, so the leak detector guarding that class moved
  from 32 KiB to 128 KiB — a straight 4× loosening of the only guard on it. The
  alternative, a detector held below the offload boundary, would kill runs on
  content the runtime had just declared acceptable and that no producer could
  have avoided.

- `examples/harbor.yaml`, `examples/dev.yaml` and `examples/serve.yaml` no
  longer pin `32768`.

### Changed — a dotted `meta_annotations` key now NESTS in the MCP `_meta` map

- **`tools.mcp_servers[].meta_annotations` keys are annotation PATHS.** A key
  with no `.` still sets a top-level `_meta` key. A DOTTED key now nests:
  `vendor.account_id: acct-42` lands at `_meta.vendor.account_id` instead of as
  the literal flat key `_meta["vendor.account_id"]`.

  **Why.** Two mechanisms write into the same outbound `_meta` map on the same
  call and they disagreed about what a dot meant. `injection.meta_key` has
  always been a path (it walks and creates intermediate maps);
  `meta_annotations` merged flat. So a receiver-style MCP server reading one
  nested namespace could be handed a per-user credential but had no route at all
  to its non-secret companion value — it could not ride `injection` (one mapping
  per connection, and a non-credential leaf is correctly refused by the
  redaction-coverage predicate) and it could not ride `headers` (a documented
  secret field, never persisted). One namespace now has one meaning regardless
  of which mechanism wrote into it, through the same helper.

  **ACTION: read this if you declared a dotted annotation key.** No config
  rewrite is required — the nested shape is what a dotted key was already asking
  for — but the connection sends a DIFFERENT `_meta` on its next call after
  upgrade, with no error and no config edit. If a downstream server matches on
  the literal flat key, either rename the key to remove the dot or update the
  server. Flat keys are completely unaffected, and no shipped Harbor example or
  config declared a dotted key.

- **Three declarations that were accepted before are now refused, loudly, at
  boot and at every runtime door** (`agent_config.add_mcp_connection`,
  `agent_config.set_revision`, and attach). Each error names the offending key
  and the rule:

  - A key whose WHOLE value **or any dot-segment** is reserved. `tenant.foo` is
    now refused; it previously passed because only the whole key was checked.
    (The whole-key check is retained, not replaced — it is the only arm that
    sees the spec-reserved `io.modelcontextprotocol/` namespace, whose segments
    carry no prefix.)
  - A path that COLLIDES with another declared path on the same connection —
    equal to it, or a prefix of it — including the `injection.meta_key` path.
    `{vendor: x, vendor.id: y}` is refused, and so is a flat `vendor` annotation
    alongside `injection.meta_key: vendor.api_key`. That second case previously
    discarded the operator's annotation SILENTLY at merge time. A connection
    persisted before this rule now fails its calls loudly rather than picking a
    non-deterministic winner.
  - A path deeper than 16 segments, or one containing an empty segment (`a..b`).

  **ACTION: a `harbor.yaml` that validated before can now fail to boot.** All
  three rules apply at boot, not only on the wire. Run `harbor validate` against
  your config before upgrading.

- **The 16-segment `_meta` path cap now applies at every door.** It previously
  existed only on the `agent_config.add_mcp_connection` wire path, so a
  boot-declared `injection.meta_key` deeper than the cap was accepted where the
  identical wire-declared one was refused. Both now consult one constant. (No
  migration: no shipped config nests past two segments.)

- **Audit note — over-redaction, not a leak.** The redactor replaces a matched
  key's WHOLE value without recursing, and its receiver-injection rule matches
  the last key segment. A flat `token.env` annotation was not redacted (last
  segment `env`); nested, the node key is `token`, which matches, so the entire
  `token` subtree collapses to `***` — non-secret siblings included. Redaction
  coverage is preserved; the cost is audit readability under a
  credential-named namespace. No audit rule changed.

### Fixed — search results are scoped to the caller's own user

- **A search that does not name a user now returns YOUR rows, not every user's
  in the tenant.** `internal/search` gated by tenant and never by user. An empty
  `user_ids` filter was read as a wildcard, so the DEFAULT request — with no
  caller input at all — returned rows for every user in the tenant across
  `search.sessions`, `search.tasks` and `search.artifacts`. A NAMED foreign user
  was honoured unexamined on all five methods. Both shapes are closed: an
  omitted `user_ids` folds to the caller's own user, and a named foreign user —
  or any multi-user set — is **refused** with `scope_mismatch` (403) rather than
  silently emptied.

  This is a security fix. The `(tenant, user, session)` triple is the isolation
  boundary and the user axis was not being enforced on this surface.

- **ACTION: the Console's ⌘K palette silently returns fewer rows for an ordinary
  operator.** The palette sends no user filter, so it lands on the folded path
  with no Console change: an operator now sees only their own sessions, tasks
  and artifacts. A deployment that had come to rely on the palette as an
  unofficial fleet view loses that, and there is deliberately no compatibility
  flag — an identity-downgrading knob is not something Harbor ships.

  **What restores the wider view:** the `admin` or `console:fleet` claim — the
  same claim that already widens the tenant axis on this surface and the same
  one `events.list` and `artifacts.list` take. Under either, naming a user reads
  that user and omitting one fans across the tenant. `harbor token --scopes
  console:fleet` mints a read-only one.

  Two known partials, stated rather than implied: a GRANTED cross-user crossing
  emits no audit event (pre-existing, and symmetric with the tenant axis), and a
  widened multi-user `search.events` read still returns only the first user's
  rows.

### Fixed — `artifact_fetch` refuses a window it cannot return intact

- **A binary artifact read through `artifact_fetch` used to arrive corrupted,
  silently, at a length the same response contradicted.** The window went out
  through a string-typed `content` field and JSON encoding rewrote every invalid
  UTF-8 byte to `U+FFFD` — an 8-byte PNG header arrived as 10, while
  `returned_bytes` still said 8.

  The read is now UTF-8-admissible or it refuses. On a refusal the response
  still carries `ref`, `mime`, `size_bytes` and `total_size_bytes` so the caller
  knows what it asked about; `content` is empty and the windowing fields
  (`offset`, `returned_bytes`, `truncated`) are zero-valued, because a refusal
  reporting `truncated: true` would invite endless paging into the same wall.
  The error names the stored MIME and the failing byte offset, and points at the
  route that does work: pass the artifact id to a tool that declares an
  artifact-reference parameter, and the runtime hands that tool the bytes
  directly.

  The gate is **content-driven, not MIME-driven** — it tests the actual window.
  In practice every binary type refuses (PDF, images, audio, archives), and so
  does a text artifact carrying one invalid byte mid-window rather than dropping
  it silently. Windowing artefacts are NOT refused: leading continuation bytes
  and a truncated trailing rune are trimmed, the window is admitted, and
  `offset` reports where the returned content actually begins rather than
  echoing what was requested.

  **ACTION:** a caller that was tolerating corrupted binary from
  `artifact_fetch` now receives an error object instead of bytes.

- **`artifacts.get` is deliberately unchanged and stays byte-exact.** Its
  `content` is `[]byte` (base64 on the wire), so it is correct for every MIME at
  every offset with no rune trimming — it is the byte-exact read, and a new test
  pins that against a future "restore consistency" refactor.

- **A `max_bytes` of 1–3 no longer pages forever.** The effective maximum floors
  at 4 bytes, so a window smaller than a single rune is served at 4 rather than
  trimming to empty while `truncated` stayed true and the documented paging rule
  returned the same offset indefinitely. The floor also applies to the
  operator-resolved `artifacts.fetch_default_max_bytes`.

- **A failed artifact-reference resolution is now machine-distinguishable.** Two
  classes ride the observation under an `error_class` key —
  `artifact_ref_not_found` (model-recoverable) and
  `artifact_resolver_unavailable` (operator misconfiguration, explicitly not
  model-recoverable) — and the same value lands on the parallel-branch
  observation as `error_class`. An ordinary tool error is byte-identical to
  before; no class key is written.

### Fixed

- **A hung MCP server no longer blocks a connection attach indefinitely.** The
  SSE/HTTP transport's session-termination request ran on a context the runtime
  does not own and the driver's HTTP client had no timeout at all, so a server
  that accepted the TCP connection and then answered nothing wedged the caller
  after its own handshake context had already expired. **This affected the
  already-shipped `agent_config.add_mcp_connection` admin verb, so deployments
  on 1.23.0 and earlier are exposed.** A request that carries no deadline of its
  own and is not a server→client event stream now gains a 15-second bound at the
  driver's shared choke point; a request the runtime already bounded keeps its
  own budget untouched, so an operator who raised a slow tool's `timeout_ms` is
  never pre-empted, and a streaming response is not truncated.

### Internal

- **The scaffold module pin and its guard.** `scaffold.FallbackModuleVersion`
  had drifted five releases behind — it is what `harbor --version` reports on an
  un-stamped source build and what `harbor new` writes into a scaffolded
  `go.mod`. The `drift-audit` check meant to catch that compared the pin against
  the SAME branch's `CHANGELOG.md` and declined to consult tags, so a branch
  whose release bookkeeping had been discarded by a release-heal merge satisfied
  it self-consistently. The check now reads the PUBLISHED tags (remote first,
  local tags, then `origin/main`'s changelog), reports honestly when no source is
  reachable instead of printing an unearned OK, and separately fails when the
  newest published release has no section in this branch's changelog.

- **Six inert smoke guards repaired, and the harness-level hole that let them
  hide.** Preflight failed on one condition only — a smoke exiting non-zero — so
  a script whose every check was a SKIP read as green. Two of this wave's guards
  had never once reported OK or FAIL: one posted to a `/v1/search/*` route that
  has never existed in the tree (the five `search.*` methods dispatch through
  `POST /v1/control/{method}`), and another posted to a `/v1/mcp/*` route that
  does not exist WITH an identity triple the dev bearer is not minted for, so it
  skipped two independent ways. Four more had their pass value and their
  can't-tell value collapsed together: two absence guards that skipped as
  "pre-215 build" on a shipped phase, a task-counter that returned `0` for a 401
  and for a malformed body alike, an `ok` arm accepting `200|401` across five
  methods, and a file guard that reported OK when the file was missing. All are
  now live and mutation-verified. Preflight itself now records each smoke's
  counters and FAILS when a script belonging to a SHIPPED phase produces zero
  OKs and zero FAILs; a pending phase's skeleton is reported and tolerated,
  which is the 404→SKIP convention working as designed. Switching that gate on
  found 24 shipped-phase scripts already asserting nothing — they are recorded
  in `scripts/smoke/inert-baseline.txt` as known debt, so a NEW inert guard
  fails immediately while the inherited backlog is visible rather than silent.

### Action required

1. **`artifacts.heavy_output_threshold_bytes` defaults to 128 KiB and no longer
   governs Console-facing Protocol replies** (`pause.list`, `memory.*`, the flow
   catalog, the three `mcp.*` reads), which pin at 32 KiB. A default config is
   unaffected; an explicit non-default value now applies to the LLM arm only.
   Set it to `32768` to restore the previous LLM-arm behaviour.
2. **Search is scoped to the caller's own user.** The Console ⌘K palette returns
   fewer rows for a non-admin operator, and a request naming a foreign user is
   refused rather than emptied. Mint `admin` or `console:fleet` for a fleet view.
3. **A dotted `meta_annotations` key now nests**, and three `_meta` declarations
   that were accepted before are refused at boot as well as on the wire. Run
   `harbor validate` against your config before upgrading.
4. **`artifact_fetch` refuses a non-text window** instead of returning corrupted
   bytes. A caller that consumed binary through it must move to
   `artifacts.get` or to a tool taking an artifact-reference parameter.
5. **Reusing an idempotency key while naming a different `agent_id` is a
   conflict**, not a silent adoption of the original task's agent.

## [1.23.0] — 2026-07-27

Minor: artifact bytes become readable on every store driver, the read-back bound
becomes operator policy, in-process tools can take content by reference without
that content ever re-entering the model's context, and an artifact listing is
scoped to the caller's own user. No Protocol wire-version change
(`ProtocolVersion` stays `0.1.0`).

**Upgrading:** two artifact store drivers migrate their key on first start and
the S3 object layout changes — see *Action required* at the end of this entry.

### Added — artifact bytes are readable on every store, and the read-back bound is yours

- **`artifacts.get` — a Protocol method that returns an artifact's bytes, served
  by every artifact driver.** `POST /v1/control/artifacts.get` takes the scope
  and the artifact id and returns the bytes plus the reference metadata.

  **Why it matters.** The only byte path the Protocol had was
  `artifacts.get_ref`, which returns a presigned URL — something only an
  S3-compatible store can mint. On the `inmem` default (and on `fs`, `sqlite`
  and `postgres`) it answers `presign_unsupported` / 501. So a deployment could
  offload heavy content correctly and then have no supported way to read it
  back. `artifacts.get` resolves through the store's mandatory read, so it works
  everywhere.

  **The two are not alternatives to pick between.** `artifacts.get` is the read
  every client can rely on. `artifacts.get_ref` is an optimisation for stores
  that can hand a large download off their own edge, so the bytes need not
  transit the Runtime. Both resolve the same reference under the same identity;
  reach for `get_ref` only when you know your deployment runs an S3-compatible
  driver.

- **Every artifact read now reports its own bound.** `artifacts.get` and the
  `artifact_fetch` built-in both answer with `total_size_bytes`,
  `returned_bytes` and `truncated` alongside the window, so a partial read is
  never mistakable for a complete one.

- **`offset` — page a large artifact instead of only reading its head.** Both
  the method and the built-in take a byte `offset`; read from 0, then re-read at
  the previous `offset + returned_bytes` while `truncated` is true. Windows are
  **byte** ranges, not lines or rows, so a window can begin and end mid-line and
  the caller splits the text itself.

### Added — two optional `artifacts` config keys

- **`artifacts.fetch_default_max_bytes` (default 65536) and
  `artifacts.fetch_hard_max_bytes` (default 1048576).** The read-back bound
  becomes yours to tune, alongside the `heavy_output_threshold_bytes` that
  governs what goes out. Both keys are optional and both default to the values
  the previous build hardcoded, so **no existing configuration changes
  behaviour.**

  A request above the ceiling is **served at the ceiling**, not refused, and the
  response says so through `truncated` — a caller has no way to discover the
  ceiling before asking, so a refusal would cost it a round trip and teach it
  nothing.

  **The ceiling bounds one read, not a sequence.** It is not a budget over
  repeated calls; aggregate consumption stays the governance layer's concern
  (cost ceilings and rate limits).

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

- **An artifact listing is scoped to the caller's own user unless an
  administrative claim widens it.** `artifacts.list` now decides its two
  identity axes separately:

  - **`user` is an isolation principal.** Omitting it lists YOUR artifacts —
    it is no longer read as "every user in the tenant". Naming another user is
    refused `403 identity_scope_required` unless the caller holds `admin` or
    `console:fleet`; with either claim, naming a user reads that user and
    omitting it fans across the whole tenant, which is the fleet view.
  - **`session` is a filter, not a boundary.** Omitting it still spans every
    session of your own, and naming one of your own sessions still narrows. No
    claim is involved either way — the `user` decision above already settles
    whose artifacts are in play.

  This is the same line `events.list` draws on the same two axes. The everyday
  call — `{"scope": {"tenant": "..."}}` — keeps working and needs no claim, and
  the Console Artifacts page and Background Jobs artifact card are unaffected
  because both already send the caller's own user. **ACTION** for anyone whose
  client relied on a tenant-only listing to enumerate a whole tenant: mint the
  token with `console:fleet` (read-only) or `admin`. `harbor token --scopes
  console:fleet` does it.

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

### Action required

- **SQLite and Postgres artifact stores run a forward-only `0002` migration on
  first start,** re-keying the artifacts table onto `(tenant, user, session,
  namespace, id)`. Rows that differ only by the producing task collapse onto a
  single row carrying the first writer's stamp. Take the usual backup before
  upgrading a Postgres deployment.
- **The S3 artifact object key drops the task segment** — new objects are
  written under `<tenant>/<user>/<session>/<namespace>/<id>`. Objects written by
  earlier versions stay readable and deletable via a session-prefix fallback and
  are never rewritten in place, so a bucket migrates by being read. Buckets whose
  lifecycle rules or external tooling match on the old key shape need those rules
  updated.
- **`artifacts.list` is scoped to the caller's own user** unless the request
  carries `admin` or `console:fleet`. A caller that omitted the user field and
  relied on receiving every user's rows in the tenant now receives its own; an
  operator tool that needs the wider view must present one of those claims.

## [1.22.0] — 2026-07-25

Minor: one shared body-identity gate across every Protocol surface, owner-scoped
MCP connection writes, MCP App replay on session reopen, and the re-landed
MCP-Apps host obligations. No Protocol wire change (`ProtocolVersion` stays
`0.1.0`), no config schema change, no migration.

**Upgrading:** one boot-time validation is newly enforced — see
*Action required* at the end of this entry.

### Added

- **A reopened session replays its rendered MCP Apps.** A `ui://` App that
  rendered during a turn now survives a session reopen instead of vanishing from
  the transcript: `HistoryTurn` carries `app` + `serverID` (reusing the live
  `MCPAppRefView` rather than a second wire shape), and history reduction folds
  the durable `mcp.app_available` event with the same guards, display-mode
  normalisation, and last-wins semantics the live decoder applies. Re-mount
  resolves the persisted tool context by its deterministic `tool_call_id` — no
  new storage and no new Protocol method. Resolution happens **before** the
  iframe mounts, so an unresolvable context renders an honest placeholder rather
  than a half-mounted frame; a resolution *failure* stays a loud error and is
  never laundered into the eviction copy. A non-empty `ToolCallID` now promises a
  fetchable record: the MCP driver stamps the id only when a tool-context record
  actually persisted, and runtime-added connections capture exactly as
  boot-config ones do. (D-348)
- **Five MCP-Apps host obligations.** `ui/notifications/size-changed` is
  consumed, with the inline frame bounded in CSS between the existing minimum
  and a new `--size-app-inline-max` token so any reported height is bounded by
  construction (fullscreen and picture-in-picture are unaffected);
  `ui/resource-teardown` and `request-teardown`; host-context `toolInfo` and
  `containerDimensions`; and `resources/templates/list`. (D-351)

### Changed

- **Every Protocol surface reconciles its request-body identity scope through
  one shared gate.** `internal/protocol/bodyscope.Reconcile` replaces thirteen
  near-duplicate helpers across both transports. The posture is a **registry
  row, not a policy value** — a call site can name a posture but never invent
  one — and each row declares its per-component rules (`Pinned` /
  `PinnedOrEmpty` / `AdminScoped`), the claims that grant a tenant crossing, a
  deny code, and a prose reason a reviewer can disagree with. A policy that
  permits a crossing must be handed a non-nil `Auditor`; a nil sink is a runtime
  error rather than an unrecorded crossing. The MCP Apps and MCP-Connections
  gates moved inside `Dispatch`, which makes those surfaces' transport-agnostic
  godoc true. (D-349)
- **`identity` carries provenance.** A third context key holds the
  transport-established triple (`WithVerified` / `FromVerified`); plain
  `identity.With` refuses to move the working identity past it, and
  `WithElevated(ctx, id, reason)` is the one named crossing, scoped to the
  audited tenant.
- **`SessionCounters` gains `Partial`,** distinguishing "we could not look" from
  "there were none" — including at the filter branches — so a cross-tenant
  admin listing never presents an unreadable scope as an empty one.
- **App→tool dispatch is confined to the App's own server.** An app-supplied
  bare tool name is qualified with the host-derived `serverID` before dispatch.
  Because the catalog key `<sourceID>_<tool>` is a string join over unconstrained
  charsets, `Registry.Register` now refuses — under the same write lock that
  installs the entry — a server id that sits inside another registered id's
  tool-name namespace, in both directions so registration order does not matter.
  `harbor validate` applies the identical rule.
- **MCP accessor errors are classified by sentinel, not by matching error
  text,** so a remote server's wording cannot decide a Harbor verdict and an App
  can distinguish "no such tool" from "you may not".

### Fixed

- **A request with no established identity is refused** with
  `identity_required` instead of falling back to the caller-supplied body scope.
- **Live MCP connection writes are owner-scoped.**
  `Registry.SetOAuthDiscoveryOrigins` resolves through a new `ownedEntry(name,
  owner)`, replacing the live allow-list only on the connection the caller's
  `(tenant, agent_id)` owns; a name another owner attached, a boot-declared
  name, and an unregistered name all refuse identically, and a zero or partial
  owner is refused at the choke point rather than at each call site. The
  boot-declared guard now applies on every path, before any revision read, so it
  is a property of the name rather than of the branch. Registry reads are
  unchanged (D-287/D-301). (D-350)
- **`agent_config.set_revision` validates connection descriptors before
  persist,** reusing `add_mcp_connection`'s shape authority — one authority, two
  doors — including the fail-closed stdio allowlist. Rejection leaves the active
  revision untouched, and the normalized descriptor is what persists, so both
  doors write identical bytes for identical input.
- **`OAuthDiscoveryAllowedOrigins` round-trips through the revision spine.**
  Both the domain and wire converters carry the field, so `set_revision` → `get`
  / `list_revisions` / `diff` reflects what the allowance write recorded.
- **Two cross-tenant admin reads no longer degrade silently.**
  `sessions.list` / `sessions.inspect` returned fabricated zero counters and
  `search.tasks` silently dropped foreign rows; both now perform an audited
  re-scope. `artifacts.delete` moved to its own admin-only registry row so the
  transport grants exactly what the surface honours.

### Internal

- **A three-part lockstep gate keeps the body-identity reconciler the only
  one,** in the `protocol-ts-gen-check` idiom, each part with a non-vacuity pin:
  *coverage* (every scope-carrying canonical request type joins to a registered
  surface, both directions — a new type or a deleted row fails `go test`),
  *enforcement* (an AST scan refusing any hand-written body-identity comparison
  outside the reconciler, reading through hoisted locals and aliased imports,
  with a reasoned allow-list), and *minting* (a reviewed call list for the
  verified-identity and elevation writers, with stale entries reported).
- **21 inert smoke guards repaired.** `assert_grep_absent` matched with basic
  regex while its siblings used extended, and swallowed grep's exit-2, so 21
  absence assertions across the suite were silently passing without running —
  among them hand-rolled-`fetch` bans on seven Console pages, a Protocol
  single-source check, a spawn-depth check, and one asserting absence from a
  file that no longer exists. All are live and passing; a malformed pattern or
  unreadable target now fails loudly with "the guard did not run". Smoke
  assertions in this wave are anchored to **call sites** rather than
  identifiers — an identifier appears in prose, a call site does not — and are
  mutation-verified to FAIL rather than SKIP.
- **Two smoke guards could only ever pass on macOS.** One asserted a
  tab-indented field with `\t` inside a `grep -E` pattern — BSD grep reads that
  as a tab, GNU grep as a literal `t`, so on Linux it could never match and
  reported a present field absent. The other ran `CGO_ENABLED=0 go test -race`,
  which on Linux fails to build with "-race requires cgo" rather than running
  anything. Both are fixed, and `drift-audit` now fails on `\t` or `\d` inside
  a `grep -E` pattern anywhere under `scripts/`, naming file:line; those two are
  the only escapes that diverge between the greps.
- **The TUI PTY end-to-end gate is hermetic.** The co-launch tests inherited the
  operator's real `HOME` and so read back a composer draft that an earlier run
  had persisted to `~/.harbor/tui-state.json`, making the suite non-idempotent.
  Each PTY launch now gets its own `HOME`, and the wave's env-key lookup walks to
  the filesystem root instead of five parents — previously, running from a nested
  working tree put the repository `.env` out of reach and three subtests skipped
  silently.

### Action required

A deployment whose configuration declares two MCP server ids where one sits
inside the other's tool-name namespace — for example `github` and
`github_enterprise` — is now refused at boot and by `harbor validate`.
Remediation is a rename, which moves every `<name>_<tool>` catalog key
referenced by agent YAML, `disabled_tools`, `paused_servers`, and persisted
revisions.

An MCP App must send the **bare** server-side tool name; an App that previously
sent an already-qualified Harbor catalog key will report a missing tool.

## [1.21.1] — 2026-07-25

Patch: Protocol body-identity reconciliation and the dev bootstrap endpoint are
tightened to the same contract the rest of the surface already applies. No
Protocol wire change, no config schema change, no migration.

### Fixed

- **Body-identity reconciliation validates the full `(tenant, user, session)`
  triple on the MCP surfaces.** The MCP Apps and MCP-Connections Protocol
  adapters reconcile a request-body identity scope against the auth-verified
  identity on all three components; an empty body triple is still backfilled
  from the verified identity, exactly as before. Neither surface has an
  admin-elevation path, so a body triple that disagrees with the verified
  identity fails closed with `identity_required`.
- **`artifacts.get_ref` reconciles the body scope's tenant before the store
  read,** so the artifact driver is never consulted for another tenant's scope.
- **`Boot` fails closed on a nil auth validator.** A validator factory that
  returns no validator and no error is now rejected with
  `ErrAuthValidatorRequired`. Identity is mandatory; the serve band never takes
  the test-kit validator-less path, and that path is unchanged for test kits
  that call `BuildMux` directly.
- **The dev bootstrap endpoint serves requests addressed to a local authority**
  (`localhost` or a loopback IP literal, with an optional port) and emits no
  CORS allow-headers, so the connection envelope is same-origin local only. The
  Console's one-click attach fetches it from `window.location.origin`; `curl`
  against `127.0.0.1`, the CLI, and every smoke script are unaffected. A request
  addressed to some other authority — including an external name resolved to a
  loopback address — is refused; address it to `127.0.0.1` or `localhost`.

## [1.21.0] — 2026-07-24

Minor: durable-by-default per-user skills, and wire-carried per-user credential injection for dynamically-added receiver-style MCP servers.

### Added

- **Durable-by-default per-user skills.** A new claim-free
  `agent_config.user.skills.{list,upsert,delete}` verb family lets an end user
  author personal skills that persist across all of their sessions, via a new
  `ScopeUser` rung keyed `(tenant, user)`. Durability follows the store driver —
  ephemeral on the in-memory driver, durable on SQLite/Postgres. The run-start
  projection unions durable user skills so they survive an admin membership pin,
  and a personal skill cannot widen capability (the default-deny capability
  filter scrubs any tool a skill names outside the run's allowed set), which is
  why the verbs need no elevated scope claim. (D-345)
- **Wire-carried per-user credential injection for runtime-added MCP servers.**
  The `add_mcp_connection` connection descriptor (and `agent_config.set_revision`)
  may now carry an optional `injection` mapping for receiver-style MCP servers,
  behind a fail-closed `tools.allow_wire_injection` / `HARBOR_ALLOW_WIRE_INJECTION`
  boot opt-in — so a coordinator can attach a receiver-style server at runtime and
  deliver each acting user's own broker-pulled credential without a boot redeploy.
  The injection provider names a boot-declared broker (no secret rides the wire),
  the reachable sink is derived from the connection URL, and every target key is
  redaction-covered. Composes with the wire-carried OAuth descriptor (D-340) and
  reuses the receiver-injection engine (D-341). (D-346)

## [1.20.0] — 2026-07-24

Minor: MCP Apps in the Console now adapt to the host — live theme + design tokens and real tool data delivered into the rendered app — re-landed handshake-safe.

### Added

- **MCP Apps host theme + design tokens.** When a `ui://` MCP App mounts in the
  Console, the host now hands it the viewer's light/dark preference (from the OS
  `prefers-color-scheme`) plus the Console's structural design tokens
  (`styles.variables`, the ext-apps `McpUiStyleVariableKey` namespace, mapped
  from `tokens.css`) in the `ui/initialize` host-context, and relays a live OS
  theme flip mid-session via `host-context-changed` — the app re-themes without
  a reload. A spec-conformant app themes itself to match the Console instead of
  booting to a fixed palette.
- **MCP Apps Data Delivery (Console half).** After the app sends
  `ui/notifications/initialized`, the host delivers the originating tool call's
  input then result into it (official AppBridge `sendToolInput` /
  `sendToolResult`, through the D-173 injected client reading
  `mcp.apps.tool_context`) — so the app renders its real content rather than an
  empty shell, and never re-invokes the tool (which would double a side effect).

  This re-lands the two Console halves reverted in #346 (D-226 Data Delivery,
  D-227 live theme) as one coherent, handshake-safe slice: the bridge is
  constructed once with the final host-context, the lifecycle effect is isolated
  from theme reactivity, and every host→app send is gated behind
  `oninitialized` — never a teardown-rebuild that would break `ui/initialize`.
  Gated by a real-iframe Playwright handshake test and a binding live-render of
  a real Dockyard `analytics-widgets` app under a real agent. Console-only; no
  Runtime/Protocol change. (D-342)

### Fixed

- **MCP-App re-render resource storm.** On a turn that produced no final answer
  (the task fails and the transcript re-renders), a single widget render could
  fire hundreds of `mcp.servers.read_resource` calls and log repeated
  "Not connected" tool-context delivery failures. The app-document fetch now
  dedups on the resource URI (an app-prop identity change to the same document
  no longer refetches and tears the bridge down), and tool-context delivery
  re-checks the transport is still connected after its async fetch — a bridge
  closed mid-fetch drops its stale delivery instead of throwing. A failed-turn
  render is now bounded to a handful of requests with zero delivery errors.

## [1.19.1] — 2026-07-24

Patch: config-validation hotfix for the v1.19.0 skills Postgres driver.

### Fixed

- **Skills `postgres` driver rejected by config validation.** The v1.19.0 skills
  `postgres` driver was rejected by config validation
  (`config.skills.driver: must be one of localdb`) despite the driver shipping —
  the static driver allowlist in the config validator was not extended.
  `skills.driver: postgres` now validates and boots; a config-validation
  regression test guards it.

## [1.19.0] — 2026-07-24

Minor: a Postgres driver for the skills subsystem — durable, shared skill storage for multi-instance deployments.

### Added

- **Skills Postgres driver.** The skills subsystem gains a `postgres` storage
  driver alongside the existing `localdb` (SQLite) default, bringing it to the
  §9 three-driver persistence parity (in-memory / SQLite / Postgres). Skills now
  persist in shared, durable Postgres for multi-instance / production
  deployments instead of only a per-instance SQLite file. The driver sits behind
  the existing `SkillStore` interface (no interface change, no `Supports*`
  ceremony), is identity-triple-scoped on every query, carries its own
  forward-only migrations, and passes the existing skills conformance suite
  unchanged. `localdb` remains the default — fully backward-compatible.

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

[Unreleased]: https://github.com/hurtener/Harbor/compare/v1.28.6...HEAD
[1.28.6]: https://github.com/hurtener/Harbor/compare/v1.28.5...v1.28.6
[1.28.5]: https://github.com/hurtener/Harbor/compare/v1.28.4...v1.28.5
[1.28.4]: https://github.com/hurtener/Harbor/compare/v1.28.3...v1.28.4
[1.28.3]: https://github.com/hurtener/Harbor/compare/v1.28.2...v1.28.3
[1.28.2]: https://github.com/hurtener/Harbor/compare/v1.28.1...v1.28.2
[1.28.1]: https://github.com/hurtener/Harbor/compare/v1.28.0...v1.28.1
[1.28.0]: https://github.com/hurtener/Harbor/compare/v1.27.0...v1.28.0
[1.27.0]: https://github.com/hurtener/Harbor/compare/v1.26.12...v1.27.0
[1.26.12]: https://github.com/hurtener/Harbor/compare/v1.26.11...v1.26.12
[1.26.11]: https://github.com/hurtener/Harbor/compare/v1.26.10...v1.26.11
[1.26.10]: https://github.com/hurtener/Harbor/compare/v1.26.9...v1.26.10
[1.26.9]: https://github.com/hurtener/Harbor/compare/v1.26.8...v1.26.9
[1.26.8]: https://github.com/hurtener/Harbor/compare/v1.26.7...v1.26.8
[1.26.7]: https://github.com/hurtener/Harbor/compare/v1.26.6...v1.26.7
[1.26.6]: https://github.com/hurtener/Harbor/compare/v1.26.5...v1.26.6
[1.26.5]: https://github.com/hurtener/Harbor/compare/v1.26.4...v1.26.5
[1.26.4]: https://github.com/hurtener/Harbor/compare/v1.26.3...v1.26.4
[1.26.3]: https://github.com/hurtener/Harbor/compare/v1.26.2...v1.26.3
[1.26.2]: https://github.com/hurtener/Harbor/compare/v1.26.1...v1.26.2
[1.26.1]: https://github.com/hurtener/Harbor/compare/v1.26.0...v1.26.1
[1.26.0]: https://github.com/hurtener/Harbor/compare/v1.25.0...v1.26.0
[1.25.0]: https://github.com/hurtener/Harbor/compare/v1.24.0...v1.25.0
[1.10.0]: https://github.com/hurtener/Harbor/releases/tag/v1.10.0
[1.0.0]: https://github.com/hurtener/Harbor/releases/tag/v1.0.0
