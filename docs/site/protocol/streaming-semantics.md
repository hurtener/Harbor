# Choreography 2 — Streaming semantics

How a Protocol client consumes the event stream: subscribe, filter, replay,
survive backpressure, and aggregate.

> Methods demonstrated: `events.subscribe`, `events.aggregate`

## One bus, one stream

Everything a Harbor Runtime does — task lifecycle, planner decisions, LLM
token chunks, tool execution, pause/resume, governance, audit — is narrated on
**one** typed event bus. There is no parallel observability channel. The
Protocol exposes that bus as Server-Sent Events. Durable semantic/lifecycle
events use the replayable `Publish` contract; LLM completion chunks use the
non-durable `PublishLive` contract for present-tense animation.

```text
GET /v1/events
Authorization: Bearer <token>
X-Harbor-Session: <session-id>
Accept: text/event-stream
```

The response is a standard SSE stream. Each frame:

```text
event: task.spawned
id: 6
data: {"type":"task.spawned","sequence":6,"occurred_at":"2026-06-10T23:03:22.781100000Z","tenant":"dev","user":"dev","session":"quickstart-demo","payload":{"TaskID":"01KTSWDBCXF2WZ4Z8YPZYF3A8C","Kind":"foreground","ParentTaskID":"","Priority":0,"IdempotencyKey":""}}
```

- `event:` — the canonical event type ([the full catalog](./events.md)).
- `id:` — the per-bus monotonic **sequence** for durable events, your
  reconnect cursor. Live animation events have `Sequence == 0` and omit
  `id:` because they have no replay position.
- `data:` — one JSON object: the type, sequence, timestamp, the flat identity
  (`tenant` / `user` / `session` / `run`), and the `payload`.

The stream opens with a `retry: 3000` directive (the reconnect backoff in
milliseconds) and emits a `: keepalive` comment every ~15s so intermediary
proxies don't reap an idle connection.

## Server-side identity filtering

The subscription is filtered **on the server** to your verified
`(tenant, user, session)` — you cannot observe another session's events, and
no client-side filtering is load-bearing. Two narrowing knobs:

- `X-Harbor-Run: <run-id>` — run-scoped instead of session-scoped: only
  events whose run id matches flow through.
- `X-Harbor-Event-Type: <type>[,<type>...]` — restrict to named event types.

## The elevated fleet view

`GET /v1/events?admin=1` requests cross-tenant fan-in (the Console's fleet
view, an org-wide observability pipe). It is gated on a verified `admin` or
`console:fleet` scope claim; without one the request is rejected
`403 identity_scope_required` before any subscription opens. Every admin
subscribe is itself audited (`audit.admin_scope_used` lands on the bus).

## Replay and reconnect

SSE's native reconnect mechanism is honoured: when your connection drops, echo
the last `id:` you processed back as a header —

```text
Last-Event-ID: 41
```

— and the Runtime **replays every retained durable event strictly newer than
that cursor** from its ring buffer before resuming the live tail. The ring's
size is operator-configured (`events.replay_buffer_size`). `Last-Event-ID: 0`
replays everything durable retained for your scope — the trick the
[quickstart](./quickstart.md) uses to read a run that finished before the tail
opened.

`llm.completion.chunk` frames are deliberately not retained and carry no
`id:`. A reconnect can therefore miss token animation between durable frames;
that is expected lossy delivery, not a cursor gap. The terminal `task.completed`
and session-turn `AnswerEnvelope` are authoritative, so the client reconciles
the final answer and task state when the run completes. A failed or cancelled
run does not receive a synthetic answer from the live lane.

When the cursor has aged out of the ring (or the configured bus driver has no
replay), the gap is **surfaced, never silently swallowed**: the stream emits
an explicit `stream.replay_unavailable` comment frame so your client knows it
has a hole and can re-snapshot via the read methods (`tasks.list`,
`sessions.inspect`, …).

## Backpressure: drop-oldest, loudly

Per-subscriber buffers are bounded (`events.subscriber_buffer_size`). A
subscriber that cannot keep up loses the **oldest** buffered events, and the
bus tells it: a `bus.dropped` event (at most one per drop window) carries the
dropped sequence range —

```text
event: bus.dropped
data: {... "payload":{"FromSeq":120,"ToSeq":143,"DroppedCount":24,"SubscriberID":7}}
```

— so a correct client treats `bus.dropped` like a failed reconnect: re-sync
state via the snapshot reads, then continue tailing. A subscriber that stops
draining entirely is reaped after the idle timeout
(`bus.subscription_idle_closed`). A dropped live animation frame has
`Sequence == 0` and cannot identify a cursor range; the terminal durable
reconciliation is the recovery path for that loss.

## Payloads: safe vs redacted

Payload field keys are the **Go field names** (`TaskID`, capital `T` — the
classic integration gotcha). Two delivery classes, catalogued per type in
[events.md](./events.md):

- **Safe payloads** arrive typed and verbatim — the declaring subsystem
  guarantees no secret-shaped content.
- Everything else passes through the audit redactor on publish and arrives as
  a redacted key/value map.

Heavy content never rides the stream inline: large tool outputs and uploads
travel as artifact references.

## Aggregation: the stream's complement

For dashboards that want **counts, not frames**, `events.aggregate` returns
time-bucketed per-type counts over a window instead of a replay. `window` and
`bucket` are Go durations on the wire — integer **nanoseconds** — and `bucket`
must evenly divide `window` (one hour in 15-minute buckets below):

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/events/aggregate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "filter": {}, "window": 3600000000000, "bucket": 900000000000}'
```

A real response (one active bucket):

```json
{
  "buckets": [
    {
      "bucket_start": "2026-06-10T22:51:35.945734Z",
      "bucket_end": "2026-06-10T23:06:35.945734Z",
      "counts": {
        "planner.decision": 2,
        "session.opened": 1,
        "task.completed": 2,
        "task.spawned": 2,
        "task.started": 2
      }
    }
  ],
  "protocol_version": "0.1.0"
}
```

Request/response shapes:
[`EventAggregateRequest`](./types.md#eventaggregaterequest) /
[`EventAggregateResponse`](./types.md#eventaggregateresponse). The same
cross-tenant scope rules as the stream apply (`admin` / `console:fleet`). The
aggregate is computed from durable event history; transient
`llm.completion.chunk` animation is intentionally absent from these counts.

## A correct minimal consumer, in pseudocode

```text
open GET /v1/events with token + session header
remember the largest id: seen
on disconnect: reopen with Last-Event-ID: <largest seen>
on bus.dropped or stream.replay_unavailable: re-snapshot via tasks.list /
    sessions.inspect, then continue
branch on event: type using the catalog in events.md
```
