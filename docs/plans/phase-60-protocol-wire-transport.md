# Phase 60 — Protocol wire transport (SSE + REST)

## Summary

Phase 60 binds the transport-agnostic Protocol surface (Phase 54's
`ControlSurface` + Phase 05's `events.EventBus`) onto the wire: a
**REST/JSON** control surface (`POST` per Protocol method) and an **SSE**
event stream (`GET` long-lived, server→client). Both enforce the identity
triple at the edge and fail closed. The transports live behind the
`internal/protocol/transports/` seam so WebSocket can be added later as an
additive alternate transport, not a fork.

## RFC anchor

- RFC §5.4
- RFC §5.5

## Briefs informing this phase

- brief 07
- brief 06

## Brief findings incorporated

- brief 07 §"the runtime owns the protocol it speaks": the Protocol surface
  owns its own vocabulary and wire shape — Phase 60 does NOT let an HTTP
  framework dictate the method set; the REST router is a thin adapter that
  decodes a request, calls `ControlSurface.Dispatch`, and encodes the
  result. The handler maps `*protocol/errors.Error.Code` onto an HTTP
  status; it never invents a new error vocabulary.
- brief 06 §1 (one bus, no parallel observability channel): the SSE stream
  is a projection of the single `events.EventBus` — Phase 60 adds no second
  event channel. The SSE handler is a Protocol-side `events.Subscription`
  consumer; the wire framing is the only thing Phase 60 adds.
- brief 06 §"server-enforced identity": the SSE subscription's `events.Filter`
  is built from the request's identity triple at the edge — a stream
  request with an incomplete triple is rejected before any subscription is
  opened. `Admin` cross-tenant fan-in is NOT exposed on the wire in Phase 60
  (it needs the cryptographic scope claim Phase 61 adds); the wire stream is
  always triple-scoped.
- brief 06 §6 + the `events.Replayer` capability: a reconnecting SSE client
  resumes from a cursor — Phase 60 honours the `Last-Event-ID` SSE reconnect
  header by mapping it onto `events.Cursor` + `events.Replayer.Replay` when
  the bus driver supports replay, and degrades cleanly (live-tail only, no
  silent gap-masking) when it does not.

## Findings I'm departing from (if any)

None.

## Goals

- A REST/JSON control surface: one `http.Handler` that accepts a Protocol
  control request (`start` + the nine steering controls), resolves the
  identity scope at the edge, calls the existing transport-agnostic
  `ControlSurface.Dispatch`, and encodes the wire response or a
  `*protocol/errors.Error` mapped onto an HTTP status.
- An SSE event stream: one `http.Handler` that opens a triple-scoped
  `events.EventBus` subscription, frames each `events.Event` as an SSE
  `event:`/`id:`/`data:` block, emits periodic keepalive comments, honours
  `Last-Event-ID` reconnect via `events.Replayer` when available, and tears
  the subscription + goroutine down on client disconnect or server shutdown.
- Identity-scope enforcement at the edge: every request on either transport
  resolves `(tenant, user, session)` (+ `run` for controls) from the request
  and is rejected closed (`identity_required`) if any component is missing.
  No identity-downgrading knob.
- A `transports` package that composes both handlers into a single
  `http.ServeMux` a future server (Phase 64) mounts — structured so a
  WebSocket transport is additive (a third sub-package + a mux entry), not a
  rewrite.
- The transport server is a reusable artifact: immutable after construction,
  safe to share across N concurrent requests, every per-request goroutine
  cancellable by the request `ctx` and joined before the handler returns.

## Non-goals

- JWT validation / cryptographic identity-scope verification — Phase 61.
  Phase 60 reads the identity triple from request headers/body (trust-based,
  exactly as `events.Filter.Admin` and `IdentityScope.Scope` are trust-based
  until Phase 61); the edge *structure* (a single resolve-identity choke
  point) is built so Phase 61 slots auth in without reshaping the handlers.
- The `harbor dev` subcommand that boots an HTTP server and mounts the mux —
  Phase 64. Phase 60 ships the transport package + `http.Handler`s; there is
  no live server in the binary yet, so the smoke uses `httptest`-backed
  package/integration tests + static guards (the same posture phase-54.sh
  took for the transport-agnostic surface).
- WebSocket / gRPC / NDJSON alternate transports — additive later phases via
  the `internal/protocol/transports/` seam.
- State snapshots / topology / artifacts / traces / metrics Protocol surfaces
  — their own later phases; Phase 60 wires the task-control + event-stream
  surfaces that exist today.

## Acceptance criteria

- [ ] `internal/protocol/transports/control` exposes an `http.Handler` that
      routes a Protocol control request onto `ControlSurface.Dispatch` and
      returns the wire response as JSON; a `*protocol/errors.Error` is mapped
      onto a stable HTTP status (`400` invalid_request, `401`
      identity_required, `403` scope_mismatch, `404` not_found / unknown_method,
      `422` payload_invalid, `500` runtime_error).
- [ ] `internal/protocol/transports/stream` exposes an `http.Handler` that
      opens a triple-scoped `events.EventBus` subscription and streams events
      as SSE frames with `event:`, `id:`, `data:` lines; emits a keepalive
      comment on an interval; closes the subscription + joins its goroutine
      on client disconnect.
- [ ] Both handlers reject a request with an incomplete identity triple
      closed (`identity_required` / HTTP `401`) before touching the runtime.
- [ ] The SSE handler honours `Last-Event-ID` — on reconnect it replays
      events strictly newer than the cursor via `events.Replayer` when the
      driver supports it; when replay is unavailable it live-tails without
      silently masking the gap (the gap is surfaced, never a silent `nil`).
- [ ] `internal/protocol/transports` composes both handlers into one
      `http.ServeMux` via `NewMux`; the package layout leaves room for a
      WebSocket sub-package without reshaping `control` or `stream`.
- [ ] An integration test (`test/integration/phase60_wire_transport_test.go`)
      wires the REAL `ControlSurface` + REAL in-mem `events.EventBus` behind
      `httptest.Server`, streams events out over SSE and submits control in
      over REST end-to-end, proves the identity triple propagates through the
      edge, covers ≥1 failure mode (missing identity fails closed), and runs
      a full-duplex N≥10 concurrency stress under `-race`.
- [ ] A D-025 concurrent-reuse test: N≥100 concurrent requests against a
      single shared transport server (mix of REST control + SSE stream
      opens), `-race`, asserting no data races, no context bleed, no
      cross-cancellation, no goroutine leak.
- [ ] A goroutine-leak test: `runtime.NumGoroutine()` returns to baseline
      after the server is shut down and all SSE streams are drained.
- [ ] `scripts/smoke/phase-60.sh` exercises both directions (it runs the
      package + integration tests that stream events out and submit control
      in) and asserts the transport package layout + static guards; FAIL = 0.

## Files added or changed

```text
internal/protocol/transports/
  transports.go            # NewMux — composes control + stream handlers; the §4.4-style seam
  transports_test.go
  concurrent_test.go       # D-025 N>=100 + goroutine-leak
  control/
    control.go             # REST/JSON control handler over ControlSurface.Dispatch
    control_test.go
    status.go              # protocol/errors.Code -> HTTP status mapping
    status_test.go
  stream/
    stream.go              # SSE event-stream handler over events.EventBus
    stream_test.go
    frame.go               # SSE frame encoding (event:/id:/data:/keepalive comment)
    frame_test.go
test/integration/phase60_wire_transport_test.go
scripts/smoke/phase-60.sh
docs/plans/phase-60-protocol-wire-transport.md
docs/decisions.md          # D-078
docs/glossary.md           # SSE stream, keepalive comment, reconnect cursor, REST control surface
docs/plans/README.md       # Phase 60 row Pending -> Shipped
README.md                  # Status table Phase 60 -> Shipped + wire-surface pointer
```

## Public API surface

```go
// internal/protocol/transports
func NewMux(cs *protocol.ControlSurface, bus events.EventBus, opts ...Option) (*http.ServeMux, error)
type Option func(*muxConfig)
func WithLogger(l *slog.Logger) Option
func WithKeepalive(d time.Duration) Option
func WithClock(now func() time.Time) Option

// internal/protocol/transports/control
func NewHandler(cs *protocol.ControlSurface, opts ...Option) (http.Handler, error)
// Route: POST /v1/control/{method}  body: types.StartRequest | types.ControlRequest

// internal/protocol/transports/stream
func NewHandler(bus events.EventBus, opts ...Option) (http.Handler, error)
// Route: GET /v1/events  (SSE; identity via headers; Last-Event-ID reconnect)
```

## Test plan

- **Unit:** `control` — request decode, identity-edge rejection, the
  `errors.Code → HTTP status` table (every canonical code mapped), JSON
  response shape for `start` + a control method. `stream` — SSE frame
  encoding (`event:`/`id:`/`data:`/comment), `Last-Event-ID` parsing,
  identity-edge rejection, keepalive emission on the controllable clock.
- **Integration:** `test/integration/phase60_wire_transport_test.go` — real
  `ControlSurface` (over real inprocess `tasks.TaskRegistry` + real
  `steering.Registry`) + real in-mem `events.EventBus` behind
  `httptest.Server`; submit `start` + a control over REST, observe the
  matching events on the SSE stream; identity propagation asserted end-to-end;
  failure mode: missing-identity request rejected `401`.
- **Conformance:** N/A — the single-source discipline is gated by Phase 58's
  `internal/protocol/singlesource` checker, which already covers the new
  `transports/` tree (no method strings / error codes / wire types
  redefined there).
- **Concurrency / leak:** `transports/concurrent_test.go` — D-025 N≥100
  concurrent requests against one shared mux (REST + SSE mix) under `-race`;
  goroutine-leak test asserting baseline restoration after shutdown. The
  integration test additionally runs a full-duplex N≥10 stress.

## Smoke script additions

- Runs `go test -race ./internal/protocol/transports/...` (unit + D-025 +
  leak) and asserts pass.
- Runs `go test -race -run TestE2E_Phase60 ./test/integration/...` (the
  both-directions wire E2E) and asserts pass.
- Static guard: `internal/protocol/transports/{control,stream}` exist and
  `transports.go` declares `NewMux`.
- Static guard: no Protocol method string / error `Code` constant is
  redefined under `internal/protocol/transports/` (defence-in-depth over the
  Phase 58 lint).
- Static guard: the transport package does not import the Console.
- The live-HTTP assertions skip per the 404/405/501 → SKIP convention —
  `harbor dev` (the server that mounts the mux) is Phase 64; until then the
  wire surface is exercised via `httptest` in the package + integration
  tests.

## Coverage target

- `internal/protocol/transports`: 85%
- `internal/protocol/transports/control`: 85%
- `internal/protocol/transports/stream`: 85%

## Dependencies

- Phase 58 — `internal/protocol` single-source layout + the `singlesource`
  checker that gates the new `transports/` tree.
- Phase 05 — `events.EventBus` / `events.Filter` / `events.Replayer` /
  `events.Subscription` — the SSE stream's source.
- (transitively) Phase 54 — the `ControlSurface` the REST handler binds.

## Risks / open questions

- RFC §11 Q-1 is RESOLVED (2026-05-14, SSE + REST) — Phase 60 is a normal
  implementation phase, no decision gate.
- SSE keepalive / reconnect discipline: a buggy keepalive interval or a
  missing flush leaves a client hung. Mitigated by a controllable clock in
  the keepalive path + an explicit `http.Flusher` assertion + the
  goroutine-leak test.
- Identity at the edge is trust-based until Phase 61. The risk is that the
  trust posture is mistaken for the final posture — mitigated by routing all
  identity resolution through one documented choke point per transport so
  Phase 61's JWT validation is a localized change.
- There is no live server to smoke against (Phase 64). Mitigated by
  `httptest`-backed package + integration tests that exercise the real wire
  framing both directions — the same posture phase-54.sh took.

### Post-PR amendments

- **PR #91 / D-082 (Wave 10 audit WARN-5)** — the SSE transport gained
  an optional `X-Harbor-Run` identity-carrier header. When present, the
  subscription is run-scoped (`events.Filter.Run` is set, so only events
  with a matching `RunID` flow through); when absent, the subscription
  is session-scoped (the original Phase 60 default). The
  conformance suite's event-filter matrix gained the
  `RunScoped_StreamOpens` scenario.

## Glossary additions

- **SSE stream** — the Protocol's server→client event channel: a long-lived
  `text/event-stream` HTTP response framing each `events.Event` as an SSE
  block.
- **Keepalive comment** — an SSE comment line (`: keepalive`) the stream
  handler emits on an interval to keep idle connections (and intermediary
  proxies) from timing out.
- **Reconnect cursor** — the `events.Cursor` an SSE client resumes from after
  a disconnect, carried on reconnect via the `Last-Event-ID` SSE header and
  resolved against `events.Replayer`.
- **REST control surface** — the Protocol's client→server request-response
  channel: one JSON `POST` per task-control method, adapting onto
  `ControlSurface.Dispatch`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test
      passes — N≥100 concurrent invocations against a single shared instance
      under `-race`.** The transport server is a reusable artifact;
      `transports/concurrent_test.go` covers it.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists, wires real drivers
      end-to-end, asserts identity propagation, covers ≥1 failure mode, runs
      under `-race`.** `test/integration/phase60_wire_transport_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md
      entry filed — N/A, no departures.
