# Phase 240 — Virtual child profiles (HA-58)

## Summary

Expose governed virtual child profiles derived from a parent profile. D-419 is the phase authority: the child is a read-only, identity-addressed view with bounded overrides and no independent persistence or authority escalation.

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

- Add D-419's governed, read-only virtual child profile derived from a parent.
- Enforce bounded overrides, verified triple authority, and one resolver for run-start and inspection.

## Non-goals

- No capability widening, parent mutation, independent revision, isolation-principal status, cross-principal bodies, or second profile resolver.

## Acceptance criteria

- [ ] Run-start and inspection resolve the same governed child view from the real durable store.
- [ ] Ordinary callers are limited to their verified identity triple; same-tenant other-user and cross-tenant calls disclose no names.
- [ ] Bounded overrides cannot widen capability, mutate the parent, or advance an independent revision.
- [ ] A virtual child profile is never used as an isolation principal.
- [ ] N=128 mixed resolutions under `-race` show no authority/context bleed and include a failure mode.

## Files added or changed

- `internal/runtime/agentcfg/{projection,protocol}/*` and resolver call sites
- `internal/protocol/{methods,types,singlesource}/*` if the inspection surface is wire-visible
- `test/integration/*` for resolver authority and isolation
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`

## Public API surface

- One governed virtual-child projection with bounded overrides and typed authority states; the virtual profile is not an isolation principal.

## Test plan

- **Unit:** authority matrix, resolver identity, verdict filtering, read-only/idempotence, typed errors.
- **Integration:** real durable store and existing agent-config surface; operator/user denial failure.
- **Conformance:** Protocol JSON and absent-state compatibility across transports.
- **Concurrency / leak:** N=128 previews against one shared resolver; no leaked goroutines or mutable snapshots.

## Smoke script additions

- Static assertions for D-419, one resolver, verified triple authority, bounded overrides, no mutation, and lockstep ledger.

## Coverage target

- `internal/runtime/agentcfg`: 90%; Protocol read surface: 90%; integration: 80%.

## Dependencies

- Depends on Phases 237 and 239; gates Phase 241; independent of Phases 236, 238, 241, and 242.

## Risks / open questions

- Preview authority is server-derived: it must derive from verified signed reach restored by the server's reach-admission authority, never from request-body claims or caller-named configuration; implementation must fail closed if reach is unavailable.
- Any added wire field triggers D-223/D-209 and hand-maintained Console mirrors.

## Validation gate ledger

- **Local skip:** local validation intentionally skipped for this documentation reconciliation.
- **Web CI:** typed Protocol client, manifest lockstep, and generated reference checks remain required if the inspection surface is wire-visible.

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
