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

1. **Generated TypeScript types** — `cmd/harbor-gen-protocol-ts/` produces `web/console/src/lib/protocol.ts` from the Go `CanonicalWireTypes` (D-093). Reuse the generator (or vendor the output) and you get typed wire shapes for free.
2. **Capability advertisement** — `runtime.info.capabilities` tells you at attach which methods this Runtime advertises. Your UI degrades gracefully on stripped-down runtimes.
3. **Stable Protocol versioning** — breaking changes go through a deprecation window. Pin the version in your client; bump deliberately.

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

```http
POST /v1/protocol
{ "jsonrpc": "2.0", "method": "runtime.info", "id": 0 }
```

The response:

```json
{
  "jsonrpc": "2.0",
  "id": 0,
  "result": {
    "version": "1.1.0",
    "protocol_version": "1.1",
    "capabilities": {
      "tasks.start": true,
      "tasks.pause": true,
      "tasks.resume": true,
      "events.subscribe": true,
      "topology.snapshot": true,
      "artifacts.put": true,
      "skills.list": true,
      "...": "..."
    },
    "limits": {
      "max_artifact_size_bytes": 104857600,
      "max_concurrent_tasks_per_session": 8
    }
  }
}
```

Read `capabilities` and shape your UI accordingly. Don't call a method whose capability is `false` — it's a 501 Not Implemented otherwise.

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

The unified pause/resume primitive (RFC §6.10) is exposed as Protocol methods:

- `tasks.pause(task_id, reason)` — your UI requests a pause (e.g. user clicked "stop" or "steer").
- `tasks.resume(task_id, payload)` — your UI provides the resume payload (e.g. new user input for steering, an approval decision, an OAuth token).

For steering during a run:

```json
{ "jsonrpc": "2.0", "method": "tasks.pause", "params": {
  "task_id": "tsk_01HXYZ",
  "reason": "user_steer"
}, "id": 2 }
```

Then:

```json
{ "jsonrpc": "2.0", "method": "tasks.resume", "params": {
  "task_id": "tsk_01HXYZ",
  "payload": { "new_input": "Actually, make it Barcelona." }
}, "id": 3 }
```

The planner picks up the new input on its next turn. The "steer vs queue" UI choice in [`drive-the-playground`](../drive-the-playground/SKILL.md) §3 maps directly to "POST `tasks.pause`" vs "wait for `task.completed` then POST a new `tasks.start`".

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

## 8. Generated TypeScript types — reuse them

The Console's `web/console/src/lib/protocol.ts` is generated from Go's `CanonicalWireTypes`. Two consumption paths:

- **Vendor the file** — copy `protocol.ts` into your client. It has the wire types + method signatures + the typed `HarborClient`. License is Apache-2.0; attribution required.
- **Re-run the generator** — `make protocol-ts-gen` produces a fresh file. If you've forked Harbor, this gives you regeneration on every Runtime update.

Hand-rolling the types is fine for a quick prototype but you'll drift. Use the generator for any client you intend to maintain.

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
- **`tasks.start` returns 501 / "method not implemented".** Capability check missed — the Runtime advertised this method as off. Call `runtime.info` first, branch on `capabilities`.
- **Artifact upload returns 413 Payload Too Large.** Above `limits.max_artifact_size_bytes` from `runtime.info`. Chunk uploads aren't supported in V1.1 — bump the Runtime's `artifacts.max_size_bytes`.
- **Topology snapshot returns 501.** Old Runtime version — `topology.snapshot` landed in V1.1. Upgrade the Runtime.
- **The Console reads internal Runtime objects.** It doesn't — that would be a CLAUDE.md §13 violation. If you suspect leakage, file a bug; the Console reads only what's documented as a Protocol surface.

## See also

- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — boot the Runtime + grab the dev token first.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — the Console's chat UI; same Protocol underneath.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — every Console page maps 1:1 to a Protocol method.
- The wire types: `internal/protocol/types/`.
- The methods registry: `internal/protocol/methods/methods.go`.
- The error codes: `internal/protocol/errors/errors.go`.
- The generator: `cmd/harbor-gen-protocol-ts/`.
- D-093 — the generated-TypeScript decision.
- RFC §7 — Harbor Protocol design.
