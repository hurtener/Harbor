# Harbor v1.15.0 — Native Runtime TUI Wave (phases 179–184)

> Coordination artifact for CLAUDE.md §17.7. The wave turns the exploratory TUI
> dossier into one authenticated Protocol-client product available through
> standalone attach, stock Runtime co-launch, and scaffolded Runtime co-launch.
> The binding UX minimum is `docs/design/tui/CONVENTIONS.md`: OpenCode-level or
> better perceived quality.

## 1. Executive Summary

The architecture is attach-first and client-only. Harbor remains headless; the
TUI never imports Runtime state or receives a private endpoint. Six phases keep
correctness, visual quality, interaction depth, control breadth, and
distribution independently reviewable:

1. **179 (D-315): Go Protocol client foundation.** One REST/SSE client and
   inspect-command consumers.
2. **180 (D-316): projection/reconciliation core.** Pure deterministic reducer,
   shared Console fixtures, replay/race/partiality correctness.
3. **181 (D-317): terminal foundation.** Bubble Tea shell, design system,
   commands/dialogs/focus, accessibility, goldens, PTY lifecycle.
4. **182 (D-318): conversation experience.** Complete attach flow, sessions,
   composer, streaming, navigation, compact mode, export, reconnect.
5. **183 (D-319): Runtime control/inspection.** Tasks, tools, artifacts, events,
   posture, interventions, controls, diagnostics, renderer registries.
6. **184 (D-320): distribution.** Readiness, `serve --tui`, `sdk/tui`, scaffold
   `--tui`, and three-mode wave-end PTY E2E.

The canonical ordered conversation/part projection remains a measured future
RFC candidate. This wave uses shared fixtures over canonical state/task/event
surfaces and does not add a private wire method.

## 2. Market Quality Floor

Functional parity is insufficient. The reference floor includes:

- stable three-surface visual hierarchy and heavy-left-rule grammar;
- exact 79/80 action and 120/121 sidebar responsive transitions;
- full editor-quality composer with history, stash, paste, and autocomplete;
- unified command palette, which-key, dialogs, focus restoration, and hints;
- sticky streaming that never yanks a scrolled-away reader;
- responsive normal/overlay sidebar and compact/native-scrollback behavior;
- dark/light, truecolor/256/16/monochrome, and reduced-motion modes;
- generic unknown tool/event/result fallbacks;
- complete intervention, reconnect, retry, and attention states; and
- terminal restoration on every success/error/signal/panic/server-loss path.

The binding matrix covers ten terminal sizes from 40x12 to 240x60. Every visual
phase carries reviewed goldens, keyboard walkthroughs, and PTY tests. A phase
that works but looks or behaves materially below the reference does not ship.

## 3. Product Boundary

In scope:

- sessions, turns, streaming, tasks, tools, interventions, artifacts, events,
  Runtime posture, controls, diagnostics, reconnect, and export;
- authenticated REST/SSE in every mode;
- local drafts, prompt history, visual preferences, and references; and
- terminal-safe lifecycle and responsive visual fidelity.

Explicitly excluded:

- Git/VCS, repository tree, source editing, patches, shell execution, LSP,
  worktrees, and coding-specific tool renderers;
- full Console/fleet-administration parity;
- Runtime-internal adapters, TUI-only endpoints, or a second event channel;
- shadow storage for Runtime entities; and
- `harbor dev` integration in this wave.

## 4. Version And Decisions

- Product target: **v1.15.0**.
- Base: tagged **v1.14.0** through Phase 178.
- Pre-assigned decisions:

| Phase | Decision | Subject |
|---|---|---|
| 179 | D-315 | One reusable authenticated Go Protocol client |
| 180 | D-316 | Pure projection/reconciliation core and shared fixtures |
| 181 | D-317 | OpenCode-level quality floor and Bubble Tea terminal foundation |
| 182 | D-318 | Complete attach conversation/session experience |
| 183 | D-319 | Generic Runtime control/inspection and renderer breadth |
| 184 | D-320 | Readiness-backed stock/scaffold distribution and ownership |

No Protocol version bump is planned. CLI additions and terminal dependencies
are RFC changes included in the planning PR.

## 5. Staging

### Stage 1 — Phase 179

Build the client and consume it from shipped `inspect-*` commands. This prevents
all later phases from copying command-local transport code.

### Stage 2 — Phases 180 And 181 In Parallel

These phases share only D-315:

- 180 builds pure projection correctness with no terminal framework;
- 181 builds the terminal system against fixture projections with no Runtime
  control logic.

Each has an in-phase consumer: shared Console/TUI fixture assertions consume
the reducer; the connection/home fixture shell consumes the visual system.

### Stage 3 — Phase 182

Join reducer and terminal foundation into the complete attach conversation
experience. This is the first user-operable TUI and the gate before breadth.

### Stage 4 — Phase 183

Add Runtime control and inspection only after conversation ergonomics and
terminal quality are proven.

### Stage 5 — Phase 184

Promote and distribute the completed runner. This phase carries the wave-end
E2E and cross-mode frame-equivalence gate.

The operator confirms this staging before implementation dispatch.

## 6. Binding Architecture

```text
harbor tui --attach ───────┐
                           │
harbor serve --tui ────────┼─> one TUI runner
                           │      ├─ terminal system
generated binary --tui ────┘      └─ projection core
                                          │
                                  Go Protocol client
                                          │
                                   REST + SSE + JWT
                                          │
                                    Harbor Runtime
```

- The TUI is part of the CLI product, not a fifth Runtime layer.
- Co-launch is process orchestration; it still dials the bound listener.
- Attach mode never owns its remote Runtime.
- Explicit co-launch modes own and drain their server.
- `sdk/tui` exposes connection options only.
- Client-local persistence holds drafts/preferences/history/references only.

## 7. Protocol Honesty Requirements

Consume and render:

- `session.reopened` and typed `session_erased`;
- session `counters_partial`;
- scoped retention horizons and event/history truncation;
- `tool_annotations` and `aggregates_partial`;
- nullable agent fields and optional task acknowledgements; and
- capability/scope absence distinct from legitimate zero.

Known gaps remain explicit:

- no canonical ordered conversation/part read model;
- no durable pending-turn queue;
- no authoritative per-task cost rollup; and
- bounded tool analytics lack a scan-truncation marker.

## 8. Terminal Stack

- `charm.land/bubbletea/v2 v2.0.8`;
- `charm.land/lipgloss/v2 v2.0.5`;
- `charm.land/bubbles/v2 v2.1.1`; and
- `github.com/charmbracelet/x/ansi v0.11.7`.

Glamour and Uniseg remain deferred until fixtures or profiling prove need. The
stack stays CGo-free and statically buildable.

## 9. Wave-End Gate

Phase 184's real-driver PTY E2E proves:

1. standalone attach conversation and controls;
2. stock `serve --tui` readiness and REST/SSE-only access;
3. generated `--with-server --with-tui` compiled-tool observation;
4. loud missing/rejected authentication in every mode;
5. cross-session identity isolation;
6. close/reopen and erased-terminal behavior;
7. approval/rejection through the unified primitive;
8. disconnect/replay with ordering or visible incompleteness;
9. attach-vs-owned-server shutdown semantics;
10. resize/theme/reduced-motion/fallback frame equivalence across modes; and
11. terminal and goroutine baseline restoration.

Run under `-race` with PTY fixtures and controllable clocks.

## 10. Documentation And Adoption

- 179 updates the Protocol skill for the Go client.
- 181 lands the binding TUI design conventions.
- 182 adds `drive-the-harbor-tui` and the docs-site entry.
- 183 deepens that skill for control/inspection.
- 184 updates scaffold skill, README, CHANGELOG, and launch/auth docs.

## 11. Completion Rule

After Phase 184 merges, run the mandatory read-only checkpoint audit and land
one audit-fix PR before beginning the next wave. V1.15 is not complete until
every phase gate, the full visual matrix, PTY E2E, coverage, and preflight pass.
