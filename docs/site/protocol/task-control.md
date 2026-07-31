# Choreography 3 — Task control

The run lifecycle as the wire sees it: spawn, observe, steer, terminal state.

> Methods demonstrated: `start`, `cancel`, `pause`, `resume`, `redirect`,
> `inject_context`, `approve`, `reject`, `prioritize`, `user_message`,
> `tasks.list`, `tasks.get`, `pause.list`

## The lifecycle, end to end

```text
POST /v1/control/start            -->  {"task_id": "...", "reused": false}
        |
        v        (everything below is narrated on GET /v1/events)
task.spawned -> task.started -> planner.decision / llm.completion.chunk /
tool.invoked ... -> task.completed | task.failed | task.cancelled
        |
        v
POST /v1/tasks/get                -->  the TaskDetail snapshot
```

Three surfaces, three roles:

- **`start`** spawns the work and returns immediately with the task handle.
- **The event stream** narrates the run live
  ([streaming semantics](./streaming-semantics.md)).
- **`tasks.list` / `tasks.get`** snapshot what the stream narrated — the
  catch-up reads for a client that just attached.

The task id doubles as the **run id**: it is the value a steering control's
`identity.run` field targets.

## start

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/start" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "query": "Summarise the quarterly report.", "idempotency_key": "turn-42"}'
```

[`StartRequest`](./types.md#startrequest) options worth knowing:

- `idempotency_key` — a second `start` with the same key returns the existing
  task with `"reused": true` instead of spawning twice. The key is scoped to
  your full identity triple (tenant, user, session), so it can only ever match
  a spawn you made yourself.
- `input_artifact_ids` — attach uploaded artifacts (`artifacts.put` first) as
  multimodal inputs; bytes never travel on this request.
- `input_artifact_dispositions` — optional per-attachment disposition hints
  keyed by artifact id (`ref` | `inline` | `provider_native` | `tool:<name>`;
  Phase 84b / D-189). The hint outranks the agent's `multimodal.disposition`
  config map and the runtime default; `tasks.get` reflects it on
  `input_artifacts[].disposition`.
- `priority` — the initial scheduling priority (changeable later via
  `prioritize`).
- `agent_id` — name which agent's **configuration** the run executes under
  (D-360). Omitting it binds the runtime's configured default exactly as
  before. See "Naming an agent" below.

A `start` on a fresh session id **creates the session**; on a closed session
it is rejected `invalid_request` ([auth & identity](./auth-and-identity.md)).

## Naming an agent

By default a run executes under the runtime's configured default agent. Pass
`agent_id` to run under a different agent's configuration — its prompt
layers, tool exposure, skills membership, LLM parameter overrides, completion
hook and naming policy:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/start" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "query": "Summarise the quarterly report.", "agent_id": "reporting-agent"}'
```

**The two-check rule.** A named agent is accepted when EITHER its id equals
the runtime's configured default agent id, OR a config revision exists for
your tenant under that id (write one with `agent_config.set_revision`).
Anything else is refused `invalid_request` **before the task is created** —
never silently replaced with the default. A caller that named agent A,
silently got agent B, and was told it succeeded is the failure this rule
exists to prevent, so there is no fallback path.

The refusal is deliberately uninformative: an agent id belonging to a
different tenant and an id that never existed produce the identical error, so
the surface cannot be probed for cross-tenant existence.

**It selects configuration only.** The run's southbound tool provenance
(`_meta.agent_id` on outbound MCP calls) and its RFC 8693 acting principal
(`actor_token`) remain the runtime's boot-derived value and are never
influenced by this field — the acting principal is the runtime's verified
identity, never a client-supplied one.

`tasks.get` / `tasks.list` reflect the value on `task.agent_id`. An **absent**
`agent_id` on a task row means the caller named none and the run bound the
runtime's configured default — "defaulted", not "unknown".

## The nine steering controls

All nine share one route shape — `POST /v1/control/{method}` — and one wire
shape, [`ControlRequest`](./types.md#controlrequest): the identity (with `run`
mandatory and `scope` carrying your steering claim), an optional
method-specific `payload`, and an optional `event_id` idempotency key.

| Method | What it does | Payload | Minimum scope (RFC §6.3) |
|---|---|---|---|
| `cancel` | Stop the run (soft; `{"hard": true}` propagates a hard cancellation context). | optional | `owner_user` |
| `pause` | Park the run at the next planner-step boundary (the unified pause primitive). | optional | `owner_user` |
| `resume` | Resume a paused run. | optional | `owner_user` |
| `approve` | Approve a HITL-gated step; the pause advances. | optional | `owner_user` |
| `reject` | Reject a HITL-gated step; the run terminates as a constraints conflict. | optional | `owner_user` |
| `redirect` | Rewrite the run's goal mid-flight. | `{"goal": "..."}` | `owner_user` |
| `inject_context` | Append operator context, visible on the planner's next step. | the context object | `session_user` |
| `user_message` | Inject a user-authored message into the run. | `{"message": "..."}` | `session_user` |
| `prioritize` | Change the task's scheduling priority. | `{"priority": N}` | `admin` |

The `scope` field is the **steering claim** (`session_user` < `owner_user` <
`admin`), checked against the table's minimum; cross-tenant steering
additionally requires `admin` regardless of the per-method minimum. A claim
below the minimum is rejected `403 scope_mismatch`.

Payloads are bounded at the edge (depth ≤ 6, ≤ 64 keys, ≤ 50 list items,
≤ 4096 chars per string, ≤ 16 KiB total) — an oversize payload is rejected
`422 payload_invalid`, never truncated.

## Preconditions: controls target LIVE runs

A steering control is delivered to the run's inbox. Three rejection shapes
matter:

- **`404 not_found`** — no live inbox: the run never started or already
  reached a terminal state. The [quickstart](./quickstart.md) demonstrates
  this deliberately by cancelling a finished run.
- **`403 scope_mismatch`** — your steering claim is below the method's
  minimum.
- **`422 payload_invalid`** — the payload broke a bound.

## The acknowledgement is not the effect

A `200` response ([`ControlResponse`](./types.md#controlresponse)) means
*validated, scope-checked, and enqueued*:

```json
{ "accepted": true, "method": "redirect", "protocol_version": "0.1.0" }
```

The control's **effect** — the redirected goal taking hold, the pause actually
blocking the loop, the approval advancing it — is observed on the event
stream: `control.received` → `control.applied` (or `control.rejected`),
followed by the event the effect causes (`task.cancelled` for a cancel;
`pause.requested` / `pause.resumed` for the pause-shaped controls — a parked
run's task status stays `running`, see
[the pause model](./pause-model.md)). A client that needs confirmation
watches the stream; a
richer synchronous response would couple the Protocol edge to the run loop's
step timing, so it deliberately does not exist.

## Pauses: finding what awaits a human

`pause.list` (`POST /v1/pause/list`) is the read-only snapshot of currently
paused runs in your scope — what a HITL inbox renders. Resuming continues
through the steering verbs (`resume` / `approve` / `reject`), never through a
pause-side mutation:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/pause/list" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}}'
```

The full pause model — what parks a run (HITL approval, tool-side OAuth),
durable pauses across restarts, timeout reaps — is
[choreography 4](./pause-model.md).

## Snapshots: tasks.list and tasks.get

`tasks.list` (`POST /v1/tasks/list`) projects the caller's session: filters
(status / kind / parent / time window / free text), cursor pagination, and
per-status aggregate counters —
[`TaskListRequest`](./types.md#tasklistrequest). To reload conversation A's
turns, send `X-Harbor-Session: A` and list.

`tasks.get` (`POST /v1/tasks/get`) returns the enriched
[`TaskDetail`](./types.md#taskdetail): the task row, parent session / parent
task references, the per-step cost rollup, and the reasoning trajectory by
reference. A cross-tenant task id returns `not_found` — existence is never
revealed across tenants.

Task `status` values on the wire: `pending`, `running`, `paused`, `complete`,
`failed`, `cancelled`. Background tasks additionally latch a
`background_acknowledged` boolean once their completion has been acknowledged.
