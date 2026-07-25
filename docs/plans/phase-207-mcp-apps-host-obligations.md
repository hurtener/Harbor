# Phase 207 — re-land the reverted MCP-Apps host obligations

## Summary

Five MCP-Apps host obligations recorded as delivered by Phase 109k (D-227) were never in the source tree: the merge that shipped 109k also reverted the entire Console half of it, and Phase 109l re-landed only the live-theme and data-delivery pieces. This phase re-lands the remaining five — the `<serverID>_` app→host tool-call confinement prefix, `ui/notifications/size-changed` consumption, graceful `ui/resource-teardown` + `request-teardown`, host-context `toolInfo` + `containerDimensions`, and `resources/templates/list` — handshake-safe against the D-342 lifecycle invariants, and corrects the record in the master plan, D-227, and the downstream-asks register. It also converts `scripts/smoke/phase-109k.sh` from SKIP-tolerant greps into hard assertions, because that degraded gate is why the gap survived four phases.

## RFC anchor

- RFC §7.3
- RFC §6.4

## Briefs informing this phase

- brief 14

## Brief findings incorporated

- brief 14 §3 (the "roots honesty" bar — *"correct behaviour for every capability you advertise"*): Harbor advertises the `serverResources` capability but answered `resources/templates/list` with a method-not-found, and advertises `serverTools` while an app-supplied bare tool name could not resolve at all. Both advertised capabilities are made honestly serviceable here.
- brief 14 §6 (the AppBridge host↔view dialect is the FULL contract — `size-changed`, `host-context-changed`, teardown, tool-input/tool-result): a host that pushes data but ignores `size-changed` and teardown is a partial host. The App half of the vendored SDK emits both unprompted; the consumption is inert only because the host never listened.
- brief 14 §2 (extension negotiation must be populated in the shape the spec/SDK READ): `toolInfo` and `containerDimensions` are populated in the vendored `McpUiHostContext` slots — `toolInfo: { id, tool }` with a real `Tool`, and `containerDimensions` in the spec's `({height}|{maxHeight}) & ({width}|{maxWidth})` intersection — not in an invented shape that would be self-consistent and invisible to a real App.
- brief 14 §0 / §4 (never claim conformance until it is substantiated): the gate for every obligation here is the REAL vendored `App` client driving a REAL `ui/initialize` in a real sandboxed iframe (the Playwright handshake spec), not a hand-authored postMessage fixture (§17.8, the D-216 class).

## Findings I'm departing from (if any)

None.

## Goals

- Re-land all five absent host obligations from D-227 item 3/4.
- Make the app→host tool-call confinement a property that cannot silently evaporate again: an unconditional host-derived qualifier, a vitest confinement case, a Go reflection guard on the wire shape, and a hard smoke assertion.
- Give a rendered App a typed, distinguishable rejection when it names a tool that does not exist on its own server, so it can degrade deliberately.
- Preserve all four D-342 lifecycle invariants, and prove each still holds with the new machinery attached.
- Correct the record: the master plan, D-227's Consequence, and the HA-38/HA-41 entries in the downstream-asks register.

## Non-goals

- **A backend `server_id` on `mcp.apps.call_tool`.** D-227 item 3 weighed and declined it; "Risks / open questions" below records why the placement is re-affirmed FOR NOW and why the strongest argument for reversing it (an exact `desc.Tool.Source` comparison the Console structurally cannot perform) is recorded as a D-351 follow-up rather than built here. Reversing it is a Protocol change, not an implementation detail.
- **Real resource-template support.** Harbor's Protocol exposes no resource-template method. The host answers the advertised capability honestly with an empty list; a future `mcp.servers.resource_templates` routes into the same adapter method with no other change.
- **HA-39 (heavy `artifact_ref` to `structuredContent`).** A separate ask with its own resolution; untouched here.
- **`_meta.ui.visibility` enforcement** (HA-41 residual item 3). Additive, independent, and explicitly deferred rather than left looking supported.
- **Relaying `containerDimensions` on resize.** The host resizes the frame in RESPONSE to the app's own report; echoing the new box back would close a report→resize→report feedback loop.

## Acceptance criteria

- [ ] An app-initiated `tools/call` dispatches `<serverID>_<name>`, where `serverID` is host-derived from the `mcp.app_available` discovery and can never be supplied or influenced by the sandboxed App.
- [ ] The qualifier is UNCONDITIONAL: a cross-server or self-prefixed name (`otherserver_drop_table`, `srv_echo`) is still prefixed and therefore cannot resolve outside the app's own namespace.
- [ ] **Registration-time separator-safety**: an MCP server id that would make `<sourceID>_<tool>` ambiguous against an already-registered id is refused fail-loud with `ErrAmbiguousServerID`, in BOTH orders, on BOTH the boot-declared and runtime-attach paths. Without it the prefix does not confine (`github` + `github_enterprise`); with it the key space is injective and the prefix means what it claims.
- [ ] Ids that merely share a prefix without a separator boundary (`github` / `githubby`) still register, and same-id re-registration (hot reload / re-attach) still works.
- [ ] Identity, the tool's approval/OAuth wrappers, and the paused-server / disabled-tool exposure gate all still fire on the app-call path (the proxy remains a re-entry, not a parallel path).
- [ ] A name that does not resolve within the app's own server returns `CodeNotFound` at the Protocol edge and reaches the App as a typed `MCPAppToolNotFoundError` naming the BARE name it asked for and the server it is confined to; a transport failure keeps `CodeRuntimeError`.
- [ ] Not-found classification is SENTINEL-based (`errors.Is`), never a substring match over the error chain — a southbound server's error text cannot launder a transport failure into a typed not-found.
- [ ] Bridge callbacks (`onInitialized` / `onSizeChanged` / `onRequestTeardown`) are guarded on bridge-instance identity, so a stale bridge cannot set the sticky `closed` state over a live successor or drive its frame height.
- [ ] `ui/notifications/size-changed` is consumed and drives the INLINE iframe height, coalesced through `requestAnimationFrame`, clamped between `--size-app-inline-min` and a new host-owned `--size-app-inline-max`.
- [ ] The envelope and the reported-height style are gated on `isInline`: the page-level fullscreen / pip panel reuses `.mcp-app__frame` and sizes it itself, so neither the clamp nor the app-driven height may reach it.
- [ ] An App that never reports a size keeps exactly the previous fixed-height behaviour (no inline height is set).
- [ ] `ui/resource-teardown` is sent before the transport closes on unmount, and NEVER for a bridge the App has not finished `ui/initialize` on.
- [ ] An App-initiated `ui/notifications/request-teardown` is granted: graceful teardown, close, and the frame is replaced by an honest "this app closed itself" placeholder that a transcript re-render does not resurrect.
- [ ] `ui/initialize` host-context carries `toolInfo` (`{ id: toolCallId, tool: { name: toolName } }`) and `containerDimensions`, both baked in at construction and never patched mid-handshake.
- [ ] `resources/templates/list` is handled and answers rather than erroring.
- [ ] D-342 holds: exactly ONE bridge construction per mount with the FINAL host-context; the lifecycle `$effect` depends only on `loadState` + `iframeEl`; every host→app send is gated behind `oninitialized`; no teardown-rebuild for a theme, data, size, or prop change.
- [ ] `scripts/smoke/phase-109k.sh` FAILS (not SKIPs) when any re-landed obligation is absent.
- [ ] The master plan's 109k row/detail, D-227's record, and HA-38/HA-41 state what actually shipped.

## Files added or changed

```text
docs/
  plans/phase-207-mcp-apps-host-obligations.md      (added)
  plans/README.md                                   (207 row + detail; 109k correction)
  decisions.md                                      (D-227 correcting note; D-351)
  glossary.md                                       (app-host tool-namespace confinement; host obligation)
  notes/downstream-asks.md                          (HA-38 to Shipped; HA-41 residue)
  skills/drive-the-playground/SKILL.md              (operator-facing MCP-App host surface)
scripts/smoke/
  phase-109k.sh                                     (SKIP-tolerant greps to hard assertions)
  phase-207.sh                                      (added)
.gitattributes                                      (added - force textual diffs for source types)
internal/protocol/
  mcp.go                                            (not-found classification is sentinel-only)
  apps.go                                           (ErrAccessorNotFound)
  apps_test.go / mcp_test.go                        (sentinel classification + the laundering guard)
  types/mcp_apps_test.go                            (added - the no-server-scope wire guard)
internal/mcpconsole/
  mcpconsole.go                                     (markNotFound - driver->Protocol sentinel,
                                                     applied on every accessor not-found path)
  toolcontext.go                                    (tool-context miss wraps the sentinel)
  apps.go                                           (resolve miss + read_resource wrap the sentinel)
  notfound_translation_test.go                      (added - enumerates every translating path)
internal/tools/drivers/mcp/
  registry.go                                       (ErrAmbiguousServerID, CheckServerIDUnambiguous)
  attach.go                                         (early pre-check, both attach paths)
  server_id_ambiguity_test.go                       (added - the escape + the guard)
web/console/src/lib/
  tokens.css                                        (--size-app-inline-max)
  chat/tokens.contract.json                         (the new token)
  chat/renderers/app-bridge-host.ts                 (qualifier, typed not-found, templates handler,
                                                     size/teardown wiring, toolInfo/containerDimensions)
  chat/renderers/mcp-app.svelte                     (size clamp, closed state, container measure)
  chat/renderers/app-bridge-host.spec.ts            (confinement + typed not-found + templates)
  chat/renderers/mcp-app-host-obligations.spec.ts   (added - renderer obligation + D-342 guards)
  mcp-app-host-client.ts / .spec.ts                 (not_found to typed error; listResourceTemplates)
  sessions/history.ts                               (toolName on the replayed ref)
  components/playground/{layout.ts,AppPanel.svelte} (toolName through the page-level render)
web/console/src/routes/(console)/playground/[session_id]/
  wire-events.ts, turn-projection.ts                (decode + project toolName)
web/console/tests/mcp-app-host-handshake.spec.ts    (real-App obligation round-trips; stale-fixture fix)
```

## Public API surface

Console-internal only; no Go public API and no Protocol wire change.

```ts
// web/console/src/lib/chat/renderers/app-bridge-host.ts
export function qualifyAppToolName(serverID: string, name: string): string;
export class MCPAppToolNotFoundError extends Error { tool: string; serverID: string | undefined }
export function containerDimensionsFromBox(box: { width: number; maxHeight?: number }):
  McpUiHostContext['containerDimensions'] | undefined;
export const APP_TEARDOWN_TIMEOUT_MS: number;

// MCPAppHostClient gains one method (implemented by the Console adapter):
listResourceTemplates(serverID: string): Promise<MCPAppResourceTemplateListing[]>;

// AppBridgeHostOptions gains: toolInfo, containerDimensions, onSizeChanged, onRequestTeardown.
// MCPAppRefView gains: toolName.
```

## Test plan

- **Unit:** (Go) `TestCatalogKeyJoin_IsNotInjective_WithoutTheRegistrationGuard` demonstrates the confinement escape against the REAL `tools.Catalog`, so the guard's value is a shown property rather than an assertion about a string; `TestRegistry_Register_RefusesSeparatorAmbiguousServerID` (both orders), `TestRegistry_Register_AllowsPrefixWithoutSeparatorBoundary`, `TestRegistry_Register_SameIDReplacementStillAllowed`, and `TestRegistry_CheckServerIDUnambiguous_AgreesWithRegister` pin the guard and its boundaries. `TestAppsSurface_CallTool_UnresolvableToolMapsToNotFound` / `..._TransportFailureStaysRuntimeError` pin the two-way classification, and `..._RemoteErrorTextCannotLaunderIntoNotFound` proves a southbound server's wording cannot forge a typed not-found. `TestMCPAppCallToolRequest_CarriesNoServerScope` is a reflection guard that the wire request never grows an app-suppliable server scope. (vitest) bare-name qualification, cross-server confinement, the self-prefixed `srv_echo` case (the escape-hatch shape), the unconditional-qualifier property, typed not-found round-trip, non-not-found propagation, `resources/templates/list` empty + mapped, `containerDimensionsFromBox` mapping.

- **Integration:** the renderer-level `mcp-app-host-obligations.spec.ts` mounts the REAL `McpAppRenderer` against a mocked bridge and drives each obligation end-to-end through the real lifecycle effects: size-changed to frame height, coalescing, per-value nonsense rejection, the fullscreen/pip surface carrying NEITHER the envelope nor the app-driven height (with the inline control case beside it), teardown-before-close ordering, teardown NOT sent pre-`initialized`, teardown failure still closing, app-requested teardown to placeholder, host-context `toolInfo`/`containerDimensions` at construction, templates handler wired, and a stale-bridge isolation case that churns the `app` prop within ONE component so a superseded bridge's late callbacks are proven inert against the live successor. The Playwright `mcp-app-host-handshake.spec.ts` drives the REAL vendored `App` client through a REAL `ui/initialize` in a real sandboxed iframe and asserts the App received `toolInfo`/`containerDimensions`, that its `sendSizeChanged` reaches the host, and that both host- and app-initiated teardown fire the App's `onteardown` (§17.8 — the fixture is the official package's own client, not our reading of the dialect).

- **Conformance:** N/A — no new driver seam; the external-protocol conformance surface is the vendored ext-apps types (compile-time) plus the real-App Playwright round-trip above.
- **Concurrency / leak:** N/A for a Go reusable artifact — no Go artifact is added (the Protocol edge change is a pure classification function). The Console-side analogue is the D-342 lifecycle proof: exactly one bridge per mount under prop churn, theme churn, and a resize storm, with `close` never called.

## Smoke script additions

- `scripts/smoke/phase-109k.sh` is rewritten: every one of its acceptance criteria becomes a hard `assert_grep_present` / `assert_grep_absent`. The `mimeTypes` capability, the `runtime.info` display modes, the namespace qualifier (both its existence AND its use in `oncalltool`), the size-changed consumer, the graceful teardown + `request-teardown` handler, the live-theme relay, host-context `toolInfo` + `containerDimensions`, and `onlistresourcetemplates` — 14 assertions, zero SKIPs.
- `scripts/smoke/phase-207.sh` (new) guards what this phase adds on top: the qualifier is unconditional and has no already-qualified escape hatch; the no-server-scope wire guard test exists; the tool-not-found marker at the Protocol edge and the typed error in the host + adapter; both size tokens exist and the frame is clamped by the maximum; the teardown send is gated on `wasInitialized`; and the record correction (D-351 in the decisions log and the master plan, HA-38 flipped in the downstream-asks register).

## Coverage target

- `internal/protocol`: ≥ 80% (unchanged; the change is one classification branch, covered both ways).
- `web/console/src/lib/chat/renderers/`: every new branch covered by vitest — the qualifier, both not-found paths, the templates handler, the size consumer (accept / coalesce / reject), the teardown gate (initialized / not-initialized / failure), and the host-context slots.

## Dependencies

- 109k (the backend half that shipped), 109l (the handshake-safe bridge lifecycle this must not regress), 204 (the tool-context prefetch + generation token this builds on).

## Console consistency

Built against `docs/design/console/CONVENTIONS.md` (CLAUDE.md §4.5 item 12, D-121):

- **§1 Routing / §2 App shell** — untouched. This phase changes no route and adds no page; it works entirely inside the shared chat module's MCP-Apps renderer, which the `(console)` Playground route already mounts.
- **§3 Shared component inventory** — no new UI primitive. The app-requested-teardown outcome reuses the renderer's existing `.mcp-app__state` placeholder block (the same markup the `unavailable` miss state uses), rather than introducing a parallel notice component.
- **§4 The four-state async contract** — the renderer's own state machine is the chat-module-local analogue of `<PageState>` and stays exhaustive: `loading` / `ready` / `error` / `empty` / `unavailable`, plus the new terminal `closed`. Every state renders something honest; none is a blank.
- **§6 Typed client layer** — no hand-rolled `fetch` is added. The new `listResourceTemplates` seam lands on the INJECTED `MCPAppHostClient` interface the chat module declares, and the Console adapter (`mcp-app-host-client.ts`) is the only place that knows about `$lib/protocol` (D-091 encapsulation; D-173 no-direct-transport). The typed `MCPAppToolNotFoundError` is declared in the chat module and raised by the adapter, so the Protocol error taxonomy never leaks across the boundary.
- **§7 Design tokens** — the frame's growth envelope is expressed as tokens, not literals: the existing `--size-app-inline-min` plus a new `--size-app-inline-max` in the single `tokens.css` surface, declared in the chat module's `tokens.contract.json` so the encapsulation guard keeps it resolvable for a second consumer. No raw px/rem enters a `.svelte` file (the app-reported height is a runtime value, bounded by the tokens).
- **§8 Error handling** — failures are surfaced, never swallowed: an unresolvable tool is a typed error the App can branch on, a failed teardown logs and still closes, and a nonsense size report is ignored in favour of the last valid one.
- **Svelte 5 runes only** — `$state` / `$derived` / `$effect` throughout; no `$:`, no `export let`, no store auto-subscription. `svelte-check --fail-on-warnings` is clean.

## Risks / open questions

- **The prefix alone does not confine — that is why the registration guard exists.** `<sourceID>_<tool>` is a single-underscore join and neither side is charset-constrained, so `github` + `github_enterprise` makes it non-injective and an App on the shorter id reaches the longer id's tools with every downstream gate approving. The plan's own claim was originally stronger than the code delivered. `mcp.Registry.CheckServerIDUnambiguous` refuses the ambiguous pairing at registration (both directions, both attach paths, enforced under `Register`'s write lock), which restores injectivity and makes the control sound. The claim is now stated with its precondition attached, in D-351 and the glossary.
- **A stronger backend argument exists and is deferred deliberately, not dismissed.** The Runtime can compare `desc.Tool.Source` EXACTLY — a check the Console structurally cannot perform, since it can only manipulate strings, which is precisely why it needs a naming convention plus a registration guard. That argument needs a Console-supplied wire `server_id` and is an architecture change; it is recorded in D-351's Follow-ups for a future wave.
- **The confinement placement (D-227 item 3) is re-affirmed FOR NOW, not reversed.** HA-41 asked whether the revert proves the property belongs on the backend instead. The revert-fragility argument does not carry it: the trust boundary the confinement defends is **App to Console host**, not Console to Runtime. The App is sandboxed code the Console mediates for; the Console is a Protocol client acting under the user's own identity, and that same user can already call any catalog tool directly through `tools.call`. A `server_id` on the wire would be supplied by the very component the control is not defending against — the Console — so it adds tamper-evidence in Go tests but no new adversary boundary, while adding a Protocol field an App could eventually be allowed to influence. The revert-fragility that motivated the question is a PROCESS failure (a gate that reported SKIP instead of FAIL), and it is fixed as a process: a hard smoke assertion on both the qualifier's existence and its use, a vitest confinement case, and a Go reflection guard that the wire request never grows a server scope.
- **The frame clamp is CSS, not JavaScript.** A JS clamp would be one edit from unbounded and would need the token values in pixels. `min-height` / `max-height` on the frame means any reported height — 5 px or 500 000 px — lands inside the host's envelope by construction, and the bound stays a design token.
- **The teardown request is a round-trip into a sandboxed iframe.** A wedged App must never pin a Svelte effect cleanup open, so the request carries a short timeout and a failure logs and proceeds to close. Closing the transport is the guarantee; the graceful notice is the courtesy.
- **`--size-app-inline-max: 40rem` is a judgement call.** It is the first host-owned bound; if real Apps routinely need more, raising the token is a one-line change with no code impact.

## Glossary additions

- **App-host tool-namespace confinement** — added to `docs/glossary.md`.
- **Host obligation (MCP Apps)** — added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A: no identity-scoped storage or filter changed; the app-call path's existing identity gate is untouched and still asserted by the AppsSurface identity tests.
- [x] **If this phase builds a reusable artifact** — N/A: no Go reusable artifact is added. The Console-side lifecycle analogue (one bridge per mount under churn) is proven by the D-342 guards in `mcp-app-host-obligations.spec.ts`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — the real-App Playwright handshake spec drives the vendored `@modelcontextprotocol/ext-apps` client end-to-end over a real sandboxed iframe, covering both the happy path and the not-initialized teardown gate.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A — none departed.
