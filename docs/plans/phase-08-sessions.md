# Phase 08 — SessionRegistry + lifecycle + GC

## Summary

Land `internal/sessions`: Harbor's session lifecycle subsystem. Ships the `Session` struct, the `SessionRegistry` interface (Open / Get / Touch / Close / Inspect / GC), a single concrete implementation backed by Phase 07's `StateStore`, the `GCPolicy` knob and reaper goroutine, and the load-bearing isolation invariants — identity captured immutably on Open, reopen-after-close rejected, GC never reaps `RUNNING` tasks (via a hook the future TaskRegistry plugs into in Phase 20). This is the first subsystem that sits on top of the StateStore; it codifies the "typed wrapper over generic state" contract from D-027 and unblocks the runtime kernel chain (Phases 09 onward) which keys envelopes on `SessionID`.

## RFC anchor

- RFC §6.9
- RFC §6.11
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §4 — session-lifetime invariants.** "A session is open until explicitly closed or GC'd. Reopen-after-close is forbidden — clients open a new session. The identity triple is captured on open and **immutable** for the session's lifetime; reusing a session ID across tenants/users is rejected. `Touch` updates `LastSeen`; GC sweeps sessions whose `LastSeen` exceeded the policy TTL and have no RUNNING tasks." All four invariants are mechanical acceptance criteria below; the immutability and reuse-rejection are the load-bearing isolation guarantees this phase pins.
- **brief 05 §5 — sharp edges.** The predecessor's `StateStoreSessionAdapter` writes session updates as audit events keyed by `f"session:{session_id}"` — a string-trick for compatibility with a trace-keyed audit log. Harbor's StateStore is keyed natively by `(identity.Quadruple, Kind)`; sessions land at `Kind = "session.lifecycle"` with the Identity carrying `(tenant, user, session)` and an empty `RunID` (sessions are session-scoped, not run-scoped). No trick required.
- **brief 05 §5 — foreground/background identity is split in the predecessor.** Harbor unifies under `TaskID` (Phases 20-21); for Phase 08, the implication is that the GC's "RUNNING task" check is a single hook returning true if any task of any kind is running for the session. Phase 08 ships a `RunningProbe` interface with a default no-op implementation (returns `false` always — there are no tasks yet); Phase 20's `TaskRegistry.Open` wires the real probe in.
- **brief 05 §6 — session lifetime tests required.** "Open → many runs → close. Reopen-after-close rejected. GC removes idle sessions with no RUNNING tasks but leaves sessions with active tasks alone." Each of those is a named test below.
- **brief 05 §9 Q-2 — GC defaults.** Brief proposed `idle TTL 24h, hard cap 30 days, sweep every 15 min, refuse-to-GC any session with a RUNNING task`. RFC §6.9 settles those exact values. Phase 08 ships them as the `GCPolicy` zero-value defaults.
- **D-027 — typed wrapper over generic StateStore.** "Consuming subsystems (sessions, tasks, planner checkpoints, memory snapshots, steering events, distributed bindings, trajectories) land their **typed wrappers at their own layer** atop this surface — not inside `internal/state`. Example: `SessionRegistry.Save(s Session)` reduces to `StateStore.Save(StateRecord{Identity: s.Identity, Kind: "session.lifecycle", Bytes: marshal(s)})`." Phase 08 is the first phase to consume that surface; the wrapper pattern lands here as the canonical example for Phases 20, 23, 42, 50 onwards.
- **D-001 — identity is the triple.** Sessions are scoped per-tenant-per-user-per-session; the same `SessionID` value reused across tenants is a different session entirely (and reopening across tenants is the operation `Open` must reject). The `Quadruple` for a session-lifecycle record carries `(tenant, user, session, RunID="")` — the empty-RunID-allowed clause in StateStore is what makes that legal.

## Findings I'm departing from (if any)

- **None.** Phase 08 follows brief 05 and RFC §6.9 verbatim. The one mechanical novelty — the `RunningProbe` hook — is the cleanest way to honor the "never reaps RUNNING tasks" invariant without forward-importing Phase 20. Documented in this plan; no departure from a brief finding.

## Goals

- Ship `internal/sessions` as a thin lifecycle layer over Phase 07's StateStore, codifying the D-027 typed-wrapper pattern.
- Pin the four session-lifetime invariants (identity immutability, no reopen-after-close, cross-tenant reuse rejection, never-reap-RUNNING) as binding tests so any future driver-side change can't quietly weaken them.
- Land a `GCPolicy` configurable from `SessionsConfig` with the RFC §6.9 defaults; the GC sweeper runs as a managed goroutine that `Close(ctx)` joins cleanly (no goroutine leaks, D-025).
- Define the `RunningProbe` seam with a no-op default so the runtime can ship today; Phase 20 swaps in the real probe by registering it in its own `init()` or via constructor wiring.
- Emit canonical events for session lifecycle (`session.opened`, `session.touched`, `session.closed`, `session.gc_reaped`) so the bus has the shape downstream Console phases will subscribe to. Phase 08 declares the EventTypes + payloads; emission paths are wired in this phase.

## Non-goals

- No SQLite or Postgres state drivers (Phases 15 / 16). Phase 08 requires Phase 07's in-memory StateStore only; the conformance surface guarantees later drivers behave identically without changing Phase 08.
- No Protocol-wire surface (`sessions.open / list / inspect / close` per Phase 60). Phase 08 ships the Go-level `SessionRegistry` interface only; the Protocol method set lands in Phase 60.
- No `SessionContext` versioning / hash / migration logic. RFC §6.9 sketches `Context: SessionContext` for "version, hash, llm/tool ctx, memory, artifacts" but those subsystems land in later phases. Phase 08 includes the field as `map[string]any` reserved for future use; the field is round-tripped through marshal/unmarshal but no validation is applied in V1.
- No `SessionLimits` enforcement. The struct is reserved on `Session` but enforcement (e.g., max tokens per session, max tools) belongs to Phase 36a/b (Governance) and Phase 26 (Tool catalog). Phase 08 stores limits unmodified.
- No `RUNNING`-task detection logic in this phase. The `RunningProbe` returns `false` always until Phase 20 wires the real probe; the seam is documented and tested with a stub-true probe to prove GC honors a non-no-op probe.
- No cross-session memory promotion / shared scope (Phase 23+ memory territory).
- No re-architecting of Phase 07's `StateStore`. Phase 08 calls `StateStore.Save / Load / Delete` and never touches its internals.

## Acceptance criteria

- [ ] `internal/sessions/sessions.go` defines `Session{ID, Identity, OpenedAt, LastSeen, Closed, ClosedAt, ClosedReason, Limits, Context}`, the `SessionRegistry` interface, `GCPolicy{IdleTTL, HardCap, SweepInterval, RunningProbe}`, the `RunningProbe func(ctx context.Context, id identity.Quadruple) (bool, error)` seam, and the sentinel errors enumerated under "Public API surface".
- [ ] `internal/sessions/registry.go` provides `New(store state.StateStore, cfg config.SessionsConfig, bus events.EventBus) (*Registry, error)`. The `*Registry` is the single concrete implementation; no driver pluralism (per AGENTS.md §4.4 — "no optional-capability ceremony when all V1 drivers will implement everything"; sessions has only one impl: StateStore-backed).
- [ ] `Open(ctx, id, ident)` validates the identity triple is non-empty (returns wrapped `identity.ErrIdentityIncomplete` if not), constructs the `state.StateRecord` with `Identity = identity.Quadruple{Identity: ident, RunID: ""}` and `Kind = "session.lifecycle"`, calls `store.Save`. If a record with the same `(Identity, Kind)` already exists AND was closed, `Open` rejects with `ErrReopenAfterClose`. If a record exists with the same `SessionID` but a different `(TenantID, UserID)`, rejects with `ErrSessionIDReuse`.
- [ ] `Get(ctx, id)` reads the latest `(Identity, Kind="session.lifecycle")` record from the StateStore. Identity is taken from `ctx` per the runtime's identity-mandatory contract (D-001); the `id` parameter is the `SessionID` only — Phase 08 does not introduce a "get any session by ID" admin operation (Phase 60 will).
- [ ] `Touch(ctx, id)` updates `LastSeen = time.Now()` and re-saves with a new `EventID`. Idempotent on simultaneous Touches: same `EventID` for two concurrent Touches in the same nanosecond is benign (StateStore handles idempotency on `EventID`).
- [ ] `Close(ctx, id, reason)` sets `Closed = true`, `ClosedAt = time.Now()`, `ClosedReason = reason`, re-saves, emits a `session.closed` event onto the bus. `Close` is idempotent: closing an already-closed session returns `nil` (no error) but does NOT update `ClosedReason` (the original reason wins).
- [ ] `Inspect(ctx, id)` returns a `SessionSnapshot` (separate type) suitable for Protocol exposure later. The snapshot includes the lifecycle fields plus a "running" boolean derived from `RunningProbe`.
- [ ] `GC(ctx, policy)` performs a single sweep: lists all open sessions (helper `listOpen`), for each: if `RunningProbe(ctx, q)` returns true, skip; else if `LastSeen + IdleTTL < now` OR `OpenedAt + HardCap < now`, close with reason `"gc:idle"` or `"gc:hard_cap"`. Returns `(reapedCount, nil)` or `(reapedCount, firstErr)`. The sweeper goroutine calls `GC` on `policy.SweepInterval` (default 15 min); the goroutine is cancelled by `Registry.Close(ctx)`.
- [ ] **Identity captured immutably on Open.** Once a session is `Open`ed, no API path can change its `(TenantID, UserID, SessionID)`. Touch / Close re-save the SAME identity from the existing record; an attacker passing a different identity in `ctx` for `Touch` is rejected with `ErrIdentityMismatch` (the registry compares `ctx`'s identity to the loaded record's identity). Pinned by `TestRegistry_Identity_Immutable_AcrossTouch`.
- [ ] **Reopen-after-close rejected.** A `Open(ctx, id, ident)` after `Close(ctx, id, "...")` returns `ErrReopenAfterClose`. Pinned by `TestRegistry_Open_AfterClose_Rejected`. (The test also asserts the closed record is preserved — clients wanting a fresh session pick a new `SessionID`.)
- [ ] **Cross-tenant SessionID reuse rejected.** Tenant A's `Open` with `SessionID=S` is followed by Tenant B's `Open` with `SessionID=S`; the second call MUST return `ErrSessionIDReuse` (NOT silently succeed by writing to a different StateStore key — the rejection is the load-bearing security guarantee, not just a deduplication ergonomic). Pinned by `TestRegistry_CrossTenant_SessionIDReuse_Rejected`.
- [ ] **GC never reaps RUNNING.** With a stub `RunningProbe` returning `true` always: `GC` reaps zero sessions even when they're past `IdleTTL`. Pinned by `TestRegistry_GC_NeverReapsRunning`.
- [ ] **GC respects IdleTTL and HardCap.** With `RunningProbe` returning `false`: `GC` reaps sessions past `IdleTTL` (idle-reap path) AND sessions past `HardCap` even when recently `Touch`ed (hard-cap path). Pinned by two separate tests; the hard-cap test uses a controllable clock (per AGENTS.md §11 — no `time.Sleep` for synchronization).
- [ ] **GC emits `session.gc_reaped` events.** Each reaped session emits one event with the lifecycle reason in the payload. The test asserts the bus subscriber sees N events for N reaped sessions, with matching identity quadruples.
- [ ] `internal/sessions/events.go` declares the EventTypes (`session.opened`, `session.touched`, `session.closed`, `session.gc_reaped`) and their payload types (each implements `events.SafePayload` — these are Harbor-internal lifecycle markers with no secret-shaped fields by construction). Registers them via `init()` so they appear in `events.EventTypes()` and pass the Phase 05 `TestEventTypes_Exhaustiveness` extension.
- [ ] `internal/config/config.go` populates `SessionsConfig` from its zero-value reservation: `IdleTTL time.Duration` (default 24h), `HardCap time.Duration` (default 30d = 720h), `SweepInterval time.Duration` (default 15m). All fields default per the values above when zero-valued; none are `reload:"live"` in V1.
- [ ] `internal/config/validate.go` adds a `SessionsConfig` validator that rejects negative durations, `IdleTTL > HardCap` (incoherent), or `SweepInterval > IdleTTL` (would cause sessions to live past TTL up to one sweep) with the standard `config.sessions.<field>` error path.
- [ ] No package-level mutable state on `*Registry` or its goroutines. Compiled registry is reusable across N goroutines (D-025); the sweeper goroutine is owned by the Registry and joined on Close.
- [ ] Coverage on `internal/sessions` ≥ 85% (matches master plan target).
- [ ] **Concurrent-reuse test (D-025):** `TestRegistry_ConcurrentReuse_ReuseContract` runs ≥100 goroutines concurrently `Open`ing distinct sessions, ≥10 goroutines `Touch`ing existing sessions, ≥10 calling `Close`, against a single shared `*Registry`; under `-race`, asserts no data races, no goroutine leaks after `Close(ctx)`, and that all sessions end up in a consistent state (Closed XOR open; identity matches the open call's identity).
- [ ] **Cross-tenant isolation test:** `TestRegistry_CrossTenant_OpenIsolation` opens 8 tenants × 4 sessions each in parallel; asserts no tenant sees another tenant's sessions in `Get`, `Inspect`, or the GC reaper output. Pins AGENTS.md §13 forbidden practice.
- [ ] **Goroutine leak test:** `TestRegistry_NoGoroutineLeak_AfterClose` asserts `runtime.NumGoroutine` returns to baseline within 2s of `Close(ctx)` for: idle registry, registry mid-GC sweep, registry with active producer goroutines.
- [ ] **Integration test:** `test/integration/sessions_state_test.go` wires real config + audit + events + state + sessions and exercises Open → Touch → Close → GC against the real in-memory StateStore. Verifies identity propagation through the StateStore boundary, covers `ErrReopenAfterClose` as the failure mode, runs under `-race` (per AGENTS.md §17).
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-08.sh` smoke script runs `go test -race ./internal/sessions/...` (Go-package only; HTTP surface still SKIPs).

## Files added or changed

```text
internal/sessions/sessions.go              # Session, SessionRegistry iface, GCPolicy, RunningProbe, sentinels
internal/sessions/registry.go              # *Registry (StateStore-backed) — single concrete impl
internal/sessions/gc.go                    # sweeper goroutine + GC loop
internal/sessions/events.go                # session.* EventTypes + payloads (SafePayload)
internal/sessions/sessions_test.go         # unit tests (lifecycle, identity, GC, isolation)
internal/sessions/registry_test.go         # registry-specific tests (concurrent reuse, leak)
internal/config/config.go                  # SessionsConfig populated
internal/config/validate.go                # validator entry
test/integration/sessions_state_test.go    # cross-subsystem integration test
scripts/smoke/phase-08.sh                  # smoke skeleton (Go-package SKIP + go test invocation)
docs/plans/README.md                       # Status: 08 → Shipped (in the implementation PR, not this plan PR)
```

## Public API surface

```go
package sessions

type Session struct {
    ID           string                 // SessionID — opaque to the registry
    Identity     identity.Identity      // (TenantID, UserID, SessionID) — immutable after Open
    OpenedAt     time.Time
    LastSeen     time.Time
    Closed       bool
    ClosedAt     time.Time              // zero when Closed=false
    ClosedReason string
    Limits       SessionLimits          // reserved for Phase 36a/b enforcement
    Context      map[string]any         // reserved for SessionContext (Phase 23+)
}

type SessionLimits struct {
    // Reserved. Populated by Phase 36a (cost ceilings) and Phase 26 (tool catalog).
}

type SessionSnapshot struct {
    Session
    Running bool   // derived from GCPolicy.RunningProbe at inspection time
}

type GCPolicy struct {
    IdleTTL       time.Duration  // default 24h
    HardCap       time.Duration  // default 720h (30d)
    SweepInterval time.Duration  // default 15m
    RunningProbe  RunningProbe   // default returns (false, nil) — Phase 20 plugs the real probe
}

// RunningProbe is the seam Phase 20 (TaskRegistry) plugs into so GC can
// honor "never reap a session with a RUNNING task." A nil probe is
// treated as the no-op default.
type RunningProbe func(ctx context.Context, q identity.Quadruple) (bool, error)

type SessionRegistry interface {
    Open    (ctx context.Context, id string, ident identity.Identity) (*Session, error)
    Get     (ctx context.Context, id string) (*Session, error)
    Touch   (ctx context.Context, id string) error
    Close   (ctx context.Context, id string, reason string) error
    Inspect (ctx context.Context, id string) (*SessionSnapshot, error)
    GC      (ctx context.Context, policy GCPolicy) (int, error)

    // Close cancels the sweeper goroutine and joins it. Idempotent.
    CloseRegistry(ctx context.Context) error
}

var (
    ErrReopenAfterClose      = errors.New("sessions: reopen-after-close forbidden")
    ErrSessionIDReuse        = errors.New("sessions: SessionID reused across tenants/users")
    ErrIdentityMismatch      = errors.New("sessions: ctx identity mismatches stored session identity")
    ErrSessionNotFound       = errors.New("sessions: session not found")
    ErrSessionAlreadyOpen    = errors.New("sessions: session already open")
)
```

## Test plan

- **Unit:**
  - `TestRegistry_Open_HappyPath`, `TestRegistry_Open_DuplicateOpenSameTenant_Rejected`,
  - `TestRegistry_Open_AfterClose_Rejected`, `TestRegistry_Open_EmptyIdentity_Rejected`,
  - `TestRegistry_CrossTenant_SessionIDReuse_Rejected`,
  - `TestRegistry_Touch_UpdatesLastSeen`, `TestRegistry_Touch_OnClosed_Rejected`,
  - `TestRegistry_Close_Idempotent`, `TestRegistry_Close_EmitsEvent`,
  - `TestRegistry_Inspect_RunningFromProbe`, `TestRegistry_Inspect_OnClosed`,
  - `TestRegistry_GC_NeverReapsRunning`, `TestRegistry_GC_ReapsIdleSession`,
  - `TestRegistry_GC_HardCapWins_OverRecentTouch`, `TestRegistry_GC_EmitsGCReapedEvent`,
  - `TestRegistry_Identity_Immutable_AcrossTouch`,
  - `TestRegistry_Sweeper_StartsAndStops_NoLeak`.
- **Integration:** `test/integration/sessions_state_test.go` per AGENTS.md §17 — real audit + state + events + sessions, identity propagation through StateStore, ≥1 failure mode (`ErrReopenAfterClose`), under `-race`. Wires Open → Touch → Close → GC and asserts the lifecycle events land on a real bus subscription.
- **Conformance:** N/A — single concrete impl. Driver pluralism arrives via Phase 15 (SQLite state) + Phase 16 (Postgres state) at the StateStore layer, not at the SessionRegistry layer.
- **Concurrency / leak:** `TestRegistry_ConcurrentReuse_ReuseContract` (≥100 concurrent Open/Touch/Close on a shared `*Registry`, under `-race`, baseline goroutines restored after `CloseRegistry` — D-025); `TestRegistry_NoGoroutineLeak_AfterClose` per the standard leak pattern; `TestRegistry_CrossTenant_OpenIsolation` for the multi-tenant invariant.

## Smoke script additions

- `phase-08.sh`: Go-package-only phase. Runs `go test -race ./internal/sessions/...` against the built tree. Mirrors phase-05 / phase-07 style (Go-package-only SKIP with a real `go test` invocation as the gate).

## Coverage target

- `internal/sessions`: 85%
- `internal/config`: not regressed by the new field

## Dependencies

- 01 (identity)
- 07 (StateStore iface + InMem)

## Risks / open questions

- **`RunningProbe` lifecycle.** The probe is set on `GCPolicy` at registry construction. Phase 20 (TaskRegistry) will need to wire its real probe in BEFORE the sweeper goroutine starts a sweep — otherwise an early sweep could reap a session whose tasks are running. Phase 08 documents that the probe MUST be set in production wiring (`cmd/harbor`) before `Open` is allowed; the test surface uses a stub probe. Phase 20's plan should reference this contract.
- **GC under load.** The brief flags concurrency tests on N×M sessions. With 10k sessions, a per-sweep `listOpen + RunningProbe` walk is O(N) memory + O(N) probe calls. Acceptable for V1 (sessions count is small per tenant); a future driver-side optimization (cursor-based listing) is out of scope here. Documented.
- **Hard-cap clock skew.** `HardCap` is a wall-clock check. If the host's clock jumps backward (NTP correction), a session that should have been reaped lives longer; if it jumps forward, sessions are reaped early. Tested with a controllable clock in `TestRegistry_GC_HardCapWins_OverRecentTouch`; documented as an operator-facing risk that can be mitigated by monotonic-clock-only checks once Go's `time` API supports them cleanly. Not gated.
- **Inspect's `Running` field staleness.** `Running` is read from the probe at inspection time; by the time the caller reads the snapshot, the value may be stale. This is intrinsic to any "snapshot" model and matches RFC §6.9's intent. Documented.
- **No `harbor dev`-side default subscription filter** — the `session.*` event family will land on the bus and Console (Phase 72-75) will subscribe; until then, operators see them via Phase 04 logger paths. Not a blocker.

## Glossary additions

- **SessionRegistry.** Subsystem that manages session lifecycle (Open / Touch / Close / GC). One concrete impl, StateStore-backed. Distinct from `state.StateStore` — sessions is the typed-wrapper layer that Phase 27+ subsystems consume.
- **GCPolicy.** Configuration knob group for the sessions GC sweeper. Defaults: `IdleTTL=24h, HardCap=720h, SweepInterval=15m`. Fields are not hot-reloadable in V1.
- **RunningProbe.** Function-typed seam the SessionRegistry's GC consults before reaping a session. Default returns `(false, nil)`; Phase 20 (TaskRegistry) wires the real probe.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestRegistry_CrossTenant_OpenIsolation` + `TestRegistry_CrossTenant_SessionIDReuse_Rejected`)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — `TestRegistry_ConcurrentReuse_ReuseContract`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. — `test/integration/sessions_state_test.go`.
- [ ] If new vocabulary: glossary updated (SessionRegistry, GCPolicy, RunningProbe)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none in Phase 08; section says "None")
