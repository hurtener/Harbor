# Phase 130 — Session erasure Protocol method (data-lifecycle deletion)

## Summary

Ship `sessions.delete` — the first identity-scoped **erasure** surface on
the Harbor Protocol. Today the wire exposes `sessions.list` /
`sessions.inspect` (read), `state.history` (read), and `artifacts.delete`
(a single-object delete), but there is **no operator- or client-initiated
way to erase a whole conversation and its scoped data**: the
SessionManager GC only reaps *idle* sessions on its own schedule, and a
generic Protocol client (a third-party Console, an IDE/TUI client, an SDK
consumer) has no method to satisfy a data-lifecycle / right-to-erasure
request. This phase adds an identity-scoped `sessions.delete` method that
deletes a session and **cascades deletion of its scoped State, Memory, and
Artifacts**, refusing fail-loud to delete a session with a RUNNING task
(mirroring the GC never-reap-running invariant), gated by the same
own-identity / `auth.ScopeAdmin` scope contract as `sessions.list`, and
emitting a redacted audited lifecycle event. The primitive (method + wire
types + error code + capability + the StateStore cascade primitive) lands
with its consumer in the same phase (CLAUDE.md §13): a server handler that
performs the real three-store cascade, exercised end-to-end by an
integration test (delete → subsequent read returns `not_found`,
cross-store erasure proven, cross-tenant rejected, running-task refused)
and a live smoke client.

## RFC anchor

<!-- Every reference below resolves to a real RFC heading (drift-audit gate). -->
- RFC §5.2 — What the Protocol exposes (the state / task-control surface;
  this phase adds an erasure entry alongside the existing `artifacts.delete`
  scope-checked deletion the table already lists).
- RFC §6.9 — Sessions and SessionManager (the GC "never reap a session
  with a RUNNING task" invariant this phase mirrors as a fail-loud erasure
  refusal; the SessionRegistry record this phase hard-deletes).
- RFC §6.11 — StateStore (the identity-scoped persistence floor; this phase
  adds the cascade primitive `DeleteScope` to the single mandatory
  interface, with conformance parity across all three V1 drivers).
- RFC §6.13 — Typed event bus (the redacted `session.erased` lifecycle
  event the cascade emits for cross-cutting observers).
- RFC §7 — Console / audit layer (the audit trail the erasure writes to the
  compliance sink, distinct from the now-erased session's own data).

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §14: *"A session contains many runs (foreground tasks).
  Identity is the triple `(tenant_id, user_id, session_id)`."* — erasure is
  scoped to exactly this triple and MUST cascade across **every run** of the
  session, so the StateStore cascade matches `(tenant, user, session)`
  ignoring `run_id`/`kind` (run-scoped trajectory records, planner
  checkpoints, and the durable event stream all live under the session and
  all go).
- brief 05 (sessions / SessionManager): the SessionManager reaps idle
  sessions but **never reaps a session with a running task** — a session is
  durable execution state, not a cache entry. This phase reads that
  invariant as a hard precondition: a client/operator erasure of a session
  whose task is still RUNNING is refused **loudly** with a distinct error,
  never silently deferred or partially applied.
- brief 06 §1: *"it guarantees Console, third-party consoles, and
  `harbor dev` see exactly the same data shape that production
  observability sees. There is no privileged 'internal' view."* — erasure
  is a Protocol method any client invokes through the same canonical wire
  contract; there is no internal back-door delete. The erasure's audit +
  `session.erased` observability flows through the same typed event bus
  every other lifecycle event does.
- brief 06 (events / observability): destructive lifecycle operations must
  be **observable** — the erasure emits a redacted `session.erased` event
  (session id + actor scope + per-store deletion counts + timestamp, NO
  user content) so a fleet observer and the audit sink record that the
  erasure happened without re-persisting any of the erased content.

## Findings I'm departing from (if any)

None.

## Goals

- A new additive Protocol method `sessions.delete` (single-sourced in
  `internal/protocol/methods`), identity-mandatory, that erases a session
  and cascades deletion of its scoped State, Memory, and Artifacts.
- A fail-loud refusal — distinct error `session_running` (HTTP 409) — when
  the target session has a RUNNING task, mirroring the GC never-reap-running
  invariant (RFC §6.9). The runtime never partially erases a running
  session.
- The same scope contract as `sessions.list` / `state.history`: a non-admin
  caller may erase **only their own** `(tenant, user, session)`; a
  cross-tenant or other-user erasure requires the verified `auth.ScopeAdmin`
  claim on the ctx (the request body carries no elevation knob).
- A real cascade **consumer in the same phase** (§13): a server handler
  (extending the existing `POST /v1/sessions/` handler with a `delete`
  branch) that performs the three-store cascade through the production
  drivers, proven end-to-end by an integration test and a live smoke
  assertion — delete → subsequent `sessions.inspect` / `state.history`
  returns `not_found` / empty, cross-store erasure asserted, cross-tenant
  rejected, running-task refused.
- A negotiable destructive surface: a new `CapSessionLifecycle` capability
  so a read-only client can detect (via `runtime.info`) whether the runtime
  it attached to supports erasure, rather than discovering it by a 404 at
  call time.
- An auditable, observable erasure: a redacted `session.erased` event on
  the canonical bus + an audit-sink record, carrying NO user content and
  NOT re-persisted under the erased session's own identity.
- A StateStore cascade primitive `DeleteScope(ctx, identity.Identity)
  (int, error)` added to the single mandatory interface with conformance
  parity across in-mem / SQLite / Postgres (RFC §6.11, §9 — no driver-only
  feature).

## Non-goals

- **Cross-session / bulk erasure (a "delete everything for this user /
  tenant" verb).** `sessions.delete` erases ONE session per call. A
  user-wide or tenant-wide purge is a heavier surface (it crosses the
  isolation triple and needs its own elevation + audit design) and is
  deferred. A caller erasing many sessions iterates the single method.
- **Purging the USER-scope agent-config revision store.** The durable
  per-user agent-config variant + revision history (the user-scope config
  tier) is keyed by `(tenant, user)`, NOT by session — it deliberately
  survives any single session and is a control-plane concern, not session
  data. Erasing one session MUST NOT touch it. A user-scope config purge,
  if ever needed, is a separate config-plane verb. Documented in Risks.
- **Soft-delete / tombstone of the erased DATA.** Right-to-erasure requires
  the State / Memory / Artifact bytes to actually be removed, not flagged. A
  tombstone is recorded only as the audit trail (the `session.erased`
  record in the compliance sink), never as retained user content. See Risks
  for the hard-delete-vs-tombstone decision.
- **A new wire transport or auth mechanism.** `sessions.delete` reuses the
  existing `POST /v1/sessions/` handler, the existing `resolveIdentity` /
  `auth.HasScope` edge, and the existing JWKS verifier. No new transport,
  no new scope minted.
- **Bumping `ProtocolVersion`.** The method, error code, capability, and
  wire types are all additive (old clients neither call nor break). Per the
  `internal/protocol/types/version.go` Major/Minor/Patch taxonomy an
  additive surface is a Minor-class change; RFC §5.3's rule that *bumping
  the version is an RFC change* is precisely why this stays additive —
  `ProtocolVersion` holds.
- **A Console "delete chat" UI.** The typed TS client gains the
  request/response types (lockstep-mandatory), but rendering a delete button
  on the Sessions / Playground page is a follow-on Console phase that
  consumes this surface; it is NOT required to satisfy §13 here (the cascade
  handler + integration + smoke is the binding consumer). Noted in Risks.

## Acceptance criteria

<!-- Binding + testable. -->

- [ ] **Single-source method registration.** `methods.MethodSessionsDelete
  Method = "sessions.delete"` is added in `internal/protocol/methods/methods.go`
  ONLY, added to `canonicalMethods` and to the `IsSessionsMethod` set, with
  the wire route `POST /v1/sessions/delete`. No second definition site; the
  single-source checker stays green.
- [ ] **Single-source error code.** `protoerrors.CodeSessionRunning Code =
  "session_running"` is added in `internal/protocol/errors/errors.go` ONLY,
  added to `canonicalCodes`, and maps to HTTP **409 Conflict** in the
  sessions wire handler. `errors.Codes()` / `IsValidCode` reflect it. No
  other erasure failure invents a new code — incomplete identity is
  `identity_required` (401), cross-identity is `not_found` (404, existence
  never revealed across identities — mirrors `tasks.get` / `state.history`),
  cross-tenant-without-admin is `scope_mismatch` (403).
- [ ] **Single-source wire types.** `SessionsDeleteRequest` /
  `SessionsDeleteResponse` are defined ONLY in
  `internal/protocol/types/sessions.go` and registered in
  `internal/protocol/singlesource.CanonicalWireTypes`. The response carries
  non-sensitive deletion telemetry only: `session_id`, `deleted bool`,
  `state_records_deleted int`, `artifacts_deleted int`, `memory_purged bool`
  — no user content.
- [ ] **Negotiable capability.** `types.CapSessionLifecycle Capability =
  "session_lifecycle"` is added in `internal/protocol/types/version.go` ONLY,
  added to `canonicalCapabilities`, and is advertised on `runtime.info`
  **only when the erasure eraser is wired** (a runtime assembled without an
  eraser does not advertise it — capability gating). `types.Capabilities()`
  (the canonical universe) includes it.
- [ ] **StateStore cascade primitive with conformance parity.**
  `state.StateStore` gains `DeleteScope(ctx context.Context, id
  identity.Identity) (int, error)` — deletes every record whose
  `(tenant, user, session)` matches `id` regardless of `run_id` / `kind`
  (covering session-scoped state, run-scoped trajectories, planner
  checkpoints, and the durable event stream), returns the count deleted, is
  idempotent (0, nil on an absent scope), and fails closed with
  `ErrIdentityRequired` on an incomplete triple. It is **identity-scoped**
  (NOT the `MaintenanceScoped` elevation `ListKind` requires — it deletes
  only the caller's own session). All three V1 drivers (in-mem / SQLite /
  Postgres) implement it and the shared `state/conformancetest` suite
  exercises it.
- [ ] **Memory erasure.** The cascade calls `MemoryStore.Flush(ctx,
  identity.Quadruple{Identity: triple})` and an assertion proves that after
  erasure `GetLLMContext` returns the clean zero patch AND (for a semantic
  store) `SearchTurns` returns no residual turns — across **every** memory
  driver via the `memory/conformancetest` suite. If any driver's `Flush`
  leaves persisted/embedded residue, that driver is fixed in this same phase
  (§17.6 "fix what the test finds").
- [ ] **Artifact erasure.** The cascade enumerates the session's artifacts
  via `ArtifactStore.List(scope)` and `Delete(scope, id)`s each (both
  idempotent); after erasure `List(scope)` returns empty. Counted into
  `artifacts_deleted`.
- [ ] **Session-record hard-delete.** A new `Registry.Erase(ctx, id)
  error` removes the persisted SessionRegistry record (not a `Closed=true`
  tombstone) and the open-sessions map entry, after verifying the ctx
  identity matches the target. A subsequent `sessions.inspect` returns
  `not_found`; a subsequent `state.history` returns empty / `not_found`.
- [ ] **Fail-loud running-task refusal.** `Registry.Erase` consults the
  configured `RunningProbe` (the same seam GC uses); when it returns true,
  Erase returns a distinct sentinel `ErrSessionRunning`, the Service surfaces
  it, and the handler returns `session_running` (409). **No store is touched
  on refusal** (the probe is checked before any delete). Proven by a test
  with a RUNNING task on the probe.
- [ ] **Scope contract (own-identity vs admin).** A non-admin caller erases
  only their own verified `(tenant, user, session)`; a body identity
  mismatching the verified identity is rejected `identity_required` (the
  existing `assertSessionsIdentity` defence-in-depth); a session belonging to
  another identity is `not_found` (existence not revealed). A cross-tenant /
  other-user erasure succeeds ONLY with the verified `auth.ScopeAdmin` claim
  on the ctx, and that admin erasure emits the `audit.admin_scope_used`
  event (mirroring `sessions.list`).
- [ ] **Audited, observable, content-free erasure.** A successful erasure
  emits a redacted `session.erased` event on the canonical bus (session id +
  actor scope + per-store deletion counts + timestamp; run through the
  `audit.Redactor`; NO user content) for cross-cutting observers + the audit
  sink. The event is **NOT re-persisted under the erased session's own
  identity** — a post-erasure `state.history` for that triple returns empty,
  not "one erasure event". Asserted by a test.
- [ ] **Idempotent / retry-safe cascade.** Each per-store delete is
  idempotent, so a cascade interrupted by a transient store error returns
  the error loudly (never a partial silent success) and is safe to re-invoke
  to convergence. A fully-completed erasure of an already-absent session
  returns `not_found` (the session is gone). A unit test drives a forced
  mid-cascade store error and asserts a loud return + safe re-run.
- [ ] **Consumer end-to-end (§13).** The `delete` branch of the sessions
  handler, wired over the production State / Memory / Artifact drivers + a
  real Registry, is exercised by `test/integration/phase130_session_erasure_test.go`:
  a session is opened, given state + a turn + an artifact + a (completed)
  task, then erased over the real wire transport behind `httptest.Server`;
  the test asserts (a) `200` with the deletion counts, (b) a subsequent
  `sessions.inspect` → `not_found`, (c) `state.history` → empty, (d) the
  artifact `List(scope)` → empty, (e) memory `GetLLMContext` → clean, (f) a
  cross-tenant erasure without admin → `403 scope_mismatch`, (g) an erasure
  of a session with a RUNNING task → `409 session_running` with all stores
  untouched. Identity propagation asserted end-to-end. Runs under `-race`.
- [ ] **Hand-mirrored TS client + manifest lockstep (D-223).** The
  hand-maintained TS wire module (`web/console/src/lib/sessions/types.ts`)
  declares `SessionsDeleteRequest` / `SessionsDeleteResponse` with every
  field; `make protocol-ts-gen` regenerates `wire-manifest.gen.json` (new
  method, new error code, new capability, new types); `make
  protocol-ts-gen-check` passes on the regenerated tree and FAILS on a stale
  manifest or an un-mirrored TS field (proven by planted drift).
- [ ] **Generated Protocol docs regen (D-209).** `make protocol-docs-gen`
  regenerates `docs/site/protocol/methods.md`, `errors.md`, and `types.md`
  for the new method / code / types; `make protocol-docs-gen-check`
  (`git diff --exit-code`) passes and the generator lockstep test (a new
  method / code / wire type without its docs join row fails `go test`) stays
  green. Regenerated pages committed in the same PR.
- [ ] **Smoke.** `scripts/smoke/phase-130.sh` asserts the static
  registration (method, code, capability, TS mirror), the build gates
  (`protocol-ts-gen-check`, `protocol-docs-gen-check`), the Go tests under
  `-race`, and — when the live dev server exposes the route — a real
  delete→inspect-404 round-trip + the cross-tenant 403 + the running-task
  409, skipping per the 404/405/501 convention when the route or a dev token
  is unavailable. FAIL = 0.

## Files added or changed

```text
internal/protocol/methods/methods.go            # MethodSessionsDelete + canonicalMethods + IsSessionsMethod
internal/protocol/methods/methods_test.go       # registration + IsSessionsMethod predicate
internal/protocol/errors/errors.go              # CodeSessionRunning + canonicalCodes
internal/protocol/errors/errors_test.go         # IsValidCode / Codes() include session_running
internal/protocol/types/sessions.go             # SessionsDeleteRequest / SessionsDeleteResponse
internal/protocol/types/sessions_test.go        # JSON round-trip for the two new types
internal/protocol/types/version.go              # CapSessionLifecycle + canonicalCapabilities
internal/protocol/types/version_test.go         # capability registration + Capabilities() universe
internal/protocol/singlesource/singlesource.go  # register the two new wire types in CanonicalWireTypes
internal/sessions/registry.go                   # Registry.Erase(ctx, id) + ErrSessionRunning + ErrSessionNotFound reuse
internal/sessions/registry_test.go              # Erase happy path / running-refusal / idempotency / identity mismatch
internal/sessions/erasure.go                    # Cascade orchestrator: refuse-if-running -> artifacts -> memory -> state -> session record -> audited event
internal/sessions/erasure_test.go               # cascade ordering, forced mid-cascade error (loud + retry-safe), event content-free
internal/sessions/protocol/protocol.go          # Eraser seam + Service.Delete(ctx, req, adminScoped) + ErrSessionRunning mapping
internal/sessions/protocol/protocol_test.go     # Service.Delete scope matrix + admin audit emit
internal/state/state.go                         # StateStore.DeleteScope added to the interface + ErrIdentityRequired doc
internal/state/drivers/inmem/*.go               # DeleteScope impl
internal/state/drivers/sqlite/*.go              # DeleteScope impl (forward-only migration NOT needed — DELETE query only)
internal/state/drivers/postgres/*.go            # DeleteScope impl
internal/state/conformancetest/*.go             # DeleteScope conformance case (all drivers)
internal/memory/conformancetest/*.go            # post-Flush no-residual assertion (all drivers)
internal/protocol/transports/stream/sessions_handler.go   # `delete` branch + classifySessionsError(session_running -> 409)
internal/protocol/transports/stream/sessions_handler_test.go  # delete branch decode + error classification
internal/protocol/transports/transports.go      # wire the Eraser into the sessions Service (advertise CapSessionLifecycle when wired)
internal/protocol/posture.go                    # advertise CapSessionLifecycle when the eraser is wired
harbortest/devstack/*.go                        # assemble the eraser into the devstack sessions Service (cmd/harbor parity — §17.6)
cmd/harbor/cmd_dev.go                            # assemble the eraser into the dev server sessions Service (production parity)
web/console/src/lib/sessions/types.ts           # SessionsDeleteRequest / SessionsDeleteResponse interfaces (hand-mirrored)
web/console/src/lib/protocol/wire-manifest.gen.json  # regenerated — GENERATED, do not hand-edit
docs/site/protocol/methods.md                   # regenerated (sessions.delete) — GENERATED, do not hand-edit (D-209)
docs/site/protocol/errors.md                    # regenerated (session_running) — GENERATED, do not hand-edit (D-209)
docs/site/protocol/types.md                     # regenerated (SessionsDelete*) — GENERATED, do not hand-edit (D-209)
test/integration/phase130_session_erasure_test.go   # E2E cascade + scope matrix + running-refusal, -race
scripts/smoke/phase-130.sh
docs/plans/phase-130-session-erasure-method.md
docs/decisions.md                               # D-262
docs/glossary.md                                # "session erasure", "erasure cascade"
docs/plans/README.md                            # Phase 130 index row + detail block — Pending (V1.7)
README.md                                        # Status table Phase 130 row + a one-line pointer on sessions.delete
docs/skills/use-the-harbor-protocol/SKILL.md    # if it enumerates the session methods, add sessions.delete (§18 same-PR rule)
docs/skills/observe-with-the-console/SKILL.md   # if it quotes the session lifecycle, note the erasure event (§18)
```

No new top-level directory (AGENTS.md §3 unchanged): `internal/sessions/erasure.go`
is a new file in the existing sessions package; all other touch points are
existing packages.

## Public API surface

```go
// internal/protocol/methods
const MethodSessionsDelete Method = "sessions.delete" // wire route POST /v1/sessions/delete

// internal/protocol/errors
const CodeSessionRunning Code = "session_running" // HTTP 409 — erasure refused: a task is RUNNING

// internal/protocol/types
const CapSessionLifecycle Capability = "session_lifecycle"

// SessionsDeleteRequest is the sessions.delete request body. Identity is
// the verified triple; the body carries NO elevation knob — a cross-tenant
// erasure is authorized by the verified auth.ScopeAdmin claim on the ctx.
type SessionsDeleteRequest struct {
    Identity IdentityScope `json:"identity"`
}

// SessionsDeleteResponse reports the erasure outcome with non-sensitive
// telemetry only — never any erased user content.
type SessionsDeleteResponse struct {
    SessionID           string `json:"session_id"`
    Deleted             bool   `json:"deleted"`
    StateRecordsDeleted int    `json:"state_records_deleted"`
    ArtifactsDeleted    int    `json:"artifacts_deleted"`
    MemoryPurged        bool   `json:"memory_purged"`
}

// internal/state — the cascade primitive added to the single mandatory interface.
//
// DeleteScope removes every record whose (tenant, user, session) matches
// id, regardless of run_id or kind. It is identity-scoped (NOT the
// MaintenanceScoped elevation ListKind requires — it deletes only the
// caller's own session). Idempotent: an absent scope returns (0, nil).
// Fails closed with ErrIdentityRequired on an incomplete triple. Returns
// the number of records deleted.
DeleteScope(ctx context.Context, id identity.Identity) (int, error)

// internal/sessions — the session-record hard-delete + cascade.
//
// Erase removes the SessionRegistry record (hard delete, not a Closed
// tombstone) for the session in ctx after verifying the ctx identity
// matches. Returns ErrSessionRunning when the configured RunningProbe
// reports a RUNNING task (no record is touched), ErrSessionNotFound when no
// record is visible to the caller's identity.
func (r *Registry) Erase(ctx context.Context, id string) error

var ErrSessionRunning = errors.New("sessions: cannot erase a session with a running task")

// internal/sessions/protocol — the Eraser seam the Service depends on and
// the dispatch method the wire handler calls.
type Eraser interface {
    // Erase performs the full three-store cascade + session-record delete
    // for the verified identity, refusing fail-loud on a running task.
    Erase(ctx context.Context, id identity.Identity, adminScoped bool) (prototypes.SessionsDeleteResponse, error)
}

func (s *Service) Delete(ctx context.Context, req prototypes.SessionsDeleteRequest, adminScoped bool) (prototypes.SessionsDeleteResponse, error)
```

```typescript
// web/console/src/lib/sessions/types.ts (hand-mirrored, lockstep-gated)
export interface SessionsDeleteRequest { identity: IdentityScope; }
export interface SessionsDeleteResponse {
  session_id: string;
  deleted: boolean;
  state_records_deleted: number;
  artifacts_deleted: number;
  memory_purged: boolean;
}
```

## Test plan

- **Unit (Go):**
  - `methods` — `MethodSessionsDelete` registered, `IsSessionsMethod`
    true, route string correct.
  - `errors` — `CodeSessionRunning` in `canonicalCodes`, `IsValidCode`
    true, `Codes()` includes it.
  - `types` — `SessionsDelete*` JSON round-trip; `CapSessionLifecycle`
    in `Capabilities()` universe; `version_test` capability registration.
  - `singlesource` — the two new wire types resolve in `CanonicalWireTypes`.
  - `internal/sessions` — `Registry.Erase`: happy-path hard-delete (record
    gone, map entry gone), running-task refusal (`ErrSessionRunning`, no
    store touched), identity-mismatch rejection, already-absent →
    `ErrSessionNotFound`. `erasure.go` — cascade ordering, a forced
    mid-cascade store error returns loud + leaves a retry-safe state, the
    `session.erased` event carries counts but NO user content (redactor
    asserted), the event is not re-persisted under the erased triple.
  - `internal/sessions/protocol` — `Service.Delete` scope matrix
    (own-identity success; cross-tenant refused without admin →
    `ErrCrossTenantScope`; admin success emits `audit.admin_scope_used`).
- **Integration:** `test/integration/phase130_session_erasure_test.go` —
  REAL State / Memory / Artifact production drivers + a real Registry behind
  the REAL `POST /v1/sessions/` handler in an `httptest.Server`. Full
  lifecycle: open → write state + turn + artifact + a completed task →
  `sessions.delete` over the wire → assert counts, then assert
  `sessions.inspect` 404, `state.history` empty, artifact `List` empty,
  memory `GetLLMContext` clean. Failure modes: cross-tenant without admin →
  403 `scope_mismatch`; RUNNING-task → 409 `session_running` with stores
  untouched. Identity propagation asserted end-to-end. `-race`.
- **Conformance:** `state/conformancetest` gains a `DeleteScope` case all
  three drivers pass (deletes all kinds/runs for the triple, idempotent,
  identity-required); `memory/conformancetest` gains a post-`Flush`
  no-residual case all drivers pass.
- **Concurrency / leak:** N≥100 concurrent `sessions.delete` dispatches
  against a single shared sessions `Service` + shared stores, each erasing a
  **distinct** session, asserting no cross-talk (session A's erasure never
  touches session B's data), no data race, no goroutine leak (baseline
  restored). Plus a cross-session-isolation assertion: a concurrent erase of
  session A and a read of session B return B intact.

## Smoke script additions

`scripts/smoke/phase-130.sh` adds (using `scripts/smoke/common.sh` helpers —
`assert_grep_present`, `assert_post_status`, `api_url`, `skip_if_404`; no new
curl wrappers):

- Static: `methods.go` declares `MethodSessionsDelete` and `sessions.delete`;
  `errors.go` declares `CodeSessionRunning` / `session_running`;
  `version.go` declares `CapSessionLifecycle` / `session_lifecycle`;
  `internal/protocol/types/sessions.go` declares `SessionsDeleteRequest` /
  `SessionsDeleteResponse`.
- Static: `web/console/src/lib/sessions/types.ts` declares
  `SessionsDeleteRequest` / `SessionsDeleteResponse`.
- Static single-source defence: `sessions.delete` and `session_running`
  appear exactly once as a definition (no second `Method` / `Code`
  declaration outside `methods.go` / `errors.go`).
- Build/test: `make protocol-ts-gen-check` passes (manifest + TS in
  lockstep); `make protocol-docs-gen-check` passes (D-209);
  `go test -race ./internal/state/... ./internal/sessions/... ./internal/protocol/...`
  and `go test -race -run TestE2E_Phase130 ./test/integration/...` pass.
- Live (skips per 404/405/501): when the dev server exposes
  `POST /v1/sessions/delete`, open a throwaway dev session, write a marker,
  `assert_post_status 200` the delete with the dev identity body, then assert
  a follow-up `sessions.inspect` returns 404 `not_found`; assert a
  cross-tenant delete body is rejected 403; assert a delete of a session with
  a running task is rejected 409. SKIP when the route 404s or no dev token is
  available (same posture as `phase-72f.sh` / the sessions smoke).

## Coverage target

- `internal/sessions` (Erase + erasure cascade): ≥ 85%.
- `internal/sessions/protocol` (Service.Delete): no regression below the
  package's existing ≥ 85%.
- `internal/state` (DeleteScope across drivers, via conformance): ≥ 85%.
- `internal/protocol/transports/stream` (delete branch + classification):
  no regression below the package's existing target.

## Dependencies

- Phase 08 — Sessions + SessionManager (the `Registry`, the `RunningProbe`
  GC seam, the session record this phase hard-deletes).
- Phase 11 — StateStore (the interface this phase extends with `DeleteScope`
  + the conformance suite).
- Phase 17 — Memory store (the `Flush` erasure primitive + conformance).
- Phase 18 — Artifacts store (`List` + `Delete` cascade).
- Phase 58 — `internal/protocol` single-source layout + `CanonicalWireTypes`
  (the registration home for the new method / code / capability / types).
- Phase 60 — the wire transport (`POST /v1/sessions/` handler + the control
  transport the integration test drives the method through).
- Phase 61 — auth (the verified identity + `auth.ScopeAdmin` scope the
  erasure scope contract reads at the edge).

## Risks / open questions

- **Hard-delete vs tombstone — RESOLVED to hard-delete for data, audit-trail
  tombstone for the record-of-fact.** Right-to-erasure semantics require the
  State / Memory / Artifact bytes to be *actually removed*, not flagged — a
  tombstone that retains content defeats the purpose. So the cascade hard-
  deletes all three stores AND the SessionRegistry record (not the existing
  `Closed=true` soft-close tombstone). The *only* durable trace is the
  redacted `session.erased` audit record (session id + actor + counts +
  timestamp, NO content), which lives in the audit/compliance sink — NOT
  under the erased session's own identity. Recorded in D-262.
- **No ACID transaction across three independent stores — fail-loud,
  idempotent, ordered cascade instead.** State / Memory / Artifacts are
  separate drivers, possibly on different backends; a single distributed
  transaction is impossible. The cascade is ordered (refuse-if-running →
  artifacts → memory → state → session record → audited event) with
  per-store idempotent deletes; a mid-cascade error returns **loudly**
  (never a partial silent success — CLAUDE.md §13) and is safe to re-invoke
  to convergence. The running-probe check happens FIRST so a refusal touches
  nothing. Documented in D-262; the unit test forces a mid-cascade error and
  asserts loud-return + retry-safety.
- **USER-scope agent-config revision purge — OUT OF SCOPE (lean).** The
  durable per-user agent-config variant + its revision history is keyed by
  `(tenant, user)`, not by session; it deliberately survives any single
  session and is a control-plane concern, not session data. Erasing a
  session MUST NOT purge it. If a "forget this user's config" operation is
  ever needed it is a separate config-plane verb with its own elevation +
  audit. Noted as a non-goal; flagged here so it is not silently conflated.
- **Memory `Flush` completeness across drivers.** `Flush` is the documented
  "reset memory for id to a clean state" primitive, but semantic/durable
  memory may hold embeddings in a separate backend. The
  `memory/conformancetest` post-`Flush` no-residual case is the gate: if any
  driver's `Flush` leaves retrievable residue, that driver is fixed in this
  phase (§17.6). If a deeper purge than `Flush` provides is required, a
  `MemoryStore.Purge` addition is the fallback — recorded as the residual
  open question, but the lean is `Flush` suffices.
- **The `session.erased` event must not re-create per-session durable
  state.** The durable event bus persists events by identity; emitting the
  erasure event naively could write a new record under the just-erased
  triple, leaving `state.history` non-empty. The cascade emits the erasure
  observation to the bus/audit sink for cross-cutting observers but does NOT
  re-persist it under the erased identity; a test asserts `state.history` for
  the triple is empty post-erasure. (RFC §6.13 / §7.)
- **Existence non-disclosure across identities.** A non-admin erasure of a
  session belonging to another `(tenant, user)` returns `not_found`, never
  `scope_mismatch` — existence is never revealed across identities (mirrors
  `tasks.get` / `state.history`). Cross-tenant *with* admin is the only path
  that may act on another identity, and it audits.
- **Concurrent erase vs in-flight read.** A session being erased while
  another caller reads a *different* session must not cross-talk; the
  concurrency test pins this. A read of the *same* session racing its
  erasure resolves to either the pre-erasure snapshot or `not_found`, never a
  partial/corrupt projection (each store delete is atomic at the record
  level).
- **§18 skill drift (binding).** `grep -rl 'sessions\.' docs/skills/` — any
  skill that enumerates the session methods or quotes the session lifecycle
  (`use-the-harbor-protocol`, `observe-with-the-console`) MUST add
  `sessions.delete` / the erasure event in the SAME PR if it quotes the
  shape; record which in the PR.
- **Full §16 brief pass** (re-read brief 05 + brief 06 + RFC §6.9 / §6.11 /
  §6.13) when dispatched.

## Glossary additions

- **session erasure** — the operator/client-initiated, identity-scoped
  deletion of a whole session and its scoped data via the `sessions.delete`
  Protocol method; distinct from SessionManager GC (which only reaps idle
  sessions on its own schedule) and from `Registry.Close` (a soft-close
  tombstone). Add to `docs/glossary.md` in the same PR.
- **erasure cascade** — the ordered, fail-loud, idempotent sequence
  `sessions.delete` runs across the three identity-scoped stores
  (Artifacts → Memory → State) plus the SessionRegistry record, emitting a
  redacted `session.erased` audit event and refusing on a running task. Add
  to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits — N/A)
- [ ] `make protocol-ts-gen-check` passes (manifest regenerated; TS
      `SessionsDelete*` in lockstep)
- [ ] `make protocol-docs-gen-check` passes (D-209: `methods.md` /
      `errors.md` / `types.md` regenerated and committed)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **Multi-isolation paths changed: cross-session isolation test passes**
      (concurrent distinct-session erasures + erase-A-while-read-B) — the
      erasure surface touches identity-scoped storage, so this is binding.
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `sessions.delete`
      dispatches against a single shared sessions `Service` + shared stores,
      distinct sessions, no race / bleed / cross-cancel / leak (`-race`).
- [ ] **Integration test exists** — real State/Memory/Artifact drivers +
      real wire transport end-to-end, identity propagation, ≥1 failure mode
      (running-task 409 + cross-tenant 403), `-race`
      (`test/integration/phase130_session_erasure_test.go`).
- [ ] If new vocabulary: glossary updated (`session erasure`,
      `erasure cascade`)
- [ ] If a brief finding was departed from: justified + decisions.md entry
      — N/A, no departures; D-262 records the design decisions

---

## Implementation handoff

Turnkey artifacts for the implementing agent. Operate only inside your
worktree (`pwd` first; STOP if a path resolves outside it). Run
`markdownlint-cli2` repo-wide before committing (blank lines around `---`
and `## D-NNN` headings in `docs/decisions.md`).

### (a) Master-plan `docs/plans/README.md` index row

Append (the table sorts by phase number; this row sorts after 127):

```text
|130 | Session erasure Protocol method (data-lifecycle deletion) (ship the additive identity-scoped `sessions.delete` — deletes a session and CASCADES deletion of its scoped State + Memory + Artifacts; refuses fail-loud on a RUNNING task with a distinct `session_running` (409) mirroring the GC never-reap-running invariant; own-identity vs `auth.ScopeAdmin` scope contract mirrors `sessions.list`; new `DeleteScope` StateStore primitive with conformance parity; redacted audited `session.erased` event; new `CapSessionLifecycle` capability; consumer = the real three-store cascade handler + E2E; NO ProtocolVersion bump; D-262) | internal/protocol + internal/sessions + internal/state + web/console | §5.2, §6.9, §6.11, §6.13, §7 | 08, 11, 17, 18, 58, 60, 61 | 85% | Pending (V1.7) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — note the blank lines)

Append at end of file:

```markdown

---

## D-262 — Session erasure: an identity-scoped sessions.delete that cascades State/Memory/Artifact deletion, refuses fail-loud on a running task, and audits content-free

**Date:** 2026-06-25

**Status:** Accepted (planning)

**Context.** The Harbor Protocol exposes read surfaces for sessions
(`sessions.list` / `sessions.inspect` / `state.history`) and a single-object
`artifacts.delete`, but NO operator- or client-initiated way to erase a
whole session and its scoped data. The SessionManager GC (RFC §6.9) only
reaps *idle* sessions on its own schedule and never reaps a session with a
RUNNING task. A generic Protocol client (a third-party Console, an IDE/TUI
client, an SDK consumer) therefore cannot satisfy a data-lifecycle /
right-to-erasure request — the session's State, Memory, and Artifacts (each
identity-scoped, possibly on different drivers) persist with no deletion
verb. This is a Protocol-surface gap, not a Console gap: erasure must be a
canonical, identity-mandatory, fail-loud method any client invokes through
the same wire contract.

**Decision.**

1. **A new additive method `sessions.delete`** (single-sourced in
   `internal/protocol/methods`, wire route `POST /v1/sessions/delete`,
   reusing the existing `POST /v1/sessions/` handler with a `delete`
   branch). Identity-mandatory. Request body carries only the identity
   scope (no elevation knob); response carries non-sensitive deletion
   telemetry only (`session_id`, `deleted`, `state_records_deleted`,
   `artifacts_deleted`, `memory_purged`) — never erased content.

2. **A fail-loud running-task refusal.** `sessions.delete` consults the same
   `RunningProbe` seam the GC uses; a session with a RUNNING task is refused
   with a distinct new error code `session_running` (HTTP 409) and NO store
   is touched — mirroring the RFC §6.9 GC never-reap-running invariant. The
   runtime never partially erases a running session.

3. **A three-store cascade + session-record hard-delete.** A new
   `internal/sessions` cascade orchestrator runs, in order:
   refuse-if-running → `ArtifactStore.List`/`Delete` each →
   `MemoryStore.Flush` → `StateStore.DeleteScope` (a new cascade primitive
   added to the single mandatory StateStore interface that deletes every
   record matching the `(tenant, user, session)` triple regardless of
   `run_id`/`kind`, with conformance parity across in-mem / SQLite /
   Postgres) → `Registry.Erase` (hard-deletes the SessionRegistry record,
   NOT a `Closed=true` tombstone) → emit a redacted `session.erased` event.
   There is no ACID transaction across the independent stores; the cascade
   is fail-loud and idempotent — a mid-cascade error returns loudly and is
   safe to re-invoke to convergence (each per-store delete is idempotent).

4. **The same scope contract as `sessions.list`.** A non-admin caller may
   erase only their own verified `(tenant, user, session)`; a body identity
   mismatch is `identity_required`; another identity's session is
   `not_found` (existence never revealed across identities); a cross-tenant
   / other-user erasure requires the verified `auth.ScopeAdmin` claim on the
   ctx and emits `audit.admin_scope_used`.

5. **Hard-delete the data; tombstone only the fact.** Right-to-erasure
   requires the State / Memory / Artifact bytes to be actually removed, not
   flagged. The only durable trace is the redacted `session.erased` audit
   record (session id + actor + counts + timestamp, NO content) in the
   audit/compliance sink — NOT re-persisted under the erased session's own
   identity (a post-erasure `state.history` for the triple returns empty).

6. **A negotiable capability.** A new `CapSessionLifecycle`
   (`session_lifecycle`) is advertised on `runtime.info` only when an eraser
   is wired, so a client detects erasure support via capability negotiation
   rather than a 404 at call time.

7. **The consumer lands in the same phase (§13).** The cascade handler over
   the production State / Memory / Artifact drivers + a real Registry is the
   consumer; an integration test proves delete → subsequent read
   `not_found`, cross-store erasure, cross-tenant 403, running-task 409
   end-to-end. A Console "delete chat" UI is a follow-on consumer (the typed
   TS client gains the request/response types regardless, per the D-223
   lockstep gate).

8. **No version bump.** Method + error code + capability + wire types + the
   StateStore method are all additive — a Minor-class change in the
   `internal/protocol/types/version.go` taxonomy. RFC §5.3's rule that
   bumping `ProtocolVersion` is an RFC change is exactly why this stays
   additive: `ProtocolVersion` holds. The generated Protocol reference
   (`docs/site/protocol/{methods,errors,types}.md`) is regenerated with
   `make protocol-docs-gen` and committed in the same PR (D-209).

**Out of scope.** Bulk/user-wide/tenant-wide erasure (one session per
call); purging the USER-scope agent-config revision store (keyed by
`(tenant, user)`, survives a session, a separate config-plane concern); a
new transport or scope.

**§4.3 deviations.** None — additive surface on existing seams.

**Cross-references.** D-223 (the TS lockstep gate the new types regen
into), D-209 (the generated Protocol-docs regen gate), D-061 (Console is a
Protocol client), D-025 (concurrent-reuse contract for the shared erasure
Service). RFC §5.2, §6.9, §6.11, §6.13, §7. brief 05, brief 06. Plan:
`docs/plans/phase-130-session-erasure-method.md`.
```

### (c) `scripts/smoke/phase-130.sh` assertions to add

Use `scripts/smoke/common.sh` helpers; no new curl wrappers
(`assert_post_status` for the live POST already exists in `common.sh`).
Each maps 1:1 to an acceptance criterion.

```bash
# Static: single-source registration of the method / code / capability.
assert_grep_present 'MethodSessionsDelete' "internal/protocol/methods/methods.go" \
  "phase 130: sessions.delete method registered"
assert_grep_present '"sessions.delete"' "internal/protocol/methods/methods.go" \
  "phase 130: sessions.delete method string single-sourced"
assert_grep_present 'CodeSessionRunning' "internal/protocol/errors/errors.go" \
  "phase 130: session_running error code registered"
assert_grep_present '"session_running"' "internal/protocol/errors/errors.go" \
  "phase 130: session_running code string single-sourced"
assert_grep_present 'CapSessionLifecycle' "internal/protocol/types/version.go" \
  "phase 130: session_lifecycle capability registered"

# Static: the two new wire types exist (Go) and are mirrored (TS).
assert_grep_present 'SessionsDeleteRequest' "internal/protocol/types/sessions.go" \
  "phase 130: SessionsDeleteRequest Go wire type present"
assert_grep_present 'SessionsDeleteResponse' "internal/protocol/types/sessions.go" \
  "phase 130: SessionsDeleteResponse Go wire type present"
assert_grep_present 'SessionsDeleteResponse' "web/console/src/lib/sessions/types.ts" \
  "phase 130: TS SessionsDeleteResponse interface mirrored"

# Static: StateStore cascade primitive on the interface.
assert_grep_present 'DeleteScope' "internal/state/state.go" \
  "phase 130: StateStore.DeleteScope cascade primitive on the interface"

# Build/test gates: manifest lockstep + generated-docs gate + the tests.
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 130: make protocol-ts-gen-check passes (manifest + TS types in lockstep)"
else
  fail "phase 130: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 130: make protocol-docs-gen-check passes (methods/errors/types regenerated, D-209)"
else
  fail "phase 130: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test -race ./internal/sessions/... ./internal/state/... >/dev/null 2>&1; then
  ok "phase 130: sessions + state cascade tests pass under -race"
else
  fail "phase 130: sessions/state tests failed (go test -race ./internal/sessions/... ./internal/state/...)"
fi
if go test -race -run TestE2E_Phase130 ./test/integration/... >/dev/null 2>&1; then
  ok "phase 130: session-erasure E2E passes under -race (cascade + scope matrix + running-task refusal)"
else
  fail "phase 130: session-erasure E2E failed (go test -race -run TestE2E_Phase130 ./test/integration/...)"
fi

# Live (skips per 404/405/501): open a throwaway dev session, write a marker,
# delete it, then assert a follow-up inspect 404. Build the {"identity":{...}}
# body from the dev triple. assert_post_status SKIPs on 404/405/501.
#   body='{"identity":{"tenant":"...","user":"...","session":"..."}}'
#   assert_post_status 200 "$(api_url /v1/sessions/delete)" "$body" \
#     "phase 130: live sessions.delete answers 200 for own session"
#   then POST sessions/inspect for the same id and assert 404 not_found.
#   Also: a cross-tenant delete body -> 403; a session with a running task -> 409.
#   SKIP when the route 404s or no dev token is available (same posture as
#   the sessions smoke / phase-72f.sh).
```

### (d) Master-plan per-phase detail-block stub

Add under the detail section of `docs/plans/README.md` (house format —
mirror the 127/125 blocks):

```markdown
### Phase 130 — Session erasure Protocol method (data-lifecycle deletion)

- **Subsystem:** internal/protocol (additive method + error code +
  capability + wire types) + internal/sessions (Registry.Erase + the
  three-store cascade) + internal/state (DeleteScope cascade primitive,
  conformance parity) + web/console (hand-mirrored TS types; Console
  delete-chat UI is a follow-on consumer).
- **RFC:** §5.2 (the erasure surface alongside `artifacts.delete`), §6.9
  (the GC never-reap-running invariant mirrored as a fail-loud refusal +
  the hard-deleted session record), §6.11 (the StateStore cascade
  primitive), §6.13 (the redacted `session.erased` event), §7 (the audit
  trail). §5.3 cited in prose only for "bumping the version is an RFC
  change" (this stays additive).
- **Deps:** 08 (sessions + RunningProbe), 11 (StateStore + conformance), 17
  (memory Flush), 18 (artifacts List/Delete), 58 (single-source +
  CanonicalWireTypes), 60 (wire transport), 61 (auth scopes).
- **What it delivers:** the additive identity-scoped `sessions.delete`
  method that deletes a session and cascades deletion of its scoped State +
  Memory + Artifacts; a fail-loud `session_running` (409) refusal on a
  RUNNING task; the own-identity vs `auth.ScopeAdmin` scope contract
  mirroring `sessions.list`; a new `StateStore.DeleteScope` primitive with
  in-mem/SQLite/Postgres conformance parity; a redacted audited
  `session.erased` event (content-free, not re-persisted under the erased
  identity); a negotiable `CapSessionLifecycle` capability. Consumer = the
  real three-store cascade handler exercised by an E2E test (delete →
  inspect-404 + cross-store erasure + cross-tenant 403 + running-task 409).
  NO ProtocolVersion bump. Generated Protocol docs regenerated (D-209).
- **Decision:** D-262.
- **Status:** Pending (V1.7).
```
