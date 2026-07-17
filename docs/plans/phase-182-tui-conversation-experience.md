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
- Optimize for one developer/operator and one active session per terminal while
  preserving Harbor's mandatory identity triple at every Protocol call.
- Preserve local interaction state without shadowing Runtime entities.
- Make all reconnect and erased/partial states understandable and recoverable.

## Non-goals

- No broad tools/tasks/events/posture control center (Phase 183).
- No stock/scaffold co-launch (Phase 184).
- No durable server-side pending-turn queue.
- No coding-agent workspace or editor behavior.

## Acceptance criteria

- [x] `harbor tui --attach <url>` performs auth/version/capability negotiation
      and opens the complete home/session flow with exactly one active session.
- [x] Session picker supports list/search/switch/new/rename/delete, closed-session
      Resume, erased-session Start Fresh, and stream-safe switching. It is a
      single-operator selector, not a simultaneous multi-session dashboard;
      switching tears down the old stream and reacquires a target-session JWT.
- [x] The lifetime-scoped token source resolves before every REST request and SSE
      connection, reloads a rotated token file, and can accept an in-memory
      replacement after visible `401` expiry without losing draft, session, or
      replay cursor. It never silently extends or persists a signed JWT.
- [x] The last active session reference is restored on terminal restart. Durable
      history rehydrates under that same ID; if closed/GC-reaped, the next turn
      uses canonical `start` and observes `session.reopened`. Erased sessions do
      not reopen and require explicit Start Fresh.
- [x] Composer supports multiline editing, selection, undo/redo, Emacs-style
      movement, bracketed paste, bounded history, draft stash, attachments,
      slash commands, and `@session/@task/@artifact/@tool` references.
- [x] Transcript renders user/answer/reasoning/task/tool/artifact/error/unknown
      blocks from the reducer with sticky-bottom behavior and semantic block
      navigation.
- [x] Active streaming does not flicker, move unrelated blocks, steal focus, or
      yank a scrolled-away viewport; text/reasoning batching meets the perceived
      latency floor in the conventions.
- [x] Follow-up intent is explicitly local, ordered, cancellable, and never
      presented as server-accepted before dispatch.
- [x] Compact/native-scrollback mode, transcript copy/export, reasoning/detail
      toggles, timestamps, and retry remediation are keyboard-accessible.
- [x] Local persistence stores bounded drafts/history/preferences and the last
      session reference, never Runtime rows or plaintext credentials.
- [x] Every applicable home/session/composer/dialog/stream state in the binding
      visual matrix has reviewed goldens and PTY key walkthroughs.
- [x] Auth expiry/rotation, disconnect, replay gap, terminal restart,
      closed/reopened, erased, resize while editing, and quit-during-stream
      preserve text and restore the terminal.

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

- **Unit:** composer editing/history/stash/autocomplete, one-active-session
  transitions, credential refresh/replacement, follow-up queue, transcript
  offsets/follow mode/export.
- **Integration:** real authenticated server; start/stream/reconcile, session
  switch with target JWT, token-file rotation, restart/reopen/erase,
  disconnect/replay, attachment reference.
- **Conformance:** conventions golden matrix for conversation workflows and
  shared projection fixtures.
- **Concurrency / leak:** N≥100 reducer/credential-source updates against shared
  immutable components; sequential active-session switches cancel and join the
  prior stream; PTY quit and signal paths restore terminal and goroutine baseline.

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

- Token identity/session switching follows verified Protocol authority; the
  client never trusts body identity over auth context. A fixed token that expires
  cannot self-renew, so absent file/issuer rotation the UI requests replacement
  visibly and preserves local interaction state while disconnected.
- Native terminal selection is emulator-dependent; explicit copy commands are
  the portable contract.

## Glossary additions

- attach mode
- local follow-up queue

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation test passes
- [x] N≥100 concurrent projection/session updates pass under `-race`
- [x] Real authenticated PTY integration covers identity and failure modes
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A; no departure
