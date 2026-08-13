# Phase 238 — App-only callback catalog (HA-56)

## Summary
Retain App-only callbacks at discovery while keeping them out of the planner projection. Dispatch them only through the matching Harbor host-derived server catalog and existing policy gates.

## RFC anchor
- RFC §6.4
- RFC §7.3
- RFC §5.2
- RFC §7

## Briefs informing this phase
- brief 03
- brief 14

## Brief findings incorporated
- brief 03 §1: source pluggability and provenance are uniform across transports.
- brief 14 §6: App content is sandboxed and bridge-proxied, not a runtime authorization shortcut.
- brief 14 §3: advertised capability must match serviced behavior.

## Findings I'm departing from (if any)
- None.

## Goals
- Implement D-412's per-server callback catalog and atomic dual projection.
- Preserve identity, agent reach, capability, OAuth/approval, current-state, redaction, and audit gates.

## Non-goals
- No provider exceptions, generic planner exposure, string-only authorization, or progressive streaming.

## Acceptance criteria
- [ ] Real SDK/spec-derived fixture publishes ordinary plus `_meta.ui.visibility:["app"]`; only ordinary is planner-visible and matching App callback succeeds.
- [ ] Cross-server, generic planner, missing/mismatched server identity fail before invocation with zero callback executions.
- [ ] Attach/reconnect/refresh/replacement/detach update both views atomically for HTTP and stdio; no stale entry.
- [ ] Paused/disabled/scope-filtered sources cannot bypass filtering; OAuth/approval gates still execute.
- [ ] Mixed identity/agent-reach concurrent tests prove no cross-talk.
- [ ] Compatibility transcripts cover supported metadata variants and rendered App behavior.

## Files added or changed
- `internal/tools/{tools.go,catalog.go,planner_view.go}`
- `internal/tools/drivers/mcp/{content.go,registry.go,fixtures_test.go}`
- `internal/mcpconsole/{apps.go,catalog.go,*_test.go}`
- `internal/protocol/{apps.go,types,mcp.go}` if additive identity is needed
- `test/integration/mcp_app_callback_catalog_test.go`
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`, `scripts/smoke/phase-238.sh`

## Public API surface
- Internal per-server App dispatch interface; any wire change is additive and host-derived.
- Existing planner catalog remains callback-free; Protocol clients never access internal descriptors.

## Test plan
- **Unit:** metadata parsing, dual projection, typed not-found/denial, refresh replacement, gate ordering.
- **Integration:** real MCP SDK fixtures and rendered App bridge over HTTP/stdio; identity and forced paused/denied failure.
- **Conformance:** provider transcript matrix for all named compatibility fixtures.
- **Concurrency / leak:** N=128 shared catalog refresh/dispatch calls with cancellation and teardown leak assertions.

## Smoke script additions
- Static checks for visibility parsing, separate catalogs, host-derived identity, both transports, compatibility fixture names, and no planner callback exposure.

## Coverage target
- `internal/tools/drivers/mcp`: 90%; `internal/mcpconsole`: 90%; protocol adapter: 85%; integration: 80%.

## Dependencies
- 207, 204, 109k, 109l. Independent of 236, 237, 239, 240, 241, 242.

## Risks / open questions
- Any additive wire identity must be mirrored by hand-maintained TS types and generated manifests/docs; otherwise keep it internal.
- Compatibility transcripts must be captured from real SDK/provider output, not self-consistent fixtures.

## Validation gate ledger
- **Local skip:** live provider compatibility probes may skip without explicit live-MCP configuration; committed real transcripts and local transport tests are required.
- **Web CI:** Console/App bridge checks and Protocol lockstep are mandatory when wire or Console files change; Go race/conformance is mandatory.

## Glossary additions
- **App-only callback catalog** — Harbor's per-server callback lookup retained beside, but never merged into, the planner projection.

## Pre-merge checklist
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real-driver/spec-derived integration with failure mode under `-race`
- [ ] Glossary updated
