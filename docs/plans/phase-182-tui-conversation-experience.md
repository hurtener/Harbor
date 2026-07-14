# Phase 182 — TUI conversation and session experience

## Summary

Connect the Phase-180 projection core to the Phase-181 terminal system and ship
the first complete `harbor tui --attach` conversation experience. Sessions,
composer editing, history/stash/autocomplete, streaming, semantic navigation,
compact mode, export, and reconnect behavior must meet the binding TUI quality
floor before runtime-control breadth is added.

## RFC anchor

- RFC §3.1
- RFC §4.1
- RFC §5.1
- RFC §5.2
- RFC §5.4
- RFC §5.5
- RFC §8

## Briefs informing this phase

- brief 05
- brief 06
- brief 11
- brief 12

## Brief findings incorporated

- brief 05 §sessions: one user can hold multiple isolated concurrent sessions.
- brief 06 §events/replay: streaming and reconnect use the same canonical bus
  and cursor semantics.
- brief 11 §PG-1: direct interaction combines conversation, task progress,
  reasoning, and artifacts without owning execution.
- brief 12 §deployment: attach remains independently usable against any Runtime.

## Findings I'm departing from (if any)

None beyond the established browser-to-native rendering adaptation in Phase
181. The command and visual behavior follows the binding TUI conventions.

## Goals

- Deliver a conversation workflow whose editing, navigation, streaming, and
  session handling meet or exceed the market reference.
- Preserve local interaction state without shadowing Runtime entities.
- Make all reconnect and erased/partial states understandable and recoverable.

## Non-goals

- No broad tools/tasks/events/posture control center (Phase 183).
- No stock/scaffold co-launch (Phase 184).
- No durable server-side pending-turn queue.
- No coding-agent workspace or editor behavior.

## Acceptance criteria

- [ ] `harbor tui --attach <url>` performs auth/version/capability negotiation
      and opens the complete home/session flow.
- [ ] Session picker supports list/search/switch/new/rename/delete, local quick
      pins, closed-session Resume, erased-session Start Fresh, and stream-safe
      switching.
- [ ] Composer supports multiline editing, selection, undo/redo, Emacs-style
      movement, bracketed paste, bounded history, draft stash, attachments,
      slash commands, and `@session/@task/@artifact/@tool` references.
- [ ] Transcript renders user/answer/reasoning/task/tool/artifact/error/unknown
      blocks from the reducer with sticky-bottom behavior and semantic block
      navigation.
- [ ] Active streaming does not flicker, move unrelated blocks, steal focus, or
      yank a scrolled-away viewport; text/reasoning batching meets the perceived
      latency floor in the conventions.
- [ ] Follow-up intent is explicitly local, ordered, cancellable, and never
      presented as server-accepted before dispatch.
- [ ] Compact/native-scrollback mode, transcript copy/export, reasoning/detail
      toggles, timestamps, and retry remediation are keyboard-accessible.
- [ ] Local persistence stores bounded drafts/history/preferences/references,
      never Runtime rows or plaintext credentials.
- [ ] Every applicable home/session/composer/dialog/stream state in the binding
      visual matrix has reviewed goldens and PTY key walkthroughs.
- [ ] Auth expiry, disconnect, replay gap, closed/reopened, erased, resize while
      editing, and quit-during-stream preserve text and restore the terminal.

## Files added or changed

- `internal/tui/app/`
- `internal/tui/conversation/`
- `internal/tui/composer/`
- `internal/tui/sessionpicker/`
- `cmd/harbor/cmd_tui.go`
- `docs/skills/drive-the-harbor-tui/SKILL.md`
- matching `docs/site/` skill stub/navigation
- `README.md`
- `test/integration/tui_attach_test.go`
- `scripts/smoke/phase-182.sh`

## Public API surface

- CLI: `harbor tui --attach <url> [--session <id>]`.

## Test plan

- **Unit:** composer editing/history/stash/autocomplete, session actions,
  follow-up queue, transcript offsets/follow mode/export.
- **Integration:** real authenticated server; start/stream/reconcile, session
  switch/reopen/erase, disconnect/replay, attachment reference.
- **Conformance:** conventions golden matrix for conversation workflows and
  shared projection fixtures.
- **Concurrency / leak:** N≥100 session switches/reducer updates; PTY quit and
  signal paths restore terminal and goroutine baseline.

## Smoke script additions

- Assert `harbor tui --help` and run a bounded PTY attach/start/quit probe.
- Run composer/session/conversation tests under `-race`.
- Assert coding-agent terms/features are absent from CLI help and commands.

## Coverage target

- touched `internal/tui` conversation packages: 85%
- touched `cmd/harbor` TUI path: 80%

## Dependencies

- 180
- 181

## Risks / open questions

- Token identity/session switching must follow verified Protocol authority; the
  client never trusts body identity over auth context.
- Native terminal selection is emulator-dependent; explicit copy commands are
  the portable contract.

## Glossary additions

- attach mode
- local follow-up queue

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes
- [ ] N≥100 concurrent projection/session updates pass under `-race`
- [ ] Real authenticated PTY integration covers identity and failure modes
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
