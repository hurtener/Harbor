# Phase 82 — v1-cut

## Summary

Phase 82 is the **v1.0.0 cut** — the release phase. It ships the
operator-facing release surfaces and tags `v1.0.0`. No new Runtime,
Protocol, Console, or CLI capability lands here; Phase 82 packages what
Waves 1–14 built into a release: a rewritten root `README.md` that reads
as a real framework front door, the `CHANGELOG.md` `[1.0.0]` roll, a
launch-announcement scaffold, and the `v1.0.0` git tag (operator-run,
from `main`).

## RFC anchor

- RFC §1 — what Harbor is: the four-layer architecture and the three
  non-negotiable product properties the v1.0.0 README presents.
- RFC §12 — release and versioning. The product release version is
  distinct from the Harbor Protocol version; tagged releases are made
  from `main`.

## Briefs informing this phase

None. Phase 82 is the release cut, not a subsystem build — it has no
phase-planning research brief. RFC §1 and §12 are the design anchors;
the release tooling was built in Phase 81 (D-139).

## Brief findings incorporated

Not applicable — no informing brief (see above).

## Findings I'm departing from (if any)

None.

## Goals

- Rewrite the root `README.md` as a framework-quality front door —
  positioning, quickstart, architecture, usage, honest V1 status — with
  the Harbor logo and a CI / release / Go-reference / version / license
  badge row. It replaces the organically-grown phase-status-table
  README.
- Roll `CHANGELOG.md` to a dated `[1.0.0]` section. Public release
  surfaces describe the product in subsystem/feature terms — the
  internal "phase" development vocabulary stays in `docs/plans/`.
- Ship a launch-announcement scaffold at `docs/announcements/v1.0.0.md`.
- Tag `v1.0.0` from `main` (operator-run) — the release workflow then
  builds and publishes the release artifact.

## Non-goals

- New runtime, Protocol, Console, or CLI capability — none lands in the
  release phase.
- Migration notes — v1.0.0 is the initial release; there is no prior
  version to migrate from, so no migration notes apply.
- Post-V1 work (deeper ReAct prompting, additional planner concretes, a
  durable distributed bus, governance extensions) — tracked in the
  master phase plan, out of scope here.

## Acceptance criteria

- [ ] `harbor version` returns `v1.0.0` (verified at tag time — the tag
      does not exist at PR time; an un-tagged build reports the
      `v0.0.0-dev` sentinel).
- [ ] `make preflight` green.
- [ ] The Protocol conformance suite is green.
- [ ] The cross-tenant isolation and goroutine-leak conformance
      harnesses are green.
- [ ] The root `README.md` carries the logo, the badge row, and no
      phase-status table.
- [ ] `CHANGELOG.md` carries a dated `[1.0.0]` section and no internal
      phase jargon.
- [ ] `docs/announcements/v1.0.0.md` exists.

## Files added or changed

- `README.md` — rewritten as the v1.0.0 framework front door.
- `CHANGELOG.md` — `[Unreleased]` rolled to a dated `[1.0.0]`,
  de-jargoned.
- `docs/announcements/v1.0.0.md` — launch-announcement scaffold (new).
- `docs/plans/phase-82-v1-cut.md` — this plan.
- `docs/plans/README.md` — Phase 82 row → `Shipped`.
- `docs/decisions.md` — D-142.
- `scripts/smoke/phase-82.sh` — static-only release-surface smoke.

The §13 predecessor-synonym scrub of research briefs 13 / 14 / `INDEX.md`
(flagged by the Wave 14 checkpoint) ships as its own `docs:` PR rather
than bundled here — it is research-brief hygiene, distinct from the
release cut, and it merges before the `v1.0.0` tag.

## Public API surface

None. Phase 82 adds no exported runtime type, no Protocol method, no
config key, no CLI subcommand.

## Test plan

Full `make preflight` — build, boot, smoke. The Protocol conformance
suite and the cross-tenant isolation / goroutine-leak / chaos
conformance harnesses run in CI on the phase-82 PR and gate the cut.
`scripts/smoke/phase-82.sh` statically asserts the release surfaces.

## Smoke script additions

`scripts/smoke/phase-82.sh` (static-only) asserts: `CHANGELOG.md` has a
dated `[1.0.0]` section; `docs/announcements/v1.0.0.md` exists; the
CHANGELOG carries no `phase-N` internal jargon.

## Coverage target

Not applicable — Phase 82 ships no Go code.

## Dependencies

Phase 81 (release engineering — the version-stamping, `CHANGELOG.md`,
and `release.yml` workflow this phase's tag triggers).

## Risks / open questions

- The `v1.0.0` tag is operator-run and irreversible. It is pushed from
  `main` after this PR merges, only once CI on `main` is green.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` clean.
- [ ] `make preflight` green (or a documented `HARBOR_PREFLIGHT_SKIP`
      under environmental machine load — CI is the gate).
- [ ] `make check-mirror` clean.
- [ ] `markdownlint-cli2` clean repo-wide.
- [ ] `docs/plans/README.md` Phase 82 row flipped to `Shipped`.
