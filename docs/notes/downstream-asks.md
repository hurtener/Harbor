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
| HA-54 | Typed retry classification for MCP `CallToolResult.IsError` | internal/tools/drivers/mcp + internal/tools | High | Contained | Planned — phase 236 / D-410 |
| HA-55 | Operator-managed per-agent skill packs across authenticated users | internal/skills + runtime/agentcfg + runtime/serve | High | Medium | Planned — phase 237 / D-411 |
| HA-56 | Per-server MCP App callback catalog outside planner projection | internal/tools/drivers/mcp + internal/tools + internal/mcpconsole + protocol | High | Medium | Planned — phase 238 / D-412 |
| HA-57 | Typed reliability classification observability on the tool-failure events | internal/tools + internal/events + internal/protocol | High | Contained | Planned — phase 239 / D-413 |
| HA-58 | Operator-verifiable per-agent skill composition preview | internal/skills + runtime/agentcfg + protocol | High | Medium | Planned — phase 240 / D-414 |
| HA-59 | Composition-preview operator consumer: CLI verb + Console skill view | cmd/harbor + web/console | Medium | Medium | Planned — phase 241 / D-415 |
| HA-60 | MCP App tool-context retention policy settled (eviction or stated session lifetime) | internal/mcpconsole + internal/state + internal/protocol | Medium | Contained | Planned — phase 242 / D-416 |

The original five were filed by a downstream team building an MCP-Apps server
against Harbor. HA-51 is a separate release-blocking fidelity report; HA-54
is a separate MCP transport/reliability report; HA-55 is a separate runtime
skills projection report; and HA-56 is a separate MCP App host/catalog
report. HA-52 and HA-53 are already allocated in the shared register and are
not available for Harbor-local filings. HA-54, HA-55, and HA-56 are now
**Planned** in the v1.27 wave — phase 236 / D-410, phase 237 / D-411, and
phase 238 / D-412 respectively (master-plan index rows and detail blocks
carry the mapping). **HA-57 through HA-60 are Harbor-internal filings**,
raised by the wave's own reliability/verification review rather than by an
outside consumer, and are planned in the same wave (phase 239 / D-413,
phase 240 / D-414, phase 241 / D-415, phase 242 / D-416). The entries are
**framework-framed** — each names a Harbor-side capability that is absent,
inert, or narrower than its documentation claims.

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

## HA-57 — the typed reliability classification is not observable on the failure events

**Priority:** High (operational honesty). **Size:** contained. **State:**
Planned — phase 239 / D-413 (Harbor-internal filing).

**What the consumer sees.** An operator reading `tool.failed` /
`tool.policy_exhausted` cannot tell "deterministic failure correctly attempted
once" from "provider outage retried four times" — the exact difference HA-54
exists to make. The classification D-410 defines lives entirely inside the
retry shell; none of it reaches the canonical failure events.

**Requested shape.** Additive, non-secret control metadata on the existing
tool-failure events: the resolved class (permanent / retryable with reason),
the attempt ordinal, and the configured budget — server-derived from the
D-410 classifier, never caller-supplied, never raw arguments or results
(§7 rule 7 / §13 hold; the audit-redactor boundary is unchanged). Legacy or
absent classifications render a representable `unclassified`, never a
fabricated class (D-311). Prefer extending the existing payloads over new
event types unless a genuine gap is proven.

**Required acceptance.**

1. A permanent-classified failure under the default policy emits exactly one
   invocation and its failure event carries `permanent` with attempt `1` of
   the configured budget.
2. A retryable classified failure emits each attempt and the terminal event
   carries the final attempt and class; timeout/5xx behavior unchanged.
3. Legacy unclassified results emit `unclassified` — no guessed class, no
   fabricated budget figure.
4. No event carries raw arguments or results; redaction-gate tests pin the
   boundary; identity/run/task keys unchanged; no new identity axes.
5. Concurrent mixed classification streams under `-race` (N≥100) with no
   cross-run bleed; integration test with real drivers, identity
   propagation, and at least one failure mode.

---

## HA-58 — the composed per-agent skill snapshot is not verifiable without starting a run

**Priority:** High (operator trust). **Size:** medium. **State:** Planned —
phase 240 / D-414 (Harbor-internal filing).

**What the consumer sees.** HA-55's composed run snapshot is consumed only
inside a run (directory injection and the three `skill_*` tools). An operator
cannot verify what a given `(tenant, user, agent)` will compose — which
operator pack, which personal skills survive, which `RequiredTools` get
filtered — without starting a run and reading its output. The composition
itself happens at run-start; there is no read surface for it.

**Requested shape.** An operator-facing, read-only composition-preview
surface: identity-addressed `(tenant, user, agent)` → the exact immutable
snapshot the next run would compose, as names and bounded verdicts (per-item
visibility, filtered required-tool outcomes) — bodies only for the addressed
caller's own row, never cross-principal content. Authority is server-derived
(D-299): an operator with verified signed reach to the effective agent may
preview any user's composition for that agent within the tenant; an ordinary
caller may preview only their own. The preview is a pure projection over
durable state — never mutates, never advances a revision, never writes a run —
reusing the one per-run composite resolver HA-55 builds. Run-time failures
(unresolvable pack, retirement tombstone) surface the same typed result;
unwired state renders representable `unavailable`, never a fabricated
composition (D-311).

**Required acceptance.**

1. An operator preview of user A's composition for agent X matches the
   composition an actual next run uses, against the real durable store.
2. An ordinary caller previews only their own composition; a same-tenant
   different user and a cross-tenant caller get a typed denial/empty result
   without discovering names.
3. A revoked or unselected pack renders the typed not-found state, never
   another principal's row.
4. The preview is provably read-only: revision hash, revision list, skill
   rows, and audit unchanged after N previews.
5. Concurrent mixed-tenant/user/agent previews under `-race` (N≥100) with no
   context or authority bleed, plus at least one failure mode.

---

## HA-59 — the composition preview needs an operator consumer in the same wave

**Priority:** Medium (§13 primitive-with-consumer closure). **Size:** medium.
**State:** Planned — phase 241 / D-415, depends on 240 (Harbor-internal
filing).

**What the consumer sees.** HA-58's preview surface would be a primitive with
no consumer: an operator can only reach it through the raw Protocol method.
Per §13, the wave that introduces a primitive must also ship a consumer that
exercises it end-to-end with a test.

**Requested shape.** An operator-facing pair consuming ONLY the preview
surface: a `harbor` CLI verb that inspects the effective agent's composition
and a Console skill/agent view rendering the preview — so an operator
verifies pack membership and filtered-tool verdicts before a run and can diff
the preview across two revisions. No new backend logic, no second composition
path. Both go through the exact signed-reach admission the method enforces
and render its typed not-found/denied/unavailable states as returned — never
a blank state (D-311).

**Required acceptance.**

1. The CLI verb returns the same composition an actual next run composes,
   against the real durable store.
2. The Console view renders pack names, personal-skill names, and verdicts
   plus the typed not-found/denied/unavailable states exactly as returned.
3. Unauthorized CLI and Console attempts fail exactly as the method fails.
4. A two-revision diff shows added/removed pack membership and changed
   verdicts.
5. An end-to-end integration test with real drivers covers identity
   propagation, at least one failure mode, and an N≥10 concurrent stress run.

---

## HA-60 — MCP App tool-context records have no explicit retention policy

**Priority:** Medium (two halves disagree). **Size:** contained. **State:**
Planned — phase 242 / D-416 (Harbor-internal filing; independent of the
composition wave).

**What the consumer sees.** This is the HA-39 sub-question that D-347 and
D-348 deliberately left open, now filed as its own ask. The read path is
built for an eviction the write path never performs:
`internal/mcpconsole/toolcontext.go:195-208` documents "an unknown or
cross-identity `(serverID, toolCallID)` returns a wrapped not-found whose
marker the Protocol surface maps to `CodeNotFound`", and the Console adapter
treats `not_found` as "no captured context, no delivery" — but there is no
TTL, no eviction, and no sweeper anywhere in the package: records are written
as ordinary session-scoped `StateRecord`s and are never expired. The two
halves disagree.

**Requested shape.** Settle the policy deliberately and enforce it. Default
lean: the **session-lifetime contract** — records live for the session's
lifetime and die with the existing session-erasure fences, so `not_found`
covers cross-identity and unknown-id only (the smallest honest contract,
since the records are already session-scoped StateRecords). Alternative: a
real bounded, identity-scoped retention/eviction policy (per-session TTL +
idle sweep) adopted only if the phase measures unbounded growth in
long-lived sessions; either way the chosen policy is enforced by test and
the unchosen branch's guard rails documented. If eviction: bounded,
identity-scoped, race-safe under `-race`, evicted ids return the typed
`CodeNotFound` (load-bearing), never a silent blank. The D-173 sandbox/CSP
posture and the D-348 honest placeholder are untouched under either choice.

**Required acceptance.**

1. Current behavior pinned by tests before the policy lands: records survive
   session reopen; `not_found` for unknown and cross-identity ids.
2. The chosen policy is implemented and enforced; the rejected option's
   consequences documented.
3. If eviction: bounded, identity-scoped, race-safe with a concurrent
   reader/writer stress run.
4. The Console renders the D-348 honest placeholder on `not_found` unchanged.
5. Integration with the real durable StateStore: identity propagation, at
   least one failure mode, cross-session isolation of records.

---

## Posture signals from the downstream team

Recorded so a future phase does not "helpfully" relax something the consumer explicitly wants kept:

- **Do NOT relax `connect-src 'none'`, and do NOT honour server-declared `connectDomains`.** The deny-by-default CSP posture on the MCP-Apps sandbox — the D-173 sanctioned deviation, under which all App traffic stays bridge-proxied through the injected client — should be **preserved**. The consuming team asked for this explicitly. Any HA-38/39/40/41 work stays inside the bridge and the injected Protocol client; a direct-network escape hatch is not wanted, is not needed by any of these asks, and the `app-bridge-host` no-direct-transport spy test that guards it should keep passing untouched.
