# Harbor TUI Investigation

This dossier studies OpenCode's TUI and maps its reusable interaction quality
onto a future **generic Harbor runtime test and control TUI** implemented in
Go with Bubble Tea. It is research only: no dependency, CLI, Protocol, SDK, or
product-surface change is authorized here.

## Scope

The target is an optional TUI for:

- a remote Harbor Protocol endpoint;
- stock `harbor serve`; and
- scaffolded agent binaries built with a future explicit TUI option.

It is not a coding agent or IDE. File trees, Git diffs, shell/PTY, source
editing, worktrees, LSP, and coding-specific tool cards are explicitly
deferred to possible future HarborCode work.

## Documents

1. [`01-scope-and-product-boundary.md`](01-scope-and-product-boundary.md) —
   product job, scope, exclusions, invariants, and first screen set.
2. [`02-render-architecture.md`](02-render-architecture.md) — OpenCode's
   OpenTUI/Solid architecture, transport, streaming, and layout model.
3. [`03-parts-tools-plugins.md`](03-parts-tools-plugins.md) — ordered message
   parts, tool renderer registry, slots, prompt state, and dialogs.
4. [`04-harbor-protocol-capability-map.md`](04-harbor-protocol-capability-map.md)
   — current Harbor user journeys, all 116 canonical methods, and wire gaps.
5. [`05-opencode-visual-grammar.md`](05-opencode-visual-grammar.md) — exact
   dimensions, colors, glyphs, breakpoints, responsive behavior, and goldens.
6. [`06-bubbletea-stack-evaluation.md`](06-bubbletea-stack-evaluation.md) —
   Bubble Tea v2 stack, terminal capabilities, CGo/static posture, and custom
   component needs.
7. [`07-harbor-native-architecture-options.md`](07-harbor-native-architecture-options.md)
   — launch options, package boundaries, auth, reconnect, reducer, rollout,
   and RFC decisions.
8. [`08-additional-reusable-opencode-patterns.md`](08-additional-reusable-opencode-patterns.md)
   — compact mode, local queue, interventions, which-key, attention, sessions,
   export, retry, and safe epilogue.
9. [`09-reconciliation-and-lifecycle-edge-cases.md`](09-reconciliation-and-lifecycle-edge-cases.md)
   — snapshot fences, tombstones, queue failure, submit races, reconnect, and
   terminal restoration.
10. [`10-final-saturation-findings.md`](10-final-saturation-findings.md) —
    armed controls, request generations, PTY gates, fatal fallback, and strict
    saturation verdict.
11. [`11-command-and-lifecycle-inventory.md`](11-command-and-lifecycle-inventory.md)
    — complete command/screen/lifecycle inventory with Harbor classification.
12. [`12-v115-main-rebase-delta.md`](12-v115-main-rebase-delta.md) — changes
    introduced by the v1.14 fixes now underlying this v1.15 research branch.

## Executive Conclusions

1. **Start standalone.** The first implementation should be
   `harbor tui --attach`, using real authenticated REST/SSE.
2. **Co-launch later.** `harbor serve --tui` and scaffolded `--tui` should be
   orchestration conveniences around the same client.
3. **Keep Runtime headless.** No internal bus, planner, task, or server-handle
   shortcut is acceptable.
4. **Harbor already has broad control coverage.** Session, retention, task-
   approval, flow-token, and tool-catalog projections are now materially more
   trustworthy. The main fidelity gap remains a canonical durable ordered
   conversation/part projection.
5. **Use a pure reducer first.** Build one Go event/snapshot projection,
   independent of Bubble Tea, and share fixtures with the Console reducer.
6. **Bubble Tea v2 fits.** Its cell renderer plus Lip Gloss v2, selected
   Bubbles packages, and `x/ansi` can reproduce the visual grammar while
   preserving CGo-free static builds.
7. **Own the visual components.** OpenCode's feel comes from precise geometry,
   not stock widgets. Harbor needs its own transcript, composer, palette,
   dialog, sidebar, intervention, and renderer-registry components.
8. **Treat reconnect as reconciliation.** Last-event replay, snapshot
   generations, tombstones, gap recovery, and anti-resurrection are core
   correctness, not polish.
9. **Keep local state local.** Drafts, history, themes, keybindings, pins, and
   a clearly labelled local follow-up queue may persist; Runtime entities may
   not.
10. **OpenCode research is saturated.** The next useful work is an approved
    Harbor RFC/phase with captured wire fixtures, reducer tests, visual spikes,
    and real PTY lifecycle gates.

## Candidate Rollout

1. RFC and dependency decision.
2. Curated Protocol client, reducer, and first `harbor tui --attach` consumer.
3. Sessions, closed-session resume, erased-session remediation,
   interventions, attachments, and replay-gap reconciliation.
4. `harbor serve --tui` with a same-wave readiness consumer.
5. Explicit scaffold `--with-tui` and public facade.
6. Visual fidelity, advanced screens, and optional compact mode.
7. Consider a canonical turn/part Protocol projection only after measured
   Console/TUI reducer drift.
