# Phase 181 — TUI terminal foundation and visual system

## Summary

Build the Bubble Tea application shell, Harbor terminal design system, command/
focus infrastructure, responsive layout, accessibility modes, golden harness,
and PTY lifecycle foundation. The binding quality floor is
`docs/design/tui/CONVENTIONS.md`: OpenCode-level or better perceived execution,
not a functional placeholder shell.

## RFC anchor

- RFC §3.1
- RFC §5.1
- RFC §5.4
- RFC §8
- RFC §10

## Briefs informing this phase

- brief 06
- brief 11
- brief 12

## Brief findings incorporated

- brief 06 §5: a polished client still consumes only canonical Protocol state.
- brief 11 §layout: stable navigation, status, detail, and event regions create
  operational hierarchy rather than an undifferentiated log viewer.
- brief 12 §two-surface model: deployment and rendering code remain outside the
  Runtime and independently testable.

## Findings I'm departing from (if any)

Briefs 11/12 target a browser Console. The native terminal uses Bubble Tea and
the measured OpenCode visual grammar instead of Svelte components, while
preserving Protocol-only architecture and equivalent interaction quality.

## Goals

- Establish the complete visual/token/layout/focus foundation all later screens
  compose rather than restyling per phase.
- Make OpenCode-level visual and interaction quality mechanically reviewable.
- Guarantee terminal restoration and deterministic responsive geometry.

## Non-goals

- No complete conversation composer/session workflow (Phase 182).
- No runtime control/inspection screens (Phase 183).
- No `serve --tui` or scaffold integration (Phase 184).
- No coding-agent UI.

## Acceptance criteria

- [x] RFC §10 pins Bubble Tea v2, Lip Gloss v2, selected Bubbles v2, and
      `charmbracelet/x/ansi`; the static CGo-free binary gate remains green.
- [x] `internal/tui/ui` defines semantic tokens, dark/light themes, one width
      implementation, heavy-left-rule cards, composer/dialog/sidebar geometry,
      and no component-local color literals.
- [x] The root layer stack is base route, sidebar overlay/scrim, autocomplete,
      toast, modal, then startup indicator, with explicit dimensions before
      styling.
- [x] Exact responsive thresholds hold: 79/80 action transition and 120/121
      sidebar transition with fixed 42-column sidebar.
- [x] One command registry powers key dispatch, command palette, which-key,
      footer hints, disabled reasons, and help.
- [x] One modal/select model powers search, categories, current item, paging,
      context actions, backdrop behavior, and focus restoration.
- [x] Truecolor, 256-color, 16-color, `NO_COLOR`, dark/light, and reduced-motion
      modes preserve state semantics.
- [x] Golden frames pass at all ten sizes and applicable states required by
      `docs/design/tui/CONVENTIONS.md`.
- [x] PTY tests cover resize, suspend/resume, SIGINT/SIGTERM, panic recovery,
      cursor visibility, alternate-screen restoration, and no goroutine leak.
- [x] Side-by-side review records show equivalent or better hierarchy,
      alignment, spacing, state feedback, and responsive behavior than the
      reference frames; “polish later” is not an accepted deviation.

## Files added or changed

- `internal/tui/app/`
- `internal/tui/ui/`
- `internal/tui/testdata/golden/`
- `cmd/harbor/cmd_tui.go` (shell/help only until Phase 182)
- `go.mod`
- `go.sum`
- `docs/design/tui/CONVENTIONS.md`
- `test/integration/tui_terminal_pty_test.go`
- `scripts/smoke/phase-181.sh`

## Public API surface

- CLI preview surface: `harbor tui --help`; attach execution remains gated until
  the Phase-182 conversation consumer is complete.
- No public Go facade.

## Test plan

- **Unit:** geometry, tokens, command registry, modal/focus stack, width cases,
  theme degradation, animation scheduling, resize transitions.
- **Integration:** PTY application lifecycle with a fixture projection source.
- **Conformance:** full conventions acceptance matrix and reference-comparison
  capture manifest.
- **Concurrency / leak:** timer/subscription command cancellation and repeated
  PTY mount/unmount baseline restoration.

## Smoke script additions

- Run UI geometry/golden/PTy tests under `-race`.
- Assert the exact breakpoint constants and full golden-size inventory.
- Assert the binary remains CGo-free.

## Coverage target

- `internal/tui/ui`: 90%
- `internal/tui/app`: 85%

## Dependencies

- 179
- 180; as built, the shell consumes the projection fixture rather than carrying
  a parallel terminal-only state shape.

## Risks / open questions

- Terminal framebuffer effects differ; parity is geometry/hierarchy/semantics
  under the pinned terminal with honest degradation elsewhere.
- Snapshot churn can make visual tests meaningless; goldens must be reviewed,
  not bulk-accepted.

## Glossary additions

- TUI quality floor
- terminal design system

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Multi-isolation N/A: fixture-only shell carries no identity-scoped read
- [x] Reusable UI/model tests cover N≥100 updates under `-race`
- [x] PTY integration covers ≥1 failure mode and goroutine cleanup
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above; no new decision
  required
