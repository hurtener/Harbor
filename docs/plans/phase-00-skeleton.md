# Phase 00 — Repo Skeleton

## Summary
Establish the repository's working surface: contributor normatives, build hygiene, CI scaffold, preflight gate, doc structure. No Go code yet (`go.mod` is a stub).

## Goals
- A new contributor (human or AI) can clone, run `make check-mirror`, `make preflight`, and `make install-hooks` and have a working dev loop.
- AGENTS.md and CLAUDE.md are verbatim mirrors enforced by CI.
- Phase smoke convention is in place — `scripts/smoke/common.sh` + `scripts/smoke/phase-00.sh` — so subsequent phases can extend it.
- The doc layout (`docs/plans/`, `docs/rfc/`, `docs/research/`) is documented and CI-enforceable.

## Non-goals
- No `cmd/harbor/main.go`. No `internal/...` packages. The Runtime arrives in Phase 01+.
- No Console code. The Console lives in its own repo or in `web/console/` once approved by RFC.
- No production CI gates that require Go sources. Go-shaped CI jobs exist but auto-skip until Phase 01 lands.

## Acceptance criteria
- [x] `AGENTS.md` exists and covers §1–§16.
- [x] `CLAUDE.md` is a verbatim mirror of `AGENTS.md`.
- [x] `make check-mirror` passes.
- [x] `make preflight` passes (no-ops Go-shaped checks; `scripts/smoke/phase-00.sh` runs).
- [x] `scripts/smoke/phase-00.sh` asserts: `AGENTS.md` present, `CLAUDE.md` present, mirror identical, README present, `docs/plans/README.md` present, `docs/rfc/README.md` present, `docs/research/` non-empty.
- [x] CI workflow runs on every PR; mirror + markdownlint + Go (auto-skip) + lint (auto-skip) + preflight.
- [x] Pre-commit hook installable via `make install-hooks` and runs the preflight gate.
- [x] No top-level doc names the predecessor project; the smoke script enforces the invariant.

## Files added or changed
- `AGENTS.md`, `CLAUDE.md`
- `README.md`
- `Makefile`
- `go.mod`
- `.editorconfig`, `.gitignore`, `.golangci.yml`, `.markdownlint.yaml`
- `.github/workflows/ci.yml`
- `.github/dependabot.yml`, `.github/CODEOWNERS`, `.github/PULL_REQUEST_TEMPLATE.md`
- `docs/plans/README.md`, `docs/plans/phase-00-skeleton.md`
- `docs/rfc/README.md`
- `scripts/preflight.sh`, `scripts/smoke/common.sh`, `scripts/smoke/phase-00.sh`, `scripts/install-hooks.sh`, `scripts/hooks/pre-commit`

## Test plan
This phase has no Go tests (no Go source). Verification is the smoke script + CI green.

## Smoke script additions
`scripts/smoke/phase-00.sh` asserts the doc & mirror invariants listed above.

## Coverage target
N/A for this phase.

## Dependencies
None.

## Open questions / risks
- Final phase numbering scheme — currently 00..NN sequential; RFC may switch to subsystem-prefixed (e.g. `R-01`, `P-01`). If it does, all phase plans get renamed in a single PR and the README reorganized.
- Final license / openness call — flagged when load-bearing.
