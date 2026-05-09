# Phase 00 — Repo Skeleton

## Summary
Establish the repository's working surface: contributor normatives, build hygiene, CI scaffold, preflight gate, doc structure, license, and the drift hygiene framework (template + glossary + decisions log + drift-audit). No Go code yet (`go.mod` is a stub at module declaration).

## Goals
- A new contributor (human or AI) can clone, run `make check-mirror`, `make drift-audit`, `make preflight`, and `make install-hooks` and have a working dev loop.
- AGENTS.md and CLAUDE.md are verbatim mirrors enforced by CI and a local hook.
- Phase smoke convention is in place — `scripts/smoke/common.sh` + `scripts/smoke/phase-00.sh` + `scripts/smoke/_template.sh` — so subsequent phases can extend it.
- The doc layout (`docs/plans/`, `docs/rfc/`, `docs/research/`, `docs/glossary.md`, `docs/decisions.md`) is documented and CI-enforceable.
- The drift hygiene framework (per-phase template + subsystem→briefs index + decisions log + glossary + drift-audit script + binding §16 authoring workflow in AGENTS.md/CLAUDE.md) is operational.

## Non-goals
- No `cmd/harbor/main.go`. No `internal/...` packages. The Runtime arrives in Phase 01+.
- No Console code. The Console lives in its own repo or in `web/console/` once approved by RFC.
- No production CI gates that require Go sources. Go-shaped CI jobs exist but auto-skip until Phase 01 lands.

## Acceptance criteria
- [x] `AGENTS.md` exists and covers §1–§17 (orientation block + 17 numbered sections).
- [x] `CLAUDE.md` is a verbatim mirror of `AGENTS.md`.
- [x] `LICENSE` (Apache-2.0) present at repo root.
- [x] `make check-mirror` passes.
- [x] `make drift-audit` passes (cross-references resolve, mirror invariant, plan↔smoke pairing, forbidden-name scan, required files present).
- [x] `make preflight` passes (drift-audit + every phase smoke; build/boot auto-skip with no `cmd/harbor/main.go`).
- [x] `scripts/smoke/phase-00.sh` asserts: AGENTS.md, CLAUDE.md, mirror identical, README, LICENSE, Makefile, go.mod, lint/format/markdown configs, CI workflow + dependabot + CODEOWNERS + PR template, the doc tree (`docs/plans/{README,phase-00,_template}.md`, `docs/rfc/README.md`, `docs/research/INDEX.md` + non-empty briefs dir, `docs/glossary.md`, `docs/decisions.md`), and the script tree (`scripts/{preflight,drift-audit,install-hooks}.sh`, `scripts/smoke/{common,phase-00,_template}.sh`, `scripts/hooks/pre-commit`).
- [x] CI workflow runs on every PR; mirror + markdownlint + Go (auto-skip) + lint (auto-skip) + preflight.
- [x] Pre-commit hook installable via `make install-hooks` and runs the preflight gate (which now includes the drift-audit).
- [x] No top-level doc names the predecessor project; the smoke script enforces the invariant.

## Files added or changed
- `AGENTS.md`, `CLAUDE.md`
- `README.md`
- `LICENSE` (Apache-2.0)
- `Makefile`
- `go.mod`
- `.editorconfig`, `.gitignore`, `.golangci.yml`, `.markdownlint.yaml`
- `.github/workflows/ci.yml`
- `.github/dependabot.yml`, `.github/CODEOWNERS`, `.github/PULL_REQUEST_TEMPLATE.md`
- `RFC-001-Harbor.md` (the design source of truth — landed alongside Phase 0)
- `docs/plans/README.md`, `docs/plans/phase-00-skeleton.md`, `docs/plans/_template.md`
- `docs/rfc/README.md`
- `docs/research/INDEX.md` (briefs already present from prior research)
- `docs/glossary.md`
- `docs/decisions.md`
- `scripts/preflight.sh`, `scripts/drift-audit.sh`, `scripts/smoke/common.sh`, `scripts/smoke/phase-00.sh`, `scripts/smoke/_template.sh`, `scripts/install-hooks.sh`, `scripts/hooks/pre-commit`

## Test plan
This phase has no Go tests (no Go source). Verification is the smoke script + CI green.

## Smoke script additions
`scripts/smoke/phase-00.sh` asserts the doc + mirror + license + drift hygiene invariants listed above (31 assertions total).

## Coverage target
N/A for this phase.

## Dependencies
None.

## Open questions / risks
- Final phase numbering scheme — currently 00..NN sequential with two non-sequential V1 inserts (36a, 36b) for Governance. If the RFC switches to subsystem-prefixed numbering (e.g. `R-01`, `P-01`), all phase plans get renamed in a single PR and the README reorganized.
- Brief 09+ may land if a substantial new investigation surface emerges; the INDEX.md + drift-audit handle additions cleanly.
