# Phase 01 — Identity foundation

## Summary
Land `internal/identity`: the canonical Go package that defines the `(tenant, user, session)` triple, its `Quadruple` extension carrying `RunID`, mandatory non-empty validation, ctx propagation helpers, sentinel errors, and a reusable conformance suite. This is the floor every subsequent phase consumes for identity-scoped logic; nothing in Harbor compiles without it.

## RFC anchor
- RFC §4
- RFC §6.1

## Briefs informing this phase
- brief 01
- brief 02
- brief 05

## Brief findings incorporated
- **brief 02 (planner / memory):** identity is mandatory at the runtime boundary; the predecessor's `require_explicit_key=False`-style opt-out knob is rejected. Harbor fails closed: any missing component (`TenantID`, `UserID`, or `SessionID`) returns `ErrIdentityIncomplete`. Decision **D-001** + AGENTS.md §6 codify the rule.
- **brief 05 (state / sessions):** the triple is the load-bearing **isolation** key; `RunID` is the per-execution scope **inside** a session and is NOT a fourth isolation dimension. The `Identity` type stays a triple; `Quadruple` is a separate type used by Envelopes and run-scoped state, never substituted for `Identity` in scoping decisions.
- **brief 01 (envelopes):** Envelopes carry the quadruple. `RunID` is Harbor's term for what the predecessor called `trace_id`; `TraceID` (OpenTelemetry) is independent and may span multiple runs. The `identity` package owns the quadruple type so the runtime/messaging layer doesn't redefine it.

## Findings I'm departing from (if any)
- None.

## Goals
- Produce a single, dependency-free Go package (`internal/identity`) that every other subsystem imports for identity types and ctx helpers.
- Make identity-mandatory + fail-closed mechanically obvious: there is no API surface that lets a caller bypass validation.
- Publish a reusable `ConformanceTest` so future identity-aware subsystems (StateStore drivers, MemoryStore drivers, Governance, Audit) all run the same correctness suite.

## Non-goals
- No JWT parsing or auth middleware (a later phase wires the Protocol layer's auth path; Phase 01 is the in-process types/ctx surface only).
- No event-bus emission of identity context (Phase 05/06 territory).
- No persistence; this package has no I/O.
- No `Quadruple` ↔ `Envelope` integration code (Phase 09 owns Envelope construction; it imports this package for types).

## Acceptance criteria
- [ ] `internal/identity/identity.go` defines `Identity`, `Quadruple`, sentinel errors, and the public functions enumerated under "Public API surface" below.
- [ ] `Validate(id Identity)` returns `ErrIdentityIncomplete` (wrapped via `fmt.Errorf("...: %w", ErrIdentityIncomplete)`) when ANY of `TenantID`, `UserID`, `SessionID` is empty. There is no opt-out knob (D-001).
- [ ] `MustFrom(ctx)` panics with `ErrIdentityMissing` when ctx carries no `Identity`. `From(ctx)` returns `(Identity{}, false)` in the same case.
- [ ] `With(ctx, id)` and `WithRun(ctx, id, runID)` validate inputs at write time; calling either with an invalid `Identity` returns / panics consistent with the validate-on-read policy: validation occurs on `With`-style calls (write-time fail-loud).
- [ ] `MustQuadrupleFrom(ctx)` panics with `ErrIdentityMissing` when no `Quadruple` is present (independent key from `Identity` in ctx).
- [ ] No package-level mutable state. `go vet`-equivalent guard via a sentinel constant + a comment block at the top of `identity.go`.
- [ ] Test coverage on `internal/identity` ≥ 90%.
- [ ] Race detector test (`-race`) running ≥ 1000 concurrent ctx-derived calls confirms no shared state mutation; goroutine count returns to baseline.
- [ ] `ConformanceTest(t *testing.T, factory func() context.Context)` exported from `internal/identity/conformance.go`; covers fail-closed-on-empty, ctx round-trip, quadruple round-trip, identity-vs-quadruple non-aliasing, and concurrent-derived-ctx isolation.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-01.sh` smoke script present and executable; reports SKIP under preflight (Phase 01 has no HTTP surface).

## Files added or changed
- `internal/identity/identity.go` (new) — type + function definitions.
- `internal/identity/identity_test.go` (new) — unit + table-driven `Validate` tests + race-detector concurrency test.
- `internal/identity/conformance.go` (new) — exported `ConformanceTest` for downstream subsystems.
- `scripts/smoke/phase-01.sh` (new) — smoke skeleton (SKIPs at preflight; flagged for upgrade if a future surface lands).
- `docs/plans/phase-01-identity.md` (this file).

No top-level directory additions; `internal/identity/` is already enumerated in AGENTS.md §3.

## Public API surface
```go
package identity

import (
    "context"
    "errors"
)

// Identity is the load-bearing isolation key. All three components are mandatory.
type Identity struct {
    TenantID  string
    UserID    string
    SessionID string
}

// Quadruple is Identity + the per-execution RunID. Used in Envelopes and
// run-scoped state. Quadruple is NOT a substitute for Identity in scoping
// decisions: the triple is the isolation boundary; RunID is the per-execution
// scope inside a session.
type Quadruple struct {
    Identity
    RunID string
}

var (
    // ErrIdentityMissing — the context carries no Identity (or no Quadruple).
    ErrIdentityMissing = errors.New("identity: no Identity in context")
    // ErrIdentityIncomplete — one or more components empty. Identity is mandatory.
    ErrIdentityIncomplete = errors.New("identity: one or more components empty")
)

// With attaches Identity to ctx. Validates at write-time; returns the original
// ctx and ErrIdentityIncomplete if validation fails.
func With(ctx context.Context, id Identity) (context.Context, error)

// WithRun is the Quadruple-flavored With. Same write-time validation.
func WithRun(ctx context.Context, id Identity, runID string) (context.Context, error)

// MustFrom returns the Identity in ctx; panics with ErrIdentityMissing if
// none is present. Use in handler/runtime paths where identity is mandatory.
func MustFrom(ctx context.Context) Identity

// From returns the Identity in ctx and a presence bool. Use when absence
// is recoverable (e.g. cross-cutting middleware that may run pre-auth).
func From(ctx context.Context) (Identity, bool)

// MustQuadrupleFrom returns the Quadruple in ctx; panics with
// ErrIdentityMissing if none is present.
func MustQuadrupleFrom(ctx context.Context) Quadruple

// QuadrupleFrom returns the Quadruple in ctx and a presence bool.
func QuadrupleFrom(ctx context.Context) (Quadruple, bool)

// Validate returns ErrIdentityIncomplete when any of (TenantID, UserID,
// SessionID) is empty. There is no opt-out knob.
func Validate(id Identity) error

// ConformanceTest is the canonical correctness suite. Identity-aware
// subsystems (StateStore drivers, MemoryStore drivers, Governance, Audit)
// must pass it. The factory returns a fresh context.Background() per call;
// the suite injects identities and asserts isolation behavior.
func ConformanceTest(t *testing.T, factory func() context.Context)
```

`Identity`, `Quadruple`, the sentinel errors, and the function set above are the entire public surface. Internal types (the unexported `ctxKey`) stay unexported.

## Test plan
- **Unit:** every public function. Table-driven `Validate` covering empty-tenant / empty-user / empty-session / all-empty / fully-populated; with-and-then-from round trips; from-on-empty-ctx; quadruple ↔ identity non-aliasing (a `WithRun`-derived ctx should NOT satisfy `From` for `Identity` alone, and vice-versa, unless explicitly composed).
- **Integration:** N/A at this phase (no other Harbor packages exist yet).
- **Conformance:** `ConformanceTest` covers fail-closed-on-empty, ctx round-trip, quadruple round-trip, identity-vs-quadruple non-aliasing, concurrent-derived-ctx isolation. Self-applied here; consumed by future phases.
- **Concurrency / leak:** `TestIdentity_RaceFreeConcurrentDerivedCtx` runs 1000+ goroutines deriving ctxs from different identities, asserts each goroutine reads back its own identity, and asserts `runtime.NumGoroutine` returns to baseline after the wait. `go test -race` is the gate.

## Smoke script additions
- `scripts/smoke/phase-01.sh` issues `skip "phase 01: identity package validated by go test (no HTTP surface)"` and calls `smoke_summary`. The SKIP counter increments cleanly under preflight; no FAIL.

## Coverage target
- `internal/identity`: 90%.

## Dependencies
- Phase 00 (skeleton). No upstream Harbor deps.

## Risks / open questions
- **Validate-at-write vs validate-at-read.** Decision: validate at write (`With` returns an error on invalid input; `MustFrom` only panics on absence). Rationale: write-time validation surfaces the bug at the call site; read-time validation surfaces it inside the consumer. Cross-references RFC §4 + AGENTS.md §6 (rule 9, identity-mandatory).
- **Two ctxKey constants vs one.** Decision: two unexported keys (`identityKey`, `quadrupleKey`) so a `WithRun`-derived ctx doesn't accidentally satisfy a non-quadruple consumer. Future-proofs the case where some consumers need just the triple.
- **Empty-string vs space-only.** Decision: empty string only is rejected by `Validate`; whitespace is the caller's problem. Documented in the godoc on `Validate`.
- No open RFC questions (Q-1..Q-6) bear on this phase.

## Glossary additions
- None. `Identity triple`, `Identity quadruple`, and `Sentinel errors` already in `docs/glossary.md`.

## Pre-merge checklist
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (90%)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (this phase IS that path; ConformanceTest covers it)
- [ ] If new vocabulary: glossary updated (N/A — none introduced)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none)
