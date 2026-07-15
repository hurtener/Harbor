---
name: drive-the-harbor-tui
description: "Attach Harbor's native terminal conversation client to a running Runtime with authenticated REST/SSE. Use when testing sessions, streaming turns, token rotation, reconnect, local drafts, compact mode, or transcript export without opening the Console."
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: "tui"
---

# Drive the Harbor TUI

`harbor tui` is an attach-only Harbor Protocol client. It does not start or own
the Runtime, import Runtime internals, call a private endpoint, or depend on the
Console.

## 1. Start a Runtime

```bash
harbor dev
```

Copy the `HARBOR_DEV_TOKEN` printed to stderr. The token is bound to its full
tenant/user/session identity and expires; the TUI never extends it.

## 2. Attach

```bash
export HARBOR_TOKEN='<HARBOR_DEV_TOKEN>'
harbor tui --attach http://127.0.0.1:18080
```

To select a session explicitly:

```bash
harbor tui --attach http://127.0.0.1:18080 --session dev
```

One terminal has exactly one committed active session. Switching resolves the
target credential, opens a provisional target stream, hydrates under an overlap
fence, reconciles buffered frames, and then cancels and joins the old reader
before committing the target generation.

## 3. Rotate credentials

The default token file is `~/.harbor/token`. It may contain one JWT or a JSON
object keyed by `tenant/user/session` for authorized session switching:

```json
{
  "dev/dev/session-a": "<JWT A>",
  "dev/dev/session-b": "<JWT B>"
}
```

The file is read before every REST request and SSE connection/reconnect, so an
atomic replacement takes effect without restarting the TUI. Expired or
wrong-session credentials are shown as authentication failures; the draft,
session reference, and replay cursor stay local, but no request is presented as
accepted.

Use `--token-file <path>` to select another rotating file. Signed JWTs are never
written into TUI state. `Ctrl+X I` probes a replacement credential against the
complete authenticated attach join before committing it in memory; enter
`clear` to remove the in-memory override and resume the rotating file.

## 4. Conversation keys

- `Enter`: submit the current turn.
- `Alt+Enter` or `Shift+Enter`: insert a newline.
- `Ctrl+A` / `Ctrl+E`: line start/end; `Ctrl+B` / `Ctrl+F`: move left/right.
- `Ctrl+_` / `Alt+_`: undo/redo.
- `PageUp` / `PageDown` / `End`: scroll without losing sticky-bottom behavior.
- `Alt+J` / `Alt+K`: next/previous semantic transcript block.
- `Ctrl+X F`: filter/search transcript blocks; `Ctrl+X X`: export Markdown.
- `Ctrl+X R` / `Ctrl+X O` / `Ctrl+X Y`: reasoning, tool detail, timestamps.
- `Ctrl+X B` / `Ctrl+X P`: stash/restore the local draft.
- `Ctrl+X A`: upload `path|disposition`; `Ctrl+X E` removes and `Ctrl+X U` retries.
- `Ctrl+X C`: compact/native-scrollback mode; `Ctrl+X M`: reduced motion.
- `Ctrl+P`: command palette; `Ctrl+X S`: context sidebar.
- `/quit`, `Ctrl+C`, or `Ctrl+D`: persist interaction state, restore the terminal, and exit. A plain `q` remains composer text.

Autocomplete is capped at ten rows and executes reachable slash commands; its
structured references contain actual canonical IDs such as `@session:<id>`,
`@task:<id>`, `@artifact:<id>`, and `@tool:<name>`. Attachments retain
their upload lifecycle and optional Runtime disposition hint; a failed or
uploading attachment disables submit with a reason.

While a task is active, another submitted turn is visibly queued as local-only
intent. `Ctrl+X J` retries a failed follow-up and resumes ordered dispatch;
`Ctrl+X K` discards the newest queued or failed follow-up. The TUI dispatches in
order after the active task becomes terminal and never clears a draft or labels
text accepted before canonical `start` succeeds.

## 5. Session lifecycle

The last session reference and bounded interaction state are restored from
`~/.harbor/tui-state.json`, keyed by the full identity and Runtime instance/wire
fingerprint. The file contains drafts and view preferences only, never Runtime
rows, transcript content, or credentials.

- A closed durable session reopens on the next ordinary turn through canonical
  `start`; wait for `session.reopened` before treating it as running.
- An erased session is terminal. Choose Start Fresh; ordinary attach/reconnect
  never retries the erased ID.
- Replay gaps, retention truncation, partial counters, disconnects, and partial
  blocks remain visibly labelled.

Use `--compact` to keep committed output in native terminal scrollback rather
than the alternate screen.

Session commands are keyboard-driven: `Ctrl+X L` searches/switches, `Ctrl+X N`
starts fresh, `Ctrl+R` renames, and `Ctrl+X D` confirms canonical erasure.
Closed/failed picker rows say that the next canonical turn resumes them. Missing
Runtime capabilities or JWT scope disable the command with the exact reason.

## Common failures

- **Authentication expired:** atomically replace the token file, or press
  `Ctrl+X I` and paste a replacement JWT held in process memory only.
- **Target session credential unavailable:** add the exact
  `tenant/user/session` entry to the token-file JSON map.
- **Malformed or unreadable local state:** fix or remove the named state file.
  Harbor fails loudly rather than silently discarding drafts/preferences.
- **Replay gap:** keep reading the visible partial output while the client
  performs an authoritative snapshot reconciliation.

## See also

- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) for starting the Runtime.
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) for building
  another REST/SSE client.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) for the
  browser control plane.
