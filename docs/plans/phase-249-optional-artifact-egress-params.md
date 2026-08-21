# Phase 249 — Optional MCP artifact-egress mapping parameters (HA-67)

## Summary

Extend the existing MCP artifact-egress mapping with a per-parameter optional
marker. A trailing `?` is parsed and removed by `artifactegress.CompileMapping`,
so the remote schema and every operator-facing parameter list keep the bare
name. Required mappings retain their existing refusal behavior; optional
omissions (missing key or JSON `null`) skip substitution while present values
still pass the existing type, empty-id, resolver, digest, and byte-ceiling
checks.

## RFC anchor

- RFC §6.4
- RFC §6.10
- RFC §7

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- brief 03 §4: argument validation and tool behavior remain runtime-owned at
  the catalog/dispatch edge; the marker does not create a provider-specific
  or remote-server-controlled capability.
- brief 03 §8: artifact references are the heavy-content boundary between
  tools and the artifact store; optionality changes only whether a supplied
  reference is present, never how bytes are resolved or delivered.
- brief 07 §4: dispatch validates the call shape before execution and fails
  the invalid branch without issuing a partial wire request; present optional
  values therefore use the existing strict path.
- brief 07 §5: the runtime keeps the dispatch-local resolved value separate
  from observations and trajectory content; this phase does not alter that
  carrier or its projections.

## Findings I'm departing from (if any)

None.

## Goals

- Accept `?` as a per-parameter optional marker in the existing flat
  `map[string][]string` mapping.
- Preserve required mappings byte-for-byte in behavior and error taxonomy.
- Skip optional missing or `nil` values without requiring a resolver, while
  refusing explicit empty ids and non-string values exactly as today.
- Keep `ParamsFor` and MCP attach/schema errors on bare parameter names.
- Prove the contract through unit, MCP-driver, and concurrent-reuse tests and
  document the config spelling for operators.

## Non-goals

- No new wire type, nesting, Protocol method, transport, or artifact resolver.
- No change to digesting, base64 encoding, size ceilings, audit records,
  schema requirements, or identity-scoped artifact resolution for supplied
  values.
- No auto-inference of optionality from a remote server schema and no third
  mapping mode.

## Acceptance criteria

- [ ] `CompileMapping` strips one trailing `?`, records the bare name, and
      preserves deterministic parameter ordering.
- [ ] `CompileMapping` rejects an empty tool, an empty parameter, `?` as an
      empty optional parameter, and duplicate bare names including `x` + `x?`.
- [ ] `ParamsFor` returns defensive copies of bare names only; MCP attach-time
      schema checks therefore address the server's declared property.
- [ ] A required mapping still refuses an absent or `nil` argument with
      `ErrMappedArgumentMissing`; supplied values retain all existing refusal
      and resolution behavior.
- [ ] An optional mapping skips an absent or `nil` argument, including when
      every mapped parameter is optional and no resolver is seated.
- [ ] An optional present empty string returns `ErrEmptyArtifactID`, a
      non-string returns `ErrMappedArgumentNotString`, and a valid id resolves
      to a `Payload` plus the existing content-free `Record`.
- [ ] A real in-memory MCP driver call proves zero-image/omitted optional
      calls proceed unchanged and supplied optional slots reach the wire via
      the existing base64 substitution path.
- [ ] Shared compiled mappings survive 128 concurrent invocations under
      `-race` without cross-call argument or record contamination.
- [ ] The example config and operator skill document the trailing-marker
      spelling and the missing-vs-empty distinction.

## Files added or changed

- `internal/tools/artifactegress/artifactegress.go` — compiled optional
  parameter metadata, bare-name projection, and lazy resolver/substitution
  handling.
- `internal/tools/artifactegress/artifactegress_test.go` — marker parsing,
  refusal, skip, wire-record, and concurrent-reuse coverage.
- `internal/tools/drivers/mcp/egress_test.go` — real MCP transport conformance
  for omitted and supplied optional slots.
- `docs/plans/phase-249-optional-artifact-egress-params.md`
- `scripts/smoke/phase-249.sh`
- `examples/harbor.yaml`
- `docs/skills/add-an-in-process-tool/SKILL.md`

## Public API surface

No new Protocol or exported SDK method. The existing internal mapping contract
accepts a trailing `?` marker in its parameter strings; `Mapping.ParamsFor`
continues to return `[]string` of bare server-schema names.

## Test plan

- **Unit:** compile marker parsing/stripping, duplicate and empty-name
  refusals, deterministic bare-name projection, required/optional Encode
  matrix, resolver laziness, and unchanged payload/record behavior.
- **Integration:** the existing in-package MCP adapter test uses a real
  `go-sdk/mcp` in-memory server/client transport and the real event bus;
  identity is carried on the invocation context, omission is exercised on a
  resolver-less callback-shaped context, and supplied values cover the
  existing content-free record plus wire-base64 path.
- **Conformance:** MCP attach-time schema validation consumes bare names and
  accepts an optional marker only when the server declares the bare string
  property; required mappings still fail on omission.
- **Concurrency / leak:** 128 calls share one immutable compiled Mapping under
  `-race`; each call owns its argument map and asserts its own substitution
  count and payloads.

## Smoke script additions

- Static guards assert the Phase 249 plan, marker parser, optional skip branch,
  bare-name projection, unit/driver tests, and operator/config documentation
  are present. No new server endpoint exists, so no live HTTP probe is added.

## Coverage target

- `internal/tools/artifactegress`: ≥90%.
- `internal/tools/drivers/mcp`: existing package target; the new test must be
  included in the focused package run.

## Dependencies

- Phase 214 (MCP pass-by-reference artifact egress).

## Risks / open questions

- A remote property whose literal name ends in `?` is not addressable through
  this flat mapping spelling; `?` is intentionally reserved as the optional
  marker rather than introducing a nested wire shape.
- The marker remains in persisted/config input and is interpreted at the
  shared CompileMapping boundary; attach/schema projections never expose it.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes (hosted/release gate; not run in this focused
      worktree).
- [ ] `make preflight` passes (authoritative hosted web CI gate; explicitly
      not run locally for this task).
- [x] `make check-mirror` passes (no rule-file edits in this phase).
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve by inspection.
- [ ] Coverage on touched packages ≥ stated target (run focused coverage gate).
- [x] Multi-isolation behavior is unchanged; the existing run-scoped resolver
      remains the only resolution source.
- [x] Concurrent-reuse test covers the shared compiled Mapping with N=128
      under `-race`.
- [x] Integration adapter test covers the existing MCP/identity/bus seam and
      a failure mode (resolver-less required/optional distinction).
- [x] No new vocabulary requires a glossary entry.
- [x] No brief finding was departed from.
