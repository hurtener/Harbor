# Phase 07 — StateStore interface + InMem driver + conformance suite

## Summary

Land `internal/state`: the single mandatory `StateStore` interface that every persistence-shaped Harbor subsystem (sessions, tasks, governance accumulators, planner checkpoints, memory snapshots) saves through; the InMem driver behind the §4.4 driver-registry seam; and the cross-package `conformancetest.Run` suite that pins the contract for the SQLite + Postgres drivers landing in phases 15 and 16. This is the persistence floor for Wave 3+ subsystems — Phase 08 (sessions) is the first consumer.

## RFC anchor

- RFC §6.11
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- **brief 05 §1 + §2 + §5 (StateStore floor; mandatory full surface).** "All three [drivers] pass the same conformance suite. Designing the interface against three backends from t=0 forces clean abstractions." Harbor adopts this from the floor: ONE mandatory interface, no `Supports*` ceremony, no per-driver capability detection. The InMem driver is the test reference; SQLite (Phase 15) and Postgres (Phase 16) inherit the conformance suite verbatim. (D-004.)
- **brief 05 §4 (idempotency on `EventID`).** `Save` keys on a caller-provided ULID and is a no-op on duplicate-with-same-content; mismatched-content under the same `EventID` fails loudly so retry-shaped writers cannot silently overwrite. The store does not generate IDs; callers do, so retry semantics are caller-controlled.
- **brief 05 §4 + §5 (Audit redaction is upstream).** "Every `StateEvent.Payload` passes through a redactor before persistence. Tool arguments and results are summarized or hashed; full payloads are never stored." Phase 07 honors this contract by storing opaque `Bytes` — the StateStore does NOT interpret payloads or run redaction itself. Callers (typically the event-bus emitter, Phase 05) run `audit.Redactor.Redact` BEFORE handing bytes to `StateStore.Save`. Mixing redaction into the store would couple a leaf package to the audit subsystem and split responsibility.
- **brief 05 §5 ("Sharp edges").** The reference implementation's `StateStoreSessionAdapter` — writing session updates as audit events keyed by string trickery — is unnecessary in Harbor: sessions are first-class consumers of the generic `(Quadruple, Kind)` surface and address themselves with a stable `Kind = "session.lifecycle"`. No string trick.
- **brief 06 §1 (event payload is the canonical record; redact upstream).** The StateStore is the durable backing of the typed event bus's audit projection. Bytes arrive pre-redacted; the store does not re-redact.

## Findings I'm departing from (if any)

- None. An earlier draft of this plan flagged a divergence from RFC §6.11's typed multi-method surface (21 methods over `Task`, `Trajectory`, `RemoteAgentBinding`, etc.). That divergence is **resolved by D-027** (settled in the same PR as this plan): RFC §6.11 is revised to the generic `(Quadruple, Kind, Bytes)` surface this plan ships, and consuming phases (08 sessions, 20 tasks, 22 distributed, 23 memory, 42 planner, 50 steering) land their typed wrappers atop it. The conformance suite's invariants (idempotency on `EventID`, identity-mandatory, cross-tenant / cross-session isolation, concurrent-safe, leak-free) hold unchanged.

## Goals

- Single mandatory `StateStore` interface in `internal/state` — five methods, no `Supports*` ceremony.
- InMem driver in `internal/state/drivers/inmem/` — registers itself via `init()` and is blank-imported by `cmd/harbor/main.go`.
- Driver-registry seam (`Register` / `Open` / `OpenDriver` / `RegisteredDrivers`) modeled on `internal/audit/registry.go`. `Open(ctx, cfg config.StateConfig)` reads the existing `Driver` enum (`inmem|sqlite|postgres`) wired in Phase 02; this phase only ships the `inmem` factory.
- Cross-package `conformancetest.Run(t, factory)` suite at `internal/state/conformancetest/conformancetest.go` — same shape as Phase 01's `internal/identity/conformancetest`. The InMem driver and every later driver (Phases 15, 16, future durable drivers) MUST pass this suite verbatim. The suite IS the gate.
- Identity-mandatory at the API boundary: the store rejects `Save` / `Load` / `Delete` for any `Quadruple` whose tenant / user / session is empty, raising `ErrIdentityRequired`. (Empty `RunID` is acceptable for state that is session-scoped rather than run-scoped.)
- Concurrent-reuse contract enforced by the suite (D-025): N≥100 goroutines saving and loading independent records against a single shared driver instance under `-race`, no data races, no record cross-talk, baseline goroutine count restored.
- Documented `EventID` (ULID) idempotency: a second `Save` with the same `EventID` and byte-equal `Bytes` is a no-op (no error, no duplicate); a second `Save` with the same `EventID` and different `Bytes` returns `ErrIdempotencyConflict`. Caller-controlled retry policy is the design intent.

## Non-goals

- No SQLite or Postgres driver — those land in Phase 15 / 16 and inherit this phase's conformance suite.
- No typed wrappers for sessions, tasks, planner checkpoints, memory state, trajectories, or steering events — each is owned by its consuming phase (08, 20, 21, 23, 42, 50). This phase ships the leaf interface only.
- No event-bus integration. The bus (Phase 05) consumes `StateStore` independently; that wiring belongs to whichever phase first emits a persisted event projection.
- No migrations machinery. Migrations are a per-driver concern (RFC §9: "forward-only, per-driver migration directories"); the InMem driver has no migrations.
- No audit redaction inside the store. `Bytes` is opaque from the store's view; callers redact upstream (brief 05 §4 + §"Audit redaction").
- No GC sweep / TTL semantics. Sessions own their own TTL (Phase 08); the store does not auto-expire records.
- No cross-quadruple query API (`ListByTenant`, `ListBySession`). The minimal `Load` / `LoadByEventID` surface is sufficient for Phase 08; richer queries land with their first consumer.

## Acceptance criteria

- [ ] `internal/state/state.go` defines `EventID`, `StateRecord`, `StateStore` interface, sentinel errors (`ErrNotFound`, `ErrIdempotencyConflict`, `ErrIdentityRequired`, `ErrUnknownDriver`), and the public function set under "Public API surface".
- [ ] `Save(ctx, r)` is idempotent on `EventID` + byte-equal `Bytes`. A second `Save` with the same `EventID` and identical `Bytes` returns nil (no-op). A second `Save` with the same `EventID` and any divergent field (`Bytes`, `Kind`, `Identity`, `Version`) returns `ErrIdempotencyConflict` wrapped with a descriptive message.
- [ ] `Save` / `Load` / `Delete` reject any `Quadruple` whose `TenantID`, `UserID`, or `SessionID` is empty. The wrapped error returns `errors.Is(err, ErrIdentityRequired) == true`. Empty `RunID` is acceptable (session-scoped state).
- [ ] `Load(ctx, id, kind)` returns `ErrNotFound` (wrapped) when no record exists for `(id, kind)`. `LoadByEventID(ctx, eventID)` returns `ErrNotFound` (wrapped) on missing IDs.
- [ ] `Delete(ctx, id, kind)` is idempotent: deleting a nonexistent record returns nil (not `ErrNotFound`).
- [ ] `Close(ctx)` is idempotent and joins any driver-internal goroutines. The InMem driver has none; the contract still requires `Close` to be safe to call.
- [ ] `internal/state/registry.go` provides `Register(name string, factory Factory)`, `Open(ctx, cfg config.StateConfig) (StateStore, error)`, `OpenDriver(name string, cfg config.StateConfig) (StateStore, error)`, `RegisteredDrivers() []string` — modeled verbatim on `internal/audit/registry.go`. `Open` switches on `cfg.Driver`; unknown driver names return `ErrUnknownDriver` wrapped with the registered list.
- [ ] `internal/state/drivers/inmem/inmem.go` registers under name `inmem` via `init()`. Backing storage: a `map[indexKey]StateRecord` keyed by `(quadruple, kind)` plus a secondary index `map[EventID]indexKey` for `LoadByEventID`. Synchronization: a single `sync.RWMutex` guards both maps.
- [ ] `cmd/harbor/main.go` blank-imports `_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"` (additive — does not disturb the existing audit driver import).
- [ ] `internal/state/conformancetest/conformancetest.go` exports `Run(t *testing.T, factory func() (state.StateStore, func()))`. The factory's second return is a teardown closure invoked by the suite after each subtest. Subtests:
  - `Save_Load_RoundTrip` — basic happy path, byte-equal recovery.
  - `Save_Idempotency_SameEventIDSameContent` — second Save is a no-op (returns nil; record not duplicated; visible state unchanged).
  - `Save_Idempotency_SameEventIDDifferentContent` — returns `ErrIdempotencyConflict`.
  - `Load_NotFound` — `errors.Is(err, ErrNotFound)`.
  - `LoadByEventID_RoundTrip` — round-trips through the secondary index.
  - `LoadByEventID_NotFound` — `errors.Is(err, ErrNotFound)`.
  - `Save_Identity_Mandatory` — empty tenant / user / session each return `errors.Is(err, ErrIdentityRequired)`.
  - `Load_Identity_Mandatory` — same.
  - `Delete_Identity_Mandatory` — same.
  - `Save_CrossTenant_Isolation` — record saved under tenant A is not loadable under tenant B for any `(Kind, RunID)`.
  - `Save_CrossSession_Isolation` — record saved under `(A, U, S1, _)` is not loadable from `(A, U, S2, _)`.
  - `Delete_CrossSession_Isolation` — deletion under one session does not affect another.
  - `Concurrent_SaveLoad_NoRace` — N≥100 goroutines saving and loading independent records on a shared driver under `-race`; each goroutine recovers exactly its own bytes; no record cross-talk.
  - `Close_Idempotent` — `Close` called twice returns nil both times.
  - `GoroutineLeak_AfterClose` — `runtime.NumGoroutine` returns to baseline after `Close` (asserted with a small wait window).
- [ ] `internal/state/conformancetest/conformancetest_test.go` self-applies `Run` against the InMem driver factory.
- [ ] `internal/state/state_test.go` covers the registry surface (`Register` / `Open` / unknown-driver) and the sentinel-error behavior at the boundary; the deep contract is owned by `conformancetest.Run`.
- [ ] Test coverage on `internal/state` ≥ 85%. The InMem driver and registry reach 100% in practice; the conformance subpackage will land at lower coverage by design (its `t.Errorf` / `t.Fatalf` paths only fire when a downstream driver fails the suite — same precedent as Phase 01's `conformancetest`).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-07.sh` present and executable; reports SKIP under preflight (Phase 07 has no HTTP surface).
- [ ] `docs/glossary.md` gains entries for `StateStore`, `StateRecord`, and `EventID` (ULID).
- [ ] `docs/decisions.md` entry **D-027** is in place (settled in the same PR as this plan) and RFC §6.11 reflects the generic surface.

## Files added or changed

- `internal/state/state.go` (new) — `EventID`, `StateRecord`, `StateStore` interface, sentinel errors, ctx-key plumbing if needed (none expected for V1; the store is reached via dependency injection, not ctx).
- `internal/state/registry.go` (new) — `Register`, `Open`, `OpenDriver`, `RegisteredDrivers`. Modeled on `internal/audit/registry.go`.
- `internal/state/state_test.go` (new) — registry tests + sentinel error wiring tests.
- `internal/state/conformancetest/conformancetest.go` (new) — exported `Run(t, factory)`. Subpackage chosen so `internal/state` does not import `testing` (precedent: Phase 01).
- `internal/state/conformancetest/conformancetest_test.go` (new) — self-applied smoke against InMem driver.
- `internal/state/drivers/inmem/inmem.go` (new) — InMem driver. `init()` registers under `"inmem"`.
- `internal/state/drivers/inmem/inmem_test.go` (new) — driver-level tests + the conformance suite invocation.
- `cmd/harbor/main.go` (modified) — additive blank import for the InMem driver.
- `scripts/smoke/phase-07.sh` (new) — SKIP-only smoke skeleton (precedent: phase-01.sh).
- `docs/plans/phase-07-state.md` (this file).
- `docs/glossary.md` (modified) — adds `EventID`, `StateRecord`, `StateStore` entries.
- `docs/decisions.md` (modified) — adds D-027.

No top-level directory additions; `internal/state/` is already enumerated in AGENTS.md §3.

## Public API surface

```go
package state

import (
    "context"
    "errors"
    "time"

    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/identity"
)

// EventID is a ULID-shaped, lexicographically sortable identifier
// supplied by the caller. It is the canonical idempotency key for
// Save: identical (EventID, Bytes) on repeat is a no-op; identical
// EventID with divergent content is ErrIdempotencyConflict.
//
// Generation lives in callers (typically via oklog/ulid). The store
// stores the value verbatim; it does not interpret the bytes beyond
// equality checks.
type EventID string

// StateRecord is the unit of persisted state. The store does not
// interpret Bytes — callers serialize their own domain types and pass
// pre-redacted bytes (see brief 05 §4 + §"Audit redaction": redaction
// runs upstream).
type StateRecord struct {
    ID        EventID            // ULID; idempotency key
    Identity  identity.Quadruple // owning (tenant, user, session, run)
    Kind      string             // caller-namespaced key, e.g. "session.lifecycle"
    Version   int                // monotonically increasing; caller-managed
    Bytes     []byte             // opaque payload; pre-redacted by caller
    UpdatedAt time.Time          // last mutation timestamp; caller-managed
}

// Sentinel errors. Callers compare via errors.Is.
var (
    // ErrNotFound — no record matches the (Identity, Kind) key, or the
    // EventID is unknown to LoadByEventID.
    ErrNotFound = errors.New("state: record not found")

    // ErrIdempotencyConflict — Save was called with an EventID that
    // matches an existing record but with divergent Bytes / Identity /
    // Kind / Version. The first writer wins; the conflicting writer
    // must reconcile.
    ErrIdempotencyConflict = errors.New("state: idempotency conflict")

    // ErrIdentityRequired — Save / Load / Delete received a Quadruple
    // whose tenant, user, or session was empty. Identity is mandatory
    // (D-001); the store fails closed at the boundary.
    ErrIdentityRequired = errors.New("state: identity required")

    // ErrUnknownDriver — Open / OpenDriver was given a driver name no
    // factory has registered. The wrapped message lists the registered
    // names.
    ErrUnknownDriver = errors.New("state: unknown driver")
)

// StateStore is the single mandatory persistence interface. All three
// V1 drivers (inmem, sqlite, postgres) implement every method; the
// conformance suite is the gate.
//
// Concurrent reuse contract (D-025): every method must be safe to call
// from N goroutines on a single shared instance. Per-call state lives
// in ctx and the StateRecord; nothing about the call writes durable
// state on the StateStore implementation itself.
type StateStore interface {
    // Save persists r. Idempotent on (r.ID, byte-equal r.Bytes); a
    // re-save with the same ID and identical Bytes returns nil.
    // Divergent Bytes / Identity / Kind / Version under the same ID
    // returns ErrIdempotencyConflict.
    Save(ctx context.Context, r StateRecord) error

    // Load retrieves the latest record for (id, kind). Returns
    // ErrNotFound (wrapped) when no such record exists.
    Load(ctx context.Context, id identity.Quadruple, kind string) (StateRecord, error)

    // LoadByEventID retrieves a record by its caller-supplied ULID,
    // independent of (Identity, Kind). Returns ErrNotFound (wrapped)
    // on miss.
    LoadByEventID(ctx context.Context, eventID EventID) (StateRecord, error)

    // Delete removes the record at (id, kind). Idempotent: deleting a
    // non-existent record returns nil (not ErrNotFound).
    Delete(ctx context.Context, id identity.Quadruple, kind string) error

    // Close releases driver resources. Idempotent. Joins any
    // driver-internal goroutines so callers can assert leak-free
    // shutdown.
    Close(ctx context.Context) error
}

// Factory builds a StateStore from the configured StateConfig. Drivers
// register one Factory each via init().
type Factory func(config.StateConfig) (StateStore, error)

// Register installs a driver factory under name. Drivers self-register
// from init(); cmd/harbor blank-imports the production driver to
// trigger registration. Per AGENTS.md §4.4. Re-registering the same
// name panics — the registration model is write-once-at-init.
func Register(name string, factory Factory)

// Open returns a StateStore built by the factory matching cfg.Driver.
// Unknown driver names return ErrUnknownDriver wrapped with the
// registered list.
func Open(ctx context.Context, cfg config.StateConfig) (StateStore, error)

// OpenDriver returns a StateStore from a specific driver name. Useful
// for tests that want to bypass cfg.Driver routing.
func OpenDriver(name string, cfg config.StateConfig) (StateStore, error)

// RegisteredDrivers returns a sorted list of driver names. Useful for
// boot-log emission ("state drivers available: inmem") and for surfacing
// in error messages.
func RegisteredDrivers() []string
```

`EventID`, `StateRecord`, `StateStore`, the sentinel errors, the registry functions, and the conformance suite (`conformancetest.Run`) are the entire public surface.

## Test plan

- **Unit:** registry surface — `Register` panics on duplicate name / empty name / nil factory; `Open` routes by `cfg.Driver`; unknown driver wraps `ErrUnknownDriver` with the registered list. Sentinel-error formatting / wrapping tests.
- **Integration:** N/A at this phase — the InMem driver is the only consumer; tests live in the driver package.
- **Conformance:** `conformancetest.Run` is the load-bearing test surface. Subtests enumerated under "Acceptance criteria". Self-applied to InMem in `conformancetest_test.go` and re-applied in the InMem driver's own test file. Phase 15 / 16 / future drivers consume the same suite.
- **Concurrency / leak (D-025 concurrent-reuse contract):** `Concurrent_SaveLoad_NoRace` is the canonical reusable-artifact test for `StateStore` — N≥100 goroutines saving and loading independent records on a shared driver instance under `-race`, asserting no data races, no record cross-talk, no goroutine leaks (baseline-restored after `Close`). Per AGENTS.md §5 + §11 + RFC §3.5 + D-025. The test is part of the conformance suite so every later driver inherits it.

## Smoke script additions

- `scripts/smoke/phase-07.sh` issues `skip "phase 07: state store — Go package only; validated by go test ./internal/state/..."` and calls `smoke_summary`. The SKIP counter increments cleanly under preflight; no FAIL. Phase 07 has no HTTP / Protocol surface.

## Coverage target

- `internal/state`: 85% (registry + sentinel surface; production code reaches ~100% in practice).
- `internal/state/drivers/inmem`: 90% (driver implements every interface method; the conformance suite drives every code path).
- `internal/state/conformancetest`: not gated; the helper's `t.Errorf` / `t.Fatalf` paths only fire when a downstream driver fails the suite, so a self-applied success run intentionally leaves them uncovered (precedent: Phase 01 `internal/identity/conformancetest` at 75%).

## Dependencies

- Phase 01 (identity) — `identity.Quadruple` is the storage key.
- Phase 03 (audit redactor) — declared by the master plan even though `internal/state` does NOT import `internal/audit`. The dependency is **logical, not structural**: callers MUST run `audit.Redactor.Redact` upstream of `StateStore.Save` per brief 05 §4. Phase 07 does not enforce this in code (it cannot — `Bytes` is opaque); it documents the contract on the godoc for `StateRecord.Bytes` and on the package overview.
- Phase 02 (config) — `config.StateConfig` is the open-time argument.

## Risks / open questions

- **RFC §6.11 generic surface (settled by D-027).** The earlier draft of §6.11 sketched a 21-method typed interface keyed on Go types belonging to unshipped phases. D-027 supersedes that sketch with the generic `(Quadruple, Kind, Bytes)` surface this phase ships; consuming phases land typed wrappers atop. Both the decisions log and §6.11 are updated in this same PR.
- **ULID library.** `oklog/ulid` is already pinned in RFC §10's stack-decisions table — pure Go, CGo-free, the canonical reference. Phase 07 introduces the dependency in `go.mod`. No risk; flagged here for visibility.
- **`indexKey` shape inside the InMem driver.** Composite-string vs struct: the implementation uses a struct value `{Tenant, User, Session, Run, Kind string}` as the map key (Go allows struct keys when all fields are comparable). This avoids string-concatenation collisions on tenant-IDs that contain delimiters. The RWMutex covers both the primary map and the EventID secondary index.
- **`Bytes` alias semantics.** Callers may mutate the slice they passed in after `Save` returns. The InMem driver defends against this by deep-copying `Bytes` on `Save` and returning a deep copy on `Load`. Documented on the godoc; future SQL drivers naturally avoid the issue (rows are independent of the caller's slice).
- **`UpdatedAt` clock.** Caller-managed: each subsystem chooses its own clock source (real, fake, monotonic). The store does not stamp time. This avoids hidden non-determinism in tests and matches the Phase 02 config-loader pattern (callers own their clocks).
- **No open RFC §11 questions block this phase.** Q-1..Q-6 are unrelated.

## Glossary additions

- **`EventID`** — a ULID-shaped (`oklog/ulid`) caller-supplied identifier that doubles as the canonical idempotency key for `StateStore.Save`. Lexicographically sortable, monotonic-per-millisecond, 26 chars in Crockford base32. RFC §6.11, §10.
- **`StateRecord`** — the unit of persisted state. Carries `(EventID, Quadruple, Kind, Version, Bytes, UpdatedAt)`. `Bytes` is opaque to the store; callers serialize their own types and run audit redaction upstream. RFC §6.11.
- **`StateStore`** — Harbor's single mandatory persistence interface. Five methods: `Save`, `Load`, `LoadByEventID`, `Delete`, `Close`. Three V1 drivers (inmem, sqlite, postgres) all pass the same `conformancetest.Run` suite. RFC §6.11, §9, D-004, D-025.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated targets (85% for `internal/state`, 90% for `internal/state/drivers/inmem`)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`Save_CrossTenant_Isolation` + `Save_CrossSession_Isolation` in the conformance suite cover this)
- [ ] **Concurrent-reuse test passes** — `Concurrent_SaveLoad_NoRace` in the conformance suite, N≥100 goroutines under `-race`, no data races, no cross-talk, no leaks (D-025).
- [ ] If new vocabulary: glossary updated (yes — `EventID`, `StateRecord`, `StateStore`)
- [ ] If a brief finding was departed from: N/A. (D-027 settles the RFC §6.11 surface in the same PR; "Findings I'm departing from" section reflects this.)
