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
| HA-52 | Typed retry classification for MCP `CallToolResult.IsError` | internal/tools/drivers/mcp + internal/tools | High | Contained | Filed |

The original five were filed by a downstream team building an MCP-Apps server
against Harbor. HA-51 is a separate release-blocking fidelity report; HA-52
is a separate MCP transport/reliability report. The entries are
**framework-framed** — each names a Harbor-side capability that is absent,
inert, or narrower than its documentation claims.

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

## HA-52 — deterministic MCP tool failures exhaust the retry budget unchanged

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

## Posture signals from the downstream team

Recorded so a future phase does not "helpfully" relax something the consumer explicitly wants kept:

- **Do NOT relax `connect-src 'none'`, and do NOT honour server-declared `connectDomains`.** The deny-by-default CSP posture on the MCP-Apps sandbox — the D-173 sanctioned deviation, under which all App traffic stays bridge-proxied through the injected client — should be **preserved**. The consuming team asked for this explicitly. Any HA-38/39/40/41 work stays inside the bridge and the injected Protocol client; a direct-network escape hatch is not wanted, is not needed by any of these asks, and the `app-bridge-host` no-direct-transport spy test that guards it should keep passing untouched.
