# Phase 229 — External release oracles

## Summary

Closes #652 and #644 and supersedes weak Phase 223/224 evidence: canonical event names have a handwritten oracle, every drift guard has an executable registered mutation, and docs PR validation cannot be cancelled by an unrelated Pages deployment.

## RFC anchor

- RFC §5.2.
- RFC §6.13.

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- brief 06 §1: events are the canonical external runtime projection and require stable exhaustive names.
- brief 07 §11: tests need external golden and failure-mode evidence, not self-derived expectations.

## Findings I'm departing from (if any)

- Phase 224's numeric census can stay green after an entire case disappears; this phase supersedes it with one case registry.

## Goals

- Make event-wire names, smoke summary enforcement, mutation coverage, and docs validation independently falsifiable.

## Non-goals

- No generated golden sourced from emitter constants; no GitHub branch-protection mutation.

## Acceptance criteria

- [x] Add/remove/rename of any canonical event fails a handwritten bidirectional golden comparison.
- [x] A summary-less shipped smoke increments failure and exits nonzero in a black-box fixture.
- [x] One registry joins guard ID, audit signature, declared cases, and executed cases; whole-case deletion fails.
- [x] PR docs validation is per-ref; only main Pages deployment is globally serialized.

## Files added or changed

- Protocol event conformance tests and golden
- `scripts/drift-audit.sh`, `scripts/smoke/phase-223.sh`, `scripts/smoke/phase-224.sh`
- `.github/workflows/docs.yml`, `scripts/smoke/phase-229.sh`

## Public API surface

- No wire additions; the exact existing canonical event-name set gains an external oracle.

## Test plan

- **Unit:** golden bidirectional diff and workflow policy parser.
- **Integration:** black-box smoke/preflight summary fixture and real audit mutations.
- **Conformance:** every canonical event and guard unit joins exactly once.
- **Concurrency / leak:** parallel audit fixtures use unique temporary directories.

## Smoke script additions

- Execute event-golden, summary-less smoke, whole-case deletion, and workflow-concurrency guards.

## Coverage target

- N/A for shell; touched Go conformance package does not regress.

## Dependencies

- 223, 224.

## Risks / open questions

- The handwritten golden is intentionally manual and must be reviewed with every canonical event change.

## Glossary additions

- External event-name oracle.

## Pre-merge checklist

- [x] Drift, mirror, CI preflight, mutation green-to-red, and docs checks pass
