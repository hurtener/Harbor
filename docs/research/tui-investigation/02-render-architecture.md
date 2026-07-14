# 02 — OpenCode TUI architecture overview

> Status: rough first pass. Built up across multiple reads of the on-disk
> snapshot at `_ref/opencode-dev/`. Citations are `path:line` against the
> `dev` branch. This doc is a **map**, not a guide — the architectural
> shape is what Harbor cares about, not the names.

## TL;DR

OpenCode is a **TypeScript-only monorepo** built on Bun. The TUI is built
on **OpenTUI** (a custom TUI engine written in TypeScript with native
binary addons) bound to **Solid.js** for fine-grained reactivity. The TUI
talks to a **long-lived HTTP server** (`opencode serve`) over a typed
SDK; events stream via **SSE**, mutations over **HTTP**. The TUI is a
**thin client** — it stores no agent state itself, it just reconciles
the server's event stream into a Solid store and re-renders. The "good
looking" features that won users — file-tree, panels, tool cards,
permission prompts, autocomplete chips — are **pluggable slots +
registry-driven part/component dispatch**, not monolithic hand-crafted
pages. This is the idea to carry forward to Go/bubbletea.

## 1. Monorepo shape

```text
packages/
  cli/                  — bin entrypoint (effects-based command parser, writer)
  app/                  — Solid/Vite web app
  desktop/              — Electron wrapper
  web/                  — alternate web client
  console/              — the OpenCode.console hosting product
  storybook/            — design system storybook
  core/                 — domain models + Effect services (Database, EventV2,
                          SessionV2, Location, Identity, Credential, etc.)
  schema/               — wire types (Drizzle ORM + Effect Schema)
  protocol/             — HTTP API definitions (Effect HttpApi), wire types
  server/               — server framework (handlers, auth, middleware, routes)
  opencode/             — the OpenCode app: server wiring, agents, CLI,
                          tools, prompts, providers, sessions, sync, ACP
  sdk/                  — generated JS SDK + rudimentary Go/Python SDKs
  sdk-next/             — composition harness for client + core + server
  llm/                  — provider adapters
  plugin/               — plugin authoring SDK
  tui/                  — the TUI itself (this is what we study)
  ui/                   — shared design-system primitives (web-shaped)
  session-ui/           — session/message components reused by web+tui
  identity/             — identity service abstractions
  containers/           — execution environments
  codemode/             — the "codemode" sandboxing layer
  effect-drizzle-sqlite/— sqlite driver + drizzle (CGo-free via better-sqlite3)
  effect-sqlite-node/   — node sqlite adapter
  slack/                — slack integration
  stats/                — stats telemetry
  enterprise/           — multi-tenant org layer
  …
sdks/
  js/                   — generated TS client (the console/client hits this)
```

The build runner is **Turborepo** (`turbo.json`), the package manager is
**Bun** (`packageManager: "bun@1.3.14"`).

Sources read so far:

- `package.json`
- `packages/cli/`
- `packages/opencode/src/{cli,server,session,sync}/`
- `packages/server/`
- `packages/protocol/`
- `packages/tui/src/`
- `packages/session-ui/src/`
- `packages/tui/src/app.tsx` (1134 lines, the TUI root)

Database technology: Drizzle ORM over `@effect/sql-sqlite-bun`. Identity
triple, multi-tenant.

## 2. Foundation library — OpenTUI + Solid

Pinned in root `package.json:54-56`:

```json
"@opentui/core": "0.4.3",
"@opentui/keymap": "0.4.3",
"@opentui/solid": "0.4.3",
```

This is the **whole** TUI layer in three packages:

- `@opentui/core` — a TUI engine with native-backed rendering pieces. Box model,
  scrollbox, textareas, syntax styling, truecolor, alt-screen, OSC
  escape sequences, mouse modes, kitty keyboard protocol, palette
  detection. Implementation: TypeScript shell on top of compiled
  bindings — the actual framebuffer lives in native code with rayon-style
  work-stealing (heuristic from seeing `node-pty`, `pierre/trees`,
  `heap-snapshot-toolkit` patchedDependencies).
- `@opentui/keymap` — multi-mode keymap: command tables, leader keys,
  chained bindings, vim-style text editing layers.
- `@opentui/solid` — Solid.js bindings for `@opentui/core`: each
  renderable is a custom element; reactivity hooks
  (`useRenderer`, `useTerminalDimensions`, ...) live here.

**Why this matters for Harbor.** Bubbletea (Charlie/Catalyst) gives us
the same conceptual pieces — model/program/update/view, keymap registry
(`bubbles/keymap` style), native-terminal primitives — but slowly.
OpenTUI's API is much closer to a Solid/React model than to a TUI loop.
The lesson: a fine-grained reactive runtime, even written in TypeScript,
wins here over a vdom-on-TTY approach like Ink. Go-side, Bubble Tea v2's
cell-diff renderer plus Lip Gloss v2 and selected Bubbles primitives provides
the relevant foundation; Harbor-owned components still implement the visual
grammar. See `06-bubbletea-stack-evaluation.md`.

## 3. Boot sequence

`packages/tui/src/app.tsx`

- Entry: `export const run = Effect.fn("Tui.run")(function* (input: TuiInput))`.
  The TUI's lifecycle is wrapped in an Effect generator.
- Effect acquires/releases the OpenTUI renderer:
  - `createCliRenderer({ targetFps: 60, useKittyKeyboard: {}, useMouse: !Flag.OPENCODE_DISABLE_MOUSE && input.config.mouse, ... })`.
  - On release: `destroyRenderer(renderer)` — we have a SIGCHUP/SIGTSTP
    path for graceful terminal suspend on Unix.
- Pre-mount: it prewarms `renderer.getPalette({ size: 16 })` and
  `waitForThemeMode(1000)` to choose dark/light without first-paint
  flash. (`app.tsx:239-243`).
- Wires a huge nested `Provider` cascade. Roughly 30 layers of Solid
  context for: paths, terminal env, startup, args, keymap, kv, route,
  config, plugin runtime, SDK, permission, project, sync, data, theme,
  local state, prompt stash, dialog, frecency, prompt history, prompt
  ref, editor, location, exit, error boundary, epilogue, clipboard,
  exit. — each `<Provider>` reads downstream values out of upstream.
- `render(() => <App ... />, renderer)` mounts and returns.
- The Effect awaits a `Deferred<unknown>` that's only resolved by
  `renderer.once("destroy", ...)`.

## 4. Process model — TUI is always a client, transport may be embedded

`packages/cli/src/index.ts` (the CLI you `bun src/index.ts`):

```ts
const Handlers = Runtime.handlers(Commands, {
  $: () => import("./commands/handlers/default"),
  api: () => import("./commands/handlers/api"),
  debug: { agents: () => import("./commands/handlers/debug/agents") },
  migrate: () => import("./commands/handlers/migrate"),
  service: { start|restart|status|stop|password: ... },
  serve: () => import("./commands/handlers/serve"),
})
```

The CLI's **default** handler launches the TUI. **serve** spins up the
HTTP API without a UI. **service** is a launchd/systemd shim that runs
the background server.

The current implementation has two full-TUI transport modes. With no
explicit network options, the CLI starts a worker and gives the SDK a
synthetic `http://opencode.internal` URL: fetches call the server's web
handler through RPC and events are forwarded from the worker bus. With
an explicit hostname, port, or mDNS, the worker opens a real listener
and the same SDK uses HTTP/SSE. `attach <url>` uses the network path
without a local worker. The invariant is therefore **Protocol-client
semantics**, not a mandatory separate OS process.

`packages/opencode/src/server/server.ts` defines `listen(opts)`. The
server is an Hono (via `effect/unstable/http`) router over the
Protocol's `HttpApi`. SSE is provided by `WebSocketTracker` and the
effect-event-streams over HTTP/1.1 chunked responses.

TUI→server transport (from `packages/tui/src/context/sdk.tsx:91-105`):

```ts
const events = await sdk.global.event({
  signal: ctrl.signal,
  sseMaxRetryAttempts: 0,
})
for await (const event of events.stream) {
  if (ctrl.signal.aborted) break
  handleEvent(event)
}
// exponential backoff retry up to 30s
```

This is a **single shared SSE connection** per TUI process. All events
fan out through one queue, are batched within 16ms windows, then
dispatched in one Solid `batch()` (`sdk.tsx:54-66`):

```ts
const handleEvent = (event) => {
  queue.push(event)
  if (timer) return
  if (Date.now() - last < 16) {
    timer = setTimeout(flush, 16)
    return
  }
  flush()
}
```

This is the **key trick**: rather than repaint-per-event, batch every
~one-frame's worth.

Mutable state lives **only** in Solid stores (`createStore` +
`produce`/`reconcile`). The TUI has strict compile-time, runtime, and
test gates against accidental per-call mutable fields on compiled
artifacts (D-025 analogue).

## 5. Render primitive

Layout is a **flex-box model** in OpenTUI. Excerpt from
`packages/tui/src/routes/session/index.tsx:1168-1183`:

```tsx
<scrollbox
  ref={(r) => (scroll = r)}
  verticalScrollbarOptions={{ visible: showScrollbar(),
    trackOptions: { backgroundColor: theme.backgroundElement, foregroundColor: theme.border } }}
  stickyScroll={true}
  stickyStart="bottom"
  flexGrow={1}
  scrollAcceleration={scrollAcceleration()}
>
```

Each node is a `<box>` or `<text>` or `<scrollbox>` JSX element — a
`BoxRenderable` / `TextRenderable` underneath. Children of `<For>` and
`<Show>` are reactive; mutations to upstream Solid stores
reconcile-on-the-fly. **No vdom**: Solid ships a fine-grained reactivity
graph → renderer listens.

Text inside `<text>` is rendered via Shiki-stream-aware AST nodes;
you can nest `<span style={{fg, bg, bold, italic}}>` and `<b>` for
inline markdown. Code blocks use `@opentui/core`'s built-in
`SyntaxStyle` (which is fed tokens from `marked` + `marked-shiki`,
one of whose deps is `shiki@4.2.0`).

Diffing: not a vdom, but a **box-tree diff** — OpenTUI's renderer walks
the tree, computes a minimal dirty-set, and writes only changing
regions. The diff library (`@pierre/diffs` 1.2.10) is used for
*content-side* diffs (file changes), not for layout diffs.

Frame pacing: `targetFps: 60` — frames are gated on a 60Hz tick. All
Solid effects in one tick flush in one draw.

## 6. The TUI root layout

From `app.tsx:1087-1133`:

```tsx
<box width=… height=… flexDirection="column" backgroundColor={theme.background}>
  <Show when={ready()}>
    <box flexGrow={1} flexDirection="column">
      <Switch>
        <Match when={route.data.type === "home"}><Home /></Match>
        <Match when={route.data.type === "session"}>
          <Show when={…sessionID…} keyed>{(id) => <Session />}</Show>
        </Match>
      </Switch>
      {plugin()}                              // <-- plugin route pages
    </box>
    <box flexShrink={0}>
      <pluginRuntime.Slot name="app_bottom" />
    </box>
    <pluginRuntime.Slot name="app" />
  </Show>
  <Show when={!startup.skipInitialLoading}><StartupLoading /></Show>
</box>
```

There are **two routes**: `home` and `session`. Everything else —
model picker, theme picker, command palette, dialogs — is a
"floating overlay" (a `<Dialog>`) on top of the route.

The sidebar (`packages/tui/src/routes/session/sidebar.tsx`, 103 lines)
is **almost empty** in core — it's three layout slots:

```tsx
<pluginRuntime.Slot name="sidebar_title" mode="single_winner"
                    session_id={…}
                    title={…} share_url={…}>
  <box>…title…<WorkspaceLabel/></box>
</pluginRuntime.Slot>
<pluginRuntime.Slot name="sidebar_content" session_id={…} />
<pluginRuntime.Slot name="sidebar_footer" mode="single_winner" session_id={…}>
  <text>…version…</text>
</pluginRuntime.Slot>
```

The "side panels" we see advertised by OpenCode are **bundled plugins**
filling these slots. That's the keystone architectural finding:
**OpenCode's TUI is intentionally an empty shell that plugins
flesh out**. (See `03-parts-tools-plugins.md` for the slot catalogue.)

## 7. Streaming and tool rendering

The session view is a `<scrollbox>` containing a `<For each={messages()}>`
where each `message` is either:

- `UserMessage`: text + optional `File` chips (`bg=theme.secondary`
  badge with "File"/"Directory" label and the path).
- `AssistantMessage`: a nested `<For each={parts()}>` where each part
  dispatches via `PART_MAPPING[part.type]` (`message-part.tsx:1482`).
  Known part types: `tool`, `reasoning`, `agent`, `text`, `compaction`,
  plus likely `step-start`, `step-finish`, `snapshot`, etc.

Each tool part has its own state machine (running/completed/error) and
renders via `ToolRegistry.render(part.tool) ?? GenericTool`
(`message-part.tsx:1549`). This is **pluggable per tool** — core ships
GenericTool + a few out-of-the-box dedicated renderers; plugins can
add more.

Spinners: `opentui-spinner@0.0.7` — appears in the per-tool header bar,
and a top-level streaming cursor as part of the active assistant message.

Reasoning parts: rendered as a separate, *visually de-emphasized* block
(`thinkingOpacity` in the theme tokens). User can collapse/expand
(`session.toggle.thinking`).

Diff rendering for file edits: `@pierre/diffs` over
`session_diff[sessionID]`. Each chat message has a file-changes panel
you can drill into.

## 8. Aside — what NOT to copy verbatim

OpenCode's TUI is **massively ergonomic** but tied to:

- a Solid.js fine-grained reactivity layer, and
- a native-binary-backed TUI runtime (OpenTUI).

We are constrained to **pure Go + bubbletea + tview/lipgloss** for
Harbor (no CGo, per Harbor AGENTS §5). Some adaptations:

| OpenCode concept | Bubbletea analogue |
|---|---|
| Solid `createStore` + `reconcile` | bubbletea `Model` + `tea.Cmd` events + lipgloss style cache |
| Solid `<box>`/`scrollbox` flex layout | Harbor-owned layout over Lip Gloss layers and selected Bubbles viewports |
| `@opentui/core` renderer | Bubble Tea v2 cell-buffer renderer |
| `@opentui/keymap` mode stack + leader | `bubbles/key` + our own mode stack (`autocomplete`, `dialog`, `editor`) |
| `PluginRuntime.Slot` (replace/single/multiple winners) | a Go `Slot[T]` registry + our own dispatcher |
| Single SSE → batched Solid updates | a single SSE/WS connection → a Go channel → `tea.Msg`-based async |
| Themed `<text>` with `<span style={fg, bg, bold}>` | `lipgloss.NewStyle().Foreground(...).Background(...).Bold(true)` |
| 60fps `targetFps` | bubbletea's natural 60Hz tick + `lipgloss` rendering |

The **core pattern** we want to preserve is the **registry-of-part-
renderer + empty-shell-with-plugin-slots** design. For the current
Harbor TUI, the registered parts describe generic agent activity:
answer text, reasoning, task lifecycle, planner decisions, tool
lifecycle, artifacts, pauses, controls, and errors. Coding-only parts
such as source diffs and shell output are deferred references, not
first-release requirements.

## 9. Open follow-ups

- Does the TUI have any websocket path or is it SSE-only?
- How does the `service` subcommand (background server) detect the TUI
  and pass the URL/credentials?
- The `@opentui/core` source itself isn't in this repo — it's a
  third-party dep — so we don't see the actual TUI primitives. Picking
  bubbletea will need a museum-tour of its primitives.
- The `session-ui` package is shared with the web app — what does the
  Console reference (our future Harbor Console is SvelteKit + our own
  styled primitives). Should we build the TUI and the Web on a
  *shared* part/component registry, or accept duplication?
