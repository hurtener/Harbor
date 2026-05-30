# Session model — Console ↔ Runtime integration contract (D-171)

> Status: implemented (runtime side). This note is the precise contract
> the Console implements against. It supersedes the prior model where
> `harbor dev` minted one token whose `session` claim was hardcoded to
> `"dev"` and every conversation wrote to that one session.

## The corrected model in one paragraph

The **connection token is a per-backend credential, like an API key**.
It authenticates the Console ↔ Runtime connection and carries
`tenant + user + scopes`. It does **not** pin a single session. The
**session is dynamic — chosen per-conversation by the client and supplied
per-request** via the `X-Harbor-Session` header. The runtime derives
`tenant`+`user` from the verified token and takes `session` from the
request. A `start` (or first write) on a not-yet-existing session id
**creates the session on first use**, under the token's `(tenant, user)`.
Many sessions coexist under one connection, fully isolated. Listing and
reloading past sessions works, including across a runtime restart against
the same state dir.

The multi-isolation triple `(tenant, user, session)` stays **mandatory
and enforced** end to end (CLAUDE.md §6). The only thing that changed is
the **source** of `session`: per-request header, scoped under the token's
verified `(tenant, user)` — never widened by the client.

## How the Console connects (the token)

The Console obtains a connection token. For `harbor dev` this is the
dev token printed at boot / returned by `POST /v1/dev/bootstrap.json`.

After D-171, the dev token's identity claims are:

| Claim     | Value   | Role                                              |
| --------- | ------- | ------------------------------------------------- |
| `tenant`  | `dev`   | verified; the connection's tenant                 |
| `user`    | `dev`   | verified; the connection's user                   |
| `session` | `dev`   | **DEFAULT only** — used iff no header is supplied  |
| `scopes`  | `["admin","console:fleet"]` | the connection's scopes          |

The `session` claim is a **back-compat default**, never a hard pin. A
client that always sends `X-Harbor-Session` never touches it.

Every authenticated request carries:

```text
Authorization: Bearer <connection-token>
```

## How the Console picks / creates a session per conversation

The Console assigns each conversation a **session id** (a client-chosen
string — a UUID is recommended; any non-empty string works). It sends
that id on **every request for that conversation**:

```text
X-Harbor-Session: <conversation-session-id>
```

Rules the runtime enforces (auth middleware, `internal/protocol/auth`):

- `X-Harbor-Session` present and non-empty → it **replaces** the token's
  `session` claim. `tenant` + `user` stay token-verified.
- `X-Harbor-Session` absent/empty → the token's `session` claim is used
  as the default.
- Neither a default claim nor a header → **401 `identity_required`** (the
  session is mandatory; identity fails closed — no silent default).
- The header can **only** set `session`. There is no `X-Harbor-Tenant` /
  `X-Harbor-User` override on the authenticated path; a client can never
  widen its tenant or user.

### New conversation = new session id = create-on-first-use

To start a **new** conversation, the Console picks a fresh session id and
issues a `start` with it. The runtime materialises the session row on the
first turn (no explicit "open session" call exists or is needed):

```http
POST /v1/control/start
Authorization: Bearer <connection-token>
X-Harbor-Session: <new-session-id>
Content-Type: application/json

{ "identity": {}, "query": "hello" }
```

- The body's `identity` may be left **empty** — the runtime backfills it
  from the per-request (header-chosen) session + token-verified
  `(tenant, user)`. If the Console does populate `identity`, every
  non-empty component MUST match the resolved request identity (same
  tenant, same user, same `X-Harbor-Session`), or the request is
  rejected `identity_required`. The simplest correct client sends
  `"identity": {}` and relies on the header.
- A `start` on an **already-open** session id is a no-op create (the
  second-and-later turns of a conversation) — it is **not** an error.
- A `start` on a **closed** session id (one that was GC-reaped or
  operator-closed) is rejected `invalid_request` (RFC §6.9:
  reopen-after-close is forbidden). The Console must start a **new
  conversation with a new session id** rather than reviving a closed one.

`start` response (`200`):

```json
{ "task_id": "<task-id>", "reused": false, "protocol_version": "..." }
```

## How the Console lists sessions

```http
POST /v1/sessions/list
Authorization: Bearer <connection-token>
X-Harbor-Session: <any-session-id>     # required to satisfy identity; value is not a filter here
Content-Type: application/json

{ "filter": {}, "limit": 50 }
```

The request body is `SessionsListRequest`
(`internal/protocol/types/sessions.go`). The `identity` field may be left
empty in the body (the wire handler folds it from the verified request
identity); `filter` / `sort` / `cursor` / `limit` are optional.

Response (`200`) is `SessionsListResponse`:

```json
{
  "rows": [
    {
      "session_id": "A",
      "status": "...",
      "user_id": "dev",
      "tenant_id": "dev",
      "started_at": "...",
      "last_activity_at": "...",
      "duration": 0,
      "tasks_count": 0,
      "events_count": 0,
      "...": "..."
    }
  ],
  "next_cursor": "",
  "truncated": false
}
```

- Returns **every session under the connection's `(tenant, user)`** —
  all conversations the connection has created (open and closed; closed
  rows carry a terminal `status`). Paginate by passing the returned
  `next_cursor` back as `cursor` until it is `""`.
- A non-admin connection is scoped to its own `(tenant, user)`. A
  cross-tenant `filter.tenant_ids` requires the `admin` scope (D-079);
  without it the call is rejected `scope_mismatch` (403).
- **Survives restart.** The runtime persists a per-`(tenant, user)`
  session catalog in the StateStore, so a fresh runtime process
  re-discovers the connection's sessions on the first `sessions.list`.
  (SQLite/Postgres state dirs persist; the in-memory driver does not —
  use a durable state driver for cross-restart listing.)

## How the Console reloads a past conversation's turns

Reloading a past conversation is two reads, scoped by the conversation's
session id via the header.

### 1. Session metadata — `sessions.inspect`

```http
POST /v1/sessions/inspect
Authorization: Bearer <connection-token>
X-Harbor-Session: A
Content-Type: application/json

{ "identity": {}, "session_id": "A" }
```

Response (`200`) is `SessionsInspectResponse` carrying the session `Row`
(+ capped recent-interventions / recent-artifacts slices, which the
Console also maintains live from the event stream). A session not visible
to the connection returns `not_found` (404). Works for any of the
connection's sessions — including closed ones — and across a restart
(the catalog re-discovers the row).

### 2. The conversation's tasks/turns — `tasks.list`

`tasks.list` projects the **caller's session** (the `X-Harbor-Session`
value). To reload conversation A's tasks:

```http
POST /v1/tasks/list
Authorization: Bearer <connection-token>
X-Harbor-Session: A
Content-Type: application/json

{ "identity": {}, "filter": {}, "page_size": 50 }
```

Response is `TaskListResponse` with `rows: TaskRow[]`, each carrying the
task's `identity` (tenant/user/session), `status`, `query`, `description`,
timestamps, and counts. The rows are scoped to session A by the header —
no cross-session bleed.

### Live turns — the event stream

For live turn content (streaming assistant output, tool calls, reasoning)
the Console subscribes to the SSE event stream filtered to the session:

```http
GET /v1/events
Authorization: Bearer <connection-token>
X-Harbor-Session: A
Accept: text/event-stream
```

The stream is session-scoped by the same header.

## Honest limitation — task/turn durability across restart (V1)

`sessions.list` and `sessions.inspect` **do** survive a runtime restart
(persistent session catalog + StateStore-backed session records). The
**task registry is in-memory** in V1 and is **not** rehydrated on boot,
so `tasks.list` for a session created in a **prior** process returns an
empty task set after a restart — the session row reloads, but its task
rows do not repopulate from before the restart. Live (post-restart) turns
appear normally. Full task/turn durable rehydration is a separate,
post-D-171 workstream; the Console should treat an empty `tasks.list` for
a pre-restart session as "history not rehydrated," not as "no session."

## Quick reference

| Action               | Method / route              | Identity source                                   |
| -------------------- | --------------------------- | ------------------------------------------------- |
| Authenticate         | `Authorization: Bearer`     | token → `tenant` + `user` + `scopes`              |
| Pick a conversation  | `X-Harbor-Session: <id>`    | per-request session (overrides token claim)       |
| New conversation     | `POST /v1/control/start`    | header session, create-on-first-use               |
| List conversations   | `POST /v1/sessions/list`    | token `(tenant, user)`; survives restart          |
| Reload session meta  | `POST /v1/sessions/inspect` | header session; survives restart                  |
| Reload turns/tasks   | `POST /v1/tasks/list`       | header session (in-memory; empty after restart)   |
| Live turns           | `GET /v1/events` (SSE)      | header session                                    |

## Error contract

The Protocol error `code` is the lowercase snake_case value in the JSON
body (`internal/protocol/errors`); the HTTP status is the transport
mirror.

| Condition                                          | HTTP | Protocol `code`     |
| -------------------------------------------------- | ---- | ------------------- |
| No bearer / malformed                              | 401  | `identity_required` |
| Token verified but no resolvable session           | 401  | `identity_required` |
| Body identity disagrees with the request identity  | 401  | `identity_required` |
| `start` on a closed session id                     | 400  | `invalid_request`   |
| Cross-tenant `sessions.list` without `admin`       | 403  | `scope_mismatch`    |
| `sessions.inspect` on an unknown/unscoped session  | 404  | `not_found`         |
