# Phase 135 — TS wire-type generator + `event-viewer-ts`

## Summary

Ship a vendorable external-client TypeScript wire-type generator,
`cmd/harbor-protocol-ts-types`, that reflects over the canonical Harbor
Protocol single sources and emits a single self-contained `.ts` module of
interfaces + string-union types a third-party client copy-vendors. A worked
`event-viewer-ts` client (the TypeScript sibling of the Go `event-viewer`)
consumes the generated module against the dev runtime. This partially
retires the long-standing TS-generation deferral (D-132) for external
clients without touching the D-223 Console wire-manifest gate or the
reserved full-Console-generator name.

## RFC anchor

- RFC §5 — Harbor Protocol: the canonical contract third-party clients
  consume; the generated module is a read-only projection of it.
- RFC §5.3 — Versioning: the generated module pins `PROTOCOL_VERSION` and
  carries the `WIRE_SURFACE_DIGEST` the runtime reports on `runtime.info`,
  so a vendored client can detect wire skew.
- RFC §3.6 — The public SDK facade: the adopter-facing client surface; this
  phase closes the "no typed TS wire shapes for a non-Console client" gap on
  that surface.

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- brief 06 §9–11: *the Protocol is the single stable contract across the
  Console, third-party consoles, and IDE/TUI clients.* The generated module
  is the typed projection of that one contract, so a third-party TypeScript
  client anchors on the same canonical surface the Console does — not on
  hand-transcribed shapes that drift.
- brief 06 §13: *the decoupling guarantees Console + third-party consoles +
  embedded clients all speak the same wire.* The generator emits types only
  (no client logic, no Console import), so the artifact a third party vendors
  carries zero Console coupling.
- brief 07 §3 (the parsing surface): *clients tolerate what they don't know —
  unknown fields ignored, unknown methods surface as 404/405.* The
  `event-viewer-ts` consumer checks Protocol-major compatibility and the
  `events_subscribe` capability before subscribing, exactly the tolerate-what-
  you-don't-know discipline; it warns (not fails) on a wire-surface-digest
  skew.

## Findings I'm departing from (if any)

None.

## Goals

- A reflect-over-`CanonicalWireTypes` Go generator that emits a vendorable
  external-client TypeScript wire-type module (interfaces for every canonical
  wire type; `HarborMethod` / `HarborErrorCode` / `HarborEventType`
  string-union types; pinned `PROTOCOL_VERSION` / `WIRE_SURFACE_DIGEST`).
- A drift gate for that module (`protocol-ts-types-gen-check`) independent of
  and additive to the D-223 Console gate.
- A worked `event-viewer-ts` client that consumes the generated module
  against the dev runtime (the §13 consumer).
- The §18 skill + doc surfaces updated in the same PR to reflect that
  external clients now have a generated TS types artifact.

## Non-goals

- Generating the FULL Console-`protocol.ts` client (the per-page TypeScript
  type + `HarborClient` modules). That stays deferred (D-132); the
  `cmd/harbor-gen-protocol-ts` name stays reserved.
- Any change to the D-223 manifest gate, the `cmd/harbor-protocol-ts-lockstep`
  tool, the committed wire manifest, or the Console's hand-maintained
  `protocol.ts`.
- Any Protocol method / error / event / wire-type change (none — read-only
  projection).
- A typed `HarborClient` transport in the generated module (types only).

## Acceptance criteria

- [ ] `cmd/harbor-protocol-ts-types` reflects over
  `internal/protocol/singlesource.CanonicalWireTypes` (+ the method / error /
  event sets) and emits a deterministic, byte-stable `.ts` module.
- [ ] The generated module declares one `interface` per canonical wire type,
  the three string-union name types, and the two pinned constants, and
  carries a `CODE GENERATED … DO NOT EDIT` banner.
- [ ] New make targets `protocol-ts-types-gen` (emit) and
  `protocol-ts-types-gen-check` (regenerate + `git diff --exit-code` + Go
  lockstep tests) exist; the existing `protocol-ts-gen` / `protocol-ts-gen-check`
  are unchanged.
- [ ] `make protocol-ts-types-gen-check` is green AND `make protocol-ts-gen-check`
  (D-223) is still green; the committed wire manifest is byte-unchanged.
- [ ] A lockstep test pins the generator's type-instance index against
  `CanonicalWireTypes` (a new canonical type without an index entry fails),
  and an in-sync test fails when the committed module is stale.
- [ ] `examples/protocol-clients/event-viewer-ts/` ships a dependency-free
  client that imports the generated module, runs under Node's
  `--experimental-strip-types`, and completes a `runtime.info` round-trip
  against the dev runtime (probe mode).
- [ ] `scripts/smoke/phase-135.sh` asserts the static surface, the gen-check,
  manifest non-interference, and the live probe (SKIP on no node / no server).
- [ ] §18: `use-the-harbor-protocol/SKILL.md` (the three TS-generation
  assertions) and `docs/site/protocol/build-a-client.md` are updated in the
  same PR.

## Files added or changed

- `cmd/harbor-protocol-ts-types/main.go` — flag surface + `run`.
- `cmd/harbor-protocol-ts-types/emit.go` — `BuildModule` + reflection + TS-type mapping.
- `cmd/harbor-protocol-ts-types/render.go` — deterministic TS text rendering.
- `cmd/harbor-protocol-ts-types/typeindex.go` — independently-pinned type-instance index.
- `cmd/harbor-protocol-ts-types/lockstep_test.go` — lockstep + coverage + determinism + in-sync + render tests.
- `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts` — the committed generated module.
- `examples/protocol-clients/event-viewer-ts/event-viewer.ts` — the worked client.
- `examples/protocol-clients/event-viewer-ts/{package.json,tsconfig.json,README.md}` — example scaffolding (no committed `node_modules`).
- `Makefile` — `protocol-ts-types-gen` / `protocol-ts-types-gen-check` targets + help + `.PHONY`.
- `scripts/smoke/phase-135.sh` — the gate.
- `docs/skills/use-the-harbor-protocol/SKILL.md` — §18 same-PR update.
- `docs/site/protocol/build-a-client.md` — the "doors up" section.
- `docs/decisions.md` — D-269.
- `docs/plans/README.md` — index row + detail block.
- `docs/glossary.md` — "external-client TS types module".

## Public API surface

- New build tool `cmd/harbor-protocol-ts-types` (operator/CI-facing, not a Go
  importable API).
- New make targets `protocol-ts-types-gen` / `protocol-ts-types-gen-check`.
- New committed, copy-vendorable artifact
  `examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts`.
- No new Go package API; no Protocol surface change.

## Test plan

- **Unit:** `cmd/harbor-protocol-ts-types` lockstep test (type-instance index
  ↔ `CanonicalWireTypes`), canonical-surface coverage (every method / error /
  event / type present, none stale), field-shape mapping on known types
  (optional scalar, nested ref, `string[]`, `Record<string, …>`),
  determinism (two builds byte-identical), committed-file-in-sync, render
  shape.
- **Integration:** the smoke's live leg runs `event-viewer-ts` against the
  booted dev runtime (real Protocol server, real dev token, real
  `runtime.info`) — the cross-subsystem round-trip proving the generated
  types match the live wire (§17.8 real-spec posture: the fixture IS the live
  runtime, not a hand-authored lookalike).
- **Conformance:** N/A — no multi-driver subsystem; the generator is a single
  build tool. The in-sync + coverage tests are the conformance-shaped guard.
- **Concurrency / leak:** N/A — the generator is a one-shot build tool with no
  long-lived component, no shared mutable artifact, and no goroutines. (The
  §11 concurrent-reuse rule targets reusable runtime artifacts; a CLI codegen
  tool is out of scope.)

## Smoke script additions

- `scripts/smoke/phase-135.sh` asserts: the generated module + client + package
  files exist; the module carries the DO-NOT-EDIT banner + a known interface +
  a known union; the client vendors the generated module and is Harbor-import-
  free with no committed `node_modules`; a fresh regeneration is byte-identical
  to the committed module; the Go lockstep tests pass; the D-223 wire manifest
  is byte-unchanged; and (live) `event-viewer-ts` probe-mode completes a
  `runtime.info` round-trip. 404 / no-node / no-strip-types → SKIP.

## Coverage target

- `cmd/harbor-protocol-ts-types`: 80% (the reflection + render + scan paths are
  fully exercised by the lockstep tests).

## Dependencies

- 113b (the `examples/protocol-clients/` worked-client pattern + the Go
  `event-viewer` this mirrors).
- 118 / D-223 (the manifest generator + reserved name this is deliberately
  distinct from and must not disturb).

## Risks / open questions

- **Open question (resolved):** full vs thin generator, and the reserved name.
  Resolved per the wave coordination doc — ship a DISTINCT
  `cmd/harbor-protocol-ts-types` (external-client types only); the
  `cmd/harbor-gen-protocol-ts` name stays reserved for the full Console
  generator; the D-223 gate is untouched; D-132 is partially retired (D-269).
- **Node version for the example.** `event-viewer-ts` runs under Node's
  `--experimental-strip-types` (Node ≥ 22.6) or any TypeScript runner (`tsx`);
  the smoke SKIPs its live leg when neither is available, so the gate is robust
  across CI node versions.
- **Index duplication.** The generator carries its own type-instance index
  (the sanctioned §4.4-adjacent duplication the docs + manifest generators
  already use); the in-package lockstep test prevents drift from the single
  source.

## Glossary additions

- **external-client TS types module** — added to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no identity-scoped storage path; build tool + read-only client).
- [ ] **Reusable-artifact concurrent-reuse test** — N/A: a one-shot CLI codegen tool builds no long-lived reusable runtime artifact (§5/§11 target engines/tools/planners/drivers/clients, not build tools).
- [ ] **Integration test** — the smoke's live leg runs `event-viewer-ts` against the booted dev runtime end-to-end (real Protocol server), covering the consume-the-generated-types round-trip; a failure mode (server unreachable) SKIPs by the 404 convention.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departure).
