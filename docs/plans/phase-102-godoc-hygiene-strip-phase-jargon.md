# Phase 102 — Godoc hygiene: strip internal phase jargon from exported docs

## Summary

Strip "Phase NN" / "phase-NN" / inline `D-NNN` / "brief NN" / wave-band references from every godoc-visible comment in shipping Go source under `internal/` and `cmd/`. Today pkg.go.dev renders Harbor with the project's internal development-jargon all over the public docs surface (2,343 occurrences across 360 files at v1.1.6) — operators reading the godoc see prose authored for the project's contributors, which damages the adoption signal. The cleanup keeps the underlying technical content; only the internal-numbering references are rewritten in operator-facing language.

## RFC anchor

- RFC §1 — Harbor as a public Go-native runtime SDK (godoc is one of the load-bearing public adoption surfaces).
- RFC §12 — release engineering hygiene.

## Briefs informing this phase

- brief 13 — operator UX (the godoc surface is part of the adoption funnel; internal jargon there is the same failure mode CLAUDE.md §13 catches in committed text by name).
- brief 06 — events / observability / devx (the public Go API surface is a devx product).

## Brief findings incorporated

- **brief 13 (operator UX).** "An operator who reads source-level docs to understand a behavior sees the project's voice. If that voice is full of internal phase numbering, the project reads as half-built; if it reads as deliberate, the project reads as production." Godoc is on the adoption funnel.
- **brief 06 (devx surface).** "Treat the public API as a product." Phase 102 extends that posture to the comments alongside the API.

## Findings I'm departing from (if any)

None.

## Goals

- Zero "Phase NN" or "phase-NN" references in godoc-visible comments under `internal/` and `cmd/` (excluding `_test.go`, which is internal to contributors).
- Zero inline `D-NNN` / `brief NN` cross-references in godoc-visible comments. These are contributor concepts; the operator reading godoc shouldn't need to know they exist.
- Zero wave-band ("Wave 8 audit", "round 3 closeout", "Stage A") references in godoc-visible comments.
- The cleanup PRESERVES the underlying technical content. A line that says "Phase 17 ships the in-memory driver" becomes "The in-memory driver ships as the default" — same fact, no internal numbering.
- A new drift-audit rule that fails on `Phase \d+` / inline `D-\d+` / `brief \d+` patterns in non-test Go source under `internal/` and `cmd/`, so this doesn't drift back over time.

## Non-goals

- Touching test files (`*_test.go`). Contributor-internal jargon is fine there.
- Touching `docs/plans/` or `docs/decisions.md` — the master plan and decisions log are explicitly contributor-internal and reference phase numbering by design.
- Rewriting the technical content. The cleanup is a wording pass, not a documentation rewrite.
- Removing the `internal/protocol/methods/methods.go` / equivalent constant references that are part of the wire surface (the literal METHOD names that happen to mention "Phase" — none are known to exist; this clause is here in case the audit surfaces one).

## Acceptance criteria

- [ ] `grep -rE '(Phase|phase-)[0-9]+' --include='*.go' internal/ cmd/ | grep -v '_test\.go'` returns zero matches.
- [ ] `grep -rE '\bD-[0-9]+' --include='*.go' internal/ cmd/ | grep -v '_test\.go'` returns zero matches.
- [ ] `grep -rE '\bbrief [0-9]+' --include='*.go' internal/ cmd/ | grep -v '_test\.go'` returns zero matches.
- [ ] `grep -rE '\b(Wave|Round|Stage)[ -][0-9A-Z]+' --include='*.go' internal/ cmd/ | grep -v '_test\.go'` returns zero matches.
- [ ] `scripts/drift-audit.sh` gains a new check that runs the four greps above and fails on a hit; the check is reset-safe so existing OKs don't depend on it.
- [ ] Every package under `internal/` whose godoc comment was edited still compiles + tests pass under `-race`.
- [ ] pkg.go.dev render of `github.com/hurtener/Harbor` after the next module fetch carries zero "Phase NN" strings (sampled across the top-level packages).

## Files added or changed

- Every `*.go` file under `internal/` and `cmd/` (excluding `_test.go`) that currently carries one of the forbidden patterns (360 files at v1.1.6; the exact set is enumerated by the grep above).
- `scripts/drift-audit.sh` — add the four greps to the forbidden-pattern scan.
- `scripts/smoke/phase-102.sh` — static-only smoke asserting the four greps return zero.
- `CLAUDE.md` + `AGENTS.md` §13 (forbidden practices) — add bullet: "internal phase numbering / D-NNN / brief NN / wave-band references in godoc-visible Go source under `internal/` or `cmd/` outside `_test.go` files."

## Public API surface

N/A — interface signatures don't change; only comments do.

## Test plan

- **Unit:** existing unit tests still pass against the reworded comments (godoc is comments-only; behavior unchanged).
- **Integration:** N/A.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A.

## Smoke script additions

- `scripts/smoke/phase-102.sh` (static-only) — runs the four greps and asserts each returns zero matches.

## Coverage target

N/A — no behavior changes; coverage targets are inherited from each touched package's existing target.

## Dependencies

- None hard-binding. Pragma: land after v1.1.6 (so the bulk of the cleanup happens against a stable surface) and BEFORE the V1.2 GitHub Pages docs site (so the site renders the cleaned godoc, not the noisy version).

## Risks / open questions

- **Volume.** 360 files × 2,343 occurrences is a long PR. The cleanup is mechanical (~80% can be done by a careful sed + manual review pass) but the diff is large; reviewers should look for "did you accidentally change a behavior comment" issues, not "is the wording right."
- **Style drift.** Different contributors reworded similar passages differently. The PR's reviewer should pull at least three rewrites and confirm the project voice stays coherent.
- **False positives.** A package that legitimately mentions "the third phase" of a runtime lifecycle (e.g. "lifecycle has three phases: pending, running, completed") would trip the grep. The acceptance criterion is `(Phase|phase-)[0-9]+` — only the PHASE-PLAN-NUMBERING pattern. The grep is intentionally narrow.
- **Drift over time.** Without the drift-audit extension, the cleanup decays the moment a new contributor adds a "// Phase 102 cleanup..." comment. The audit hook is the load-bearing guard.

## Glossary additions

None — the cleanup removes existing terminology rather than introducing new.

## Pre-merge checklist

- [ ] `make drift-audit` passes (with the new check active)
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (no behavior changes)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (comments only)
- [ ] Concurrent-reuse test — N/A (no reusable artifact built; existing artifacts unchanged)
- [ ] Integration test — N/A (no cross-subsystem seam touched)
- [ ] If new vocabulary: glossary updated — N/A (removing terms, not adding)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A
