# Phase 241 — Artifact and output forwarding (HA-59)

## Summary

Forward task artifacts and bounded outputs across child-profile and same-run task boundaries by reference. Consumers receive declared handles, while models and unrelated sessions do not receive raw content accidentally.

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

- brief 06 §3: CLI and Console are Protocol clients of the same canonical surface.
- brief 11 §CC-2: UI gates are convenience; Protocol authorization remains authoritative.
- brief 12 §35–49: Console components use the typed injected client and shared conventions.
## Findings I'm departing from (if any)

- None.
## Goals

- Add `harbor` composition inspection and a Console skill/agent view using only D-414.
- Provide a two-revision diff without duplicating composition logic.
## Non-goals

- No raw-content duplication, cross-session forwarding, private Console store, or second forwarding mechanism.
## Acceptance criteria

- [ ] CLI output matches actual next-run composition against the real durable store.
- [ ] Console renders pack/personal names, verdicts, not-found, denied, and unavailable exactly as returned.
- [ ] Unauthorized CLI/Console calls fail identically to the Protocol method.
- [ ] Two-revision diff shows membership and verdict changes without revealing forbidden bodies.
- [ ] Real-driver integration proves identity, a failure mode, and N≥10 concurrent stress.
## Files added or changed

- `cmd/harbor/cmd_inspect_skill_composition.go` and CLI tests
- `web/console/src/routes/(console)/skills/+page.svelte` and typed protocol client module/tests
- `test/integration/skill_composition_consumers_test.go`
- `docs/skills/use-the-harbor-protocol/SKILL.md`, `docs/glossary.md`, `CHANGELOG.md`
- `scripts/smoke/phase-241.sh`
## Public API surface

- CLI verb and Console route are clients only; D-414 remains the sole runtime surface.
- Forwarding uses an artifact reference with preserved provenance and bounded output.
## Test plan

- **Unit:** CLI formatting/diff and Console state matrix including all typed states.
- **Integration:** CLI + Console client against real Protocol and durable drivers; identity and denial failure.
- **Conformance:** method response and diff fixtures across supported Protocol transports.
- **Concurrency / leak:** N≥10 consumer stress, plus shared typed client cancellation and no cross-talk.
## Smoke script additions

- Static assertions for CLI verb, route, typed-client usage, D-414-only consumption, diff states, and Console conventions; live Protocol assertion is added at implementation time.
## Coverage target

- `cmd/harbor`: 75%; Console skill view: 85% check coverage; integration: 80%.
## Dependencies

- 240; independent of 236, 238, 239, and 242.
## Risks / open questions

- CLI and Console must not invent fallback states when the Protocol returns an error; preserve typed status vocabulary.
- Local frontend checks are a skip only when `web/console` dependencies are unavailable; web CI must execute the full gate.
## Validation gate ledger

- **Local skip:** frontend checks may skip only when dependencies are unavailable; Protocol/client and CLI integration remain required. Postgres may skip only without its explicit DSN.
- **Web CI:** `npm ci`, check, lint, build, typed Protocol lockstep, and Console route coverage are mandatory; no committed build artifacts.
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
