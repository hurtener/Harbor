# Phase 52 — steering-inbox

## Summary

Phase 52 lands `internal/runtime/steering` — the per-run steering inbox owned by the Runtime, the nine-event control taxonomy (`INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`), the Protocol-edge payload validation / sanitisation (depth ≤ 6, ≤ 64 keys, ≤ 50 list items, ≤ 4096 chars/string, ≤ 16 KiB total), and the per-event scope-claim checks. It is the primitive only: wiring steering into the engine's run loop (drain-between-steps, CANCEL propagation, PAUSE blocking) is Phase 53, the §13 first consumer in the same wave.

## RFC anchor

- RFC §6.3

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- **brief 02 §2 (`ControlEvent` / `ControlEventType`):** the nine-type taxonomy is kept verbatim — `INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`. Phase 52 ships them as the `ControlType` enum + the `ControlEvent` record.
- **brief 02 §3 ("Protocol exposure for steering"):** "The runtime validates+sanitises the event (depth/keys/list/string caps), then deposits it on a per-run inbox … **The planner sees the result via `RunContext.Control` only; it does not receive the inbox.**" Phase 52 ships the validate/sanitise pass + the per-run `Inbox` (enqueue + drain). The planner-facing `RunContext.Control` projection is Phase 53.
- **brief 02 §4 finding 2 ("Steering at planner level"):** the predecessor drained a steering inbox *inside* the planner loop — "Harbor moves the inbox into the runtime; planners observe only `RunContext.Control`." Phase 52 puts the inbox in the runtime (`internal/runtime/steering`), never in the planner package.
- **brief 02 §6 finding 8 ("Steering payload size limits"):** the predecessor's payload caps — 16384 total bytes, depth 6, 64 keys, 50 list items, 4096-char strings — "Harbor keeps the same caps." Phase 52 ships them as named constants (`MaxPayloadTotalBytes`, `MaxPayloadDepth`, `MaxPayloadKeys`, `MaxPayloadListItems`, `MaxPayloadStringLen`) enforced at the edge.
- **brief 02 §4 finding 8 + RFC §6.3 "Steering authn/authz" (resolving brief 02 Q-3):** per-event scopes — `CANCEL/APPROVE/REJECT/PAUSE/RESUME` need the originating user/admin scope; `INJECT_CONTEXT/USER_MESSAGE` accept the session-scoped user; `PRIORITIZE` needs admin; `REDIRECT` needs the user; cross-tenant steering needs admin. Phase 52 ships this as the `Scope` enum + `CheckScope`.

## Findings I'm departing from (if any)

None.

## Goals

- Ship the nine-type `ControlType` taxonomy + the `ControlEvent` record as the canonical steering data structures (RFC §6.3 — Settled).
- Ship the per-run `Inbox` owned by the Runtime — an enqueue + drain surface, identity-scoped to one run quadruple — plus a process-wide `Registry` that mints / looks up / retires per-run inboxes.
- Ship the Protocol-edge `ValidatePayload` pass enforcing the five RFC §6.3 payload bounds, failing loud (never truncating) on a violation.
- Ship the per-event `CheckScope` check enforcing the RFC §6.3 per-event scope mapping + the cross-tenant-requires-admin rule, failing closed on a mismatch.
- Ship the `control.rejected` canonical event + `EmitRejection` so a validation/scope failure is auditable on the event bus — the "per-event scope mismatch returns 403 + audit" acceptance, minus the 403 (Phase 54's Protocol-edge job).

## Non-goals

- **Engine run-loop wiring (Phase 53).** Drain-between-steps, CANCEL hard/soft propagation, PAUSE blocking at the next boundary, RESUME unblocking, INJECT_CONTEXT/REDIRECT/USER_MESSAGE projection onto `RunContext.Control`, APPROVE/REJECT advancing a pause, PRIORITIZE updating the task, control-history capping — all Phase 53.
- **The steering Protocol endpoints (Phase 54).** `task.cancel` / `task.pause` / `task.resume` / `task.inject_context` / `task.redirect` / `task.approve` / `task.reject` / `task.prioritize` / `task.user_message` — the network surface that derives the caller `Scope` from a JWT and maps `ErrScopeMismatch` to a 403 — is Phase 54.
- **Pause coordination.** `PAUSE`/`RESUME`/`APPROVE`/`REJECT` are taxonomy + scope-check entries here; their side effects wire onto the unified pause/resume primitive (`internal/runtime/pauseresume`, Phase 50) in Phase 53. Phase 52 does not reinvent pause coordination (CLAUDE.md §7 rule 4).
- **Cryptographic scope verification.** The `Scope` claim is trust-based at Phase 52, mirroring `events.Filter.Admin`; cryptographic verification arrives with Protocol auth (Phase 61).

## Acceptance criteria

- [ ] The nine-type control taxonomy is present verbatim (`INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`); `IsValidControlType` / `ControlTypes` enforce exhaustiveness.
- [ ] Oversize / over-deep payloads are rejected at the edge — `ValidatePayload` returns a wrapped `ErrPayloadInvalid` (or `ErrUnsupportedPayloadValue`) for every one of: depth > 6, > 64 keys in a map, > 50 list items, a string > 4096 runes, > 16 KiB total. The payload is never silently truncated.
- [ ] Per-event scope mismatch fails closed: `CheckScope` returns `ErrScopeMismatch` when the caller scope is below the control type's RFC §6.3 minimum, and when a non-admin caller submits cross-tenant. `EmitRejection` publishes a `control.rejected` audit event for the rejection. (The 403 is Phase 54's Protocol-edge mapping.)
- [ ] The per-run `Inbox` enqueues only events that pass identity + taxonomy + scope + payload validation, in FIFO order; `Drain` atomically empties it; per-run state never leaks across runs (an event for run A is rejected on run B's inbox).
- [ ] The `Registry` mints (`Open`), looks up (`Lookup`), and retires (`Retire`) per-run inboxes; double-`Open` and lookup-after-`Retire` fail loud.
- [ ] Unit tests cover every validation bound and every per-event scope check; the integration test covers auth-scope-per-event against a real `events.EventBus`.
- [ ] The D-025 concurrent-reuse test runs N≥100 concurrent invocations against one shared `Registry` (and one shared `Inbox`) under `-race`.
- [ ] Coverage on `internal/runtime/steering` ≥ 85%.

## Files added or changed

```text
internal/runtime/steering/                  # NEW package (AGENTS.md §3 — already listed)
├── steering.go                             # package doc + Clock seam
├── taxonomy.go                             # ControlType enum (nine types) + registry
├── validate.go                             # payload bounds + ValidatePayload
├── scope.go                                # Scope enum + per-event mapping + CheckScope
├── inbox.go                                # ControlEvent record + per-run Inbox
├── registry.go                             # process-wide Registry (Open/Lookup/Retire)
├── events.go                               # control.rejected event + EmitRejection
├── errors.go                               # sentinel errors
├── taxonomy_test.go                        # unit — taxonomy exhaustiveness
├── validate_test.go                        # unit — every payload bound
├── scope_test.go                           # unit — every per-event scope check
├── inbox_test.go                           # unit — Inbox enqueue/drain/fail-closed
├── registry_test.go                        # unit — Registry lifecycle + per-run isolation
├── events_test.go                          # unit — control.rejected + EmitRejection
└── concurrent_test.go                      # D-025 concurrent-reuse (N≥100)
test/integration/phase52_steering_test.go   # NEW — auth-scope-per-event vs a real EventBus
scripts/smoke/phase-52.sh                   # NEW — phase smoke
docs/plans/phase-52-steering-inbox.md       # NEW — this plan
docs/plans/README.md                        # Phase 52 status row → Shipped
docs/decisions.md                           # NEW — D-070
docs/glossary.md                            # NEW terms (Steering inbox, ControlEvent, ...)
README.md                                   # status table — Phase 52 row → Shipped

```

## Public API surface

```go
package steering

// Taxonomy.
type ControlType string
const ( ControlInjectContext, ControlRedirect, ControlCancel, ControlPrioritize,
        ControlPause, ControlResume, ControlApprove, ControlReject, ControlUserMessage ControlType = ... )
func IsValidControlType(t ControlType) bool
func ControlTypes() []ControlType

// Payload validation (Protocol edge).
const ( MaxPayloadDepth, MaxPayloadKeys, MaxPayloadListItems, MaxPayloadStringLen, MaxPayloadTotalBytes = ... )
func ValidatePayload(p map[string]any) error

// Per-event scopes.
type Scope string
const ( ScopeSessionUser, ScopeOwnerUser, ScopeAdmin Scope = ... )
func IsValidScope(s Scope) bool
func RequiredScope(t ControlType) (Scope, bool)
func CheckScope(t ControlType, callerScope Scope, callerTenant string, run identity.Quadruple) error

// The per-run inbox + the process-wide registry.
type ControlEvent struct { Type ControlType; Identity identity.Quadruple; CallerScope Scope
                            CallerTenant string; Payload map[string]any; EventID string; EnqueuedAt time.Time }
type Inbox struct { /* opaque */ }
func (in *Inbox) Identity() identity.Quadruple
func (in *Inbox) Enqueue(ev ControlEvent) error
func (in *Inbox) Drain() ([]ControlEvent, error)
func (in *Inbox) Len() int

type Registry struct { /* opaque */ }
type Option func(*Registry)
func WithClock(c Clock) Option
func NewRegistry(opts ...Option) *Registry
func (r *Registry) Open(q identity.Quadruple) (*Inbox, error)
func (r *Registry) Lookup(q identity.Quadruple) (*Inbox, error)
func (r *Registry) Retire(q identity.Quadruple) error
func (r *Registry) Len() int

// Audit-on-rejection.
const EventTypeControlRejected events.EventType = "control.rejected"
type ControlRejectedPayload struct { events.SafeSealed; Type, Reason, CallerScope string }
func EmitRejection(ctx context.Context, bus events.EventBus, q identity.Quadruple,
                   t ControlType, callerScope Scope, rejectErr error) error

```

## §13 primitive-with-consumer obligation

Phase 52 ships a **primitive** — the control taxonomy + the per-run inbox + the validation/scope surface. Per CLAUDE.md §13 ("Shipping a primitive without its first consumer in the same wave"), the primitive's first consumer must land in the **same wave**.

**The §13 obligation is discharged by Phase 53** (Wave 9, Stage 3 — "Steering wiring, 9 control events"). Phase 53 wires the inbox into the engine's run loop: it `Lookup`s the per-run `Inbox`, `Drain`s it between planner steps, applies each `ControlEvent`'s side effect (CANCEL → raise; PAUSE → block at the next boundary via the unified `pauseresume` Coordinator; REDIRECT → rewrite the goal; INJECT_CONTEXT/USER_MESSAGE → append to the trajectory; APPROVE/REJECT → advance a pause; PRIORITIZE → update the task), and projects the result onto `RunContext.Control`. Phase 53's `Deps` are `52, 13` and it lands before Wave 9 closes.

Phase 52's own tests are **not** a substitute for the §13 consumer — they are the direct exercise of the primitive (taxonomy exhaustiveness, every payload bound, every per-event scope check, the inbox enqueue/drain lifecycle, the D-025 concurrent-reuse stress, the auth-scope-per-event integration test against a real `EventBus`). The §13 obligation is satisfied at the wave level by Phase 53.

## Test plan

- **Unit:** `taxonomy_test.go` — nine-type exhaustiveness + verbatim wire strings. `validate_test.go` — every payload bound at-cap (valid) and over-cap (rejected): depth, per-map keys, list items, per-string runes (rune-counted, not byte-counted), total bytes; unsupported leaf types (chan/func/complex); the fail-loud "never truncates" contract. `scope_test.go` — every control type at its minimum scope (accepted), below it (rejected), and admin (always accepted); the RFC §6.3 mapping pinned verbatim; cross-tenant-requires-admin; empty-caller-tenant fails closed. `inbox_test.go` — FIFO enqueue/drain, clock-stamped `EnqueuedAt`, rejection of pre-filled `EnqueuedAt`, foreign-run-identity rejection (per-run isolation), incomplete-identity rejection, unknown-type / scope-mismatch / oversize-payload rejection, nil-payload acceptance, retired-inbox fails closed. `registry_test.go` — Open/Lookup/Retire lifecycle, duplicate-Open rejection, incomplete-identity rejection, double-Retire rejection, per-run isolation (two runs' inboxes never share state), same-triple-different-run distinct inboxes. `events_test.go` — `control.rejected` registered + `SafePayload`, `classifyRejection` mapping, `EmitRejection` nil-arg / publish-failure / happy-path payload shape.
- **Integration:** `test/integration/phase52_steering_test.go` — wires the steering inbox against the **real** `events.EventBus` (in-mem production driver) + the **real** patterns audit redactor. `TestE2E_Phase52_AuthScopePerEvent` walks every one of the nine control types at min scope (accepted) and below-min (rejected → `control.rejected` audit event on the bus with `Reason == "scope_mismatch"`). `TestE2E_Phase52_CrossTenantRequiresAdmin` — foreign-tenant non-admin rejected, foreign-tenant admin accepted. `TestE2E_Phase52_PayloadBoundsFailureMode` — oversize payload rejected + audited with `Reason == "payload_invalid"` (the §17.3 #3 failure mode). `TestE2E_Phase52_PerRunIsolation` — an identity-scoped subscriber sees only its own run's rejections (cross-run isolation). `TestE2E_Phase52_ConcurrencyStress` — N=24 concurrent submitters against one shared `Registry` + bus, no cross-talk, goroutine-count returns toward baseline. All under `-race`.
- **Conformance:** N/A — Phase 52 ships a single in-process primitive, not a multi-driver subsystem (no §4.4 seam — D-070).
- **Concurrency / leak:** `concurrent_test.go` — `TestConcurrentReuse_Registry` runs N=200 goroutines through the full `Open → Enqueue → Lookup → Drain → Retire` lifecycle against one shared `Registry` under `-race`, asserting no data races, no context bleed (distinct per-goroutine run quadruples; a drained event carrying a foreign `RunID` fails the test), and no goroutine leak (baseline-restored). `TestConcurrentReuse_SingleInbox` stresses one shared `Inbox` with N=120 concurrent producers + a concurrent draining consumer, asserting every event is drained exactly once (no loss / duplication).

## Smoke script additions

- `scripts/smoke/phase-52.sh`: runs `go test -race ./internal/runtime/steering/...` (unit + D-025) and `go test -race -run TestE2E_Phase52 ./test/integration/...`; static-greps `taxonomy.go` for all nine RFC §6.3 control-type wire strings; static-greps `validate.go` for all five payload-bound constants; guards that the package does not import the Console; guards that no parallel pause `Coordinator` is declared (pause-family controls converge on `internal/runtime/pauseresume`); guards that no `drivers/` tree exists (the inbox is an in-process primitive); ends with a `skip` for the not-yet-built Protocol surface (404/405/501 → SKIP convention).

## Coverage target

- `internal/runtime/steering`: 85% (achieved: 96.6%).

## Dependencies

- 50 (the unified pause/resume primitive — `PAUSE`/`RESUME`/`APPROVE`/`REJECT` are taxonomy entries here; Phase 53 wires their side effects onto the Phase 50 `Coordinator`).
- 05 (the typed event bus — `control.rejected` is a canonical event; the auth-scope-per-event integration test runs against a real `events.EventBus`).

## Risks / open questions

- **`MaxPayloadKeys` is per-map, not cumulative.** The RFC says "≤ 64 keys"; the predecessor's key cap was a per-map cap. Phase 52 reads it per-map (a payload with two 64-key maps is valid; one 65-key map is not). The total-bytes cap (16 KiB) bounds the cumulative size regardless. Settled in D-070.
- **The `Scope` claim is trust-based at Phase 52.** Cryptographic verification arrives with Protocol auth (Phase 61), exactly as `events.Filter.Admin` is trust-based until then. The `control.rejected` audit emit on every rejected submission makes abuse retroactively detectable in the interim.
- **The integration test exercises `EmitRejection` directly** rather than through a Protocol endpoint (Phase 54 hasn't landed). This is the correct seam for Phase 52: the test proves the audit-on-rejection path is alive against a real bus; Phase 54's smoke will exercise the HTTP 403 mapping.

## Glossary additions

- **Steering inbox** — the per-run, Runtime-owned queue of validated `ControlEvent`s. Added to `docs/glossary.md`.
- **`ControlEvent` / `ControlType`** — the canonical steering record + the nine-type taxonomy enum. Added to `docs/glossary.md`.
- **Steering `Scope`** — the three-tier caller privilege claim (`session_user` / `owner_user` / `admin`) the per-event scope check gates against. Added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (96.6% ≥ 85%)
- [x] If multi-isolation paths changed: cross-session isolation test passes (`TestRegistry_PerRunIsolation`, `TestE2E_Phase52_PerRunIsolation`)
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** `concurrent_test.go` runs N=200 against one shared `Registry` and N=120 against one shared `Inbox`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists, wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** `test/integration/phase52_steering_test.go` wires the real `events.EventBus` + patterns redactor; covers the payload-bounds + scope-mismatch failure modes.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departures.
