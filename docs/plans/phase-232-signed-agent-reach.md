# Phase 232 — Signed agent reach

## Summary

Bind each authenticated data-plane bearer to a bounded signed set of agent registration IDs and enforce that authority through one effective-agent gate. The gate covers `control.start`, all five `agent_config.session.*` methods, all eight `agent_config.user.*` methods, and only the explicit agent projection of `tools.describe`.

## RFC anchor

- RFC §5.5.
- RFC §6.16.

## Briefs informing this phase

- brief 06
- brief 07
- brief 09
- brief 11

## Brief findings incorporated

- brief 06 §4: cross-scope authority is server-derived, explicit, and observable rather than inferred from caller-selected data.
- brief 07 §2: one runtime mechanism owns a cross-cutting policy instead of transport-specific copies.
- brief 09 §5: agent-bound authority is explicit and keyed by registration identity.
- brief 11 §4: Protocol clients request projections while server-side authorization remains authoritative.

## Findings I'm departing from (if any)

- brief 09's early agent-as-isolation framing is superseded by RFC §6.16 and D-059: `agent_id` is registration metadata and a resource entitlement target, never a fourth isolation principal.

## Goals

- Parse a strict bounded `agent_reach` JWT claim into immutable verified authority.
- Resolve every effective agent choice before one shared authorization check.
- Deny before session, task, config, skill, or projection side effects.
- Preserve `tools.describe` without `agent_id` as the boot-effective projection.

## Non-goals

- No wildcard reach, agent hierarchy, cross-agent delegation, or new isolation axis.
- No reinterpretation of required empty agent-config targets as defaults.
- No Protocol-version bump or request-body authority field.

## Acceptance criteria

- [ ] `agent_reach` is a unique nonblank JSON string array, bounded to 128 IDs of at most 128 bytes; malformed claims reject authentication, while absent/empty reach authorizes no agent-addressed data-plane call.
- [ ] One shared gate is constructed once by production/devstack assembly and used by every enumerated call site.
- [ ] Explicit and omitted/defaulted `control.start` targets require reach before creating a session or task; configured existence remains only a resolvability check.
- [ ] All five session methods and all eight user methods, including the three currently claim-free user-skill routes, require reach before service invocation.
- [ ] `tools.describe` with `agent_id` requires reach; omission remains byte-compatible boot-effective behavior.
- [ ] Carrier-identity and direct in-process calls without signed verified reach fail closed.
- [ ] Dev/token minting can issue bounded reach and the default dev bearer reaches only the boot agent.
- [ ] A table-driven live matrix pins permitted, excluded, missing, empty, malformed, cross-tenant, and no-side-effect behavior for every covered method; N≥100 concurrent calls show no reach bleed.

## Files added or changed

- `internal/protocol/auth/`
- `internal/protocol/control.go`
- `internal/protocol/transports/stream/`
- `internal/runtime/serve/`
- `cmd/harbor/` and `harbortest/devstack/`
- `test/integration/agent_reach_test.go`
- `scripts/smoke/phase-232.sh`
- RFC, decisions, glossary, generated Protocol documentation, and release notes

## Public API surface

- `auth.Verified.AgentReach []string`
- One immutable shared agent-reach authorizer/gate injected into Protocol assembly.
- Additive `harbor token mint --agent-reach <id>[,<id>...]` support.

## Test plan

- **Unit:** strict claim parsing, boundedness, gate result, default resolution, and no-service-call refusals.
- **Integration:** authenticated real mux over every enumerated method, default selection, cross-tenant config, carrier posture, and `tools.describe` omission.
- **Conformance:** a closed method census fails if a covered method is added or removed without a gate row.
- **Concurrency / leak:** N≥100 mixed-authority calls against one mux under `-race`, cancellation isolation, and goroutine baseline.

## Smoke script additions

- Mint allowed, excluded, missing, empty, and malformed-reach bearers and exercise all four method families against the live server.
- Assert denied starts create no session/task and denied config calls do not mutate revisions, skills, or overlays.

## Coverage target

- `internal/protocol/auth`: 90%; `internal/protocol` and `internal/protocol/transports/stream`: 85%; touched runtime/CLI packages do not regress below their v1.25 floors.

## Dependencies

- 151, 205, 221, 228.

## Risks / open questions

- Existing callers must mint reach before using agent-addressed data-plane surfaces; the release note is mandatory.
- Strict malformed-claim rejection is intentionally authentication-wide even when a request targets an unrelated method.

## Glossary additions

- Agent reach.
- Effective agent selection.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse N≥100 test passes with no race, bleed, cancellation cross-talk, or leak
- [ ] Cross-subsystem real-mux integration test covers identity and at least one refusal
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
