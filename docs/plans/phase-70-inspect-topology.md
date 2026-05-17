# Phase 70 — `harbor inspect-topology`

## Summary

Replace the Phase 63 stub of `harbor inspect-topology` with the real subcommand. Render a single run's node graph as deterministic ASCII (golden-pinned), driven by a Protocol-client read of the Phase 60 SSE event stream filtered by `X-Harbor-Run`. The topology is **trajectory-synthesised** from existing per-event types (`tool.invoked` / `tool.completed` / `task.spawned` / `pause.requested` / `planner.finish`) because the canonical `topology.snapshot` event (Phase 74) has not yet landed. The renderer is byte-stable for a given event ordering; both ASCII (default) and JSON (`--json`) outputs are pinned by golden files.

## RFC anchor

- RFC §8

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §3 (Devx surface): "CLI golden tests: `harbor dev --help`, `harbor scaffold --help`, `harbor validate`, `harbor inspect-events --run <id>` produce stable output matching golden files." Phase 70 ships golden-pinned ASCII + JSON outputs so a future change to the renderer is a deliberate, reviewed regeneration via `go test -update`.
- brief 06 §3 (Devx posture): "All of this is implemented as protocol clients of the same runtime — no private hooks." `harbor inspect-topology` is a pure Protocol client: it reads the Phase 60 SSE wire shape (`internal/protocol/transports/stream.wireEvent`) by re-declaring the JSON struct in `cmd/harbor`. The CLI never imports `internal/protocol/transports/stream` — the contract is the WIRE shape, not the Go struct.
- brief 06 §4 (CLI subcommand breadth at V1): `inspect-topology` is settled as a V1 subcommand. Phase 70 lands the body; future phases (Phase 74 `topology.snapshot` events) extend the same renderer with a preferred-source branch.

## Findings I'm departing from (if any)

None. The master-plan-named `topology.snapshot` event is the canonical source per RFC §6.13; Phase 74 is its owner. Phase 70 ships the renderer end-to-end now (per §13 primitive-with-consumer) so the CLI subcommand has a working acceptance the day it lands — when Phase 74 ships the canonical event, the renderer's `BuildTopologyFromEvents` branch handles it as one additional event-type case. The "trajectory-synthesised" departure is documented in D-102.

## Goals

- Replace `cmd/harbor/cmd_inspect_topology.go`'s `not_implemented` stub with a working command body.
- Render a single run's node graph as deterministic ASCII (indent-based + `+--` connectors).
- Pin the wire-side shape with golden files for both ASCII and JSON modes.
- Provide a structured `CLIError` for every operator-facing failure mode (bad bind, bad width, missing token, unknown run, transport failure, HTTP status).
- Ship the Wave 12 wave-end E2E (`test/integration/wave12_test.go`) per CLAUDE.md §17.7 step 5.

## Non-goals

- Streaming live topology as it changes — the command is a snapshot, terminating on `planner.finish` OR `--idle-timeout`. A live-tail variant (`--watch`) is a post-V1 follow-up.
- Reading the canonical `topology.snapshot` event — that landing is Phase 74. The synthesise path is documented in D-102 as the V1 source.
- Mutating any runtime state. `inspect-topology` is read-only.
- Implementing a Console view — Console renders topology directly from the Protocol events; this CLI is the operator-side observability surface, not a Console feeder.

## Acceptance criteria

- [x] `harbor inspect-topology <run-id>` exits 0 against a running runtime and emits the rendered ASCII tree to stdout.
- [x] `harbor inspect-topology --json <run-id>` emits canonical `Topology` JSON parseable by `jq`.
- [x] Two runs with the same event sequence produce byte-identical output (golden-pinned, regenerable via `go test -update ./cmd/harbor/...`).
- [x] Sample run produces stable ASCII matching `cmd/harbor/testdata/golden/inspect-topology-happy.txt` (master-plan acceptance — "Sample run produces stable ASCII matching golden").
- [x] Each operator-facing failure mode produces a stable `CLIError` code (`inspect_topology_bind_invalid`, `inspect_topology_width_invalid`, `inspect_topology_auth_missing`, `inspect_topology_run_id_missing`, `inspect_topology_connect_failed`, `inspect_topology_http_status`, `inspect_topology_run_not_found`).
- [x] `scripts/smoke/phase-70.sh` exercises the cmd + renderer + live-server path and shows OK > 0 / FAIL = 0.
- [x] `scripts/smoke/phase-63.sh`'s stub-subcommands array drops `inspect-topology` (cross-phase smoke maintenance per §17.6).
- [x] `cmd/harbor/cmd_stub_test.go` drops the `inspect-topology` entry from `stubCases`.
- [x] `cmd/harbor/testdata/golden/help.txt` regenerated; the `(Phase 70)` suffix drops from the subcommand's Short.
- [x] Wave 12 wave-end E2E (`test/integration/wave12_test.go`) bundled with this PR per CLAUDE.md §17.7 step 5: real drivers across the wave's surface, identity propagation, ≥1 failure mode, N≥10 concurrency stress.

## Files added or changed

- `cmd/harbor/cmd_inspect_topology.go` — replace stub with real body; ~400 LOC.
- `cmd/harbor/cmd_inspect_topology_test.go` — cobra-driver + transport tests.
- `cmd/harbor/topology_render.go` — pure ASCII renderer (Render + RenderJSON + Topology / TopologyNode types).
- `cmd/harbor/topology_render_test.go` — renderer unit tests + golden round-trip.
- `cmd/harbor/topology_synthesise.go` — wire-frame → Topology builder.
- `cmd/harbor/cmd_stub_test.go` — drop `inspect-topology` from `stubCases`.
- `cmd/harbor/testdata/golden/help.txt` — regenerate (drop Phase 70 suffix).
- `cmd/harbor/testdata/golden/inspect-topology-happy.txt` — golden ASCII.
- `cmd/harbor/testdata/golden/inspect-topology-happy.json` — golden JSON.
- `scripts/smoke/phase-70.sh` — new smoke.
- `scripts/smoke/phase-63.sh` — remove `inspect-topology` from stubs array.
- `test/integration/wave12_test.go` — wave-end E2E (per §17.7 step 5).
- `docs/decisions.md` — append D-102.
- `docs/plans/phase-70-inspect-topology.md` — this file.
- `docs/plans/README.md` — flip Phase 70 row to Shipped.
- `README.md` — flip Phase 70 row in the Status table.

## Public API surface

- `cmd/harbor` does NOT export any new public Go symbols — the package is `package main`.
- `Topology` / `TopologyNode` / `TopologyNodeKind` / `TopologyNodeStatus` / `Render` / `RenderJSON` / `BuildTopologyFromEvents` / `ParseSSEFrames` / `WireEventFrame` are internal-to-`cmd/harbor` (lowercase package). They are NOT part of the runtime surface — `harbor inspect-topology` is the only consumer.

## Test plan

- **Unit:** `topology_render_test.go` pins the renderer (Render + RenderJSON + truncation + sort determinism + empty-nodes + ErrEmptyRunID) and the synthesiser (every event-kind → node-kind rule, paired-event upgrade, depth inference, orphan handling, identity copy from first frame). `cmd_inspect_topology_test.go` pins the cobra-driver paths (every CLIError code path) and exercises the SSE fetcher against an `httptest.Server` for both happy and failure modes.
- **Integration:** `test/integration/wave12_test.go` boots the assembled dev stack via `harbortest/devstack.Assemble` (D-094), drives a `start` request to spawn a run, opens an SSE subscription against the live runtime, and invokes `inspect-topology` against the real run-id end-to-end. Cross-tenant isolation (two tenants in parallel; each `inspect-topology` sees only its own events) + ≥1 failure mode (synthetic nonexistent run-id → `inspect_topology_run_not_found`) + N=10 concurrent operators streaming `inspect-events`-style SSE subscriptions (the concurrency-stress shape — proves no goroutine leak in the shared bus when many CLI clients fan in).
- **Conformance:** N/A — the CLI is a Protocol client, not a driver.
- **Concurrency / leak:** the renderer is a pure function (no goroutines); the fetcher's lifecycle owns ONE reader goroutine that is joined via ctx-cancel. The wave-end E2E asserts goroutine-baseline restoration after 10 concurrent SSE clients close.

## Smoke script additions

- `scripts/smoke/phase-70.sh` — runs `go test -race ./cmd/harbor/...`, asserts `harbor inspect-topology --help`/no-args/bad-flags structured-error shapes, and when `HARBOR_DEV_TOKEN` is exported in env (preflight harness or operator-set) drives `inspect-topology` against the live preflight-booted dev server and asserts `inspect_topology_run_not_found` for a synthetic run id.
- `scripts/smoke/phase-63.sh` — drops `inspect-topology` from the stub-subcommands array (cross-phase smoke maintenance).

## Coverage target

- `cmd/harbor`: ≥ 70% (master-plan target). Phase 70 lifts coverage on the new files (renderer + synthesiser + cmd body) past the threshold.

## Dependencies

- Phase 63 (CLI skeleton — stub subcommand) — Shipped.
- Phase 60 (Protocol transports — SSE event stream) — Shipped.
- Phase 64 (`harbor dev` — boots the live runtime the smoke + wave-end E2E both reach for) — Shipped.

## Risks / open questions

- The synthesise path assumes the V1 event taxonomy is stable. When Phase 74 ships `topology.snapshot`, the renderer gains a preferred-source branch; the synthesise path stays as fallback for runs whose snapshot frames pre-date the producer. No retrofit-required risk.
- The `--idle-timeout` default (1500ms) is a heuristic: fine-tuning may be needed in CI when the dev runtime is slow to flush. Operators can override via the flag; smoke uses 800ms for the deliberately-nonexistent-run test (fast SKIP) and 1500ms for live runs.
- The SSE parser is permissive (defensive against multi-line `data:` payloads per spec); a malformed frame returns the bytes accumulated so far + a wrapped error. The cmd surfaces this as `inspect_topology_connect_failed` with a "Runtime version mismatch" hint.

## Glossary additions

- None. "topology snapshot" already implied by RFC §6.13's `topology.snapshot` event language; no new term coined.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes (run as part of validation gate below)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (cmd/harbor lifts ≥ 70% with this PR)
- [x] If multi-isolation paths changed: cross-session isolation test passes — wave-end E2E asserts cross-tenant isolation across `inspect-topology` against two tenants in parallel.
- [x] **If this phase builds a reusable artifact:** the renderer (`Render` / `RenderJSON`) is a pure function — no goroutines, no shared state. The synthesiser (`BuildTopologyFromEvents`) is likewise pure. The cmd's SSE fetcher (`fetchSSEUntilIdle`) owns one reader goroutine per invocation joined via ctx — the wave-end E2E's N=10 concurrent invocations against a SHARED httptest.Server is the D-025 stress proof.
- [x] **If this phase consumes a shipped subsystem's surface:** Yes — Phase 60 SSE + Phase 64 dev cmd. The wave-end E2E (`test/integration/wave12_test.go`) wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`.
- [x] If new vocabulary: N/A
- [x] If a brief finding was departed from: N/A (D-102 documents the trajectory-synthesised source choice as a phase-scope departure, not a brief departure)
