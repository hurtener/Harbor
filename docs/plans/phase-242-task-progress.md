# Phase 242 — Task progress (HA-60)

## Summary
Expose durable task progress as a typed, identity-safe Protocol projection. Progress reports completed and active step tranches, forwarded artifact handles, and resumability without leaking raw outputs.

## RFC anchor
- RFC §6.10
- RFC §6.11
- RFC §7.3
- RFC §5.2

## Briefs informing this phase
- brief 14
- brief 03

## Brief findings incorporated
- brief 14 §6: App delivery remains sandboxed and bridge-proxied.
- brief 03 §8: StateStore is the persistence seam and identity is propagated through every provider call.
- brief 14 §3: `not_found` must be an honest, typed state rather than silent degradation.

## Findings I'm departing from (if any)
- None.

## Goals
- Enforce D-416's chosen session-lifetime policy, or adopt measured bounded eviction only if growth evidence requires it.
- Align godoc, StateStore lifecycle fences, Protocol mapping, and Console placeholder behavior.

## Non-goals
- No raw output streaming, unrelated artifact retention, new identity axis, or second progress source.

## Acceptance criteria
- [ ] Baseline tests prove records survive session reopen and unknown/cross-identity ids return typed not-found.
- [ ] Chosen policy is explicit and enforced; rejected policy's guard rails and rationale are documented.
- [ ] If eviction is selected, TTL/sweep is bounded, identity-scoped, race-safe, and reader/writer stress-tested.
- [ ] Console's D-348 honest placeholder is unchanged for not-found.
- [ ] Real durable StateStore integration proves identity propagation, failure mode, and cross-session isolation; erasure removes records.

## Files added or changed
- `internal/mcpconsole/toolcontext.{go,test.go}`
- `internal/state` lifecycle adapters and conformance tests only if required
- `internal/protocol/mcp.go` only if a bounded retention signal is necessary
- `test/integration/mcp_app_tool_context_retention_test.go`
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`, `scripts/smoke/phase-242.sh`

## Public API surface
- Default: no wire change; typed `CodeNotFound` retains its existing meaning for unknown/cross-identity records. An eviction signal is additive only if selected.

## Test plan
- **Unit:** policy, reopen, unknown/cross-identity miss, erasure, placeholder contract.
- **Integration:** real durable StateStore and App accessor with identity and forced read failure.
- **Conformance:** in-memory, SQLite, and Postgres lifecycle/erasure behavior where drivers are enabled.
- **Concurrency / leak:** if eviction is selected, N≥100 readers/writers under `-race`; otherwise N≥100 shared accessor reads and close/reopen lifecycle checks.

## Smoke script additions
- Static assertions for D-416, session-lifetime default, erasure fence, typed not-found, unchanged placeholder, and optional eviction ledger. No script execution in this planning change.

## Coverage target
- `internal/mcpconsole`: 90%; StateStore touched adapters: 90%; integration: 85%.

## Dependencies
- 204, 207, 233a. Independent of 236–241; compatible with 238's callback catalog.

## Risks / open questions
- Long-lived sessions may grow without bound; measure before changing the default, and do not claim eviction without a real sweeper.
- Postgres integration may be local-skip without `HARBOR_PG_DSN`; CI must run the durable matrix.

## Validation gate ledger
- **Local skip:** Postgres durable lifecycle checks may skip only without `HARBOR_PG_DSN`; in-memory/SQLite erasure and isolation checks are local-required.
- **Web CI:** all three StateStore conformance paths, race integration, Protocol error lockstep if changed, and Console placeholder regression are required.

## Glossary additions
- **Task progress** — a bounded, identity-addressed projection of a task's tranche state and resumability.

## Pre-merge checklist
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real durable-driver integration with identity and failure mode under `-race`
- [ ] Glossary updated
