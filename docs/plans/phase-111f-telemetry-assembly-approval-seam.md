# Phase 111f — Telemetry assembly + approval-gate authorizer seam

## Summary

Two halves, both "the layer below is right, the assembly never happened."

**(1) Telemetry.** `telemetry.New` — the redactor-mandatory,
identity-attribute, bus-paired Logger (`internal/telemetry/logger.go:87`) —
has **zero production callers**: the dev binary boots bare `slog`
(`cmd/harbor/cmd_dev.go:258`) and never upgrades, so RFC §6.14's "Logger.Error
emits both an slog record AND a paired `runtime.error` bus event" is false on
every path. `engine.WithRunErrorHandler`'s godoc describes "production wiring"
that doesn't exist (`internal/runtime/engine/options.go:114-127`; the only
engine construction, `internal/runtime/flow/flow.go:165`, passes no handler).
And the trace half of observability never got its bridge: metrics got
`BridgeBusToMetrics` (`internal/telemetry/metrics.go:536`); traces got
nothing — `NewTracer` (`internal/telemetry/tracing.go:205`) is never
constructed on any production path despite `main.go:101-104` blank-importing
the span exporters for it. Phase 111f wires `telemetry.New` into the
production assembly, passes the RunErrorHandler through the flow seam, ships
`telemetry.BridgeBusToTracer` symmetric with metrics, and writes
`docs/recipes/observe-an-embedded-runtime.md` (audit §7's named recipe).

**(2) Approval-gate authorizer seam.** `ApprovalGate.ResolveApproval`
hard-requires `internal/protocol/auth` scope claims on ctx
(`internal/tools/approval/gate.go:13` import; `gate.go:353-355` →
`ErrApprovalScopeRequired`), and the runtime's own steering bridge must
**self-elevate** via `protocolauth.WithScopes` to call its own gate
(`internal/runtime/steering/apply.go:373-385`) — wire-layer auth vocabulary
inside an in-process runtime control path, the tell that the check sits one
layer too low (audit §4). Phase 111f makes the privilege check an injected
seam on `GateDeps`, removes the `protocol/auth` import from
`internal/tools/approval`, deletes the self-elevation, and records the
direction rule the audit proposed: **runtime may import protocol TYPES
(data), never protocol auth/methods/transports (behaviour)**.

## RFC anchor

- RFC §6.14 — Telemetry ("Slog + OpenTelemetry from t=0 … No retrofit";
  the Logger.Error/`runtime.error` pairing; OTel propagation conventions).
- RFC §6.4 — Tool catalog (the approval-gate HITL surface whose resolve path
  this phase re-seams).
- RFC §5.1 — Decoupling rule (the layering principle the protocolauth leak
  violates in mirror image: the Protocol's auth vocabulary reaching into
  runtime control is the same boundary smell as runtime internals reaching
  the Console).

## Briefs informing this phase

- brief 06 — events, observability, devx (bus-first observability; OTel as a
  first-class derivation of the event bus; the no-OTel-in-runtime
  anti-pattern; metrics-cardinality rules)
- brief 02 — planner + steering + HITL (the approval/steering control path
  the authorizer seam sits on)

## Brief findings incorporated

- **brief 06 §"No OpenTelemetry in the runtime" anti-pattern.** "OTel traces
  and metrics should be a first-class derivation of the event bus, shipped
  from t=0, not retrofitted." Metrics honoured this (BridgeBusToMetrics);
  traces are currently the documented anti-pattern — exporters imported,
  tracer never constructed. The trace bridge restores symmetry.
- **brief 06 §metrics cardinality footgun.** "`Event.TraceID` is for logs and
  OTel traces; metrics derive from Type/NodeName/Producer only." The trace
  bridge MAY stamp run/trace identifiers as span attributes (that is what
  traces are for); the existing metrics derivation stays untouched — the
  bridge pair enforces the split rather than blurring it.
- **brief 06 §audit-as-subscriber.** "Auditing is a bus subscriber that
  persists a redacted projection — not a parallel system." The Logger's
  redactor-mandatory construction is the same principle at the log edge;
  booting bare slog around it is the parallel system the brief forbids.
- **brief 02 §steering authn.** Per-event scopes are enforced at the
  Protocol edge (APPROVE/REJECT require originating-user/admin scope). The
  gate re-checking PROTOCOL scopes in-process is double accounting in the
  wrong vocabulary; the seam keeps defence-in-depth but in runtime terms
  (identity tuple / control scope).

## Findings I'm departing from (if any)

None.

## Goals

### Half 1 — telemetry assembly

- **`telemetry.New` in production.** Once `bootDevStack` has the redactor +
  bus, it constructs
  `telemetry.New(cfg.Telemetry, red, telemetry.WithBusEmitter(eventbus.New(bus)))`
  and threads the resulting Logger into the stack's subsystems as the
  request-scoped logger source (the pre-redactor boot logger remains only
  for the bootstrap window — documented, narrow). `Logger.Error` paths now
  emit the paired `runtime.error` event per RFC §6.14. Devstack D-094
  mirror in the same PR. (If Wave B's 110d `Assemble` has merged, the
  wiring lands once there.)
- **RunErrorHandler wired.** The flow construction seam
  (`internal/runtime/flow`) accepts and forwards a run-error handler to
  `engine.New(engine.WithRunErrorHandler(...))`; the production assembly
  passes a handler invoking `telemetry.Logger.Error` — making
  `options.go:114-127`'s godoc true. The Wave A chore's godoc-honesty edit
  on that site is superseded by the real wiring.
- **`telemetry.BridgeBusToTracer(ctx, bus, tracer, f events.Filter) (stop func(), err error)`**
  — symmetric in shape and lifecycle with `BridgeBusToMetrics`
  (subscribe → drain goroutine → stop func; fail-loud on nil bus/tracer).
  Span model (implementor-owned within these rails, brief 06 "span
  lifecycle from events" as the guide): canonical lifecycle pairs
  (task.started/completed/failed, tool lifecycle, llm call lifecycle) open
  and end spans; non-lifecycle events attach as span events on the
  enclosing context; identity + run/task IDs ride as span attributes;
  `Event.TraceID` aligns with the propagation conventions
  (`internal/telemetry/propagation.go`).
- **`NewTracer` constructed in production** when telemetry config selects a
  span exporter (the noop driver remains the no-collector default — the
  blank imports at `main.go:101-104` finally resolve to something
  constructed); the bridge starts alongside the metrics bridge and joins
  the closer chain.
- **Recipe.** `docs/recipes/observe-an-embedded-runtime.md`: the
  audit→bus→logger→metrics→traces assembly order, the required blank
  imports, the redactor-mandatory rule, and the two bridges — headless
  first, binary second.

### Half 2 — approval authorizer seam

- **`approval.GateDeps.Authorizer`** — a small injected seam
  (`type ResolveAuthorizer interface { AuthorizeResolve(ctx context.Context, pending PendingInfo) error }`
  or the func equivalent; implementor picks, keeping GateDeps's
  "production binary wires all" posture):
  - **Package default (direct construction / runtime control path):** an
    identity-tuple check in runtime vocabulary — the resolving ctx carries
    either the pause's originating identity or an elevated control-scope
    claim (evaluate and prefer the existing
    `internal/runtime/registry.WithControlScope` precedent,
    `registry.go:340-349`, over minting a new claim shape).
  - **Protocol-edge implementation:** the protocolauth scope check
    (admin OR console:fleet — today's exact behaviour) moves OUT of the
    approval package into a one-way adapter owned by the Protocol/server
    side (e.g. alongside the server's gate wiring), injected when the gate
    is assembled for wire-driven resolution. Wire behaviour is unchanged;
    only the layering moves.
  - `internal/tools/approval` drops its `internal/protocol/auth` import
    entirely; the steering bridge's self-elevation
    (`apply.go:373-385`'s `protocolauth.WithScopes` block) is DELETED — the
    bridge resolves through the runtime-vocabulary default.
  - Nil-authorizer construction fails loud (`ErrInvalidConfig`-shaped) —
    an approval gate with no resolve privilege check is a misconfiguration,
    not a permissive mode.
- **Direction rule recorded.** D-203 (reserved; logged when the phase
  ships) records: *runtime packages may import `internal/protocol/types`
  (pure data projection); they must never import protocol auth / methods /
  transports (behaviour)* — with this phase's removal as the closing of the
  one standing violation (audit §4's otherwise-clean direction check).

## Non-goals

- No event-taxonomy changes, no new event types (the bridges derive; they
  do not mint).
- No OTel logs exporter / OTLP-logs pipeline — slog stays the log edge
  (RFC §6.14's settled "one logger").
- No span-per-chunk streaming traces (chunk volume × span cost; chunks
  remain span events at most — implementor may exclude them via the bridge
  filter, documented).
- No approval-gate behaviour change at the wire: Protocol-driven
  APPROVE/REJECT keeps today's scope requirements exactly — only WHERE the
  check lives moves.
- No mechanical import-direction linter (a depguard rule for the recorded
  direction rule is a worthy follow-up; this phase records the rule and
  closes the violation, and notes the linter in D-203).

## SDK-consumer reachability

This phase is most of what `observe-an-embedded-runtime.md` needs to stop
being unwritable (audit §7: "until the promotions land, the recipe cannot
honestly be written"). A headless consumer composes: redactor →
`telemetry.New(cfg, red, WithBusEmitter(...))` → `BridgeBusToMetrics` +
`BridgeBusToTracer` — all exported, no binary. On the approval half, a
headless embedder constructing gates directly no longer needs to import
protocol auth vocabulary (or know `protocolauth.WithScopes` folklore) to
resolve its own approvals — the default authorizer speaks the identity tuple
the embedder already has.

## Acceptance criteria

### Telemetry half

- [ ] **§13 primitive-with-consumer:** `telemetry.New` gains its first
      production caller (the assembly); `WithRunErrorHandler` gains its
      production pass-through + caller; `NewTracer` is constructed on the
      production path when configured — all in the same phase as the new
      bridge, exercised end-to-end with tests.
- [ ] `Logger.Error` on a production-shaped stack emits the slog record AND
      the paired `runtime.error` bus event (RFC §6.14 made true; asserted
      via the bus in the integration test).
- [ ] The flow seam forwards a run-error handler to
      `engine.WithRunErrorHandler`; a terminal node failure on a
      flow-as-tool run reaches `telemetry.Logger.Error` (asserted).
- [ ] `telemetry.BridgeBusToTracer` ships: shape/lifecycle symmetric with
      `BridgeBusToMetrics` (stop func, fail-loud nil checks, drain-until-
      close); lifecycle events open/end spans; identity + run/task IDs as
      span attributes; verified against an in-memory span exporter
      (`WithSpanExporter` test seam).
- [ ] Both bridges started by the production assembly (+ devstack mirror)
      and joined on shutdown; goroutine-leak test green.
- [ ] No metrics-cardinality regression: the metrics derivation still never
      tags by RunID/TraceID (existing guard re-asserted).
- [ ] `docs/recipes/observe-an-embedded-runtime.md` ships (headless
      assembly order + blank imports + both bridges).

### Approval half

- [ ] `GateDeps` carries the injected authorizer; nil fails loud at
      construction; the package default is the runtime-vocabulary check
      (identity-tuple / control-scope, `WithControlScope` precedent
      evaluated and the choice documented).
- [ ] `internal/tools/approval` has NO `internal/protocol/auth` import
      (grep-asserted in the smoke); the protocolauth check lives in a
      Protocol-side adapter injected at server-side gate assembly; wire
      behaviour (admin/console:fleet required for Protocol-driven resolve)
      is unchanged — existing Phase 31/54 tests still pass.
- [ ] `apply.go:373-385`'s self-elevation block is deleted; the steering
      bridge resolves approvals through the default authorizer; the D-097
      bridge E2E (approve + reject via the control surface) still passes.
- [ ] Failure mode: a resolve ctx with neither matching identity nor
      control scope is rejected loudly (the seam's replacement for
      `ErrApprovalScopeRequired` — same fail-closed posture, runtime
      vocabulary).
- [ ] The direction rule (types yes; auth/methods/transports never) is
      recorded in D-203 and referenced from the approval package godoc.

### Shared

- [ ] `scripts/smoke/phase-111f.sh` asserts both halves (see Smoke script
      additions).
- [ ] D-203 (reserved; logged when the phase ships).

## Files added or changed

- `cmd/harbor/cmd_dev.go` — `telemetry.New` construction + threading;
  tracer + bridges start; closer-chain joins.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `internal/telemetry/tracebridge.go` — **NEW** `BridgeBusToTracer` (+
  `tracebridge_test.go`).
- `internal/runtime/flow/flow.go` — run-error-handler pass-through option.
- `internal/runtime/engine/options.go` — godoc stays; now true.
- `internal/tools/approval/gate.go` — authorizer seam; protocolauth import
  removed; package godoc records the direction rule pointer.
- `internal/tools/approval/authorizer.go` — **NEW** default
  runtime-vocabulary authorizer (+ tests).
- `internal/server/` (or the Protocol-side gate-wiring site the implementor
  locates) — the protocolauth-backed authorizer adapter, injected at
  wire-side assembly.
- `internal/runtime/steering/apply.go` — self-elevation deleted.
- `docs/recipes/observe-an-embedded-runtime.md` — **NEW**;
  `docs/recipes/README.md` — index entry.
- `test/integration/phase111f_telemetry_test.go` +
  `phase111f_approval_seam_test.go` — the two E2Es (one file acceptable).
- `scripts/smoke/phase-111f.sh` — real assertions.
- `docs/decisions.md` — D-203 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `telemetry.BridgeBusToTracer(ctx, bus events.EventBus, tracer *Tracer, f events.Filter) (stop func(), err error)`.
- `approval.GateDeps.Authorizer` (+ the exported default constructor and
  the Protocol-side adapter's constructor).
- Behavioural surface: `runtime.error` events paired with `Logger.Error`;
  OTel spans derived from the bus when tracing is configured.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future
> facade/export RFC (audit §5 / Wave D), out of scope for this phase.

## Test plan

- **Unit:** trace bridge — lifecycle pairing, span attributes, filter
  honoured, nil-bus/nil-tracer fail-loud, stop-func idempotence (mirror the
  `BridgeBusToMetrics` test shape); default authorizer — originating
  identity passes, foreign identity fails, control-scope passes;
  protocolauth adapter — preserves today's admin/console:fleet matrix.
- **Integration:** telemetry E2E — real drivers (audit patterns redactor,
  inmem bus, in-memory span exporter), boot-shaped assembly,
  `Logger.Error` → slog + `runtime.error` pairing, a failed flow run
  reaching the handler, spans observed for a task lifecycle, identity
  propagation on events AND span attributes, failure mode: redactor-missing
  construction fails loud. Approval E2E — the full D-097 wire path
  (control approve/reject → steering drain → bridge → gate → typed
  `pause.resumed`) green WITHOUT the self-elevation; plus the
  fail-closed foreign-resolver case.
- **Conformance:** N/A — no driver seam added (the tracer's exporter
  drivers already exist; the bridge is a single implementation).
- **Concurrency / leak:** N≥100 concurrent event publishes through both
  bridges under `-race`; N≥100 concurrent ResolveApproval attempts (mixed
  authorized/unauthorized) against one gate under `-race` — exactly-once
  resolution preserved; goroutine baselines restored after both bridges +
  gate close.

## Smoke script additions

`scripts/smoke/phase-111f.sh`:

- Static: `internal/tools/approval` contains no `protocol/auth` import
  (grep gate — the direction rule's mechanical tripwire until a depguard
  rule lands); `telemetry.New` has a non-test caller under `cmd/` (or the
  promoted assembly); `apply.go` contains no `protocolauth.WithScopes`.
- `go test ./internal/telemetry/... ./internal/tools/approval/... -run
  'Bridge|Authoriz'` green; `go test ./test/integration/ -run Phase111f`
  green.
- Live (when classified live-server): the preflight server log shows the
  telemetry logger's structured output; a forced error path surfaces a
  `runtime.error` event on the events stream. 404/405/501 → SKIP
  pre-phase.

## Coverage target

- `internal/telemetry`: 85% (bridge fully covered).
- `internal/tools/approval`: 90% (package's existing high bar; the seam +
  both authorizers covered).
- `internal/runtime/steering`: 85%.

## Dependencies

- 03 (audit redactor — the Logger's mandatory dep), 04 (telemetry Logger),
  05 (event bus), 31 (approval gates), 55 (OTel tracing), 56 (OTel
  metrics — the bridge symmetry target), 54 (Protocol edge auth — the wire
  behaviour preserved).
- The D-192 steering-dispatch fix (Wave A): the approval-half E2E exercises
  the planner-dispatched gated-tool path that only works post-fix.

## Risks / open questions

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111f has no hard 110-band dependency (the
  assembly wiring site moves into `Assemble` if 110d lands first — §17.6
  covers it); all six 111-band phases are mutually independent.
- **Two halves, one phase.** They share nothing but the audit section and
  the assembly touchpoint. If implementation scope balloons, the approval
  seam is cleanly severable into a follow-up PR within the same phase
  number (the plan's ACs are grouped to permit it); do not let half 2's
  layering work stall half 1's observability wiring.
- **Boot-window logging.** A short bare-slog window before the redactor
  exists is unavoidable (config load errors must print). The plan accepts
  it, bounded and documented; nothing identity- or payload-shaped is
  logged in that window.
- **Span volume.** A chatty bus produces span floods on busy runs; the
  bridge takes an `events.Filter` precisely so production assembly can
  scope to lifecycle events. Default filter choice is implementor-owned and
  documented in the recipe.
- **Authorizer default strictness.** The runtime-vocabulary default must
  not be WEAKER than today's check on any reachable path: the steering
  bridge path today requires the Protocol edge's scope enforcement before
  the inbox — the new default re-checks identity/control-scope at the
  gate, preserving defence-in-depth. The integration matrix asserts no
  path got more permissive.

## Glossary additions

- **Bus→tracer bridge** — `telemetry.BridgeBusToTracer`, the OTel-span
  derivation of the canonical event bus, symmetric with the metrics bridge.
  Add to `docs/glossary.md`.
- **Resolve authorizer** — the injected privilege check on
  `approval.GateDeps` deciding who may resolve a pending approval: runtime
  identity/control-scope by default, Protocol scopes via the server-side
  adapter. Add to `docs/glossary.md`.
- **Protocol import direction rule** — runtime packages may import
  `internal/protocol/types` (data), never protocol auth/methods/transports
  (behaviour). Recorded in D-203. Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation: span attributes + approval resolution stay
      identity-scoped (asserted)
- [ ] **Primitive + consumer in the same wave (§13):** `telemetry.New`,
      `WithRunErrorHandler`, `NewTracer`, and the new trace bridge all gain
      production consumers in this phase, exercised end-to-end with tests —
      checked.
- [ ] Concurrent-reuse tests pass (bridges + gate, N≥100, `-race`)
- [ ] Integration tests wire real drivers end-to-end, assert identity
      propagation, cover ≥1 failure mode, run under `-race`
- [ ] Wire-side approval behaviour unchanged (Phase 31/54 suites green)
- [ ] Glossary updated
- [ ] D-203 filed when the phase ships (incl. the direction rule)
