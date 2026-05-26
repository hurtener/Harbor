# Phase 101 — GitHub Actions Node 24 modernisation

## Summary

Bump every GitHub Actions step in `.github/workflows/` from the Node-20-bound `@v4` family (`actions/checkout@v4`, `actions/download-artifact@v4`, `actions/upload-artifact@v4`, `actions/setup-go@v5`) to Node-24-capable successors before GitHub's 2026-09-16 cutoff. Workflows currently emit the deprecation annotation on every run; the bump is a small infra-hygiene change that keeps CI + the release pipeline running past the cutoff. Mirrors Dockyard's pinned-at-v6 posture so the two repos age together.

## RFC anchor

- RFC §12 — release engineering and CI/CD posture.

## Briefs informing this phase

- brief 13 — operator UX (CI annotations leak into PR review surfaces; deprecation warnings degrade the operator-author signal).

## Brief findings incorporated

- **brief 13 (operator UX surface).** Operators reading PR conversations see the deprecation annotations and treat them as project-health signals. A workflow that keeps emitting "Node.js 20 actions are deprecated" reads as "the maintainer isn't paying attention." The bump is cheap and the signal it removes is load-bearing.

## Findings I'm departing from (if any)

None.

## Goals

- Every workflow uses action versions that ship a Node-24 runtime.
- Zero "Node.js 20 actions are deprecated" annotations on CI / release runs.
- Workflow versions match Dockyard's pinned set (sibling repo `~/Repos/Dockyard/.github/workflows/`) so both projects age together.
- Release pipeline still passes on a `v*` tag push after the bump (the bump is in-place; the matrix + SLSA provenance + checksums.txt logic stays).

## Non-goals

- Restructuring workflow logic — the change is version-bumps only; the matrix shape, the SLSA attestation, and the release-build script stay as-is.
- Opting into `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true` before the bump (the explicit-bump path is cleaner; the temporary opt-in is for projects that can't bump yet).
- Adding new actions or workflows. Phase 101 is purely a maintenance pass over the existing surface.

## Acceptance criteria

- [ ] Every step in every `.github/workflows/*.yml` file uses a Node-24-capable action version (`actions/checkout@v6`, `actions/setup-go@v6`, `actions/setup-node@v4` or later, `actions/upload-artifact@v4` if Node-24-pinned by maintainer or successor, `actions/download-artifact@v4` ditto, `actions/attest-build-provenance@v4` ditto, etc.).
- [ ] `grep -rE 'actions/(checkout|setup-go)@v[45]' .github/workflows/` returns zero matches (the two actions whose Node-24 successors exist today are pinned forward).
- [ ] CI workflow (`ci.yml`) green on the bump PR.
- [ ] Release workflow (`release.yml`) green when the next `v*` tag push exercises it; SLSA attestations still emit; `checksums.txt` still verifies.
- [ ] No new deprecation annotations on the bump PR run.

## Files added or changed

- `.github/workflows/ci.yml` — bump actions versions.
- `.github/workflows/release.yml` — bump actions versions.
- `.github/workflows/*` — bump every remaining workflow (codeowners, dependabot config left as-is — they're not action invocations).
- `scripts/smoke/phase-101.sh` — static-only smoke asserting no deprecated action versions remain.

## Public API surface

N/A — workflow internals.

## Test plan

- **Unit:** N/A (workflow YAML, not Go).
- **Integration:** The bump PR's CI run is itself the integration test; the release workflow's next exercise (next `v*` tag) is the validation gate.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A.

## Smoke script additions

- `scripts/smoke/phase-101.sh` (static-only):
  - assert no `actions/checkout@v4` or `actions/checkout@v5` references remain.
  - assert no `actions/setup-go@v4` or `actions/setup-go@v5` references remain.
  - assert every workflow file (`.github/workflows/*.yml`) parses as YAML (basic sanity).

## Coverage target

N/A — no Go code touched.

## Dependencies

- 81 (release engineering — the release workflow whose action versions get bumped).

## Risks / open questions

- **Breaking changes in v5/v6.** `actions/checkout@v6` and `actions/setup-go@v6` shipped after v4 with mostly-API-compatible interfaces, but submodule + Git-LFS handling has occasionally regressed across major bumps. The bump PR runs the full CI matrix to catch that.
- **Cache key invalidation.** `actions/setup-go@v6` may compute its cache key differently from `v5`; first run after the bump rebuilds the cache (~minutes, not blocking).
- **Dockyard parity drift.** If Dockyard bumps further before Harbor lands this phase, Harbor's bump should match the higher version. Cross-check `~/Repos/Dockyard/.github/workflows/` at PR-author time.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (no Go code touched)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (workflow YAML only)
- [ ] Concurrent-reuse test — N/A (no reusable Go artifact built)
- [ ] Integration test — N/A (Dependencies is 81 — the CI run itself is the integration gate per §17.1 acceptable shapes)
- [ ] If new vocabulary: glossary updated — N/A (no new terms)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A
