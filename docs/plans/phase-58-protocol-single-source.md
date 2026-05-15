# Phase 58 — protocol-single-source

## Summary

Phase 58 formalises the Harbor Protocol single-source discipline that
Phase 54 (D-072) laid the foundation for: `internal/protocol/methods`,
`internal/protocol/errors`, and `internal/protocol/types` are the *only*
definition sites for Protocol method names, error codes, and wire types.
It ships a `go/parser` AST-walking checker (`internal/protocol/
singlesource`) plus a build-gating `go test` that fails the moment a
hardcoded Protocol method string, a Protocol error-code constant, or a
redeclared Protocol wire type appears anywhere else under
`internal/protocol/`. The checker is the Phase-58 enforcement Phase 54
deliberately deferred — Phase 54 built the layout correctly so Phase 58
is the lint, not a cleanup.

## RFC anchor

- RFC §5.1
- RFC §5.2
- RFC §5.3

<!-- The master-plan Phase 58 row cites "§5, §8". RFC-001 has no §8;
     "§8" is CLAUDE.md §8 ("Harbor Protocol rules") — the binding
     operational spec for this phase (single-source method names / error
     codes / wire types). RFC §5 is the design anchor; CLAUDE.md §8 is
     the rule this phase mechanically enforces. This is recorded as a
     §4.3 documentation note in D-075, not a plan departure. -->

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- **brief 07 "the runtime owns the protocol it speaks":** brief 07's
  keystone is that Harbor owns its planner/LLM protocol rather than
  inheriting a provider's. The same discipline applies to the
  client-facing Protocol — the method names, wire types, and error codes
  are Harbor-owned and single-sourced. Phase 58 makes "single-sourced" a
  *mechanically enforced* property: a `go test` walks the Protocol tree
  and fails on a second definition site, so the discipline cannot erode
  silently as later Protocol surfaces (Phases 59–62) land.
- **brief 06 §1 "protocol-grade event bus" + the decoupling rule:**
  "Console, third-party consoles, and `harbor dev` see exactly the same
  data shape." A stable client-facing contract requires the contract to
  have exactly one definition site — a method string duplicated in a
  handler, an error code re-declared in a sibling package, or a wire
  struct redefined elsewhere all fork the contract. The Phase 58 checker
  is the gate that keeps the contract single — the same kind of
  AST-walking lint `internal/planner/conformance/importgraph_test.go`
  already uses to gate the §13 planner-does-not-import-runtime invariant.
- **brief 06 §"Wire format" open question:** the wire transport is still
  being chosen (Phase 60); Phase 58 is transport-agnostic — it lints the
  *definitions*, not any transport binding. The checker walks
  `internal/protocol/` and is correct whether or not `transports/`
  exists yet.

## Findings I'm departing from (if any)

None. Phase 58 is purely additive enforcement over the layout Phase 54
already shipped per D-072 §1 ("Phase 58 is then a no-op formalisation,
not a cleanup"). The only nuance is the master-plan "§8" citation
resolving to CLAUDE.md §8 rather than an RFC section — documented above
and in D-075 as a citation note, not a design departure.

## Goals

- Ship `internal/protocol/singlesource` — a reusable `go/parser`-based
  checker (`ScanProtocolTree`) that returns every single-source
  Violation in the `internal/protocol/` tree: hardcoded Protocol method
  strings outside `methods/`, Protocol error-code constants outside
  `errors/`, redeclared canonical Protocol wire types outside their home
  package.
- Ship the build-gating `go test` (`TestSingleSource_ProtocolTreeIsClean`)
  that fails CI + preflight when the tree carries any violation.
- Consolidate the one pre-existing drift the checker surfaces:
  `internal/protocol/control.go` and two `_test.go` files hardcoded the
  `"start"` / `"cancel"` / `"pause"` method wire strings — re-derive
  them from the `methods` package constants.
- Pin the checker's canonical-set duplication (`CanonicalMethods`,
  `CanonicalWireTypes`) in lockstep with the `methods` / `types` /
  `errors` packages via tests, so a new Protocol method or wire type
  cannot land without the checker noticing.
- Leave the `methods` / `errors` / `types` packages' *content*
  unchanged — Phase 54 already defined them correctly. Phase 58 adds the
  enforcement, not new definitions.

## Non-goals

- **A custom `golangci-lint` analyzer.** A golangci plugin needs a
  separate build + a `.golangci.yml` entry (a new linter needs a PR
  rationale per CLAUDE.md §5). The repo's established pattern for an
  AST-level invariant lint is a `go/parser` `go test`
  (`importgraph_test.go`) — Phase 58 reuses it: zero external-tool
  dependency, runs under `go test` + CI + preflight like every other
  test.
- **Linting the whole `internal/` tree for method strings.** Strings
  like `"cancel"` / `"pause"` / `"reject"` are legitimate, unrelated
  domain vocabulary in other subsystems (`tasks/groups.go`'s
  `GroupAction`, `runtime/registry`'s agent commands,
  `planner/trajectory`'s entry kinds). CLAUDE.md §8's "no hardcoded
  method strings elsewhere" is scoped to Protocol-surface code; the
  checker walks `internal/protocol/` only. A cross-tree scan would be
  all false positives.
- **New Protocol methods / error codes / wire types.** Phase 58 adds no
  Protocol surface — it enforces the existing one. New surface lands in
  its own phase (59 versioning, 60 transport, 61 auth, 62 conformance).
- **The wire transport.** Phase 60. The checker is transport-agnostic.
- **A §4.4 driver seam.** The checker is a pure function over a
  filesystem root — no plausible alternate backend, no `drivers/` tree.

## Acceptance criteria

- [ ] `internal/protocol/singlesource` ships `ScanProtocolTree(root)
  ([]Violation, error)` — a `go/parser` AST walk returning every
  single-source breach in the tree, deterministically sorted.
- [ ] `TestSingleSource_ProtocolTreeIsClean` passes — the live
  `internal/protocol/` tree carries zero violations.
- [ ] The checker catches a hardcoded Protocol method string outside
  `internal/protocol/methods` (proven against a synthetic tree).
- [ ] The checker catches a Protocol error-code constant declared
  outside `internal/protocol/errors` (proven against a synthetic tree).
- [ ] The checker catches a canonical Protocol wire type redeclared
  outside its home package (proven against a synthetic tree).
- [ ] The checker does NOT flag the canonical definition sites
  themselves, nor a comment / struct-tag / substring mention of a method
  name (no false positives — proven against synthetic trees).
- [ ] The checker lints `_test.go` files too (a method string hardcoded
  in a test is the same drift — matches the `importgraph_test.go`
  precedent).
- [ ] `CanonicalMethods` / `CanonicalWireTypes` are pinned in lockstep
  with `internal/protocol/methods` + `internal/protocol/types` +
  `internal/protocol/errors` by tests — a new method / wire type that
  lands without updating the checker fails CI.
- [ ] The pre-existing `"start"` / `"cancel"` / `"pause"` hardcoded
  method literals in `control.go` + `errors_internal_test.go` +
  `types/types_test.go` are re-derived from the `methods` constants;
  build + `go test -race ./internal/protocol/...` stays green.
- [ ] `scripts/smoke/phase-58.sh` runs the checker test + a static guard
  and passes.
- [ ] Coverage on `internal/protocol/singlesource` ≥ 90%.

## Files added or changed

```text
internal/protocol/
  singlesource/
    singlesource.go            # ScanProtocolTree + Violation + the AST checker
    singlesource_test.go       # the build-gating clean-tree lint + detection + lockstep tests
    internal_test.go           # in-package unit tests for the unexported predicates
  control.go                   # re-derive the "start" method literals from methods.MethodStart
  errors_internal_test.go      # re-derive the "cancel"/"start" test literals from methods constants
  types/
    types_test.go              # re-derive the "pause" test literal from methods.MethodPause
docs/plans/phase-58-protocol-single-source.md   # this plan
docs/plans/README.md           # Phase 58 row Pending -> Shipped
docs/decisions.md              # D-075
docs/glossary.md               # "Protocol single-source checker"
scripts/smoke/phase-58.sh      # runs the checker test + a static guard
README.md                      # Phase 58 status row
```

No new top-level directory — `internal/protocol/` is in CLAUDE.md §3;
`singlesource/` is a sub-package of it.

## Public API surface

```go
package singlesource

// Violation is a single single-source breach: file, line, kind, detail.
type Violation struct {
    File, Kind, Detail string
    Line               int
}
func (v Violation) String() string

const (
    KindMethodLiteral = "method-literal"
    KindErrorCode     = "error-code"
    KindWireType      = "wire-type"
)

// CanonicalMethods / CanonicalWireTypes — the single-sourced sets the
// checker gates against, pinned in lockstep with the canonical packages
// by the package's tests.
var CanonicalMethods   map[string]struct{}
var CanonicalWireTypes map[string]string // type name -> home package dir

// ScanProtocolTree walks the Go source tree at root (the internal/
// protocol directory) and returns every single-source Violation. A
// returned error means the walk itself failed; a Violation is a
// successful scan finding drift. Pure function, safe for concurrent use.
func ScanProtocolTree(protocolRoot string) ([]Violation, error)
```

## Test plan

- **Unit:** `singlesource_test.go` — `TestSingleSource_ProtocolTreeIsClean`
  (the binding lint: the live tree is clean);
  `TestSingleSource_ScannerReachesTheTree` (sanity gate — the walk
  inspects ≥12 files, so a moved/empty tree cannot silently pass);
  `TestSingleSource_DetectsMethodLiteral` /
  `…DetectsErrorCodeRedefinition` / `…DetectsWireTypeRedefinition` (each
  proves the checker catches its breach kind against a synthetic tree);
  `…AllowsCanonicalPackages` + `…NoFalsePositiveOnNonProtocolCode` (no
  false positives on the canonical homes, comments, struct tags, or
  substrings); `…DetectsMethodLiteralInTestFile` (test files are linted
  too); `…IgnoresNonGoFilesAndReportsSorted` (non-Go files skipped;
  deterministic sort); `…ScanErrorsOnUnparseableSource` (a parse failure
  surfaces loud, not a silent zero-violation pass);
  `…ViolationString`; `…CanonicalMethodsInLockstep` /
  `…CanonicalWireTypesInLockstep` (the checker's duplicated sets are
  pinned to the canonical packages). `internal_test.go` —
  `TestDirAllowsKind_AllBranches` + `TestIsProtocolErrorsCodeType_AllBranches`
  (the unexported predicates, every branch).
- **Integration:** N/A — Phase 58 is a build-time static checker over
  the `internal/protocol/` source tree; it consumes no shipped
  subsystem's *runtime* surface and opens no cross-subsystem seam. The
  checker's "integration" is exercising it against the real Protocol
  tree (`TestSingleSource_ProtocolTreeIsClean`), which the unit bucket
  already covers. Per CLAUDE.md §17.1, a phase whose only dependency is
  the skeleton-adjacent layout — and which wires no runtime drivers — is
  exempt.
- **Conformance:** N/A — no multi-driver subsystem.
- **Concurrency / leak:** N/A — `ScanProtocolTree` is a pure function
  with no package-level mutable state and starts no goroutines; it is
  not a "compiled artifact" in the D-025 sense (it holds no
  construction-time dependencies, runs no per-invocation goroutines).
  The function is trivially safe for concurrent calls; there is no
  reusable-artifact lifecycle to stress.

## Smoke script additions

- `scripts/smoke/phase-58.sh` runs
  `go test -race ./internal/protocol/singlesource/...` — the build-gating
  clean-tree lint + the detection + lockstep + no-false-positive tests.
- Static guard: `internal/protocol/singlesource/singlesource.go` exists
  (the checker is present).
- Static guard: re-asserts no Protocol method wire string is hardcoded
  under `internal/protocol/` outside `methods/` and `singlesource/` — a
  cheap grep backstop that catches a regression even if the Go test
  binary is somehow skipped.
- Phase 58 ships no HTTP / Protocol-wire surface — the wire transport is
  Phase 60. The wire assertions `skip` per the 404/405/501 → SKIP
  convention.

## Coverage target

- `internal/protocol/singlesource`: 90%

## Dependencies

- 01 — the identity package + the repo's Go module skeleton (the
  master-plan Phase 58 `Deps`). In practice Phase 58 also reads the
  Phase 54 Protocol layout it enforces — Phase 54 (D-072) is shipped, so
  the layout the checker gates already exists.

## Risks / open questions

- **The checker's `CanonicalMethods` set duplicates
  `internal/protocol/methods`.** The checker cannot import the `methods`
  package — it must be runnable against a tree where `methods/` itself
  is the thing under audit. The duplication is deliberate and pinned:
  `TestSingleSource_CanonicalMethodsInLockstep` fails the moment the two
  drift, which is exactly when a new Protocol method landed without
  updating the checker. Same for `CanonicalWireTypes` vs the `types` /
  `errors` packages.
- **Scope of the method-literal lint is `internal/protocol/` only, not
  all of `internal/`.** This is a deliberate, documented call (see
  Non-goals): method-name *strings* are legitimate unrelated vocabulary
  in other subsystems. CLAUDE.md §8's "no hardcoded method strings
  elsewhere" is read as "elsewhere in Protocol-surface code". If a later
  phase grows Protocol-method handling *outside* `internal/protocol/`
  (it should not — that would itself be a layering smell), the checker's
  root would need widening; recorded in D-075.
- **`go/parser` precision vs a grep.** The checker flags only real
  string-literal expressions / const declarations / type declarations —
  a method name in a comment, a doc string, or a struct tag is not a
  violation. A grep-based check would false-positive on all three; the
  AST walk is why the lint is precise.

## Glossary additions

- **Protocol single-source checker** — the `go/parser` AST-walking
  checker (`internal/protocol/singlesource`) that gates CLAUDE.md §8's
  single-source rule: Protocol method names, error codes, and wire types
  have exactly one definition site each. Added to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, Phase 58 is a static source checker; it touches no identity-scoped path.
- [ ] **If this phase builds a reusable artifact:** N/A — `ScanProtocolTree` is a pure function with no construction-time state and no per-invocation goroutines; it is not a D-025 compiled artifact. See Test plan "Concurrency / leak".
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** N/A — Phase 58 is a build-time static checker over the `internal/protocol/` source tree; it wires no runtime drivers and opens no seam. See Test plan "Integration".
- [ ] If new vocabulary: glossary updated — "Protocol single-source checker" added to `docs/glossary.md`.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — no departure; D-075 records the settled enforcement design.
