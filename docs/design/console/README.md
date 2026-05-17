# Console page specs — Wave 13 mockup authoring inputs

This directory holds one self-contained design specification per Harbor Console page. Each spec captures the page's purpose, IA position, functionality matrix, anatomy, components, controls, empty/loading/error/unauthorized states, multi-tenant / multi-runtime nuances, identity scope claims, and V1 deferrals — at enough depth that an operator can author a mockup against it without re-reading every research brief.

The 14 specs feed the Wave 13 Console-wave mockup-authoring work. They are NOT phase plans (those follow the §16 phase-authoring ritual when Wave 13 re-decomposes phases 72–75 against the full 14-page IA). The Wave 13 dispatch prompt will use these specs as the per-page mandatory-reading anchor, alongside Brief 11 (`docs/research/11-console-feature-surface.md`), Brief 12 (`docs/research/12-console-deployment-and-shared-ui.md`), the two existing mockup assets (`docs/research/console-mockup-runtime-view.png` and `docs/rfc/assets/console-agents-page.png`), CLAUDE.md §4.5, and decisions D-091 / D-092 / D-093.

The Console-wave pre-plan note in `docs/plans/README.md` (around lines 754–764) points to this directory as the authoritative per-page spec source for Wave 13 mockup work.

## Index

| # | Spec file | Page | Sidebar cluster | Mockup status |
|---|---|---|---|---|
| 1 | [`page-overview.md`](page-overview.md) | Overview | Runtime | TBD — drives mockup |
| 2 | [`page-live-runtime.md`](page-live-runtime.md) | Live Runtime | Runtime | `docs/research/console-mockup-runtime-view.png` (legacy location per Brief 12) |
| 3 | [`page-sessions.md`](page-sessions.md) | Sessions | Execution | TBD — drives mockup |
| 4 | [`page-tasks.md`](page-tasks.md) | Tasks | Execution | TBD — drives mockup |
| 5 | [`page-agents.md`](page-agents.md) | Agents | Execution | `docs/rfc/assets/console-agents-page.png` (canonical) |
| 6 | [`page-tools.md`](page-tools.md) | Tools | Execution | TBD — drives mockup |
| 7 | [`page-events.md`](page-events.md) | Events | Execution | TBD — drives mockup |
| 8 | [`page-background-jobs.md`](page-background-jobs.md) | Background Jobs | Execution | TBD — drives mockup |
| 9 | [`page-flows.md`](page-flows.md) | Flows | Resources | TBD — drives mockup |
| 10 | [`page-memory.md`](page-memory.md) | Memory | Resources | TBD — drives mockup |
| 11 | [`page-mcp-connections.md`](page-mcp-connections.md) | MCP Connections | Resources | TBD — drives mockup |
| 12 | [`page-artifacts.md`](page-artifacts.md) | Artifacts | Resources | TBD — drives mockup |
| 13 | [`page-settings.md`](page-settings.md) | Settings | Settings | TBD — drives mockup |
| 14 | [`page-playground.md`](page-playground.md) | Playground (session-level surface) | n/a (not a sidebar entry) | TBD — drives mockup |

## How the specs are organised

Every spec follows the same eleven-section template — the consistency is load-bearing for cross-page comparison. The template's three tag set on every functionality bullet — `[shipped]`, `[wave-13-extends]`, `[deferred]` — is the per-page sense of how much Wave 13 must add to the Protocol surface before the page can ship.

- `[shipped]` — renderable today against the existing Protocol surface. The bullet cites the Protocol method / event type / payload struct that supplies the data (verbatim names from `internal/protocol/methods/`, `internal/*/events.go`, `internal/protocol/types/`).
- `[wave-13-extends]` — needs a Protocol-surface addition Wave 13 must ship. The bullet proposes the method / event / type name and what it returns. The Console page phase that consumes it MUST land in the same wave per D-062's binding rule.
- `[deferred]` — explicitly OUT of V1, with the deferring decision cited (D-063 Flows authoring, D-064 Evaluations, D-065 session priority, D-066 control-claim deferral, etc.).

## Binding invariants every spec preserves

These carve-outs are pinned in every spec so they survive review independently:

- **D-065** — every page that renders Session or Task lists / cards MUST explicitly note "No Priority field rendered — D-065 dropped session-level priority from V1." (Task-level priority via the `prioritize` Protocol method stays shipped.)
- **D-091** — every page that mentions deployment notes "served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev`."
- **D-061** — any saved-views / layouts / annotations bullet cites "Console DB holds Console-local state only — never a shadow source of truth for runtime entities (D-061)."
- **D-062 (runtime-lens principle)** — every page is a projection over `state snapshots + realtime events + control commands`; no standalone features; no privileged hooks.
- **D-066** — control-plane verbs (Approve / Reject / Pause / Drain / Restart / ForceStop / Hard-Cancel) require the more-elevated control-scope claim; observation does not.

## How Wave 13 will consume these specs

Per the §17.7 wave-delivery cadence, the Wave 13 dispatch prompt will:

1. Group the 14 page-spec files into stages by Protocol-surface dependency — pages whose only `[wave-13-extends]` items are independent ride together; pages whose `[wave-13-extends]` items extend a shared method (`tasks.list`, `sessions.list`, `artifacts.list`) wait for that method's phase.
2. Name this directory in every per-page agent's mandatory reading list, alongside Brief 11, Brief 12, the relevant mockup asset (where one exists), CLAUDE.md §4.5 + §13 frontend bullets, and the three Console decisions D-091 / D-092 / D-093.
3. Cap each per-page agent's `[wave-13-extends]` Protocol-surface additions to those declared in the spec — drift between the spec and the phase plan is a §16 drift signal.

## References

- `docs/research/11-console-feature-surface.md` — the verbal decomposition of Live Runtime + Playground + 12 other views.
- `docs/research/12-console-deployment-and-shared-ui.md` — deployment posture, shared chat module, mockup asset inventory.
- `docs/rfc/assets/console-agents-page.png` — Agents page mockup (canonical).
- `docs/research/console-mockup-runtime-view.png` — Live Runtime page mockup (legacy location per Brief 12).
- `RFC-001-Harbor.md` §5 (Harbor Protocol), §7 (Console layer).
- `CLAUDE.md` §4.5 (Console / Protocol-client conventions), §13 (Forbidden practices — frontend bullets).
- `docs/decisions.md` — D-002, D-061, D-062, D-063, D-064, D-065, D-066, D-091, D-092, D-093.
- `docs/plans/README.md` — Console-wave pre-plan note (around lines 754–764), phases 72–75 (`Pending`).
