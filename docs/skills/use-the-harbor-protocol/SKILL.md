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

1. **A generated, drift-gated contract reference** — the published [Protocol adoption track](https://hurtener.github.io/Harbor/protocol/) carries four pages (methods / events / errors / types) emitted by `cmd/harbor-gen-protocol-docs` from the Go single sources and gated in CI by `make protocol-docs-gen-check`, plus an executed quickstart, five choreography guides, a worked build-a-client walkthrough, and the conformance-certification path. For typed TS wire shapes, vendor the generated external-client module `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts` (emitted by `cmd/harbor-protocol-ts-types`, drift-gated by `make protocol-ts-types-gen-check`); the Console's `protocol.ts` is the hand-maintained client when you also want the typed `HarborClient` (the FULL Console-`protocol.ts` *generator* stays deferred per D-132, name reserved).
2. **Capability advertisement** — `runtime.info.capabilities` tells you at attach which Protocol surfaces this Runtime advertises (`task_control`, `events_subscribe`, `runtime_posture`, `topology_snapshot`, `state_snapshots`, `agent_config`). Your UI degrades gracefully on stripped-down runtimes.
3. **Stable Protocol versioning** — breaking changes go through a deprecation window; same-major versions are compatible. Pin the major in your client; tolerate additive change. The full adopter contract is the published [versioning & compatibility choreography](https://hurtener.github.io/Harbor/protocol/versioning-and-compatibility).

The Protocol is what makes Harbor headless. The Runtime never imports Console code; the Console never reads internal Runtime objects. Your UI sits in the same posture as the Console.

## 1. The wire — base URL, auth, identity

The wire is **REST-per-method**: each Protocol method is its own route under `/v1/`, you POST a flat JSON body, and you get a flat JSON response back — there is no JSON-RPC envelope. Every request carries:

```http
POST /v1/control/start HTTP/1.1
Host: 127.0.0.1:18080
Content-Type: application/json
Authorization: Bearer <JWT>
X-Harbor-Tenant: <tenant_id>
X-Harbor-User: <user_id>
X-Harbor-Session: <session_id>
```

- **Bearer JWT**: RS256/RS384/RS512/ES256/ES384/ES512 signed token. Issuer + audience match the Runtime's `identity:` block, exactly — `harbor serve` hard-rejects an `iss`/`aud` mismatch with `401 auth_rejected`. For `harbor dev`, the ephemeral `HARBOR_DEV_TOKEN` (printed on stderr) is what you use — see [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md). For **production** (`harbor serve` mints no token), obtain a JWT from your IdP or self-issue one — the full setup (OIDC app registration, the `(tenant, user, session)` + `scopes` claim mapping, the `iss`/`aud` exact-match contract, and the no-IdP `harbor token` on-ramp) is the [production identity setup guide](https://hurtener.github.io/Harbor/protocol/production-identity-setup).
- **`X-Harbor-Session`**: the per-request session selector (D-171). The connection JWT verifies the WHO (`tenant` + `user`) and the scopes; the **session is chosen per-conversation** by this header and may differ on every request — the connection token is a per-backend credential, not a single-session pin. A new session id is a new conversation (create-on-first-use on the first `start`). The token's `session` claim is a back-compat **default** used only when the header is absent. `X-Harbor-Tenant` / `X-Harbor-User` can never widen the JWT-verified principal. Every storage call still filters by the full `(tenant, user, session)` triple — no cross-session leakage. Full Console contract: [`docs/notes/session-model-contract.md`](../../notes/session-model-contract.md).

Routes group by surface family:

- **Task control** — `start` plus the nine steering verbs (`cancel` / `pause` / `resume` / `redirect` / `inject_context` / `approve` / `reject` / `prioritize` / `user_message`) all POST to `POST /v1/control/{method}` (e.g. `/v1/control/start`, `/v1/control/cancel`). The read-only posture methods (`runtime.info`, `topology.snapshot`) and `artifacts.put` share this route shape.
- **Event stream** — `GET /v1/events` (SSE; see §4).
- **Read surfaces** group by family under their own prefix: `POST /v1/tasks/{method}` (e.g. `/v1/tasks/get`), `POST /v1/tools/{method}`, `POST /v1/sessions/{method}`, `POST /v1/memory/{method}`, and so on.
- **Admin control surfaces** are family-prefixed and gate on the verified `admin` scope claim (a non-admin caller gets `403 {"code": "scope_mismatch"}`). The governance tenant-default LLM overrides are here: `POST /v1/governance/set_tenant_overrides` sets a tenant's default model / additive extra-instructions / temperature / max-tokens / reasoning-effort live (no redeploy; applied to every session's next run), and `POST /v1/governance/get_tenant_overrides` reads them back. `POST /v1/governance/rotate_key` rotates the LLM provider API key live (no redeploy; the swap is **immediate** — the next call uses the new key). The new key is a **secret**: send it on the request body only; the response + the `governance.key_rotated` event carry only a `sha256:` fingerprint, never the key. (`governance.posture` / `llm.posture` remain read-only posture methods.) The tenant default composes UNDER a per-session next-turn override — `POST /v1/runs/set_overrides` records a one-shot override (reasoning-effort / temperature / max-tokens / `system_prompt_override` (a full prompt **replace**) / `model` (the session model swap)) for the caller's own session; the effective per-run value resolves **session › per-agent › tenant-wide baseline › config** (the session override wins, then the per-agent agent-config LLM-params layer below, then the tenant-wide baseline; the session slot is consumed once on the next run). The **agent-config control plane** is the admin family `POST /v1/agent_config/*`: it versions an agent's configuration as immutable, content-addressed revisions where the active config is a pointer to a revision — `set_revision` writes a new revision and advances the pointer, `list_revisions` returns the chain newest-first, `diff` compares two revisions, `rollback` repoints to an existing revision (never mutating it), and `get` reads the active one. Skills control is the first consumer: `POST /v1/agent_config/skills/upsert` / `skills/delete` / `skills/list` manage an agent's skills, recording each membership change as a config revision (so skills inherit diff + rollback) and emitting `agent.config.revised` / `agent.config.reverted`. `POST /v1/agent_config/set_prompt_layers` sets the agent's **layered system prompt** — an operator-owned `base` layer plus an optional `user` layer that composes *above* the base without replacing it (the composition order is the trust boundary; a `user`-only writer can extend guidance but never weaken the operator base). It replaces only the prompt-layer section (skills + tool exposure are preserved) and records a revision; at run start the durable base/user resolve under the per-session `system_prompt_override` (which replaces the whole base+user spine for one message) and above the additive `extra_instructions`. `POST /v1/agent_config/set_llm_params` pins the agent's **per-agent LLM parameters** (`model` / `temperature` / `max_tokens` / `reasoning_effort`) as a versioned section — a set `model` is validated against the configured `ModelProfiles` at set time (an unknown model is rejected, never persisted). It replaces only the LLM-params section (the prompt-layer + skills + tool-exposure + connection sections are preserved) and overrides the tenant-wide baseline per field for the agent's next run  `POST /v1/agent_config/set_tool_exposure` sets the agent's **MCP-exposure / per-tool policy**: `paused_servers` (MCP source ids excluded next-run, live transport stays warm), `disabled_tools` (individually-excluded tools), and the runtime **loading-mode overrides** — `server_loading_modes` (per MCP source id, applies to TOOL-form descriptors only) and `tool_loading_modes` (exact per-tool name, unconditional), each valued `always`/`deferred`; an unknown value is rejected `400 {"code": "invalid_request"}` **before** any revision is recorded. A `deferred` override hides the tool from the next run's prompt-time catalog while it stays `tool_search`-discoverable and dispatch-callable (the two-turn discovery cycle is untouched); a paused server / disabled tool stays strictly stronger — hidden from BOTH the prompt and dispatch. Precedence: per-tool override > per-server override > the boot `tools.entries[].loading_mode` > the driver default. It replaces only the tool-exposure section (skills + prompt layers + connections + LLM params are preserved). (resolution **session › per-agent › tenant-wide baseline › config**). Every edit applies on the agent's **next run** (next-turn projection — never mid-flight); an upsert that would overwrite a pack-origin skill is refused with `400 {"code": "invalid_request"}`, never silently.

The body is a flat JSON object — the method's request shape — with an `identity` object carrying the triple (or the headers above; the body's `identity` may be left empty when the headers supply it):

```json
{ "identity": { "tenant": "dev", "user": "dev", "session": "dev" }, "query": "Hello, agent!" }
```

The response is the method's flat response shape directly — no `result` / `error` wrapper. A failure is an HTTP status plus a `{"code": "..."}` envelope (e.g. `404 {"code": "unknown_method"}`).

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
  "uptime_seconds": 16,
  "wire_surface_digest": "sha256:f870c37dce2b26e8b4b35af1fbf51e056c1a5c9a7a1d93bda6682aee8c5ba861"
}
```

Three things to read and act on:

- `protocol_version` — the wire-contract version (distinct from `build_version`, the Runtime's own release). Same major ⇒ compatible; on a major mismatch, warn loudly or refuse.
- `capabilities` — the advertised Protocol surfaces. Shape your UI on this list: a runtime that doesn't advertise `topology_snapshot` gets the topology panel disabled, not a crash. A method outside the Runtime's registry returns the canonical `404 {"code": "unknown_method"}` envelope — treat it (and 405 / 501) as "not served here, degrade", the same SKIP posture Harbor's own smoke scripts encode.
- `wire_surface_digest` — an opaque, stable `sha256:` fingerprint of the Runtime's canonical wire surface (the Protocol version + method / error / capability / wire-type *names*; it deliberately excludes field shapes and event-type names). Stamp the digest your client was built against into the build, then compare it here at attach: equal ⇒ same surface; different ⇒ surface drift, surface it loudly; absent/empty ⇒ the Runtime predates digest support (an informational note, never a drift alarm). It is a coarse name-level early-warning, not a substitute for the field-level checks a client that vendors the wire manifest runs at build time.

## 3. Starting a task — the chat-message equivalent

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/start" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "query": "What'\''s the weather in Madrid?", "input_artifact_ids": []}'
```

The request is the flat `StartRequest`: `identity` (the triple — empty here because the headers supply it), the `query` string, and the optional `input_artifact_ids`. There is no `foreground` field — every `start` mints a task and you observe it on the event stream.

Response is the flat `StartResponse`:

```json
{
  "task_id": "tsk_01HXYZ...",
  "reused": false,
  "protocol_version": "0.1.0"
}
```

`reused` is `true` only when you supplied an `idempotency_key` that matched an existing task; `protocol_version` lets you detect a version skew.

To make a task return a **schema-conforming answer**, add the optional `output_schema` field — a JSON-Schema document the runtime validates the task's terminal answer against. On success the completed task's envelope carries a validated `answer_payload` alongside the plain `answer` string, readable via `tasks.get`'s `result_inline` (and via a parent run's AwaitTask observation). An empty, non-compiling, or over-cap (64 KiB) schema is rejected at the edge with a `400 {"code": "invalid_request"}` envelope before any task spawns; a schema-invalid answer after the runtime's correction budget fails the task loud with the `output_invalid` error code — never a schemaless success. A schema-constrained task suppresses assistant token deltas on the event stream (a validate-and-retry loop cannot retract streamed tokens), so the answer arrives once, on completion. Omit the field for the default schemaless behaviour.

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/start" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "query": "Classify the sentiment of: shipping delayed again.",
       "output_schema": {"type": "object", "required": ["sentiment"],
                         "properties": {"sentiment": {"type": "string"}},
                         "additionalProperties": false}}'
```

For multimodal input, upload artifacts FIRST (`artifacts.put`, see §6) and pass the returned IDs in `input_artifact_ids` (D-166). The per-MIME dispatch — image inline vs PDF/audio as ArtifactStub — happens inside the planner; your client just passes refs. To override how an attachment is handed to the model, add the optional `input_artifact_dispositions` map (Phase 84b — D-189), keyed by artifact id with values `ref` | `inline` | `provider_native` | `tool:<name>` (e.g. `{"art_x": "tool:pdf.extract"}` forces the named catalog tool). Your hint is the top precedence layer (hint > the agent's `multimodal.disposition` config map > the runtime default: image inline, everything else ref); an omitted map keeps today's behaviour. `tasks.get` reflects the hint on `input_artifacts[].disposition`, and the resolution (including degradations — e.g. an unknown `tool:<name>`) is observable as `task.input_disposition.resolved` events. A `provider_native` hint is honoured end-to-end (Phase 84c — D-190): the LLM driver uploads the attachment to the provider's file surface and the upload is observable as `llm.provider_file.uploaded` events (artifact ref, provider, modality, `file_id`).

## 4. The events stream — SSE `events.subscribe`

The Protocol exposes events as Server-Sent Events:

```http
GET /v1/events?access_token=<JWT>
Accept: text/event-stream
X-Harbor-Tenant: <tenant_id>
X-Harbor-User: <user_id>
X-Harbor-Session: <session_id>
```

The subscription is **identity-scoped** — it streams the whole session's events — so there is **no `task_id` query param**. A client that can set headers narrows server-side with the optional `X-Harbor-Run` (a task id) and the repeatable `X-Harbor-Event-Type` headers. A browser `EventSource` (which can't set custom headers) authenticates via the `?access_token=` query-param shim — same JWT, same identity triple, its `session` claim scoping the stream — and filters client-side on the event payload's task id. The query-param shim is documented in `internal/protocol/transports/transports.go`.

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
2. POST `start`, get `task_id`.
3. Open an SSE stream for that `task_id`.
4. Append `llm.completion.chunk` content to a streaming "assistant turn" bubble.
5. Render `tool.invoked` / `tool.result` as collapsed cards inside the assistant bubble.
6. Close the bubble on `task.completed`.

## 4a. Reopening a long session — `state.history`

`events.subscribe` is the LIVE tail. To **reopen** a closed conversation you don't want to re-stream every event from sequence 1 — a 5 000-event session would flood the client before the newest turn renders. The `state.history` method (capability `state_snapshots`) is the bounded, **tail-first** windowed read of the same durable event stream:

```http
POST /v1/state/history
Authorization: Bearer <JWT>
Content-Type: application/json

{ "session_id": "<session_id>", "before": 0, "limit": 50 }
```

`before: 0` means "from the tail" (the newest retained events); `limit` is the window size K (default 50, max 200). The response is a page of flat events **oldest-first within the window**, plus the bounds and a scroll-up cursor:

```json
{
  "events": [ { "type": "...", "sequence": 4951, "occurred_at": "...", "payload": {...}, "artifacts": [...] }, ... ],
  "head_sequence": 1,
  "tail_sequence": 5000,
  "next_cursor": 4951,
  "has_more": true,
  "truncated": false
}
```

To **scroll up** (load one window older), pass the previous response's `next_cursor` back as `before`. When `next_cursor` is `0`, you've reached the retained head — no older events remain. Reduction of events → chat messages stays **on your client** (the same reducer you use for the live stream): the surface returns flat events, not pre-reduced turns.

Heavy payloads (a large tool result offloaded above the heavy-output threshold) ride by a **routable** `StateArtifactRef` on `events[].artifacts[]` — a content-addressed `id` (+ `sha256`), never inline bytes. Resolve it the same way as any artifact: POST the `id` to `artifacts.get_ref` (presigned URL on an S3-compat store; the typed `presign_unsupported`/501 on the default inmem/fs stores).

Identity rules: identity is mandatory (an incomplete triple is `identity_required`/401); an unknown or cross-identity `session_id` is `not_found`/404 (existence is never revealed across identities — never a 403); a cross-tenant read requires the verified `admin` scope claim.

## 4b. Erasing a session — `sessions.delete`

To satisfy a data-lifecycle / right-to-erasure request, `sessions.delete` (capability `session_lifecycle`) **deletes a whole session and cascades deletion of its scoped State, Memory, and Artifacts**. It is **own-session-only** — you erase solely your own verified `(tenant, user, session)`; there is no admin / cross-tenant path. The route is `POST /v1/sessions/delete`:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/sessions/delete" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"tenant": "dev", "user": "dev", "session": "'$SESSION'"}}'
# → 200 {"session_id":"...","deleted":true,"state_records_deleted":N,"artifacts_deleted":M,"memory_purged":true}
```

The response carries non-sensitive deletion telemetry only — never erased content. The bytes are **hard-deleted** (not tombstoned); the only durable trace is a redacted, content-free `session.erased` audit event written under your observability scope (so a follow-up `state.history` for the erased session returns empty). Rules: identity is mandatory (`identity_required`/401, which a body identity mismatching your verified triple also hits); a session with a **RUNNING task is refused** `session_running`/409 with **no store touched** — wait for the task to finish (or cancel it) and retry; an absent session is `not_found`/404. Check `runtime.info.capabilities` for `session_lifecycle` before calling — a runtime that did not wire an eraser does not advertise it and answers a 404 at the route.

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

For images / PDFs / audio uploads from your UI, `artifacts.put` is a control-surface method: POST the bytes (base64-encoded inline on the request leg) and you get back a reference, never an echo of the body:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/artifacts.put" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{
    "scope": {"tenant": "dev", "user": "dev", "session": "dev"},
    "bytes": "'"$(base64 < report.pdf)"'",
    "opts": {"mime_type": "application/pdf", "filename": "report.pdf"}
  }'
```

Response carries the canonical `ref`:

```json
{
  "ref": {
    "id": "art_01H...",
    "mime_type": "application/pdf",
    "size_bytes": 142853,
    "filename": "report.pdf"
  },
  "protocol_version": "0.1.0"
}
```

Pass `ref.id` in `start`'s `input_artifact_ids`. The upload bytes ride the request leg only (base64-inline, bounded by the Runtime's max request size — an oversize body is rejected with `request_too_large`); the response is a reference, and bytes never reach the LLM edge inline.

## 7. Topology snapshot — render the runtime's wiring

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/topology.snapshot" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}}'
```

Response is a graph of components + edges — Bifrost, tool catalog (with per-tool nodes), memory driver, state driver, artifact store, event bus, skill catalog. The Console's Topology page is one consumer; your custom dashboard could be another.

The capability is `topology.snapshot: true` (V1.1 phase 84a).

## 8. Typed wire shapes — where they actually come from

Three trustworthy sources, none of which is hand-rolling:

- **Vendor the generated external-client TS module (TypeScript clients — start here).** `cmd/harbor-protocol-ts-types` emits `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts` — a self-contained, dependency-free module of TypeScript `interface`s for every canonical wire type plus `HarborMethod` / `HarborErrorCode` / `HarborEventType` string-union types and the pinned `PROTOCOL_VERSION` / `WIRE_SURFACE_DIGEST` constants. Copy that one file into your client and write your transport against it. Regenerate / re-vendor with `make protocol-ts-types-gen`; it is drift-gated by `make protocol-ts-types-gen-check`. The worked `event-viewer-ts` client consumes it against the dev runtime. This module carries **types only, no client logic** — it is distinct from the Console's hand-maintained `protocol.ts` (which carries the typed `HarborClient`), and the FULL Console-`protocol.ts` generator remains deferred (D-132); the `cmd/harbor-gen-protocol-ts` name stays reserved for it.
- **The generated contract reference** — [the generated types page](https://hurtener.github.io/Harbor/protocol/types) catalogues every canonical wire struct field-by-field with the snake_case JSON keys, generated by `cmd/harbor-gen-protocol-docs` from the Go single sources and drift-gated by `make protocol-docs-gen-check`. Read it (or transcribe from it) when you want the field-level reference; when the wire changes, the page changes in the same PR by construction.
- **Vendor the Console's client module** — copy `web/console/src/lib/protocol.ts` into your TS client when you also want the typed `HarborClient` transport (not just the types). It is **hand-maintained** in lockstep with the Go wire types (the full TS-client *generator* is deferred per D-132 / issue #179), kept honest by the Console's CI. License is Apache-2.0; attribution required.

Hand-rolling the types from scratch is fine for a quick prototype but you'll drift. Anchor any client you intend to maintain on the generated module or the generated reference.

## 9. A minimal client (TS, ~30 LoC)

```typescript
const baseUrl = "http://127.0.0.1:18080";
const token = "<HARBOR_DEV_TOKEN>";
const identity = { tenant: "dev", user: "dev", session: "dev" };

// One REST call per method: POST /v1/<family>/<method>, flat body in, flat body out.
async function call<T>(route: string, body: object): Promise<T> {
  const res = await fetch(`${baseUrl}${route}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${token}`,
      "X-Harbor-Tenant": identity.tenant,
      "X-Harbor-User": identity.user,
      "X-Harbor-Session": identity.session,
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ code: `http_${res.status}` }));
    throw new Error(err.code ?? `http_${res.status}`);
  }
  return res.json() as Promise<T>;
}

const info = await call("/v1/control/runtime.info", { identity: {} });
console.log("connected to harbor", info);

const { task_id } = await call<{ task_id: string }>("/v1/control/start", { identity: {}, query: "Hello!" });

// The stream is session-scoped (no task_id query param), so filter client-side.
const sse = new EventSource(`${baseUrl}/v1/events?access_token=${encodeURIComponent(token)}`);
sse.addEventListener("llm.completion.chunk", (e) => {
  const data = JSON.parse(e.data);
  if (data.task_id === task_id) process.stdout.write(data.chunk);
});
sse.addEventListener("task.completed", (e) => {
  if (JSON.parse(e.data).task_id === task_id) sse.close();
});
```

That's a working CLI chatbot in 30 lines. Wrap the same in React/Svelte/Vue/whatever your stack is, render the chunks into a bubble, and you have a chat UI.

## Common failure modes

- **Every call returns 401.** Token expired (24h TTL) or rotated (`harbor dev` restarted). Re-fetch token, retry.
- **CORS preflight fails.** Your origin isn't in `server.allowed_origins`. Add it to the yaml + restart Runtime.
- **SSE stream opens but no events.** The `payload.TaskID` capital-T gotcha — your handler is reading `payload.task_id` (lowercase). Fix the case.
- **A control call returns `404 {"code": "unknown_method"}` or 405/501.** This runtime doesn't serve that surface. Call `runtime.info` first, branch on `capabilities`, and degrade the feature instead of crashing (the [versioning & compatibility contract](https://hurtener.github.io/Harbor/protocol/versioning-and-compatibility)).
- **Artifact upload returns 413 Payload Too Large.** The request body exceeded the Runtime's `protocol.max_request_bytes` (default 4 MiB) — the canonical `{"code": "request_too_large"}` envelope. Chunk uploads aren't supported in V1.1; raise `protocol.max_request_bytes` in the Runtime's `harbor.yaml` if you need larger inline uploads.
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
- The docs generator: `cmd/harbor-gen-protocol-docs/` (D-209). The external-client TS wire-type generator: `cmd/harbor-protocol-ts-types/` (D-269) → `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts`. The FULL Console-`protocol.ts` TS-client generator remains deferred (D-132 / issue #179, name `cmd/harbor-gen-protocol-ts` reserved); `protocol.ts` is hand-maintained.
- RFC §5 — Harbor Protocol design.
