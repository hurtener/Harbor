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
non-durable `PublishLive` contract for immediate animation, followed by
persist-only batches at each completed planner step and at run sealing.

Durable batches remain wire-invisible: each member keeps its own event frame,
strictly increasing sequence, and reconnect cursor. Internally, cost and
universal tool-lifecycle events may enter one bounded FIFO and return after
acceptance; any later synchronous durable publication is an ordering barrier,
so a successful terminal event cannot overtake them. A hard process loss
before durable commit can lose accepted observability metadata. Clients must
not infer a second transcript or exactly-once receipt from async acceptance.

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
that cursor** before resuming the live tail. The in-memory driver retains
a ring; the durable driver reads its retained StateStore-backed log. Retention
and replay availability are operator-configured. Restart recovery requires
`events.driver: durable` backed by a persistent StateStore, not an in-memory
store. Merely selecting the durable bus with an in-memory store does not survive
a process restart. `Last-Event-ID: 0`
replays everything durable retained for your scope — the trick the
[quickstart](./quickstart.md) uses to read a run that finished before the tail
opened.

### Intermediate assistant updates

`llm.completion.chunk` has two delivery stages. Live frames have sequence zero
and no `id:`. The runtime buffers those frames and, when the planner step
returns, persists them **before executing that step's tool**. Persist-only
writes assign durable sequences without publishing a second live copy. The
run's terminal seal drains accepted callbacks before successful completion;
a failed persistence barrier must not become a successful task completion.
Completed-step frames can therefore be recovered by SSE replay or
`state.history`, including after reopening a persistent database. Both delivery
stages expose the same payload fields; durable storage's internal `Data` wrapper
is not part of the wire payload.

Group chunks by run/task and `payload.Kind`: `content` is ordinary assistant
text, while `reasoning` belongs only in a separate reasoning/activity view.
`payload.Done` closes that LLM response's lane, **not the task**. A nonterminal
response can contain a user-facing update alongside native tool calls; several
such responses can precede the final answer. Do not concatenate progress updates
or reasoning into the final `AnswerEnvelope`.

Durability starts at the persistence boundary, not at live delivery. A crash,
cancellation, or failed write before the boundary can lose an already animated
unflushed tail. Clients must treat sequence-zero text as provisional. On
reconstruction, rebuild the affected run's progress from retained history rather
than appending replayed text on top of provisional animation: those two deliveries
do not share a durable event ID. A cursor later than a step intentionally omits
its older chunks. Use history backfill for old updates, not just the newest live
cursor. Retention gaps remain explicit; never synthesize missing updates or a
successful answer for a failed/cancelled run. Schema-constrained tasks retain
the existing suppression of unvalidated text deltas.

The session-turn projection is **not a raw progress transcript**.
`sessions.turns.list/get` provides the final answer and consumer-safe activity;
it does not embed completion chunks or raw provider reasoning. A client that
needs historical intermediate text must also consume the authorized retained
event history. In-memory `tasks.get` trajectory enrichment is not a restart
recovery source.

The authoritative reopen choreography is unchanged: read
`sessions.turns.list`, establish running/paused membership, and open SSE with
that page's `live_resume_seq`; browser `Last-Event-ID` still wins on reconnect.
One terminal frame still causes one `sessions.turns.get` reconciliation.

When the cursor has aged out of retention (or the configured bus driver has no
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
aggregate is computed from retained durable event history. Sequence-zero live
frames are absent; completion chunks persisted at step boundaries are present.
Do not interpret chunk counts as task or assistant-message counts.

## A correct minimal consumer, in pseudocode

```text
open GET /v1/events with token + session header
remember the largest id: seen
on disconnect: reopen with Last-Event-ID: <largest seen>
on bus.dropped or stream.replay_unavailable: re-snapshot via tasks.list /
    sessions.inspect, then continue
branch on event: type using the catalog in events.md
```
