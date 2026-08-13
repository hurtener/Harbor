# Phase 241 — Artifact and output forwarding (HA-59)

## Summary

Forward task artifacts and bounded outputs across governed virtual-child and same-run task boundaries by reference. D-420 is the phase authority: consumers receive declared handles, while models and unrelated sessions do not receive raw content accidentally.

## RFC anchor

- RFC §6.7
- RFC §6.16
- RFC §7
- RFC §8

## Briefs informing this phase

- brief 04
- brief 06
- brief 11
- brief 12

## Brief findings incorporated

- brief 06 §4: bounded payloads and server-side identity filtering are mandatory.
- brief 11 §CC-2: authorization remains authoritative at the Protocol boundary.
- brief 12 §35–49: any Console consumer remains a typed Protocol client.

## Findings I'm departing from (if any)

- None.

## Goals

- Add virtual-child execution artifacts and bounded output forwarding by reference.
- Preserve authorization and provenance without duplicating raw content.

## Non-goals

- No raw-content forwarding, cross-session exposure, private Console store, composition-preview CLI/Console consumer, or second forwarding mechanism.

## Acceptance criteria

- [ ] A virtual-child execution artifact is created with preserved provenance.
- [ ] Authorized same-run consumers receive bounded output and artifact references only.
- [ ] Unauthorized, erased, cross-session, and cross-tenant references fail closed before bytes are exposed.
- [ ] Raw content is absent from task projections and unrelated model/session context.
- [ ] Real-driver integration proves identity, a failure mode, and N≥10 concurrent stress.

## Files added or changed

- `internal/artifacts/*`, `internal/tasks/*`, and runtime virtual-child execution paths
- `internal/protocol/*` only where the authorized reference surface is wire-visible
- `test/integration/*` for forwarding, provenance, and identity isolation
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`

## Public API surface

- Forwarding uses an authorized artifact reference with preserved provenance and bounded output.
- No CLI or Console composition-preview consumer feature is introduced.

## Test plan

- **Unit:** artifact-reference authorization, provenance, bounded output, and raw-content exclusion.
- **Integration:** virtual-child execution against real artifact/task drivers; identity and denial failure.
- **Conformance:** reference and bounded-output response fixtures across supported Protocol transports.
- **Concurrency / leak:** N≥10 forwarding stress with cancellation and no cross-talk.

## Smoke script additions

- Static assertions for D-420, reference-only forwarding, provenance, raw-content exclusion, identity fences, and Protocol lockstep.

## Coverage target

- `cmd/harbor`: 75%; Console skill view: 85% check coverage; integration: 80%.

## Dependencies

- Depends on Phase 240; independent of Phases 236, 238, 239, and 242.

## Risks / open questions

- CLI and Console must not invent fallback states when the Protocol returns an error; preserve typed status vocabulary.
- Local frontend checks are a skip only when `web/console` dependencies are unavailable; web CI must execute the full gate.

## Validation gate ledger

- **Local skip:** local validation intentionally skipped for this documentation reconciliation.
- **Web CI:** Protocol lockstep, generated references, race integration, and artifact authorization gates remain authoritative; no committed build artifacts.

## Glossary additions

- **Output forwarding** — passing a declared artifact or bounded output by reference to an authorized same-run consumer.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 where applicable
- [ ] Real-driver integration with identity/failure and N≥10 stress under `-race`
- [ ] Glossary and operator skill updated
