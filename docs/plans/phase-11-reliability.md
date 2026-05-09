# Phase 11 — Reliability shell

## Summary

Layer the reliability shell onto Phase 10's engine: per-node `NodePolicy{Validate, TimeoutMS, MaxRetries, BackoffBase, BackoffMult, MaxBackoff}`, the structured `RunError` envelope, and the worker-loop wrapper that applies them. Errors route to the Protocol unconditionally (via Phase 04's logger + Phase 05's bus); egress emission is opt-in via `engine.WithErrorEmissionToEgress(true)`. Backoff math is exponential with jitter (`base * 2^attempt + jitter`, capped at `MaxBackoff`). Phase 11 is the first phase that gives operators visibility into runtime failures with enough context to debug offline.

## RFC anchor

- RFC §6.1
- RFC §5

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §4 — reliability shell.** "`_execute_with_reliability` wraps the node call with timeout, retry-with-backoff, and run-cancel checks. On terminal failure it constructs a `RunError` and optionally routes it to the egress (`emit_errors_to_rookery`). Harbor: keep the shell, keep the optional error-to-egress, and add an 'error-to-protocol' hook so Console can render failures without the egress consumer needing to handle them." Phase 11 implements exactly this: the shell wraps the node call inside the worker loop; `RunError` always goes to the bus + logger; egress emission is opt-in.
- **brief 01 §2 — `NodePolicy` validate modes.** `both / in / out / none` — the perf escape hatch (`none` on hot streaming paths) is necessary and Phase 11 keeps it. Validation in V1 is a function pointer (`Validate func(env messages.Envelope) error`); JSON-Schema validation lands at the Protocol edge in Phase 60. Per RFC §6.1 brief 01 Q-3 settled: "(c) generics-typed nodes for the typed core, (b) JSON-schema reserved for protocol-edge ingress."
- **brief 01 §6 — backoff math.** "Backoff calculation given various `attempt`/`MaxBackoff` combinations." Phase 11 ships exponential-with-jitter: `min(BackoffBase * 2^attempt + uniform_jitter, MaxBackoff)`. The unit test enumerates the calculation across attempt counts to pin the math.
- **brief 01 §5 — sharp edges.** Type-mismatch is a hard error, not `warnings.warn`. Phase 11 returns `RunError(NodeException)` when a node returns the wrong shape. The predecessor's "log-and-continue" pattern is rejected.
- **RFC §6.1 — error routing.** Errors go to the Protocol unconditionally (resolves brief 01 Q-5). Phase 11 wires the always-on path through Phase 04's logger + Phase 05's bus (via the Phase 04→05 eventbus adapter Wave 2 shipped). Egress emission stays opt-in via Phase 10's `WithErrorEmissionToEgress` option.
- **RFC §6.1 — resilience composition.** Per-node retry/backoff/timeout/validation come from `NodePolicy`. Per-flow `flow.Budget` (deadline / hop / cost cap) lands at Phase 26a, NOT Phase 11. Phase 11 ships only per-node — the engine has no notion of "flow boundary" yet.
- **RFC §6.1 — `RunError` shape.** "Structured error envelope" per brief 01 §2 — carries `RunID`, `NodeName`, `NodeID`, `Code`, `Message`, `Cause`, `Metadata`. Phase 11 ships exactly these fields plus a `ToPayload() map[string]any` helper for the bus.
- **D-025 — concurrent reuse.** The reliability shell adds NO mutable state to the `Engine` struct: backoff state lives on the worker stack (per-invocation); validate-function pointers live in the immutable `NodePolicy`. The N≥100 reuse test from Phase 10 still passes; Phase 11 extends it to assert that retries don't cross-talk between concurrent invocations.

## Findings I'm departing from (if any)

- **None.** Phase 11 follows brief 01 + RFC §6.1 verbatim. The biggest decision point was V1 validation strategy (JSON-Schema vs Go-generic vs hybrid) — RFC §6.1 settles brief 01 Q-3 in favor of "function-pointer validators in the typed core; JSON-Schema reserved for the Protocol edge". Phase 11 ships function-pointer validators only; Phase 60 will introduce JSON-Schema at the wire boundary.

## Goals

- Wrap Phase 10's worker loop with the reliability shell so node failures produce a structured `RunError` instead of unwinding the worker.
- Pin the backoff math: exponential with jitter, capped at `MaxBackoff`, attempt count respects `MaxRetries`. Wrong math is the easiest place to ship a subtle production bug; the unit test enumerates the calculation explicitly.
- Route errors to the Protocol unconditionally — `Logger.Error` (Phase 04) + `runtime.error` event on the bus (Phase 05). Egress emission is opt-in (Phase 10's `WithErrorEmissionToEgress`).
- Validate modes: `both / in / out / none`. The `none` escape hatch is documented but the default is `both` — fail-loud per Harbor's runtime principles (CLAUDE.md §5 "Fail loudly").
- Identity propagation through `RunError`: every `RunError` carries the full quadruple from the failing envelope, so audit logs and bus subscribers can scope by tenant/user/session/run.

## Non-goals

- No streaming primitive — Phase 12.
- No cancellation — Phase 13. (Phase 11's worker still observes ctx cancellation — the shell respects `ctx.Done()` between retries — but `Cancel(runID)` itself is Phase 13.)
- No routers — Phase 14.
- No JSON-Schema validation. Validators are `func(env messages.Envelope) error` only; JSON-Schema lands at the Protocol edge in Phase 60.
- No per-flow `flow.Budget` (deadline / hop / cost cap). Phase 26a wires those.
- No `Cause` chaining beyond the immediate parent. `RunError.Cause error` carries one level; deeper chains use `errors.Unwrap` on the wrapped error. Documented.
- No retry-with-feedback (the Phase 36 LLM-side retry loop). Phase 11's retries are unconditional within the policy's bounds.

## Acceptance criteria

- [ ] `internal/runtime/engine/policy.go` defines `NodePolicy{Validate ValidateMode, TimeoutMS int, MaxRetries int, BackoffBase time.Duration, BackoffMult float64, MaxBackoff time.Duration, ValidateFunc func(messages.Envelope) error}`. Zero-valued `NodePolicy` is "no timeout, no retry, no validate, no backoff" — i.e. Phase 10's bare worker behavior. `ValidateMode` is a string-typed enum with constants `ValidateBoth / ValidateIn / ValidateOut / ValidateNone`.
- [ ] `internal/runtime/engine/runerror.go` defines `RunError{RunID, NodeName, NodeID string, Code RunErrorCode, Message string, Cause error, Metadata map[string]any}`. `Code` is a string-typed enum: `NodeTimeout`, `NodeException`, `RunCancelled`, `DeadlineExceeded`, `ValidationFailed`. `RunError.Error() string`, `RunError.Unwrap() error`, `RunError.ToPayload() map[string]any` for bus emission.
- [ ] **Backoff math:** `internal/runtime/engine/backoff.go` ships `func nextBackoff(attempt int, base, max time.Duration, mult float64, rand func() float64) time.Duration` returning `min(base * mult^attempt + jitter, max)` where `jitter` is `0..base*0.1`. Test `TestBackoff_Math_Enumerated` pins the calculation across attempt 0..5 with deterministic `rand`.
- [ ] **Reliability shell.** Phase 10's worker loop wraps node invocation with the shell: validate-in (per `Policy.Validate`) → invoke-with-timeout (`context.WithTimeout(ctx, time.Duration(Policy.TimeoutMS) * time.Millisecond)`) → on err, increment attempt, sleep `nextBackoff(...)`, retry up to `Policy.MaxRetries` → on terminal failure, build `RunError`, emit to logger + bus, optionally emit to egress.
- [ ] **`Validate=both` rejects malformed envelopes.** Test `TestNodePolicy_ValidateBoth_RejectsMalformedIn` and `TestNodePolicy_ValidateBoth_RejectsMalformedOut`. The validator is a function pointer; if it returns an error, the worker shapes a `RunError(ValidationFailed)` and skips invocation.
- [ ] **`Validate=none` escape hatch.** Test `TestNodePolicy_ValidateNone_SkipsValidator` confirms the validator is not called. Documented as the perf escape hatch for hot streaming paths (Phase 12).
- [ ] **Timeout produces `RunError(NodeTimeout)`.** Test `TestNodePolicy_Timeout_ProducesRunError` constructs a node that sleeps longer than `TimeoutMS`; asserts the `RunError.Code == NodeTimeout` and the worker did NOT block beyond the timeout.
- [ ] **`MaxRetries` bound respected.** Test `TestNodePolicy_MaxRetries_StopsAfterN` constructs a node that always errors; asserts the worker invokes it `MaxRetries+1` times (initial + retries) and then emits `RunError`.
- [ ] **Errors route to Protocol unconditionally.** Test `TestRunError_RoutesToBus` asserts an admin subscriber receives a `runtime.error` event whose payload is the `RunError.ToPayload()` shape. The bus integration uses the Wave 2-shipped eventbus adapter; no new wiring at this layer.
- [ ] **Errors route to logger.** Test `TestRunError_RoutesToLogger` asserts `Logger.Error` is called with the standard attribute set (tenant_id, user_id, session_id, run_id, task_id) and the error's structured fields.
- [ ] **Egress emission opt-in.** Test `TestRunError_EgressEmission_DisabledByDefault` asserts `Fetch` does NOT return a `RunError`-shaped envelope when `WithErrorEmissionToEgress(true)` was NOT passed. The matching `TestRunError_EgressEmission_EnabledByOption` test confirms the opt-in path works.
- [ ] **Identity propagation in `RunError`.** Test `TestRunError_CarriesIdentity` asserts every error event carries the full quadruple of the failing envelope.
- [ ] **`ctx.Done()` respected between retries.** Test `TestNodePolicy_CtxCancelled_AbortsRetries` cancels the worker's ctx mid-retry-sleep; asserts the retry loop returns immediately without calling the node again.
- [ ] No package-level mutable state. The shell's per-invocation state (attempt count, last error) lives on the worker stack. Phase 10's reuse test extended: `TestEngine_ConcurrentReuse_ReuseContract_WithPolicy` wraps the same N=100 emitters but with retry-prone nodes; asserts no race / no leak / no cross-run state.
- [ ] Coverage on the new files (`policy.go`, `runerror.go`, `backoff.go`, plus the worker-loop changes in `engine.go`) ≥ 85%. Phase 10's overall coverage stays ≥ 85%.
- [ ] **Concurrent-reuse test (D-025):** the policy/shell additions are immutable per-invocation state on the worker stack. `TestEngine_ConcurrentReuse_ReuseContract_WithPolicy` is the extended D-025 test.
- [ ] **Integration test:** `test/integration/runtime_engine_test.go` (extends Phase 10's) — adds a failing-node scenario; asserts the bus subscriber sees the `runtime.error` event with the right `RunError.Code`.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-11.sh` smoke runs Phase 10's smoke + Phase 11-specific tests.

## Files added or changed

```text
internal/runtime/engine/policy.go          # NodePolicy, ValidateMode constants
internal/runtime/engine/runerror.go        # RunError, RunErrorCode constants, ToPayload, Unwrap
internal/runtime/engine/backoff.go         # nextBackoff math
internal/runtime/engine/shell.go           # the reliability-shell wrapper around NodeFunc
internal/runtime/engine/engine.go          # worker loop wires the shell in
internal/runtime/engine/policy_test.go     # validate / timeout / retry tests
internal/runtime/engine/backoff_test.go    # backoff math tests
internal/runtime/engine/runerror_test.go   # RunError shape + identity propagation
test/integration/runtime_engine_test.go    # extended for failing-node scenario
scripts/smoke/phase-11.sh                  # Go-package + integration test invocation
docs/plans/README.md                       # Status: 11 → Shipped (in implementation PR)
docs/glossary.md                           # adds NodePolicy, RunError, ValidateMode, RunErrorCode
internal/events/events.go                  # NEW: register runtime.error EventType (if not already)
internal/runtime/engine/payloads.go        # NEW: RunErrorPayload — events.SafePayload? — bus payload
```

## Public API surface

```go
package engine

// NodePolicy controls per-node reliability semantics. Zero-value is
// "no policy" — Phase 10's bare worker behavior. Construct explicitly
// for production nodes; the engine never silently applies defaults
// (per AGENTS.md "No silent degradation").
type NodePolicy struct {
    Validate     ValidateMode
    TimeoutMS    int
    MaxRetries   int
    BackoffBase  time.Duration
    BackoffMult  float64
    MaxBackoff   time.Duration
    ValidateFunc func(messages.Envelope) error
}

type ValidateMode string

const (
    ValidateBoth ValidateMode = "both"
    ValidateIn   ValidateMode = "in"
    ValidateOut  ValidateMode = "out"
    ValidateNone ValidateMode = "none"
)

// RunError is the structured error envelope emitted on terminal node
// failure. Carries the full identity quadruple via RunID + the per-
// invocation context (NodeName, NodeID). Cause carries one level;
// deeper unwrapping uses errors.Unwrap on Cause.
type RunError struct {
    RunID    string
    NodeName string
    NodeID   string
    Code     RunErrorCode
    Message  string
    Cause    error
    Metadata map[string]any
}

func (e *RunError) Error() string
func (e *RunError) Unwrap() error
func (e *RunError) ToPayload() map[string]any

type RunErrorCode string

const (
    NodeTimeout       RunErrorCode = "node_timeout"
    NodeException     RunErrorCode = "node_exception"
    RunCancelled      RunErrorCode = "run_cancelled"
    DeadlineExceeded  RunErrorCode = "deadline_exceeded"
    ValidationFailed  RunErrorCode = "validation_failed"
)
```

## Test plan

- **Unit:** `TestBackoff_Math_Enumerated` (attempt 0..5, deterministic rand), `TestBackoff_RespectsMax`, `TestNodePolicy_ZeroValue_BareWorker`, `TestNodePolicy_ValidateBoth_RejectsMalformedIn`, `TestNodePolicy_ValidateBoth_RejectsMalformedOut`, `TestNodePolicy_ValidateNone_SkipsValidator`, `TestNodePolicy_Timeout_ProducesRunError`, `TestNodePolicy_MaxRetries_StopsAfterN`, `TestNodePolicy_CtxCancelled_AbortsRetries`, `TestRunError_ToPayload_Roundtrip`, `TestRunError_CarriesIdentity`, `TestRunError_Unwrap_OneLevel`.
- **Integration:** `test/integration/runtime_engine_test.go` extended — failing-node scenario, bus subscriber asserts `runtime.error`. Real audit + events + state + sessions + engine; identity propagation; failure mode exercised; `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** `TestEngine_ConcurrentReuse_ReuseContract_WithPolicy` (D-025 with retry-prone nodes), `TestEngine_NoGoroutineLeak_AfterStop_WithPolicy`.

## Smoke script additions

- `phase-11.sh`: runs Phase 10's smoke + Phase 11-specific tests (`go test -race ./internal/runtime/engine/... && go test -race -run TestE2E_Phase11 ./test/integration/...`).

## Coverage target

- `internal/runtime/engine`: 85%

## Dependencies

- 10 (engine — wraps the worker loop)
- 04 (logger — Phase 11 calls `Logger.Error`)
- 05 (events bus — Phase 11 emits `runtime.error`)

## Risks / open questions

- **Backoff jitter under load.** A coarse jitter source (`math/rand`) is fine for V1's single-process scope. If a future post-V1 driver introduces distributed retry, we may need crypto-grade jitter to avoid synchronized retry storms. Out of scope; documented.
- **`MaxRetries=0` semantics.** Means "no retries" — the node is invoked exactly once; on failure, immediate `RunError`. Matches the predecessor's behavior. Documented.
- **Timeout vs ctx cancellation precedence.** When a node's per-invocation ctx is cancelled by the operator (Phase 13's `Cancel`), the shell honors the cancellation immediately rather than waiting for `TimeoutMS`. The worker observes `ctx.Err() == context.Canceled` first; the shell maps to `RunError(RunCancelled)` not `RunError(NodeTimeout)`. Documented.
- **`Cause` chaining depth.** V1 carries one level. If the node's error wraps a deeper chain, `errors.Unwrap(RunError.Cause)` gets the next layer. Documented.

## Glossary additions

- **`NodePolicy`.** Per-node reliability config: `Validate`, `TimeoutMS`, `MaxRetries`, `BackoffBase / Mult / MaxBackoff`, `ValidateFunc`. Zero value is "no policy" (Phase 10's bare worker). Sensible defaults are set explicitly, not silently — fail-loud per AGENTS.md §5.
- **`RunError`.** Structured error envelope from the runtime's reliability shell. Carries `RunID`, `NodeName`, `NodeID`, `Code` (one of `node_timeout / node_exception / run_cancelled / deadline_exceeded / validation_failed`), `Message`, `Cause`, `Metadata`. Routes to logger + bus unconditionally; egress emission opt-in.
- **Reliability shell.** The worker-loop wrapper that applies `NodePolicy` per invocation: validate-in → invoke-with-timeout → retry-with-backoff → on terminal failure, emit `RunError` to logger + bus.
- **`ValidateMode`.** `both / in / out / none`. Per-node choice for whether the engine runs the validator on input, output, both, or skips it. `none` is the perf escape hatch for hot streaming paths.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (Phase 10's `TestEngine_CrossTenant_NoBleed` still passes; new test asserts `RunError` carries identity)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** See AGENTS.md §5 + §11 + D-025. — `TestEngine_ConcurrentReuse_ReuseContract_WithPolicy`.
- [ ] **If this phase consumes a shipped subsystem's surface: an integration test exists.** See AGENTS.md §17. — `test/integration/runtime_engine_test.go` extended.
- [ ] If new vocabulary: glossary updated (NodePolicy, RunError, Reliability shell, ValidateMode)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 11; section says "None")
