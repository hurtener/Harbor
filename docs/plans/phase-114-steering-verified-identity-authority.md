# Phase 114 — steering verified-identity authority

## Summary

The Protocol steering-control surface derives caller authority (privilege
tier + tenant) from the **verified identity on the request context**, not
from the request body. Trusting the body-supplied `IdentityScope.Scope`
string let any caller assert `admin` against the steering control plane —
a privilege escalation; and setting the event's `CallerTenant` from the
body's (target-run) tenant meant the cross-tenant-requires-admin gate
could never fire. This phase closes both: a steering control now fails
closed when no verified identity is present, and authority is computed by
comparing the verified caller against the target run.

## RFC anchor

- RFC §6.3 — Steering and the unified pause/resume primitive (the
  per-event scope mapping: who may submit which control; cross-tenant
  steering requires admin).
- RFC §5.5 — Authentication (the Protocol rejects any request without an
  identity scope; identity flows from the verified token, not the body).

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- brief 02 §511–515 ("Steering authn/authz model", open question Q-3):
  *"Who can submit which control events? … Should the protocol require a
  scope/role per event type …"* — resolved by RFC §6.3's per-event scope
  table. This phase makes the **edge** honour that table by deriving the
  caller's tier from the verified principal, instead of accepting a
  body-claimed tier the per-event check then rubber-stamps.
- brief 02 §206: *"scoping triple already bound; the planner cannot reach
  to other scopes."* — the same containment must hold at the control
  edge: a non-admin caller may steer only runs within its own
  `(tenant, user)`.

## Findings I'm departing from (if any)

None.

## Goals

- Caller authority for every steering control is read from the verified
  ctx identity + JWT scope claims, never from the request body.
- A steering control with no verified caller identity on ctx fails closed
  with `CodeIdentityRequired` — no fallback to the body.
- The steering event's `CallerTenant` is the **verified** caller tenant,
  so `steering.CheckScope`'s cross-tenant-requires-admin gate is live.
- A non-admin caller can steer only runs it owns (`(tenant, user)`
  match → `owner_user`); admin can steer any run, cross-tenant included.

## Non-goals

- Minting non-admin (session-scoped / owner-scoped) tokens — the dev
  bootstrap still mints admin only. The lesser-privileged token contract
  that turns this fix into a live, exploitable-without-it boundary is a
  follow-on phase. This phase is the prerequisite hardening.
- Production JWKS verification / a `harbor serve` production auth path
  (separate follow-on).
- Any change to the `ScopeSessionUser` tier semantics beyond declining to
  derive it from an ambiguous bare-session-id match (see Risks).

## Acceptance criteria

- [ ] `dispatchControl` reads the caller via `identity.From(ctx)` and
      fails closed with `CodeIdentityRequired` when absent.
- [ ] `deriveSteeringScope(ctx, caller, run)` maps admin→`ScopeAdmin`,
      owning `(tenant,user)`→`ScopeOwnerUser`, otherwise no authority.
- [ ] The body `IdentityScope.Scope` field is never read for authority.
- [ ] The steering `ControlEvent.CallerTenant` is the verified caller
      tenant.
- [ ] A non-admin caller submitting an admin-only control while claiming
      `Scope:"admin"` in the body is rejected `CodeScopeMismatch`.
- [ ] A cross-tenant control by a non-admin is rejected; by an admin it
      is accepted carrying the verified caller tenant.
- [ ] All prior steering-control tests pass once migrated to ctx-borne
      identity; the conformance suite stays green.

## Files added or changed

- `internal/protocol/control.go` — verified-identity derivation in
  `dispatchControl`; new `deriveSteeringScope` helper.
- `internal/protocol/control_test.go` — migrated round-trip /
  admin-satisfies / per-run-isolation tests + new escalation,
  no-verified-identity, and cross-tenant (non-admin reject / admin allow)
  tests.
- `internal/protocol/protocol_test.go` — shared `authCtx` helper;
  migrated control failure-mode tests; obsolete body-scope tests removed.
- `internal/protocol/concurrent_test.go` — concurrent-reuse control call
  authenticated via ctx.
- `internal/protocol/conformance/conformance.go` — `callerCtx` helper;
  in-process control scenarios authenticate via ctx.
- `internal/protocol/transports/control/control_test.go`,
  `test/integration/wave9_test.go` — in-process control dispatches
  authenticate via ctx.
- `scripts/smoke/phase-114.sh` — control-plane regression assertions.

## Public API surface

No new exported surface. `deriveSteeringScope` is unexported. The wire
`types.IdentityScope.Scope` field is retained for compatibility but is no
longer consulted for steering authority (documented as ignored).

## Test plan

- **Unit:** escalation-rejection (body `Scope:"admin"` ignored),
  no-verified-identity fail-closed, cross-tenant non-admin reject,
  cross-tenant admin allow, owner-tier round-trip across all nine
  controls, admin-satisfies-lower-scopes.
- **Integration:** `test/integration/wave9_test.go` drives inject/approve
  /prioritize/cancel through the assembled surface with ctx-borne
  identity (real drivers on the seam, identity propagation, ≥1 failure
  mode, `-race`).
- **Conformance:** `internal/protocol/conformance` MethodMatrix +
  ErrorCodeMatrix stay green (in-process scenarios authenticate via ctx;
  the wire scenarios already sign tokens).
- **Concurrency / leak:** `TestConcurrentReuse_ControlSurface` —
  N=150 concurrent ctx-authenticated controls against one shared surface
  under `-race`, asserting no context bleed (each event carries its own
  identity + verified `CallerTenant`) and baseline goroutine restoration.

## Smoke script additions

- `scripts/smoke/phase-114.sh` asserts the control surface still accepts
  a well-formed steering control from the admin dev token (regression
  that authority derivation did not break the golden path), and that a
  control method is reachable (skips on 404/405/501 per the convention).
  The negative escalation assertion needs a non-admin token, which the
  dev bootstrap does not yet mint — documented in the script as deferred
  to the lesser-privileged-token follow-on.

## Coverage target

- `internal/protocol`: ≥ 85% (maintain; the surface is already covered,
  this adds branches).

## Dependencies

- Phase 52 (steering inbox + `CheckScope`), Phase 55/56 (protocol auth
  scopes + `identity.From` on ctx). All shipped.

## Risks / open questions

- **`ScopeSessionUser` is not derived here.** Granting it off a bare
  session-id match is unsafe (session ids are not globally unique across
  users; a same-tenant collision would confer authority over another
  user's run). Owner-tier covers every same-user control by rank. The
  session-scoped tier becomes safe to grant only once a non-admin token
  carries a verified session principal — the follow-on token-contract
  phase owns that.
- **Latent-until-consumed.** With admin-only dev tokens there is no
  lesser caller to exploit the old behaviour today; this fix must precede
  any phase that mints a non-admin token (RFC §6.3 + §13 fail-closed).

## Glossary additions

None — `owner_user` / `session_user` / `admin` steering scopes already in
`docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes (no AGENTS/CLAUDE edit)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse test passes (`TestConcurrentReuse_ControlSurface`, N=150, `-race`)
- [x] Integration test exists, real drivers, identity propagation, ≥1 failure mode, `-race`
- [x] If new vocabulary: glossary updated — N/A
- [x] If a brief finding was departed from: justified — N/A (no departure)
