# Phase 54 — protocol-control-surface

## Summary

Phase 54 creates the Harbor Protocol layer (`internal/protocol/`) and ships
its **task control surface**: the ten canonical method names, the request /
response wire types, the Protocol error codes, the Protocol version pin, and
a transport-agnostic in-process `ControlSurface` handler that maps a Protocol
method call onto the already-shipped runtime — `start` onto the Phase 20
`tasks.TaskRegistry`, the nine steering controls (`cancel`, `pause`, `resume`,
`redirect`, `inject_context`, `approve`, `reject`, `prioritize`,
`user_message`) onto a Phase 52 `steering.ControlEvent` enqueued on the run's
`steering.Inbox`. Identity scope is enforced at the Protocol edge on every
method. The SSE+REST wire transport is Phase 60 — Phase 54 ships the surface
in-process-invocable and testable now, which is what lets the Wave 9 E2E
exercise it end-to-end.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.3

## Briefs informing this phase

- brief 02
- brief 06
- brief 07

## Brief findings incorporated

- **brief 02 §3 "Protocol exposure for steering":** the Protocol exposes a
  control-plane endpoint per session/task; the runtime validates + sanitises a
  `ControlEvent`, then deposits it on a per-run inbox. Phase 54's
  `ControlSurface` is exactly that edge: it constructs a `steering.ControlEvent`
  from a Protocol request, lets the Phase 52 `Inbox.Enqueue` do the
  validation / scope check / bounds enforcement (no second validator), and
  returns a typed Protocol response. The nine control methods map 1:1 onto the
  nine `steering.ControlType` values.
- **brief 02 §5 sharp-edge #2 "Steering at planner level" / the control plane
  is a Protocol concern:** "the control plane is a Protocol concern... the
  runtime intercepts control... the planner observes the result." Phase 54 is
  the Protocol-side half — the surface a Console / TUI / third-party client
  drives — and it reaches the runtime ONLY through the public Phase 52 / Phase
  20 surfaces. It never touches `RunContext` or the planner.
- **brief 06 §1 "protocol-grade event bus" + the decoupling rule:** "Console,
  third-party consoles, and `harbor dev` see exactly the same data shape" and
  "the Console NEVER reads runtime internals." Phase 54's wire types are
  Protocol-owned structs in `internal/protocol/types/` — never a re-export of
  a runtime-internal Go type. A Protocol method that mapped 1:1 onto an
  internal Go signature would be the RFC §5.1 reject-on-sight smell; the
  control methods deliberately accept a flat wire shape (`tenant` / `user` /
  `session` / `run` strings + a `payload` map) and the surface translates.
- **brief 06 §"Wire format" open question + RFC §5.4:** the wire transport is
  still being chosen (SSE+REST is the current lean) and "the relevant phase
  blocks until it resolves." Phase 54 takes the explicit consequence: it ships
  the transport-AGNOSTIC surface (method names, wire types, the in-process
  dispatcher) NOW, and the HTTP/SSE binding lands in Phase 60. The
  `ControlSurface` is a plain Go type with a `Dispatch(ctx, method, request)`
  entry point — a Phase 60 HTTP handler is a thin adapter over it.
- **brief 07 §"the full protocol is six lines" — the runtime owns the protocol
  it speaks:** brief 07's keystone is that Harbor owns its planner/LLM
  protocol rather than inheriting a provider's. The same discipline applies to
  the client-facing Protocol: the method names, wire types, and error codes
  are Harbor-owned and single-sourced (`internal/protocol/methods`,
  `internal/protocol/types`, `internal/protocol/errors`) — no hardcoded method
  strings, no third type-definition site (CLAUDE.md §8 + §13).

## Findings I'm departing from (if any)

None. The master-plan acceptance line says the nine endpoints + `start`
"round-trip via SSE+REST (phase 60)" — Phase 54 reads that as a forward
reference (the SSE+REST binding is Phase 60's work) and ships the
transport-agnostic surface that Phase 60 will bind. This is not a departure —
the master-plan detail block, the RFC §5.4 "the relevant phase blocks until
[the transport] resolves", and brief 06's open wire-format question all point
the same way. The split is recorded in D-072.

## Goals

- Create the `internal/protocol/` tree per CLAUDE.md §3 — `types/`, `methods/`,
  `errors/` — laid out correctly from the start so Phase 58 ("Protocol
  types/methods/errors single source") is a no-op formalisation, not a
  cleanup.
- Pin the Protocol version in `internal/protocol/types/version.go`.
- Define the ten canonical task-control method names in
  `internal/protocol/methods/methods.go` (single source — no hardcoded method
  strings elsewhere).
- Define the request / response wire types for the ten methods in
  `internal/protocol/types/` (single source — no Protocol message struct
  defined anywhere else).
- Define the Protocol error codes in `internal/protocol/errors/errors.go`
  (single source).
- Ship `ControlSurface` — a transport-agnostic, in-process-invocable handler
  that dispatches a Protocol method call onto the runtime: `start` →
  `tasks.TaskRegistry.Spawn`; the nine controls →
  `steering.Registry.Lookup(run).Enqueue(ControlEvent)`.
- Enforce identity scope at the Protocol edge on every method — a request
  with an incomplete identity triple fails closed; a cross-tenant or
  insufficient-scope steering submission fails closed (the Phase 52
  `CheckScope` already does the per-event scope work; the surface maps its
  errors to Protocol error codes).
- Author `test/integration/wave9_test.go` — the Wave 9 wave-end E2E.

## Non-goals

- **The SSE+REST (or any) wire transport.** That is Phase 60. Phase 54 ships
  no HTTP handler, no SSE stream, no `net/http` import. `internal/protocol/
  transports/` is created by Phase 60, not here.
- **JWT parsing / auth.** Protocol auth is Phase 61. Phase 54 takes the
  identity triple + the `steering.Scope` as already-derived inputs on the
  request (exactly as Phase 52's `CheckScope` takes a trust-based `Scope` until
  Phase 61). The surface enforces the scope; it does not verify a token.
- **The streaming-events / state-snapshot / topology / artifacts / traces /
  metrics Protocol surfaces** (RFC §5.2's other rows). Phase 54 is the task
  control surface only; the others are later phases.
- **A new persistence or transport driver seam.** The `ControlSurface` is an
  in-process handler with no plausible alternate backend — no §4.4 driver tree.
- **Running a planner loop.** Phase 53's `RunLoop` already drains the inbox and
  applies the controls; Phase 54 only enqueues onto the inbox. The wave9 E2E
  wires a real `RunLoop` to prove the enqueue→drain→apply path composes.

## Acceptance criteria

- [ ] `internal/protocol/types/`, `internal/protocol/methods/`,
  `internal/protocol/errors/` exist and compile.
- [ ] `internal/protocol/types/version.go` pins the Protocol version as an
  exported constant.
- [ ] `internal/protocol/methods/methods.go` declares all ten canonical
  task-control method names (`start`, `cancel`, `pause`, `resume`, `redirect`,
  `inject_context`, `approve`, `reject`, `prioritize`, `user_message`) as
  exported constants + an `IsValidMethod` / `Methods()` pair.
- [ ] `internal/protocol/types/` declares the request + response wire types
  for the ten methods; no Protocol message struct is defined outside
  `internal/protocol/types/`.
- [ ] `internal/protocol/errors/errors.go` declares the Protocol error codes;
  no Protocol error code is defined outside `internal/protocol/errors/`.
- [ ] `ControlSurface.Dispatch(ctx, method, request)` routes `start` to
  `tasks.TaskRegistry.Spawn` and the nine controls to a
  `steering.ControlEvent` enqueued on the run's `steering.Inbox`.
- [ ] Each of the ten methods round-trips through the in-process surface in a
  test (the smoke script exercises each via the package + integration tests).
- [ ] A request with an incomplete identity triple fails closed with the
  identity-required Protocol error code (no method reaches the runtime).
- [ ] A cross-tenant / insufficient-scope steering submission fails closed with
  the scope-mismatch Protocol error code (the surface maps `steering.
  ErrScopeMismatch`).
- [ ] An oversize / over-deep control payload fails closed with the
  payload-invalid Protocol error code (the surface maps `steering.
  ErrPayloadInvalid`).
- [ ] `ControlSurface` is a D-025 compiled artifact — a concurrent-reuse test
  runs N≥100 concurrent `Dispatch` calls against one shared surface under
  `-race` (no data races, no context bleed, no goroutine leaks).
- [ ] `test/integration/wave9_test.go` exists, wires real drivers across the
  full Wave 9 surface (pauseresume `Coordinator` + Agent Registry + steering
  inbox/`RunLoop` + the Phase 54 `ControlSurface`), asserts identity
  propagation, covers ≥1 failure mode, runs an N≥10 concurrency stress, and
  passes under `-race`.
- [ ] `scripts/smoke/phase-54.sh` exercises each of the ten methods and passes.
- [ ] Coverage on `internal/protocol/...` ≥ 85%.

## Files added or changed

```text
internal/protocol/
  protocol.go                  # package doc + ControlSurface + NewControlSurface + Dispatch
  control.go                   # the ten per-method dispatch handlers
  errors.go                    # surface-internal error mapping (runtime err -> protocol.Code)
  protocol_test.go             # unit: dispatch routing, identity/scope/payload failure modes
  control_test.go              # unit: per-method request/response round-trips
  concurrent_test.go           # D-025 concurrent-reuse (N>=100)
  types/
    version.go                 # Protocol version pin
    control.go                 # request/response wire types for the ten methods
    types_test.go              # wire-type JSON round-trip + version pin
  methods/
    methods.go                 # the ten canonical method-name constants + IsValidMethod/Methods
    methods_test.go            # exhaustiveness + validity
  errors/
    errors.go                  # Protocol error codes + Error wire type
    errors_test.go             # code stability + Error round-trip
docs/plans/phase-54-protocol-control-surface.md   # this plan
docs/plans/README.md           # Phase 54 row Pending -> Shipped
docs/decisions.md              # D-072
docs/glossary.md               # Protocol control surface, Protocol method, Protocol error code
scripts/smoke/phase-54.sh      # exercises each of the ten methods
test/integration/wave9_test.go # Wave 9 wave-end E2E
README.md                      # Phase 54 status row
```

No new top-level directory — `internal/protocol/` is already in CLAUDE.md §3.

## Public API surface

```go
package methods

// Method is the string-typed enum of canonical Protocol method names.
type Method string

const (
    MethodStart         Method = "start"
    MethodCancel        Method = "cancel"
    MethodPause         Method = "pause"
    MethodResume        Method = "resume"
    MethodRedirect      Method = "redirect"
    MethodInjectContext Method = "inject_context"
    MethodApprove       Method = "approve"
    MethodReject        Method = "reject"
    MethodPrioritize    Method = "prioritize"
    MethodUserMessage   Method = "user_message"
)

func IsValidMethod(m Method) bool
func Methods() []Method            // deterministic sorted snapshot

package types

const ProtocolVersion = "0.1.0"   // pinned; bumping is an RFC change

// IdentityScope is the flat wire identity a Protocol request carries.
type IdentityScope struct {
    Tenant, User, Session, Run string
    Scope                      string // steering scope claim (trust-based until Phase 61)
}

type StartRequest  struct { Identity IdentityScope; Query, Description string; Priority int; IdempotencyKey string }
type StartResponse struct { TaskID string; Reused bool }

type ControlRequest  struct { Identity IdentityScope; Payload map[string]any; EventID string }
type ControlResponse struct { Accepted bool; Method string }

package errors

type Code string

const (
    CodeInvalidRequest   Code = "invalid_request"
    CodeIdentityRequired Code = "identity_required"
    CodeScopeMismatch    Code = "scope_mismatch"
    CodePayloadInvalid   Code = "payload_invalid"
    CodeUnknownMethod    Code = "unknown_method"
    CodeNotFound         Code = "not_found"
    CodeRuntimeError     Code = "runtime_error"
)

type Error struct { Code Code; Message string }
func (e *Error) Error() string

package protocol

// ControlSurface is the transport-agnostic task-control handler. Built once,
// shared across N concurrent Dispatch calls (D-025).
type ControlSurface struct { /* unexported */ }

func NewControlSurface(tasks tasks.TaskRegistry, steering *steering.Registry, opts ...Option) (*ControlSurface, error)

// Dispatch routes a Protocol method call onto the runtime. Transport
// adapters (Phase 60 HTTP/SSE) call Dispatch; it is the single entry point.
func (s *ControlSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
```

## Test plan

- **Unit:** `methods_test.go` — the ten method names + `IsValidMethod` +
  `Methods()` exhaustiveness. `types_test.go` — wire-type JSON round-trip, the
  version pin. `errors_test.go` — code stability, `*Error` round-trip + the
  `error` interface. `protocol_test.go` — `Dispatch` routes each method to the
  right runtime call; unknown method → `CodeUnknownMethod`; incomplete
  identity → `CodeIdentityRequired`; cross-tenant non-admin → `CodeScopeMismatch`;
  oversize payload → `CodePayloadInvalid`. `control_test.go` — each of the ten
  methods' request → response round-trip against a real `tasks.TaskRegistry`
  (inprocess) + real `steering.Registry`.
- **Integration:** `test/integration/wave9_test.go` — the Wave 9 wave-end E2E.
  Real drivers on every seam: real `tasks.TaskRegistry` (inprocess over a real
  in-mem `state.StateStore`), real `events.EventBus` (inmem), real patterns
  redactor, real `pauseresume.Coordinator` (over the real StateStore checkpoint
  store), real `registry.AgentRegistry`, real `steering.Registry` +
  `steering.RunLoop`, real `protocol.ControlSurface`. The E2E drives a run
  through `ControlSurface.Dispatch(start)` → the `RunLoop` drains an
  `inject_context` / `pause` / `approve` submitted via `Dispatch` →
  `Finish`; asserts identity propagation through every layer; covers ≥1
  failure mode (missing-identity rejection at the surface edge, scope mismatch,
  oversize payload); runs an N≥10 concurrency stress (concurrent runs +
  concurrent `Dispatch` calls against shared artifacts) with the goroutine
  baseline restored on teardown.
- **Conformance:** N/A — Phase 54 ships no multi-driver subsystem.
- **Concurrency / leak:** `concurrent_test.go` — N≥100 concurrent `Dispatch`
  calls against one shared `ControlSurface` under `-race`: distinct
  per-goroutine identity quadruples (a context bleed surfaces as a foreign
  triple), no data races, baseline `runtime.NumGoroutine` restored after join.

## Smoke script additions

- `scripts/smoke/phase-54.sh` runs `go test -race ./internal/protocol/...`
  (covers the ten method names, the wire types, the error codes, `Dispatch`
  routing for each of the ten methods, the identity/scope/payload failure
  modes, and the D-025 concurrent-reuse test).
- Runs `go test -race -run TestE2E_Wave9 ./test/integration/...` (the wave-end
  E2E — the ten-method surface composed against the real Wave 9 runtime
  surface).
- Static guard: the ten canonical method names are present in
  `internal/protocol/methods/methods.go`.
- Static guard: `internal/protocol/` does not import `net/http` (the wire
  transport is Phase 60 — Phase 54 is transport-agnostic).
- Static guard: `internal/protocol/` does not import the Console (the Runtime
  never imports Console code — CLAUDE.md §13).
- Static guard: no Protocol message struct / method string / error code is
  defined outside `internal/protocol/{types,methods,errors}` (the single-source
  rule — CLAUDE.md §8; Phase 58 formalises the lint).
- The HTTP/Protocol-wire assertions skip with a reason per the 404/405/501 →
  SKIP convention — the SSE+REST wire surface lands in Phase 60.

## Coverage target

- `internal/protocol`: 85%
- `internal/protocol/types`: 85%
- `internal/protocol/methods`: 85%
- `internal/protocol/errors`: 85%

## Dependencies

- 50 — `pauseresume.Coordinator` (the pause-lifecycle controls converge here;
  the wave9 E2E wires it).
- 53 — `steering.Registry` / `steering.Inbox` / `steering.ControlEvent` /
  `steering.RunLoop` (the nine controls map onto this).
- 20 — `tasks.TaskRegistry` (`start` maps to `Spawn`).

(Also touches Phase 52's `steering.CheckScope` / `ValidatePayload` — shipped as
part of the Phase 52/53 steering cluster — and Phase 53a's `registry.
AgentRegistry`, wired in the wave9 E2E.)

## Risks / open questions

- **Wire transport choice (RFC §11 Q-1, RFC §5.4).** Still SSE+REST-leaning but
  not locked. Phase 54 is deliberately transport-agnostic so the choice does
  not block this phase — `ControlSurface.Dispatch` is the seam any transport
  adapter binds. If Q-1 resolves to something other than SSE+REST, only Phase
  60 is affected, not Phase 54.
- **Identity-scope verification is trust-based until Phase 61.** Phase 54
  enforces the scope (via Phase 52's `CheckScope`) but takes the
  `IdentityScope.Scope` claim on trust, exactly as Phase 52's `CheckScope` and
  Phase 05's `events.Filter.Admin` do until Protocol auth (Phase 61) lands.
  This is the established project posture, not a new risk.
- **`ControlResponse` shape.** Phase 54 returns a minimal `{accepted, method}`
  acknowledgement for the nine controls — the run's *effect* of a control is
  observed via the canonical event stream (`control.received` /
  `control.applied`, Phase 53), not synchronously in the response. A richer
  synchronous response would couple the Protocol edge to the run loop's timing
  — the event stream is the canonical observation channel (brief 06 §1).

## Glossary additions

- **Protocol control surface** — the transport-agnostic task-control handler
  (`protocol.ControlSurface`) that maps a Protocol method call onto the
  runtime. Added to `docs/glossary.md`.
- **Protocol method** — a canonical client→runtime request name
  (`internal/protocol/methods`); the ten task-control methods are the Phase 54
  set. Added to `docs/glossary.md`.
- **Protocol error code** — a canonical, stable, client-facing error string
  (`internal/protocol/errors`); the single source for what a Protocol client
  branches on. Added to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. `ControlSurface` is a reusable artifact — `concurrent_test.go` pins it.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. `test/integration/wave9_test.go` is the integration test.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
