# Phase 240 — Virtual child profiles (HA-58)

## Summary

Expose virtual child profiles derived from a governed parent profile. A child is a read-only, identity-addressed view with explicit overrides and no independent persistence or authority escalation.

## RFC anchor

- RFC §6.7
- RFC §6.16
- RFC §5.2
- RFC §7


## Briefs informing this phase

- brief 04


## Brief findings incorporated

- brief 04 §4.2: identity is mandatory and cross-key reads are impossible by API construction.
- brief 04 §4.5: capability filtering and redaction happen before injection.
- brief 04 §6: concurrent cross-session isolation is a required proof, not an optimization.


## Findings I'm departing from (if any)

- None.


## Goals

- Add D-414's pure preview projection, server-derived authority, typed states, and lockstep contract.
- Guarantee preview and next-run composition use one resolver.


## Non-goals

- No independent child mutation, revision advance, run creation, cross-principal bodies, or second profile resolver.


## Acceptance criteria

- [ ] Operator preview matches actual next-run composition against the real durable store.
- [ ] Ordinary callers see only their own; same-tenant other-user and cross-tenant calls disclose no names.
- [ ] Revoked/unselected packs return typed not-found; unwired/legacy state returns `unavailable`.
- [ ] N previews leave revision hash/list, skill rows, and audit byte-identical.
- [ ] N=128 mixed previews under `-race` show no authority/context bleed and include a failure mode.


## Files added or changed

- `internal/runtime/agentcfg/{projection,protocol}/*`
- `internal/protocol/{methods,types,singlesource}/*`
- `test/integration/skill_composition_preview_test.go`
- `docs/skills/use-the-harbor-protocol/SKILL.md`, `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`
- `scripts/smoke/phase-240.sh`


## Public API surface

- One additive read method/projection returning names, bounded verdicts, and typed availability states; bodies are principal-scoped.


## Test plan

- **Unit:** authority matrix, resolver identity, verdict filtering, read-only/idempotence, typed errors.
- **Integration:** real durable store and existing agent-config surface; operator/user denial failure.
- **Conformance:** Protocol JSON and absent-state compatibility across transports.
- **Concurrency / leak:** N=128 previews against one shared resolver; no leaked goroutines or mutable snapshots.


## Smoke script additions

- Static assertions for D-414, one resolver, authority matrix, no mutation, typed states, and lockstep/skill ledger.


## Coverage target

- `internal/runtime/agentcfg`: 90%; Protocol read surface: 90%; integration: 80%.


## Dependencies

- 237; gates 241; independent of 236, 238, 239, and 242.


## Risks / open questions

- Preview authority must derive from verified signed reach, not request-body claims; implementation must fail closed if reach is unavailable.
- Any added wire field triggers D-223/D-209 and hand-maintained Console mirrors.


## Validation gate ledger

- **Local skip:** durable Postgres preview integration may skip only without `HARBOR_PG_DSN`; in-memory/SQLite authority and idempotence tests are required.
- **Web CI:** typed Protocol client, manifest lockstep, Console skill update, and generated reference checks are required if the preview is wire-visible.


## Glossary additions

- **Virtual child profile** — a read-only, derived profile that applies bounded child overrides to a governed parent.


## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real-driver integration with identity and failure mode under `-race`
- [ ] Glossary and operator skill updated
