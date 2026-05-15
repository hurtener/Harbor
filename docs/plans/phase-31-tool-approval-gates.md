# Phase 31 — Tool-side approval gates

## Summary

Phase 31 ships synchronous "approve this tool call" gates that pause a run via
the unified pause/resume primitive (Phase 50) and resume on a Protocol
`approve` / `reject` (Phase 54). Distinct from Phase 30's OAuth gate (no
token, no third-party authorization server, no URL flow): the approval gate
asks a question and waits for an admin / fleet-control caller to say
APPROVE or REJECT. REJECT raises a typed `tool.rejected` event with the
verified identity triple; APPROVE resumes the run and the tool proceeds.

## RFC anchor

- RFC §6.4 — tool catalog and transports; "Tool-side OAuth + HITL uses the
  unified pause/resume primitive."
- RFC §3.3 — the unified pause/resume primitive (HITL approval / tool OAuth /
  A2A AUTH_REQUIRED / steering PAUSE — one primitive, four consumers).

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the pause-reason taxonomy + the
  nine control types including APPROVE / REJECT).
- brief 03 — tools + integrations + LLM client (the catalog seam approval
  gates wrap).
- brief 06 — events + observability + devx (the `tool.rejected` audit
  event the master plan calls out).

## Brief findings incorporated

- brief 02 §"Pause-reason taxonomy": Harbor preserves the four canonical
  pause reasons; `ReasonApprovalRequired` is the textbook home for HITL
  approval gates. Phase 31 emits exactly that reason on
  `Coordinator.Request`, NOT `ReasonExternalEvent` (which is what Phase 30
  uses for OAuth callbacks).
- brief 02 §"Pause-state serialisation (the contract that MUST FAIL
  LOUDLY)": no silent `None` / `nil` returns; an unserializable approval
  payload fails Request loud. The Coordinator already enforces this — the
  gate's `Payload` map flows through `trajectory.ValidateEncodable`
  unchanged.
- brief 03 §"Audit redaction lives in the audit subsystem": every
  approval-request payload runs through `audit.Redactor.Redact` BEFORE
  the bus publish. The post-approve tool call uses the ORIGINAL args
  (held in the gate's pending-call map, not in the event payload) so a
  redactor that elides a secret does not corrupt the executed tool call.
- brief 06 §"the single bus is the canonical record": `tool.rejected` is
  a first-class typed event with a SafePayload payload, NOT a string
  blob; the Console subscribes to it the same way it subscribes to
  `tool.auth_required`.
- brief 09 §"the unified pause/resume primitive — converge or die": Phase
  30's OAuth path proves the convergence pattern. Phase 31 mirrors the
  shape (`Coordinator.Request` at gate entry; `Coordinator.Resume` via
  the steering inbox's APPROVE / REJECT path; bus events on both ends).

## Findings I'm departing from (if any)

None.

## Goals

- A reusable, concurrent-safe `ApprovalGate` artifact tools opt into.
- `ApprovalPolicy` interface so operators declare WHICH tool calls
  require approval (per-tool, per-args, per-identity); a registry-wide
  default that approves nothing AND a "must-be-explicit" boot-time
  invariant (CLAUDE.md §13 amendment — no silent stub default).
- Convergence on the Phase 50 Coordinator and the Phase 53 steering
  inbox — APPROVE / REJECT control events land at the inbox, advance
  the pause, and unblock the gate.
- Typed `tool.approval_requested` (observability) + `tool.rejected`
  (the master-plan acceptance criterion) + `tool.approved` events;
  every payload is SafePayload + audit-redacted at the source.
- Scope-gated resolution: a caller without `auth.ScopeAdmin` or
  `auth.ScopeConsoleFleet` (Phase 61) CANNOT approve a pending gate.
  Phase 31's resolution helper enforces this in-process; the Phase 54
  Protocol edge enforces it at the JWT boundary.
- A typed `*ErrToolRejected` sentinel callers can `errors.As` against.

## Non-goals

- A Console UI rendering the approval queue (Console-side work; the
  events surface is the contract, the rendering is downstream).
- Per-tool descriptor metadata that AUTOMATICALLY wraps tools — Phase
  31 ships the gate as an explicit wrapper / middleware, opt-in.
  Auto-installation lives in a later phase that wires the gate into
  the dispatch trio (RFC §6.4).
- Multi-approver flows (M-of-N approvals, escalation, time-window
  policies). The V1 shape is single-approver with a free-form policy
  predicate; richer flows are post-V1.
- Persistence of pending approvals separate from the pause record.
  The Coordinator's checkpoint store carries the approval state
  (because the pause record carries it); a fresh `Coordinator` over
  the same `StateStore` rehydrates the gate's state from the
  Coordinator's payload — Phase 31 does NOT mint a second persistence
  driver.

## Acceptance criteria

- [ ] An `ApprovalGate.RunGuarded` (or `Invoke` middleware) call
      consults the configured `ApprovalPolicy`; when the policy
      returns `Required=true`, the gate parks the call via
      `Coordinator.Request(reason=ApprovalRequired)`, blocks on a
      per-gate resolution channel, and unblocks on APPROVE / REJECT
      via the Phase 53 inbox.
- [ ] APPROVE → the gate proceeds; the original tool args (held in the
      gate's pending map, not in the event payload) drive the
      invocation; `tool.approved` event publishes (observability).
- [ ] REJECT → the gate returns `*ErrToolRejected` (wrapping a typed
      reason string); `tool.rejected` typed event publishes with the
      verified identity triple in the Event envelope.
- [ ] An `ApprovalGate` constructed with NO policy refuses to invoke
      (`ErrPolicyRequired`); the §13 amendment forbids silent
      auto-approval.
- [ ] The approval-request event payload is audit-redacted via
      `audit.Redactor.Redact` BEFORE publish. The post-approve tool
      call uses the ORIGINAL args.
- [ ] Resolution helper `ResolveApproval(ctx, token, decision)`
      enforces `auth.HasScope(ctx, ScopeAdmin) || HasScope(ctx,
      ScopeConsoleFleet)`; a missing claim returns
      `ErrApprovalScopeRequired`.
- [ ] Cross-identity APPROVE / REJECT is rejected — a caller from
      tenant B cannot resolve a gate registered under tenant A.
      Coordinator's scope check enforces this; the gate surfaces the
      sentinel.
- [ ] Concurrent reuse: N≥100 concurrent invocations against ONE
      shared `ApprovalGate` instance under `-race`, no data races, no
      context bleed, no cancellation cross-talk, no goroutine leaks.
- [ ] Initiate-then-cancel goroutine-leak test: 25 cycles of
      pause-then-cancel-ctx-without-resolution → baseline
      `runtime.NumGoroutine()` restored.
- [ ] `test/integration/phase31_approval_gates_test.go` exercises the
      full APPROVE + REJECT cycle against real
      `pauseresume.Coordinator` + real `events.EventBus` + real
      `audit.Redactor` + real `steering.Inbox` — every layer the gate
      composes against is a production driver.
- [ ] `scripts/smoke/phase-31.sh` runs the unit + integration tests
      under `-race`; static guards on the package layout
      (`ApprovalPolicy` + `ApprovalGate` + `tool.rejected` event
      registration) and the §13-amendment "no default policy" boot
      invariant (`NewApprovalGate` rejects a nil policy).

## Files added or changed

- `internal/tools/approval/approval.go` — types: `ApprovalPolicy`,
  `ApprovalDecision`, `ApprovalRequest`, `*ErrToolRejected`,
  sentinels.
- `internal/tools/approval/gate.go` — `ApprovalGate` concrete
  artifact + `NewApprovalGate` constructor + the
  `Invoke` / `RunGuarded` surface + `ResolveApproval` helper.
- `internal/tools/approval/events.go` — `tool.approval_requested`,
  `tool.approved`, `tool.rejected` `EventType` + SafePayload shapes,
  plus `init()` registrations.
- `internal/tools/approval/policies.go` — `AlwaysApprovePolicy` (for
  tests only, gated by build-tag-free godoc), `AlwaysDenyPolicy`
  (for tests), `TaggedPolicy` (production reference — approve when
  every tag in the call's `Tags` is in an operator-supplied allow
  list).
- `internal/tools/approval/{approval,gate,events,policies}_test.go` —
  unit-test coverage per type.
- `internal/tools/approval/concurrent_test.go` — D-025 N≥100 stress.
- `test/integration/phase31_approval_gates_test.go` — §17
  integration test wiring real Coordinator + Bus + Redactor +
  steering Inbox; APPROVE + REJECT cycles; scope-gating failure
  mode; goroutine-leak; concurrency stress.
- `scripts/smoke/phase-31.sh` — phase smoke.
- `docs/glossary.md` — new entries: `approval.ApprovalGate`,
  `approval.ApprovalPolicy`, `approval.ErrToolRejected`,
  `tool.rejected`, `tool.approved`, `tool.approval_requested`.
- `docs/decisions.md` — D-086.
- `docs/plans/README.md` — Phase 31 row Status flip + detail block.
- `README.md` — Status row Phase 31 → Shipped.

## Public API surface

```go
// internal/tools/approval

type ApprovalDecision string
const (
    DecisionPending  ApprovalDecision = "pending"
    DecisionApprove  ApprovalDecision = "approve"
    DecisionReject   ApprovalDecision = "reject"
)

type ApprovalRequest struct {
    Tool      tools.Tool      // descriptor view; redacted before emit
    Args      json.RawMessage // ORIGINAL args; never put on the bus
    Identity  identity.Identity
    Tags      []string        // optional caller-supplied classification
}

type ApprovalPolicy interface {
    ShouldApprove(ctx context.Context, req *ApprovalRequest) (Required bool, Reason string, Err error)
}

type ErrToolRejected struct {
    Tool     string
    Reason   string
    Identity identity.Identity
}
func (e *ErrToolRejected) Error() string
func (e *ErrToolRejected) Is(target error) bool

var (
    ErrToolRejectedSentinel    = errors.New("approval: tool call rejected")
    ErrPolicyRequired          = errors.New("approval: ApprovalPolicy required")
    ErrApprovalScopeRequired   = errors.New("approval: admin or console:fleet scope required")
    ErrApprovalAlreadyResolved = errors.New("approval: already resolved")
    ErrApprovalCancelled       = errors.New("approval: cancelled before resolution")
)

type GateDeps struct {
    Policy      ApprovalPolicy            // mandatory
    Coordinator pauseresume.Coordinator   // mandatory
    Bus         events.EventBus           // mandatory
    Redactor    audit.Redactor            // mandatory
}

type ApprovalGate struct { /* unexported */ }
func NewApprovalGate(deps GateDeps) (*ApprovalGate, error)

// RunGuarded consults the policy; on Required=true it parks the run
// via Coordinator.Request(ApprovalRequired), publishes
// tool.approval_requested (redacted), and blocks on a per-pause
// resolution channel until APPROVE / REJECT arrive via the Phase 53
// inbox (which calls Coordinator.Resume with the rejected:true
// marker for REJECT). On APPROVE it returns (Args, nil); on REJECT
// it returns (nil, *ErrToolRejected).
//
// On policy "no approval needed" it returns (req.Args, nil)
// immediately — the caller proceeds to invoke the tool.
//
// Concurrent-safe (D-025): one *ApprovalGate is shared by N runs.
func (g *ApprovalGate) RunGuarded(ctx context.Context, req *ApprovalRequest) (json.RawMessage, error)

// ResolveApproval is the in-process resolution helper. The Phase 53
// inbox + Phase 54 Protocol edge wires APPROVE / REJECT through this
// — but in-process callers (tests, future Console-collocated runtimes)
// use it directly. Enforces auth.HasScope(ctx, ScopeAdmin) or
// auth.HasScope(ctx, ScopeConsoleFleet) — a non-elevated caller is
// rejected with ErrApprovalScopeRequired.
func (g *ApprovalGate) ResolveApproval(ctx context.Context, token pauseresume.Token, decision ApprovalDecision, reason string) error
```

## Test plan

- **Unit:**
  - `approval_test.go`: `ApprovalRequest` validation; `ErrToolRejected`
    `errors.Is` against the sentinel; `ApprovalDecision` enum bounds.
  - `policies_test.go`: `AlwaysApprovePolicy` / `AlwaysDenyPolicy` /
    `TaggedPolicy` behavior; nil-request guard.
  - `gate_test.go`: happy approve path; happy reject path; nil-policy
    rejection at NewApprovalGate; missing Coordinator / Bus / Redactor
    rejection; redactor error fails-loud (no silent emit-skip);
    identity-missing fails closed.
  - `events_test.go`: every event type registered in
    `events.IsValidEventType`; SafePayload guarantee on every payload.

- **Integration:** `test/integration/phase31_approval_gates_test.go`
  - Real `pauseresume.Coordinator` + real `events.EventBus` (inmem) +
    real `audit.Redactor` (patterns driver) + real `steering.Inbox`.
  - APPROVE round-trip: a planner-style "I want to call tool X" path
    pauses via the gate; an admin-scope caller submits APPROVE through
    `inbox.Enqueue(ControlApprove)`; the runloop's `applier.applyEvent`
    calls `Coordinator.Resume`; the gate unblocks; the original args
    are returned; `tool.approved` event observed on the bus.
  - REJECT round-trip: same shape; the inbox APPROVE arm is REJECT
    instead; the gate returns `*ErrToolRejected`; `tool.rejected`
    event observed.
  - Scope-gating failure mode: a non-admin / non-console-fleet caller
    calling `ResolveApproval` is rejected with `ErrApprovalScopeRequired`.
  - Cross-identity rejection: a tenant-B caller cannot resolve a
    tenant-A pause (the Coordinator's `ErrScopeMismatch` propagates
    through `ResolveApproval`).
  - Goroutine-leak: 25 cycles of pause-then-cancel-ctx without
    resolution → `runtime.NumGoroutine()` baseline-restored.
  - Concurrency stress: N=16 distinct identity stacks each approve
    one call concurrently — no cross-talk.
  - Identity propagation asserted everywhere.

- **Conformance:** N/A — Phase 31 introduces no new persistence-shaped
  interface (the gate consumes the Coordinator, which consumes the
  existing `state.StateStore` seam via D-067).

- **Concurrency / leak:** `concurrent_test.go` — N≥100 concurrent
  `RunGuarded` invocations against ONE shared `*ApprovalGate` under
  `-race`. Policy yields a random Required across the 100; both
  approve and reject paths exercised; no goroutine leak; no
  cross-context bleed (every invocation's returned identity matches
  its caller's identity).

## Smoke script additions

- `scripts/smoke/phase-31.sh`:
  - Run `go test -race -count=1 -timeout 180s ./internal/tools/approval/...`.
  - Run `go test -race -count=1 -timeout 180s -run TestE2E_Phase31 ./test/integration/...`.
  - Static guard: `internal/tools/approval/approval.go` defines
    `ApprovalPolicy` + `ApprovalGate`.
  - Static guard: `internal/tools/approval/events.go` declares
    `EventTypeToolRejected = "tool.rejected"` and registers it.
  - Static guard: `NewApprovalGate` rejects a nil `Policy` (the §13
    amendment trip-wire — grep for the explicit nil-check).
  - HTTP/Protocol surface skips per the 404/405/501 convention until
    Phase 64's `harbor dev` lands.

## Coverage target

- `internal/tools/approval/`: 80%

## Dependencies

- 30 — Phase 30 is the sibling consumer of the Coordinator + events
  surface; Phase 31 mirrors the patterns (typed sentinel, SafePayload
  events, audit-redacted before emit, identity-mandatory).

## Risks / open questions

- The convergence on the steering inbox + Coordinator means a bug in
  Phase 53's APPROVE / REJECT dispatch would surface as a Phase 31
  gate-not-unblocking. §17.6 expectation: if the integration test
  surfaces a Phase 53 bug, fix it in this PR.
- Policy-as-Go-interface vs. policy-as-config-YAML: Phase 31 ships the
  Go interface (operators write a tiny Go `ApprovalPolicy`
  implementation, or use the bundled `TaggedPolicy`). A future phase
  may add a YAML / DSL surface; that is post-V1 and does not change
  the interface shape settled here.

## Glossary additions

- `approval.ApprovalGate`
- `approval.ApprovalPolicy`
- `approval.ErrToolRejected`
- `tool.approval_requested`
- `tool.approved`
- `tool.rejected`

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test
      passes — `concurrent_test.go::TestApprovalGate_ConcurrentReuse_NoCrossTalk`
      runs N=128 concurrent invocations under `-race`.**
- [x] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists in
      `test/integration/phase31_approval_gates_test.go` — real drivers,
      identity propagation, scope-gate + cross-identity failure modes,
      goroutine-leak, concurrency stress.**
- [x] If new vocabulary: glossary updated (six new entries)
- [x] If a brief finding was departed from: justified above + decisions.md
      entry filed (no departures; D-086 records the design)
