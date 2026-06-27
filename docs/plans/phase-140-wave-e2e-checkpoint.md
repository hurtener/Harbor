# Phase 140 — Wave E2E + v1.8.0 checkpoint audit

## Summary

The wave-end composing end-to-end test for the v1.8.0 "Adopter-Path" band.
Ships `test/integration/wave_v18_test.go` (matching the `wave_v17_test.go`
precedent), which proves the three advertised adopter paths — embed, protocol,
MCP-tool-dispatch — are alive TOGETHER on real drivers under `-race`, then runs
the §17.5 read-only checkpoint audit that gates the next band's scoping. This
phase ships no production code: it is the wave's integration gate.

## RFC anchor

- RFC §5.4 — the wire transport the `harbor serve` Protocol leg exercises.
- RFC §5.5 — authentication; the JWKS-verified attach both on-ramps prove.
- RFC §6.4 — the tool catalog + transports the MCP dispatch leg drives.
- RFC §8 — the CLI layer (`harbor serve` / `harbor token`) the Protocol legs boot.

## Briefs informing this phase

- brief 07

## Brief findings incorporated

- brief 07 (the ReAct planner / tool-calling path): the MCP dispatch leg drives
  a real planner→executor tool invocation and asserts the dispatch signal in the
  step OBSERVATION (the executor's result), not the planner's input args — the
  same discriminating-signal discipline the brief's tool-calling model requires,
  so a catalog-listing test can never produce a false green.

## Findings I'm departing from (if any)

None.

## Goals

- Prove the v1.8.0 embed / protocol / MCP adopter paths compose end-to-end on a
  single suite with real drivers, under `-race`.
- Exercise BOTH protocol on-ramps (IdP mock-OIDC and `harbor token`
  bring-your-own) against a real `harbor serve`, with the verifier untouched.
- Re-assert the wave's load-bearing invariants (D-220 "serve mints nothing",
  identity propagation, no cross-run bleed) at the composed surface.
- Gate the next band: the §17.5 checkpoint audit closes the wave.

## Non-goals

- No new production code, no new Protocol method, no new decision (D-NNN).
- No re-implementation of the per-phase gates — they remain the per-phase gates;
  this suite proves the COMBINED surface.
- The `harbor` CLI-binary paths already covered by the per-phase smokes (131d /
  133 / 138) are not re-driven here as binary smokes; this Go E2E drives
  in-process (and boots `harbor serve` as a subprocess only for the protocol
  legs, mirroring 131c).

## Acceptance criteria

- [ ] `test/integration/wave_v18_test.go` exists, `package integration`, named
  `TestE2E_WaveV18_*`.
- [ ] EMBED leg: a goal via `Stack.RunOnce` returns a terminal answer; a
  `WithStream` variant asserts ordered chunks arrive BEFORE the final envelope.
- [ ] PROTOCOL leg: the binding mock-OIDC → `harbor serve` → `runtime.info` OK
  with the JWT-decoded granted scope; the `harbor token` keygen → `jwks_file` →
  mint → `runtime.info` OK bring-your-own leg; a mismatched-`iss` token → 401.
- [ ] MCP leg: an in-process goal invokes `mcptest_echo` through the executor;
  the echoed sentinel appears in the step observation.
- [ ] ≥1 failure mode per edge: mismatched-iss 401, garbage-token 401,
  missing-identity 401; the D-220 invariant re-asserted at the binary surface.
- [ ] Identity propagation asserted end-to-end + a cross-identity `Tasks.Get`
  isolation assertion.
- [ ] N≥10 concurrency stress: N concurrent `RunOnce` against one shared `Stack`,
  no answer/chunk bleed, goroutine baseline restored after teardown.
- [ ] `scripts/smoke/phase-140.sh` pins the file present + a `go test -list`
  no-match-fails guard on `TestE2E_WaveV18`.
- [ ] `go test -race -run TestE2E_WaveV18 ./test/integration` passes and is
  non-flaky across repeats.

## Files added or changed

- `test/integration/wave_v18_test.go` (new — the composing wave-end E2E).
- `docs/plans/phase-140-wave-e2e-checkpoint.md` (this plan).
- `docs/plans/README.md` (index row + detail block).
- `scripts/smoke/phase-140.sh` (new — static name-pinning gate).

## Public API surface

None. This phase consumes the wave's shipped surfaces (`Stack.RunOnce`,
`assemble.WithStream`, `harbor serve`, `harbor token`, the MCP dispatch path)
and adds no new exported identifier.

## Test plan

- **Unit:** N/A — this phase IS the integration test.
- **Integration:** `test/integration/wave_v18_test.go` —
  `TestE2E_WaveV18_AdopterPaths` (embed RunOnce + WithStream ordering; both
  protocol on-ramps against a real `harbor serve`; MCP tool dispatch through the
  executor) and `TestE2E_WaveV18_ConcurrencyStress` (N≥16 concurrent `RunOnce`).
  Real drivers on every seam (the dev-only mock LLM on the embed path and the
  scripted LLM on the MCP path are the explicit, gated test surfaces); identity
  propagation asserted across all legs; ≥1 failure mode per edge; under `-race`.
- **Conformance:** N/A.
- **Concurrency / leak:** the N≥16 concurrent-`RunOnce` stress against one shared
  `Stack` asserts no answer bleed, no cross-run chunk bleed, and a restored
  goroutine baseline after `Close`.

## Smoke script additions

- `scripts/smoke/phase-140.sh`: asserts `test/integration/wave_v18_test.go`
  exists and defines `TestE2E_WaveV18_AdopterPaths`; runs `go test -list` with a
  no-match-fails guard on `^TestE2E_WaveV18` so the gate can never silently match
  zero tests (the false-green hazard 136 closes for its own gate).

## Coverage target

N/A (test-only phase — adds no production package; the touched package is
`test/integration`, which carries no coverage gate).

## Dependencies

131c, 131d, 132, 132-stream, 133, 135, 136, 137, 138.

## Risks / open questions

- The protocol legs boot `harbor serve` as a subprocess (the binary is built
  once via the shared OIDC round-trip build helper). On a build that predates
  `harbor serve` / `harbor token`, the affected subtests SKIP (the 404/405/501 →
  SKIP convention applied to a CLI surface), so the gate degrades gracefully on
  older builds while asserting the surface where it exists.

## Glossary additions

None.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (N/A — test-only)
- [x] If multi-isolation paths changed: cross-session isolation test passes (the
  concurrency stress + cross-identity `Tasks.Get` assertion cover it)
- [x] **If this phase builds a reusable artifact:** N/A — this phase builds no
  reusable artifact; it composes already-shipped ones. The N≥16 concurrent-reuse
  stress against the shared embed `Stack` is exercised here regardless.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a
  cross-subsystem seam:** `test/integration/wave_v18_test.go` wires real drivers
  end-to-end, asserts identity propagation, covers ≥1 failure mode per edge, and
  runs under `-race`.
- [x] If new vocabulary: glossary updated (none)
- [x] If a brief finding was departed from: justified above + decisions.md entry
  filed (none departed from)
