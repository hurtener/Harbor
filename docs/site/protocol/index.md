# The Harbor Protocol

The Harbor Protocol is the canonical contract between a Harbor Runtime and every
client that observes or drives it. The bundled Console holds **no privileged
access** — the same surface powers a remote attach, a third-party dashboard, an
IDE or TUI client, or an event pipe into your observability stack (RFC §5.1).
This track is everything you need to build against that surface without reading
Go source.

The wire is deliberately small: **REST/JSON for control** (request → response)
and **SSE for the event stream** (server → client), both server-enforced for
identity. No gRPC, no WebSocket shim, no SDK requirement — `curl` is a complete
client.

## Start here

**[Speak Protocol in 15 minutes](./quickstart.md)** — bootstrap a dev token,
start a run, watch the planner think on the live event stream, read the result,
steer. Every request and response on that page is real: the project's preflight
gate extracts the page's commands and executes them against a live `harbor dev`
server on every commit, so the quickstart cannot drift from the wire.

## The reference (generated — cannot drift)

Four pages, emitted by `cmd/harbor-gen-protocol-docs` from the same canonical
sources the Runtime compiles from, and gated in CI by
`make protocol-docs-gen-check` (`git diff --exit-code` after regeneration — a
Go-side wire change without regenerated docs fails the build):

| Page | What it catalogues |
|---|---|
| [Methods](./methods.md) | Every canonical method: route, classification, request/response types, auth posture. |
| [Events](./events.md) | Every canonical event type with its payload shape, field-level. |
| [Errors](./errors.md) | Every error code: HTTP binding, when it fires, retry guidance. |
| [Types](./types.md) | Every wire struct, field-level, with the snake_case JSON keys. |

## The choreographies (sequence and intent)

What a reference cannot teach — how the calls compose:

1. **[Auth & identity](./auth-and-identity.md)** — the JWT shape, the scope
   vocabulary, the session-blank connection model, what 401 / 403 mean.
2. **[Streaming semantics](./streaming-semantics.md)** — subscribe, filtering,
   replay/reconnect, backpressure, aggregation.
3. **[Task control](./task-control.md)** — the run lifecycle as the wire sees
   it: start, steer, observe, terminal states.
4. **[The pause model](./pause-model.md)** — `pause.requested` → the
   intervention surfaces (approve/reject, the OAuth callback, plain resume),
   durable pauses across restarts, timeout reaps.
5. **[Versioning & compatibility](./versioning-and-compatibility.md)** — what
   the Protocol version promises, what to pin, what to tolerate.

## Build a client, then prove it

**[Build a client](./build-a-client.md)** walks the shortest credible client
— a ~150-line, SDK-free Go event viewer whose source ships in the repo and is
compile-gated in CI — then points at the reference implementations. When you
need a compatibility claim,
**[conformance certification](./conformance-certification.md)** documents the
suite Harbor itself is gated on and what passing it means.

## Who this track serves

- **The evaluator** — take the 15-minute curl tour before committing.
- **The client builder** — dashboard / TUI / IDE: the complete reference plus
  the streaming and control choreographies.
- **The event integrator** — pipe `events.subscribe` into your bus: the event
  catalog with payload shapes, filters, and replay semantics.
- **The control integrator** — drive runs from your product: the task-control
  choreography.

Operators looking for a task recipe rather than the adopter reference should
start at the [`use-the-harbor-protocol`](../skills/use-the-harbor-protocol/SKILL.md)
skill instead.
