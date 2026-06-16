# Phase 118 — Protocol TS lockstep gate

## Summary

Build the `make protocol-ts-gen-check` gate D-093 specified, as a
field-level **lockstep VERIFICATION** of the hand-maintained Console
Protocol client against the Go single source — NOT a generator that
replaces the client (a §4.3 deviation from the plan-on-disk; see D-223).
A Go tool (`cmd/harbor-protocol-ts-lockstep`) reflects over
`internal/protocol/singlesource.CanonicalWireTypes` and emits a committed
wire manifest (`web/console/src/lib/protocol/wire-manifest.gen.json`); a
TS-source scan checks the hand-written per-page interfaces declare every
manifest type's fields; a Go lockstep test + a `git diff` staleness check
close the loop. This closes the standing drift risk — a Go-side wire-type
change that is not mirrored into the TS client now fails CI — while
keeping the correct per-page modular split the Console already has.

## RFC anchor

- RFC §5 — Harbor Protocol (the wire contract the client mirrors).
- RFC §5.3 — Versioning (the manifest pins the Protocol version).
- RFC §7 — Console layer (the Protocol client the Console consumes).

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §13: *"it guarantees Console, third-party consoles, and
  `harbor dev` see exactly the same data shape that production
  observability sees. There is no privileged 'internal' view."* — a
  mechanically-gated client guarantees the Console's types stay in
  lockstep with the Go wire source, not a hand-drifted approximation.
- brief 06 §9–11: the Protocol is the single stable contract across
  versions; the gate makes that contract mechanically enforced on the
  TypeScript side, the same way single-source enforcement does on the Go
  side.

## Findings I'm departing from (if any)

None at the brief level. The PLAN-on-disk assumed a single generated
`protocol.ts`; that assumption was stale (the wire types are correctly
split across ~18 per-page modules). This phase delivers the lockstep gate
as VERIFICATION (option A) rather than generation; the departure from
D-093's "generate" half is recorded in D-223.

## Goals

- `cmd/harbor-protocol-ts-lockstep` emits a committed JSON wire manifest
  from `singlesource.CanonicalWireTypes` (per type: JSON field keys,
  TS-type tokens, named-type refs, optionality) plus the method, error,
  and event name sets, headed with a `GENERATED … DO NOT EDIT` note.
- `make protocol-ts-gen` regenerates the manifest; `make
  protocol-ts-gen-check` runs the three-half gate (manifest `git diff`,
  Go lockstep test, TS-source scan) and is wired into CI alongside
  `protocol-docs-gen-check`.
- A TS-source scan (`web/console/scripts/check-protocol-ts-lockstep.mjs`,
  in `npm run lint`) fails when a hand-written interface drops/renames/
  mistypes a manifest field, or when a new wire type lands without a TS
  interface or a justified untyped-allowlist entry.

## Non-goals

- Changing any wire type — pure verification of the existing surface.
- Generating the per-domain TypeScript type modules (the deferred future
  phase "B"; the `cmd/harbor-gen-protocol-ts` name is reserved for it).
- Generating clients for other languages.

## Acceptance criteria

- [ ] The manifest generator emits a deterministic, `GENERATED`-headed
      `wire-manifest.gen.json` covering all `CanonicalWireTypes` + methods
      + errors + events.
- [ ] `make protocol-ts-gen-check` passes on a clean tree and FAILS when a
      Go wire type/field/method/error/event changes without a regenerated
      manifest, AND when a hand-written TS interface drifts from a manifest
      field (proven by planted drift).
- [ ] The manifest carries a `GENERATED … DO NOT EDIT` note; hand-edits
      are rejected on sight (CLAUDE.md §4.5, §13).
- [ ] A new canonical method / error code / event type / wire type without
      its manifest coverage fails the Go lockstep test.
- [ ] The TS scan + Go lockstep test are green against the CURRENT
      hand-written TS (pre-existing Go↔TS drift reconciled, §17.6).

## Files added or changed

- `cmd/harbor-protocol-ts-lockstep/` — the manifest generator + lockstep
  tests (the `cmd/harbor-gen-protocol-ts` name stays reserved for "B").
- `web/console/src/lib/protocol/wire-manifest.gen.json` — the committed,
  generated wire manifest.
- `web/console/scripts/check-protocol-ts-lockstep.mjs` — the TS-source
  scan (wired into `npm run lint`).
- `web/console/scripts/protocol-ts-untyped-allow.json` — the justified
  untyped-type allowlist.
- `web/console/src/lib/protocol/tests/protocol-ts-lockstep.spec.ts` — the
  vitest mirror of the scan.
- Several `web/console/src/lib/**` wire-type modules — pre-existing
  Go↔TS drift fixes (§17.6).
- `Makefile` — `protocol-ts-gen` + `protocol-ts-gen-check`.
- `.github/workflows/docs.yml` — the CI gate step.
- `scripts/smoke/phase-118.sh`.
- CLAUDE.md / AGENTS.md §4.5 rule 5 — the lockstep-gated wording (verbatim
  mirror); `web/console/src/lib/protocol.ts` header reworded.

## Public API surface

- `cmd/harbor-protocol-ts-lockstep` (a build tool). No runtime Go surface.

## Test plan

- **Unit:** the Go lockstep test pins the manifest against
  `CanonicalWireTypes` / methods / errors / events + a field-shape
  sanity check; the vitest spec asserts the scan reports zero violations
  and the manifest carries the DO-NOT-EDIT banner.
- **Integration:** `make protocol-ts-gen-check` is the end-to-end gate;
  planted Go-side and TS-side drift each fail it.
- **Conformance:** N/A — mirrors the existing canonical surface.
- **Concurrency / leak:** N/A (build tool).

## Smoke script additions

- `scripts/smoke/phase-118.sh` (static-only): the manifest exists and is
  `GENERATED`-headed; the `protocol-ts-gen-check` Make target exists; the
  lockstep tool builds.

## Coverage target

- `cmd/harbor-protocol-ts-lockstep`: ≥ 80%.

## Dependencies

- Phase 113a (`cmd/harbor-gen-protocol-docs` — the sibling generator +
  `protocol-docs-gen-check` pattern this mirrors; same single source).

## Risks / open questions

- Mapping Go type shapes (`time.Time`, `[]byte`, `json.RawMessage`,
  string-enum named types) to canonical TS-type tokens — follow the
  `harbor-gen-protocol-docs` precedent; the field-token check is
  best-effort with a documented residual (an in-place field-type swap
  without a rename).
- The untyped allowlist must stay honest — a new Go wire type that the
  Console does not type must be a CONSCIOUS allowlist add, not a reflex;
  the scan's stale-entry check backs this.
- Full §16 brief pass (brief 06 + D-093) when dispatched; the
  `use-the-harbor-protocol` skill is unaffected (no operator-facing client
  shape changed).

## Glossary additions

- None — `CanonicalWireTypes` / single-source already in glossary; "wire
  manifest" is described inline in the manifest header + D-223.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (the §4.5 rule reword lands in both files)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A
- [ ] Concurrent-reuse test — N/A (build tool, no reusable runtime artifact)
- [ ] Integration test — the `protocol-ts-gen-check` gate IS the integration guard
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified + decisions.md entry (D-223)
