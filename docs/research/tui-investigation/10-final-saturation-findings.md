# Final Saturation Findings

The final OpenCode source pass targeted the remaining generic gaps. It found
several defensive requirements but no new top-level architecture. Further
local OpenCode excavation is unlikely to add material generic Harbor TUI
findings.

## 1. Armed Controls Are Session-Scoped State

The safe implementation is not a global counter. Each armed operation needs:

```text
session ID + task generation + operation + one replaceable deadline
```

Reset on:

- session switch;
- active task generation change;
- control applied/rejected;
- input clear;
- explicit disarm;
- deadline expiry; and
- teardown.

Use a deterministic clock in tests. Never leave multiple expiry timers active.

OpenCode's compact mode demonstrates the safer owned-timeout pattern; the
full-screen prompt exposes stale timer and cross-session risks.

Sources:

- `_ref/opencode-dev/packages/tui/src/component/prompt/index.tsx:283-308,392-419`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/footer.ts:684-692,930-1005`

## 2. Exit Semantics Belong To The Host

All presentation modes should share one host policy:

- first interrupt clears a non-empty draft when that is the configured
  behavior;
- exit while work is active is armed;
- second press inside the window requests exit/cancel;
- failed/rejected control remains visible;
- expiry and session/task changes disarm; and
- normal idle exit is immediate or explicitly configured.

Do not implement different exit safety independently in full and compact
shells.

## 3. Session Picker Needs Request Generations

Every async list/search request should capture:

```text
connection identity + tenant + user + scope + query + generation
```

Ignore results when any captured value no longer matches. Cancel where
possible. Required race tests:

- search B resolves before A;
- identity/connection changes while A is pending;
- dialog closes before completion;
- deletion and refresh overlap search; and
- stale selected row is rejected under a newer scope.

OpenCode's current tests do not establish these guarantees.

Sources:

- `_ref/opencode-dev/packages/tui/src/component/dialog-session-list.tsx:62-93`
- `_ref/opencode-dev/packages/tui/test/component/dialog-session-list.test.ts:4-46`

## 4. Reconnect Conformance

Classify and test:

- clean EOF;
- connection rejection;
- iterator/read failure;
- malformed termination;
- authentication expiry;
- cancellation during backoff;
- shutdown during backoff;
- duplicate replay frames;
- replay cursor propagation; and
- queue pressure during prolonged disconnect.

Each attempt is caught inside the retry loop. Flush coalesced reducer input
before retry. Backoff resets only after a defined stable interval.

Recovery should be a fenced transaction:

```text
subscribe/replay -> authoritative snapshot refresh/merge -> expose live
```

Blind reconnect is insufficient.

## 5. Feature Initialization Cannot Block Host Restoration

Bound and cancel feature initialization as well as cleanup. Host-critical
terminal restoration and stream cancellation must not wait for:

- plugin/feature load;
- feature initialization;
- feature cleanup; or
- a shared cleanup timeout.

Test hanging initialization, hanging cleanup, ordinary cleanup errors, reverse
feature order, reverse registration order, and terminal restoration before any
feature budget expires.

Sources:

- `_ref/opencode-dev/packages/opencode/src/plugin/tui/runtime.ts:388-555,1029-1048`
- `_ref/opencode-dev/packages/tui/src/app.tsx:191-228`

## 6. Partial Boot Fault Injection

Install restoration immediately after terminal acquisition. Fault-inject at:

- renderer creation;
- theme/background query;
- keymap registration;
- dynamic component import;
- component construction;
- first layout/render;
- stream startup; and
- first snapshot hydration.

Assert restoration of screen mode, cursor, mouse, paste, keyboard protocol,
terminal title, signals, stdin, stdout/stderr routing, and renderer resources.

## 7. Which-Key Continuation Tests

Preview remains observational. Prove:

- valid leader continuation dispatches underlying command;
- unmatched continuation is not swallowed;
- escape and timeout clear preview and sequence;
- panel scrolling keys win only where intended; and
- collisions between continuation and panel navigation have explicit
  precedence.

## 8. Real PTY Lifecycle Gate

Model and renderer tests are not enough. A future implementation needs a real
PTY harness covering:

- normal exit;
- SIGINT, SIGTERM, and SIGHUP;
- startup failure;
- panic-equivalent failure;
- resize;
- suspend/SIGCONT on Unix;
- server-first shutdown;
- renderer-first shutdown; and
- Windows console restoration.

Assert terminal modes and emitted control sequences, not only an internal
`destroyed` flag.

## 9. Fatal-Error Fallback

The TUI needs a minimal failure screen outside its normal dependency graph. It
must not depend on:

- the transcript reducer;
- live Protocol client;
- compiled theme;
- command registry;
- dialog stack; or
- optional feature modules.

It should provide:

- safe fallback palette;
- scrollable redacted error details;
- copy diagnostics;
- quit; and
- restart only if restart rebuilds the complete client/reducer scope.

The fallback and restart path require dedicated failure tests.

Source:

- `_ref/opencode-dev/packages/tui/src/component/error-component.tsx:10-98`

## 10. Saturation Verdict

Strict saturation is reached for the local OpenCode snapshot's generic-agent
surface. Covered areas include:

- architecture and transport;
- full and compact presentation shells;
- command/keybinding and modal systems;
- prompt history, drafts, queue, and autocomplete;
- sessions, timeline, export, interventions, retry, and attention;
- renderer/tool/part registries and extension lifecycle;
- visual measurements, theme, responsiveness, and terminal capabilities;
- hydration, replay, reconnect, race, and cleanup constraints; and
- Harbor Protocol mapping and Bubble Tea implementation options.

Further profitable work is Harbor-native validation:

- captured Harbor wire fixtures;
- shared Console/TUI projection tests;
- a prototype reducer spike;
- Bubble Tea geometry spikes under the visual matrix; and
- PTY lifecycle tests.

Those activities belong to a future approved RFC/phase, not this research-only
branch.

## 11. Post-Rebase Harbor Delta

After rebasing onto the v1.14 fixes that form the v1.15 base:

- closed-session reactivation is implemented through `start` and
  `session.reopened`;
- erased sessions fail with typed `session_erased`;
- session counters, pending-intervention state, retention scope, and partiality
  signals are materially more honest;
- `tool_annotations` enables real catalog-level policy/OAuth/metric posture;
- the canonical ordered transcript and durable user-turn gap remains open;
- production `tasks.get` cost remains non-authoritative; and
- bounded tool analytics still lack a truncation marker.

See `12-v115-main-rebase-delta.md` for the full inventory update.
