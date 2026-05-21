# Phase 85j — mcp-client-conformance

## Summary

The band's closing phase: a conformance harness that exercises every MCP client capability Phases 85a–85i added, plus the scoped, *substantiated* compliance statement. The harness runs Harbor's MCP client against a battery of mock MCP servers — each mock exercising one capability area (transports, OAuth, sampling, elicitation, roots, completions/logging/templates/progress, Apps, Tasks) — and asserts spec-correct behaviour. The compliance statement (a doc) is only as true as the harness that backs it; this phase makes the claim defensible.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §4: "never write 'fully MCP compliant' unscoped. The correct phrasing once the band lands is *'MCP 2025-11-25 core-compliant, with stdio + Streamable HTTP transports, OAuth for remote servers, Roots, Sampling, Elicitation, Tasks, and MCP Apps support.'* Phase 85j produces the conformance harness that *substantiates* that sentence." — this phase is exactly that.
- brief 14 §2: the capability matrix is the harness's checklist — every row classified **Wired** by the end of the band gets a conformance assertion.
- brief 14 §7: "85j is last — it conforms the whole band." — the dependency ordering.
- brief 14 §5: Tasks is experimental; the conformance harness marks the Tasks pass as experimental-tier so a Tasks spec change is a contained, visible harness diff.

## Findings I'm departing from (if any)

- None.

## Goals

- A conformance harness — a battery of mock MCP servers + a test suite — that exercises every capability the 85-band added and asserts spec-2025-11-25-correct client behaviour.
- The harness is structured per capability area, so a single area's regression is pinpointed (mirrors the `conformance.RunSuite` pattern Harbor uses for persistence drivers).
- A `docs/` compliance statement: the precise, scoped sentence describing Harbor's MCP client support, with each clause traceable to a harness assertion.
- The harness runs in CI (it uses mock servers, no live network / no credentials) and gates regressions.
- A capability matrix doc — the brief 14 §2 matrix, updated to the post-band state — lives alongside the compliance statement as the authoritative "what Harbor's MCP client does" reference.

## Non-goals

- New client capabilities — this phase adds no MCP feature; it conforms what 85a–i shipped.
- Live-server conformance against third-party MCP servers — the harness uses mock servers for determinism; an optional live-probe script (operator-runnable, not a CI gate) may be included but is not the conformance gate.
- The official MCP conformance test suite, if one exists upstream — if upstream ships one, a follow-up wires it; this phase ships Harbor's own.

## Acceptance criteria

- [ ] A conformance harness at `internal/tools/drivers/mcp/conformance/` (or `test/integration/mcp_conformance_test.go`) with one mock-server + assertion group per capability area: base protocol, stdio transport, Streamable HTTP transport, HTTP OAuth, tools, resources (+ templates + subscriptions), prompts, completions, logging, progress, roots, sampling, elicitation, Apps capability negotiation, Tasks.
- [ ] Each group asserts spec-correct behaviour, including ≥1 failure mode (the harness is not happy-path only).
- [ ] The harness asserts capability *honesty* — Harbor advertises exactly the capabilities it services (the regression guard for the brief 14 §3 roots defect).
- [ ] Pagination is conformance-tested with a multi-page mock (regression guard for the brief 14 §3 truncation bug).
- [ ] The harness runs under `-race` in CI with no live network and no credentials.
- [ ] `docs/design/mcp-compliance.md` (or similar) carries: (a) the scoped compliance statement; (b) the post-band capability matrix; (c) a clause-to-assertion traceability table — every phrase in the statement points at the harness assertion that backs it.
- [ ] The compliance statement uses the brief 14 §4 binding wording — scoped, never "fully compliant" unqualified.
- [ ] The Tasks conformance group is marked experimental-tier (Tasks is experimental in 2025-11-25); a Tasks spec bump is a contained harness diff.

## Files added or changed

- `internal/tools/drivers/mcp/conformance/` (new) — the mock-server battery + the per-area assertion groups. (Or `test/integration/mcp_conformance_test.go` if a single-package home is cleaner — implementer's call, documented.)
- `docs/design/mcp-compliance.md` (new) — the compliance statement + post-band matrix + traceability table.
- `.github/workflows/ci.yml` — ensure the conformance harness runs in the `go` job (it is plain `go test`, no special infra).
- `README.md` — the root README's MCP section gains the scoped compliance sentence (linked to `docs/design/mcp-compliance.md`).
- `scripts/smoke/phase-85j.sh`.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

No production API. The harness is test code; the compliance doc is documentation.

## Test plan

- **Unit:** each mock server is itself unit-tested for spec-correct *server* behaviour (a mock that misbehaves invalidates the conformance result).
- **Integration / conformance:** the harness IS the integration test — Harbor's real MCP client driver against the mock battery, every capability area, ≥1 failure mode each, `-race`.
- **Concurrency / leak:** a cross-area concurrency group — N≥10 concurrent connections spanning multiple capability areas — asserts no cross-connection bleed and a restored goroutine baseline.
- **Regression guards:** explicit tests for the two brief 14 §3 defects (pagination truncation, roots honesty) so they cannot silently return.

## Smoke script additions

- `scripts/smoke/phase-85j.sh` (classification: `static-only`):
  - Assert `docs/design/mcp-compliance.md` exists and contains the scoped compliance sentence (grep for "2025-11-25 core-compliant").
  - Assert the compliance doc does NOT contain the unscoped phrase "fully compliant" / "fully MCP compliant".
  - Assert the conformance harness directory/file exists.

## Coverage target

- `internal/tools/drivers/mcp` (overall, post-band): 85% — the conformance harness materially raises exercised coverage; this phase must leave the package at or above target.

## Dependencies

- 85a, 85b, 85c, 85d, 85e, 85f, 85g, 85h, 85i — the harness conforms all of them; it is the band's terminal phase.

## Risks / open questions

- **Mock-server fidelity.** A conformance harness is only as good as its mocks; a mock that diverges from the spec produces a false pass. Each mock is unit-tested against the spec's example payloads (brief 14 §5 + the spec pages) to anchor fidelity.
- **Upstream conformance suite.** If the MCP project ships an official client conformance suite, Harbor's harness should converge with it; this phase notes that as a tracked follow-up rather than blocking on it.
- **Statement drift.** The compliance statement can rot if a later phase changes behaviour without updating it. The clause-to-assertion traceability table is the guard: a changed assertion forces a statement review. A drift-audit-style check (statement clauses ↔ harness assertions) is a stretch goal.
- **Apps conformance scope.** Apps is Console-side; the harness conforms the wire-level Apps *capability negotiation*, while the rendering/sandbox conformance is Phase 85g's Playwright suite. The compliance doc is explicit about this split.

## Glossary additions

- **MCP conformance harness** — Harbor's battery of mock MCP servers + assertion groups that substantiate the scoped MCP compliance statement. Runs in CI under `-race`, no live network.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — the cross-area concurrency group asserts cross-connection isolation.
- [ ] **Concurrent-reuse test passes** — N≥10 concurrent multi-area connections under `-race`.
- [ ] **Integration test passes** — the conformance harness itself is the integration gate.
- [ ] Glossary updated.
- [ ] No brief departures.
