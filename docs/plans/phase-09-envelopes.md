# Phase 09 — Envelopes, Headers, Identity quadruple

## Summary

Land `internal/runtime/messages`: the `Envelope` type Harbor passes along every channel and the `Headers` type that carries routing + identity. `Envelope` carries the full identity quadruple `(TenantID, UserID, SessionID, RunID)` plus `Timestamp`, `DeadlineAt`, free-form `Meta`. `RunID` is the runtime concurrency boundary — Harbor's term for what predecessors call `trace_id`; `TraceID` is reserved for OpenTelemetry-style traces (which may span multiple runs). This phase ships only the wire-shape types and the helpers (`WithRunID`, JSON round-trip, `Meta` merge) that downstream phases depend on; the engine itself lands in Phase 10.

## RFC anchor

- RFC §6.1
- RFC §4

## Briefs informing this phase

- brief 01

## Brief findings incorporated

- **brief 01 §2 — identity is a quadruple, not a single id.** `(TenantID, UserID, SessionID, RunID)` is the runtime identity. `RunID` is the active concurrency boundary; `SessionID` groups runs into a multi-turn conversation. The predecessor uses `tenant + trace_id` only; Harbor extends to the full quadruple from t=0. Phase 09 ships the wire shape that pins this — every later phase reads identity from the Envelope.
- **brief 01 §2 — `DeadlineAt` is wall-clock, not duration.** Set once at the boundary; checked before scheduling each node. Avoids the predecessor's "duration that drifts as it propagates" footgun. Phase 09 makes this typed: `*time.Time` (nil = no deadline).
- **brief 01 §2 — `Meta` is free-form.** Survives fan-out, fan-in, and subflow boundaries. Last-write-wins on key collisions in V1; an explicit merge-function registry is deferred to a future RFC follow-up. Phase 09 documents the rule and ships the simplest implementation that respects it.
- **brief 01 §1 / RFC §6.1 — `RunID` semantics.** RFC §6.1 settles brief 01 Q-1: keep `RunID` as the canonical name; reserve `TraceID` for OTel-style traces (often spanning multiple runs). Phase 09's tests assert that `RunID` round-trips through JSON and that an empty `RunID` is rejected at the API boundary (per AGENTS.md §6 identity-mandatory).
- **brief 01 §5 — sharp edges to design out.** The predecessor's silent-coercion pattern (`warnings.warn` for type-mismatch instead of hard error) is rejected at the message layer too: any malformed `Envelope` failing to unmarshal returns a typed error rather than a partial value. Phase 09's JSON round-trip test pins this with a malformed-input case.
- **D-001 — identity is the triple at the isolation boundary; the quadruple is identity-plus-RunID for run-scoped data.** Phase 09's `Envelope.Identity()` helper returns the `identity.Quadruple` directly so downstream code reads from one canonical type rather than the four scattered string fields.

## Findings I'm departing from (if any)

- **None.** Phase 09 is small, well-specified, and follows brief 01 verbatim. The quadruple decision is settled in RFC §6.1 + D-001; the wall-clock `DeadlineAt` is settled; `Meta` last-write-wins is the documented V1 rule. No departure.

## Goals

- Ship the `Envelope` and `Headers` types in `internal/runtime/messages` so every later runtime phase has the canonical wire shape to import.
- Provide a `WithRunID(runID string)` helper that returns a *copy* (not a mutation), aligning with D-025's "compiled artifacts immutable; per-run state lives in `ctx`".
- Pin the identity-quadruple round-trip through JSON so no field silently drops on serialization (the predecessor's silent-context-loss class is forbidden).
- Document `Meta` last-write-wins in godoc + tests so collision behavior is unambiguous.
- Provide an `Envelope.Identity() identity.Quadruple` helper that lets downstream code treat the four identity fields as one type — discourages the scattered-string-field anti-pattern.

## Non-goals

- No `Engine`, `Node`, or `Channel` types (Phase 10).
- No reliability shell (Phase 11).
- No streaming primitive (Phase 12).
- No cancellation, no FetchByRun (Phase 13).
- No routers, no subflows (Phase 14).
- No bus emission of envelope events. The bus integration happens via Phase 10's worker hooks.
- No `Meta` merge-function registry — V1 is last-write-wins; merge functions are a future RFC follow-up. Phase 09 documents the rule but does NOT add a merge-function seam, per "Don't design for hypothetical future requirements" (CLAUDE.md "Doing tasks").
- No persistence of envelopes. Envelopes are wire-shaped, not stored — they flow through channels in Phase 10's engine.

## Acceptance criteria

- [ ] `internal/runtime/messages/envelope.go` defines `Envelope`, `Headers`, and `WithRunID(string) Envelope`. `Envelope.Identity() identity.Quadruple` returns the quadruple from the embedded fields.
- [ ] `Envelope` carries: `Payload any`, `Headers Headers`, `RunID string`, `SessionID string`, `Timestamp time.Time`, `DeadlineAt *time.Time`, `Meta map[string]any`. `Headers` carries: `TenantID string`, `UserID string`, `Topic string`, `Priority int`.
- [ ] `WithRunID(id)` returns a *copy* of the envelope with the new RunID, never a mutation. Test asserts the source envelope's RunID is unchanged.
- [ ] `Identity()` returns `identity.Quadruple{Identity: identity.Identity{TenantID, UserID, SessionID}, RunID}`. Empty SessionID or empty RunID is allowed at the type level (the runtime enforces identity-mandatory at the engine boundary in Phase 10); `Identity()` itself never panics.
- [ ] `Envelope` round-trips through `encoding/json`: `Marshal` → `Unmarshal` produces an envelope with all fields equal except `Meta` map iteration order (which is preserved by JSON's object semantics for the round-trip but compared via `reflect.DeepEqual`).
- [ ] `DeadlineAt` is `*time.Time` (nil-safe). When non-nil, JSON serializes to RFC3339Nano; when nil, JSON serializes to `null`.
- [ ] `Meta` last-write-wins on collisions. Phase 09 ships a `MergeMeta(dst, src map[string]any) map[string]any` helper with explicit godoc: "Last-write-wins on key collisions; the result is `dst` mutated in place AND returned. An explicit merge-function registry is reserved for a future RFC follow-up." Test asserts `MergeMeta(map[string]any{"a":1}, map[string]any{"a":2,"b":3})` equals `map[string]any{"a":2,"b":3}`.
- [ ] No package-level mutable state. Compiled artifacts (none here — the package is types-only) trivially satisfy D-025.
- [ ] Coverage on `internal/runtime/messages` ≥ 90% (small types-only package; coverage is cheap and high).
- [ ] **Concurrent-reuse test (D-025):** the package is types-only and ships no compiled artifacts, so D-025 trivially holds. Documented in the plan under "Risks / open questions" with a one-line "N/A — types-only package" justification (per AGENTS.md pre-merge checklist).
- [ ] **Identity-quadruple round-trip test:** `TestEnvelope_IdentityQuadruple_RoundTrip` constructs an envelope, serializes through JSON, and asserts `Identity()` returns the same quadruple before and after.
- [ ] **Empty-RunID at the type level:** `TestEnvelope_EmptyRunID_AllowedAtTypeLevel` constructs an envelope with empty RunID and asserts `Identity()` returns `Quadruple{RunID: ""}` without panicking. Documents that the engine layer (Phase 10) enforces non-empty RunID at the API boundary; the type itself does not.
- [ ] **Meta merge:** `TestMergeMeta_LastWriteWins` and `TestMergeMeta_NilSource_ReturnsDst` and `TestMergeMeta_NilDst_ReturnsCopy` cover the three merge cases.
- [ ] **JSON malformed input:** `TestEnvelope_Unmarshal_MalformedInputFailsLoud` rejects a malformed payload (e.g. `DeadlineAt` not a string) with a typed `*json.UnmarshalTypeError` rather than a partial value.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-09.sh` smoke script runs `go test -race ./internal/runtime/messages/...` (Go-package only; HTTP surface still SKIPs).

## Files added or changed

```text
internal/runtime/messages/envelope.go        # Envelope, Headers, WithRunID, Identity, MergeMeta
internal/runtime/messages/envelope_test.go   # round-trip + MergeMeta + Identity tests
scripts/smoke/phase-09.sh                    # Go-package SKIP + go test invocation
docs/plans/README.md                         # Status: 09 → Shipped (in the implementation PR, not this plan PR)
docs/glossary.md                             # adds Envelope, Headers, RunID, DeadlineAt
```

## Public API surface

```go
package messages

// Envelope is the canonical message shape on every Harbor channel.
// Carries the identity quadruple (Tenant, User, Session, Run) plus
// timing and free-form Meta. RunID is Harbor's runtime concurrency
// boundary; TraceID is reserved for OpenTelemetry traces and lives
// outside the Envelope (carried by ctx via internal/telemetry).
type Envelope struct {
    Payload    any            `json:"payload"`
    Headers    Headers        `json:"headers"`
    RunID      string         `json:"run_id"`
    SessionID  string         `json:"session_id"`
    Timestamp  time.Time      `json:"timestamp"`
    DeadlineAt *time.Time     `json:"deadline_at,omitempty"`
    Meta       map[string]any `json:"meta,omitempty"`
}

// Headers carries routing + identity. TenantID and UserID complete the
// triple Headers also names, alongside Topic for routing and Priority
// for ordering. The runtime layer reads Tenant/User from Headers; the
// session layer (Phase 08) and run layer (Phase 13) layer SessionID
// and RunID into Envelope directly so they're not buried in Headers.
type Headers struct {
    TenantID string `json:"tenant_id"`
    UserID   string `json:"user_id"`
    Topic    string `json:"topic,omitempty"`
    Priority int    `json:"priority,omitempty"`
}

// WithRunID returns a copy of e with RunID replaced. Never mutates the
// receiver — Envelopes are values that flow through channels, not
// shared state.
func (e Envelope) WithRunID(id string) Envelope

// Identity returns the identity.Quadruple derived from the Envelope's
// (Headers.TenantID, Headers.UserID, SessionID, RunID). Empty fields
// pass through unchanged; the engine layer enforces non-empty at the
// API boundary.
func (e Envelope) Identity() identity.Quadruple

// MergeMeta applies last-write-wins semantics: keys from src overwrite
// keys in dst. An explicit merge-function registry for non-LWW
// semantics is reserved for a future RFC follow-up. nil src returns
// dst unchanged; nil dst returns a copy of src.
func MergeMeta(dst, src map[string]any) map[string]any
```

## Test plan

- **Unit:** `TestEnvelope_WithRunID_ReturnsCopy`, `TestEnvelope_Identity_HappyPath`, `TestEnvelope_EmptyRunID_AllowedAtTypeLevel`, `TestMergeMeta_LastWriteWins`, `TestMergeMeta_NilSource_ReturnsDst`, `TestMergeMeta_NilDst_ReturnsCopy`, `TestEnvelope_DeadlineAt_NilJSON_OmitEmpty`, `TestEnvelope_Unmarshal_MalformedInputFailsLoud`.
- **Integration:** N/A — Phase 09 is types-only with no cross-subsystem seam yet (the engine + bus + state wiring lives in Phase 10+). Per AGENTS.md §17.1 the integration-test rule fires when `Deps` names another shipped subsystem and the phase consumes that subsystem's surface; `messages` does not consume any shipped runtime surface (it IS the surface). Documented under "Risks / open questions".
- **Conformance:** N/A — single concrete type with no driver pluralism.
- **Concurrency / leak:** N/A — types-only package, no goroutines, no compiled artifact (per D-025 the test is required only when the phase builds a reusable artifact; types-only packages are exempt).

## Smoke script additions

- `phase-09.sh`: Go-package-only phase. Runs `go test -race ./internal/runtime/messages/...`. Mirrors the phase-07 / phase-08 shape (Go-package SKIP plus a real `go test` invocation as the gate). Phase 10's smoke will subsume this once the engine surface lands.

## Coverage target

- `internal/runtime/messages`: 90%

## Dependencies

- 01 (identity)
- 08 (sessions — Envelope.SessionID is meaningful only because Phase 08 shipped the session lifecycle; the type carries the field but Phase 09 doesn't open or persist sessions)

## Risks / open questions

- **No integration test in Phase 09.** The package is types-only with no shipped consumer in this PR — Phase 10 (engine) will be the first consumer. AGENTS.md §17.1 fires "when the phase consumes a shipped subsystem's surface" — Phase 09 consumes Phase 01 + Phase 08 only at the type level (it imports `identity.Quadruple` but never calls a runtime method). Documented; the integration test lands with Phase 10 where the engine actually exercises envelope flow.
- **D-025 trivially N/A.** Types-only package; no compiled artifacts, no goroutines, no shared mutable state. The pre-merge checkbox is marked N/A with this reason.
- **`Meta` merge will need an upgrade post-V1.** V1 is last-write-wins. Once subsystems start emitting `Meta` keys that callers want to *combine* (e.g. cost accumulation across fan-out), an RFC will introduce a merge-function registry. Out of scope here; documented in godoc.

## Glossary additions

- **Envelope.** Harbor's canonical message shape: `Payload`, `Headers`, identity quadruple, `Timestamp`, `DeadlineAt`, `Meta`. Flows along every runtime channel. Defined in `internal/runtime/messages`.
- **Headers (envelope).** Routing + identity sub-record on `Envelope`: `TenantID`, `UserID`, `Topic`, `Priority`. Distinct from HTTP headers; the term is RFC-settled vocabulary.
- **`DeadlineAt`.** Wall-clock deadline on an `Envelope`. Set once at the API boundary; checked before scheduling each node (Phase 10). `nil` means "no deadline."
- **Meta (envelope).** Free-form `map[string]any` propagated with the envelope. Last-write-wins on key collisions in V1. Survives fan-out / fan-in / subflow boundaries.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (types-only, no per-session state)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — N/A: Phase 09 ships only types and pure functions (`MergeMeta`); there's no shared mutable state to reuse.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** See AGENTS.md §17. — N/A with reason: Phase 09 imports `identity.Quadruple` and the future `messages.Envelope` will be consumed by Phase 10, but no runtime method is called from this phase. The cross-subsystem integration test lands with Phase 10's engine.
- [ ] If new vocabulary: glossary updated (Envelope, Headers, DeadlineAt, Meta)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 09; section says "None")
