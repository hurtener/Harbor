---
name: drive-the-harbor-tui
description: "Attach Harbor's native terminal Runtime test/control client with authenticated REST/SSE. Use when testing sessions, tasks, tools, artifacts, events, posture, interventions, controls, reconnect, or transcript export without opening the Console."
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: "tui, serve"
---

# Drive the Harbor TUI

`harbor tui` is an attach-only Harbor Protocol client. It does not start or own
the Runtime, import Runtime internals, call a private endpoint, or depend on the
Console.

## 0. One command (local dev)

For the local agent-development loop, co-launch the Runtime and the TUI in a
single process and terminal with the ephemeral dev token — zero config, no
token copy:

```bash
harbor dev --tui
```

`harbor dev --tui` boots the dev Runtime, waits for readiness, mints the dev
token in memory, and attaches the native TUI over authenticated loopback
REST/SSE. The TUI stays a pure Protocol client (no Runtime handle). Quitting the
TUI drains the owned Runtime and restores the terminal. Hot-reload is disabled
in this mode; use the two-process flow below (§1–§2) when you want the fsnotify
watcher, or to attach a TUI to a Runtime running elsewhere.

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

### Co-launch from `harbor serve --tui`

The attach flow above requires a separately-started Runtime. For a production
server (JWKS-verified JWT, no dev token), `harbor serve --tui` co-launches the
TUI from the serve binary itself — one process, one terminal:

```bash
# The operator supplies the JWT (HARBOR_TOKEN or ~/.harbor/token).
# serve verifies it against identity.jwks_url / jwks_file.
export HARBOR_TOKEN='<your-signed-jwt>'
harbor serve --config serve.yaml --bind 127.0.0.1:0 --tui
```

The serve binary boots the Runtime, waits for the listener to bind
(`WaitReady`), then attaches the TUI through authenticated REST/SSE — the same
Protocol-only path as `harbor tui --attach`. The TUI receives no Runtime
handle. Quitting the TUI drains the owned server (co-launch quit drains;
attach quit leaves the remote alive). Runtime logs go to a captured sink so
Bubble Tea frames are never overwritten; on server failure the terminal is
restored before the captured log is printed.

A scaffolded serving binary (`harbor scaffold --with-server --with-tui`) gains
the same `--tui` flag — the generated `cmd/<name>/main.go` calls `sdk/server`
and `sdk/tui` after `WaitReady`. See
[`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) §6.

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

+ `Enter`: submit the current turn.
+ `Alt+Enter` or `Shift+Enter`: insert a newline.
+ `Ctrl+A` / `Ctrl+E`: line start/end; `Ctrl+B` / `Ctrl+F`: move left/right.
+ `Ctrl+_` / `Alt+_`: undo/redo.
+ `PageUp` / `PageDown` scroll the conversation by lines; `Home` / `End` jump to
  the top / newest output (End re-engages tail-following). While scrolled away,
  new output never moves your view.
+ `Alt+J` / `Alt+K`: next/previous semantic transcript block.
+ `Ctrl+X F`: filter/search transcript blocks; `Ctrl+X X`: export Markdown.
+ `Ctrl+X Y`: timestamps.
+ `Ctrl+X B` / `Ctrl+X P`: stash/restore the local draft.
+ `Ctrl+X A`: upload `path|disposition`; `Ctrl+X E` removes and `Ctrl+X U` retries.
+ `Ctrl+X R` / `Ctrl+X O`: expand or collapse all reasoning / tool detail
  (collapsed by default); `Ctrl+X M`: reduced motion.
+ `Ctrl+P`: command palette; `Ctrl+X S`: context sidebar.
+ `/quit`, `Ctrl+C`, or `Ctrl+D`: persist interaction state, restore the terminal, and exit. A plain `q` remains composer text.

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

+ A closed durable session reopens on the next ordinary turn through canonical
  `start`; wait for `session.reopened` before treating it as running.
+ An erased session is terminal. Choose Start Fresh; ordinary attach/reconnect
  never retries the erased ID.
+ Replay gaps, retention truncation, partial counters, disconnects, and partial
  blocks remain visibly labelled.

The TUI owns the whole terminal on the alternate screen: a persistent banner
at the top (product, version, model, session, attach URL), the conversation
flowing top-down beneath it, and the composer pinned at the bottom. Quitting
restores the terminal exactly as it was. The `--compact` flag is deprecated
and has no effect.

Session commands are keyboard-driven: `Ctrl+X L` searches/switches, `Ctrl+X N`
starts fresh, `Ctrl+R` renames, and `Ctrl+X D` confirms canonical erasure.
Closed/failed picker rows say that the next canonical turn resumes them. Missing
Runtime capabilities or JWT scope disable the command with the exact reason.

## 6. Runtime inspection and control

The TUI remains a one-active-session surface. Runtime views never fan out into
fleet or simultaneous-session panes:

+ `F2`: task lifecycle, parent/child tree, progress, groups, activity latency,
  result/trajectory posture, and explicitly non-authoritative task cost.
+ `F3`: tool transport, schema, OAuth, approval, reliability, bounded metrics,
  and heavy-content posture. Rich rows require `tool_annotations`; analytics
  are labelled best-effort because the bounded scan has no completeness field.
+ `F4`: artifact metadata and references. Heavy bytes are never rendered
  inline; preview depends on canonical `artifacts.get_ref` support.
+ `F5`: live/retained event diagnostics and aggregates, with truncation and
  reconnect state visible.
+ `F6`: Runtime health, drivers, capabilities, identity, Protocol compatibility,
  retention, governance, LLM, metrics, and stream posture.
+ `F7`: the unified intervention queue, correlated by opaque `PauseToken` even
  when multiple interventions share one run.
+ `F8`: capability, retention, partiality, and typed Protocol failures.
+ `F9`: the one action matrix for task, intervention, tool, artifact, and
  session controls.

On list routes, `Up` / `Down` or `Ctrl+P` / `Ctrl+N` selects the exact row,
`PageUp` / `PageDown` changes the bounded page, and `/` edits the route filter.
The selected canonical ID or `PauseToken` is frozen into the confirmation; a
generation, identity, or inspection-epoch change closes stale action dialogs.

Every action row names its canonical method, required authority, target, and
confirmation posture. Cancel, reject, OAuth revoke, artifact deletion, and
session erasure require explicit destructive confirmation showing the active
identity target. A successful control response means accepted for delivery;
the TUI keeps a pending marker until `control.applied`, `control.rejected`, and
the resulting task/pause snapshot reconcile. HTTP 401, 403, 404, 409, and 501
remain visible with their canonical Protocol codes without changing a live
transport to disconnected. JWT authority is server-enforced and shown as
unknown until the server accepts or rejects the operation; capability
advertisement is not presented as proof of authorization. Disabled actions
stay in the matrix with an exact capability, lifecycle, or target reason.

Available task controls are canonical `cancel`, `pause`, `resume`, `redirect`,
`inject_context`, `user_message`, and `prioritize`. Interventions use only
`approve`, `reject`, or `resume` with the selected `PauseToken`; OAuth and input
requirements never bypass the unified pause primitive. There is no direct tool
invoke command, model picker, fleet control, or speculative method.

## Common failures

+ **Authentication expired:** atomically replace the token file, or press
  `Ctrl+X I` and paste a replacement JWT held in process memory only.
+ **Target session credential unavailable:** add the exact
  `tenant/user/session` entry to the token-file JSON map.
+ **Malformed or unreadable local state:** fix or remove the named state file.
  Harbor fails loudly rather than silently discarding drafts/preferences.
+ **Replay gap:** keep reading the visible partial output while the client
  performs an authoritative snapshot reconciliation.

## See also

+ [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) for starting the Runtime.
+ [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) for building
  another REST/SSE client.
+ [`observe-with-the-console`](../observe-with-the-console/SKILL.md) for the
  browser control plane.
