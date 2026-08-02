# Phase 234 — Agent-config retirement

## Summary

Add a durable CAS retirement verb that makes an agent terminally unresolvable for new runs while preserving immutable revision history. The tombstone retains the exact pre-retirement hash, operation ID, frozen-manifest digest/count, and bounded cleanup/scrub progress so a same-operation retry resumes after any interruption; durable session records use Phase 233a's lifecycle-and-erasure fence composition rather than process-local compensation.

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
- Freeze every agent- and user-tier durable mutation after retirement and make every session overlay/personal record inaccessible through the four-slot lifecycle, pending-erasure, tombstone-erasure, and target-record CAS composition.
- Resolve explicit/default/omitted new-run targets as pure selection, authorize
  signed reach before any tenant-local lifecycle/config lookup, and deny a
  retired target before session or task spawn while preserving already-acquired
  run snapshots.
- Resume a fixed, idempotent, owner-scoped cleanup manifest for the same
  operation only, through a deterministic paged scan after the tombstone wins.

## Non-goals

- No unretire, agent-ID reuse, broad credential sweep, or deletion of immutable revision history.
- No change to `agents.deregister`; fleet registration and config lifecycle stay orthogonal.
- No teardown of boot/global resources or live-only resources lacking durable agent ownership.
- No `agent_reach` requirement on the admin control-plane retirement verb; reach remains data-plane authority.

## Acceptance criteria

- [ ] `agent_config.retire` requires admin scope, exact identity, non-empty bounded operation ID, and expected active content hash (or `ExpectNoActiveRevision`); it never requires `agent_reach`.
- [ ] The agent active slot becomes a backward-compatible lifecycle envelope; a tombstone contains prior revision ID/hash, operation/time, fixed-manifest digest/count, and bounded discovery/cleanup/scrub progress. Pending external items are compacted to content-free digest anchors only after cleanup advancement is durable, and completion requires cleanup and scrub cursors to reach the manifest count.
- [ ] Initial retirement and every progress update use `SaveIf`; a stale writer, rollback, or user-tier writer cannot resurrect or mutate the tombstoned agent.
- [ ] Overlay and agent-owned personal-record mutation builds four exact `SaveIf` expectations (target, lifecycle, pending erasure, terminal erasure) and writes only the target; retirement or erasure wins with no local compensation fiction, every later read/write refuses the corresponding terminal state, and a discovered personal target that becomes absent converges only under that exact session's pending or terminal erasure fence.
- [ ] Uncertain retirement tombstone/progress writes reread lifecycle target
  plus operation/progress expectations; cleanup-item writes reread item target
  plus applicable session fences. Only each class's intended event/content is
  accepted, and cleanup never performs unconditional compensation or delete.
- [ ] Overlay/personal/composite reads use Phase 233a's before/after lifecycle
  and erasure-fence generation check, retrying or failing closed on a changed
  generation so retirement's postcondition includes read inaccessibility.
- [ ] Same-operation retries return and resume stored status; expected-hash mismatch, a different operation ID, or an incompatible same-slot retirement return `agent_retirement_conflict` (HTTP 409). Landed-but-unacknowledged writes converge by exact reread.
- [ ] `EffectiveAgentID(requested)` is pure explicit/default/omitted selection. The shared signed-reach gate runs before tenant-local lifecycle/config lookup; only then does the protocol-owned resolver return `active`, `unresolvable`, or `retired`. Missing reach reveals none of unknown/configured/retired state; a configured default never bypasses lifecycle lookup.
- [ ] A reach-authorized `control.start`, every active/current or mutating config/session/user/skills projection, skill-list current projection, and every session method returns `agent_retired` (HTTP 409) after tombstone. Generic `unresolvable` remains the non-oracle refusal.
- [ ] Admin `agent_config.list_revisions`/`diff` and exact immutable revision reads remain under existing admin authority. `agent_config.user.list_revisions`/`user.diff` remain only under their existing verified user scope plus signed reach. No claim is broadened.
- [ ] Both explicit and omitted/default `control.start` fail before session/task spawn; a run whose immutable start snapshot predates the tombstone may finish unchanged.
- [ ] Retirement emits canonical redacted identity-scoped `agent_config.retirement.started`, `.progress`, and `.completed` events. Payloads contain only identity, agent ID, operation-ID hash, and bounded stage/class/counters/generation. Each transition persists a pending event checkpoint before emit and exact CAS-acknowledges after; a bus/ack failure is loud, blocks later cleanup progress, and same-operation retry resumes at-least-once delivery.
- [ ] Cleanup detaches only manifest-listed runtime-added MCP connections and uninstalls only durably owner-scoped providers; a D-401 signed OAuth capability pair is one manifest item and is removed/revoked as a pair from its frozen durable pair fingerprint. Retirement/removal never gates teardown on current authority revalidation: it must close even when the envelope is expired, replayed, revoked, key-rotated, or no longer verifiable. After the lifecycle tombstone freezes the owned keyset, its fixed manifest uses `ScanKindForTenant`. New encoded personal records use a collision-safe exact per-agent prefix; raw schema-1 overlays use the common overlay prefix plus exact `LegacyOverlayKind(agentID)` equality (including an `a`/`ab` no-overmatch test). Every result is mutated only under its own identity. It retains user and agent revision history plus `ScopeUser` skills, never touches boot/global, remote-process memory, shared/unattributable legacy bodies, or credentials, and makes no unconditional `Delete`. Schema-1 overlay records remain compatibility-readable and need no separate retirement tombstone because the lifecycle fence is terminal.
- [ ] Tombstone and progress survive restart on SQLite/Postgres; fault injection after every side effect/progress boundary converges on same-operation retry.
- [ ] `agents.deregister` neither installs nor deletes a tombstone, and retirement does not remove the fleet record.

## Files added or changed

- `internal/agentcfg/`, `internal/agentcfg/drivers/statestore/`, the Phase
  233a session-record ownership resolver, and StateStore scan conformance
- `internal/runtime/agentcfg/` and `internal/runtime/serve/`
- `internal/protocol/{types,methods,errors,singlesource}/`
- `internal/protocol/transports/stream/agentconfig_handler.go`
- `web/console/src/lib/protocol/` and generated wire manifest
- `test/integration/agent_retirement_test.go`
- `scripts/smoke/phase-234.sh`
- generated Protocol docs, operator skills, RFC, decisions, glossary, and release notes

## Public API surface

- `agent_config.retire` request/response wire types, closed agent-resolution state, canonical `agent_retired` / `agent_retirement_conflict` errors, and retirement lifecycle events.
- `agentcfg.Registry.Retire`, `RetirementStatus`, `ErrAgentRetired`, and `ErrRetirementConflict`.
- Owner-scoped idempotent cleanup interface injected by runtime assembly.

## Test plan

- **Unit:** lifecycle decoding, CAS install, operation replay/conflict, freeze
  matrix, pure effective-target/reach/lifecycle order, explicit/default/omitted
  resolver behavior, manifest construction, raw legacy Kind equality (`a`/`ab`),
  class-specific uncertain-write convergence, exact historical-read matrix,
  retirement error/status mapping, and redacted event payload/checkpoint rules.
- **Integration:** real StateStore registry + Phase 233a composite resolver +
  runtime projection + mux, restart durability, unauthorized-reach hiding of
  unknown/configured/retired distinctions, explicit/default/omitted start
  refusal before spawn, history preservation, exact paged owned-record cleanup
  after tombstone, event emit/ack failure and restart replay, and
  `agents.deregister` independence.
- **Conformance:** all agentcfg drivers implement terminal lifecycle and same-operation replay; the 17 spine writes plus five session writes are held in a closed refusal census.
- **Concurrency / leak:** stale writer/rollback/user/overlay/personal-record
  writer versus retirement and erasure across two instances; four-slot
  condition failures, commit-then-error recovery, exact paged cleanup replay,
  two retirees, N≥100 reads/retries, cancellation, restart, and goroutine
  baseline under `-race`.

## Smoke script additions

- Retire the dev agent, assert explicit/default/omitted start refusal before
  task spawn, immutable history/read matrix, same-operation replay versus
  `agent_retirement_conflict`, fenced session-record refusal, exact owned
  cleanup, redacted lifecycle events, and separate fleet deregistration
  behavior.
- Assert a bearer without reach cannot distinguish unknown, configured, and
  retired targets; run every cleanup/event fault boundary and require at least
  one real live-server assertion.

## Coverage target

- `internal/agentcfg` and statestore driver: 90%; runtime agentcfg/protocol/serve and Protocol handler packages: 85%; generated lockstep gates stay green.

## Dependencies

- 232, 233, 233a, 233b.

Delivery note (2026-08-02): this branch is based on the shipped Phase 233a
four-slot session overlay/personal-state work. Phase 233b is not present on
this base, so the D-401 signed OAuth capability-pair manifest item remains
explicitly deferred; this delivery does not infer, scan, or revoke that pair.
The existing owner-scoped MCP-connection and OAuth-provider cleanup remains,
and the Phase 233a personal/legacy cleanup is integrated here. The combined
cleanup acceptance criterion above and Phase 234's master-plan status remain
open until Phase 233b lands and the pair path is integrated and verified.

The fixed cleanup manifest is operation-owned and discovered into bounded
StateStore records, one deterministic ordinal at a time. Each item includes
its exact source and successor discovery authority, so replay validates and
advances an occupied ordinal before any rescan. The lifecycle tombstone keeps
only bounded discovery, cleanup, and scrub cursors plus manifest count/digest.
A same-operation response exposes only the next pending item. After its side
effect is durably acknowledged, the item is conditionally compacted to a
content-free digest anchor and the scrub cursor advances; final completion
requires every item to be cleaned and scrubbed. This ordering closes crashes
before cleanup CAS, before compaction, and before scrub-cursor CAS without
retaining erased session or canonical resource identities after completion.

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
- [ ] Error-code/status, event, Protocol-doc, and Console lockstep gates pass
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
