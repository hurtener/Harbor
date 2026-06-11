# Speak Protocol in 15 minutes

Pure `curl` against a local `harbor dev` server: mint a dev token, start a run,
watch the planner think on the live event stream, read the result, then steer.
Every request and response below is real — **this page is executed**: the
project's preflight gate extracts the tagged command blocks below and runs
them, in order, against a live dev server on every commit
(`scripts/smoke/phase-113a.sh`). If the page and the wire ever disagree, the
build fails.

## Before you start

Boot a Runtime and point `HARBOR_BASE_URL` at it:

```bash
harbor dev          # boots on 127.0.0.1:18080 by default
export HARBOR_BASE_URL="http://127.0.0.1:18080"
```

You need `curl` and `jq`. Everything else comes from the wire.

## Step 1 — bootstrap a dev token

`harbor dev` (and `harbor console`) mount a **loopback-only** bootstrap
endpoint that mints a fresh dev JWT and returns the full connection envelope.
It is never mounted by a production `harbor serve`.

<!-- qs-step: 1-bootstrap -->

```bash
BOOTSTRAP="$(curl -sS -X POST -d '{}' "$HARBOR_BASE_URL/v1/dev/bootstrap.json")"
TOKEN="$(printf '%s' "$BOOTSTRAP" | jq -r .token)"
printf '%s\n' "$BOOTSTRAP" | jq 'del(.token)'
```

The response (token elided):

```json
{
  "base_url": "http://127.0.0.1:18080",
  "identity": { "tenant": "dev", "user": "dev", "session": "dev" },
  "scopes": ["admin", "console:fleet"],
  "protocol_version": "0.1.0"
}
```

The token authenticates **who you are** — `(tenant, user)` plus scopes. It does
NOT pin which conversation you are in: the session travels per-request in the
`X-Harbor-Session` header, and a new session id is a new conversation,
materialised on first use. (The full contract is the
[Auth & identity choreography](./auth-and-identity.md).)

## Step 2 — start a run

`start` spawns a task. Pick a session id for this conversation and send the
query. The body's `identity` can stay empty — the Runtime folds it from the
verified token + your session header, and rejects any body identity that
disagrees.

<!-- qs-step: 2-start -->

```bash
SESSION="quickstart-demo"
START="$(curl -sS -X POST "$HARBOR_BASE_URL/v1/control/start" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "query": "Say hello, then stop."}')"
TASK_ID="$(printf '%s' "$START" | jq -r .task_id)"
printf '%s\n' "$START" | jq .
```

```json
{
  "task_id": "01KTSWDBCXF2WZ4Z8YPZYF3A8C",
  "reused": false,
  "protocol_version": "0.1.0"
}
```

That `task_id` is the run's handle for everything that follows. (Send an
`idempotency_key` if you need spawn dedup — see
[`StartRequest`](./types.md#startrequest).)

## Step 3 — tail the event stream and watch the planner think

Everything the Runtime does is narrated on one typed event bus, exposed as
Server-Sent Events at `GET /v1/events`, server-filtered to your identity. The
`Last-Event-ID: 0` header replays the session's history from the ring buffer
before live-tailing, so you see the run you just started even if it already
finished. We bound the tail to five seconds for the tour — a real client keeps
the stream open.

<!-- qs-step: 3-events -->

```bash
EVENTS="$(curl -s -N --max-time 5 \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Last-Event-ID: 0" \
  "$HARBOR_BASE_URL/v1/events" || true)"
printf '%s\n' "$EVENTS" | grep '^event:' | sort | uniq -c | sort -rn
```

A short run's stream, counted by event type:

```text
   7 event: llm.completion.chunk
   1 event: task.spawned
   1 event: task.started
   1 event: task.completed
   1 event: session.opened
   1 event: planner.decision
```

Each frame is `event:` (the type), `id:` (the per-bus sequence — your reconnect
cursor), and a `data:` JSON object. Here is a real `task.spawned` frame:

```text
event: task.spawned
id: 6
data: {"type":"task.spawned","sequence":6,"occurred_at":"2026-06-10T23:03:22.781100000Z","tenant":"dev","user":"dev","session":"quickstart-demo","payload":{"TaskID":"01KTSWDBCXF2WZ4Z8YPZYF3A8C","Kind":"foreground","ParentTaskID":"","Priority":0,"IdempotencyKey":""}}
```

Note the payload keys are the Go field names (`TaskID`, capital `T`) — the
[events reference](./events.md) catalogues every type's exact shape. The
`llm.completion.chunk` frames are the token stream a chat UI renders live;
`planner.decision` is the planner's reasoning channel.

## Step 4 — read the result

The stream narrated the run; `tasks.get` snapshots it.

<!-- qs-step: 4-tasks-get -->

```bash
TASK="$(curl -sS -X POST "$HARBOR_BASE_URL/v1/tasks/get" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d "{\"identity\": {}, \"id\": \"$TASK_ID\"}")"
printf '%s\n' "$TASK" | jq '.task | {id, status, query}'
```

```json
{
  "id": "01KTSWDBCXF2WZ4Z8YPZYF3A8C",
  "status": "complete",
  "query": "Say hello, then stop."
}
```

The full [`TaskDetail`](./types.md#taskdetail) also carries the cost rollup,
the parent session, and the reasoning trajectory (by reference — heavy content
never travels inline).

## Step 5 — steer

Nine steering controls share one route shape (`POST /v1/control/{method}`) and
one wire shape ([`ControlRequest`](./types.md#controlrequest)): `cancel`,
`pause`, `resume`, `redirect`, `inject_context`, `approve`, `reject`,
`prioritize`, `user_message`. A control targets a **live run**: the `run` field
carries the task id, and `scope` is your steering claim
(see [task control](./task-control.md)).

<!-- qs-step: 5-cancel -->

```bash
CANCEL="$(curl -sS -w '\n%{http_code}' -X POST "$HARBOR_BASE_URL/v1/control/cancel" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d "{\"identity\": {\"run\": \"$TASK_ID\", \"scope\": \"owner_user\"}}")"
CANCEL_STATUS="${CANCEL##*$'\n'}"
printf '%s\n' "${CANCEL%$'\n'*}"
echo "HTTP $CANCEL_STATUS"
```

Two outcomes are possible, and both teach the same lesson — steering is
asynchronous and targets in-flight work:

- The run is still live → `200` with the acknowledgement envelope
  (`{"accepted": true, "method": "cancel", ...}`). The cancel's *effect* shows
  up on the event stream (`control.received` → `task.cancelled`), never
  synchronously in the response.
- The run already reached a terminal state (our short demo run finished in
  step 3) → `404` with the canonical error envelope:

```json
{ "code": "not_found", "message": "method \"cancel\": no live run for the requested run id" }
```

Every error on the wire is that one envelope — branch on `code`, never on
`message` ([errors reference](./errors.md)).

## Where to next — three doors

1. **The generated reference** — every [method](./methods.md),
   [event](./events.md), [error](./errors.md), and [wire type](./types.md),
   generated from the Runtime's own sources and drift-gated in CI.
2. **The choreographies** — [auth & identity](./auth-and-identity.md),
   [streaming semantics](./streaming-semantics.md),
   [task control](./task-control.md), [the pause model](./pause-model.md),
   and [versioning & compatibility](./versioning-and-compatibility.md):
   sequence and intent, not just shapes.
3. **[Build a client](./build-a-client.md)** — a complete ~100-line
   event-viewer client, worked line by line, with the
   [conformance-certification path](./conformance-certification.md) as the
   closer. The bundled Console is the reference full client, and the
   [`use-the-harbor-protocol`](../skills/use-the-harbor-protocol/SKILL.md)
   skill is the operator-side recipe.
