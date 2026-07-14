# Phase 183 — TUI Runtime control and inspection

## Summary

Deepen the attach TUI into Harbor's terminal Runtime test/control surface:
tasks, tools, artifacts, events, posture, interventions, control actions,
diagnostics, attention, and renderer registries. Every new screen and state must
meet the same OpenCode-level quality contract as the conversation surface.

## RFC anchor

- RFC §3.3
- RFC §4.2
- RFC §5.1
- RFC §5.2
- RFC §5.4
- RFC §5.5
- RFC §6.3
- RFC §8

## Briefs informing this phase

- brief 02
- brief 03
- brief 05
- brief 06
- brief 11

## Brief findings incorporated

- brief 02 §pause/steering: every intervention routes through the unified
  pause/resume and canonical control taxonomy.
- brief 03 §tools: unknown tools require a safe generic renderer; transport and
  policy remain Runtime-owned.
- brief 05 §tasks/artifacts: terminal views consume scoped registries and
  artifact references rather than raw heavy content.
- brief 06 §observability: diagnostics derive from canonical events/posture and
  represent dropped/replayed data honestly.
- brief 11 §Live Runtime: task, intervention, artifact, event, and posture views
  form one operational lens.

## Findings I'm departing from (if any)

Brief 11's full fleet administration remains Console scope. This phase provides
single-runtime terminal inspection/control and capability-gates elevated views.

## Goals

- Make Runtime behavior inspectable and steerable without leaving the terminal.
- Provide generic extensible rendering with complete unknown fallbacks.
- Preserve honest partiality and privilege visibility in every screen.

## Non-goals

- No full multi-runtime fleet dashboard.
- No direct generic tool invocation until a policy-preserving Protocol method
  exists.
- No arbitrary MCP App HTML/PDF/audio/video host in the terminal.
- No coding-specific renderers.

## Acceptance criteria

- [ ] Task views provide list/filter/detail/status/result/trajectory projection,
      parent/child navigation, and explicit non-authoritative cost treatment.
- [ ] Tool views provide catalog/schema/policy/OAuth/metrics/content posture,
      capability-gated by `tool_annotations` with bounded analytics labelled
      best-effort.
- [ ] Artifact views provide list/reference/upload/delete where authorized and
      safe terminal previews/fallback metadata without raw heavy-content leaks.
- [ ] Event/diagnostic views provide live filters, raw safe payload inspection,
      retention/truncation/reconnect state, and export.
- [ ] Runtime status covers health, drivers, capabilities, identity, Protocol
      compatibility, retention horizons, LLM/governance posture, and stream state.
- [ ] Intervention inbox handles approval, rejection editor, resume, OAuth/input
      required, expiry, and resolved-elsewhere ordering through the unified
      primitive; destructive/elevated actions show target identity and confirm.
- [ ] Cancel, pause, resume, redirect, inject context, user message, and
      prioritize use canonical controls with disabled reasons based on scope and
      lifecycle.
- [ ] Typed part and tool renderer registries ship with generic fallbacks and no
      plugin API broader than the in-repo first consumers require.
- [ ] Attention/toast/notification behavior is deduplicated, non-modal unless
      action is required, and does not steal composer focus.
- [ ] Every applicable control, intervention, dialog, sidebar, fallback,
      diagnostics, and color-mode state has reviewed convention goldens and PTY
      keyboard walkthroughs.

## Files added or changed

- `internal/tui/tasks/`
- `internal/tui/tools/`
- `internal/tui/artifacts/`
- `internal/tui/events/`
- `internal/tui/interventions/`
- `internal/tui/renderers/`
- `internal/tui/posture/`
- `internal/tui/app/`
- `docs/skills/drive-the-harbor-tui/SKILL.md`
- matching `docs/site/` skill stub
- `test/integration/tui_control_test.go`
- `scripts/smoke/phase-183.sh`

## Public API surface

- No new wire or public Go surface. The attach CLI gains commands/screens over
  already-canonical methods.

## Test plan

- **Unit:** screen derivations, command availability, renderer dispatch,
  unknown fallbacks, intervention ordering, privilege/partiality labels.
- **Integration:** real drivers and auth; task/tool/artifact/event/posture reads,
  approval/reject/control paths, missing scope, unavailable capability.
- **Conformance:** applicable TUI visual matrix and canonical renderer fixture
  inventory.
- **Concurrency / leak:** N≥10 concurrent event producers/intervention changes;
  screen switching and stream teardown under `-race`.

## Smoke script additions

- Drive task, posture, intervention, and generic tool fallback through a bounded
  PTY session against the preflight server.
- Assert unavailable/partial data never renders as a fabricated zero.
- Run focused control/renderer tests under `-race`.

## Coverage target

- touched `internal/tui` control/inspection packages: 85%

## Dependencies

- 182
- 72e
- 72f
- 72g
- 73d
- 73f
- 73l
- 162
- 163
- 174
- 175
- 177
- 178

## Risks / open questions

- Protocol detail remains weaker than OpenCode for per-invocation tool records;
  the UI must not invent arguments/results or imply stable call IDs.
- Admin/fleet controls are privilege-sensitive; verified scope and explicit
  identity targets are mandatory.

## Glossary additions

- generic TUI renderer
- intervention inbox

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes
- [ ] Concurrent event/control test passes under `-race` with no leak
- [ ] Real-driver integration covers identity, scope rejection, and failure
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
