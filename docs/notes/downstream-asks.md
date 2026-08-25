# Downstream asks register (HA-NN)

> Status: live register. This file holds downstream asks that have been **received and triaged but not yet turned into a phase**. It is the staging area between "a consumer told us something is missing" and the point where the ask acquires a phase plan, a `D-NNN` decision, and a row in `docs/plans/README.md`.

## The `HA-NN` convention

Harbor tracks downstream asks under a monotonic `HA-NN` identifier. The identifier is **not** a phase number and never becomes one — it is the stable handle that survives the ask being re-scoped, split, merged, or rejected.

An ask graduates through three stages:

1. **Filed** — an entry in this file: what the consumer observed, what was verified in-tree (`file:line`), the corrected framing where the original report was stale, a priority, and a size.
2. **Planned** — the ask acquires a `docs/plans/phase-NN-<slug>.md` whose title carries the `(HA-NN)` suffix, a `D-NNN` entry in `docs/decisions.md`, and a `Status: Pending` row in the `docs/plans/README.md` phase index citing `HA-NN/D-NNN`.
3. **Shipped** — the register row flips to `Shipped (vX.Y)`; the `HA-NN` handle stays in the phase-plan title and the master-plan detail block so the provenance is greppable forever.

Prior asks (HA-16 … HA-37) skipped step 1 — they went straight from a conversation to a phase plan, which is why `grep -rn 'HA-3[89]' docs/` returned nothing for asks that had been discussed but not planned. This file closes that gap: **an ask is filed here the day it is received, whether or not a wave has room for it.**

Reading order for a triager: this file → the cited `file:line` evidence → `docs/decisions.md` for any `D-NNN` the ask touches → `docs/plans/README.md` for the phase that will carry it.

---

## Open asks

| Ask | Title | Area | Priority | Size | State |
|-----|-------|------|----------|------|-------|
| HA-38 | Host consumption of `ui/notifications/size-changed` | web/console (MCP Apps host) | Medium | Small | Shipped — phase 207 / D-351 |
| HA-39 | `artifact_ref` resolution in the MCP-Apps data-delivery path | web/console (MCP Apps host) | High | Small | Shipped — the D-347 consumer-1 Console fix |
| HA-40 | Replay of `mcp.app_available` in session-history hydration | web/console (sessions) | Medium | Contained | Shipped — phase 204 / D-348 |
| HA-41 | App→host `tools/call` server-namespace confinement | web/console (MCP Apps host) | High (security) | Small | Shipped — phase 207 / D-351 (items 1–2); item 3 (`_meta.ui.visibility`) still Filed |
| HA-42 | Progressive `tool-input-partial` streaming into a rendered App | internal/llm + internal/protocol + web/console | Low | Large | Deferred — reserved as D-343 |
| HA-51 | Bifrost reasoning byte fidelity | internal/llm + planner + tasks + Console | Release blocker | Contained | Shipped (v1.26) — phase 233c / D-402 |
| HA-54 | Typed retry classification for MCP `CallToolResult.IsError` | internal/tools/drivers/mcp + internal/tools | High | Contained | Shipped (v1.27) — phase 236 / D-410; planner-replay amendment Shipped (v1.28) |
| HA-55 | Operator-managed per-agent skill packs across authenticated users | internal/skills + runtime/agentcfg + runtime/serve | High | Medium | Planned — phase 237 / D-411 |
| HA-56 | Per-server MCP App callback catalog outside planner projection | internal/tools/drivers/mcp + internal/tools + internal/mcpconsole + protocol | High | Medium | Shipped (v1.27) — phase 238 / D-412; fresh render-admission amendment Shipped (v1.28) |
| HA-57 | Finite same-run step-tranche receipts/resume of the original live run | runtime + tasks + protocol | High | Contained | Shipped — phase 239 / D-418 |
| HA-58 | Governed read-only virtual child profiles derived from a parent | runtime + agentcfg + protocol | High | Medium | Shipped — phase 240 / D-419 |
| HA-59 | Virtual-child execution artifacts and bounded output forwarding by reference | artifacts + tasks + runtime + protocol | Medium | Medium | Shipped — phase 241 / D-420 |
| HA-60 | Durable identity-scoped task-progress projection | tasks + state + protocol | Medium | Contained | Shipped — phase 242 / D-421 |
| HA-61 | Verified-caller two-phase `SKILL.md` package import into durable personal skills | skills + agentcfg protocol + state + protocol | High | Contained-to-medium | Shipped (v1.28) — phase 243 / D-422 |
| HA-62 | Draft-only personal-skill proposer as an ordinary runtime tool | skills + tools + agentcfg + artifacts | High | Contained | Shipped (v1.28) — phase 244 / D-423 |
| HA-63 | Lifecycle-only session catalog and inspection projection | sessions/protocol + protocol + console | High | Contained | Shipped (v1.28) — phase 245 / D-424 |
| HA-64 | Durable tail-paged conversation turns | turns projection + sessions/protocol + protocol + console | High | Large | Shipped (v1.28); v1.29.4 idle-convergence and v1.29.5 lost-wake corrections shipped — phase 246 / D-425 |
| HA-65 | Persistent queryable observability rollups without raw-event scans | observability rollup + events + sessions + protocol + console | High | Large | Shipped (v1.28); v1.29.4 idle-rollup correction shipped — phase 247 / D-426 |
| HA-66 | Boot-declared resource-free operator skill baseline for the resolved boot/default agent | skills + config + runtime/serve + devstack | Medium | Contained | Shipped (v1.28) — phase 248 / D-427 |
| HA-67 | Optional per-parameter MCP artifact-egress mapping | tools/artifactegress + MCP driver | Medium | Small | Shipped (unreleased candidate; focused evidence only) — phase 249 / D-429 |
| HA-68 | Same-runtime organization skill publications with immutable revisions and exact agent references | skills/publication + StateStore + Protocol + runtime composition | High | Medium | Implemented (unreleased candidate; focused evidence only; hosted CI pending) — phase 250 / D-430 |
| HA-69 | v1.29.1 event metadata index and six-store PostgreSQL fleet safety | events + persistence + runtime pool/migrations + cutover | Release blocker | Large | v1.29.3 shipped; legacy-head repair extension closed — phases 251/252/253, D-431/D-432/D-433; HA-13 historical collision recorded |
| HA-72 | Stock coordinator receipt transport and grant readiness | llm receipts + runtime serve + protocol posture | High | Large | Accepted for the v1.30.2 release candidate — phases 257/258/261, D-437/D-438/D-441; tag/release/downstream acceptance pending |
| HA-73 | Stock coordinator-bound external-grant credential resolution | llm credential contract + runtime serve + public SDK | High | Contained | Implemented local candidate — phase 262 / D-442; hosted CI/release/downstream acceptance pending |
| HA-74 | Top-up successor grant preserves immutable authority and attempt identity | internal/llm + public SDK | High | Contained | Accepted for the v1.30.2 release candidate — phases 259/261, D-439/D-441; tag/release/downstream acceptance pending |
| HA-75 | Reach-admitted effective AgentID bound into external execution grants and receipts | internal/llm + runtime/serve + protocol posture + public SDK | High | Contained | Accepted for the v1.30.2 release candidate — phase 260 / D-440; tag/release/downstream acceptance pending |

The original five were filed by a downstream team building an MCP-Apps server
against Harbor. HA-51 is a separate release-blocking fidelity report; HA-54
is a separate MCP transport/reliability report; HA-55 is a separate runtime
skills projection report; and HA-56 is a separate MCP App host/catalog
report. HA-52 and HA-53 are already allocated in the shared register and are
not available for Harbor-local filings. HA-54, HA-55, and HA-56 were filed
as the v1.27 wave's registrations — phase 236 / D-410, phase 237 / D-411,
and phase 238 / D-412 respectively (master-plan index rows and detail blocks
carry the mapping), with HA-54's planner-replay amendment and HA-56's fresh
render-admission amendment shipped in v1.28. **HA-57 through HA-60 are Harbor-internal filings**,
raised by the wave's own reliability/verification review rather than by an
outside consumer, and are shipped in the same wave (phase 239 / D-418,
phase 240 / D-419, phase 241 / D-420, phase 242 / D-421). The entries are
**framework-framed** — each names a Harbor-side capability that is absent,
inert, or narrower than its documentation claims. **HA-61 through HA-66 are
the v1.28 filings** — personal-skill package import and draft authoring,
chat-open latency (lifecycle-only session projection and durable tail-paged
conversation turns), administrative observability (rebuildable rollup
projection), and the boot-declared operator skill baseline — each
**Shipped (v1.28)** as phase 243 / D-422, phase 244 / D-423, phase 245 / D-424,
phase 246 / D-425, phase 247 / D-426, and phase 248 / D-427, and each
**framework-framed**: they name Harbor-side surfaces that were absent or
read-shape-mismatched against the Protocol surface.

**HA-67 and HA-68 are the next Harbor-local filings.** HA-67 records the
small artifact-egress mapping gap: callers need an explicit optional marker
without changing the wire shape or weakening supplied-reference checks. It is
implemented as Phase 249 / D-429; this register records focused evidence only,
not the broad preflight gate. HA-68 records the organization publication gap:
one reviewed skill revision must be available to users with signed reach to an
agent without copying rows across user identities or returning bodies in
metadata. Phase 250 / D-430 has the domain, StateStore, canonical wire
contract, strict Protocol transport/client/capability/generated-doc lockstep,
exact run-start composition, and shared production/devstack bootstrap at the
 reviewed base. The shared publication StateStore conformance harness covers
 in-memory and SQLite locally and Postgres under `HARBOR_PG_DSN`; with that DSN
 unset, the local Postgres leg is skipped. Focused local evidence covers
 configured and unavailable wiring postures; broad preflight/full suites were
 not run locally and hosted CI evidence remains pending. This register records
 Harbor implementation evidence only and does not claim downstream acceptance.
Both asks are **framework-framed** and Harbor-local.

**HA-13 collision and HA-69 emergency filing.** The current canonical
register already consumes HA-13 for `flows.runs.list` (the historical
deferred global-sequence-index note in Harbor's older decision history does
not reserve that identifier). Reusing HA-13 would make two unrelated asks
share one handle, so the verified next free Harbor-local identifier is HA-69.
HA-69 is one release-blocking v1.29.1 ask with two coupled legs: (A) the
metadata-first durable event index that fixes `events.list`,
`events.aggregate`, and session-counter scan amplification with safe
atomicity/backfill/erasure; and (B) runtime-wide connection budgeting,
shared-pool ownership, namespaced/checksummed migration ledgers, and safe
split-to-unified cutover across state, memory, artifacts, skills,
sessions/turns, and observability/rollups. The existing Basic-4GB PostgreSQL
limit (`max_connections=103`) is a binding deployment constraint; no plan
upgrade or Harbor-side fleet mutation is part of this ask. Phase 251 / D-431
is planned and remains pending implementation, reviews, hosted CI, and the
v1.29.1 release lifecycle.

---

## HA-55 — operator-managed agent skill packs are not visible across the agent's users

**Priority:** High (functional gap). **Size:** medium.

**What the consumer sees.** A runtime can configure the durable PostgreSQL
skills store and register `skill_search`, `skill_get`, and `skill_list`, but an
operator cannot seed one authoring playbook for every authenticated
user of one agent. Importing or upserting the pack under the operator's
identity succeeds; an ordinary authenticated user session on that same agent sees an
empty catalog. Enabling the tools anyway would advertise a capability whose
normal production calls return no useful skill, rather than giving the agent
the intended playbook.

**Verified in-tree.**

- `internal/skills/skills.go:60-95` declares `project`, `tenant`, and
  `global` scope markers, but the actual storage contract is identity-keyed:
  `SkillStore.Upsert` is documented as `(tenant, user, session, scope, name)`
  at `:303-304`. `ScopeUser` is the only durable cross-session rung; the
  `SkillReader` contract says every other V1 rung remains pinned to the caller
  session at `:262-265`.
- The PostgreSQL driver makes that narrower behavior concrete. It persists
  every non-user row with the caller's session and always includes the caller's
  tenant and user in its key (`internal/skills/drivers/postgres/postgres.go:208-219`).
  Its normal `Get` and `List` resolution union only that caller session with
  same-user `ScopeUser` rows (`:371-381`, `:436-442`); the FTS, regex, exact,
  and semantic `Search` paths carry the same tenant/user predicate
  (`internal/skills/drivers/postgres/search.go:201-214`, `:285-288`,
  `:380-391`, `:421-425`). A row labelled `tenant` or `global` is therefore
  not visible to another user.
- `agent_config.skills.upsert` records a selected name into the versioned
  agent-config revision, but it writes the skill body through that same
  identity-scoped store (`internal/runtime/agentcfg/protocol/skills.go:58-105`)
  and the run-start resolver reads the resulting membership once per run
  (`internal/runtime/serve/runloop.go:750-778`). Membership cannot make a
  body that the caller's `SkillStore` cannot resolve appear. The separate
  user-skills API deliberately forces `ScopeUser`
  (`internal/runtime/agentcfg/protocol/userskills.go:87-145`), so it is also
  not an operator-pack mechanism.

**Correction to a tempting workaround.** Do not solve this by treating
`ScopeTenant` or `ScopeGlobal` labels as implemented visibility, by copying a
pack into every user triple, or by using a shared service identity. The first
two either produce an empty tool result or make lifecycle/revocation and
cross-user isolation nondeterministic; the last makes a user session read a
different principal's catalog. A CLI import triple is a local bootstrap aid,
not a production registration contract.

**Requested shape.** Add a first-class, operator-managed **per-agent skill
pack** source, durable and versioned with the agent-config revision. It should
be addressable by `(tenant_id, agent_id, skill_name)` for configuration
selection, but `agent_id` remains a runtime/config entity rather than an
identity principal: the read still starts from the authenticated caller's
verified `(tenant, user, session)` and its signed reach to the effective
agent. The composed run snapshot should contain only (a) the selected operator
pack for that effective agent and tenant plus (b) that caller's permitted
personal/session skills. It must not turn an ordinary tenant/global catalog
read into a broad cross-user search.

The control plane should offer an elevated operator mutation path that stores
the pack body and advances the selected agent's content-addressed revision
atomically (or compensates on failure), with the existing diff/rollback and
audit semantics. A pack item carries required-tool metadata, but that metadata
is never a grant: the existing run-visible-tool capability filter and redactor
must remain in front of the injected directory and all three `skill_*` tools.
The runtime should expose the same immutable composed snapshot to directory
injection, `skill_search`, `skill_get`, and `skill_list`; a change applies
next run only.

**Required acceptance.**

1. With a real durable Postgres skills/agent-config backing store, an elevated
   operator creates a pack and pins it to one agent. Two different users and
   sessions in the same tenant, each with signed reach to that agent, both see
   it through the directory and `skill_search`/`skill_get`/`skill_list` after
   restart.
2. A same-tenant user selecting a different agent, and a user in another
   tenant, cannot discover or fetch that pack by any name/query. Each user's
   personal skill remains invisible to the other user and cannot replace the
   operator-pack body.
3. A pack whose `RequiredTools` mention a missing, paused, disabled, or
   scope-filtered MCP tool is filtered/redacted exactly as today; it never
   expands the run's visible tool set. The test must exercise a dynamically
   attached MCP source, not only in-process tools.
4. A membership change, rollback, and revoke affect the next run only. An
   already-started run retains its immutable snapshot; an unselected or
   revoked pack answers a typed not-found/empty result without falling through
   to another principal's row.
5. Non-operator callers cannot create, update, or remove packs; malformed
   scope/agent/identity input fails before persistence; all mutations emit the
   existing skill and agent-config audit/revision evidence. Include a
   concurrent mixed-tenant/user/agent test under `-race` with no context or
   authority bleed.

**Consumer follow-up once this lands.** A consumer's broad operator `SKILL.md`
must be converted to a compact Harbor skill body for its private-owner route
before registration: retain the context → bounded read → typed apply → present
workflow, remove unavailable publish/share instructions, and declare the
actual downstream tool names as required metadata. The consumer will then
configure the skills Postgres DSN, enable `skill_search`, `skill_get`, and
`skill_list`, register the pack through the new elevated agent-pack path, and
add a real authenticated-session acceptance test. Until then it will keep the
durable tool guidance in its agent prompt and will not claim runtime skills
are available.

---

## HA-56 — app-only MCP callbacks need a per-server dispatch catalog distinct from planner projection

**Priority:** High (host interoperability and least-privilege discovery).
**Size:** medium (MCP discovery/catalog representation, App host dispatch
surface, Protocol/transport parity, and integration fixtures).
**State:** Shipped (v1.27) — phase 238 / D-412; the fresh render-admission governance amendment below is Shipped (v1.28).

**Governance amendment — corrected fresh render-admission contract (Shipped
v1.28).** A verification review corrected the render-admission half of the shipped
contract; the original app-only callback catalog history is preserved. A
rendered MCP App has exactly two render paths: the LIVE path may use a
bounded, short-lived, provider-local binding for a live tool-result App
only — never durable, never restored — while embedded/durable reopen uses a
FRESH stateless, integrity-protected, shared-KEK admission minted only after
verified identity, signed effective-agent reach, retirement, erasure,
current session/agent exposure, exact server, exact current `ui://` resource
availability, paused/disabled state, and the exact CURRENT
provider/catalog generation — a deterministic, replica-stable generation of
the server's current discovered catalog that changes on detach, replacement,
and ANY successful discovery/catalog/resource change even when deployment
descriptor configuration did not change (the existing exact registration
descriptor fingerprint remains a retained input but is never alone
sufficient authority; a process-local discovery counter is not acceptable,
and a replica holding a different current catalog fails closed as a
generation mismatch). The exact reopen order is: the durable App reference
from the reopened session's turn rows, a successful `mcp.apps.tool_context`
replay (a failed / unavailable / evicted / foreign replay mints no
authority), the current `ui://` read explicitly requesting one fresh
admission (`request_render_admission: true` — the only minting read;
ordinary and AppBridge-secondary resource reads never mint), the
iframe/AppBridge mount, and then same-server app-only callback dispatch
through the existing wrapped invocation (the distinct admission-aware
AppsAccessor path) echoing the fresh admission as the distinct
`render_admission` authority. The fresh admission is distinct from, never
aliases, and never coexists with the legacy live binding; neither is
persisted or restored. Claims bind schema/time/triple/effective-agent/server/
resource/current provider/catalog generation and carry no raw args, secrets,
provider output, callback name, or general capability. Ordinary resource reads never mint;
only the explicit admission-requesting read path does. Callbacks stay absent
from planner/`tools.list`/search/generic resolution and dispatch via
same-server `ResolveAppTool` + existing approval/OAuth/policy/redaction/
retry/audit. HA-64 rows retain metadata/component availability only, no
token; `mcp.apps.tool_context` replay is unchanged and never reruns the
originating tool. Typed unavailable/expired is explicit and refresh requires
fresh checks. The surface is strictly opt-in — sealer availability alone
never enables render admission, even when an OAuth broker already supplies
the shared KEK — and every mint/verify reads the reach-admitted effective
agent stamped in the request context, never a fixed boot/default fallback.
Production/devstack share one implementation and one immutable
shared sealer; the surface is enabled by
`tools.mcp_app_render_admission.enabled` (default `false`) and seals with the
existing `tools.oauth_token_kek_env` KEK — no second authority field; an
enabled surface with an empty env name, a missing/unset/invalid KEK, or a
sealer construction failure fails readiness loud even with no OAuth provider
or credential broker configured; restart/multi-replica success requires the
shared KEK and the same current provider/catalog generation — a replica
holding a different current catalog fails closed as a generation mismatch.
No generic capability framework, persisted callback authority, arbitrary
origins, provider exceptions, hot registry, or transcript impersonation. No
new phase, decision, HA, event, or Protocol method is allocated; HA-56
remains phase 238 / D-412.

**What the consumer sees.** A provider may mark an MCP tool with
`_meta.ui.visibility: ["app"]` because it is a callback for its rendered App,
not an operation for the model to select. Harbor currently treats that marker
as decorative: the tool is shown to the planner and, if a future list filter
simply hides it, the existing App callback path loses the catalog entry it
resolves. The host needs two deliberately different views of one discovered
server: an app-owned callback catalog for that server and a model/planner
projection that excludes app-only tools.

**Verified in-tree.**

- `internal/tools/drivers/mcp/content.go:155-186` parses MCP App resource and
  display metadata, but not `_meta.ui.visibility`. `internal/tools/tools.go:122-188`
  has no field that can retain an app-only visibility class once discovery
  returns a normal `Tool`.
- `internal/tools/protocol/catalog_projector.go:253-264` projects `tools/list`
  from the ordinary catalog, and `internal/tools/planner_view.go:48-60` lists
  and resolves against that catalog. Neither surface has an app-only filter.
- `internal/mcpconsole/apps.go:253-304` resolves an App `tools/call` through
  the same full catalog before applying the current-state gate. It therefore
  cannot survive a planner projection that removes the entry unless it has a
  server-owned callback lookup. The existing HA-41 confinement work is not a
  substitute: it confines the App to its source server; it does not retain a
  separate callback catalog or hide the callback from the model.

**Requested shape.** At MCP discovery, preserve the provider-authored
`_meta.ui.visibility: ["app"]` classification and construct an internal,
per-MCP-server **App dispatch catalog** alongside the ordinary planner/model
projection. An app-only entry must be absent from planner context, generic
`tools/list`, planner search/resolve, and ordinary model tool invocation. It
must remain callable only by the rendered App associated with the same
host-derived server identity through a host/App dispatch surface; no string
prefix or remembered global tool name may select another server's callback.

The App dispatch path remains subject to the exact existing identity triple,
effective-agent capability filtering, OAuth/approval wrappers, current-state
checks, redaction, and audit rules. Visibility is not a grant. A caller in a
different tenant/user/session, a different agent without reach, or a rendered
App from another MCP server must receive a typed not-found/denied result before
tool execution. A generic planner call to an app-only name must not become a
backdoor merely because the name is known.

Dynamic attach, reconnect, and catalog refresh must rebuild both views from
one discovered server snapshot: a newly added callback becomes usable only
through that server's App host, a removed callback stops resolving everywhere,
and no stale callback may survive a replacement or detach. The contract must
hold for both HTTP and stdio MCP transports. The phase may choose the internal
API and any additive wire shape, but it must make the host-derived server
identity available to the dispatch check rather than trusting an App-supplied
catalog name.

**Required acceptance.**

1. A real MCP fixture publishes one ordinary tool and one
   `_meta.ui.visibility: ["app"]` callback from the same server. The ordinary
   tool is visible to the planner; the callback is absent from planner context,
   generic `tools/list`, search, resolve, and ordinary call paths, while the
   matching rendered App invokes it successfully through the same server's
   callback catalog.
2. A callback request from another server's rendered App, a direct generic
   planner request using the remembered callback name, and a mismatched or
   missing server identity all fail before invocation. The test records zero
   callback executions and proves existing OAuth/approval/current-state gates
   still run for the permitted same-server invocation.
3. Dynamic attach, reconnect, refresh, replacement, and detach tests prove
   the two views update atomically without stale entries, for HTTP and stdio
   servers. Include a paused/disabled/scope-filtered source so an app-only
   marker cannot bypass capability filtering.
4. Authenticated mixed-tenant, mixed-user, and mixed-agent-reach tests prove
   an App callback catalog cannot cross the verified identity boundary. Run the
   concurrent path under `-race` without context or authority bleed.
5. Compatibility fixtures for Bamboo, WorkBridge, and Prototype Workbench
   exercise their app-only callback metadata and prove no callback is exposed
   to the planner while each provider's own rendered App remains functional.

**Non-goals.** This ask does not make `_meta.ui.visibility` an authorization
shortcut, expose app-only tools to ordinary callers, or add provider-specific
exceptions. It asks for one reusable Harbor catalog/dispatch primitive that
preserves an MCP App callback without advertising it as a model tool.

---

## HA-38 — the host never consumes `ui/notifications/size-changed`

**Priority:** Medium. **Size:** small (one handler + a clamp).

**What the consumer sees.** An inline MCP App iframe is stuck at a fixed minimum height, so every content-bearing App must hard-code one height and scroll internally — a second scroll region nested inside the already-scrolling chat transcript. The App half emits the size notification unprompted (the `@modelcontextprotocol/ext-apps` view SDK auto-emits it); the host discards it.

**Verified in-tree.**

- `web/console/src/lib/chat/renderers/app-bridge-host.ts:507-511` assigns exactly five bridge hooks — `oncalltool`, `onreadresource`, `onlistresources`, `onrequestdisplaymode`, `oninitialized`. No size-change handler is assigned.
- A repo-wide search for `sizechange` / `onsizechange` / `size-changed` across `web/console/src` returns **zero** hits. Nothing consumes the notification anywhere in the Console.
- `web/console/src/lib/chat/renderers/mcp-app.svelte:362` pins the iframe to `min-height: var(--size-app-inline-min)`, defined as `12rem` at `web/console/src/lib/tokens.css:177`. There is no growth path above it.

**The discrepancy against a Shipped phase — CONFIRMED.** `docs/plans/phase-109k-mcp-apps-conformance-hardening.md:49` carries the acceptance criterion "the host consumes `ui/notifications/size-changed` and the inline iframe height tracks the app's reported content height"; `docs/decisions.md` D-227 item 4 records it as delivered; the `docs/plans/README.md` phase index marks 109k **Shipped (V1.1.x)**. The handler is not there.

**Root cause (traced, not inferred).** The 109k merge commit is `b37ca533` — *"phase-109k: MCP Apps spec-conformance hardening **+ revert Console MCP-Apps surface to v1.4 (handshake regression)**"*. Its second commit reverted the entire Console half of 109k because the additive Console changes broke the `ui/initialize` postMessage handshake (a rendered App showed `request 'ui/initialize' timed out after 30000ms`). The Go/backend half was kept; the Console half — size-changed, `ui/resource-teardown`, `request-teardown`, live theme, host-context `toolInfo`/`containerDimensions`, `onlistresourcetemplates`, and the HA-41 namespace prefix — went away in the same PR that "shipped" them. Phase 109l (`0b4618fc`) later re-landed **only** the live-theme and data-delivery halves. `git log -S'onSizeChanged' -- web/console/` returns nothing: the handler was never in the source tree at any commit — it exists only as prose in D-227's test notes.

**Why it went unnoticed.** `scripts/smoke/phase-109k.sh:64-68` guards the obligation with a `grep -q 'sizechange'` that emits `skip` (not `fail`) when absent. Per CLAUDE.md §4.2 item 5, *"a SKIP that should be an OK is a bug"* — the counter has been silently reporting SKIP for a Shipped phase ever since.

**Requested shape.** Assign a size-change handler on the bridge and let the inline iframe height track the app's reported content height, clamped between the existing `--size-app-inline-min` and a new host-owned maximum, coalesced through `requestAnimationFrame`/debounce so a resize storm cannot thrash layout. The sandbox and CSP posture are untouched. When it lands, flip the 109k smoke's SKIP to an OK.

**RESOLVED — phase 207 / D-351 (2026-07-25).** The handler is assigned (`AppBridgeHost` wires `onsizechange`) and the inline frame's height tracks the app's reported content height, rAF-coalesced. The clamp is CSS rather than JavaScript — `min-height: var(--size-app-inline-min)` plus a new host-owned `max-height: var(--size-app-inline-max)`, scoped to a `.mcp-app__frame--inline` modifier so the fullscreen / side-by-side panel (which reuses the base class and sizes itself) is not capped — so any reported value lands inside the host's envelope by construction and a misbehaving app scrolls inside its own box; a JS clamp would have been one edit from unbounded. An app that never reports a size keeps exactly the previous fixed-height behaviour. Sandbox and CSP posture untouched. `scripts/smoke/phase-109k.sh` no longer has a SKIP to flip: it was rewritten into 14 hard assertions, so the same regression now FAILS preflight.

**Related residue from the same revert (not part of this ask, but the same root cause):** `ui/resource-teardown` on unmount, the `request-teardown` handler, host-context `toolInfo` / `containerDimensions`, and `onlistresourcetemplates` were likewise absent from `app-bridge-host.ts` while D-227 item 4 records all of them as shipped. **Phase 207 closed all of them in the same slice** (the explicit decision this paragraph asked for), and corrected the record in D-227's Consequence, the master-plan 109k row + detail, and this register.

---

## HA-39 — heavy tool data reaches the App as a stub, not as renderable structure

**Priority:** High (functional gap). **Size:** small.

**Correction to the original report.** The ask as filed claimed a **silent `undefined` data loss**. That framing is **stale** and should not be carried into a phase plan. Phase 109l shipped a by-reference text stub with an explicit "fail loudly, never silently inline" posture, so nothing is silently dropped today. The residual gap is narrower and different in kind.

**The actual residual gap.** When the tool payload is heavy, the App receives *prose about* the data instead of the data:

- **App-initiated `tools/call` results** — `web/console/src/lib/chat/renderers/app-bridge-host.ts:343-359`: on `result.artifactRef` (line 344) the host pushes a single text block `[artifact <id> · <n> bytes]` and returns. It makes **no** attempt to resolve the reference, and `structuredContent` is left unset — it is only populated from an inline `result.content` (line 358). An App that renders off `structuredContent` gets nothing.
- **Initial tool-context delivery** — `app-bridge-host.ts::#payloadToResult` (`:634-660`): the heavy branch *does* resolve the artifact via `fetchArtifactText` — but it returns `{ content: [{ type: 'text', text }], isError }` (line 648) with **no `structuredContent`**, while the inline branch sets `structuredContent` (line 641). So even on the path that fetches the bytes, the App's rich view is starved; only the inline-sized path feeds it.

Either way an App cannot render its rich view over heavy data — which was Phase 109j's stated goal (a resolve-or-stub at the iframe edge).

**The resolution path already exists, in the same file's neighbourhood.** `web/console/src/lib/mcp-app-host-client.ts:119-126` implements `resolveArtifact` → `artifacts.get_ref` → `presigned_url`, and `fetchArtifactText` sits beside it at `:150`. The App-bundle renderer already uses this path for the heavy `ui://` document. So the fix is wiring an existing capability into two branches, plus setting `structuredContent` from the parsed bytes — not new machinery.

**Open sub-question (record, do not resolve here).** The design anticipates `CodeNotFound` for an **evicted** tool-context id — `internal/mcpconsole/toolcontext.go:195-208` documents "an unknown or cross-identity `(serverID, toolCallID)` returns a wrapped not-found error whose marker the Protocol surface maps to `CodeNotFound`", and `web/console/src/lib/mcp-app-host-client.ts:129-148` treats `not_found` as "no captured context, no delivery". But there is **no TTL, no eviction, and no sweeper** anywhere in `internal/mcpconsole/toolcontext.go`: records are written as ordinary session-scoped `StateRecord`s and are never expired by that package. The two halves disagree — the read path is built for eviction that the write path never performs. A phase that picks up HA-39 should settle which is right: either an explicit retention/eviction policy for tool-context records (and then the `CodeNotFound` branch is load-bearing), or a statement that these records live for the session's lifetime and the `not_found` branch covers cross-identity and unknown-id only.

**RESOLVED — the D-347 consumer-1 Console fix (2026-07-26).** Both branches now resolve the reference and both deliver `structuredContent`, so an App's rich view is fed identically whether its data was small enough to inline or not — SIZE no longer decides the shape.

- **The resolution path the ask pointed at was itself broken, which is why the fix is not merely "wire the existing capability into two branches."** `fetchArtifactText` resolved `artifacts.get_ref` and fetched the presigned URL it returned. `get_ref` type-asserts the OPTIONAL `artifacts.Presigner` capability — one of five shipped drivers implements it, and it is **not** `artifacts.DefaultDriver`. So wiring the two branches onto that path would have moved the failure without removing it: an App on a stock deployment would still have received prose. It now routes through `artifacts.get`, the driver-independent byte read (D-353), which resolves through the MANDATORY `ArtifactStore.Get`. The read PAGES — one response is bounded by the deployment's fetch ceiling and says so — and the windows are concatenated as bytes and decoded once, because a byte window may split a multi-byte rune.
- **`resolveArtifact` deliberately stays on `get_ref`.** Its caller needs a URL a browser can load into a frame, which no Protocol method can mint. That arm was already made driver-independent from the other side (D-218 raised the Runtime's inline cap for `ui://` documents), so a stock deployment does not reach it.
- **Both §17.6 riders D-347 named are closed with it.** The by-reference result branch now sets `structuredContent`; and `#payloadToArgs`'s bare `catch { return {} }` is gone — an unreadable input raises the typed `MCPAppArtifactUnavailableError` and the host **withholds** the `ui/notifications/tool-input` notification (whose `arguments` field is optional in the spec) rather than asserting that the tool ran with none. `{}` is now sent by exactly one branch: the one where the capture genuinely holds no input. The result delivery proceeds either way.
- **The unavailability notice is retitled.** `[artifact <id> · <n> bytes — unavailable on this store]` became `… — could not be read]`: the old wording named a cause that is no longer true, since every store serves the read.
- **Coverage.** The byte path is pinned by a CAPTURED TRANSCRIPT of a live `harbor dev` on the default `inmem` store (`web/console/src/lib/testdata/artifacts-get-inmem-transcript.json`), which carries the real multi-window `artifacts.get` responses **and the real `presign_unsupported`/501 `get_ref` refusal from the same store** — so a regression back to the presign route fails exactly as it fails in production. The real-App Playwright gate now delivers its result BY REFERENCE, so the vendored ext-apps client itself proves `structuredContent` arrives on that branch.
- **The retention sub-question is still NOT settled** — nothing here touches it.

---

## HA-40 — rendered Apps vanish when a session is reopened

**Priority:** Medium. **Size:** contained.

**What the consumer sees.** An App that rendered live in the transcript is gone after reopening the session; the transcript degrades to the deliberately-terse model-facing text the tool emitted for the LLM, which reads as a broken or empty turn.

**Verified in-tree.** `web/console/src/lib/sessions/history.ts::reduceHistoryTurns` (`:258-388`) folds `llm.completion.chunk`, `llm.cost.recorded`, `planner.decision`, `tool.completed`, `tool.failed` / `tool.policy_exhausted`, `task.started`, and the terminal task types. It does **not** handle `mcp.app_available`. Independently, the `HistoryTurn` interface (`:135-171`) has no `app` (or `serverID`) field — so even a reducer case would have nowhere to put the result. The type cannot represent an App.

**Why this is a reduction gap, not a durability gap.** The backing data is already durable: the tool context is persisted as a session-scoped `StateRecord` under a deterministic content-hash key (`internal/mcpconsole/toolcontext.go`), and `mcp.app_available` is a canonical bus event that `state.history` replays. Nothing needs to be newly stored.

**Requested shape.** Reduce `mcp.app_available` in `reduceHistoryTurns`, add the `app` field to `HistoryTurn`, and have the hydration path mount the renderer exactly as the live path does. **Define the miss-behaviour explicitly**: when the referenced tool context is evicted or otherwise unresolvable, the hydrated turn must render an honest placeholder that names what is missing — never a blank bubble and never a half-mounted iframe that fails its handshake. (That placeholder decision interacts with the HA-39 eviction sub-question above; the two should be settled together.)

**State: SHIPPED — phase 204 / D-348** (`docs/plans/phase-204-mcp-app-replay.md`). Zero-wire, Console-only, exactly the requested shape: `HistoryTurn` gains `app` (the LIVE `MCPAppRefView`, reused rather than re-declared) + `serverID`, `reduceHistoryTurns` folds `mcp.app_available`, `hydratePastTurns` sets both, and the shipped renderer re-mounts — reading the already-persisted tool context by its deterministic content-hash `tool_call_id`. The miss is defined by moving the context resolution BEFORE the iframe mount: `not_found` renders a stable "this view is no longer available" placeholder with no iframe and no bridge; a non-`not_found` failure keeps the loud error state; a ref that never carried a correlation id mounts unchanged. **The HA-39 retention sub-question is deliberately NOT settled by it** — the Console is made correct under either answer, since `not_found` is already reachable today for an unknown id, a cross-identity id, and a restarted in-memory state driver. HA-39's heavy-`artifact_ref` limitation is likewise inherited, not fixed: a replayed App behaves no worse than a live one.

**It also closed a related production gap (§17.6), which is relevant to anyone reading HA-39's sub-question.** Defining the miss made the correlation id a promise, and two producers were breaking it: the runtime-attach path (`MCPConnectionAttacher`) never wired the tool-context capturer — only the boot-config path did — and the driver stamped the id unconditionally, so a `ui://` app declared by a tool on a server attached via `add_mcp_connection` advertised a context that had never been written. Both are fixed: the attacher threads the store, and the id is stamped only after a successful capture. So a `not_found` today genuinely means "the record is gone", not "it may never have existed" — which narrows the retention question HA-39 still has to answer.

---

## HA-41 — an App is not confined to its own server's tools

**Priority:** High (security posture). **Size:** small.

**Correction to the original report.** The ask as filed claimed **D-227 is still an OPEN decision** and that the capability "is neither working nor cleanly absent — it is undetermined." That framing is **wrong and should not be carried forward**. D-227 is Accepted and its FAIL-2 resolution is fully specified (item 3): the frontend prefixes the app-supplied bare tool name with the bridge's `serverID` before dispatching, which both resolves the catalog key and confines the App to its own server's namespace, with no Protocol change. There is **no open decision**. What is open is that the decided code was reverted and never re-landed.

**Backend trace, end to end.**

1. Method constant: `internal/protocol/methods/methods.go:752-759` — `MethodMCPAppsCallTool = "mcp.apps.call_tool"`.
2. Wire request: `internal/protocol/types/mcp_apps.go:116-131` — `MCPAppCallToolRequest{Identity, Tool, Arguments}`. **There is no `server_id` field.** The `Tool` godoc states the value is "the catalog tool name to invoke (the Harbor-side `<source>_<tool>` name)" — i.e. the wire contract expects an already-qualified name and has no way to derive or verify one.
3. Handler: `internal/protocol/apps.go:273-322` (`AppsSurface.handleCallTool`) — validates identity and non-empty `Tool`, then passes `r.Tool` through verbatim to the invoker. No qualification, no scoping.
4. Runtime adapter: `internal/mcpconsole/apps.go:248-251` (`AppsAccessor.CallTool`) — `desc, ok := a.cat.Resolve(tool)` on the **raw** name.
5. Catalog: `internal/tools/catalog.go:145-150` — a flat `c.byName[name]` map lookup with no source scoping. The catalog handed in is `in.Catalog`, the **full** runtime catalog (`internal/runtime/serve/mux.go:292-301`), not a per-server or per-run view.

**Answers to the four questions.**

1. **Is D-227 FAIL-2 implemented?** **No.** The backend never receives a server id and never namespaces. D-227 placed the namespacing in the frontend, and the frontend does not do it: `web/console/src/lib/chat/renderers/app-bridge-host.ts:338-342` — `createAppHandlers.oncalltool` calls `client.callTool(name, args)` with the app-supplied name unchanged, even though `serverID` is destructured into scope on line 336 and used by the sibling `onreadresource` / `onlistresources` handlers. `web/console/src/lib/mcp-app-host-client.ts:84-85` forwards it straight to the Protocol. `git log -S'${serverID}_' -- web/console/src/lib/chat/renderers/app-bridge-host.ts` returns **no commit**: the prefix has never existed in that file at any point in history. It was written and then reverted inside the single squashed 109k merge `b37ca533` (see HA-38), so no commit ever carried it, and 109l did not re-land it.
2. **Is the server id host-derived or caller-influenced?** In D-227's design it is **host-derived and safe**: `AppBridgeHost` receives `serverID` as a construction option (`app-bridge-host.ts:478`) from the renderer's props, which trace back to the backend-minted `server_id` on the `mcp.app_available` event — stamped from the tool descriptor's source at `internal/mcpconsole/apps.go:435-446`. Code inside the sandboxed iframe cannot influence it. The design is sound; only the wiring is missing.
3. **Can an App call another MCP server's tool?** **Yes, today.** Because the raw name is looked up in the flat full catalog, an App that emits `tools/call` with `otherserver_some_tool` reaches that tool. The blast radius is in fact wider than "another MCP server": **any** catalog entry — in-proc tools, HTTP tools, A2A tools, planner meta-tools — is reachable by exact name. The only checks between the App and invocation are (a) the identity triple, (b) the current-state exposure gate (`internal/mcpconsole/apps.go:308-351`, which rejects paused servers and disabled tools but performs **no** origin-server confinement), and (c) the tool's own approval / OAuth wrappers, which do fire because the proxy re-enters the real dispatch path. So this is a **confinement** gap, not an authentication or approval bypass.
4. **What does a caller get for an unresolvable name?** A **generic** error, not a typed not-found. `internal/mcpconsole/apps.go:250-251` returns `fmt.Errorf("%w: %q", tools.ErrToolNotFound, tool)`, whose text is `tools: tool not found` (`internal/tools/tools.go:430`). The Protocol edge classifies by string marker: `internal/protocol/mcp.go:1099-1108` (`isMCPNotFound`) matches only `"server not found"` and `"tool context not found"` — neither matches — so `mapMCPError` (`:1076-1093`) falls through to the default and returns **`CodeRuntimeError` with the message "MCP read failed"**. A caller cannot distinguish "that tool does not exist" from "the MCP transport broke."
5. **Is `_meta.ui.visibility: ["app"]` enforced?** **No — it is not even parsed.** The `_meta.ui` reader (`internal/tools/drivers/mcp/content.go:145-190`) extracts only `resourceUri` and the display-mode hint (`preferredFrame` / `displayMode`). A repo-wide search finds no `visibility` handling in any MCP or catalog path, and `tools/list` applies no app-only filter. A server marking a tool app-only today has that tool advertised to the planner like any other. Purely decorative.

**Honest residual sizing: (b) small refinements — not (a) nothing, and not (c) an open decision.** Three concrete, independently-shippable items, all inside already-settled decisions:

1. **Re-land the D-227 item 3 prefix** in `createAppHandlers.oncalltool` (`app-bridge-host.ts:338`), with the vitest cases D-227's test notes already specify (bare-name prefix + cross-server confinement) and the `scripts/smoke/phase-109k.sh:55-60` guard flipped from SKIP to OK. Note that the existing spec bakes in the current behaviour — `web/console/src/lib/chat/renderers/app-bridge-host.spec.ts:156-163` passes an ALREADY-qualified `'srv_echo'` and asserts verbatim pass-through — so a naive re-land turns it red; the case must be rewritten to pass a bare name and assert the qualified dispatch, and a cross-server case added beside it. There is no confinement test in the tree today. This is a handful of lines re-applying an Accepted decision; it needs **no** new `D-NNN`. *Consider whether the confinement belongs on the backend after all* — D-227 chose the frontend as minimal-surface, but the revert demonstrated that a frontend-only security property is one bad merge away from silently evaporating, and the smoke guard degraded to SKIP rather than failing. A backend `server_id` on `MCPAppCallToolRequest` (validated against the tool's `Source`) would be tamper-evident at the wire and gate-able in Go tests. That trade-off **is** a decision worth recording, and is the only genuinely new design content in this ask.
2. **A typed not-found** at the Protocol edge so an unresolvable tool returns `CodeNotFound` rather than `CodeRuntimeError` — either by adding the `"tool not found"` marker to `isMCPNotFound` (`internal/protocol/mcp.go:1099`) or, better, by classifying on the wrapped sentinel rather than on message text.
3. **`_meta.ui.visibility` enforcement at `tools/list`** — parse the field in `content.go` and exclude app-only tools from the planner-facing catalog view while keeping them reachable through the app-call proxy. This is additive and independent of items 1–2; if it is deferred, say so explicitly rather than leaving the field looking supported.

---

**RESOLVED (items 1–2) — phase 207 / D-351 (2026-07-25).**

- **Item 1 — the D-227 item 3 prefix is re-landed, and the placement question is SETTLED, not left open.** `createAppHandlers.oncalltool` now qualifies the app-supplied bare name through `qualifyAppToolName(serverID, name)` before dispatch. The prefix is UNCONDITIONAL, so `otherserver_drop_table` becomes `srv_otherserver_drop_table` and cannot resolve — the answer to question 3 above is now "no". The pre-baked spec case that asserted verbatim pass-through was rewritten (bare name in, qualified name dispatched) and a cross-server confinement case added beside it, exactly as this ask anticipated. **The prefix alone was NOT sufficient, and D-351 now says so.** `<sourceID>_<tool>` is a single-underscore join with neither side charset-constrained, so `github` + `github_enterprise` made it non-injective: an App on `github` asking for `enterprise_delete_repo` dispatched `github_enterprise_delete_repo` and reached the other server's tool, with the exposure gate evaluating THAT server's posture. Phase 207 added a registration-time guard (`mcp.Registry.CheckServerIDUnambiguous`, enforced inside `Register` under its write lock and pre-checked in `Attach`, both directions so order does not matter, covering boot-declared and runtime-attach) that refuses the ambiguous pairing outright. The honest claim is therefore: the qualifier confines an App to its own server GIVEN registration-time separator-safety. **On the frontend-vs-backend trade-off this ask raised:** the FRONTEND placement is re-affirmed FOR NOW — the boundary the control defends is App ↔ Console host, and a wire `server_id` would be supplied by the Console itself. But the stronger argument is NOT tamper-evidence: the Runtime can compare `desc.Tool.Source` EXACTLY, a check the Console structurally cannot perform because it can only manipulate strings. That argument is recorded as a follow-up in D-351 for a future wave to decide deliberately; it needs a Console-supplied wire `server_id` and is an architecture change, not a patch. The revert-fragility that motivated the original question is a PROCESS failure, and it is fixed as process: `scripts/smoke/phase-109k.sh` went from SKIP-tolerant greps to hard assertions pinning BOTH the qualifier's existence and its use in `oncalltool` (pinning only the helper would let a refactor strand it unused, which is how the property evaporated the first time), and a Go reflection test fails if `MCPAppCallToolRequest` ever grows a server-scope field.
- **Item 2 — the typed not-found landed, by SENTINEL rather than by marker.** Widening `isMCPNotFound`'s marker set was the first attempt and was discarded: the error chain carries a southbound server's text verbatim, so matching on it let a remote party mint a typed not-found out of a transport failure. The marker path is deleted. `internal/mcpconsole` translates the driver's not-found into `protocol.ErrAccessorNotFound` at the accessor boundary and the edge classifies by `errors.Is`, so an unresolvable app tool-call returns `CodeNotFound` instead of `CodeRuntimeError` / "MCP read failed"; the Console adapter maps that onto a typed `MCPAppToolNotFoundError` and the host re-raises it naming the BARE name the app asked for plus the server it is confined to. A transport-shaped failure still maps to `CodeRuntimeError` — both directions are pinned, including a laundering guard.
- **Item 3 — `_meta.ui.visibility` enforcement is DEFERRED, explicitly.** Still not parsed, still decorative. Phase 207 named the deferral in its Non-goals rather than leaving the field looking supported; it stays Filed here and is additive and independent of items 1–2 whenever it is picked up.

---

## HA-42 — progressive `tool-input-partial` into a rendered App (DEFERRED)

**Priority:** Low. **Size:** large. **State:** deferred — **already reserved as `D-343`**; not in scope for v1.22.

This ask is a pointer, not a new design. `docs/decisions.md` D-343 ("RESERVED: progressive `tool-input-partial` streaming into a rendered MCP App") records the deferral and the reason: the "app assembles as it streams" experience is not Console-only, because the runtime has no source for partial tool-call arguments. Do not re-litigate it as new work.

The two things it needs, per D-343 and confirmed in-tree:

1. **A new runtime `llm.toolcall.partial` streaming event** emitted at the LLM driver's fragment-assembly site. Today partial tool-call arguments are merged inside the driver by index and only the complete structured call leaves it; `internal/llm/events.go:290-299` shows `CompletionChunkPayload` carries `Delta` / `Kind` (content and reasoning deltas) and nothing else. This is a Go + Protocol-additive event with the usual `make protocol-ts-gen` / `make protocol-docs-gen` lockstep and a smoke assertion, followed by a thin Console relay onto the vendored bridge's partial-input send.
2. **A settled redaction posture, decided before any wire work.** Tool arguments routinely contain secrets — CLAUDE.md §5 forbids logging raw tool arguments, §7 item 7 forbids untyped tool arguments in audit payloads, and §13 forbids raw heavy content reaching the LLM edge. Putting arguments on the wire **per token** must therefore go through audit redaction and heavy-content handling **by construction**, not as a follow-up. A partial-fragment stream is the worst case for a redactor: a secret can straddle two fragments, so a fragment-at-a-time redactor can miss what a whole-value redactor would catch. That property has to be designed for, not discovered.

---

## HA-54 — deterministic MCP tool failures exhaust the retry budget unchanged

**Priority:** High (functional correctness and avoidable downstream load).
**Size:** contained (typed transport contract plus policy/transport tests; no
new tool protocol method).
**State:** Shipped (v1.27) — phase 236 / D-410; the planner-replay governance amendment below is Shipped (v1.28).

**Governance amendment — confirmed planner-replay gap (Shipped
v1.28).** A verification
review confirmed that the classified outcome must survive the runloop's step
recording and reach the actual next ReAct prompt: the typed class,
retry-policy outcome, bounded provider message, and retained bounded MCP
result content are preserved through step recording and appear in the next
ReAct prompt; a generic `Step.Error` never masks the richer classified
`LLMObservation`; legacy unstructured errors may render a generic safe
fallback; and canonical task/tool events agree with the planner observation
on the terminal error. The full-path acceptance runs `IsError` → typed
classification → retry policy → runloop → the next ReAct prompt end to end: a
permanent class invokes exactly once, a typed deterministic failure in the
`revision_conflict` shape carries the current revision for reread/retry, a
retryable provider failure uses the configured policy, and raw arguments or
secrets never reach the observation or the prompt. No new phase, decision,
HA, event, or Protocol method is allocated; HA-54 remains phase 236 / D-410.

**What the consumer sees.** A tool can reject an invalid request correctly,
yet Harbor repeats the identical request four times under the default tool
policy. Prototype Workbench is the second consumer that exposed the gap: a
missing selected-node identifier is a deterministic argument validation
failure, and an unknown edit operation is a deterministic tool-domain
failure. Neither can converge by resending the same arguments, but both are
currently retried as if a network dependency had flaked.

**Verified in-tree.**

- `internal/tools/drivers/mcp/content.go:344-349` states the current lowering
  rule directly: every MCP `CallToolResult` with `IsError == true` becomes
  `ErrMCPToolError` and reaches the policy classifier as retryable transient by
  default. The implementation has no typed branch: `:419-421` wraps only the
  rendered text body.
- `internal/tools/policy.go:83-107` makes the default retry set
  `transient`, `timeout`, and `5xx` with three retries, i.e. four invocations
  total. `ClassifyError` falls through to `transient` for an unrecognised
  error at `:495-550`.
- The existing MCP test deliberately proves the current behavior:
  `internal/tools/drivers/mcp/mcp_test.go:327-350` retries two `IsError`
  responses until the third succeeds. The mock fixtures call all `IsError`
  results transient at `internal/tools/drivers/mcp/mockserver_test.go:76-107`.
  There is no counterpart proving that a deterministic `IsError` gets one
  call.
- This is not a request to infer meaning from prose. The downstream result
  already has a standard MCP `IsError` signal, but its boolean shape cannot
  distinguish invalid arguments from a provider outage. Parsing rendered text
  would be brittle, locale-dependent, and unsafe as a retry contract.

**Requested shape.** Define a transport-safe, typed MCP error-classification
contract that an MCP server may attach to an `IsError: true` result while
remaining a normal MCP result for clients that do not understand the extra
classification. The exact extension placement/negotiation belongs in the
phase design, but it must preserve the standard `CallToolResult` semantics and
be carried through every Harbor MCP transport without a text parser or a
transport-specific side channel. A structured-content envelope with an
advertised schema is one candidate; a phase must compare it with any relevant
standard MCP extension before settling it.

The contract must express, at minimum:

1. `invalid_argument` / validation and deterministic tool-domain failures as
   **permanent for the unchanged invocation**. They reach the model as the
   original error result, but the retry shell performs exactly one attempt.
   This includes a missing required selector and an unknown closed operation;
   it does not silently coerce aliases.
2. Genuine transport and provider/service failures as retryable according to
   the configured policy. Existing timeout and 5xx behavior remains intact.
3. An explicit compatibility fallback for servers that return only ordinary
   `IsError` today, and for absent, malformed, or unrecognised classifications.
   The fallback must be documented and tested; it must not guess from text or
   turn a foreign extension into a permanent failure accidentally.
4. The classified result's bounded message/content and structured content are
   preserved for the planner/model and App paths. Classification metadata is
   control information, not a route to log raw tool arguments or results.

**Acceptance evidence for a future phase.**

- Real MCP SDK fixtures over each supported Harbor MCP transport demonstrate
  that typed `invalid_argument`, validation, and deterministic
  `tool_domain` `IsError` results make exactly one call under the default
  policy; their original result content remains observable.
- A typed retryable provider/transport failure still uses the configured
  attempt budget and can recover, while timeout and 5xx classifications retain
  their present policy behavior.
- Legacy unclassified `IsError`, missing classification, malformed
  classification, and an unknown future class follow the explicitly documented
  compatibility fallback. No test derives a class from error text.
- Unit coverage proves lowering, policy classification, and error wrapping;
  an end-to-end driver test proves the selected class reaches the retry shell
  without changing standard MCP result handling. The existing transient
  `IsError` retry case remains as the compatibility regression fixture.

**Non-goals.** This filed ask does not implement a retry policy override for
one server, redefine MCP's `isError` boolean, or add a Workbench-specific
exception. It asks Harbor to provide a reusable typed reliability seam for a
second consumer and every future MCP server.

---

## HA-57 — same-run step-tranche receipts resume the original live run

**Priority:** High (run integrity). **Size:** contained. **State:**
Shipped — phase 239 / D-418 (Harbor-internal filing).

**What the consumer sees.** A paused or interrupted run can continue from a
finite committed step-tranche receipt while the original run loop remains
live. The continuation preserves the identity quadruple and does not replay
completed work or create a replacement run. D-417 makes the fresh-process,
restart-unavailable boundary explicit.

**Requested shape.** Persist a bounded receipt for each same-run step tranche
and resume only the original live run at the last committed boundary. Verify
`(tenant,user,session,run)` and fail loudly for stale or unauthorized receipts.
If the original run is unavailable after process restart, return the typed
restart-unavailable result rather than relaunching from mutable configuration.
This ask does not add tool-failure events or a classifier surface.

**Required acceptance.**

1. Resume continues from the last committed tranche without replaying steps.
2. Repeated resume is idempotent; stale receipts fail loudly.
3. Resume verifies the identity quadruple and never accepts caller-selected
   replacement identity.
4. A fresh process returns typed restart-unavailable rather than relaunching a
   frozen run.
5. Concurrent same-run receipts under `-race` show no cross-run bleed and
   include a failure mode.

---

## HA-58 — governed virtual child profiles need one authoritative resolver

**Priority:** High (authority integrity). **Size:** medium. **State:** Shipped —
phase 240 / D-419 (Harbor-internal filing).

**What the consumer sees.** A governed virtual child profile is a read-only
view derived from a parent profile. It is not an isolation principal and cannot
mutate the parent, advance an independent revision, or widen capabilities.

**Requested shape.** Resolve the child from the parent through one resolver at
run-start and inspection. Admission uses verified `(tenant,user,session)`
authority and agent reach; `agent_id` and virtual-profile keys are not
identity axes. Bounded overrides are server-governed and read-only.

**Required acceptance.**

1. Run-start and inspection use the same resolver and real durable parent.
2. Ordinary callers cannot cross the verified identity triple; denied calls
   disclose no names.
3. Bounded overrides cannot widen capability, mutate the parent, or advance an
   independent revision.
4. The virtual child is never used as an isolation principal.
5. Concurrent mixed-tenant/user/session resolutions under `-race` show no
   authority bleed and include a failure mode.

---

## HA-59 — virtual-child execution needs bounded artifact forwarding

**Priority:** Medium (execution integrity). **Size:** medium.
**State:** Shipped — phase 241 / D-420, depends on 240 (Harbor-internal
filing).

**What the consumer sees.** Virtual-child execution can forward an authorized
artifact reference and bounded output to a same-run consumer while preserving
provenance, without exposing raw content or crossing session boundaries.

**Requested shape.** Create a virtual-child execution artifact and forward
declared outputs through authorized artifact references with preserved
provenance. Unauthorized, erased, cross-session, and cross-tenant references
fail closed before bytes are exposed. There is no CLI or Console
composition-preview consumer feature in this ask.

**Required acceptance.**

1. Authorized same-run consumers receive only declared references and bounded
   output with provenance.
2. Forbidden references fail closed before bytes are exposed.
3. Raw content is not duplicated into task rows, model context, or unrelated
   sessions.
4. An end-to-end integration test with real drivers covers identity
   propagation, at least one failure mode, and an N≥10 concurrent stress run.

---

## HA-60 — task progress needs a durable bounded projection

**Priority:** Medium (two halves disagree). **Size:** contained. **State:**
Shipped — phase 242 / D-421 (Harbor-internal filing).

**What the consumer sees.** Task progress is available as a durable, bounded
projection rather than a raw stream or second source of truth. It is scoped to
the verified identity triple and fenced by session lifecycle and erasure.

**Requested shape.** Add additive optional `progress_snapshot`, `virtual_key`,
and `virtual_label` fields to `TaskRow`, derived from the task source of
truth. Keep the snapshot bounded, enforce identity-scoped reads, and remove
the projection at session erasure. This phase is limited to the task-progress
projection.
**Required acceptance.**

1. `TaskRow` exposes the three additive optional fields without a protocol
   version bump.
2. Progress remains bounded and derived rather than becoming a second task
   state source.
3. Unknown and cross-identity reads return typed not-found without leakage.
4. Session lifecycle and erasure fences remove stale projections.
5. Real durable StateStore integration proves identity propagation, a failure
   mode, and cross-session isolation.

---

## HA-61 — verified-caller `SKILL.md` package import needs a two-phase validate/commit path

**Priority:** High (verified-caller skill authoring). **Size:** contained-to-medium.
**State:** Shipped (v1.28) — phase 243 / D-422 (framework-framed filing).

**What the verified caller sees.** Harbor already ships `artifacts.put`, the
path-safe `internal/skills/importer` pipeline, and the claim-free
`agent_config.user.skills.{list,upsert,delete}` family, but the importer is
reachable only from a trusted local filesystem/CLI path. A Protocol caller
can upload bytes and hand-author the smaller `AgentConfigSkillInput`, but
cannot ask Harbor to validate a complete package with a top-level
`SKILL.md`, review the normalized result and warnings, and then save exactly
that reviewed package as the verified caller's durable personal skill.
Reimplementing the importer on the caller side would create two validators and
force a Protocol client to become authoritative for a runtime entity.

**Requested shape.** Two-phase, identity-mandatory user-skill import —
provisionally `agent_config.user.skills.import_validate` and
`agent_config.user.skills.import_commit` — where a caller uploads a
bounded `.zip` package (or a single Markdown file) through the existing
`artifacts.put` method under its verified `(tenant,user,session)` and
receives an `ArtifactRef`. Validation accepts only that caller-owned artifact
ref plus the effective `agent_id` (neither tenant nor user is selectable
authority), applies the ONE existing importer/validator with archive
entry-count, expanded-byte, per-file and total-size ceilings and the path/
type/compression-bomb rejections, and returns the closed normalized review
plus bounded warnings and hashes — performing ZERO writes of any kind
(no proposal-ledger write): the full reviewed state is sealed into a bounded
opaque versioned proposal token. Commit
echoes the sealed token and reviewed hashes, re-derives identity and effective
Agent reach, rechecks policy and ceilings, re-resolves the exact immutable
artifact, forces `ScopeUser`/caller ownership server-side, and atomically
records the skill body plus membership in ONE conditional package write;
a moved revision, expired token,
changed package, unapproved replacement, lost reach, or policy revocation
fails before visible mutation, and response-loss retry is idempotent through
a token-derived commit ledger (durable idempotency state begins only in the
commit phase). Supporting files are copied into the durable package
representation (addressed by immutable `skillpkg://` references), never
dereferenced from the staging session's artifacts; a failed commit
compensates through the exact receipt, never a visible skill
pointing at missing content.

**Required acceptance.**

1. Against every shipped SkillStore and ArtifactStore driver, import a
   minimal `SKILL.md` and a package with supporting files, review, commit,
   start a run, and observe the skill only for the correct
   `(tenant,user,agent)`.
2. Prove byte/hash stability, explicit same-name replace, response-loss
   replay, and N>=100 concurrent imports under `-race`.
3. Refuse malformed frontmatter, missing steps, unknown sections,
   oversized/compression-bomb archives, absolute/traversal paths,
   duplicate/case-colliding entries, symlink/hardlink/device entries,
   dangling attachments, and MIME/extension lies.
4. Prove cross-tenant/user/session/agent guessed refs fail without
   enumeration; a second user cannot validate or commit the first user's
   package; membership/policy/Agent-reach revocation between validate and
   commit leaves no visible skill or orphaned membership.

**Framing.** This is a generic Protocol-caller surface over Harbor's
already-shipped skill importer and durable user-skill tier; Harbor's
implementation uses only its own interface and runtime vocabulary.

---

## HA-62 — draft-only personal-skill authoring needs an ordinary runtime tool

**Priority:** High (skill authoring UX). **Size:** contained.
**State:** Shipped (v1.28) — phase 244 / D-423, depends on 243 (framework-framed
filing).

**What the verified caller sees.** D-411's `agent_config.agent_packs.propose` is an
admin-scoped operator-pack authoring method paired with an admin commit. Its
underlying proposer is draft-only, structured, safety-wrapped, and rejects
authority-bearing fields, but an ordinary user cannot invoke that method and
must never receive an admin credential. A caller needs the familiar
conversational "create a skill" experience inside an ordinary run, while
final save remains an explicit user action through HA-61 — not a privileged
tool side effect.

**Requested shape.** A stock/config-declarable Harbor tool, provisionally
`skill_create_draft`, disabled by default and enabled per Agent by operator
policy. It runs inside the caller's normal authenticated run, derives
`(tenant,user,session,agent)` exclusively from run context, accepts only a
bounded authoring intent plus optional non-authorizing revision feedback,
reuses the configured safety-wrapped LLM and the closed proposer schema, and
validates the result with the same skill validator used by HA-61. It returns
a canonical `SKILL.md` draft as a caller-scoped Harbor artifact reference
plus bounded structured metadata — and does NOT call
`agent_packs.commit`, `user.skills.upsert`, or HA-61 commit; does not select
scope, user, tenant, audience, or publication; does not attach/expose
required tools; and does not silently install anything. If an ordinary
discovered/MCP tool can implement this exact contract using the runtime's
existing identity and artifact services, no new authority-bearing Protocol
method is needed; what must be canonical is the draft result shape, policy
gate, provenance, and compatibility with HA-61.

**Required acceptance.**

1. In a real run, two users of one Agent independently ask the tool for
   drafts; each receives a different caller-scoped artifact and can
   validate/commit only their own through HA-61.
2. A foreign tenant and guessed artifact/proposal ids fail without
   enumeration.
3. Disable the tool or revoke personal-skill policy/Agent reach between draft
   and import and prove commit fails with no mutation.
4. Prompt-inject every forbidden authority field and prove closed-schema
   rejection; declare unavailable tools and prove the draft may warn but the
   run tool set does not widen.
5. Exercise edit/re-draft, model refusal, timeout/cancellation,
   response-loss/replay, and N>=100 concurrent invocations under `-race`;
   prove no call path reaches an admin pack mutation and no draft
   auto-publishes or auto-installs.

**Framing.** This is the governed skill proposer's ordinary least-authority
tool surface inside a verified run.

---

## HA-63 — session catalog and inspection need a lifecycle-only projection

**Priority:** High (chat-open latency). **Size:** contained. **State:**
Shipped (v1.28) — phase 245 / D-424 (framework-framed filing). Linked dependency:
HA-64; both are required for the complete chat-open acceptance below.

**What the verified caller sees.** Opening a chat catalog or resolving one known
session needs only lifecycle metadata, but `sessions.list` and
`sessions.inspect` always run counter enrichment that scans as many as
10,000 events per session and then reports only a partial lower bound when
the scan truncates. A page of session rows can therefore trigger one
historical scan per returned row; an exact-session probe pays the same scan
merely to prove existence and obtain a title.

**Requested shape.** Add an explicit lifecycle projection selector to both
`sessions.list` and `sessions.inspect` (for example `projection: "lifecycle"`;
the spelling is not load-bearing). It returns only session id, lifecycle
status, title and title source, start/update/completion/last-activity
timestamps where authoritative, duration where derivable without enrichment,
and the effective/default agent id only where Harbor can represent it
honestly. The lifecycle path MUST NOT invoke event-history, task, pause,
artifact, App, or counter enrichment. Counter fields use the closed
availability state `current | partial | not_requested | unavailable`: the
lifecycle shape marks them `not_requested` (never merely absent);
`unavailable` means enrichment or projection unavailable; `partial` remains
a lower bound; full counters are exact at `current`; an omitted selector
defaults to full. Zero may not mean "not computed." Filters and sorting
over lifecycle fields retain their existing semantics; a counter-dependent
filter or sort paired with the lifecycle projection fails as a typed invalid
request rather than silently switching to the expensive projection. Existing
full projection behavior remains compatible and explicitly selectable.

**Required acceptance.**

1. With a real durable session containing more than 100,000 events,
   lifecycle list and inspect perform zero history-replayer/enricher reads
   and work remains bounded by the page size, before and after restart.
2. A page of N session rows does not run N counter scans; existing full
   reads still return exact/partial counters with their present honesty
   contract.
3. Cursor, lifecycle filter, and lifecycle sort behavior is stable;
   incompatible counter filters fail typed and do not fall back to
   enrichment.
4. Same-user, foreign-user, cross-tenant, signed-session-reach, admin/fleet,
   and erased-session cases pass on every durable driver; cross-identity
   denial does not become an existence oracle.
5. Protocol manifest, generated clients, docs, and Harbor's own chat
   catalog (the Console) use the new projection.

---

## HA-64 — durable tail-paged conversation turns are needed for chat open

**Priority:** High (chat-open latency and replay correctness). **Size:** large.
**State:** Shipped (v1.28) — phase 246 / D-425 (framework-framed filing). The
v1.29.4 idle-convergence and v1.29.5 lost-wake corrections are shipped; linked
dependency: HA-63.

**v1.29.4 correction.** Durable turn materialization now advances through a
fully examined current source watermark after every returned canonical event;
persisted bus-internal notices and fenced-session tails no longer repeat
global `state_records` prefix scans on an idle runtime. Concurrent final-fence
filtering preserves the page's pre-filter overflow proof, so a truncated page
cannot promote its checkpoint past a later canonical event.

**v1.29.5 correction.** The turns materializer's lost-wake fallback for
deferred terminal snapshots now polls every 30 seconds rather than every 2
seconds. Durable event watches remain the fast path, and the bounded fallback
still catches up when the source watermark is ahead or a deferred terminal
snapshot needs reconciliation; an idle durable runtime now performs two
source-watermark reads per minute instead of thirty.

**v1.29.5 release evidence.** Implementation PR #735 merged at
`f0cd36b0c82f2332df575a5434b1a3e7a0d7a586`; hosted CI run `32628090701`
attempt 2 completed successfully on the exact reviewed implementation head
`280518aa36628ec602b668ea3b22fde1c082585f`, and documentation run
`32628090829` also completed successfully. The immutable annotated `v1.29.5`
tag object is `8aba749eadfc0919668bf0769796d26793181ba6` and peels to
`8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`; release workflow `32635519880`
succeeded, publishing 13 assets with verified aggregate and six sidecar
checksums and six GitHub attestations at the [GitHub release](https://github.com/hurtener/Harbor/releases/tag/v1.29.5).
The native darwin/arm64 artifact reports Harbor v1.29.5, Protocol 0.1.0,
build `8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`; module provenance records
`Sum=h1:iD6KARsZ3yWkLoiQeHvdCoLpEtQ0T9F8deC169S6280=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`, and
`Origin.Ref=refs/tags/v1.29.5`. Post-tag scaffold pin and golden cleanup are
complete. Focused local `go test ./cmd/harbor -run TestScaffold_Golden`,
`make drift-audit`, `make markdownlint`, and `make docs` passed; local
`make preflight` was not run and no downstream runtime, fleet, or database
mutation is claimed.

**What the verified caller sees.** Harbor has authoritative task rows/results and
raw event history, but no ready-to-render conversation page. One Protocol
consumer must enumerate every `tasks.list` page, locally sort the full
history, then issue `tasks.get` plus separately paged `events.list` reads for
each of 60 visible turns. Harbor's own Console similarly tail-pages raw state
history and performs a task lookup for query/timing. Initial render therefore
grows with total history, uses N+1 calls, and reconstructs runtime state from
forensic events; if event enrichment fails, a valid answer can survive while
its Activity and durable MCP App reference disappear.

**Requested shape.** Add a dedicated session-centric read model rather than
making callers join task/result/event/App authority themselves:
`sessions.turns.list` returns a stable tail page of conversation turns;
`sessions.turns.get` returns the same turn shape for one `(session, task)`
and is the bounded terminal reconciliation read after live streaming. These
two are the named public methods. Bounded Activity rides inline covering at
least Harbor's configured per-turn tool-call budget; a separate named
activity method is NOT a v1.28 method or acceptance — if the Protocol
response ceiling forces the exact attachment contract, a future named
fallback is recorded as a deferred follow-up. The
list request carries
`session_id`, an opaque exclusive older-page cursor, `limit` (default 20,
maximum 50), and the authorized projection; the response carries a session
header, snapshot/as-of identifier, page turns in a declared stable order,
`next_older_cursor`, `has_more`, optional remaining-older count with
`count_exact`, a live resume cursor, and page completeness/partial reasons.
The opaque cursor is snapshot/keyset anchored with an immutable task/turn
tie-breaker; appending a new turn while paging older history produces neither
duplicates nor omissions, and invalid/foreign/retention-expired/snapshot-
expired cursors have distinct typed outcomes. Backend work is proportional to
page size, independent of total event/turn cardinality, with a bounded
constant number of storage operations, no full task enumeration, no
request-path raw event scan, and no per-row reads. Each row is one root
foreground user turn carrying everything needed to render the current chat
without another task or event request: authoritative query and input
timestamp, ordered input attachment metadata, a closed status enum with
timestamps/duration and a bounded terminal reason, an explicit answer union
(inline bounded result OR artifact reference, or `empty`/`evicted`/
`unavailable`), running-turn content snapshots tied to the same turn version
and `last_applied_event_sequence` as the live-resume cursor, ordered
consumer-safe reasoning steps, ordered tool activity entries with a shared
monotonic Activity sequence and compact cardinality-capped totals, model and
token/cost usage with availability, consumer-safe intervention metadata (the
opaque action token only for a caller satisfying the pause's
resume/approval/control tier), durable ordered MCP App references whose
replacement identity is exactly `(effective_agent_id, server_id,
resource_uri)`, and per-component exact/partial/unavailable state. The read
model is runtime-owned and derived from Harbor's task, result, event, and
App-context authority; it is incrementally materialized with idempotent
sequence checkpoints, reconciles after interruption, survives restart on
durable drivers, and is erased/fenced with its session. `complete`,
`partial`, `rebuilding`, `retention_gap`, `evicted`, and `unavailable` are
distinguishable, and a missing/stale projection never triggers an unbounded
synchronous event rebuild during chat open. The snapshot-to-live handoff is
page-before-subscribe: the consumer folds the durable page and establishes
bounded running/paused membership FIRST, then opens the EventSource with
`live_resume_seq` as the initial `resume_seq`; the server replays events
strictly newer than that snapshot through the existing bounded replay source,
and a browser reconnect `Last-Event-ID` takes precedence (one terminal event
causes one
`sessions.turns.get` terminal reconciliation). Consumer versus operator is a
hard boundary: the `conversation` projection returns query, answer/
ref, consumer-safe reasoning/activity, own pause state, App refs, and compact
totals and must never return raw args/results/events, credentials, system
prompt, or provider stack; the operator `operations` projection is not part
of this ask, and no content-read/impersonation authority is requested.

**Complete chat-hydration acceptance (HA-63 + HA-64).**

1. A persisted session with more than 100,000 events, at least 10,000 turns,
   and one turn with more than 100 tool calls reopens its latest 20 turns —
   including the newest running or paused turn — with exactly one HA-63
   lifecycle read plus one HA-64 turn-page read; the critical path performs
   zero raw history scans, zero full task enumerations, zero per-turn
   `tasks.get`, and zero per-turn `events.list`.
2. The same renderable message skeletons, every inline answer, ordered
   Activity, usage, terminal cause, and ordered App refs are returned before
   and after durable-driver restart.
3. Reopening a newest running or paused turn preserves its mutable version
   and later converges to the sealed terminal row; older paging has no
   skip/duplicate while a new turn starts; page/live handoff loses or
   duplicates zero reasoning chunks, lifecycle changes, or App refs.
4. Projector replay/restart is idempotent; retention gaps, evicted
   answer/context, partial ordered collections, and rebuilding states are
   honest and never become exact empty/zero values; session erasure makes the
   projection unrecoverable.
5. Same-user/session-reach, foreign-user, cross-tenant, erased-session,
   admin, and fleet cases run across every production durable driver,
   including N>=100 concurrent mixed identities under `-race`; a paused owner
   without the required approval/control tier receives no action token and
   cannot resume.
6. Wire manifest, generated clients, capability/version discovery, protocol
   docs, and Harbor's own chat surface (the Console) land with the methods.
   The fallback path may use raw reads only as an explicit
   degraded/forensic action, never a silent normal-open path.

**What remains available.** `events.list` / `state.history` remain raw
forensic drill-down; `tasks.get` remains explicit task detail; live SSE
remains the narrow stream; lazy App resource/context and artifact-byte reads
remain separate. No shadow transcript or summary store is introduced.

---

## HA-65 — administrative observability queries need a rebuildable rollup projection

**Priority:** High (confirmed operational scalability gap). **Size:** large.
**State:** Shipped (v1.28) — phase 247 / D-426 (framework-framed filing). The
v1.29.4 idle-rollup correction is shipped.

**v1.29.4 correction.** Rollup lost-wake polling now compares the cheap source
watermark with the durable projection checkpoint before opening a bounded
source page. A current idle projection performs no global event-head scan;
stale and catching-up projections retain the existing bounded replay and
explicit completeness semantics.

**What the verified caller sees.** An active session can exceed Harbor's bounded
10,000-event session-counter scan. At that point `sessions.list` and
`sessions.inspect` warn that per-session scans are truncated and counters are
a lower bound. The Runtime remains healthy, but `events_count`,
`total_cost_cents`, and `total_tokens` become lower bounds, and the Console
cannot answer basic administrative questions efficiently or exactly: cost by
tenant/user/session/model, token usage dimensions, successful LLM
completion counts, task outcome counts, latency and
cost trends over a period, and most-expensive breakdowns — without
replaying the raw event log. The session rollup scans at most 10,000 events
per visible session row (`internal/sessions/protocol/enricher.go`), D-309
chose read-time enrichment with bounded-scan truncation as an honesty
requirement, `events.aggregate` has a separate 100,000-event bound and
aggregates event-type counts only, `llm.cost.recorded` already carries the
identity quadruple plus model/cost/token/latency for successful driver-level
completions, task events already distinguish outcomes, the generic StateStore
is not an indexed analytics interface, and OTel metrics intentionally forbid
identity-derived labels. D-296 currently rejects Harbor becoming a counters/
metrics TSDB or keeping a shadow history store.

**Requested shape.** A first-class durable observability projection behind
its own typed interface and §4.4 driver seam (in-memory, SQLite, Postgres),
consuming canonical Harbor outcomes incrementally, storing aggregate rows
rather than duplicating raw event payloads, rebuildable from the durable
event log, supporting indexed administrative queries without scanning the
event log or every session, preserving `(tenant, user, session)` isolation
and server-derived admin widening, and exposing freshness/completeness
explicitly rather than returning plausible stale or partial totals. Storage
base grain is exactly the fixed UTC MINUTE bucket plus the authoritative
dimensions
`(tenant_id, user_id, session_id, model)`; `agent_id` is not a rollup
dimension (not even conditionally), and a query may coarsen the bucket.
Measures are existing source-backed payloads only
— the `llm.cost.recorded` successful-completion count, exact integer/decimal
cost, prompt/completion/reasoning/cache-read/cache-write/total tokens,
latency count/sum/min/max, and task completed/failed/cancelled counts.
Attempts, failed LLM calls, retry/downgrade, task-spawned, and user-message
counts are unsupported and reported unavailable — never mandated, inferred,
or backed by new canonical events; unsupported
measures are omitted or marked unavailable, never synthesized. Each applied source event
needs a durable idempotency identity (the existing local durable event
sequence) with a durable applied-through cursor/watermark; restart catch-up
and full rebuild behavior are specified; every query response carries an
observed watermark/freshness stamp and an explicit completeness state
(`current`, `catching_up`, `rebuilding`, or `unavailable`), and an
unavailable projection never falls back silently to zeros. ONE Protocol-owned
administrative query surface (`observability.query`) supports a mandatory
time window,
server-authorized filters, a closed `group_by` set, bounded bucket sizes,
pagination with deterministic sorting, exact or explicitly partial results,
and a maximum result/bucket budget that fails loudly; the fail-loud LLM
publication contract is unchanged and projection application failures are
best-effort. Ordinary callers query
only their authorized identity scope, and cross-identity queries require
verified admin or `console:fleet` authority from the request context with the
established widened-read audit evidence. The Console remains a pure Protocol
client. The projection participates in deletion semantics: session erasure
removes or tombstones every aggregate attributable to that session and
reconciles parent user/tenant totals; retention policy and the rebuildable
event-log horizon are explicit; a rebuild over a pruned log exposes that
historical incompleteness. The existing session enricher either reads this
projection or remains an honest fallback.

**Required acceptance.**

1. A session emits more than 10,000 events; session and admin usage queries
   still return exact projection-backed totals without `counters_partial`
   and without scanning those events at read time.
2. Cost and all supported token dimensions reconcile exactly with canonical
   `llm.cost.recorded` fixtures, including sub-cent calls and cache-token
   fields, under the best-effort contract.
3. Queries group correctly by tenant, user, session, and model across
   multiple users, concurrent sessions, and models, with no identity bleed.
4. Successful LLM completions and task completed/failed/cancelled counts are
   distinct measures backed by canonical source events; attempts, failed LLM
   calls, retry/downgrade, task-spawned, and user-message counts are
   unsupported and reported unavailable, never synthesized.
5. Replaying the same source event is idempotent; restart catch-up, crash
   between source persistence and projection application, and concurrent
   replica application do not lose or double-count values.
6. Projection failure and rebuild states are visible through health/query
   responses; a query never returns zero as a substitute for "projection
   unavailable," and the session enricher falls back honestly.
7. A verified fleet caller can run widened grouped queries and produces
   exactly the required audit evidence; an ordinary caller cannot enumerate
   another user, session, or tenant.
8. Session erasure removes the session's rows and reconciles every
   higher-level grouping; rebuild does not resurrect erased aggregates.
9. SQLite and Postgres query plans use bounded indexed access for the
   supported filters/groupings, with a large fixture proving query work is
   independent of the total raw-event count.
10. D-296 is explicitly amended or superseded: the decision must explain why
    this rebuildable projection is allowed while a general-purpose Harbor
    TSDB and identity-labelled OTel metrics remain rejected.

**v1.29.4 release evidence.** Implementation PR #733 merged at
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; hosted candidate run
`32620015889` completed successfully, including the live preflight,
PostgreSQL conformance, both Go platforms, Playwright, isolation, leak,
chaos, lint, docs, and examples. The immutable annotated `v1.29.4` tag
object is `d85ca3928171cbf5c72e890f7c4b622e4b2cf1ff` and peels to
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; release workflow `32622414573`
succeeded, publishing [13 release assets](https://github.com/hurtener/Harbor/releases/tag/v1.29.4)
with verified aggregate `checksums.txt`, six sidecar checksums, and six
GitHub attestations. The native darwin/arm64 artifact reports Harbor v1.29.4,
Protocol 0.1.0, build `90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; module
provenance records `Sum=h1:GNQ902D6ddXlYtiOmC+wGMN7LSbE7VQilFb5HggKUyU=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=90f5f8ce96f83f994462e33cdfeccc77c535ca7e`, and
`Origin.Ref=refs/tags/v1.29.4`. The post-tag scaffold pin and golden
fixtures are complete. Focused local
`go test ./cmd/harbor -run TestScaffold_Golden` and `make drift-audit`
passed; local `make preflight` was not run. No downstream runtime, fleet,
or database mutation is claimed.

---

## HA-66 — the resolved boot/default agent needs a boot-declared resource-free operator skill baseline

**Priority:** Medium (operator deployment posture). **Size:** contained.
**State:** Shipped (v1.28) — phase 248 / D-427 (Harbor-internal filing).

**What the operator sees.** Harbor's operator skill tier (D-411) is durable
and per-agent, and D-414's composition preview reads that durable state. But
a runtime serving its resolved boot/default agent has no config-file-declared
baseline: the boot agent's skill composition is empty until an operator
stands up the full durable pack workflow, and the preview surface cannot show
a baseline that was never declared anywhere. A deployment that wants a small,
immutable, operator-owned skill set on its default agent before any durable
pack membership exists has no path.

**Requested shape.** A boot-declared, resource-free operator skill baseline
for the resolved boot/default agent: a config-file-relative strict eager
immutable loader that runs before readiness — include root is the config
file's own directory (never the CWD); each include is one relative directory
with one case-sensitive top-level regular UTF-8 `SKILL.md` (resource-free, no
support-file references); traversal, recursive discovery,
symlink/hardlink/special entries, duplicates, and canonical-name collisions
are rejected under declaration/item/file/aggregate bounds — validates every
entry through
the ONE existing importer/validator (fail-loud otherwise), and eagerly copies
and freezes the set for the process lifetime (restart-only). The loader
performs no persistence, exposes no
admin verbs, advances no config revision, and materializes no lifecycle
record; the durable Postgres `${SKILLS_DSN}` `boot_agent_packs` schema
persists agent revisions and personal state while the boot config stays
node-local and reconstructed, with no convergence claim. The baseline binds
exactly to the resolved `(tenant, boot_agent_id)`
pair — never a placeholder or wildcard, never an invented boot identity — and
merges with the agent's active durable operator-pack revision into ONE
combined operator tier FIRST (same canonical name + same semantic hash
dedupes as `source=both`; differing hash fails; exactly 256 unique combined
items), pre-reading every declared tenant-agent active revision before
readiness and retaining the run-start conflict defense; the combined tier
applies LAST over base/user/session skills (operator-tier-last rule).
Boot-declared entries are boot-owned:
`upsert` and every proposal commit (replay/prepared/publish) and
rollback/activation reject a boot-owned canonical name even at equal hash;
removal may delete an actual legacy durable revision shadow and leave boot;
a boot-only remove is a typed read-only refusal, never false success;
`agent_packs.list` remains durable-revision authoring only. Config removal
removes boot only on the next deployment; a legacy durable revision remains;
in-flight snapshots retain captured bytes and hash. A deterministic set hash
over the normalized baseline entries
rides the run snapshot and the composition preview so an operator can verify
exactly what the boot agent composes. The shipped run path is: the eager
immutable index opened before readiness is handed to the run-loop driver, an
exact `(tenant, effective-boot-agent)` run-start membership lookup binds the
frozen boot entries into the run snapshot, and the concrete resolver's
strict combined operator tier is frozen into the snapshot with the boot set
hash, the combined hash, and per-item `boot|revision|both` provenance.
Production and devstack use the single
loader path. Headless `RunOnce` is explicitly unsupported and fails loud when
`boot_agent_packs` is configured. Because
D-414's preview is absent/incomplete on this base, the phase delivers ONE
shared strict effective-composition resolver + preview that includes the
baseline, used by boot preflight, run, and preview alike, plus the read-only
Protocol path (clients, manifest, generated docs), minimal Console and CLI
consumers (D-415), config docs and example, operator skill, and smoke; the
preview shows `boot|revision|both` and `boot_pack_set_hash` under
authority/reach gating with no lifecycle materialization.
`EnsureBootAgentLifecycle` is separate and
unchanged and may write a revision; the baseline loader and composer
themselves perform zero persistence, zero admin pack verbs, zero lifecycle
writes, and zero config revisions, and the
phase never claims startup performs no revision writes whatsoever.

**Required acceptance.**

1. A config-file-declared baseline loads eagerly, copies, and freezes before
   readiness and appears in the composition preview for the resolved boot
   agent alongside the D-414 durable/personal tiers — one strict resolver,
   one preview — reporting `boot|revision|both` and `boot_pack_set_hash`
   under authority/reach gating.
2. A malformed, unresolvable, or un-importable baseline entry fails the boot
   loud before readiness; an unresolvable default agent fails loud rather than
   inventing a boot identity; headless `RunOnce` fails loud when
   `boot_agent_packs` is configured.
3. The loader/composer performs zero durable writes, zero admin pack verbs,
   zero lifecycle writes, and zero config revisions (skill rows, config
   revisions, lifecycle records, admin verbs) — asserted by a not-invoked
   spy / store idempotence, not timing; `EnsureBootAgentLifecycle` remains
   separate, unchanged, and may write a revision.
4. The baseline binds exactly to the resolved `(tenant, boot_agent_id)`;
   other tenants and non-default agents never compose it.
5. The baseline merges with the active durable operator-pack revision into
   ONE combined operator tier FIRST — same canonical name + same semantic
   hash dedupes as `source=both`; differing hash fails; exactly 256 unique
   combined items; every declared tenant-agent active revision pre-read
   before readiness; run-start conflict defense retained — applied LAST over
   base/user/session skills.
6. Protocol mutation/removal verbs refuse every boot-declared baseline name
   with a canonical typed error and no partial effect: upsert and every
   proposal commit and rollback/activation reject even equal hash; removal
   may delete an actual legacy durable revision shadow while leaving boot; a
   boot-only remove is a typed read-only refusal; `agent_packs.list` remains
   durable-revision authoring only.
7. The deterministic set hash is stable across restarts for an unchanged
   config file and appears in the run snapshot and the preview; config
   removal removes boot only on the next deployment, and a legacy durable
   revision remains.
8. Production and the devstack resolve the same loader path; the devstack's
   synthetic boot agent composes the baseline exactly like production.
9. Required-tool validation applies only after the static catalog/policy
   wrapping and against the granted-scope ceiling, with no invented identity;
   the read-only preview Protocol path, clients/manifest/generated docs,
   minimal Console and CLI consumers (D-415), config docs/example, operator
   skill, and smoke ship with the phase.
10. N>=100 concurrent mixed-run compositions under `-race` against one shared
    resolver show no context bleed, no cancellation cross-talk, no goroutine
    leak, and byte-identical snapshots for identical inputs, with identity,
    reach, and retirement gates included.

---

## HA-67 — optional MCP artifact-egress mapping parameters

**Priority:** Medium. **Size:** small. **State:** Shipped as Phase 249 / D-429
in the unreleased candidate; focused evidence is recorded, while broad local
preflight is intentionally not claimed.

**Observed Harbor gap.** The existing MCP artifact-egress mapping required an
artifact reference for every mapped remote property. A caller that legitimately
omits one image or file could not express that omission without either sending
a placeholder or weakening the supplied-reference validation path.

**Required shape.** One trailing `?` marks a mapping parameter optional at the
shared compiler boundary. The compiler strips it before remote schema checks
and `ParamsFor`, so a server sees the bare property name. Missing or JSON
`null` optional values skip substitution; present values still require a
string, a non-empty id, and successful identity-scoped resolution with the
existing digest/encoding/ceiling behavior. Required mappings remain unchanged.

**Evidence and guardrails.** Unit and MCP adapter tests cover marker parsing,
duplicate rejection, missing/null skip, present-value refusal/success, and
N=128 concurrent reuse. This is an internal mapping/config contract: it adds
no Protocol route, wire type, artifact resolver, or broad capability.

---

## HA-68 — same-runtime organization skill publications

**Priority:** High. **Size:** medium. **State:** Implemented as Phase 250 / D-430;
publication domain, StateStore contract, canonical wire types/methods/errors,
strict transport/client/capability/generated-doc lockstep, exact run-start
composition, shared production/devstack bootstrap, and the shared StateStore
publication conformance harness are landed. The harness covers in-memory and
SQLite locally and Postgres under `HARBOR_PG_DSN`; with that DSN unset, the
local Postgres leg is skipped. Focused local evidence covers configured and
unavailable wiring postures; broad preflight/full suites were not run locally
and hosted CI evidence remains pending. This register does not claim
downstream acceptance.

**Observed Harbor gap.** Ordinary skill rows are identity-scoped to the
caller. An organization needs one reviewed revision to be available to users
with signed reach to a selected agent, but copying rows across user triples,
using a shared principal, or interpreting a broad scope label would weaken
isolation and make revocation ambiguous.

**Required shape.** Organization publications are immutable,
content-addressed revisions with `active|retired` lifecycle, exact generation
and hash CAS, and content-free metadata, references, and receipts. Admin
methods publish/list/get/publish-successor/retire. Caller methods
available/install/update/remove/references.list require verified identity and
signed effective-agent reach. A durable reference pins publication id,
revision id, generation, content hash, and runtime/deployment id for the
tenant/user/agent; only the authorized same-runtime resolver returns the body.

**Required guardrails.** StateStore `SaveIf` and idempotency provide one-winner
transitions and response-loss replay. Retired, stale, foreign-runtime,
wrong-hash, and unreachable references fail closed. Publication bodies never
appear in metadata, receipts, events, audit payloads, logs, or model-visible
discovery. The phase adds no new driver, migration, global catalog, portable
reference, isolation principal, or Protocol-version bump. Focused Phase 250
implementation tests cover exact run-start resolution, runtime binding,
configured/unavailable production and devstack mounts, and failure behavior.
Broad release gates remain outside this local evidence and hosted CI is
pending; this ask's register entry does not claim downstream acceptance.

---

## HA-69 — v1.29.1 event-index and six-store PostgreSQL fleet safety

**Priority:** Release blocker. **Size:** large. **State:** Planned — Phase 251
/ D-431. This is one emergency hotfix ask with an event-read leg and a
PostgreSQL fleet-safety leg; the latter is not complete if it omits any
Harbor-owned PostgreSQL projection.

**Observed production failure.** The shared Render PostgreSQL cluster reports
`max_connections=103`, 96 occupied connections at the incident sample, 72
idle, 0 active, 0 idle-in-transaction, and `idle_session_timeout=0`.
PgBouncer port 6432 returns SQLSTATE 53300 (`too many clients`). Released
v1.29.0 opens independent pools for state, memory, artifacts, skills,
sessions/turns, and observability/rollups; each historically allowed 25 open,
5 idle, and a 5-minute lifetime with no finite idle-time budget. Graceful
shutdown closes stores, so this is steady-state pool multiplication/retention,
not merely a missing close. The v1.29.1 default must fit the existing
Basic-4GB instance and must not require a Render plan upgrade.

**Leg A — bounded durable event reads.** The released durable event path can
load all 25,000 event bodies for a sparse 168-match or zero-match one-hour
window, and the session counter enrichment repeats the class per visible row.
The fix is a first-class exact metadata index over the canonical durable
sequence. It selects identity/type/time/cursor candidates before payload load,
preserves D-294/D-305 filters, audit, redaction, erasure, cursor, and honest
partial/truncation semantics, and uses atomic publication or a readiness
watermark. Existing rows require crash-safe idempotent backfill and catch-up;
no stale index may be treated as an empty history. Rollups consume the same
canonical sequence and remain D-426 best-effort derived state.

**Leg B — six-store runtime budget and migration identity.** All six stores —
state, memory, artifacts, skills, sessions/turns, and observability/rollups —
participate in one runtime-owned PostgreSQL pool/migration registry. Equal
canonical DSNs share one `*sql.DB` closed once; distinct DSNs remain a valid
stage-one topology but share the runtime-wide aggregate permits. The
documented default is one logical database per runtime, with consolidation
optional at first hotfix boot and performed one runtime at a time.

The operator fields are `postgres.pool.max_open`, `postgres.pool.max_idle`,
`postgres.pool.conn_max_lifetime`, and `postgres.pool.conn_max_idle_time`.
Defaults are 3 aggregate opens, 1 idle, 5m lifetime, and 30s finite idle-time.
The six direct migration sessions in the budget are an operator/orchestrator
rollout ceiling, not a Harbor runtime configuration field. The
worst planned overlap is 9 runtimes × 2 generations × 3 = 54, plus 6 direct
5432 apply sessions, 12 reserved for Pengui/capabilities, and a 25-connection
operator reserve: 97 of 103, leaving 6 below the hard cap. Steady state is
70. A deterministic nine-runtime accounting test rejects over-budget
topologies and exercises same-DSN and distinct-DSN deployments.

Migration apply is direct/session-affine PostgreSQL on 5432; steady ordinary
traffic and read-only verify may use transaction-pooled PgBouncer on 6432.
Applying through an unproven transaction pool fails loudly. Each ledger and
lock is namespaced and checksum-bound to subsystem, migration filename, and
version. Verify proves the expected ledger and required schema objects.
Correctly-shaped legacy stores may be adopted only after schema/checksum
inspection. The exact false-readiness fixture — `version=1` plus
`state_records`, no `memory_state`, and memory `migration_mode=verify` — must
fail with expected subsystem, observed tables/ledger, and remediation. Split
sources are classified by actual schema rather than DSN/env names; the
MPR/TAA-like wrong memory sources are treated as empty/misprovisioned unless
stronger evidence identifies preserved data.

**Cutover and evidence.** A dry-run/non-destructive tool or exact procedure
freezes or drains writes, applies destination migrations directly, copies all
six compatible projections, and emits source/destination row counts plus
canonical content hashes. It reconciles state, memory, artifact bodies,
skill revisions, identities, receipts, durable turn ordering/cursors/
activity/usage, and rollup watermarks before switching one runtime to the
unified DSN and 6432 verify. Harbor never deletes or reconfigures fleet
databases; old sources remain available for rollback and independent operator
removal.

**Release evidence required.** The ask is complete only after focused tests,
real PostgreSQL all-six boot/restart/idempotence, shared-pool cap/close-once,
separate-DSN compatibility, migration identity/adversarial tests, cutover
reconciliation, race/PgBouncer tests where available, hosted CI, two
independent Terra High reviews, immutable v1.29.1 tag/release/provenance and
checksums, post-tag version pin/cleanup, and a parent handoff containing the
exact fields, commands, compatibility notes, tag commit, and checksums. Local
preflight is deferred to hosted CI by emergency instruction and must not be
reported as passed.

### HA-69 v1.29.2 compatibility extension — Phase 252 / D-432

**State:** Shipped in v1.29.2. This extends the existing HA-69 handle; no new
Harbor ask identifier was allocated.

The first drained upgrade canary exposed a v1.29.1 legacy-backfill
compatibility defect. Durable heads and entries are keyed by the session
triple with `RunID=""`, while ordinary persisted event bodies retain their
real, non-empty RunIDs. The backfill check must validate exact storage
identity/kind, sequence, and tenant/user/session identity, but must not compare
the body RunID with the intentionally empty storage-key RunID.

The v1.29.2 extension keeps canonical event bodies authoritative and preserves
RunIDs in metadata, `events.list` metadata reads, and full event reads. It
refuses malformed/unknown bodies, sequence or tenant/user/session mismatches,
wrong returned StateRecord identity/kind, stale metadata/body divergence, and
checksum failures. Its regression fixture uses a v1.29.0-shaped multi-run
session head, proves restart/checksum repair and RunID filters, and runs
through the real PostgreSQL StateStore driver under `HARBOR_PG_DSN`.
The existing hosted `state-postgres` job selects the exact test and rejects
skip/no-test output; local absence of the DSN is an honest skip. Harbor does
not mutate the downstream fleet or its databases. Implementation PR #728
merged as `bbc058e6dcfa30b0903f5546d394bcf2860ba836`; hosted candidate run
`32582607218` completed successfully, including exact real PostgreSQL
StateStore acceptance and live preflight. The immutable annotated `v1.29.2`
tag object is `ebe0a907b92a745887fa469bb6e62cd018c53062` and peels to
`bbc058e6dcfa30b0903f5546d394bcf2860ba836`; release workflow `32584633258`
succeeded, publishing 13 assets with verified aggregate/sidecar checksums and
six attestations at the [GitHub release](https://github.com/hurtener/Harbor/releases/tag/v1.29.2).
Post-tag scaffold
pin/golden cleanup is complete. Local `make preflight` was never run and no
downstream fleet cutover is claimed.

### HA-69 v1.29.3 compatibility extension — Phase 253 / D-433

**State:** Shipped in v1.29.3; the compatibility extension is closed. This
extends the existing HA-69 handle; no new Harbor ask identifier was allocated.

The next legacy-head audit found a portable integrity condition that v1.29.2
must continue to reject at boot but did not yet expose a Harbor-owned repair
path for: redundant sequence references in an otherwise attributable durable
head. Generic inventory reports approximately 4,500 legacy heads and 89
duplicated sequence values / redundant references. The scale makes manual
rewrites, payload logging, or implicit best-effort healing unsafe.

The required surface is an offline `harbor events repair-legacy-heads`
command. Inspect/dry-run is the default and is content-free: only affected
head counts, duplicate counts and positions, stable record identifiers or
hashes, generations, immutable entry hashes, and outcome may be printed or
persisted. Apply requires the event writer to be stopped and an explicit
freeze/drain acknowledgement. PostgreSQL mutation requires a direct,
session-affine `5432` endpoint and refuses URL/keyword PgBouncer or `6432`
transaction-pool forms before opening a write handle.

The scan uses the StateStore bounded-enumeration contract and reads at most
`max-heads+1` records before refusing an oversized inventory; it is
cancellable and never materializes all head bodies solely to hash them.

Only exact, unambiguous duplicates are repairable. Every occurrence must point
to the same immutable entry slot and validate kind, decoded sequence, storage
tenant/user/session identity, event identity triple, and event type; existing
metadata must agree exactly. The v1.29.2 storage-key exception remains valid:
`RunID=""` on a session-scoped key does not conflict with a non-empty payload
RunID, which remains authoritative. Missing entries, body/metadata mismatch,
identity or sequence mismatch, malformed data, non-canonical ordering, or
changed generations fail closed with no partial write.

Apply retains the first validated occurrence, removes only redundant head
references and duplicate metadata, never mutates/deletes immutable entries,
and atomically records a content-free receipt through StateStore CAS. A second
apply and response-loss replay are no-ops with the same receipt. The shared
contract covers in-memory, SQLite, and PostgreSQL drivers, cancellation, and
concurrent attempts. Operators must stop/backup, inspect, apply, verify, and
repair all affected heads before admitting any v1.29.2+ event writer; Harbor
does not mutate downstream databases or platform configuration.

Implementation PR #730 merged at `dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`;
hosted candidate run `32600083120` and documentation run `32600083113`
completed successfully. The immutable annotated `v1.29.3` tag object is
`eeae1f44f4fb7d862f581f9cbbabb40a7827146a` and peels to
`dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`; release workflow `32602553267`
succeeded, publishing 13 assets with verified aggregate and six sidecar
checksums and six attestations at the [GitHub release](https://github.com/hurtener/Harbor/releases/tag/v1.29.3).
The native darwin/arm64 artifact reports v1.29.3, Protocol 0.1.0, build
`dabbcff4`; module provenance records
`Sum=h1:uRya1FQV+hu4YKH5jzQDVP0z0wqOnH6DnsOe8M7oxog=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`, and
`Origin.Ref=refs/tags/v1.29.3`. Post-tag scaffold pin/golden cleanup is
complete. Local `make preflight` was never run and no downstream fleet repair
or cutover is claimed.

---

## HA-71 — provider-neutral descriptors, runtime validation, and model discovery

**Priority:** High. **Size:** medium. **State:** Shipped in Harbor v1.30.0,
Harbor Phase 255 / D-435. Hosted CI and the release workflow are green;
downstream deployment and acceptance remain pending.

**Observed gap.** Bifrost already owns provider setup and model-listing
mechanics, but a control-plane consumer cannot safely ask Harbor what a
provider connection technically supports. Re-deriving credential fields,
custom endpoint support, model capabilities, or discovery behavior outside
the runtime would drift from the execution source of truth and risks exposing
provider errors or secrets.

**Required shape.** Harbor exposes a provider-neutral typed descriptor with
credential modes and logical field kinds, custom-endpoint support,
runtime-origin validation capability, discovery capability, and explicit
supported/unsupported/manual/unavailable/partial/stale/unknown/unpriced
states. Model discovery normalizes only reported context/input/output limits,
modalities, tools, canonical reasoning signals, deprecation, and pricing
provenance. Missing facts remain unknown or unpriced. Configured custom model
lists remain manual; when a custom endpoint cannot provide discovery, Harbor
returns that manual list as an explicit fallback rather than calling it
discovered.

**Boundary and guardrails.** Presentation metadata (friendly names, logos,
copy, and consumer aliases) remains outside Harbor. The adapter reuses the
real Bifrost account path and `ListModelsRequest`; it is bounded and
cancellable, rejects malformed/duplicate rows, and maps errors to stable
redacted codes. No provider response body, `ProviderExtra`, endpoint value,
environment variable name, credential, prompt, or identity value enters the
descriptor/result. The offline consumer is read-only `harbor llm providers`
and reports `runtime_origin=false`. The booted runtime also exposes the
contract through the existing protected `llm.posture` envelope with
`provider_operation=validate|discover` for admin-tier callers; it uses the
shared runtime credential path and is the only path that reports
`runtime_origin=true`. No new Protocol version or method is added.

**Release-candidate evidence.** Implementation PR #738 was merged at
`d9bf28fe703e10eb9f995657f4ac52949aa57e04` (tree
`72f8093049a3f7bc952d8d3e0decdd8d02ea7744`). Hosted candidate run
`32670321270` completed successfully on PR head
`1da7845326088e451bcf19970136a62b8e274e5a` (the same tree), including the full
preflight. Post-merge main run `32673186738` also completed successfully on
merged commit `d9bf28fe703e10eb9f995657f4ac52949aa57e04`, including full
preflight.
Independent adversarial review and an operator-approved runtime-origin
validation/discovery probe with sanitized output remain separate acceptance
evidence. The annotated `v1.30.0` tag object is
`53c388028f1150c9afb6263332583d319c3ba544` and peels to
`466b307c563f8193950ac5abef36677e48b1bae8`; release workflow `32683661507`
completed successfully and published 13 assets with verified aggregate
`checksums.txt`, six sidecar checksums, and six GitHub attestations. Public
module provenance records `Sum=h1:9MfAk67WbACqvXnwSMMv0WYonE+S0fV5Y7wcuhwNo8o=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=466b307c563f8193950ac5abef36677e48b1bae8`, and
`Origin.Ref=refs/tags/v1.30.0`. The native darwin/arm64 artifact reports
Harbor v1.30.0, Protocol 0.1.0, and build
`466b307c563f8193950ac5abef36677e48b1bae8`. The post-tag scaffold pin and
golden cleanup is included in this follow-up; this ask does not claim provider
account readiness, fleet rollout, or downstream database mutation.

## Posture signals from the downstream team

Recorded so a future phase does not "helpfully" relax something the consumer explicitly wants kept:

- **Do NOT relax `connect-src 'none'`, and do NOT honour server-declared `connectDomains`.** The deny-by-default CSP posture on the MCP-Apps sandbox — the D-173 sanctioned deviation, under which all App traffic stays bridge-proxied through the injected client — should be **preserved**. The consuming team asked for this explicitly. Any HA-38/39/40/41 work stays inside the bridge and the injected Protocol client; a direct-network escape hatch is not wanted, is not needed by any of these asks, and the `app-bridge-host` no-direct-transport spy test that guards it should keep passing untouched.

---

## HA-70 — context-bound external execution grants and durable usage receipts

**Priority:** High. **Size:** large. **State:** Shipped in Harbor v1.30.0,
Harbor Phase 254 / D-434. Hosted CI and the release workflow are green;
downstream deployment and acceptance remain pending. This is a generic
runtime execution-edge request; it does not prescribe a coordinator product or
provider-specific policy vocabulary.

**Observed gap.** Harbor's LLM edge has local governance, retry/failover,
provider-neutral reasoning controls, and token/cost telemetry, but a remote
coordinator cannot currently authorize one provider attempt with a signed,
context-bound policy decision while keeping credential custody outside the
runtime. Local `LiveKey` rotation is not a substitute for a per-attempt
runtime/identity/run/route binding, and post-call telemetry alone cannot make
cross-runtime consumption auditable or replay-safe.

**Requested shape.** Add one opt-in external-execution layer around the existing
one-method `LLMClient`. A coordinator-signed, content-free grant must carry a
version, signing key id, audience, expiry, policy generation, runtime and
verified identity/run binding, provider connection and immutable connection
generation, provider model/route, opaque
credential-binding handle, immutable credential-asset generation, reasoning
ceiling, output ceiling, and bounded compute lease. The runtime verifies the
signature and every claim against the request-edge verified context before
the Bifrost driver is reached. The caller cannot select authority, a provider
key, or a secret. Legacy calls remain byte-compatible when the mode is disabled;
optional mode supports a mixed fleet; strict mode refuses a missing or invalid
grant.

Credential resolution is a separate verified-context-only operation. The
provider account resolves the opaque handle only after grant verification and
rechecks exact runtime, organization, identity, run, provider, connection, and
asset generation. Rotation/revocation advances the generation and fences old
grants. A strict grant runtime must not need a boot API key. Harbor never logs,
serializes, or places credential bytes in grants, requests, receipts, errors,
or audit payloads.

The layer must sit inside retry, structured-output downgrade, and Harbor's
orchestrated failover so every provider attempt is checked and metered. A
bounded lease is checked before each call and may be replaced only by a newly
authorized coordinator top-up; the runtime never extends authority locally.
Existing local governance remains an emergency ceiling for both legacy and
granted calls.

Each attempted provider call emits an immutable content-free receipt with
stable grant/attempt/idempotency identifiers, route/model/provider dimensions,
policy and asset generations, token/cost/latency usage, outcome, and a
canonical body hash. A StateStore-backed outbox durably queues receipts,
conditionally ACKs them, deduplicates response-loss replay by receipt id and
body hash, retries with bounded backoff, and opens a circuit breaker during a
coordinator outage. Receipts contain no prompt, response, tool arguments,
reasoning trace, or secrets. The delivery contract must acknowledge duplicate
receipt submissions without double counting.

**Release-candidate evidence.** Implementation PR #738 was merged at
`d9bf28fe703e10eb9f995657f4ac52949aa57e04` (tree
`72f8093049a3f7bc952d8d3e0decdd8d02ea7744`). Hosted candidate run
`32670321270` completed successfully on PR head
`1da7845326088e451bcf19970136a62b8e274e5a` (the same tree), including the full
preflight. Post-merge main run `32673186738` also completed successfully on
merged commit `d9bf28fe703e10eb9f995657f4ac52949aa57e04`, including full
preflight.
The annotated `v1.30.0` tag object is
`53c388028f1150c9afb6263332583d319c3ba544` and peels to
`466b307c563f8193950ac5abef36677e48b1bae8`; release workflow `32683661507`
completed successfully and published 13 assets with verified aggregate
`checksums.txt`, six sidecar checksums, and six GitHub attestations. Public
module provenance records `Sum=h1:9MfAk67WbACqvXnwSMMv0WYonE+S0fV5Y7wcuhwNo8o=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=466b307c563f8193950ac5abef36677e48b1bae8`, and
`Origin.Ref=refs/tags/v1.30.0`. The native darwin/arm64 artifact reports
Harbor v1.30.0, Protocol 0.1.0, and build
`466b307c563f8193950ac5abef36677e48b1bae8`. The post-tag scaffold pin and
golden cleanup is included in this follow-up. This ask does not claim
coordinator integration or downstream deployment, fleet, or database
acceptance.

### HA-70 v1.30.1 compatibility extension — Phase 256 / D-436

**State:** Shipped in Harbor v1.30.1; downstream deployment and acceptance
remain pending.

The first external consumer found a generic framework gap in the v1.30.0
surface: grant/config/receipt delivery types and canonical receipt validation
were internal-only, and the strict grant shape required a coordinator-selected
provider route even when a runtime should continue using its existing configured
provider/model. This hotfix exposes the smallest public SDK aliases/helpers and
adds a signed `route_mode` with two explicit shapes:

- `coordinator_bound` retains the v1.30.0 provider/model/route/credential
  binding semantics and remains the default/legacy blank interpretation.
- `runtime_default` carries no provider or credential claims, uses the runtime's
  configured provider/model, records actual provider/model in the receipt, and
  retains all signed limits, durable attempt reservation, receipt, retry, and
  downgrade enforcement.

Canonical receipt JSON/hash and receipt-to-grant validation include stable
parent logical-call/nonce, planner-step, retry, downgrade, fallback, and
attempt coordinates. Forged root/child or mixed-route receipts fail closed;
response-loss replay cannot issue a second provider call. The public delivery
interface is transport-neutral; this ask does not add an HTTP endpoint or
provider-specific policy vocabulary. Protocol version remains `0.1.0`.

Implementation PR #742 was reviewed at exact head `9af8e6e7` and squash-merged
as `506d1f8c`. Hosted candidate run `32705738802` and post-merge main run
`32710662323` completed successfully, including full preflight. The annotated
`v1.30.1` tag object `8175c93a3ff974d522210054b8c39c2a21ba7199`
peels to `fd801b14`; release workflow `32720513063` succeeded with 13 assets,
verified checksums and attestations, the expected native binary stamp, and
public module provenance. The annotated tag object was reissued with the
canonical maintainer identity without changing its peeled release commit or
published assets. Post-tag scaffold cleanup is included in the follow-up. No
downstream deployment, fleet, database, or acceptance claim is made.

---

## HA-72 — stock coordinator receipt transport and grant readiness

**Priority:** High. **Size:** large. **State:** Accepted for the v1.30.2
release candidate through Phase 257 / D-437, Phase 258 / D-438, and Phase 261 /
D-441. Tag, release, downstream deployment, and acceptance remain pending.

**Observed gap.** Harbor v1.30.1 publishes the transport-neutral receipt
delivery seam and canonical marshal/hash/validation helpers, but an external
receiver cannot parse the private snake-case canonical wire through the public
SDK. Separately, stock `harbor serve` injects no authenticated coordinator
transport, and `runtime.info` cannot distinguish a configured grant toggle from
a concretely wired strict path.

**Parser slice.** Phase 257 adds the public
`UnmarshalCanonicalAttemptUsageReceipt([]byte)` inverse beside the existing
marshal helper. It decodes through Harbor's one private canonical wire,
validates the projected public receipt and body hash, and then requires
byte-identical canonical re-encoding. Unknown, duplicate, missing, reordered,
alternatively encoded, or trailing content fails closed under
`ErrInvalidUsageReceipt`; malformed input is never reflected in errors or
logs. The function adds no Protocol method, transport, timer, database read, or
idle work. Historical blank public route mode is represented as explicit
`coordinator_bound` on the canonical wire; when its preserved legacy hash
validates, parsing restores the blank public value and re-marshal remains
byte-identical.

**Stock transport/readiness completion.** Phase 258 gives stock `harbor serve`
an opt-in authenticated coordinator transport for bounded canonical receipt
batches, exact receipt-ID/body-hash ACKs, response-loss-safe durable replay,
and stable jittered backoff. Disabled/default deployments do no coordinator
work, and `runtime_default` remains independent of coordinator-managed provider
credentials and catalogs.

Lease settlement now atomically leaves a removable pending-receipt handoff for
success, error, and cancellation. The stock outbox consumes that prefix and
removes an exact handoff only after durable enqueue; retained attempt history
is never scanned for steady-state recovery. A versioned marker makes the old
whole receipt-prefix pass an upgrade-only operation, so ordinary ACKed lifetime
history cannot amplify idle database work or starve later crash-gap facts.

The additive `runtime.info.external_grant` projection reports supported and
configured modes, accepted and independently ready route shapes,
verifier/reservation/credential wiring, strict parser readiness, concrete
receipt transport kind and observed readiness, the exact unsupported,
host-injected, or stock-authenticated-HTTP top-up kind, and a fail-closed
fully-wired result. No endpoint, token,
identity, receipt content, provider response, or product-specific vocabulary
appears in the posture response, logs, or errors. Cadence reconciliation
failures degrade the projection while retrying and recover only after success;
the whole readiness object is optional for mixed-version clients. Phase 261
adds the separately authenticated stock renewal exchange and replay-idempotent
durable successor application without adding idle work.

---

## HA-73 — stock coordinator-bound external-grant credential resolution

**Priority:** High. **Size:** contained. **State:** Implemented as a local
candidate through Phase 262 / D-442. Hosted CI, release, downstream deployment,
and acceptance remain pending.

**Observed gap.** The public external-grant SDK exposed a host-injected
`CredentialResolver`, but stock `harbor serve` had no configuration or public
server option that could supply it. A strict coordinator-bound runtime could
therefore verify grants and deliver receipts yet still fail boot or remain
unready unless a custom embedding host rebuilt the serving layer.

**Framework answer.** Phase 262 publishes `sdk/llm/credentials` as the exact
version-1 canonical exchange and adds two equivalent production doors: a
public `sdk/server.Options.ExternalGrant` injection for embedding hosts and an
optional stock authenticated `llm.external_grant.coordinator.credential_url`.
The request contains only the complete signed grant already installed by the
verified grant wrapper. It has no separate organization, identity, route,
provider, connection, generation, handle, endpoint, or secret override. The
response must exactly match the signed provider, opaque credential handle,
provider-connection generation, and credential-asset generation before its
short-lived secret reaches the provider driver.

The stock transport accepts only HTTPS or loopback HTTP, refuses redirects,
bounds duration and bodies, authenticates with the existing boot-named runtime
service token, and never reflects an endpoint, response body, token, handle,
or secret in an error. Its secret-bearing cache is keyed by the SHA-256 of the
complete canonical verified grant, bounded to 256 entries and 30 seconds (also
clamped to grant and response expiry), coalesced only for that exact digest,
generation-fenced, and cleared on close. One caller's cancellation does not
cancel another exact-binding waiter; different organizations sharing one
runtime never share a cache key or result.

Absent configuration constructs no client, performs no network or StateStore
work, and starts no goroutine or timer. `runtime_default` keeps using the
runtime's configured provider key and requires neither this endpoint nor a
coordinator model catalog. `runtime.info.external_grant` reports
`coordinator_bound` ready only when its resolver and the existing verifier,
reservation, and receipt seams are all concretely wired. No Protocol shape or
version changes.

---

## HA-74 — top-up successor grant preserves immutable authority and attempt identity

**Priority:** High. **Size:** contained. **State:** Accepted for the v1.30.2
release candidate through Phase 259 / D-439 and Phase 261 / D-441; tag,
release, and downstream acceptance remain pending.

**Observed gap.** `LeaseTopUpper.TopUp` returns a newly signed
`ExternalGrant`, but the v1.30.1 wrapper compared only logical-call id,
attempt nonce, and effective route mode before replacing the verified grant.
Re-verification proved that the returned grant was valid in isolation; it did
not prove that it was a bounded successor of the original authority. A broken
top-up service could therefore substitute a different grant, identity, route,
credential generation, policy, ceiling, or lease id and still reach the
provider if that replacement independently verified.

**Implemented framework shape.** The public
`ValidateExternalGrantTopUpSuccessor` helper compares the raw signed route mode
and every immutable contract field. It permits only rotating key id,
issued-at, signature, strictly advancing lease state, and non-rewinding
validity.
The lease id is immutable; the epoch advances exactly once; total capacity
increases positively by no more than the provider call's requested units;
consumption cannot rewind; and remaining capacity is sufficient for the call.
Grant and lease deadlines may remain unchanged or move forward while retaining
at most the respective lifetime signed into the predecessor. The wrapper calls
this helper before it accepts the successor, then runs the configured
signature/context verifier again. Legacy blank route mode must remain blank
across a top-up; it cannot be normalized into wider authority.

Table tests mutate every preserved field and prove refusal before the provider
call. Separate tests cover both route modes, epoch/capacity/consumption and
validity adversaries, integer overflow, deterministic response replay, stale
successor refusal, N=100 concurrent reuse under the race detector, fuzzed
immutable-string drift, and external-package SDK reachability. This is a
generic execution-grant safety correction. It adds no transport, quota store,
billing model, provider catalog, product policy, credential format, Protocol
method, or Protocol version.

**Operational completion.** Phase 261 adds a public transport-neutral canonical
exchange, the optional authenticated stock client, narrow predecessor
authentication, reason-aware expiry renewal, and the replay-idempotent durable
successor hook. Relationship validation and ordinary successor verification
still precede durable application, and durable application precedes reservation
and provider execution.

---

## HA-75 — reach-admitted effective AgentID bound into external execution grants and receipts

**Priority:** High. **Size:** contained. **State:** Accepted for the v1.30.2
release candidate through Phase 260 / D-440; tag, release, and downstream
acceptance remain pending.

**Observed gap.** The deployed external grant binds organization, runtime,
identity triple, logical run/call, route, credential generation, policy, and
lease, but not the effective agent configuration selected by the normal
signed-reach admission. A coordinator could therefore not prove that one
receipt and provider attempt belonged to the same admitted agent configuration.

**Implemented framework shape.** Version 2 adds a required signed AgentID. The
reference verifier reads the private effective-agent capability restored from
the durable control-start reach receipt and requires an exact match before any
reservation, credential resolution, or provider call. Explicit and omitted
runtime-default selections use the same gate. AgentID is immutable across a
top-up and appears only as a content-free receipt identity fact. Runtime
readiness advertises grant versions `[1,2]` and `required_v2` binding support.

Version 1 remains exact for deployed signatures and blank-agent receipt bytes,
but a v1 grant is rejected if it tries to carry AgentID. Runtime-default v2
grants remain valid without a coordinator-supplied provider credential or model
catalog. The Protocol version is unchanged.

**Operational boundary.** This answer covers every strict grant-bearing
provider attempt in the canonical runtime path. Auxiliary naming, compression,
and rolling-summary calls do not currently receive distinct grants; required
mode blocks ungranted calls before a provider, while optional mode retains its
compatibility behavior. Arbitrary embedder calls are not described as
reach-admitted unless their verifier establishes an equivalent trusted context.

**Shared v1.30.2 release-candidate evidence (HA-72, HA-74, HA-75).** PR #747
head `0992356db24b43776a10a6572e3df56b610cf50e` was squash-merged as
`459278f7ce599aa6a66f83c3ffbaeb42bb6b7f0c` (tree
`5b7583150d8e7cd3149da1eb77eda4e68ff63f64`). Candidate docs run
`32850686252` and post-merge docs run `32852635507` succeeded. At this cut,
candidate CI `32850686237` and post-merge CI `32852635451` had no failed jobs
but each final preflight gate was still in progress. Due the one-hour pause
deadline, the owner authorized proceeding with the release and tag despite
those pending gates; any late failure will be fixed later that day. As of this
release-cut commit, no tag, release, downstream deployment, or acceptance is
claimed.
