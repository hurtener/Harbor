# Phase 153 — Admin-widened fleet enumeration for `tasks.list` + `agents.list`

## Summary

`tasks.list` and `agents.list` are strictly session-scoped today: the projectors read only the caller's own `(tenant, user, session)` triple, so a fleet observer (a coordinator control plane rendering a fleet-wide Tasks board / Agents catalog) gets an empty list from any synthetic observer session — unlike `sessions.list`, whose projector already widens to `(tenant, user)` and, under the verified admin claim, to named tenants. This phase completes the anticipated shape (`registry_projector.go`'s own godoc reserves "a future cross-runtime aggregating projector slots in behind the Projector interface"): admin-scope-widened enumeration for tasks and agents behind new aggregating projectors, gated on the SAME `auth.ScopeAdmin` claim, audited via the SAME `audit.admin_scope_used` event, mirroring the sessions precedent exactly. Cross-runtime federation stays coordinator-side, as with sessions — Harbor ships the per-runtime read.

## RFC anchor

- RFC §6.8
- RFC §6.16
- RFC §5.2

## Briefs informing this phase

- brief 05
- brief 11
- brief 06

## Brief findings incorporated

- brief 05 §6: task observability reads must stay projections over the registry — never a shadow store; the fleet read is a WIDER projection over the same registry, not a second bookkeeping surface.
- brief 11 §2: the Console fleet view is an elevated cross-session observer by design — CLAUDE.md §6 rule 5 sanctions it only behind an explicit elevated scope claim with audit, which is exactly the sessions `adminScoped` + `audit.admin_scope_used` shape this phase reuses.
- brief 06 §5: every privileged read leaves an audit trace; a fleet enumeration without an emitted admin-scope event is an unobservable privilege use.

## Findings I'm departing from (if any)

- None.

## Goals

- An admin-scoped caller can enumerate tasks tenant-wide (all users, all sessions of named tenants) via `tasks.list`, and agents tenant-wide via `agents.list`, on one runtime.
- Non-admin behavior is byte-compatible with today: own-triple scope, and a widening request without the verified admin claim fails LOUD with the existing scope-mismatch error — never a silent narrowing to own scope.
- Every widened call emits `audit.admin_scope_used` (both subsystems already have the emit helper pattern).
- Rows in widened responses carry their full identity attribution (tenant/user/session per row) so a coordinator can attribute per-source, mirroring what `sessions.list` rows expose.

## Non-goals

- No new scope vocabulary (no "fleet scope" claim — `auth.ScopeAdmin` is the one elevated claim, per the sessions precedent).
- No cross-RUNTIME federation inside Harbor — that is the coordinator's job over per-runtime reads (same division as sessions/events today).
- No change to `tasks.get` / `agents.get` detail methods' scoping in this phase beyond what widened enumeration strictly requires (a widened LIST names rows; detail reads stay caller-scoped unless the plan-time investigation shows the Console fleet drill-in needs the admin leg too — if so, same gate + audit, recorded in the PR).
- No pagination redesign — the existing cursor pagination applies to the widened set unchanged.

## Acceptance criteria

- [x] `tasks.TaskRegistry` (the seam shared by the `inprocess` and `durable` drivers via `internal/tasks/engine`) gains an explicit tenant-scoped enumeration — a separate method taking an explicit tenant scope argument, NOT an optional/blank session on the existing `List` (identity stays mandatory on the session-scoped path; no identity-downgrading knob, CLAUDE.md §13). — `ListTenant(ctx, tenantID, f)` on `TaskRegistry`, shared impl in `engine.Engine`.
- [x] The driver conformance suite covers the tenant-scoped read on all shipped task drivers with parity. — `ListTenant_*` subtests in `conformancetest`, run against both inprocess + durable.
- [x] A tasks aggregating projector implements the existing `Projector` interface's widened read; `Service.List` routes to it ONLY when the request names widened scope AND `adminScoped` is true; a widened request without the claim → the existing `ErrScopeMismatch`, loud. — `aggregating_projector.go`, routed on `filter.tenant_ids`.
- [x] `agents.list` gains the same shape: an aggregating projector over the Agent Registry enumerating agents across sessions of named tenants under the admin claim; same loud gate, same audit emit. — `registry.ListTenant` (StateStore maintenance-scan) + `ListTenantAgents` + new `ErrScopeMismatch`.
- [x] Wire changes are additive (widening fields on the two list requests + per-row identity attribution where missing); full D-223 TS lockstep (manifest regen + typed-client mirror) and D-209 protocol-docs regen in the same PR. — `TaskFilter.TenantIDs`, `AgentFilter.TenantIDs`, `Agent.Identity`; manifest + `types.md` regenerated.
- [x] `audit.admin_scope_used` emitted on every widened call, tagged with the method name and tenant count. — tasks `emitAdminAudit` + new agents `emitAdminAudit`.
- [x] Cross-session isolation proof (§6 rule 10): an integration test runs N≥10 concurrent sessions across ≥2 tenants and asserts (a) non-admin callers see exactly their own triple's rows, (b) an admin scoped to tenant A never receives tenant B rows, (c) no cross-talk under `-race`. — `test/integration/fleet_enumeration_test.go` (2×2×2 matrix, both task drivers, N=32 concurrent stress).
- [x] `docs/skills/` surfaces mentioning `tasks.list` / `agents.list` (grep `metadata.surface: protocol|tasks`) updated in the same PR (§18). — `use-the-harbor-protocol/SKILL.md` gains a fleet-enumeration note.
- [x] `scripts/smoke/phase-153.sh` shows OK ≥ 2, FAIL = 0; prior smokes pass.

## Files added or changed

- `internal/tasks/tasks.go` (or the registry iface home) — tenant-scoped enumeration on the seam.
- `internal/tasks/engine/engine.go` — the shared implementation.
- `internal/tasks/conformancetest/` — conformance coverage for both drivers.
- `internal/tasks/protocol/registry_projector.go`, `internal/tasks/protocol/list.go`, + a new aggregating projector file — the widened read + routing + gate.
- `internal/runtime/registry/protocol/service.go` + projector implementation — the agents analogue.
- `internal/protocol/types/` — additive wire fields; `internal/protocol/methods/` untouched (no new method names).
- `web/console/src/lib/protocol/*` + `wire-manifest.gen.json` — D-223 lockstep.
- `docs/site/protocol/*` — D-209 regen.
- `scripts/smoke/phase-153.sh`.

## Public API surface

- `TaskRegistry` gains one method (exact name/signature settled in implementation, shape: `ListTenant(ctx, tenantID string, f TaskFilter) ([]TaskSummary, error)`); the agents projector interface gains the widened-read analogue. Both documented as admin-gate-only call sites — the Protocol service is the ONLY production caller and it gates on the verified claim.

## Test plan

- **Unit:** gate logic (widened + non-admin → loud reject; widened + admin → routed; non-widened → byte-compatible today-path), cursor pagination over widened sets, per-row attribution.
- **Integration:** `test/integration/` — real state driver + both task drivers + real bus: seed tasks/agents across 2 tenants × 2 users × 2 sessions; admin widened read returns exactly the named tenant's rows with correct attribution + audit event observed on the bus; non-admin widened read fails loud. ≥1 failure mode = the non-admin reject + a closed-registry read.
- **Conformance:** tenant-scoped enumeration added to the task-driver conformance suite (parity across inprocess/durable).
- **Concurrency / leak:** N≥10 concurrent mixed admin/non-admin listers against live task churn under `-race`; no cross-tenant bleed, no goroutine growth after teardown.

## Smoke script additions

- live-server: `tasks.list` without widening → 200 (today-path intact); `tasks.list` naming a foreign tenant WITHOUT admin → scope-mismatch error shape asserted; with the dev admin token → 200 + `rows[].identity` attribution present. Same trio for `agents.list`. skip_if_404 throughout.

## Coverage target

- `internal/tasks/protocol`: 85%; `internal/tasks/engine`: 80%; `internal/runtime/registry/protocol`: 85%

## Dependencies

- 87 (durable task driver — conformance parity), 53a (Agent Registry), 118 (D-223 lockstep gate), 130 (sessions admin/audit precedent shape).

## Risks / open questions

- The Agent Registry's underlying store shape may make "enumerate across sessions" a scan; acceptable at V1 fleet cardinality — note it in godoc, and if the implementor finds an index is needed, that is a §4.3 deviation to record, not silent scope creep.
- Whether `tasks.get` needs the admin leg for fleet drill-in — investigate during implementation; if yes, same gate + audit + tests, recorded in the PR body.

## Glossary additions

- Aggregating projector (added to `docs/glossary.md` in this PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse: the new aggregating projectors are compiled artifacts — N≥100 concurrent widened+narrow reads against single shared instances under `-race`. — `TestListTenantTasks_ConcurrentReuse_D025` (128) + `TestListTenantAgents_ConcurrentReuse_D025` (128).
- [x] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [x] If new vocabulary: glossary updated — "Aggregating projector".
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — none departed.
