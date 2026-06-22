# Phase 92o — Run-start connection reconciliation (closes #375 gap 2)

> Part of the parked, RFC-anchored wave `docs/plans/wave-mcp-oauth-decomposition.md`
> (§3 "92o", §4 staging, §7 risks). Planning only — no code lands until the wave is
> picked up. Decision **D-245** is reserved here and logged on ship (§17.7 step 3).

## Summary

The run-start agent-config projection (`internal/runtime/agentcfg/projection`)
gains a `ReconcileConnections` helper that attaches every connection the agent's
active revision DECLARES but the live catalog does not yet carry — so a server
authorized in a prior process (or re-declared by a rollback) comes online on the
next run. The helper is called by BOTH run-loop drivers (the production driver and
the `harbortest/devstack` twin — D-094, so they cannot drift). This closes #375
gap 2: persisted connection descriptors finally have a read-path. Detach-on-rollback
is explicitly out of scope (D-240 decision 5 / D-245) — pausing is the revoke path.

## RFC anchor

- RFC §6.16 — Agent Registry (the active revision's connection descriptors are part
  of the agent's durable config; reconciliation reads them at run start, keyed by
  the agent's registration identity).
- RFC §6.4 — Tool catalog and transports (the MCP southbound attach the reconciler
  re-drives for a declared-but-absent server).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth from bifrost):** the agent-bound token outlives the process
  that minted it (the sealed `TokenStore` persists it keyed by `agent_id`, D-059).
  Reconciliation leans on exactly this: a server authorized in a prior process is
  re-attached at the next run by reading the SAME agent-bound token — the reconciler
  needs no fresh consent.
- **brief 14 (MCP client/host compliance):** attaching a server is Connect →
  `initialize` → Discover → register; a half-attached server must never be
  registered. The reconciler delegates the whole sequence to the injected
  `ConnectionAttacher` (the same seam 92f/92m use) and fails loud on any step — it
  does not re-implement the handshake.

## Findings I'm departing from (if any)

None.

## Goals

- Add `ReconcileConnections` to the shared run-start projection: it reads the agent's
  active revision connection descriptors and, for each DECLARED-but-ABSENT source
  (not already live in the catalog/registry), attaches it via an injected attacher
  reading the agent-bound token.
- Wire it into BOTH run-loop drivers (production + devstack twin) so the read-path
  cannot drift between the two binaries (D-094 / §17.6).
- Idempotent: an already-attached source is skipped (no re-attach, no churn).
- Serialised per agent so two concurrent run-starts cannot double-attach a source.
- Driver-agnostic projection: the package takes an attacher interface + a known-source
  predicate as parameters/closures and MUST NOT import the concrete MCP driver
  (the §4.4 boundary the existing `projection` package already respects — it has ZERO
  connection references today).
- Fail loud on a registry read error (no silent fall-through to "attach nothing"),
  identity-scoped by the run's triple, fresh-per-run (the concurrent-reuse contract).

## Non-goals

- **Detach-on-rollback is NOT done (D-240 decision 5 / D-245).** A rollback past an
  add does NOT tear down the live MCP transport. Revocation stays the job of the
  pause/resume tool-exposure primitive (D-237: "pause/resume covers the disable
  need") — **pausing is the revoke path.** Tearing down a warm transport on a
  rollback is inconsistent with the next-turn-projection model and carries draining/
  ordering risk; the removal direction is a recorded, deliberate deferral, not an
  oversight. This is stated loud so a future reader does not mistake the asymmetry
  for a bug.
- The resume-completes-attach in-memory bridge (92n) — the durable run-start backstop
  here is the complement, not a replacement, of that bridge.
- Spec-faithful OAuth discovery (92p) and the Console advisory (92q).
- Any new OAuth machinery — the agent-bound token is read through the same attacher
  seam; this phase mints no credential.

## Acceptance criteria

- [ ] `ReconcileConnections` attaches a declared-but-absent source at run start,
      reading the agent-bound token (identity-scoped test: the attach runs under the
      run's verified triple; a foreign triple's descriptors never leak in).
- [ ] An already-attached source is SKIPPED — the reconciler is idempotent across
      repeated run-starts (no second `Attach` call for a live source).
- [ ] Per-agent serialisation: under concurrent run-starts for the same agent, a
      source is attached at most once (no double-attach).
- [ ] BOTH run-loop drivers call the shared helper (a twin-parity test or an explicit
      cross-driver assertion — neither binary inlines its own copy, D-094 / §17.6).
- [ ] A registry read error fails the run loudly (the error is returned, never
      swallowed into "attach nothing" — §13 no silent degradation).
- [ ] Detach-on-rollback is explicitly NOT implemented and is documented as a
      deliberate deferral (godoc + this plan's Non-goals), with the operator note that
      pausing is the revoke path.
- [ ] The `projection` package still imports NO concrete MCP driver (the attacher +
      known-source predicate are injected; the §4.4 boundary holds).

## Files added or changed

- `internal/runtime/agentcfg/projection/projection.go` — add `ReconcileConnections`
  (+ its injected `ConnectionAttacher` interface or a reuse of the existing seam's
  shape, and a per-agent serialisation guard documented as internally-synchronised).
- `internal/runtime/agentcfg/projection/projection_test.go` — declared-but-absent
  attach, idempotent skip, per-agent serialisation under concurrency, read-error
  fails loud, identity isolation.
- `cmd/harbor/cmd_dev_runloop.go` — add the attacher field + call
  `projection.ReconcileConnections` at run start (the driver lacks an attacher field
  today; this wires one in, reusing the 92f/92m `ConnectionAttacher` concrete).
- `harbortest/devstack/devstack.go` — the D-094 twin: same call, same attacher
  concrete, so the two drivers cannot drift.
- `docs/plans/phase-92o-connection-reconciliation.md` (this file).
- `scripts/smoke/phase-92o.sh`.

## Public API surface

```go
// ReconcileConnections attaches every connection the agent's active revision
// declares but the live catalog does not yet carry, reading the agent-bound
// token through the injected attacher. Idempotent (already-live sources are
// skipped) and serialised per agent. A registry read error is returned so the
// caller fails the run loudly. The package stays driver-agnostic: the attacher
// and the known-source predicate are injected; it imports no concrete driver.
// Removal of a no-longer-declared source is intentionally NOT done here —
// pausing is the revoke path.
//
// func ReconcileConnections(ctx, reg, agentID, id, attacher, isKnown) error
```

## Test plan

- **Unit:** declared-but-absent source attaches (attacher invoked once with the
  descriptor); already-known source skipped (attacher not invoked); a registry read
  error returns and is not swallowed; an empty/absent connection section is a no-op;
  the known-source predicate gates correctly.
- **Integration:** an in-package run-start test wires a real `agentcfg.Registry` (real
  driver) + a fake attacher recording each attach, asserts the run's triple flows into
  the attacher's request, and asserts a foreign triple's descriptors never leak across.
  A forced registry read error is the ≥1 failure mode; runs under `-race`.
- **Conformance:** reuses the `agentcfg` registry conformance (no new driver shape).
- **Concurrency / leak:** N≥100 concurrent `ReconcileConnections` for the same agent
  against one shared registry + counting attacher under `-race` — assert a source is
  attached at most once (per-agent serialisation), no data races, no context bleed
  across runs, no goroutine leak (baseline-restored after all runs return). This is the
  concurrent-reuse-contract test the phase ships (D-025).

## Smoke script additions

- `scripts/smoke/phase-92o.sh` (`# PREFLIGHT_REQUIRES: unit-tests`): the run-start
  projection is exercised by Go tests, not a live endpoint. Skeleton keeps a single
  `skip "phase 92o: ..."` until the phase implements `ReconcileConnections`; on ship it
  runs `go test` against `internal/runtime/agentcfg/projection` asserting the
  reconciliation unit + concurrency tests pass.

## Coverage target

- `internal/runtime/agentcfg/projection`: 85%

## Dependencies

- 92m — `add_mcp_connection` OAuth config + `InitiateFlow` parking (it lands the
  persisted connection descriptors + the agent-bound token this phase reads back; the
  `ConnectionAttacher` seam the reconciler reuses is wired by the add path).

## Risks / open questions

- **Restart-survival is the whole point.** The in-memory resume bridge (92n) cannot
  re-drive a pause whose process died; this run-start reconcile is the durable
  backstop (the descriptor revision + the agent-bound token both survive). The two
  must not double-attach when both fire — the per-agent serialisation guard + the
  known-source predicate (skip already-live) are the joint defence; covered by the
  concurrency test.
- **Asymmetry by design.** Attach-on-declare without detach-on-remove is a deliberate
  asymmetry (D-245). The risk is a future reader filing it as a bug; mitigated by the
  loud godoc + Non-goal + the operator note that pausing is the revoke path.
- **Attacher-seam reuse.** The reconciler must reuse the same `ConnectionAttacher`
  the add path uses — a second seam would re-introduce the drift D-094 closes. The
  twin-parity acceptance criterion pins this.

## Glossary additions

- **run-start connection reconciliation** — the shared run-start projection step that
  attaches every connection the agent's active revision declares but the live catalog
  does not yet carry (reading the agent-bound token), so a server authorized in a
  prior process comes online on the next run. Idempotent, per-agent serialised,
  driver-agnostic. The attach-only complement of the in-memory resume bridge;
  detach-on-remove is deferred (pausing is the revoke path).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes — N≥100 concurrent `ReconcileConnections`
      against one shared registry under `-race`, asserting at-most-once attach per
      source, no races, no context bleed, no goroutine leaks.** See §5 + §11 + D-025.
- [ ] **Integration test exists (in-package run-start test), real registry driver +
      fake attacher on the seam, identity propagation asserted, ≥1 failure mode
      (registry read error), `-race`.** See §17.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
