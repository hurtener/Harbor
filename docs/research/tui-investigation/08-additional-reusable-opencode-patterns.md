# Additional Reusable OpenCode Patterns

This pass inspected OpenCode's mini TUI, system feature plugins, attention
service, session dialogs, retry behavior, and shutdown presentation. It records
only patterns that remain useful for a generic Harbor runtime TUI.

## 1. Compact Split-Footer Mode

OpenCode's mini UI is not merely a smaller full-screen layout. It divides the
terminal into:

1. immutable native scrollback for committed transcript entries; and
2. one mutable split footer for composer, status, menus, interventions, local
   queue, and child-task inspection.

Only the footer is repainted. Streaming text commits stable complete rows and
keeps the unstable final row mutable. Markdown commits only parser-designated
stable blocks.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/types.ts:1-13`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/runtime.lifecycle.ts:171-200`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.ts:1-26,531-564`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/scrollback.surface.ts:225-307`

### Harbor Mapping

Keep the full-screen TUI as the primary product, but avoid coupling the
Protocol client and transcript reducer to a viewport shell. A later mode could
be:

```text
harbor tui --compact
```

Both shells would consume the same typed projection:

```go
type Presentation interface {
    Append(TranscriptBlock)
    PatchFooter(FooterState)
    PresentIntervention(Intervention)
}
```

Compact mode is not first-release scope. Its existence does strengthen the
decision to keep reducer, command registry, and renderer semantics independent
of Bubble Tea screen composition.

## 2. Resize Replay For Immutable Scrollback

Native scrollback cannot be safely reflowed. Mini mode uses a transactional
resize rebuild:

1. debounce resize for 250 ms;
2. fetch fresh history and intervention snapshots;
3. buffer incoming events;
4. wait for pending output to settle;
5. reset split-footer scrollback;
6. replay server-backed history;
7. merge bounded optimistic rows at stable anchors;
8. drain deduplicated live events; and
9. visibly disable future replay if reconstruction fails.

Only the newest 100 optimistic local rows are retained.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/runtime.ts:366-369,504-535`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/stream.transport.ts:978-1124`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/session-replay.ts:263-326`

This is required only by a future compact mode. A full-screen viewport can
reflow its retained typed blocks directly.

## 3. Editable Client-Local Follow-Up Queue

Prompts submitted during an ordinary active turn remain client-local and are
dispatched one at a time. Pending entries receive IDs immediately and can be
searched, edited, or removed until execution starts. Editing atomically
removes the queue entry and restores it to the composer.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/runtime.queue.ts:1-10,54-59,268-329`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.command.tsx:672-767`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.view.tsx:691-703`

### Harbor Rules

- Label entries as `local queue`.
- They are not Harbor tasks.
- Edit/delete is available only before dispatch.
- Dispatch creates another `start` in the same session.
- Distinguish queued follow-up from immediate `user_message` steering.
- Do not imply multi-client durability or reconciliation.

This is a fidelity feature, not a reason to invent a private Protocol queue.

## 4. Priority-Ordered Intervention Inbox

Mini mode merges pending blockers from parent and child sessions into one
ordered queue. Only the earliest unresolved blocker owns the footer. Resolved
entries disappear, and the remaining count stays visible.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/stream.transport.ts:289-361,470-502`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.question.tsx:1-14,167-349`

### Harbor Rules

- Rehydrate through `pause.list` on attach/reconnect.
- Include authorized blockers across the selected task tree.
- Order by canonical pause/event sequence where available, not only client
  receive time.
- Show one active intervention plus count/navigation.
- Use generic payload rendering unless Harbor defines a typed question model.

This belongs in first-release intervention behavior.

## 5. Which-Key From The Live Command Registry

OpenCode's which-key panel queries active reachable commands for the current
mode. It automatically reflects route, modal state, disabled commands, user
overrides, leader continuations, and active feature modules.

It supports a dock that consumes layout height and an overlay that can appear
only while a sequence is pending. Entries group by category and use up to
three responsive columns.

Sources:

- `_ref/opencode-dev/packages/tui/src/feature-plugins/system/which-key.tsx:184-366,532-605`

Harbor's command registry must be authoritative for:

```text
ID, title, description, category, bindings,
active mode, enabled predicate, palette/help visibility
```

Command palette, help, footer hints, slash autocomplete, and which-key should
derive from it. Start with palette/help; add automatic pending-sequence preview
after leader behavior is stable.

## 6. Capability-Aware Tips

Tips disappear when their command has no active binding and format shortcuts
from the live keymap. Connection/onboarding state can replace random tips, and
the chosen tip is stable for one component mount.

Sources:

- `_ref/opencode-dev/packages/tui/src/feature-plugins/home/tips-view.tsx:78-147`
- `_ref/opencode-dev/packages/tui/src/feature-plugins/home/tips.tsx:9-51`

Harbor tips should be predicate-driven, low stakes, and sourced from command
IDs rather than hard-coded keys. This is polish only.

## 7. Transition-Aware Attention

Notifications are reduced from state transitions rather than emitted on every
raw event:

- one notification per question/permission request ID;
- done only on active/retry to idle;
- completion suppressed after an announced error;
- child/background completion uses lower-noise policy;
- OS notifications can be blurred-only while sound remains independent; and
- Runtime text is ANSI/control-sanitized and truncated.

Sources:

- `_ref/opencode-dev/packages/tui/src/feature-plugins/system/notifications.ts:9-87`
- `_ref/opencode-dev/packages/tui/src/attention.ts:41-75,107-252`
- `_ref/opencode-dev/packages/tui/src/config/index.tsx:100-109`

A future Harbor `AttentionService` should consume the normalized projection,
not token chunks. Keep it opt-in. Audio is not a first-release dependency.

## 8. Plugin Lifecycle Lesson

OpenCode tracks registrations inside a plugin scope, aborts it on disposal,
cleans registrations in reverse order, applies a cleanup timeout, and starts
plugins sequentially because order affects precedence.

Sources:

- `_ref/opencode-dev/packages/opencode/src/plugin/tui/runtime.ts:388-650,1093-1118`

Harbor should retain only this lesson for statically linked feature modules:

- registration returns cleanup;
- cleanup is joined and bounded;
- ordering is explicit; and
- failed initialization removes partial registrations.

Do not copy arbitrary package installation or runtime code loading.

## 9. Session Picker Behavior

Reusable details:

- browse page and remote search are separate;
- non-empty search is debounced 150 ms;
- server results reconcile with fresher live records;
- current and pinned sessions remain visible even when outside the page;
- groups are Pinned, Today, and date;
- busy/retrying sessions show a spinner;
- up to nine client-local pins become quick-switch slots;
- delete uses inline two-press confirmation; and
- deletion of the current session returns to home.

Sources:

- `_ref/opencode-dev/packages/tui/src/component/dialog-session-list.tsx:22-93,208-345`
- `_ref/opencode-dev/packages/tui/src/context/local.tsx:452-499`
- `_ref/opencode-dev/packages/tui/src/app.tsx:1008-1015`

Harbor must still clone the session-scoped client and replace the stream when a
quick slot is selected. Pins are local references, not Runtime state.

A closed Harbor session should expose Resume, implemented by `start` against
the same ID and confirmed by `session.reopened`. An erased ID is terminal; the
picker should explain the 409 `session_erased` result and offer Start Fresh
with a new ID.

## 10. Timeline And Export

### Timeline

OpenCode indexes meaningful user turns, previews selection in the transcript,
then opens nested actions. Harbor can initially provide navigation, copy, and
inspect only. Reliable timeline behavior reinforces the need for stable
durable user turns and block IDs. Improved session counters do not close that
turn-identity gap.

Sources:

- `_ref/opencode-dev/packages/tui/src/routes/session/dialog-timeline.tsx:22-46`
- `_ref/opencode-dev/packages/tui/src/routes/session/dialog-message.tsx:21-107`

### Export

Export independently controls reasoning, tool detail, assistant/task metadata,
and filename, defaulting from but not mutating current display preferences.
Harbor should add optional raw event appendix and use the same redacted
projection as the screen.

Sources:

- `_ref/opencode-dev/packages/tui/src/ui/dialog-export-options.tsx:8-73`
- `_ref/opencode-dev/packages/tui/src/routes/session/index.tsx:914-978`

## 11. Armed Destructive Controls

OpenCode requires a repeated interrupt inside a five-second window:

1. first press arms and changes the hint;
2. second press sends abort;
3. armed state expires after five seconds; and
4. new activity clears it.

Sources:

- `_ref/opencode-dev/packages/tui/src/component/prompt/index.tsx:392-419,1584-1588`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.ts:907-1005`

Harbor should use this for:

- `cancel`;
- exit while a task is active; and
- destructive session erasure.

Do not require it for reversible `pause`. The second press only submits the
control; UI state changes after `control.applied` or `control.rejected`.

## 12. Retry And Remediation

Inline retry state shows sanitized message, attempt number, live countdown,
and expandable details. A separate remediation dialog appears only when a
typed action exists and is rate-limited by reason.

Sources:

- `_ref/opencode-dev/packages/tui/src/component/prompt/index.tsx:1510-1589`
- `_ref/opencode-dev/packages/tui/src/routes/session/index.tsx:90-114,350-368`

Harbor should infer no remediation from error prose. Typed actions may later
open authentication, governance, or provider-posture views.

## 13. Safe Epilogue And Diagnostics Bundle

After restoring the terminal, full-screen mode prints a route-owned epilogue.
Harbor should print session ID/title, endpoint display name, safe continuation
command, and active terminal task state, never credentials.

```text
Session   investigate-pause
Continue  harbor tui --attach https://runtime.example --session 01J...
```

A separate one-key diagnostics bundle should copy only redacted client,
Runtime, Protocol, terminal, stream, session/task, capability, and driver
metadata.

Sources:

- `_ref/opencode-dev/packages/tui/src/util/presentation.ts:29-38`
- `_ref/opencode-dev/packages/tui/src/app.tsx:353-362`
- `_ref/opencode-dev/packages/tui/src/component/dialog-debug.tsx:22-88`

## 14. Saturation After This Pass

The generic interaction surface is highly covered. Remaining profitable
research targets are:

1. tests for mini resize replay, queue races, and blocker ordering;
2. exhaustive edge cases in the mini event reducer;
3. terminal-protocol behavior inside OpenTUI itself, which is external to the
   snapshot; and
4. Harbor-specific reducer fixtures and wire-gap validation.

Provider commerce, workspaces, self-upgrade, arbitrary plugin installation,
and the intentionally deferred coding surfaces are not useful next targets for
this product.
