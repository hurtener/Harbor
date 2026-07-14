# 03 — OpenCode TUI part catalogue, tools registry, plugin slots

> Author: hand-written from a careful read of `packages/session-ui/`,
> `packages/tui/src/component/`, and `packages/tui/src/plugin/` on disk
> (`_ref/opencode-dev/`).

## 1. Message part catalogue (`PART_MAPPING`)

`packages/session-ui/src/components/message-part.tsx`

The runtime defines a fixed taxonomy of "parts" — sub-records hanging
off an assistant message (a part can be a tool call, a chunk of text,
reasoning, etc.). Each `part.type` maps to a renderer. Built-ins found
in this file:

| `part.type`   | Renderer                            | Visual purpose                            |
|---------------|-------------------------------------|-------------------------------------------|
| `"tool"`      | `ToolPartDisplay`                   | Routes by tool name (see §2)              |
| `"text"`      | `TextPartDisplay`                   | Streamed/markdown text                    |
| `"reasoning"` | `ReasoningPartDisplay`              | Collapsible thinking block                |
| `"compaction"` | `CompactionPartDisplay`            | A "@@@ Compaction @@@" divider            |

Plus two utility types I extrapolated (the canonical part shape
includes these per `packages/schema/src/session-message.ts`):

| Type         | Purpose                                              |
|--------------|------------------------------------------------------|
| `file`       | File attachment shown as a `<FileMedia>` chip        |
| `agent`      | Subagent invocation card                             |
| `step-start` | A new turn-counter (borders the below)               |
| `step-finish`| Last part of a turn — used for frame timing + footer |
| `snapshot`   | A "compacted" session resume marker                  |

The registry itself is a mutable record on a module-local `state`
object (`message-part.tsx:1465-1479`):

```ts
const state: Record<…> = {}
export function registerTool(input: { name: string; render?: ToolComponent }) { … }
// or
PART_MAPPING[type] = component
```

A plugin (or a host call) can register a part renderer *or* a tool
renderer. The TUI host's `<pluginRuntime.Slot>` layer can also fill
named DOM regions.

## 2. ToolRenderer registry (the big hitter)

`packages/session-ui/src/components/message-part.tsx:1753-2623`

14 tools ship with dedicated renderers. Each registers via
`ToolRegistry.register({ name, render })` and is dispatched by
`render = ToolRegistry.render(part().tool) ?? GenericTool`. So:

| Tool name        | Notes (file:line)                                       |
|------------------|---------------------------------------------------------|
| `"read"`         | shows file path, line range, content                   |
| `"list"`         | directory listing                                       |
| `"glob"`         | matching files                                          |
| `"grep"`         | matched lines + content                                 |
| `"webfetch"`     | URL + HTML/markdown body                                |
| `"websearch"`    | provider label + query                                  |
| `"task"`         | subagent session id + description                       |
| `"bash"`         | command + elapsed + output stream (long, 200+ lines)    |
| `"edit"`         | BEFORE/AFTER diff (via `@pierre/diffs`)                 |
| `"write"`        | creates file, shows written content                     |
| `"apply_patch"`  | uses `apply-patch-file.tsx`; a unified patch UX         |
| `"todowrite"`    | renders the latest todo list inline (kind of a nav)     |
| `"question"`     | masked during pending/running (so the QuestionPrompt UI underneath drives the flow) |
| `"skill"`        | skill name + list of sub-output files                   |

Unknown tools fall back to `GenericTool` from `basic-tool.tsx` — that
file is a small framework: `icon + trigger title + collapsible body`
with mount-deferral so off-screen tools don't render.

`ToolStatusTitle` / `ToolCountSummary` / `ToolErrorCard` are
sub-components used inside many of these for shared structure (the
"running spinner", "X/Y done" footer, etc.).

## 3. Built-in tool taxonomy on the runtime side

Mirrors the renderers, missing only `apply_patch` (vs `write` + `edit`).
These names are coding-domain reference material only. The generic Harbor TUI
must dispatch Harbor's canonical tool lifecycle and tool manifest shapes,
regardless of tool name. Dedicated coding-tool renderers are deferred to any
future HarborCode work.

## 4. Plugin slot catalogue (`pluginRuntime.Slot name="..."`)

Found by grepping `packages/tui/src/**/*.tsx` for `pluginRuntime.Slot`.
Sorted + deduped:

| Slot name               | Where it's mounted                                | Mode             |
|-------------------------|---------------------------------------------------|------------------|
| `app_bottom`            | bottom of the **whole TUI**, under the route box  | (default)        |
| `app`                   | bottom-middle overlay (full-screen)               | (default)        |
| `home_logo`             | home — replaces the OpenCode logo                 | `replace`        |
| `home_prompt`           | home — replaces the input prompt                  | `replace`        |
| `home_prompt_right`     | home — the trailing UI to the right of the prompt | (default)        |
| `home_bottom`           | home — under the prompt                           | (default)        |
| `home_footer`           | home — footer band                                | `single_winner`  |
| `session_prompt_right`  | session view — right of the prompt                | (no mode)        |
| `sidebar_content`       | session sidebar — body                            | (no mode)        |
| `sidebar_footer`        | session sidebar — footer                          | `single_winner`  |

**Important for Harbor**: the sidebar in OpenCode is `title` (single
winner) + `content` (any) + `footer` (single winner) — three slots,
nothing else. The "rich sidebar with context, tools, diffs, etc." is
**plugins adding their own blocks.**

## 5. Slot dispatch semantics (`slots.tsx`)

`createSolidSlotRegistry` lives in `@opentui/solid`. The high-level view
in OpenCode's wrapper:

- `mode="replace"` — the **outer** element passed as a child is
  replaced wholesale by the plugin's contribution. E.g. `home_logo`
  lets a plugin redraw the logo; `home_prompt` lets a plugin build
  their own prompt component.
- `mode="single_winner"` — like LastWriteWins: each plugin's
  contribution scores itself via a heuristic; the best wins.
- (default / no mode) — append, the plugin owns its own slot region.

Plugins throw errors that go through `onPluginError` and are emitted
to the TUI toast/error overlay (`slots.tsx:37-46`). So plugin bugs
don't crash the host.

## 6. Routes

`packages/tui/src/plugin/api.ts:11-37` — a Map keyed by `route.id`
holding an array of renderers. `routes.get(name)` returns the
**latest-registered** renderer. So routes have the same
LastWriteWins semantics as slots.

In `app.tsx:1080-1085` the host routes plug-in sub-routes:

```tsx
const plugin = createMemo(() => {
  if (!ready()) return
  if (route.data.type !== "plugin") return
  const render = pluginRuntime.routes.get(route.data.id)
  if (!render) return <PluginRouteMissing id={route.data.id} … />
  return render({ params: route.data.data })
})
```

So `route.type = "plugin"` directly resolves into a plugin full-screen
view.

## 7. The TUI plugin API surface

`packages/tui/src/plugin/api.ts:42-52` defines `createTuiApi`. The
constructed `TuiPluginApi` exposes (from `app.tsx:388-406`):

- `version` (build version)
- `tuiConfig` (resolved keybinds + preferences)
- `dialog` (show / push / pop confirm + alert + select)
- `keymap` (dispatch commands, intercept keys, leader state)
- `kv` (key-value store)
- `route` (navigate + current route)
- `routes` (the host's plugin route registry)
- `event` (event subscription)
- `sdk` (the OpenCode client)
- `sync` (the sync store)
- `theme` (active theme + dark/light)
- `toast` (toast API)
- `renderer` (escape hatches)
- `attention` (notification attention bitmap)
- `Slot` (the plugin slot registry — wait this looks self-referential…)

`lifecycle.signal` + `lifecycle.onDispose()`. Plugins receive **all
the same primitives** the host has, scoped by their `pluginId`.

## 8. Frecency, History, Stash

`packages/tui/src/prompt/` is the small set of side-channel stores
that the prompt consults. They live in XDG state dir as NDJSON, capped
at 50 entries, atomic rewrite on trim.

- **frecency.tsx** — for `@`-mention candidates: ranks file/agent
  options by `(timestamp + uses)`. Uses a "frequency × recency"
  formula similar to Mozilla's. Files touched recently and often
  rank above older ones.
- **history.tsx** — every submitted `PromptInfo` (input + parts) is
  appended. `history.move(direction, input)` rewinds / fast-forwards
  through past inputs (up/down arrows in the prompt).
- **stash.tsx** — when the operator invokes the unbound-by-default
  `prompt.stash` command, push the current draft. `pop`/`remove`
  retrieve/manage. The `DialogStash` UI lists them.

These three are **independent** of the server's session history —
they're TUI-side memories about the user's *interaction* with the
prompt, not the conversation.

## 9. Local-attachment / edit / paste pipeline

These three smaller files (`local-attachment.ts`, `move.tsx`,
`workspace.tsx`) handle the prompt's *local-only* ergonomics:

- `local-attachment` (48 lines) — on paste, look up the path: if it
  points to a file under the workspace, auto-attach as a `File` part
  with extmark. otherwise it's just text.
- `move` — `usePromptMove` handles vim-style cursor movement with
  word/buffer/line endpoints + selection.
- `workspace` — gives the prompt context about the current workspace
  (worktree path), used to filter file `@`-mentions.

Together these are **not part of the AssistantMessage structure** —
they live entirely in TUI-side state.

## 10. Autocomplete (the @ and / UX)

`packages/tui/src/component/prompt/autocomplete.tsx`, 781 lines.
Reads:

- anchors the autoc popover to a sibling `anchor()` `BoxRenderable`
  and computes its position via `setInterval(50ms)` polling of
  `anchor.x / .y / .width` — because **the textarea cursor moves
  reactively**, and anchoring on each render would be noisy.
- `visible: false | "@" | "/"` — only two trigger chars.
- The popup pulls candidates from:
  - `useCommandSlashes()` → `keymap.getCommandEntries({ visibility: "reachable", namespace: "palette" })`.
  - For `@`: pulls all files (`sdk.client.find.files`), agents, and
    extension / configuration files via `sdk.client.find.text` etc.
  - Renders matched files highlighted via `fuzzysort`.
- On select: inserts an `extmark` (virtual text region in the
  textarea) — the `@foo.ts` text is *displayed* but is logically a
  `file` `Part`. The textarea shows two typefaces on either side.

The popover is a separate React-like component overlaid by
`zIndex=1000` relative to the prompt box.

## 11. Dialog primitives (the modal layer)

`packages/tui/src/ui/dialog.tsx` is the base. `packages/tui/src/ui/dialog-*.tsx` and `packages/tui/src/component/dialog-*.tsx` are the actual dialog compositions. Categorised:

| Category     | Files                                                                                       |
|--------------|----------------------------------------------------------------------------------------------|
| **Confirm**  | `dialog-confirm.tsx` — yes/no/always with auto-approve toggle                              |
| **Alert**    | `dialog-alert.tsx` — single OK informational                                              |
| **Select**   | `dialog-select.tsx` (790 lines) — the BIG one: fuzzy filter, keys, footer hints, actions, groupBy |
| **Prompt**   | `dialog-prompt.tsx` — text input with validation                                          |
| **Help**     | `dialog-help.tsx` — rendered help screen                                                    |
| **Export**   | `dialog-export-options.tsx` — choose Markdown export options                              |

The `Dialog` base component handles:

- sizes: `medium` (60w), `large` (88w), `xlarge` (116w).
- mouse-down inside the dialog dismisses on click-out.
- mode-stack push: opening a dialog pushes `"dialog"` mode in the
  OpenTUI keymap so unbound keys focus the dialog.
- hint footer with formatted keybindings (e.g. `↵ select`, `Esc cancel`).
- a tiny custom dialog manager that supports **stacks** (`useDialog().stack`).

The 30+ `dialog-{model,agent,mcp,theme,…}.tsx` files in `component/`
are concrete compositions: each one wraps `DialogSelect` /
`DialogConfirm` over a specific data source (model list, agent list,
theme list).

## 12. Status / Debug / Help dialogs

`DialogStatus` shows project + version + storage backend + MCP
connection summary + LSP status. `DialogDebug` shows extensions, key
config, server URL, process info. Both are "opener" dialogs from
command palette.

## 13. Summary of architectural insights

1. **Tool-registry-driven renderer**: any new tool type just calls
   `ToolRegistry.register({ name, render })`. The TUI doesn't bake in
   per-tool UI; that keeps unknown tools first-class.
2. **Slot-based page composition**: a side panel "works" because
   `<pluginRuntime.Slot>` lets plugins stack layered content.
3. **Sub-elements as Partitions**: every assistant message is a list
   of typed parts. The renderer dispatches per part. Same for session
   components on the web side.
4. **Prompt has its own memory** (history, frecency, stash) outside
   the server's session history. Same pattern as browsers storing
   form-data separate from page history.
5. **Dialog stack is the modal model**, not separate pages. A keymap
   mode stack keeps dialog/non-dialog input routing cleanly.
6. **Subdialogs over background work**: while you're typing, the
   server is streaming — the prompt never blocks. Streaming is a
   side-channel that updates a separate part.

## 14. What The Generic Harbor TUI Should Keep

- A registry-driven `PartRenderer` with a `GenericPart` fallback.
- A registry-driven `ToolRenderer` with a `GenericTool` fallback.
- A `Slot[K, V]` registry for plugin page composition.
- A `Dialog` stack + mode-stack model for modals.
- Local / remote separation of memory: prompt-history vs.
  session-history.
- Splitting prompt UX into an independent model so the same Protocol behavior
  can be tested against the Console and TUI without sharing rendering code.

## 15. What The Generic Harbor TUI Should Not Copy

- OpenTUI's native-binary TUI primitive (we're Go + bubbletea).
- The domain preload + deferred-mount dance in `basic-tool.tsx` (the
  off-screen deferral is solid, not the implementation).
- The frecency heuristic — replace with a simpler LRU if we don't
  have data.
- OpenCode's specific slot names. Harbor slots should match sessions, runs,
  tasks, interventions, tools, artifacts, events, and runtime posture.
- Coding-specific file, patch, shell, worktree, LSP, and source-editor
  functionality. It is outside this product even when OpenCode implements it
  elegantly.
