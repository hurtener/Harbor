# Phase 139 — Public-site honesty sweep

## Summary

Corrects three stale claims on Harbor's public marketing landing surface (the
VitePress `landingSpec.ts`) and one stale godoc reference in the `harbor dev`
hot-reload test. Docs-only: no runtime, Protocol, or config surface changes.
The sweep keeps the operator-facing surface honest — the canonical Protocol
method count, the `harbor dev` hot-reload capability, and a cosmetic dev-banner
artifact are brought back in line with what the binary actually ships, per the
CLAUDE.md §18 docs-site honesty mandate.

## RFC anchor

- RFC §7.1 — the runtime-lens principle: the Console (and its marketing landing
  surface) is a Protocol client; the claims it renders must reflect the real
  canonical Protocol surface, not a stale snapshot.
- RFC §7.3 — Console binding conventions: the published surface stays in
  lockstep with the canonical Protocol method/event/error sets.

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Console as a Protocol client": every figure the Console surfaces
  (method counts, capability claims) is derived from the canonical Protocol
  surface — a hardcoded `109` that has drifted from the registry's `110` is the
  exact class of stale claim this sweep removes.
- brief 12 §"Decoupled deployment / `harbor dev`": `harbor dev` runs a local
  Runtime + Protocol server with config (`harbor.yaml`) reload; it does NOT
  recompile in-process `.go` tools. The landing copy is corrected to advertise
  the genuine config-reload capability rather than an unqualified "hot reload".

## Findings I'm departing from (if any)

None.

## Goals

- The marketing landing surface advertises **110** canonical Protocol methods
  (the current count, pinned by `internal/protocol/methods/methods_test.go` and
  `docs/site/protocol/methods.md`), with no `v1.6`-pinned qualifier.
- The `harbor dev` hot-reload claim is qualified to the genuine capability
  (config/YAML reload), since `.go` reload is honest-WARN-only after phase 138.
- The cosmetic, never-printed "3 drivers registered" dev-banner fragment is
  removed.
- The `harbor dev` hot-reload test no longer references the non-existent
  `test/integration/phase65_hot_reload_test.go`; its godoc points at the real
  in-package real-bus tests.

## Non-goals

- No change to the README serve section (`192-194`) — already honest ("mints no
  token", points at `identity.jwks_url`/`jwks_file`).
- No softening of "in under a second" — that string does not exist anywhere in
  `landingSpec.ts` (verified); the original directive is dropped.
- No README hot-reload edit — the README has zero hot-reload mentions
  (verified); the original directive is dropped.
- No change to the `v1.6` eyebrow / banner / release-note copy on the landing
  page — out of scope for this sweep (only the methods-count line drops its
  `v1.6` qualifier, because the count is current and not version-pinned).
- No new runtime, Protocol, or config surface.

## Acceptance criteria

- [ ] `landingSpec.ts` Protocol section reads "110 canonical methods" with no
      "at v1.6" qualifier (was "109 canonical methods at v1.6").
- [ ] `landingSpec.ts` stats grid reads `"110"` for "canonical Protocol
      methods" (was `"109"`).
- [ ] `landingSpec.ts` `harbor dev` deploy copy advertises config reload, not an
      unqualified "hot reload".
- [ ] `landingSpec.ts` dev banner no longer contains "3 drivers registered".
- [ ] `cmd/harbor/cmd_dev_hot_reload_test.go` godoc references the in-package
      real-bus tests, not `test/integration/phase65_hot_reload_test.go`; the
      stale string appears nowhere in the repo.
- [ ] `scripts/smoke/phase-139.sh` (static-only honesty greps) passes with
      FAIL=0.
- [ ] Console gates pass (`npm run check && npm run lint && npm run build`).
- [ ] `make drift-audit` clean; `make preflight` green.

## Files added or changed

- `docs/site/.vitepress/theme/landingSpec.ts` — 109→110 (×2), drop "at v1.6",
  qualify hot-reload claim, remove "3 drivers registered".
- `cmd/harbor/cmd_dev_hot_reload_test.go` — repoint stale godoc reference at the
  in-package real-bus tests.
- `docs/plans/phase-139-public-site-honesty.md` — this plan.
- `docs/plans/README.md` — phase 139 index row + detail block.
- `scripts/smoke/phase-139.sh` — static-only honesty-guard smoke.

## Public API surface

None. Docs-only; no Go, Protocol, or config surface added or changed.

## Test plan

- **Unit:** `cmd/harbor` hot-reload tests still compile and pass
  (`go test ./cmd/harbor/ -race -run HotReload`) — the edit is godoc-only, no
  behaviour change.
- **Integration:** N/A — docs-only; no cross-subsystem seam is opened or
  consumed. The hot-reload godoc edit references existing in-package real-bus
  tests; it adds no new wiring.
- **Conformance:** N/A — no driver surface touched.
- **Concurrency / leak:** N/A — no reusable artifact built.

## Smoke script additions

`scripts/smoke/phase-139.sh` (static-only) asserts the honesty invariants:

- landing surface reads "110 canonical methods" / `"110"` stat, present.
- no stale "109 canonical methods" and no "at v1.6" qualifier on the methods
  claim.
- no "3 drivers registered" in the dev banner.
- `harbor dev` copy qualified to "config reload".
- no `phase65_hot_reload_test.go` reference in the hot-reload test.

The smoke adds no live-server surface — it is the grep half of the
"VitePress build + greps" gate.

## Coverage target

N/A — docs-only; no production Go added. The one touched `.go` file is a
`_test.go` (godoc comment only), so package coverage is unchanged.

## Dependencies

- 138 (the `.go`-reload honest-WARN fix that makes the config-reload
  qualification accurate). Lands in the same wave; this docs sweep lands first.

## Risks / open questions

- Risk: the canonical method count could drift again. Mitigated by the
  static-only smoke greps in `scripts/smoke/phase-139.sh` plus the existing
  `methods_test.go` count assertion and the generated `methods.md` lockstep.

## Glossary additions

None — introduces no new vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (N/A — docs-only)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (N/A)
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes —
      N/A, docs-only; no reusable artifact built.
- [ ] If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: integration test exists — N/A, docs-only; no seam
      opened or consumed.
- [ ] If new vocabulary: glossary updated (N/A)
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed (N/A — no departure)
