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

- Chat history (assistant + user turns).
- Foreground task indicator (the LLM is reasoning; tool calls visible).
- Input box at the bottom — type + Enter to send.
- File-upload chip (paperclip icon) — drag-and-drop or click to attach.

Type a message, hit Enter. The Runtime mints a Task, dispatches it through the planner, and streams events back. You see the assistant response token-by-token as Bifrost streams from the provider.

## 2. File uploads — multimodal dispatch

Click the paperclip or drag a file into the chat. The Playground POSTs to the Runtime's `artifacts.put` endpoint, gets back an `ArtifactID`, and includes it in the next `StartRequest` via `InputArtifactIDs` (D-166).

The runtime then dispatches based on MIME:

- **`image/*`** — Path 1 INLINE. The image is base64-encoded into a `DataURL` and passed in the LLM call as a multimodal content block. The LLM sees the pixels directly. Works for any LLM provider that speaks multimodal vision (Claude, GPT-4o, Gemini, Llama 3.2 Vision).
- **`application/pdf`** — Path 2 ARTIFACT STUB. The planner sees `{ "ref": "art-abc123", "mime": "application/pdf", "size": 142853, "filename": "report.pdf" }` and can decide what to do (e.g. call a `pdf.extract_text` tool to pull pages out). The bytes never inline into the LLM context window.
- **`audio/*`** — Path 2 ARTIFACT STUB. Same as PDF — the planner sees a stub and routes to a transcription tool if you have one.
- **Other MIMEs** — Path 2 ARTIFACT STUB. Conservative default.

Per-MIME tool dispatch is controlled by `Tool.HandlesMIME(mime) bool` in the tool's spec. A tool that opts in to a MIME gets first-call rights when that MIME shows up in `InputArtifactIDs`.

### Limits

- **Max upload size**: governed by `artifacts.max_size_bytes` (default 100MB).
- **Path 1 inline cap**: ~20MB of image data per LLM call (the provider's actual limit varies). Larger images get downscaled by the Console before upload — the original lives in the artifact store; the LLM sees the downscaled version inline.

## 3. Foreground vs background tasks

The Playground's chat input drives FOREGROUND tasks — synchronous, the chat panel waits for completion. The Tasks page (in nav) drives BACKGROUND tasks — fire-and-forget, the agent works while you go elsewhere.

For a chat agent, foreground is what you want — you're in conversation. But the planner CAN spawn background tasks mid-run (e.g. "I'll fetch the data in the background while we keep talking"). Those show up in the Tasks page; the foreground chat reflects them with a small "background task spawned" event in the chat history.

### Steer vs queue — input during a running foreground task

When a foreground task is running and you type into the chat input, you get a CHOICE:

- **Steer** — interrupt the current run, redirect with your new input. The current run gets a `RequestPause` event with reason `user_steer`; the planner picks up the new input from its next turn.
- **Queue** — let the current run finish, then your input goes as the next user turn.

The UI presents two buttons; pick one. There is no default — the choice is explicit because steering mid-tool-call has different semantics than queuing. Steering during a tool call cancels the tool call's `ctx` and the planner sees the cancellation; queuing waits for the tool to finish.

The unified pause/resume primitive (RFC §6.10) is what makes this work — `RequestPause` is the same mechanism used for HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`. Steering is just one more reason code.

## 4. Reading the chat for debugging signals

The chat history surfaces several event types inline:

- **Assistant text** — the streamed LLM response. Starts from byte 0: Phase 107c moved the React planner onto native provider tool-calling, so `Content` deltas are the user-facing prose by structural construction (no JSON wrapper / no `{tool, args}` envelope buffering — the LLM no longer emits one). Chunks flow straight from bifrost's `OnContent` callback through to the Console with no extractor in the middle.
- **Tool calls** — collapsed by default; click to expand the args/result panel. Tool calls arrive on their own structured channel (`resp.ToolCalls []ToolCallStructured`) and are rendered as cards rather than inlined into the prose stream. **The agent can call several tools at once in a single turn (Phase 107d):** when it does, the runtime dispatches them concurrently and you'll see multiple tool-call cards for the same assistant turn, each with its own result. Concurrent dispatch is on by default; set `planner.parallel_tool_calls: false` in `harbor.yaml` to make the runtime run them one per step instead.
- **Thoughts/reasoning** — click the "Reasoning (N steps)" toggle on any agent bubble to see the model's intermediate thinking trace from the planner trajectory. The accordion shows the per-step reasoning the model produced; collapsed by default, one click expands it. Phase 107a.
- **Pause events** — yellow inline cards with reason ("oauth_required", "approval_pending", "user_steer", etc.). The card has a Resume button when applicable.
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
