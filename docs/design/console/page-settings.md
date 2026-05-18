# Console page — Settings

**Slug:** `settings` &middot; **Sidebar cluster:** Settings &middot; **Route:** `/console/settings`
**Mockup:** TBD — this spec drives mockup authoring

## 1. Purpose

Settings is the per-operator and per-runtime configuration surface — the page that holds everything Console-local (theme, density, keybindings, notification routing, saved-view defaults) plus the per-operator runtime-attach configuration (which Harbor runtimes the Console is connected to, per-runtime JWTs encrypted via WebCrypto per D-091, per-user OAuth tokens for `ScopeUser` tool bindings). It also surfaces operator-facing runtime configuration that's safe to read but not edit from the Console (the runtime's governance config, the active LLM provider, the storage drivers in use). The page is split into sections — most are local-only per D-061; a handful are runtime-side reads.

## 2. Where it sits in the IA

Settings sits alone in the **Settings** cluster (Settings → Settings). It is reached from the sidebar, from the top-bar user menu, from the runtime context chip's "Open Settings", from any per-page banner suggesting "Open Settings → Connected Runtimes" on connection failure, and from the keyboard shortcut palette. Breadcrumb: `<runtime> / Settings`.

## 3. Functionality matrix

- **Section: Connected Runtimes — list of attached runtimes; add new; remove; per-runtime status (Connected / Disconnected / Auth-Failed).** `[shipped]` Local Console-side state per D-091; multi-runtime context per Brief 11 §CC-1. Per-runtime JWT stored encrypted in browser localStorage / IndexedDB via WebCrypto with passphrase entered at first attach (D-091).
- **Section: Per-runtime auth — re-enter WebCrypto passphrase; rotate stored JWT; revoke stored JWT.** `[shipped]` Console-local; the runtime itself is unaware of the Console's per-runtime auth UX — it just receives a `Bearer` per request and validates it per Phase 61 (`internal/protocol/auth`).
- **Section: API tokens (per-user OAuth) — operator's `ScopeUser` OAuth tokens for tool bindings.** `[shipped]` Same Connect / Reconnect / Revoke flow as MCP Connections page's OAuth bindings, but scoped to the operator's own user-bound tokens. Subscribe to `tool.auth_required` / `tool.auth_completed` events filtered to the operator.
- **Section: Theme — Light / Dark / System.** `[shipped]` Console-local; tokens-driven per CLAUDE.md §4.5 #3 (`web/console/src/lib/tokens.css`).
- **Section: Density — Comfortable / Compact.** `[shipped]` Console-local.
- **Section: Keybindings — customisable shortcuts per Skills `keybindings-help`.** `[shipped]` Console-local; persists in Console DB per D-061.
- **Section: Time zone + locale — for date/time rendering across the Console.** `[shipped]` Console-local.
- **Section: Notifications routing — which notification types trigger email / Slack / web-push for this operator.** `[wave-13-extends]` UI is Console-local, but the underlying `notification.*` Protocol topic (Brief 11 §CC-3) is `[wave-13-extends]` — Wave 13 must land the topic.
- **Section: Runtime info (per active runtime, read-only) — Protocol version (`types.ProtocolVersion`), Protocol capabilities (`types.Capabilities()`), active deprecations (`types.Deprecations()` per D-077), runtime build hash, runtime uptime.** `[shipped]` Read via `types.VersionHandshake` from the negotiation entry point (Phase 60 / D-077).
- **Section: Governance posture (per active runtime, read-only) — currently active per-identity cost ceilings, rate limits, MaxTokens defaults from `IdentityTiers` per D-081.** `[wave-13-extends]` `governance.posture` Protocol method (NEW) returning the currently active per-tier configuration. (Brief 11 §"Settings view" calls out per-user-route notifications and per-runtime status; governance posture is a natural Settings home.)
- **Section: Storage drivers (per active runtime, read-only) — which `StateStore` / `ArtifactStore` / `MemoryStore` / `SkillStore` drivers are active.** `[wave-13-extends]` Either an extended `types.VersionHandshake` payload OR a separate `runtime.drivers` snapshot method (NEW — Wave 13 to decide).
- **Section: LLM provider posture (per active runtime, read-only) — active LLM provider name, model registry, dev-mock indicator (the `[DEV-ONLY MOCK LLM]` banner per the §13 amendment).** `[wave-13-extends]` `llm.posture` Protocol method (NEW) or extend `runtime.drivers`.
- **Section: About — Console version, build commit, Protocol version negotiated with each runtime, license (Apache-2.0 per D-014).** `[shipped]` Console-local + per-runtime `VersionHandshake`.
- **Section: Console-driven governance actions (Post-V1) — key rotation via `governance.rotate_key` (Phase 91, `Post-V1`), mid-session model swap via `governance.swap_model` (Phase 92, `Post-V1`).** `[deferred]` Mockup may gesture as "future" affordances; not in V1.
- **No Priority field rendered anywhere.** `[deferred]` D-065 invariant preserved.
- **No "embed in `harbor dev`" toggle.** `[deferred]` D-091 — Console is served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev`; the Console is not configurable to be embedded in `harbor dev` (a future packed dev UI is a SEPARATE post-V1 surface, not a Settings toggle).
- **Saved-view defaults — per-page default filters / sorts.** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page):
  - Row 1 — section-nav rail (left, persistent) listing the section names.
  - Row 2 — selected section content (right, full canvas).
- **Right rail** (per-page): empty (the section-nav rail and the section content fill the page).
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Section-nav rail | static (Console-local) | Click → switch section (local UI state) | `[shipped]` |
| Connected Runtimes section | Console DB (per-runtime endpoint + encrypted JWT) | Add / Remove / Re-attach (local UI state) | `[shipped]` |
| Per-runtime auth section | Console DB | Re-enter passphrase / rotate JWT / Revoke (local UI state; WebCrypto operations) | `[shipped]` |
| API tokens (per-user OAuth) section | `tool.auth_required` / `tool.auth_completed` events filtered to user; binding metadata from `tools.get` (NEW) | Connect / Reconnect / Revoke per `ScopeUser` binding | `[wave-13-extends]` |
| Theme / Density / Keybindings / TZ / locale sections | Console DB (local) | Set / Reset (local UI state only) | `[shipped]` |
| Notifications routing section | Console DB (local) + `notification.*` topic (NEW) for subscribe registration | Set per-type routing (local UI state); subscribe / unsubscribe on the runtime topic | `[wave-13-extends]` |
| Runtime info section | `types.VersionHandshake` (Phase 59 / D-077) | none (read-only) | `[shipped]` |
| Governance posture section | `governance.posture` (NEW) | "View ceilings detail" (local) | `[wave-13-extends]` |
| Storage drivers section | `runtime.drivers` (NEW) OR `VersionHandshake` extended | none (read-only) | `[wave-13-extends]` |
| LLM provider posture section | `llm.posture` (NEW) | none (read-only) | `[wave-13-extends]` |
| About section | Console-local + per-runtime `VersionHandshake` | none | `[shipped]` |
| Key-rotation action (Post-V1) | `governance.rotate_key` (Phase 91, `Post-V1`) | Submit (Post-V1) | `[deferred]` |
| Model-swap action (Post-V1) | `governance.swap_model` (Phase 92, `Post-V1`) | Submit (Post-V1) | `[deferred]` |

## 6. Controls + actions

- **Section-nav:** click section → switch (local UI state).
- **Connected Runtimes actions:** Add (form: name + URL + passphrase); Remove (confirms); Re-attach (re-enter passphrase).
- **Per-runtime auth actions:** Re-enter passphrase; Rotate JWT (upload new bearer); Revoke (clear stored encryption).
- **API tokens actions:** Connect (OAuth popup); Reconnect; Revoke.
- **Theme / Density / Keybindings / Notifications routing:** local form controls.
- **Keyboard shortcuts:** `,` (comma) opens Settings (operator-rebindable per Brief 11 §CC-5); `Esc` close.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| No runtimes attached | First Console launch, no `~/.harbor/console.yaml` and no manual adds | "Add your first runtime" CTA on the Connected Runtimes section | Click Add |
| Runtime unreachable | Stored endpoint refuses connection | Red badge on the runtime row + "Test connection" / "Remove" | Test / Remove |
| Auth failed | Stored JWT rejected at `Bearer` verification | Yellow badge: "Authentication failed — re-enter token or passphrase" | Re-auth |
| Passphrase wrong | WebCrypto decrypt failed | Inline error on the re-attach modal: "Wrong passphrase — try again or revoke" | Retry or revoke |
| `governance.posture` / `runtime.drivers` / `llm.posture` not yet shipped | Wave 13 in flight | Section renders empty placeholder: "Coming in Wave 13" + link to spec | Wait for Wave 13 |
| Protocol error — `CodeAuthRejected` on action | JWT expired mid-action | Banner + re-auth | Re-enter passphrase |
| Protocol error — `CodeScopeMismatch` on Post-V1 governance actions | Operator lacks scope (when those land) | Inline error | Request elevated scope |

## 8. Multi-tenant / multi-runtime nuances

Settings is the page where the multi-runtime model is most exposed: the Connected Runtimes section IS the per-operator multi-runtime configuration. Each attached runtime has its own JWT, its own scope claims, its own per-runtime `VersionHandshake`, its own storage / governance / LLM posture. Console-local settings (theme, density, keybindings) are global across attached runtimes; per-runtime auth and OAuth tokens are per-runtime by construction. The Console is served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev`, per D-091 — Settings does NOT include an "embed in `harbor dev`" toggle because that's architecturally rejected (D-091).

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — read all Console-local settings, manage own `ScopeUser` OAuth tokens, read per-runtime `VersionHandshake`.
- `admin` (`auth.ScopeAdmin`) — required to read full `governance.posture` (per-tenant tier configuration), `runtime.drivers`, `llm.posture` when those land in Wave 13; required to perform Post-V1 governance actions (`rotate_key`, `swap_model`).
- `console:fleet` (`auth.ScopeConsoleFleet`) — required to read cross-runtime aggregates (post-V1 fleet posture).
- **Post-V1 control verbs (`rotate_key`, `swap_model`)** require the control-scope claim per D-066 — strictly more elevated than ordinary `admin`.

## 10. Out of V1 (deferred)

- **Console-driven key rotation.** Phase 91 (`Post-V1`); `governance.rotate_key` Protocol method.
- **Console-driven mid-session model swap.** Phase 92 (`Post-V1`); `governance.swap_model` Protocol method.
- **Per-tenant Console theming.** Brief 11 §"Open architectural questions" #9 — post-V1.
- **Cross-runtime aggregator (fleet posture).** D-091 — post-V1.
- **Embed Console in `harbor dev`.** Architecturally rejected per D-091 (couples developer iteration to operator observability; wrong scope). A future packed dev UI is a separate post-V1 surface, NOT a Settings toggle.
- **Priority field rendered anywhere.** D-065 invariant preserved.

## 11. References

- Brief 11 §"Settings view", §CC-1 (multi-runtime), §CC-2 (identity-aware UI), §CC-3 (notifications routing), §CC-6 (theme / density / accessibility).
- Brief 12 §"Why `harbor console`, not `harbor dev`, serves the Console", §"`harbor console` subcommand — what the future phase delivers" (auth-storage threat model).
- RFC-001-Harbor.md §5.3 (versioning), §5.5 (Authentication), §6.15 (Governance — cost ceilings + rate limits + MaxTokens), §7 (Console).
- Decisions: D-014 (Apache-2.0 license), D-061 (Console DB local-only), D-065 (no session priority — invariant), D-066 (control claim), D-077 (Protocol versioning + capabilities + deprecations), D-079 (Protocol auth scope claims), D-081 (governance config consolidation — `IdentityTiers`), D-089 (`harbor dev` LLM-default + mock escape hatch — surfaces as the LLM-posture banner), D-091 (Console deployment posture — served by `harbor console` via `embed.FS`, NOT `harbor dev`), D-092 (Svelte 5 + runes — not exposed in UI), D-093 (`protocol.ts` generated — not exposed in UI).
- Phase plan: phase 36a (Cost accumulator + per-identity ceilings — `Shipped`), phase 36b (Per-identity rate limits + MaxTokens — `Shipped`), phase 59 (Protocol versioning + deprecation policy — `Shipped`), phase 61 (Protocol auth — `Shipped`), phase 91 (Console-driven key rotation — `Post-V1`), phase 92 (Console-driven mid-session model swap — `Post-V1`).
- Glossary terms used: `Console`, `Console DB`, `Protocol`, `Protocol version`, `Deprecation window`, `Scope claim`, `Fleet control / fleet observation`, `HARBOR_DEV_TOKEN`.
