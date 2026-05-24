# Phase 83k — console-release-embed

## Summary

Phase 83k closes the Console-release-embed gap the operator validation
surfaced. The current state: `cmd/harbor/consoledist/` is gitignored
except for `.gitkeep`, so a fresh `git clone` + `go build ./cmd/harbor`
(or `go install github.com/.../cmd/harbor@latest`) produces a binary
that embeds an EMPTY Console bundle. Operators must remember to run
`make console-build` before `make build` to get a working `harbor
console`. The release pipeline (`scripts/release-build.sh` +
`.github/workflows/release.yml`) ALSO skips the Console build, so
tagged releases would ship the same broken bundle if not for a stale
local artifact carrying over.

83k makes the Console bundle a first-class build artifact:
`make build` rebuilds the Console first; `scripts/release-build.sh`
rebuilds the Console first; `harbor console`'s placeholder page tells
operators clearly when the bundle is empty + how to fix it.

The "first-run reach" half of 83k (favicon, brand tokens applied,
empty-state copy) is intentionally deferred to a follow-up
(post-walkthrough): the visual walkthrough that follows this phase
produces the punch list those polish items live in. Shipping the
release-embed half first means the walkthrough can run against the
shipped binary instead of a manually-rebuilt local one.

## RFC anchor

- RFC §5 — Console as Protocol client (the binary that serves it).
- RFC §8 — CLI surface (`harbor console`).

## Briefs informing this phase

- brief 06
- brief 12

## Brief findings incorporated

- brief 06 §1 — DevX is binding: a `go install` that yields a half-
  working binary is exactly the first-clone failure mode the brief
  argues against. 83k makes `go install` operators see a binary that
  either (a) carries the real Console (when built via `make` or the
  release pipeline) or (b) tells them loudly what to do (when built
  via a bare `go build`).
- brief 12 §2 — The Console deployment story is "one binary, no
  daemon, embed the bundle." 83k makes the embed actually happen
  on the release path.

## Findings I'm departing from (if any)

None.

## Goals

- `make build` rebuilds the Console bundle before invoking
  `go build`. Order: `make build` → `make console-build` → `go build`.
  A new `make build-fast` target preserves today's behavior for
  iterative dev work that doesn't touch `web/console/`.
- `scripts/release-build.sh` runs `make console-build` (or its
  inline equivalent) before `go build`. The release artifact ALWAYS
  carries a fresh Console.
- `cmd/harbor/console_embed.go`'s placeholder page (served when the
  embedded bundle is empty) gains a clearer message that names the
  exact rebuild command + points to `docs/CONFIG.md` for the
  `harbor console` config.
- A CI staleness gate (`scripts/check-console-bundle.sh`) runs
  `make console-build` then `git diff --exit-code -- cmd/harbor/consoledist/`
  — fails the build when `web/console/` changed without a matching
  rebuild. Mirrors the `make protocol-ts-gen-check` pattern (D-093).
- A new smoke check confirms the placeholder page renders the
  rebuild instructions when the bundle is empty.

## Non-goals

- **Favicon, brand-token application, empty-state copy.** All visual
  polish is deferred to a follow-up driven by the post-83k visual
  walkthrough's punch list.
- **Embedding the Console into `harbor dev`.** Settled in D-091
  (Console only serves under `harbor console`); 83k doesn't touch
  that.
- **Switching the Console build from `npm` to a faster bundler.**
  Out of scope; the npm-build dependency is the project's settled
  posture (CLAUDE.md §4.5).

## Acceptance criteria

- [ ] `make build` rebuilds the Console bundle first; the produced
      `bin/harbor` carries a real Console (verified by serving it
      and curl-ing `/` for the SvelteKit index marker).
- [ ] `make build-fast` produces the today's-behavior binary (skip
      Console rebuild; iterative-dev shortcut).
- [ ] `scripts/release-build.sh` rebuilds the Console before
      `go build`; the released binary carries a real Console.
- [ ] The placeholder page (served when the embedded bundle is
      empty) names the rebuild command verbatim AND a one-line
      next-step for `go install` operators.
- [ ] `scripts/check-console-bundle.sh` exists and fails when
      `web/console/` is dirty relative to `consoledist/`. Wired
      into the `frontend-e2e` CI job.
- [ ] `scripts/smoke/phase-83k.sh` static-asserts the surface
      changes.

## Files added or changed

- `Makefile` — `build` target gains `console-build` dependency;
  new `build-fast` target preserves the no-console path.
- `scripts/release-build.sh` — runs `make console-build` (or inline
  equivalent) before `go build`.
- `cmd/harbor/console_embed.go` — placeholder page copy reworded
  with the exact rebuild commands.
- `scripts/check-console-bundle.sh` — new CI staleness gate.
- `.github/workflows/ci.yml` — wire the staleness gate into the
  `frontend-e2e` job (one new step).
- `docs/plans/README.md` — Phase 83k row + flip to Shipped.
- `docs/decisions.md` — D-157.
- `docs/glossary.md` — `Console bundle staleness gate` entry.
- `docs/plans/phase-83k-console-release-embed.md` — this plan.
- `scripts/smoke/phase-83k.sh` — static-surface assertions.

## Public API surface

None. 83k is build-pipeline + placeholder copy only.

## Test plan

- **Unit:** N/A — build-pipeline changes are smoke-asserted.
- **Integration:** smoke script asserts the surface changes; the
  placeholder-page rewording is grep-checkable.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A.
- **Manual:** `make build` then `./bin/harbor console` → serves
  real Console. `go build ./cmd/harbor && ./harbor console` →
  serves the placeholder page with the new copy. Both verified
  before commit.

## Smoke script additions

`scripts/smoke/phase-83k.sh` asserts:

- `Makefile`'s `build:` target depends on `console-build`.
- A `build-fast:` target exists.
- `scripts/release-build.sh` calls `make console-build` or its inline
  equivalent.
- `scripts/check-console-bundle.sh` exists and is executable.
- The CI workflow (`.github/workflows/ci.yml`) references the
  staleness gate script.
- `cmd/harbor/console_embed.go`'s placeholder page contains the
  rebuild-command marker.

## Coverage target

- N/A — build-pipeline phase.

## Dependencies

- Phase 73m (consoledist embed surface).
- Phase 81 (release pipeline).
- Phase 83n (docs/CONFIG.md — the placeholder page links to it).

## Risks / open questions

- **`make build` becomes slower for everyone.** Mitigated by
  `make build-fast`; documented in the Makefile comment block.
- **The staleness gate produces false positives when an unrelated
  CI run rebuilds the bundle non-deterministically.** SvelteKit
  builds with `adapter-static` are deterministic given pinned
  versions; the existing `package-lock.json` pins everything. The
  risk is real only if a new locale-dependent emit slips in;
  managed by the same gate (it'll fail loud + flag).
- **Operators who use `go install` (not the release artifacts) get
  the placeholder.** Documented in the new placeholder copy. The
  long-term fix is publishing release artifacts via GitHub Releases
  (Phase 81 covers this); 83k closes the local-build gap.

## Glossary additions

- **Console bundle staleness gate** — the CI gate
  (`scripts/check-console-bundle.sh`) that runs `make console-build`
  then asserts `git diff --exit-code -- cmd/harbor/consoledist/` is
  empty. Mirrors the `make protocol-ts-gen-check` (D-093) pattern:
  a generated artifact that drifts from its source fails the build
  loud.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on touched packages — N/A (build pipeline)
- [ ] Concurrent-reuse — N/A
- [ ] Integration test exists per §17 — N/A (manual + smoke)
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
