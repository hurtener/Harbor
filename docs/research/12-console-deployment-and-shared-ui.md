# Research Brief 12 — Console deployment posture + shared UI library

**Date:** 2026-05-15
**Status:** Settled deployment-posture brief — recording the decisions in D-091 / D-092 / D-093 with the surrounding context a future Console-phase planner needs. Companion to Brief 11 (which inventoried the Console's feature surface). When Brief 11 and this brief conflict, this brief wins for deployment / packaging / shared-library questions; Brief 11 wins for feature / view / mockup questions.

## Why this brief exists

Brief 11 was authored before the operator settled the Console's deployment posture and the future packed-dev-UI question. It assumed "the Console" was one product with one deployment path. The settled answer is **two surfaces sharing one component library**:

1. **The full Console** — operator / admin tool, multi-runtime, all 14 pages from Brief 11, deployed via the `harbor console` subcommand.
2. **A future packed dev UI** — single-agent developer tool, deployed embedded in `harbor dev`. Post-V1. Reuses the full Console's chat + playground + MCP-Apps renderer components via a shared library.

This brief records that posture so the Console-wave plan authors have a coherent model to plan against. It also closes the open questions in Brief 11 §"Open architectural questions" #6 (multi-runtime auth storage) and #7 (cross-runtime fleet view scope), and pins the new `harbor console` subcommand phase Brief 11 §"Phase-mapping summary" flagged as "genuinely new."

## The two-surface model

| Surface | Binary | Audience | Scope | Deployed via |
|---|---|---|---|---|
| Full Console | `harbor console` | Operators / admins | All 14 pages (Brief 11), multi-runtime fleet view, intervention queue, playground, settings | Static SvelteKit build baked into `cmd/harbor` via `embed.FS` |
| Packed dev UI (post-V1) | `harbor dev --ui` (or equivalent) | Developers iterating on ONE agent | Single-agent subset: chat + playground + traces + logs + minimal topology. NO fleet view, NO multi-agent, NO cross-runtime UI | Same `embed.FS` mechanism, but mounted by `harbor dev` instead of `harbor console`; same static build, smaller route tree exposed |

Both surfaces:

- Built with SvelteKit + Tailwind + GSAP + adapter-static (CLAUDE.md §4.5 #1).
- Use Svelte 5 + runes mode exclusively (D-092).
- Reference design tokens from a single location (CLAUDE.md §4.5 #3); raw color/spacing literals in `.svelte` rejected (§13).
- Talk to runtimes ONLY through the typed Protocol client (D-093 — generated from `CanonicalWireTypes`).
- NEVER read Runtime Go types directly (CLAUDE.md §4.5 #10).

Neither surface:

- Is served by `harbor dev` in V1. `harbor dev` (Phase 64, shipped) is headless.
- Is a side-channel: every operation flows through canonical Protocol methods.

## The shared chat / playground library

The Playground (Brief 11 §PG-1 .. PG-7), the chat composer, the MCP-Apps content renderer registry (Brief 11 §PG-3), the file-upload pipeline (Brief 11 §PG-2), the rich-output renderers (Brief 11 §PG-4), and the trace toggle (Brief 11 §PG-7) are a single component library used by both surfaces.

**Location at first ship:** `web/console/src/lib/chat/`. Encapsulated inside the Console initially; extracted to `web/shared/chat/` when the second consumer (the packed dev UI) materialises post-V1. The encapsulate-first-extract-on-second-consumer pattern matches the §4.4 driver-seam rule.

**Hygiene rules enforced by the phase that introduces the module:**

1. The chat module imports ONLY: the typed Protocol client (`protocol.ts`), the design tokens (`tokens.css`), Skeleton primitives, and its own internals.
2. The chat module exposes a typed `ProtocolClient` interface that callers inject. It NEVER imports a Console-specific singleton (no `import { runtimeStore } from '../stores/runtime'`).
3. The MCP-Apps renderer registry (Brief 11 §PG-3) lives at `web/console/src/lib/chat/renderers/{markdown,image,audio,link,embedded}.svelte` with a typed dispatch table.
4. File-upload pipeline routes through the Artifact store via the Protocol (Brief 11 §PG-2 + Phases 17–19). The chat module NEVER touches a filesystem path directly.
5. The trace toggle (Brief 11 §PG-7) consumes the Console's topology renderer through an injected interface, NOT a direct import.

These rules make the future `git mv` to `web/shared/chat/` mechanical: the module is already a self-contained library, just located inside the Console for V1.

## Why `harbor console`, not `harbor dev`, serves the Console

Three reasons, in priority order:

1. **Decoupling principle (CLAUDE.md §4.5 #2).** The Runtime is headless. The Console can be co-located, remote, or third-party. Embedding the Console into `harbor dev` would couple a developer's iteration loop to the operator-facing observability tool — wrong scope, wrong default.
2. **Multi-runtime support (Brief 11 §CC-1).** The Console connects to N runtimes simultaneously. A `harbor dev` instance has a 1:1 relationship with ITS runtime; bolting a multi-runtime UI onto a single-runtime developer command is architecturally confused.
3. **Audience separation.** Operators run `harbor console` against production / staging runtimes. Developers run `harbor dev` against their local iteration loop. The tools should be discoverable independently.

The future packed dev UI in `harbor dev` is NOT a contradiction — it ships a SUBSET of the Console surface (single-agent chat + traces + logs), explicitly NOT the full Console. The full Console remains `harbor console`.

## `harbor console` subcommand — what the future phase delivers

A new phase (slotted at Console-wave re-decomposition time; not yet numbered in the master plan):

- `cmd/harbor/cmd_console.go` (new) — cobra subcommand body.
- `cmd/harbor/console_assets.go` (new) — `embed.FS` over `web/console/build/` baked at compile time. The `make build` target runs `npm run build` in `web/console/` first; the Go build picks up the static output.
- Multi-runtime config: reads `~/.harbor/console.yaml` (or `--config <path>`) listing runtime endpoints + auth; accepts `--runtime <name>=<url>` for ad-hoc additions. Browser-side state (Brief 11 §CC-1) layers on top.
- Lifecycle: foreground process; ctrl-C exits. The subcommand prints the local URL (`Console serving on http://127.0.0.1:<port>`) and the runtimes it bootstrapped.
- Auth-storage threat model (Brief 11 §"Open architectural questions" #6 resolved here): per-runtime JWTs stored in browser `localStorage` / `IndexedDB`, encrypted via WebCrypto API with a passphrase the operator enters at first runtime-attach. Loss of passphrase invalidates stored tokens but does NOT corrupt other state. AES-GCM with PBKDF2-derived KEK is the obvious algorithm starting point; the phase plan owns the exact pin.
- The phase's smoke MUST exercise: (a) subcommand starts, (b) `/` serves the static index, (c) `protocol.ts`'s typed methods round-trip against a Phase 60 `httptest.Server`.

## Mockup asset inventory

Console design reference images (binding for the views they depict):

| Asset | Depicts | Source location |
|---|---|---|
| `docs/rfc/assets/console-agents-page.png` | Agents page (Brief 11 §"Agents view") | Operator mockup |
| `docs/research/console-mockup-runtime-view.png` | Live Runtime page (Brief 11 §"Live Runtime view") | Operator mockup |

The canonical home for Console design assets is `docs/rfc/assets/`. The runtime-view mockup is in `docs/research/` for historical reasons (predates Brief 12) and should migrate to `docs/rfc/assets/` when convenient — not blocking. Future Console mockups land in `docs/rfc/assets/console-<view>.png`.

A phase plan that ships a Console page or component MUST reference the mockup(s) for the view(s) it ships. The §17.7 dispatch prompt for Console phases names the asset paths explicitly.

## Open architectural questions Brief 11 raised, resolved here

| Brief 11 question | Resolution in this brief |
|---|---|
| §"Open architectural questions" #5 — Playground session `Kind` field | **Adopt.** `session.kind = production \| dev` lands as a session-metadata field. Dev sessions count against ceilings but split on cost dashboards. The first Console SvelteKit phase that touches sessions wires this. |
| §"Open architectural questions" #6 — multi-runtime auth storage | **Resolved.** Per-runtime JWTs in browser `localStorage` / `IndexedDB`, WebCrypto-encrypted with operator passphrase. Documented in the `harbor console` phase plan. |
| §"Open architectural questions" #7 — cross-runtime fleet view scope | **Console-side aggregator for V1**, as Brief 11 recommended. The `harbor console` subcommand maintains N persistent Protocol connections; fleet views aggregate client-side. Gateway pattern post-V1. |
| §"Phase-mapping summary" "two genuinely new things" — Agent management surface phase | **Required.** Lands as part of the Console wave's re-decomposition (the Agents page from Brief 11). Depends on the Agent Registry (Phase 53a, shipped). |
| §"Phase-mapping summary" "two genuinely new things" — Notification taxonomy | **Required.** A `notification.*` Protocol topic ships as a named acceptance criterion of the Overview page phase (Brief 11 §"Overview" — implicit; the intervention queue surfaces notifications). Source events stay at their original types; the `notification.*` topic is a runtime-internal mapper layer that decides which events also become user-facing notifications. |

## Open questions this brief does NOT resolve

- **Whether `harbor console`'s embedded build is always-current** (the binary always ships the `web/console/build/` produced by the same git commit that produced the Go binary) or **operator-overrideable** (a `--static-dir` flag for development against a live `npm run dev`). Recommendation: always-current default; `--static-dir` as a developer-only escape hatch behind a build tag or env var. Settle in the phase plan.
- **The full route shape of `harbor console`'s SvelteKit app.** Brief 11's 14 pages map to SvelteKit routes; the exact tree (which pages are top-level, which nest under shared layouts) is a Phase 72-onward call.
- **The packed dev UI's exact subset of the Console surface.** Post-V1. Sketched here as "chat + playground + traces + logs + minimal topology" but the precise feature inventory waits for the phase plan.
- **The Skeleton component mapping per page.** Phase 72+ owns this; the brief intentionally does not dictate which Skeleton primitive backs which view.

## What this brief does NOT do

- It does not specify the SvelteKit route tree (Phase 72+ owns this).
- It does not pick the WebCrypto algorithm for auth-storage (the `harbor console` phase plan owns this — AES-GCM with PBKDF2-derived KEK is the obvious starting point but not pinned here).
- It does not enumerate every MCP-Apps content type (Brief 11 §PG-3 owns this).
- It does not propose Protocol method signatures (the Protocol phases own this).

## Re-discussion checklist

When the Console wave is re-decomposed (per the master-plan pre-plan note immediately following this brief's introduction), confirm:

- [ ] A `harbor console` subcommand phase is in the wave's stage list, depending on (Phase 60 wire transport, Phase 61 auth, the first Console SvelteKit phase).
- [ ] The first Console SvelteKit phase creates `web/console/` with: `svelte.config.js` runes mode, `package.json` pinning Svelte 5, `tokens.css`, `protocol.ts` (generated), `stylelint` config rejecting raw literals.
- [ ] The Playground / chat-module phase enforces the encapsulation hygiene rules from §"The shared chat / playground library" above.
- [ ] Session `Kind` field is wired in the first Console SvelteKit phase that touches sessions.
- [ ] Each Console page phase references its mockup asset (when one exists).
- [ ] The `notification.*` Protocol topic lands as a named acceptance criterion of the Overview page phase, not as a free-floating primitive.

## Source artifacts referenced

- Brief 11 (`docs/research/11-console-feature-surface.md`) — Console feature surface.
- CLAUDE.md §4.5 (Console / Protocol-client conventions, updated in this PR).
- CLAUDE.md §13 (Forbidden practices — Console-side bullets).
- `docs/decisions.md` — D-091 (deployment posture), D-092 (Svelte 5 + runes), D-093 (`protocol.ts` generation).
- `docs/rfc/assets/console-agents-page.png`, `docs/research/console-mockup-runtime-view.png` — mockup assets.
- Master plan (`docs/plans/README.md`) — Console-wave deployment + shared-library posture pre-plan note (immediately following the existing Phases 72–75 page-decomposition note).
