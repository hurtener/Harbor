# OpenCode Command And Lifecycle Inventory

This inventory records the complete interaction families discovered in the
captured OpenCode TUI. Items marked **deferred coding reference** are not
requirements for the generic Harbor TUI.

## 1. Launch And Attachment Modes

### Full Local TUI

The default command:

- resolves and changes to the project directory;
- starts a dedicated server worker;
- combines piped stdin and an explicit prompt;
- loads TUI configuration;
- validates an explicit session before rendering;
- schedules update checks;
- launches the full-screen TUI; and
- shuts down worker and renderer with a bounded timeout.

When no hostname, port, or mDNS option is supplied, the SDK uses a synthetic
`http://opencode.internal` endpoint: request fetches cross worker RPC into the
server handler and events are forwarded from the worker bus. With explicit
network options, the worker opens a real HTTP listener and the same SDK uses
network transport.

### Remote Attach

`attach <url>` uses the same full TUI against a remote server. It supports
directory scope, continuation/session/fork, and basic authentication. It does
not create a local worker.

### Mini TUI

Mini mode uses native scrollback plus a mutable footer rather than a full-
screen viewport. It supports local and remote attachment and is described in
`08-additional-reusable-opencode-patterns.md`.

Sources:

- `_ref/opencode-dev/packages/opencode/src/cli/cmd/tui.ts`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/attach.ts`
- `_ref/opencode-dev/packages/opencode/src/cli/tui/worker.ts`
- `_ref/opencode-dev/packages/opencode/src/cli/cmd/run/`

## 2. Command Registry Semantics

Commands are independent of keybindings and carry:

- stable command ID;
- title and optional description;
- category;
- hidden/suggested/enabled state;
- slash command name and aliases;
- context/mode activation; and
- execution callback.

Keymap behavior includes:

- `leader` token, default `ctrl+x`;
- two-second leader timeout;
- aliases such as `enter -> return`, `esc -> escape`, and page-key aliases;
- base and nested modal modes;
- global mode-less layers that remain active through dialogs;
- escape to clear a pending sequence;
- backspace to remove the latest pending stroke;
- context-sensitive binding layers;
- formatted shortcut labels from the live registry; and
- slash commands generated only from visible reachable palette commands.

Source: `_ref/opencode-dev/packages/tui/src/keymap.tsx` and
`packages/tui/src/config/keybind.ts`.

## 3. Generic Application Commands

| Command | Default | Harbor mapping |
|---|---|---|
| command palette | `ctrl+p` | core |
| exit | `ctrl+c`, `ctrl+d`, leader+`q` | core, with armed active-task exit |
| theme list | leader+`t` | fidelity |
| theme mode switch/lock | unbound | fidelity |
| status | leader+`s` | core connect/runtime view |
| debug/diagnostics | unbound | fidelity |
| help/docs | unbound | core/fidelity |
| terminal suspend | `ctrl+z` | platform feature |
| terminal title toggle | unbound | preference |
| animations toggle | unbound | accessibility |
| paste-summary toggle | unbound | composer preference |
| permission auto mode | unbound | do not copy without Harbor policy review |

Debug overlay, heap snapshot, internal console, provider organization, and
self-update are product/developer functions rather than first-release Harbor
requirements.

## 4. Generic Session Commands

| Command | Default | Harbor mapping |
|---|---|---|
| new session | leader+`n` | core |
| list/switch session | leader+`l` | core |
| resume closed session | unbound/palette | core; `start`, then await `session.reopened` |
| quick slots 1-9 | leader+`1` through `9` | fidelity, local pins |
| rename | `ctrl+r` | core |
| delete | `ctrl+d` | core with identity confirmation |
| timeline | leader+`g` | fidelity after durable turn IDs |
| export | leader+`x` | core/fidelity |
| copy transcript | unbound | fidelity |
| share/unshare | unbound | deferred, no Harbor primitive |
| fork | unbound | deferred, no Harbor primitive |
| compact/summarize | leader+`c` | Protocol-dependent, not assumed |
| interrupt | `escape` | map to armed cancel |
| timestamps | unbound | preference |
| generic tool output | unbound | renderer preference |
| queued prompts | leader+`q` | client-local queue |
| parent/child navigation | arrows/leader+down | task/session tree when canonical |

OpenCode undo/redo revert conversation state; Harbor has no equivalent
primitive and should not map these keys to a private implementation.

## 5. Transcript Navigation

| Action | Default |
|---|---|
| page up/down | page keys, alternate Ctrl/Alt bindings |
| line up/down | Ctrl/Alt+`y` / `e` |
| quarter-page up/down | Ctrl/Alt+`u` / `d` |
| first/last | Home/End plus Ctrl alternatives |
| next/previous message | unbound |
| last user turn | unbound |
| copy last assistant answer | leader+`y` |
| show/hide reasoning | unbound |
| show/hide action details | unbound |
| show/hide scrollbar | unbound |

Navigation skips synthetic/ignored messages and falls back to viewport movement
when no semantic block boundary exists.

## 6. Generic Prompt Commands

- submit;
- clear;
- multiline newline;
- history previous/next;
- open stash, stash current draft, pop latest draft;
- open skills;
- slash-command completion;
- structured `@` reference completion;
- external editor (**deferred coding reference**);
- workspace selection (**deferred coding reference**).

Textarea editing includes arrow and Emacs-style movement, line/buffer
boundaries, selection variants, word movement/deletion, undo/redo, select-all,
and bracketed paste.

Harbor should retain the editing quality but use artifact/task/session/tool
references instead of treating repository files as the primary `@` domain.

## 7. Dialog And Autocomplete Commands

Reusable dialog interactions:

- previous/next option with arrows and Ctrl+`p`/`n`;
- page up/down;
- home/end;
- submit;
- context-specific actions;
- footer hints generated from active bindings;
- escape/cancel with focus restoration; and
- autocomplete previous/next/hide/select/complete.

MCP toggle, plugin install/toggle, and workspace move actions are advanced or
product-specific.

## 8. Generic Pickers And Overlays

Found concrete dialogs for:

- command palette;
- session list;
- session rename/delete confirmation;
- message timeline and actions;
- export options;
- status;
- diagnostics;
- theme list;
- agent list;
- model and variant list;
- MCP status/toggle;
- provider/authentication;
- skills;
- draft stash;
- retry remediation;
- workspace management; and
- plugin manager.

Harbor first-release equivalents are command, session, task, theme, artifact,
event-filter, Runtime status, and intervention dialogs. Agent/model pickers are
capability-gated until the Protocol supports selection/discovery correctly.
Rich tool status is separately gated by `tool_annotations`.

## 9. Generic Live Data Store

The TUI's synchronized store contains:

- providers and auth methods;
- agents;
- commands;
- permissions and questions by session;
- sessions and statuses;
- server-projected session counters with `counters_partial`;
- scoped retention horizons and aggregate truncation;
- diffs and todos;
- messages and typed parts;
- LSP/formatter/VCS state;
- MCP status and resources;
- configuration and console organization state; and
- experimental capabilities.

For Harbor, the reusable lesson is a single client projection updated from one
event stream plus snapshots. Coding-specific LSP, formatter, VCS, file, and
source-diff data are deferred.

## 10. Generic Tool/Part Presentation

The session UI dispatches ordered parts through a registry and tool calls
through a second registry with a generic fallback. Dedicated OpenCode tool
renderers are:

```text
read, list, glob, grep, webfetch, websearch, task,
bash, edit, write, apply_patch, todowrite, question, skill
```

These names are deferred coding/product reference. Harbor's generic registry
dispatches canonical task, planner, tool, artifact, pause, control, result,
error, and unknown blocks. Tool-name specialization is additive and never
required for safe display.

## 11. Local Persistence

OpenCode keeps TUI interaction state separate from server session state:

- prompt history as bounded JSONL;
- draft stash as bounded JSONL;
- frecency state;
- theme and view preferences;
- terminal-title preference;
- local quick-session slots;
- notification preferences; and
- plugin enablement.

Harbor should keep the same separation and never mirror Runtime entities in a
TUI database.

## 12. Tested Generic Invariants

The captured tests establish or motivate:

- mode-less global bindings remain active through modal modes;
- dialog submit wins over overlapping textarea newline;
- duplicate submit cannot create phantom sessions or lose text;
- prompt history/stash survive corrupt JSONL and remain bounded;
- Unicode display widths drive mention offsets;
- local attachment handling fails safely;
- live hydration cannot overwrite newer events or resurrect deletions;
- entering a session tolerates message-history failure;
- project/workspace events are scoped;
- notifications dedupe unresolved blockers and use transition semantics;
- SIGHUP destroys renderer and cleanup occurs once;
- clipboard/editor failures remain explicit;
- theme registration, fallback, and precedence are deterministic;
- route and slot registration restore previous entries on removal;
- session list remains usable from cached data during failed refresh;
- transcript export handles reasoning, tools, errors, fences, and metadata;
- unknown tools fall back generically;
- narrow-width wrapping and blank-line separation are golden-tested; and
- terminal lifecycle gaps remain where real PTY tests are absent.

See `09-reconciliation-and-lifecycle-edge-cases.md` and
`10-final-saturation-findings.md` for the failure-mode requirements derived
from these tests.

## 13. Deferred Coding-Agent Functionality

Retained only for possible future HarborCode analysis:

- Git/VCS diff viewer and file tree;
- shell/PTY workflows;
- source read/write/edit/patch cards;
- LSP and formatter state;
- worktrees and coding workspaces;
- source file mentions and line ranges;
- code concealment;
- external source editor integration;
- coding-provider/model workflows; and
- code-specific file-context toggles.

None should appear in the generic Harbor TUI roadmap.

An erased-session failure is not ordinary reconnect. The picker should explain
that the conversation was deleted and offer Start Fresh with a newly generated
ID.
