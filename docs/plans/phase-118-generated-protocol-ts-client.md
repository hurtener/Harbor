# Phase 118 — generated typed Protocol TS client

## Summary

Build the `cmd/harbor-gen-protocol-ts` generator and the
`make protocol-ts-gen-check` CI gate that D-093 specified but never
shipped, so `web/console/src/lib/protocol.ts` is regenerated from the Go
single source (`internal/protocol/singlesource.CanonicalWireTypes`)
instead of hand-maintained. This closes the standing drift risk: today a
Go-side wire-type change must be mirrored into `protocol.ts` by hand, and
the file carries an accurate "hand-maintained" notice as a stopgap.

## RFC anchor

- RFC §5 — Harbor Protocol (the wire contract the client mirrors).
- RFC §5.3 — Versioning (the generated client tracks the pinned version).
- RFC §7 — Console layer (the Protocol client the Console consumes).

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §13: *"it guarantees Console, third-party consoles, and
  `harbor dev` see exactly the same data shape that production
  observability sees. There is no privileged 'internal' view."* — a
  generated client guarantees the Console's types are byte-identical to
  the Go wire source, not a hand-drifted approximation.
- brief 06 §9–11: the event bus / Protocol is the single stable contract
  across versions; a generated client makes that contract mechanically
  enforced on the TypeScript side, the same way the Go side is the source.

## Findings I'm departing from (if any)

None — this implements D-093 as written.

## Goals

- `cmd/harbor-gen-protocol-ts` emits `web/console/src/lib/protocol.ts`
  from `singlesource.CanonicalWireTypes` (types, method names, error
  codes, event payload keys), headed `CODE GENERATED … DO NOT EDIT`.
- `make protocol-ts-gen-check` regenerates and `git diff --exit-code`s,
  wired into the CI docs/frontend workflow — a Go-side wire change without
  a regenerated `protocol.ts` fails CI.
- The hand-maintained `protocol.ts` is replaced by the generated output;
  the CLAUDE.md §4.5 rule reverts to "generated, never hand-edited."

## Non-goals

- Changing any wire type — pure generation of the existing surface.
- Generating clients for other languages (Go SDK already exists; others
  are out of scope).

## Acceptance criteria

- [ ] The generator produces a `protocol.ts` that `svelte-check` accepts
      and the Console compiles against unchanged (semantically).
- [ ] `make protocol-ts-gen-check` passes on a clean tree and FAILS when a
      Go wire type changes without regeneration (lockstep test).
- [ ] The generated file carries the `CODE GENERATED … DO NOT EDIT`
      header; hand-edits are rejected on sight (CLAUDE.md §4.5).
- [ ] A new method / error code / event type / wire type without its
      generated counterpart fails the generator's lockstep test.

## Files added or changed

- `cmd/harbor-gen-protocol-ts/` — the generator.
- `web/console/src/lib/protocol.ts` — becomes generated output.
- `Makefile` — `protocol-ts-gen` + `protocol-ts-gen-check` targets.
- `.github/workflows/` — the gate in the frontend/docs job.
- `scripts/smoke/phase-118.sh`.
- CLAUDE.md / AGENTS.md §4.5 rule 5 — revert to "generated, never
  hand-edited" (verbatim mirror).

## Public API surface

- `cmd/harbor-gen-protocol-ts` (a build tool). No runtime Go surface.

## Test plan

- **Unit:** generator lockstep — a fixture wire type round-trips to the
  expected TS; a new canonical type with no emission fails.
- **Integration:** `make protocol-ts-gen-check` on a mutated wire type
  fails (the gate works end-to-end).
- **Conformance:** N/A — mirrors the existing conformance surface.
- **Concurrency / leak:** N/A (build tool).

## Smoke script additions

- `scripts/smoke/phase-118.sh` (static-only): the generated `protocol.ts`
  carries the `CODE GENERATED` header; `cmd/harbor-gen-protocol-ts` builds.

## Coverage target

- `cmd/harbor-gen-protocol-ts`: ≥ 80%.

## Dependencies

- Phase 113a (`cmd/harbor-gen-protocol-docs` — the sibling generator +
  `protocol-docs-gen-check` pattern this mirrors; same single source).

## Risks / open questions

- Mapping Go type shapes (embedded structs, `omitempty`, enums-as-strings)
  to idiomatic TS — follow the `harbor-gen-protocol-docs` precedent.
- Sequencing vs the hand-extensions `protocol.ts` accumulated — the first
  generated output must be diffed against the current hand file and any
  intentional gap reconciled before the gate turns on.
- Full §16 brief pass (brief 06 + D-093) when dispatched; updates the
  `use-the-harbor-protocol` skill if the client shape an operator follows
  changes (CLAUDE.md §18).

## Glossary additions

- None — `CanonicalWireTypes` / single-source already in glossary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (the §4.5 rule revert lands in both files)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A
- [ ] Concurrent-reuse test — N/A (build tool, no reusable runtime artifact)
- [ ] Integration test — the `protocol-ts-gen-check` gate IS the integration guard
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified + decisions.md entry
