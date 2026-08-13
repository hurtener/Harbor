# Phase 237 — Agent-owned skills and governed authoring (HA-55)

## Summary

Add durable agent-owned skill records and a governed authoring path. Authoring is revisioned, identity-addressed, audited, and composed into the next run without turning an agent into an isolation principal or capability grant.

## RFC anchor

- RFC §6.7
- RFC §6.16
- RFC §6.11
- RFC §5.2
## Briefs informing this phase

- brief 04
## Brief findings incorporated

- brief 04 §4.2: incomplete identity fails closed; no default scope.
- brief 04 §4.5: RequiredTools are filtered and redacted at injection time.
- brief 04 §4.6: the virtual directory is an immutable, bounded identity-scoped snapshot.
## Findings I'm departing from (if any)

- None.
## Goals

- Add D-411's elevated mutation path, durable pack records, revision/diff/rollback/audit integration, and one composite resolver.
- Feed the exact immutable snapshot to directory injection and `skill_search/get/list`.
## Non-goals

- No copying into user rows, shared service identity, broad tenant/global search, or agent-based storage filter.
- No mid-run mutation or RequiredTools grant.
## Acceptance criteria

- [ ] A real Postgres backing store lets an operator pin a pack to an agent and two users/sessions see it after restart.
- [ ] Other agents and tenants cannot discover/fetch it; personal skills remain isolated.
- [ ] Missing/paused/disabled/scope-filtered required MCP tools filter/redact and never expand tools; dynamic MCP is exercised.
- [ ] Membership, rollback, and revoke affect next runs only; old runs retain snapshots; misses are typed.
- [ ] Non-operators and malformed identity/agent input fail before persistence; audit and revision evidence is emitted.
- [ ] Mixed tenant/user/agent concurrent isolation test has no bleed.
## Files added or changed

- `internal/skills/{skills.go,pack.go,pack_test.go,drivers/postgres/*_test.go}`
- `internal/runtime/agentcfg/{protocol,projection}/*`
- `internal/runtime/serve/runloop.go` and resolver tests
- `internal/protocol/{types,methods,singlesource}/*` if the additive read/write surface is wired
- `test/integration/agent_skill_pack_test.go`
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`, `scripts/smoke/phase-237.sh`
## Public API surface

- Elevated agent-pack mutation and revision membership use the existing agent-config control-plane contract.
- `SkillProvider` receives a composed immutable snapshot; no new identity axis.
- `agent_id` remains a runtime/config address, never an isolation principal.
## Test plan

- **Unit:** validation, content hash, CAS/revision, rollback/revoke, capability filtering, redaction, resolver equivalence.
- **Integration:** real Postgres + agent-config + dynamically attached MCP source; restart and failure mode.
- **Conformance:** durable skill drivers preserve pack visibility and typed absence.
- **Concurrency / leak:** N=128 shared resolver across mixed identities; run cancellation and snapshot immutability.
## Smoke script additions

- Static checks for D-411, pack key shape, no `agent_id` identity filter, next-run wording, required-tool filtering, and real-Postgres/`-race` gate ledger.
## Coverage target

- `internal/skills`: 90%; `internal/runtime/agentcfg`: 90%; persistence drivers: 90%; integration: 80%.
## Dependencies

- 201, 221, 233, 233a. Gates 240; independent of 236, 238, 239, 242.
## Risks / open questions

- Revision and pack CAS failure must compensate without exposing a candidate; use existing conditional-save fences.
- Postgres is local-skip only when `HARBOR_PG_DSN` is absent; required in web/CI integration lane.
## Validation gate ledger

- **Local skip:** Postgres-backed integration may skip only without `HARBOR_PG_DSN`; SQLite/in-memory isolation remains local-required.
- **Web CI:** frontend lockstep/lint runs when the Protocol mirror changes; backend CI must run the Postgres integration and race matrix.
## Glossary additions

- **Governed skill authoring** — revisioned authoring of agent-owned skills under verified identity and policy.
## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real-driver integration with identity and failure mode under `-race`
- [ ] Glossary updated
