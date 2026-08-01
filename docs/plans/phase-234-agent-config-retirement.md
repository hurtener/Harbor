# Phase 234 — Agent-config retirement

## Summary

Add a durable CAS retirement verb that makes an agent terminally unresolvable for new runs while preserving immutable revision history. The tombstone retains the exact pre-retirement hash, operation ID, cleanup manifest, and progress so a same-operation retry resumes after any interruption.

## RFC anchor

- RFC §5.5.
- RFC §6.11.
- RFC §6.16.
- RFC §6.13.

## Briefs informing this phase

- brief 05
- brief 06
- brief 07
- brief 09

## Brief findings incorporated

- brief 05 §4: terminal state and cleanup progress persist through the mandatory StateStore seam on every driver.
- brief 06 §4: lifecycle facts are canonical, identity-scoped, and redacted.
- brief 07 §2: retirement is one runtime lifecycle mechanism, not separate Protocol and run-loop policies.
- brief 09 §7: agent-owned OAuth/tool resources require explicit owner-scoped lifecycle handling.

## Findings I'm departing from (if any)

- brief 09's early agent-as-isolation proposal is superseded by RFC §6.16 and D-059; cleanup enumerates identity-scoped records and treats agent ID as ownership metadata.

## Goals

- Install a terminal lifecycle tombstone by exact StateStore compare-and-swap.
- Freeze every agent- and user-tier durable mutation after retirement and make every session overlay inaccessible, compensating a local write when retirement wins concurrently.
- Deny explicit and defaulted new-run selection while preserving already-acquired run snapshots.
- Resume a fixed, idempotent, owner-scoped cleanup manifest for the same operation only.

## Non-goals

- No unretire, agent-ID reuse, broad credential sweep, or deletion of immutable revision history.
- No change to `agents.deregister`; fleet registration and config lifecycle stay orthogonal.
- No teardown of boot/global resources or live-only resources lacking durable agent ownership.
- No `agent_reach` requirement on the admin control-plane retirement verb; reach remains data-plane authority.

## Acceptance criteria

- [ ] `agent_config.retire` requires admin scope, exact identity, non-empty bounded operation ID, and expected active content hash (or `ExpectNoActiveRevision`).
- [ ] The agent active slot becomes a backward-compatible lifecycle envelope; a tombstone contains prior revision ID/hash, operation/time, fixed cleanup manifest, and per-step progress.
- [ ] Initial retirement and every progress update use `SaveIf`; a stale writer, rollback, or user-tier writer cannot resurrect or mutate the tombstoned agent.
- [ ] Each process-local session mutator reads the lifecycle before and after its local write, compensates the exact write when retirement wins between them, and every later overlay read/write refuses the tombstone; an already-completed remote-process overlay is inaccessible rather than falsely claimed erased.
- [ ] Same-operation retries resume; different operation IDs return a typed conflict; landed-but-unacknowledged writes converge by exact reread.
- [ ] `Active` and every mutation return `ErrAgentRetired`; historical Get/List/Diff remain available to authorized admin/audit callers.
- [ ] Both explicit and omitted/default `control.start` fail before new work; a run whose immutable start snapshot predates the tombstone may finish unchanged.
- [ ] Cleanup detaches only manifest-listed runtime-added MCP connections and uninstalls only durably owner-scoped providers; it removes local session overlays through identity-scoped enumeration, retains user and agent revision history, and never touches boot/global, remote-process memory, or unattributable credentials.
- [ ] Tombstone and progress survive restart on SQLite/Postgres; fault injection after every side effect/progress boundary converges on same-operation retry.
- [ ] `agents.deregister` neither installs nor deletes a tombstone, and retirement does not remove the fleet record.

## Files added or changed

- `internal/agentcfg/` and `internal/agentcfg/drivers/statestore/`
- `internal/runtime/agentcfg/` and `internal/runtime/serve/`
- `internal/protocol/{types,methods,errors,singlesource}/`
- `internal/protocol/transports/stream/agentconfig_handler.go`
- `web/console/src/lib/protocol/` and generated wire manifest
- `test/integration/agent_retirement_test.go`
- `scripts/smoke/phase-234.sh`
- generated Protocol docs, operator skills, RFC, decisions, glossary, and release notes

## Public API surface

- `agent_config.retire` request/response wire types and typed retirement error codes.
- `agentcfg.Registry.Retire`, `RetirementStatus`, `ErrAgentRetired`, and `ErrRetirementConflict`.
- Owner-scoped idempotent cleanup interface injected by runtime assembly.

## Test plan

- **Unit:** lifecycle decoding, CAS install, operation replay/conflict, freeze matrix, resolver/default behavior, manifest construction, and typed Protocol mapping.
- **Integration:** real StateStore registry + runtime projection + mux, restart durability, explicit/default start refusal, history preservation, and `agents.deregister` independence.
- **Conformance:** all agentcfg drivers implement terminal lifecycle and same-operation replay; the 17 spine writes plus five session writes are held in a closed refusal census.
- **Concurrency / leak:** stale writer/rollback/user/session-overlay writer versus retirement, including after-write/before-recheck compensation; two retirees; N≥100 reads/retries; cancellation, restart, and goroutine baseline under `-race`.

## Smoke script additions

- Retire the dev agent, assert explicit/default start refusal, immutable history, retry status, operation conflict, and separate fleet deregistration behavior.
- Run every cleanup fault boundary and require at least one real live-server assertion.

## Coverage target

- `internal/agentcfg` and statestore driver: 90%; runtime agentcfg/protocol/serve and Protocol handler packages: 85%; generated lockstep gates stay green.

## Dependencies

- 232, 233.

## Risks / open questions

- In-flight projection preservation requires cleanup to avoid destructive process-global teardown until no pre-retirement snapshot can reference it; the cleaner records a deferred step rather than violating that boundary.
- Unattributable live provider state is reported as retained, not silently claimed cleaned; a future ownership migration may add a manifest class.

## Glossary additions

- Agent-config retirement.
- Retirement tombstone.
- Retirement replay identity.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session and cross-tenant retirement isolation tests pass
- [ ] Reusable registry/cleaner N≥100 concurrent-reuse test passes with no race, bleed, cancellation cross-talk, or leak
- [ ] Real-driver integration covers identity, restart, conflict, and cleanup failure
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
