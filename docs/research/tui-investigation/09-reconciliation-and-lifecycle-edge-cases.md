# Reconciliation And Lifecycle Edge Cases

This pass inspected OpenCode's tests for hydration, replay, queueing,
interventions, notifications, plugins, and terminal cleanup. It did not change
the top-level Harbor architecture, but it found constraints that must be part
of any future implementation plan.

## 1. Live/Snapshot Merge Needs Fences And Tombstones

A snapshot fetched during hydration or replay must not overwrite events that
arrived after the fetch began:

- live part content wins over stale snapshot content;
- a live deletion prevents resurrection;
- retention is applied after merge, with cleanup of child indexes;
- an orphan delta does not suppress a later authoritative part; and
- terminal snapshots reconcile state without replacing newer local lifecycle
  transitions.

Sources:

- `_ref/opencode-dev/packages/tui/test/cli/cmd/tui/sync-live-hydration.test.tsx:36-262`

Harbor's reducer needs a snapshot generation or mutation journal plus deletion
tombstones. Ordinary fetch-then-replace is incorrect.

## 2. Compact Resize Replay Is Latest-Request-Wins

For future immutable-scrollback mode, resize requests can arrive while a replay
is snapshotting, resetting, rebuilding, or draining. Requirements:

- intermediate requests do not reset the terminal;
- exactly one trailing replay runs;
- it uses the newest dimensions/reset callback;
- requests during event drain still coalesce;
- snapshot acquisition completes before destructive reset; and
- failure preserves current scrollback and disables future auto-reflow
  visibly.

Sources:

- `_ref/opencode-dev/packages/opencode/test/cli/run/stream.transport.test.ts:819-930,1088-1120`

A plain debounce or mutex is insufficient. Use a serialized state machine with
one replaceable trailing request.

## 3. Optimistic Anchors May Target Inside A Block

A local diagnostic can occur between two streamed deltas that later persist as
one coalesced block. Replaying by block ID alone loses chronology. Compact-mode
anchors need a stable block identity plus visible-prefix offset or an
equivalent stream position.

Persisted prompt appearance should remove its duplicate optimistic row while
retaining independent local diagnostics.

Sources:

- `_ref/opencode-dev/packages/opencode/test/cli/run/session-replay.test.ts:423-446,522-565`

## 4. Queue Failure Must Preserve Unrelated Intent

OpenCode serializes local dispatch and publishes copies of queue state, but one
failed turn currently closes the loop and clears the rest. Harbor should define
a safer rule:

1. mark the failed active entry;
2. stop automatic dispatch;
3. retain all unrelated pending entries;
4. keep them editable/removable; and
5. require explicit resume/retry.

Queue projections sent into Bubble Tea should be immutable copies.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/runtime.queue.ts:74-83,212-249,331-348`
- `_ref/opencode-dev/packages/opencode/test/cli/run/runtime.queue.test.ts:321-360,468-480`

## 5. Submission Gate Precedes Every Async Boundary

Concurrent Enter events can both pass empty/input validation and then race
session creation or composer clearing. The submit path must atomically:

1. acquire exclusion;
2. snapshot text, artifacts, dispositions, overrides, and target session;
3. clear/transition UI state; and only then
4. start I/O.

Sources:

- `_ref/opencode-dev/packages/tui/test/cli/tui/prompt-submit-race.test.ts:3-97`

Client-generated session IDs do not remove this requirement.

## 6. Intervention Anti-Resurrection

A delayed `pause.list` response may arrive after a live resolution event. It
must not reopen the blocker. Permission/approval ordering may also supersede an
already displayed lower-priority question.

Use snapshot generation fencing or resolved-token tombstones. Canonical
sequence ordering alone does not protect against an older HTTP response
completing later.

Sources:

- `_ref/opencode-dev/packages/opencode/test/cli/run/stream.transport.test.ts:1885-2006`
- `_ref/opencode-dev/packages/opencode/test/cli/run/session-data.test.ts:181-235`

## 7. Repair Missing Lifecycle Starts

A terminal tool/task/control event may arrive without its opening event because
of replay gaps, filtering, retention, or reconnect. The reducer must synthesize
a valid standalone block, mark it incomplete where useful, and apply the
terminal state.

On cancel, stream loss, or shutdown, all open blocks are flushed and sealed
exactly once. Repeated sealing is a no-op.

Sources:

- `_ref/opencode-dev/packages/opencode/test/cli/run/session-data.test.ts:511-570`

Harbor session lifecycle adds two rules:

- `session.reopened` invalidates stale snapshots that still mark the session
  closed; and
- `session_erased` is a terminal tombstone. Reconnect logic must not retry
  `start` against that ID as though it were an ordinary closed session.

## 8. Which-Key Preview Is Observational

Leader-sequence preview must not push a modal mode or consume unmatched
continuation keys. It may register dedicated scrolling keys at higher priority,
but the underlying command registry still owns continuation dispatch.

Global mode-less bindings remain reachable while question/autocomplete modes
are active.

Sources:

- `_ref/opencode-dev/packages/tui/src/feature-plugins/system/which-key.tsx:194-366`
- `_ref/opencode-dev/packages/tui/test/keymap.test.tsx:66-140`

## 9. Attention Dedupe Has Lifecycle Scope

Deduplication lasts only while a request is unresolved. Resolution clears the
entry, allowing a later generation with the same ID to notify again. Focus
begins unknown; focus-conditional channels fail quiet until a real focus/blur
event establishes posture.

Harbor keys should include identity, session, pause token, and unresolved
generation rather than use a process-lifetime set.

Sources:

- `_ref/opencode-dev/packages/tui/test/cli/cmd/tui/notifications.test.ts:110-137`
- `_ref/opencode-dev/packages/opencode/test/cli/cmd/tui/attention.test.ts:114-165`

## 10. Host-Critical Cleanup Is Outside Feature Budgets

Feature/plugin cleanup can hang or fail. Terminal restoration, stream
cancellation, credentials cleanup, and signal-handler removal must live in the
host lifecycle outside feature-module cleanup budgets.

Requirements:

- immediately install finalizers after acquisition;
- feature cleanup handles are idempotent;
- ordinary feature errors do not stop remaining cleanup;
- a feature timeout cannot starve terminal restoration; and
- failed initialization removes partial registrations.

Sources:

- `_ref/opencode-dev/packages/opencode/src/plugin/tui/runtime.ts:388-467`
- `_ref/opencode-dev/packages/opencode/test/cli/tui/plugin-lifecycle.test.ts:11-224`
- `_ref/opencode-dev/packages/opencode/test/cli/tui/plugin-loader.test.ts:1167-1219`

## 11. Partial Boot Failure Must Restore The Terminal

OpenCode mini mode exposes an uncovered leak: renderer creation can succeed,
then later initialization fail without restoring captured output or destroying
the renderer.

Harbor must acquire terminal ownership with an immediate defer/finalizer before
theme probing, keymap setup, component construction, or first render.

Fault-inject after every stage and assert restoration of:

- alt/inline screen mode;
- cursor visibility;
- mouse mode;
- bracketed paste;
- keyboard enhancement mode;
- terminal title;
- signal handlers;
- stdin state; and
- stdout/stderr routing.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/runtime.lifecycle.ts:88-107,176-200,401-405`

## 12. Reconnect Must Catch Every Failure Shape

Clean EOF, connect failure, iterator failure, malformed stream termination,
authentication expiry, and cancellation during backoff need explicit tested
transitions. Catch each attempt inside the retry loop, flush pending reducer
input before retry, and reset backoff only after a defined stable interval.

Sources:

- `_ref/opencode-dev/packages/tui/src/context/sdk.tsx:82-116`
- `_ref/opencode-dev/packages/opencode/test/cli/run/stream.transport.test.ts:2229-2269`

## 13. Future Harbor Test Matrix Additions

### Reconciliation

- live mutation during snapshot request;
- deletion during snapshot request;
- orphan delta followed by authoritative part;
- retention eviction after merge;
- stale pause snapshot after resolution;
- terminal event without start;
- repeated terminal sealing;
- duplicate and out-of-order sequence;
- replay gap during active tool and reasoning streams;
- closed-session snapshot racing `session.reopened`;
- erased-session tombstone during retry;
- `counters_partial` and retention-horizon scope transitions; and
- aggregate `truncated` transitions.

### Submission And Queue

- duplicate Enter in same scheduler tick;
- session switch during submit;
- artifact/override mutation during submit;
- one queue entry failure with later entries retained;
- retry/resume dispatch;
- immutable queue projection;
- exit with pending local entries.

### Lifecycle

- failure after every acquisition stage;
- server-first and renderer-first failure;
- cancel/exit arming expiry race;
- session switch while cancel is armed;
- control rejection after second press;
- cancellation during reconnect backoff;
- token expiry during stream;
- terminal cleanup after panic-equivalent error paths.

## 14. Saturation

The architecture is saturated: no new top-level boundary emerged. The
Protocol-client model, pure reducer, owned stream reader, local queue,
intervention inbox, command registry, and host-owned lifecycle remain correct.

Remaining useful work is validation rather than architecture discovery:

- direct which-key continuation tests;
- cancel/exit arming races;
- session-picker request races;
- reconnect conformance;
- partial boot fault injection;
- PTY behavior; and
- Harbor-native captured wire fixtures.
