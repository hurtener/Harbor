---
name: drive-the-playground
description: "Use the Console's Playground page to chat against your agent, attach files (images / PDFs / audio), and steer or queue input during a run. Use when validating the agent end-to-end interactively, the foreground/background task posture, image-vs-PDF MIME dispatch, and the playground's steering UI."
license: Apache-2.0
metadata:
  framework: harbor
  surface: playground
  verbs: ""
---

# Drive the Playground

The Playground is the Console page where you chat against your live agent — same identity triple, same task surface, same events stream as production. It's the round-trip validation gate: if your agent works in the Playground against a real LLM, it works for end users. This skill covers the chat input, file uploads, the multimodal MIME dispatch (Path 1 inline vs Path 2 ArtifactStub), foreground vs background tasks, and the "steer or queue" posture when input lands during a run.

## 1. Boot to chat

Prerequisites:

- `harbor dev` running with a real LLM provider configured (see [`wire-the-llm-provider`](../wire-the-llm-provider/SKILL.md)).
- `harbor console` running and attached (see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md)).
- The Console's connection footer says "Connected <http://127.0.0.1:18080>" — token freshly seeded.

Navigate to **Playground** in the Console nav. The page shows:

- **Header row** — agent display name, a status pill (`Active` / `Ready` / `Paused` / `Failed`), a planner-type pill, a token-count chip, and a cost chip. Cancel-run and Restart buttons sit on the right.
- **KPI strip** — four tiles under the header: Tokens (with a mini sparkline), Cost (with ceiling-percent label), p50 latency, and Status. The cost tile turns warning-coloured when you are ≥80% of the ceiling.
- **Chat history** — assistant + user turns. Assistant bubbles render markdown (bold, italic, inline code, lists) and show an avatar + timestamp + planner-phase label.
- **Bottom status bar** — streaming state (`Idle` / `Streaming`), Protocol version, Events Stream live indicator, and Console build version.
- **Input box** at the bottom — type + Enter to send, or Cmd/Ctrl-Enter.
- **File-upload chip** (paperclip icon) — drag-and-drop or click to attach.

Type a message, hit Enter. The Runtime mints a Task, dispatches it through the planner, and streams events back. You see the assistant response token-by-token as Bifrost streams from the provider.

## 2. File uploads — multimodal dispatch

Click the paperclip or drag a file into the chat. The Playground POSTs to the Runtime's `artifacts.put` endpoint, gets back an `ArtifactID`, and includes it in the next `StartRequest` via `InputArtifactIDs` (D-166).

The runtime then dispatches based on MIME:

- **`image/*`** — Path 1 INLINE. The image is base64-encoded into a `DataURL` and passed in the LLM call as a multimodal content block. The LLM sees the pixels directly. Works for any LLM provider that speaks multimodal vision (Claude, GPT-4o, Gemini, Llama 3.2 Vision).
- **`application/pdf`** — Path 2 ARTIFACT STUB. The planner sees `{ "ref": "art-abc123", "mime": "application/pdf", "size": 142853, "filename": "report.pdf" }` and can decide what to do (e.g. call a `pdf.extract_text` tool to pull pages out). The bytes never inline into the LLM context window.
- **`audio/*`** — Path 2 ARTIFACT STUB. Same as PDF — the planner sees a stub and routes to a transcription tool if you have one.
- **Other MIMEs** — Path 2 ARTIFACT STUB. Conservative default.

Per-MIME tool dispatch is controlled by `Tool.HandlesMIME(mime) bool` in the tool's spec. A tool that opts in to a MIME gets first-call rights when that MIME shows up in `InputArtifactIDs`.

### Per-attachment disposition override (Phase 84b — D-189)

The dispatch above is the **runtime default**, not a hardcode. Each attachment chip in the composer carries a small disposition selector:

- **Auto** (default) — send no hint; the agent's `multimodal.disposition` config map or the runtime default above decides.
- **Reference (tool fetch)** — force `ref`: the model gets an ArtifactStub + a `Fetch.Tool` pointer even for an image.
- **Inline** — force the DataURL inline path (`image/*` only at V1.1; other MIMEs degrade to ref with a logged notice).
- **Provider-native** — opt in to the provider's own vision/audio/video/document understanding (Phase 84c — D-190): the LLM driver uploads the attachment to the provider's file surface and the model sees the real content via an opaque `file_id`. The upload is observable on the `llm.provider_file.uploaded` event; a provider without support for the attachment's modality keeps the ArtifactStub reference with a logged notice — never silent.

The pick rides the `start` request as `input_artifact_dispositions` and outranks the agent's config map. Forcing a specific tool (`tool:<name>`) is available via the Protocol field directly (see [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md)).

### Limits

- **Max upload size**: governed by `protocol.max_request_bytes` (the `artifacts.put` upload body bound; default 4 MiB). A body above this fails with `CodeRequestTooLarge` / HTTP 413 rather than silently truncating.
- **Path 1 inline cap**: ~20MB of image data per LLM call (the provider's actual limit varies). Larger images get downscaled by the Console before upload — the original lives in the artifact store; the LLM sees the downscaled version inline.

## 3. Foreground vs background tasks

The Playground's chat input drives FOREGROUND tasks — synchronous, the chat panel waits for completion. The Tasks page (in nav) drives BACKGROUND tasks — fire-and-forget, the agent works while you go elsewhere.

For a chat agent, foreground is what you want — you're in conversation. But the planner CAN spawn background tasks mid-run (e.g. "I'll fetch the data in the background while we keep talking"). Those show up in the Tasks page; the foreground chat reflects them with a small "background task spawned" event in the chat history.

As of Phase 107e the dev runtime actually RUNS those spawned background tasks (each gets its own planner sub-run) and the agent can join one to read its result. A background sub-task can itself spawn further sub-tasks; `planner.absolute_max_spawn_depth` (default 4) caps how deeply that nests, so a runaway agent can't recurse without bound — a spawn past the cap surfaces as an error the planner re-plans against. A model can also batch several spawns alongside ordinary tool calls in ONE response; `planner.max_batch_spawns` (default 5) caps how many spawns one such batch may carry — the breadth ceiling, distinct from the depth cap — and a batch over the ceiling is rejected whole (nothing dispatches) rather than truncated.

The agent can also **observe and cancel** the background tasks its own run spawned, mid-run, without waiting for them: `_task_status` reports the state of the tasks this run spawned (each `{task_id, status, description, group_id}`; omit the ids to list the whole subtree), and `_cancel_task` cancels one of them by id. Both are descendant-scoped — a run sees and cancels ONLY the tasks it spawned itself, directly or transitively, never another run's tasks even in the same session (an out-of-scope id is rejected loud, not silently ignored). This is how a model that fanned out several explorations and got its answer from the first cancels the losing branches. Both are sent on their own (never batched with other calls in this wave).

Beyond observe-and-cancel, the agent can **steer, pause, and resume** those same spawned tasks mid-run: `_steer_task` sends a free-text `directive` to one of its spawned tasks (the task sees it at its next step, through the same steering inbox an operator's steering uses), `_pause_task` parks one of them, and `_resume_task` releases a paused one (optionally injecting a `directive` as it continues). All three are descendant-scoped exactly like `_task_status` / `_cancel_task` — a run steers/pauses/resumes ONLY the tasks it spawned, never a sibling run's — and all are sent on their own (never batched). Pausing a spawned task never pauses your own turn; only the named task parks. And the operator always wins: from the Tasks page (or the Protocol control surface) an operator can steer, pause, or resume ANY task and overrides the agent — a task the agent paused can be resumed by the operator, and a task the agent cannot reach the operator still can. This completes the agent side of the same control taxonomy the operator's steering UI (below) drives.

A spawn can be marked to **survive the spawning agent's own cancellation**: `_spawn_task` accepts `propagate_on_cancel: "isolate"` (default is `"cascade"`, where cancelling a parent sweeps the task too). An isolate task detaches from the agent's cascade — cancelling the task that spawned it does NOT cancel it — but it is NEVER detached from a direct cancel: the operator (via the Tasks page / the Protocol `cancel`) reaches any task at any time, and a session-scoped operator cancel sweeps isolate tasks too. There is no uncancellable task; isolate only changes whose cascade it rides, not whether the operator can stop it.

### Steer vs queue — input during a running foreground task

When a foreground task is running and you type into the chat input, you get a CHOICE:

- **Steer** — interrupt the current run, redirect with your new input. The current run gets a `RequestPause` event with reason `user_steer`; the planner picks up the new input from its next turn.
- **Queue** — let the current run finish, then your input goes as the next user turn.

While a run is active the composer swaps its Send affordance for a single Queue/Steer mode dropdown next to the send arrow; **Queue is the default**. Pick Steer from the dropdown when you want to interrupt rather than wait — the choice matters because steering mid-tool-call has different semantics than queuing. Steering during a tool call cancels the tool call's `ctx` and the planner sees the cancellation; queuing waits for the tool to finish.

The unified pause/resume primitive (RFC §6.10) is what makes this work — `RequestPause` is the same mechanism used for HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`. Steering is just one more reason code.

## 4. Reading the chat for debugging signals

The chat history surfaces several event types inline:

- **Assistant text** — the streamed LLM response. Starts from byte 0: Phase 107c moved the React planner onto native provider tool-calling, so `Content` deltas are the user-facing prose by structural construction (no JSON wrapper / no `{tool, args}` envelope buffering — the LLM no longer emits one). Chunks flow straight from bifrost's `OnContent` callback through to the Console with no extractor in the middle.
- **Tool calls** — collapsed by default; click to expand the args/result panel. Tool calls arrive on their own structured channel (`resp.ToolCalls []ToolCallStructured`) and are rendered as cards rather than inlined into the prose stream. **The agent can call several tools at once in a single turn (Phase 107d):** when it does, the runtime dispatches them concurrently and you'll see multiple tool-call cards for the same assistant turn, each with its own result. Concurrent dispatch is on by default; set `planner.parallel_tool_calls: false` in `harbor.yaml` to make the runtime run them one per step instead.
- **Thoughts/reasoning** — click the "Reasoning (N steps)" toggle on any agent bubble to see the model's intermediate thinking trace from the planner trajectory. The accordion shows the per-step reasoning the model produced; collapsed by default, one click expands it. Phase 107a. The ordered accordion also survives leaving and reopening a session — the reopened bubble reconstructs the same per-step reasoning (interleaved with the tool calls each step preceded) from the durable event stream, rendering identically to the live view (Phase 165).
- **Pause events** — yellow inline cards with reason ("oauth_required", "approval_pending", "user_steer", etc.). The card has a Resume button when applicable.
- **MCP App widgets** — when an MCP server's tool declares an interactive UI (a `ui://` resource bound to the tool via its `_meta.ui.resourceUri`), the Console renders it **inline** as a sandboxed-iframe widget in the chat scroll (Phase 109b). The discovery is automatic: the runtime emits an `mcp.app_available` event when the agent invokes a tool that declares the app (Phase 109d; the runtime reads the tool's own definition, so it fires against real ext-apps servers — Phase 109e), the Playground attaches it to that turn's assistant bubble, and the widget mounts — no extra wiring on your side. The app runs under a strict sandbox (no parent-DOM / cookie / `localStorage` access) and a strict CSP; it cannot open its own network connection. Any tool call the app makes is proxied back through the Harbor Runtime under your identity triple and hits the SAME approval / OAuth gates a planner-initiated call does (D-173) — an app call to a gated tool parks on the same pause card above. **Apps render on whichever artifact store you configured** (in-memory, filesystem, SQLite, Postgres — not just S3): the runtime serves an ordinary-sized app's HTML document inline up to a 2 MiB cap (Phase 109g), so there's no presigned-URL dependency. A pathologically large app document (over 2 MiB) is offloaded to the artifact store by reference (D-026) and the Console fetches those bytes into the same sandboxed frame (Phase 109f); the document never inlines through the LLM context.
- **MCP App DisplayMode — `inline` / `fullscreen` / `pip` (Phase 109c).** An MCP App can declare (or request at runtime) a larger layout than the inline chat widget, and the Playground honours it without reloading your session:
  - **`fullscreen`** — the app takes over the chat + composer region; a **tab strip** appears so you can flip between **Chat** and the app (and between several apps — each fullscreen app is its own tab). Close an app's tab (the **×**) to drop it and return focus to Chat.
  - **`pip`** — a resizable **50/50 split** with chat on the left and the app on the right. The right detail rail is **hidden by default**; a **Hide rail / Show rail** toggle reopens it (reopening never resets your drag position). Drag the divider to re-balance the split; the ratio is clamped to sane bounds.
  - Each App panel header carries **Inline / PiP / Fullscreen** switch buttons plus **Close**, so you can drive the layout yourself; the app can also request a mode change itself. Closing or tearing down the app returns the page to the default chat + rail layout. `pip` is one app beside chat — it is **not** a two-agent comparison view.
  - **Pop an inline app out yourself (Phase 109f).** You don't have to wait for the app to ask: the inline widget carries an **expand ⤢** button (and a fullscreen button) in its top-right corner. Click it to pop the app to the **side-by-side (`pip`)** split (or fullscreen) on the spot — same layout, no session reload. Close the app to return to the inline chat scroll.
  - **A rendered app survives leaving and reopening the session (Phase 204).** Reopen the conversation and the widget comes back on the same turn it was discovered on, in the same display mode, with the same data — the Console replays the `mcp.app_available` event from the durable stream and re-reads the tool call's captured input/result behind it. You don't re-run anything. If that captured data is no longer available (an unknown or expired record, or a runtime that restarted with the in-memory state driver), the turn shows a plain **"This view is no longer available"** note instead of the widget — never a blank bubble and never an app frame that loads but stays empty. The rest of the turn (its text, tool cards, and stats) is unaffected.
  - **The app adapts to your Console theme, and renders with real data — no extra wiring.** When the widget mounts, the host hands the app your light/dark preference (from the OS `prefers-color-scheme`) plus the Console's structural design tokens, so a spec-conformant app themes itself to match instead of booting with a fixed palette; flip your OS light/dark mid-session and the rendered app re-themes live (it is never reloaded — the handshake stays intact). The host also **delivers the originating tool call's input and result into the app after it initializes**, so the app renders its real content (the numbers/rows the tool returned) rather than an empty shell — it reads that pushed data and never re-invokes the tool. Both are automatic: a real Dockyard App (e.g. `analytics-widgets`) renders themed, with its data, on first mount.
  - **The widget grows to fit the app's content (Phase 207).** A spec-conformant app reports its own rendered height and the inline widget follows it, so a content-heavy app no longer sits in a fixed box with its own scrollbar nested inside the chat scroll. The growth is bounded by the Console, not by the app: it never shrinks below the usual minimum and never grows past the host's maximum — past that the app scrolls inside its own frame. An app that reports no size keeps the fixed height it always had, so nothing changes for apps that never asked.
  - **An app can close itself, cleanly (Phase 207).** If the app asks to be shut down, the host gives it a chance to save state first, then unmounts the frame and leaves a short **"This app closed itself"** note on the turn — never a dead frame. Reopening the conversation does not resurrect it.
  - **An app can only call its own server's tools (Phase 207).** When a rendered app invokes a tool, the Console resolves that call inside the app's OWN MCP server's namespace. An app cannot reach a tool on a different server, or any other tool in your catalog, by naming it — and it never picks the server itself; the host derives that from where the app came from. Everything else is unchanged: the call still runs under your identity triple and still hits the same approval / OAuth / paused-server gates. If the app names a tool that does not exist on its server, it gets a clear "not available here" answer it can render around, instead of a generic failure.
- **Errors** — red inline cards with the wrapped error chain. Click to expand stack/audit details.

For deeper introspection (per-event payloads, identity headers, raw LLM prompts), jump to the Task page — the Playground links to it from the task indicator at the top of the chat panel.

## 5. Multi-image conversation tip

Multiple images in one turn — drop them all at once, or use multiple paperclip clicks before pressing Enter. They all attach to the same `InputArtifactIDs` list. The LLM sees them as separate content blocks in order. The Playground shows them as thumbnails in the user turn.

## Common failure modes

- **"Task failed: ErrMissingAPIKey".** Same root cause as `harbor dev`'s — the env var isn't set in the shell that boots Runtime. Restart `harbor dev` with the key exported.
- **Image upload silently succeeds but the LLM "doesn't see" it.** Your provider doesn't speak multimodal vision (e.g. `gpt-3.5-turbo`, `claude-3-haiku`). Swap to a vision-capable model (claude-haiku-4.5, gpt-4o, gemini-1.5-pro). Bifrost passes the DataURL regardless; the provider rejects or ignores it.
- **PDF dropped but no tool picks it up.** You don't have a PDF-handling tool registered. Wire one — `add-an-in-process-tool` for an in-house PDF extractor, or attach an MCP server that exposes one.
- **Chat freezes mid-stream.** Almost always the LLM provider taking longer than `llm.timeout`. Bump the timeout, OR check provider status. The Task page shows the in-flight LLM call's elapsed time live.
- **"Steer" cancelled my tool call but the tool already wrote to my external API.** Tool calls aren't transactional — the cancellation kills the goroutine but doesn't roll back side effects the tool already executed. Design tools to be idempotent OR use approval (HITL pause before the side-effecting call).

## See also

- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — boot Runtime + Console first.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — Tasks / Events / Tools / Memory tabs for deeper introspection.
- [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) — register tools the Playground can drive.
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) — when you're building a chat UI OTHER than the Console.
- RFC §6.10 — the unified pause/resume primitive that backs steering.
- D-166 — `StartRequest.InputArtifactIDs` semantics.
