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

Go clients should use `github.com/hurtener/Harbor/sdk/protocolclient` rather
than reimplementing the HTTP and SSE layers below. Construct one client with an
injected `TokenSource`, then use `WithSession` for each conversation. The clones
are immutable and safe to use concurrently; token discovery and reconnect
backoff remain application policy.

```go
client, err := protocolclient.New(protocolclient.Connection{
    BaseURL: "http://127.0.0.1:18080",
    Token: protocolclient.StaticToken(token),
    Identity: protocolclient.IdentityScope{
        Tenant: "dev", User: "dev", Session: "conversation-1",
    },
})
if err != nil {
    return err
}

info, err := client.RuntimeInfo(ctx) // validates Protocol-major compatibility
stream, err := client.Subscribe(ctx, protocolclient.StreamOptions{
    LastEventID: lastSeenSequence,
})
if err != nil {
    return err
}
defer stream.Close()
```

The client resolves the token for every request, so a refreshing `TokenSource`
can replace an expired credential without rebuilding session clones. REST
failures are `*protocolclient.Error` values carrying both HTTP `Status` and the
canonical Protocol `Code`. `EventStream.Close` cancels and joins its reader;
callers own reconnect timing and pass the last frame's `ID` back as
`StreamOptions.LastEventID`.

The Protocol is what makes Harbor headless. The Runtime never imports Console code; the Console never reads internal Runtime objects. Your UI sits in the same posture as the Console.

## 1. The wire — base URL, auth, identity

This same wire surface is served by any production Harbor Protocol server — the stock `harbor serve` binary **or** an external Go binary built with `harbor scaffold --with-server` (which reaches the Protocol through the public `sdk/server` facade; see [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md)). Both enforce the identical JWKS auth posture below, so a client written against one drives the other unchanged.

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
- **Admin control surfaces** are family-prefixed and gate on the verified `admin` scope claim (a non-admin caller gets `403 {"code": "scope_mismatch"}`). The governance tenant-default LLM overrides are here: `POST /v1/governance/set_tenant_overrides` sets a tenant's default model / additive extra-instructions / temperature / max-tokens / reasoning-effort live (no redeploy; applied to every session's next run), and `POST /v1/governance/get_tenant_overrides` reads them back. `POST /v1/governance/rotate_key` rotates the LLM provider API key live (no redeploy; the swap is **immediate** — the next call uses the new key). The new key is a **secret**: send it on the request body only; the response + the `governance.key_rotated` event carry only a `sha256:` fingerprint, never the key. (`governance.posture` / `llm.posture` remain read-only posture methods.) The tenant default composes UNDER a per-session next-turn override — `POST /v1/runs/set_overrides` records a one-shot override (reasoning-effort / temperature / max-tokens / `system_prompt_override` (a full prompt **replace**) / `model` (the session model swap)) for the caller's own session; the effective per-run value resolves **session › per-agent › tenant-wide baseline › config** (the session override wins, then the per-agent agent-config LLM-params layer below, then the tenant-wide baseline; the session slot is consumed once on the next run). The **agent-config control plane** is the admin family `POST /v1/agent_config/*`: it versions an agent's configuration as immutable, content-addressed revisions where the active config is a pointer to a revision — `set_revision` writes a new revision and advances the pointer, `list_revisions` returns the chain newest-first, `diff` compares two revisions, `rollback` repoints to an existing revision (never mutating it), and `get` reads the active one. Skills control is the first consumer: `POST /v1/agent_config/skills/upsert` / `skills/delete` / `skills/list` manage an agent's skills, recording each membership change as a config revision (so skills inherit diff + rollback) and emitting `agent.config.revised` / `agent.config.reverted`. `POST /v1/agent_config/set_prompt_layers` sets the agent's **layered system prompt** — an operator-owned `base` layer plus an optional `user` layer that composes *above* the base without replacing it (the composition order is the trust boundary; a `user`-only writer can extend guidance but never weaken the operator base). It replaces only the prompt-layer section (skills + tool exposure are preserved) and records a revision; at run start the durable base/user resolve under the per-session `system_prompt_override` (which replaces the whole base+user spine for one message) and above the additive `extra_instructions`. `POST /v1/agent_config/set_llm_params` pins the agent's **per-agent LLM parameters** (`model` / `temperature` / `max_tokens` / `reasoning_effort`) as a versioned section — a set `model` is validated against the configured `ModelProfiles` at set time (an unknown model is rejected, never persisted). It replaces only the LLM-params section (the prompt-layer + skills + tool-exposure + connection + hooks sections are preserved) and overrides the tenant-wide baseline per field for the agent's next run (resolution **session › per-agent › tenant-wide baseline › config**). `POST /v1/agent_config/set_tool_exposure` sets the agent's **MCP-exposure / per-tool policy**: `paused_servers` (MCP source ids excluded next-run, live transport stays warm), `disabled_tools` (individually-excluded tools), and the runtime **loading-mode overrides** — `server_loading_modes` (per MCP source id, applies to TOOL-form descriptors only) and `tool_loading_modes` (exact per-tool name, unconditional), each valued `always`/`deferred`; an unknown value is rejected `400 {"code": "invalid_request"}` **before** any revision is recorded. A `deferred` override hides the tool from the next run's prompt-time catalog while it stays `tool_search`-discoverable and dispatch-callable (the two-turn discovery cycle is untouched); a paused server / disabled tool stays strictly stronger — hidden from BOTH the prompt and dispatch. Precedence: per-tool override > per-server override > the boot `tools.entries[].loading_mode` > the driver default. It replaces only the tool-exposure section (skills + prompt layers + connections + LLM params + hooks are preserved). The agent's **MCP connection set** is managed by `POST /v1/agent_config/add_mcp_connection` (register a runtime-added MCP server as a new revision) and `POST /v1/agent_config/remove_mcp_connection` (drop a runtime-added descriptor, pruning that server's tool-exposure residue atomically and detaching it at the next run-start reconcile; a boot-declared yaml server or an unknown name is refused loud) — both ride the same revision machinery (diff / rollback / next-turn projection). `POST /v1/agent_config/set_mcp_discovery_origins` (`{agent_id, name, allowed_origins[]}`, FULL-REPLACE) writes a runtime-added **http** connection's OAuth-discovery cross-origin allow-list — the origins the discovery walker may fetch authorization-server metadata from: it records a revision (carrying every sibling section forward) AND applies the allow-list to the live MCP registry so the very next discovery uses it; a revoke prunes the recorded requirement's now-unallowed authorization-server entries, and a rollback revokes the origin live through the owner-scoped run-start reconcile. Every origin runs the shared validator (https `scheme://host[:port]`, no path/IP-literal), and the allowance is never an SSRF hole — a granted origin resolving private/loopback is still refused at dial. An unknown name, a boot-declared (yaml) name, and a stdio connection each fail loud with a distinct typed error. The agent's **Protocol-installed OAuth providers** are managed by `POST /v1/agent_config/set_oauth_provider` (install/upsert) and `POST /v1/agent_config/remove_oauth_provider` (uninstall). The install descriptor is **zero-URL** — `{name, driver: "tokenexchange", credential_source: "remote", credential_broker, scopes?}` — because no admin-writable field may determine where a credential is sent (D-300): the token endpoint, the credential-pull endpoint, the allowed downstream hosts, the audience, and the scope ceiling are all pinned at boot on the named `credential_broker` (a `tools.oauth_credential_brokers[]` entry). A write carrying `token_url` / `auth_url` / `client_id_env` / `client_secret_env` / `remote` is rejected `400 {"code": "invalid_request"}` **by name** (the field is not on the wire struct); an empty `credential_source`, an unknown broker, and a name colliding with a boot-declared provider each fail loud too. The install records a revision AND installs the provider live (so a connection added moments later can bind it via its `oauth_provider` field with no restart); uninstall records a revision AND CLOSES the provider, so a still-bound connection's next call fails loud rather than degrading to an unauthenticated dial (deliberately breaking; confined to the owning agent by the owner-scoped run-start reconcile AND by a store-boundary owner check that refuses a cross-owner drop — defense in depth, so the store enforces owner-scoping independently of caller-side owner resolution). The versioned `hooks` section (the run-completion hook's durable home — `{run_completion: {tool, timeout_ms}}`) rides the same machinery with no dedicated verb: set it via `set_revision` (a negative `timeout_ms` is rejected at set time), read it on `get`, and `diff`/`rollback` cover it like every other section; it resolves above the yaml `runtime.hooks` default at the agent's next run. The versioned `naming` section (session auto-naming's durable home — `{auto, after_turns, repeat_every, max_repetitions, max_title_len, model}`) likewise rides the same machinery with no dedicated verb: set it via `set_revision` (an invalid policy — a negative bound, an out-of-range `max_title_len`, or `repeat_every > 0` with `max_repetitions < 1` since no unlimited value exists — is rejected `400 {"code": "invalid_request"}` at set time), read it on `get`, and `diff`/`rollback` cover it; it resolves above the yaml `runtime.naming` default at the agent's next run — a PRESENT section is authoritative either way, so a bare `{"naming": {"auto": false}}` revision is an explicit per-agent opt-out that wins over a yaml-on fleet default — and, when active, titles the session at each run's terminal boundary (a failure emits `session.naming_failed` and never alters the run). Every edit applies on the agent's **next run** (next-turn projection — never mid-flight); an upsert that would overwrite a pack-origin skill is refused with `400 {"code": "invalid_request"}`, never silently.

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

### 2a. Retention horizons — how far back this Runtime actually holds data

`runtime.health` (POST `/v1/control/runtime.health`) carries, alongside the per-subsystem readiness rollup, an additive `retention[]` block: one **observed** oldest-retained instant per durable surface (`events`, `tasks`, `sessions`). Read it before a windowed history/enumeration read so a merged "last N days" fleet view can mark "this runtime retains only back to X" instead of implying a complete window.

Each entry carries a **`scope`** marker — `runtime` (identity-free, the whole retained set), `tenant`, or `session` — that makes an absent timestamp representable:

- `scope:"runtime"` + no `oldest_retained_at` ⇒ a *trustworthy empty* — the runtime genuinely retains nothing on that surface.
- `scope:"session"` / `scope:"tenant"` + no `oldest_retained_at` ⇒ the runtime-wide truth is simply *not observable at your scope*; do not read it as empty. Mark the window's completeness unverifiable.

The `events` horizon is always `scope:"runtime"`. For an ordinary caller the `tasks` horizon is `scope:"session"` and the `sessions` horizon is `scope:"tenant"`. A caller carrying a verified `admin` **or** `console:fleet` scope (derived server-side from the session, never the request body) reads the `tasks` and `sessions` horizons at `scope:"runtime"` too — the fleet-observe path a `svc:` coordinator uses to observe all three. That widened read emits one `audit.admin_scope_used` per request. No new method or capability — it rides `runtime.health`.

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

**Background-wake notifications (`notification.*`).** The runtime also
synthesises an operator-facing `notification.*` family onto the same
`events.subscribe` stream (no separate method) — the conversational mirror of
background work you'd otherwise miss. Each carries a human-readable
`Summary` your UI renders inline (fall back to the bare event type when
absent). The wake classes:

- `notification.task_completed` — a background task that opted in with
  `NotifyOnComplete` finished.
- `notification.task_group_resolved` — a parallel task group resolved
  (with ref-shaped member-outcome counts).
- `notification.task_group_cancelled` — a parallel task group was cancelled
  **without the operator asking** (a fail-fast gate firing on a member
  failure, or a cascade inherited from an ancestor cancel). This closes the
  sibling asymmetry the resolved-group mirror opened: a batch's winners wake
  the conversation while its unprompted-cancelled losers would otherwise
  vanish silently. **Suppression rule:** a group cancel the operator drove
  DIRECTLY produces NO notification (they already know) — the runtime keys
  this on the cancel's typed origin, so you only ever receive this event for
  an unprompted cancel worth surfacing.
- `notification.task_failed` — a background task failed (suppressed for a
  foreground turn's own failure, which the client surfaces itself).

These are additive event classes on the existing stream; a client that
doesn't recognise a `notification.*` type can ignore it. `ProtocolVersion`
is unchanged.

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

**Cross-session / time-ranged historical read — `events.list`.** `state.history` is a **by-id** read (one session). When you want the raw events across a **time window** — possibly across sessions, for a fleet observability view — use `events.list` (`POST /v1/events/list`): the same wire `EventFilter` you pass to `events.subscribe`/`events.aggregate` (identity axes + `since`/`until` + `event_types`) plus the SAME tail-first paging grammar as `state.history`:

```http
POST /v1/events/list
Authorization: Bearer <JWT>
Content-Type: application/json

{ "filter": { "since": "2026-07-04T00:00:00Z" }, "cursor": 0, "limit": 50 }
```

Rows are the **same flat `StateEvent` shape** `state.history` returns (heavy payloads by routable `StateArtifactRef`, never inline); `cursor: 0` reads from the tail and you scroll one page older by passing the response's `next_cursor` back as `cursor` (0 ⇒ the retained head). `truncated: true` is the honest retention-gap flag at the window edge (a best-effort in-memory ring evicted older rows; a `durable` driver serves complete windows). By default the read is scoped to your own verified triple; a **cross-tenant (fleet) widening** — a `filter.tenant_ids` naming a tenant other than yours — requires the verified `admin` **or** `console:fleet` scope (derived server-side from your session, never the body) and emits one `audit.admin_scope_used` per request. Same authz as the live/aggregate feeds, so a `console:fleet` operator reads historically exactly what it can subscribe to.

**Bucketed counts — `events.aggregate`.** For a time-bucketed count series (the Events-page sparkline) rather than the raw rows, `POST /v1/events/aggregate` takes the same `EventFilter` plus a `window` and a `bucket` (nanoseconds; `window % bucket == 0`) and returns `{ "buckets": [ { "bucket_start": "...", "bucket_end": "...", "counts": { "tool.failed": 3 } }, ... ], "truncated": false, "protocol_version": "0.1.0" }`. It works on EVERY driver — including the `durable` log — with the same authz as `events.list` (a cross-tenant filter needs the verified `admin`/`console:fleet` scope, one `audit.admin_scope_used` per widened call). A window too wide to count in full comes back as **partial buckets with `truncated: true`** — uniformly across drivers, DATA not an error; a driver difference changes WHAT the method returns, never WHETHER it works, so there is no over-wide-window 400.

By default the grid is anchored at "now" — the buckets run `[now-window, now)`, so a `bucket_start` is NOT addressable twice (two polls a few seconds apart return two different boundary sets and nothing caches). Pass the OPTIONAL `anchor` (RFC-3339 UTC) to floor the boundaries onto the fixed grid `anchor + k*bucket` instead: two calls at two instants with the same `anchor` + `window` + `bucket` then share bucket coordinates, so a bucket is cacheable and re-requestable — pass the Unix epoch (`"1970-01-01T00:00:00Z"`) for a globally-shared grid identical across a runtime restart and across two fleet runtimes. One grid-edge caveat: with an anchor the LAST bucket's `bucket_end` is the grid boundary covering "now" (generally just AFTER now, by up to one bucket), so don't render it as "up to this instant." The field is additive — omit it and you get the byte-identical clock-anchored series.

Widening semantics (same for `events.aggregate` and `events.list`): you WIDEN by NAMING `filter.tenant_ids` (or another principal — a foreign/multi user, a multi-session set). A widened read fans in across ALL users and sessions of the named tenant scope — the elided user/session axes legitimately mean "all" within the tenant(s) you authorized, so a fleet read of `{"filter":{"tenant_ids":["t2"]}}` returns every principal's counts in `t2`, not just your own. An ELIDED `tenant_ids` always stays YOUR OWN tenant, even for an admin (name-to-widen, matching `tasks.list` / `agents.list`) — you never silently fan across every tenant. Every widened read is gated on the verified `admin`/`console:fleet` scope (derived from your session, never the body) and emits one `audit.admin_scope_used`.

**Per-tenant attribution — `by_tenant`.** An aggregate bucket is a bag of scalars (`{"counts": {"tool.failed": 3}}`) with NO tenant attribution, so — unlike a raw `events.list` row that carries its own tenant — you cannot verify a widened count against the `tenant_ids` you asked for; the runtime's honouring of the filter IS the whole tenant boundary. Set the OPTIONAL `by_tenant: true` on the `events.aggregate` body to get per-bucket attribution: each bucket gains a `counts_by_tenant` map (tenant → event-type → count) alongside `counts`, e.g. `{ "counts": {"tool.failed": 3}, "counts_by_tenant": {"t-a": {"tool.failed": 2}, "t-b": {"tool.failed": 1}} }`. It is returned ONLY on an admin-widened read (the same verified `admin`/`console:fleet` scope, derived server-side); on any other read the flag is IGNORED and `counts_by_tenant` is absent — the flag never elevates a read and never widens what is counted (fail-closed). The keys are a subset of the tenants you named, and per bucket `Σ_tenant counts_by_tenant[tenant][type] == counts[type]`, so you can independently reconcile the breakdown against the totals and confirm no tenant outside your entitled set appeared. Additive — omit `by_tenant` and the response is byte-identical to before. `ProtocolVersion` stays `0.1.0`.

**Fleet enumeration (admin-widened `tasks.list` / `agents.list`).** By default both list methods project only your own `(tenant, user, session)` triple — a synthetic observer session sees nothing. A coordinator control plane rendering a fleet-wide Tasks board / Agents catalog widens the read with an additive `filter.tenant_ids` selector: `POST /v1/tasks/list` (or `/v1/agents/list`) with `{"filter": {"tenant_ids": ["tenant-a", "tenant-b"]}}` enumerates every task / agent across ALL sessions of the named tenants. Widening rides the SAME verified `admin` scope claim (no new "fleet" scope) — a `tenant_ids` request without it fails LOUD with `403 {"code": "scope_mismatch"}`, never a silent narrowing to own scope. Every widened row carries full per-`identity` `{tenant,user,session}` attribution, and every widened call emits an `audit.admin_scope_used` event. Cross-RUNTIME federation stays coordinator-side over these per-runtime reads (the same division as `sessions.list` / `events.subscribe`). One `agents.list`-specific row to know about: every actively-serving runtime returns its **default agent** — the boot-configured agent it serves through, never registered as a fleet entity — as a first-class row marked `is_default: true` (well-known `agent_id`, `agents.get` resolves it, the `Active` metric counts it). On a narrow read it carries your own triple; on a widened read one such row appears per named tenant, attributed by tenant only. So a fleet catalog reads "one agent, not enumerable this way" instead of an empty page — but there is NO control surface over it: a `agents.pause`/`.drain`/etc. against the well-known id fails `404` (`agent_not_found`), because the runtime's own process is not a fleet-controllable member. A real registration reusing the well-known id suppresses the synthetic row (real data wins; never a duplicate id).

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

## 4c. Naming a session — `sessions.set_title`

Sessions display as raw ids until you give one a human-readable **title**: `sessions.set_title` sets (non-empty `title`) or clears (empty `title`) the `Title` field on the session record (D-288). Unlike `sessions.delete`, the write scope is your whole verified `(tenant, user)`, not just your own connecting session — `session_id` is a **dedicated field** that may name a **sibling** session you own, so a Console-style "rename any of my sessions from the list" flow needs no elevation. The route is `POST /v1/sessions/set_title`:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/sessions/set_title" \
  -H "Authorization: Bearer $TOKEN" -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"tenant": "dev", "user": "dev", "session": "'$SESSION'"}, "session_id": "'$SESSION'", "title": "Q3 onboarding chat"}'
# → 200 {"session_id":"...","title":"Q3 onboarding chat","title_source":"manual"}
```

The verb **always** writes `title_source: "manual"` — `auto` provenance is not expressible over the wire, so a title you set here can never be silently overwritten by a later auto-namer (that's the internal-only producer behind Phase 158). An empty `title` clears both fields back to unset. Titles are single-line and bounded to 200 runes: a title with a newline/control character, or over the limit, is rejected `invalid_request`/400 — **never a silent clamp**. Rules: identity is mandatory (`identity_required`/401, same body-identity-mismatch check as `sessions.delete`); a `session_id` you don't own (wrong tenant/user, or simply unknown) is `not_found`/404 — existence is never revealed across identities; renaming a **closed** session is fine (it's metadata on a historical conversation), renaming an **erased** one is `not_found` (the record is gone). The title itself never rides an event, log, or audit payload — `session.title_changed` carries only `{session_id, source}`, so a subscriber that wants the text re-reads `sessions.list` / `sessions.inspect`, both of which now project `title` / `title_source` on every row.

**Session counters are TRUTHFUL, and honest about their own limits (D-309).** Every `sessions.list` / `sessions.inspect` row carries six per-session counters populated at the runtime source: `tasks_count`, `events_count`, `total_cost_cents`, `total_tokens`, `has_pending_intervention`, `has_failed_task`. So `filter.cost_above_cents`, `filter.has_failed_task`, `filter.has_intervention`, and `sort: "cost_desc"` narrow / order on REAL data — "show my sessions over $5" returns the expensive ones, not an empty page. When a session's per-scan bound is hit, the row carries `counters_partial: true`: its cost / tokens / events counts are then an **honest lower bound** (render them with a "≥"), and a `cost_desc` ordering over such a row is non-authoritative — never a silent undercount. The two agent fields take the opposite tack: there is **no single-valued session→agent binding** in V1 (a session may run several agents), so `agent_id` / `agent_name` are **nullable and omitted** rather than a fabricated value, and `filter.agent_ids` fails **loud** with `invalid_request`/400 rather than returning a believable-but-false empty page. The multi-field `query` search is never failed whole for touching the empty agent fields — it still matches `session_id` / `user_id`, so id / user search keeps working.

**The same "no facet over an unpopulated field" rule now holds across `tasks` / `flows` / `memory` / `tools`, mechanically (D-313, extends D-309).** A build-time projection-completeness gate (`internal/protocol/projectioncheck`) fails the build if any read surface ships a filter / sort / aggregate over a wire field its projector never assigns — so the false-absence bug can't be reintroduced. What this means on the wire:

- **`tasks.list`** — `has_pending_approval` is populated from the pause/approval registry, so `filter.has_pending_approval=true` narrows to tasks actually blocked on a HITL gate (not an empty page). `background_acknowledged` is `omitempty` (elided when false — never a fabricated known-false).
- **`flows.list` / `flows.get`** — `budget_consumption.tokens_used` is summed per run (symmetric with `cost_usd_used`), truthful wherever a run is recorded.
- **`memory.list` / `memory.health`** — the always-empty `has_ttl_expiring` facet and the two `expiring_in_1h` aggregate fields are **removed** from the wire (V1 memory has no TTL); `filter.agent_ids` loud-rejects with `invalid_request`/400 (a V1 record carries no producer identity), never a false-empty page.
- **`tools.list` / `tools.metrics` / `tools.content_stats`** — a runtime that advertises the `tool_annotations` capability (negotiate via `Accepts(tool_annotations)`) serves REAL per-tool annotations: `filter.oauth_statuses` / `filter.approval_policies` narrow to real rows, the annotator-backed aggregates (`active` / `pending_approval` / `awaiting_oauth`) carry real counts (no `aggregates_partial`), `tools.metrics` returns real error-rate gauges + invocation/failure counts over the window, and `tools.content_stats` returns a real result-size histogram (D-314). The admin `tools.set_approval_policy` / `tools.revoke_oauth` methods persist through `tools/approval` / `tools/auth` with audit (they no longer return `admin_unsupported`). A runtime that does NOT advertise `tool_annotations` (a headless catalog stack) loud-rejects `filter.oauth_statuses` / `filter.approval_policies` with `invalid_request` and returns `aggregates_partial: true` with those counters zeroed — render them "unavailable," never a real-looking 0; only `aggregates.total` is authoritative in that state.

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
- **Import the curated Go client facade** — Go applications import `github.com/hurtener/Harbor/sdk/protocolclient`. It exposes only the supported typed methods, generic JSON call core, typed errors, and SSE stream; it does not wholesale re-export Runtime internals or every Protocol method.

Hand-rolling the types from scratch is fine for a quick prototype but you'll drift. Anchor any client you intend to maintain on the generated module or the generated reference.

### 8a. Reading a downstream scope shortfall

When an MCP-backed tool call is refused downstream with a `403` + `WWW-Authenticate: Bearer error="insufficient_scope"` (RFC 6750 §3.1), the shortfall surfaces as **structured data on the two surfaces you already read** — never an opaque error string:

- **On the tool-result error path** — the `tool.failed` event's `ScopeShortfall` field carries `downstream_resource`, `required_scopes`, `granted_scopes`, the verbatim `www_authenticate` header, and `origin`. It is set only when the challenge is explicitly marked `insufficient_scope`; an unmarked 403 stays an opaque failure. The runtime classifies the shortfall as **permanent** (it never retries a shortfall retrying cannot fix).
- **On the MCP connection view** — `mcp.servers.get` returns `MCPServerView.last_scope_shortfall` (`MCPScopeShortfallView`) recording the last observed shortfall on that connection — visible even to a reader who never made the offending call. It rides the DETAIL read only (like `oauth_requirement`), not the hot list row.

Both are **report-only**: the runtime never auto-escalates, re-consents, or widens a binding on a shortfall. The operator acts on it via the boot-declared `oauth_provider` / `tool_oauth_providers` bindings (which bind a distinct provider per MCP-side entry — a tool call by tool name, a resource read / subscribe by resource URI, and a prompt get by prompt name — for a server fronting several downstream audiences).

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
- The public Go client: `sdk/protocolclient`.
- The docs generator: `cmd/harbor-gen-protocol-docs/` (D-209). The external-client TS wire-type generator: `cmd/harbor-protocol-ts-types/` (D-269) → `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts`. The FULL Console-`protocol.ts` TS-client generator remains deferred (D-132 / issue #179, name `cmd/harbor-gen-protocol-ts` reserved); `protocol.ts` is hand-maintained.
- RFC §5 — Harbor Protocol design.
