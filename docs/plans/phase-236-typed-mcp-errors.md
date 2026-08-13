# Phase 236 — Typed MCP errors (HA-54)

## Summary

Define Harbor's transport-neutral typed MCP error contract. Permanent errors stop the unchanged call after one invocation; retryable service errors retain the configured retry budget.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §6.13
## Briefs informing this phase

- brief 03
- brief 07
- brief 14
## Brief findings incorporated

- brief 03 §4: every transport lowers to one `ToolDescriptor.Invoke` contract.
- brief 07 §5: classification belongs at runtime dispatch, not in an LLM/provider adapter.
- brief 14 §2: compliance claims must be based on real SDK behavior and preserved structured content.
## Findings I'm departing from (if any)

- None.
## Goals

- Implement D-410's additive, standard-result-compatible classification envelope and explicit fallback.
- Preserve bounded text, structured content, audit redaction, and App delivery.
- Exercise stdio, streamable HTTP, and SSE using fixtures derived from the official MCP SDK/spec artifact.
## Non-goals

- No per-server retry override, text parsing, `isError` redefinition, or consumer-specific exception.
- No Protocol version bump unless a wire projection is proven necessary.
## Acceptance criteria

- [ ] `invalid_argument`, validation, and `tool_domain` are permanent for the unchanged call and invoke exactly once.
- [ ] Typed retryable/provider failures consume the configured budget and can recover; timeout and 5xx behavior is unchanged.
- [ ] Legacy, absent, malformed, and unknown classes follow a documented compatibility fallback without text inference.
- [ ] Original bounded content and structured content reach planner/model and App paths.
- [ ] Real SDK/spec-derived fixtures cover stdio, streamable HTTP, and SSE; the legacy transient regression remains.
- [ ] Identity `(tenant,user,session)`, redaction, cancellation, and no raw result/argument logging are proven.
## Files added or changed

- `internal/tools/drivers/mcp/{content.go,mcp_test.go,fixtures_test.go}`
- `internal/tools/policy.{go,test.go}`
- `test/integration/mcp_error_classification_test.go`
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`
- `scripts/smoke/phase-236.sh`
## Public API surface

- `tools.ErrorClass` and the existing `ToolPolicy.RetryOn` consume the typed classification.
- MCP transport lowering remains a normal `ToolResult`/error contract; no new Protocol method.
## Test plan

- **Unit:** envelope validation, lowering, fallback, policy mapping, wrapping, bounded-content preservation.
- **Integration:** real SDK-derived MCP fixtures over stdio/streamable HTTP/SSE through the retry shell; identity and a forced failure.
- **Conformance:** the same fixture matrix and call-count assertions for all supported MCP transports.
- **Concurrency / leak:** one shared driver, 128 mixed identities, cancellation and joined goroutines under `-race`.
## Smoke script additions

- Static assertions for the phase plan, D-410, all three transport names, permanent classes, fallback, and the no-text-parser rule; Protocol probe is reserved only if an additive wire surface lands.
## Coverage target

- `internal/tools/drivers/mcp`: 90%; `internal/tools`: 85%; integration fixture paths: 80%.
## Dependencies

- 26b, 28. Independent of 237, 238, 240, 241, and 242; gates 239.
## Risks / open questions

- SDK extension placement may differ across supported transports; pin the observed wire transcript before implementation and fail closed on ambiguity.
- A future Protocol projection would trigger D-223/D-209 lockstep.
## Validation gate ledger

- **Local skip:** real-provider/network probes may skip when their explicit live-MCP environment is absent; committed SDK transcripts remain mandatory.
- **Web CI:** not applicable; Go CI must run transport conformance and race integration.
## Glossary additions

- **MCP error classification** — typed control metadata on an `IsError` result that distinguishes permanent unchanged-call failures from retryable service failures without parsing prose.
## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test N≥100 passes under `-race`
- [ ] Integration test uses real MCP drivers, identity propagation, a failure mode, and `-race`
- [ ] New vocabulary: glossary updated
- [ ] No brief departure requires justification
