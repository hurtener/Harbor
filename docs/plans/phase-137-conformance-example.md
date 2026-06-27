# Phase 137 — Conformance worked example (in-tree)

## Summary

Ships an in-tree, worked example of certifying a Protocol-server fork (or
embedder assembly) against Harbor's conformance suite: a `go test`-compiled
harness at `examples/protocol-clients/conformance-fork/` that wires a **custom
`conformance.Factory`** — assembling its own real-driver runtime surface — and
hands it to `conformance.RunSuite`. It reinforces the already-documented
in-tree Factory-seam posture (reaffirming D-210); it does **not** make the
conformance suite externally importable.

## RFC anchor

- RFC §5.1 (the Console/Protocol decoupling rule the suite certifies a fork
  still honours)
- RFC §5.2 (what the Protocol exposes — the surface the suite exhaustively
  exercises across both consumer profiles)
- RFC §5.3 (versioning — the version + capability handshake the suite asserts)

## Briefs informing this phase

- brief 07
- brief 06

## Brief findings incorporated

- brief 07 §"the runtime owns the protocol it speaks": the Protocol surface is
  versioned and contract-stable, and a Protocol consumer reaches it only
  through the canonical wire types / methods / errors / events. The worked
  example makes that contract's *certification* concrete for a fork: it runs
  the same executable conformance contract over a fork's own assembly, so the
  fork proves wire-compatibility instead of asserting it.
- brief 06 §"developer experience is a first-class surface": adoption-facing
  artifacts must be runnable and rot-proof, not prose. The example is gated by
  `go test` (the suite itself is the correctness gate, §17.8) so the documented
  Factory seam cannot silently drift from a working call site.

## Findings I'm departing from (if any)

None.

## Goals

- Give an implementer a copy-paste-able, runnable demonstration of wiring a
  custom `conformance.Factory` + `RunSuite` against their own Protocol-server
  assembly.
- Make the demonstration self-verifying: the conformance suite running green
  over the custom Factory IS the gate (a mis-wire fails, never silently).
- Point the certification page at the worked example so the documented Factory
  seam has a runnable referent.

## Non-goals

- **Making the conformance suite externally importable.** The suite stays under
  `internal/protocol/conformance` (deliberately, per D-210); `RunSuite` stays
  `*testing.T`-bound. Publishing an importable package or a standalone
  certification runner is a §3-layout / RFC change this phase does not propose.
- A runnable client binary. `RunSuite` is `*testing.T`-bound, so the example is
  a `_test.go` harness, not a `package main` like the sibling event-viewer.
- Any change to the conformance suite, the Protocol surface, or `harbor serve`.

## Acceptance criteria

- [ ] `examples/protocol-clients/conformance-fork/` exists with a `doc.go`
      (package narrative) and a `_test.go` harness.
- [ ] The harness wires a **custom** `conformance.Factory` that assembles a
      real-driver `*conformance.Stack` (its own event bus, state store, task
      registry, control surface, wire mux, ES256 validator) — not merely the
      one-line `NewDefaultFactory` the cert page already shows.
- [ ] `go test -race ./examples/protocol-clients/conformance-fork/...` passes —
      the full suite runs green over the custom Factory.
- [ ] `docs/site/protocol/conformance-certification.md` points at the worked
      example and states why it is a `go test` harness, not a binary.
- [ ] `scripts/smoke/phase-137.sh` passes (file presence + custom-Factory pins
      + cert-page pointer + the `go test` execution gate); OK ≥ 9, FAIL = 0.
- [ ] No Protocol surface, conformance-suite, or `serve` behaviour changes.

## Files added or changed

- `examples/protocol-clients/conformance-fork/doc.go` (new — package narrative)
- `examples/protocol-clients/conformance-fork/conformance_fork_test.go` (new —
  the custom-Factory harness + `TestConformanceFork`)
- `docs/site/protocol/conformance-certification.md` (the worked-example pointer)
- `scripts/smoke/phase-137.sh` (new — the gate)
- `docs/plans/phase-137-conformance-example.md` (this plan)
- `docs/plans/README.md` (index row + detail block, Pending)

## Public API surface

None. The example consumes the already-exported conformance surface
(`conformance.Factory`, `conformance.Stack`, `conformance.RunSuite`,
`conformance.FixedNow`); it exports nothing other phases depend on.

## Test plan

- **Unit:** N/A — the deliverable IS a test harness.
- **Integration:** `TestConformanceFork` runs the entire conformance suite
  (every canonical method on both consumer profiles, every error code, the
  version/capability handshake, the fail-closed auth pipeline, the wire status
  mapping, the N=100 concurrent-reuse run) against a custom Factory wired from
  real V1 in-memory drivers. Real drivers on every seam (§17.3); identity
  propagation, the fail-closed auth legs (HS256 / `alg:none` / expired tokens
  rejected), and the concurrent-reuse stress are all exercised by the suite.
- **Conformance:** the example IS a conformance-suite consumer.
- **Concurrency / leak:** inherited — the suite's
  `ConcurrentReuse_SharedStack_NoCrossTalk` scenario runs under `-race`
  (`go test -race ./examples/protocol-clients/conformance-fork/...`).

## Smoke script additions

`scripts/smoke/phase-137.sh` (PREFLIGHT_REQUIRES: unit-tests):

- Asserts `doc.go` + the `_test.go` harness are present.
- Pins the custom-Factory shape: the harness references `conformance.Factory`,
  `conformance.RunSuite`, `conformance.Stack`, and assembles its own
  `transports.NewMux` + `auth.NewValidator` (so it is a real fork seam, not the
  one-line default).
- Asserts the certification page points at `conformance-fork`.
- EXECUTION GATE: `go test ./examples/protocol-clients/conformance-fork/...`
  passes.

## Coverage target

N/A — `examples/protocol-clients/conformance-fork` is a worked example whose
only Go file outside tests is `doc.go` (no production logic). The binding gate
is that the suite runs green over the custom Factory.

## Dependencies

- 62 (the Protocol conformance suite — `Factory` / `Stack` / `RunSuite`)
- 113b (the conformance-certification page this example is pointed at; D-210)

## Risks / open questions

- **Drift from the canonical assembly.** The custom Factory reproduces the
  real-driver wiring (event bus, state, tasks, surface, mux, validator, token
  closures). Mitigation: the suite itself is the gate (§17.8) — a wiring that
  diverges from the real surface fails `RunSuite`, so the example cannot
  silently rot into a self-consistent lookalike.
- **No new external-importability demand is created.** The example deliberately
  stays in-tree; the export decision still waits on real third-party demand via
  the issue tracker (D-210). No open question reopened.

## Glossary additions

None — no new vocabulary introduced.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (N/A — worked example; gate
      is the suite running green)
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (N/A — no isolation code changed; the suite's isolation scenarios run
      unchanged over the custom Factory)
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes
      (N/A — the example builds no production artifact; it consumes the suite's
      own concurrent-reuse scenario under `-race`)
- [ ] If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists, wires real drivers
      end-to-end, asserts identity propagation, covers ≥1 failure mode, runs
      under `-race` — satisfied: `TestConformanceFork` runs the full suite over
      real drivers under `-race`.
- [ ] If new vocabulary: glossary updated (N/A)
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed (N/A — no departure)
