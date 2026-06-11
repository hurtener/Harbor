---
name: use-the-harbor-protocol
description: "Build a chat UI (or any other client) against the Harbor Protocol directly — auth headers, the typed wire surface, events.subscribe SSE, the topology_snapshot capability, artifact upload. Use when shipping a frontend that talks to the runtime WITHOUT the bundled Console — a custom chatbot, a Slack bot, a TUI, an IDE plugin."
license: Apache-2.0
metadata:
  framework: harbor
  surface: protocol
  verbs: ""
---

# Use the Harbor Protocol

The Harbor Protocol is the canonical event/state contract between Runtime and any client. The bundled Console is one consumer; this skill walks the path for building your own. A working chatbot UI is achievable in a day on top of the Protocol — the wire is small, typed, and stable.

Three properties make this practical:

1. **A generated, drift-gated contract reference** — the published [Protocol adoption track](https://hurtener.github.io/Harbor/protocol/) carries four pages (methods / events / errors / types) emitted by `cmd/harbor-gen-protocol-docs` from the Go single sources and gated in CI by `make protocol-docs-gen-check`, plus an executed quickstart, five choreography guides, a worked build-a-client walkthrough, and the conformance-certification path. For typed TS wire shapes, vendor the Console's hand-maintained `web/console/src/lib/protocol.ts` (the D-093 TS *generator* was deferred per D-132 — `protocol.ts` is hand-maintained today, kept honest by the Console's own CI).
2. **Capability advertisement** — `runtime.info.capabilities` tells you at attach which Protocol surfaces this Runtime advertises (`task_control`, `events_subscribe`, `runtime_posture`, `topology_snapshot`). Your UI degrades gracefully on stripped-down runtimes.
3. **Stable Protocol versioning** — breaking changes go through a deprecation window; same-major versions are compatible. Pin the major in your client; tolerate additive change. The full adopter contract is the published [versioning & compatibility choreography](https://hurtener.github.io/Harbor/protocol/versioning-and-compatibility).

The Protocol is what makes Harbor headless. The Runtime never imports Console code; the Console never reads internal Runtime objects. Your UI sits in the same posture as the Console.

## 1. The wire — base URL, auth, identity

Every Protocol request carries:

```http
POST /v1/protocol HTTP/1.1
Host: 127.0.0.1:18080
Content-Type: application/json
Authorization: Bearer <JWT>
X-Harbor-Tenant: <tenant_id>
X-Harbor-User: <user_id>
X-Harbor-Session: <session_id>
```

- **Bearer JWT**: RS256/RS384/RS512/ES256/ES384/ES512 signed token. Issuer + audience match the Runtime's `identity:` block. For `harbor dev`, the ephemeral `HARBOR_DEV_TOKEN` (printed on stderr) is what you use — see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md).
- **`X-Harbor-Session`**: the per-request session selector (D-171). The connection JWT verifies the WHO (`tenant` + `user`) and the scopes; the **session is chosen per-conversation** by this header and may differ on every request — the connection token is a per-backend credential, not a single-session pin. A new session id is a new conversation (create-on-first-use on the first `start`). The token's `session` claim is a back-compat **default** used only when the header is absent. `X-Harbor-Tenant` / `X-Harbor-User` can never widen the JWT-verified principal. Every storage call still filters by the full `(tenant, user, session)` triple — no cross-session leakage. Full Console contract: [`docs/notes/session-model-contract.md`](../../notes/session-model-contract.md).

Body is JSON-RPC 2.0:

```json
{
  "jsonrpc": "2.0",
  "method": "tasks.start",
  "params": { "input": "Hello, agent!" },
  "id": 1
}
```

CORS is default-deny. For browser clients, your origin must be in the Runtime's `server.allowed_origins`. See [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) §2.

## 2. The handshake — `runtime.info` first

The first call your client makes:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/runtime.info" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}}'
```

A real response from a dev Runtime:

```json
{
  "instance_id": "harbor-dev-192.168.1.7",
  "display_name": "harbor dev",
  "build_version": "v0.0.0-dev",
  "build_commit": "dev",
  "build_go_version": "go1.26.3",
  "protocol_version": "0.1.0",
  "capabilities": ["events_subscribe", "runtime_posture", "task_control"],
  "uptime_seconds": 16
}
```

Two things to read and act on:

- `protocol_version` — the wire-contract version (distinct from `build_version`, the Runtime's own release). Same major ⇒ compatible; on a major mismatch, warn loudly or refuse.
- `capabilities` — the advertised Protocol surfaces. Shape your UI on this list: a runtime that doesn't advertise `topology_snapshot` gets the topology panel disabled, not a crash. A method outside the Runtime's registry returns the canonical `404 {"code": "unknown_method"}` envelope — treat it (and 405 / 501) as "not served here, degrade", the same SKIP posture Harbor's own smoke scripts encode.

## 3. Starting a task — the chat-message equivalent

```http
POST /v1/protocol
{
  "jsonrpc": "2.0",
  "method": "tasks.start",
  "params": {
    "input": "What's the weather in Madrid?",
    "input_artifact_ids": [],
    "foreground": true
  },
  "id": 1
}
```

Response is a task envelope:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "task_id": "tsk_01HXYZ...",
    "session_id": "sess_dev",
    "status": "running"
  }
}
```

For multimodal input, upload artifacts FIRST (`artifacts.put`, see §6) and pass the returned IDs in `input_artifact_ids` (D-166). The per-MIME dispatch — image inline vs PDF/audio as ArtifactStub — happens inside the planner; your client just passes refs.

## 4. The events stream — SSE `events.subscribe`

The Protocol exposes events as Server-Sent Events:

```http
GET /v1/protocol/events?task_id=tsk_01HXYZ&access_token=<JWT>
Accept: text/event-stream
```

(Note: SSE doesn't allow custom headers from EventSource, so the auth is via the `access_token` query param shim — same JWT, same identity triple, encoded in the URL. The query-param shim is documented in `internal/protocol/transports/sse.go`.)

The stream is a sequence of `event: <type>\ndata: <JSON>\n\n` blocks:

```text
event: llm.completion.chunk
data: {"task_id":"tsk_01HXYZ","chunk":"Hello"}

event: llm.completion.chunk
data: {"task_id":"tsk_01HXYZ","chunk":" there!"}

event: tool.invoked
data: {"task_id":"tsk_01HXYZ","tool":"weather.get_current","args":{"city":"Madrid"}}

event: tool.result
data: {"task_id":"tsk_01HXYZ","tool":"weather.get_current","result":{"temperature_c":21.3}}

event: task.completed
data: {"task_id":"tsk_01HXYZ","status":"completed"}
```

**A gotcha**: the event payload's task ID field is `payload.TaskID` (capital T) — match exactly when parsing in JS/TS. Documented in the Console's chat panel handler; easy to miss when hand-rolling.

For a chat UI, you'd:

1. Append a "user turn" bubble to the chat.
2. POST `tasks.start`, get `task_id`.
3. Open an SSE stream for that `task_id`.
4. Append `llm.completion.chunk` content to a streaming "assistant turn" bubble.
5. Render `tool.invoked` / `tool.result` as collapsed cards inside the assistant bubble.
6. Close the bubble on `task.completed`.

## 5. Pause + steer + resume

The unified pause/resume primitive (RFC §3.3) is one wire choreography for every cause — HITL approval, tool-side OAuth, operator pause. The steering verbs share one route shape, `POST /v1/control/{method}`, with the run id and your steering scope in the body's `identity`:

```bash
# park the run at the next planner-step boundary
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/pause" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"run": "'$TASK_ID'", "scope": "owner_user"}}'

# feed it context while parked, then wake it
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/inject_context" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"run": "'$TASK_ID'", "scope": "session_user"}, "payload": {"note": "Actually, make it Barcelona."}}'

curl -sS -X POST "$HARBOR_BASE_URL/v1/control/resume" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"run": "'$TASK_ID'", "scope": "owner_user"}}'
```

The `200 {"accepted": true, …}` means *enqueued*; the effect is narrated on the event stream — `pause.requested` when the run parks, `pause.resumed` (with a typed `Decision` of `approve` / `reject` / `resume` / `timeout`) when it wakes. The planner sees injected context on its next step.

For HITL: an approval-gated tool emits `tool.approval_requested` with a pause token; your UI routes the human verdict through `POST /v1/control/approve` or `/reject` with `"payload": {"token": "<pause-token>", "reason": "…"}`. `POST /v1/pause/list` is the snapshot of everything currently awaiting a human — reconcile against it on every (re)attach; it is authoritative across Runtime restarts. The full wire choreography (including the OAuth callback leg and `DecisionTimeout` reaps) is the published [pause-model choreography](https://hurtener.github.io/Harbor/protocol/pause-model).

The "steer vs queue" UI choice in [`drive-the-playground`](../drive-the-playground/SKILL.md) §3 maps directly to "POST `/v1/control/pause` + inject + resume" vs "wait for `task.completed` then POST a new `start`".

## 6. Artifact upload — multimodal input

For images / PDFs / audio uploads from your UI:

```http
POST /v1/protocol/artifacts
Content-Type: multipart/form-data; boundary=...
Authorization: Bearer <JWT>
X-Harbor-Tenant: dev
X-Harbor-User: dev
X-Harbor-Session: dev

--<boundary>
Content-Disposition: form-data; name="file"; filename="report.pdf"
Content-Type: application/pdf

<bytes>
--<boundary>--
```

Response:

```json
{ "artifact_id": "art_01H...", "mime": "application/pdf", "size": 142853 }
```

Pass `artifact_id` in `tasks.start.input_artifact_ids`. Bytes never go on the JSON-RPC wire.

## 7. Topology snapshot — render the runtime's wiring

```http
POST /v1/protocol
{ "jsonrpc": "2.0", "method": "topology.snapshot", "id": 4 }
```

Response is a graph of components + edges — Bifrost, tool catalog (with per-tool nodes), memory driver, state driver, artifact store, event bus, skill catalog. The Console's Topology page is one consumer; your custom dashboard could be another.

The capability is `topology.snapshot: true` (V1.1 phase 84a).

## 8. Typed wire shapes — where they actually come from

Two trustworthy sources, neither of which is hand-rolling:

- **The generated contract reference** — [the generated types page](https://hurtener.github.io/Harbor/protocol/types) catalogues every canonical wire struct field-by-field with the snake_case JSON keys, generated by `cmd/harbor-gen-protocol-docs` from the Go single sources and drift-gated by `make protocol-docs-gen-check`. Transcribe your client types from it; when the wire changes, the page changes in the same PR by construction.
- **Vendor the Console's client module** — copy `web/console/src/lib/protocol.ts` into your TS client. It carries the wire types + the typed `HarborClient`. It is **hand-maintained** (the D-093 TS generator was deferred per D-132 / issue #179), kept honest by the Console's CI rather than by codegen. License is Apache-2.0; attribution required.

Hand-rolling the types from scratch is fine for a quick prototype but you'll drift. Anchor any client you intend to maintain on the generated reference.

## 9. A minimal client (TS, ~30 LoC)

```typescript
const baseUrl = "http://127.0.0.1:18080";
const token = "<HARBOR_DEV_TOKEN>";
const identity = { tenant: "dev", user: "dev", session: "dev" };

async function call<T>(method: string, params?: object): Promise<T> {
  const res = await fetch(`${baseUrl}/v1/protocol`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${token}`,
      "X-Harbor-Tenant": identity.tenant,
      "X-Harbor-User": identity.user,
      "X-Harbor-Session": identity.session,
    },
    body: JSON.stringify({ jsonrpc: "2.0", method, params, id: crypto.randomUUID() }),
  });
  const json = await res.json();
  if (json.error) throw new Error(json.error.message);
  return json.result as T;
}

const info = await call("runtime.info");
console.log("connected to harbor", info);

const { task_id } = await call<{ task_id: string }>("tasks.start", { input: "Hello!", foreground: true });

const sse = new EventSource(`${baseUrl}/v1/protocol/events?task_id=${task_id}&access_token=${encodeURIComponent(token)}`);
sse.addEventListener("llm.completion.chunk", (e) => {
  const data = JSON.parse(e.data);
  process.stdout.write(data.chunk);
});
sse.addEventListener("task.completed", () => sse.close());
```

That's a working CLI chatbot in 30 lines. Wrap the same in React/Svelte/Vue/whatever your stack is, render the chunks into a bubble, and you have a chat UI.

## Common failure modes

- **Every call returns 401.** Token expired (24h TTL) or rotated (`harbor dev` restarted). Re-fetch token, retry.
- **CORS preflight fails.** Your origin isn't in `server.allowed_origins`. Add it to the yaml + restart Runtime.
- **SSE stream opens but no events.** The `payload.TaskID` capital-T gotcha — your handler is reading `payload.task_id` (lowercase). Fix the case.
- **A control call returns `404 {"code": "unknown_method"}` or 405/501.** This runtime doesn't serve that surface. Call `runtime.info` first, branch on `capabilities`, and degrade the feature instead of crashing (the [versioning & compatibility contract](https://hurtener.github.io/Harbor/protocol/versioning-and-compatibility)).
- **Artifact upload returns 413 Payload Too Large.** Above `limits.max_artifact_size_bytes` from `runtime.info`. Chunk uploads aren't supported in V1.1 — bump the Runtime's `artifacts.max_size_bytes`.
- **Topology snapshot rejected.** This Runtime doesn't advertise the `topology_snapshot` capability — check `runtime.info.capabilities` before enabling the panel.
- **The Console reads internal Runtime objects.** It doesn't — that would be a CLAUDE.md §13 violation. If you suspect leakage, file a bug; the Console reads only what's documented as a Protocol surface.

## See also

- **The Protocol adoption track** — [the published quickstart + generated reference + choreographies](https://hurtener.github.io/Harbor/protocol/): the adopter-facing contract docs this skill's recipes sit on top of. Start there for any client you intend to maintain. The track is complete: five choreographies (including [the pause model](https://hurtener.github.io/Harbor/protocol/pause-model) and [versioning & compatibility](https://hurtener.github.io/Harbor/protocol/versioning-and-compatibility)), the worked [build-a-client guide](https://hurtener.github.io/Harbor/protocol/build-a-client) (its ~150-line SDK-free event viewer ships at `examples/protocol-clients/event-viewer/`), and the [conformance-certification path](https://hurtener.github.io/Harbor/protocol/conformance-certification).
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — boot the Runtime + grab the dev token first.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — the Console's chat UI; same Protocol underneath.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — every Console page maps 1:1 to a Protocol method.
- The wire types: `internal/protocol/types/`.
- The methods registry: `internal/protocol/methods/methods.go`.
- The error codes: `internal/protocol/errors/errors.go`.
- The docs generator: `cmd/harbor-gen-protocol-docs/` (D-209). The D-093 TS generator remains deferred (D-132 / issue #179); `protocol.ts` is hand-maintained.
- RFC §5 — Harbor Protocol design.
