# Phase 59 — protocol-versioning

## Summary

Phase 59 turns the Harbor Protocol version *pin* (the `ProtocolVersion`
string Phase 54 / D-072 §1 placed in `internal/protocol/types/version.go`)
into a *versioning discipline*: a parsed, comparable `Version` value with
a same-major `Compatible` check so a client can mechanically detect
version skew; a settled `Deprecation` note format with a typed
mechanism so a deprecated Protocol element carries its removal window in
a structured shape rather than a free-text comment; and a capability
set + `VersionHandshake` wire shape so a Protocol client and the Runtime
can negotiate which Protocol surfaces are live. It adds **no** version
bump (that stays an RFC change) — it establishes the *mechanism and the
discipline*, all in the single canonical home `internal/protocol/types`.

## RFC anchor

- RFC §5.3

<!-- The master-plan Phase 59 row cites "§5.3". RFC-001-Harbor.md §5.3
     ("Versioning") is the design anchor and resolves. CLAUDE.md §8
     ("Harbor Protocol rules") is the binding operational spec this phase
     mechanises — "The Protocol version is pinned in
     internal/protocol/types/version.go. Bumping the version is an RFC
     change... Breaking changes require a deprecation window so
     third-party consoles aren't whipsawed." RFC §5.2 (the surface table
     the capability set mirrors) is referenced in prose but not listed as
     an anchor heading since §5.3 is the phase's home section. -->

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- **brief 07 "the runtime owns the protocol it speaks":** brief 07's
  keystone — Harbor owns its protocol rather than inheriting a
  provider's — applies directly to the client-facing Protocol's version
  story. Phase 59 makes the version a *Harbor-owned, structured*
  artifact: a parsed `Version` (Major/Minor/Patch) with an explicit
  same-major `Compatible` rule, not a bare string a client must
  string-compare. The Runtime decides what "compatible" means; the
  client asks, it does not guess.
- **brief 06 §1 "protocol-grade event bus" + the decoupling rule
  ("Console, third-party consoles, and `harbor dev` see exactly the
  same data shape"):** a stable client-facing contract needs a
  *negotiable* surface — a third-party Console built against Protocol
  `0.1.0` must be able to ask the Runtime "which surfaces are live?" and
  get a structured answer, not discover a missing surface by a 404.
  Phase 59 ships the `Capability` set + `VersionHandshake` so the
  negotiation is explicit and single-sourced.
- **brief 06 §"Wire format" open question:** the wire transport is still
  Phase 60. Phase 59 is transport-agnostic — `Version`, `Deprecation`,
  `VersionHandshake`, and `Capabilities()` are plain Go values + wire
  structs in `internal/protocol/types`; a Phase 60 SSE+REST adapter (or
  a `harbor version` subcommand, Phase 63) consumes them later. Phase 59
  binds to no transport.

## Findings I'm departing from (if any)

None. Phase 59 is additive discipline over the layout Phase 54 shipped
per D-072 §1 and the single-source enforcement Phase 58 added per D-075.
The one nuance — Phase 59 adds three new exported wire structs to
`internal/protocol/types`, so `singlesource.CanonicalWireTypes` (the
Phase 58 checker's lockstep map) must record them under home `types` in
the same PR — is the CLAUDE.md §17.6 "fix what the lint finds"
discipline, not a departure: the Phase 58 lockstep test
(`TestSingleSource_CanonicalWireTypesInLockstep`) is *designed* to fail
when a new wire type lands without updating the checker.

## Goals

- Ship a parsed, comparable `Version` value in
  `internal/protocol/types/version.go` — `ParseVersion`, the
  `Major`/`Minor`/`Patch` fields, `String`, `Compare`, and the
  `Compatible` same-major rule — so a client detects version skew
  mechanically. `CurrentVersion` is the parsed form of the existing
  `ProtocolVersion` string constant (which stays, unchanged: it is the
  RFC-change trip-wire Phase 54 pinned).
- Ship a settled `Deprecation` note format — a typed struct
  (`Subject`, `Kind`, `DeprecatedIn`, `RemovedIn`, `Replacement`,
  `Note`) plus the discipline doc — so a deprecated Protocol element
  carries its removal window structurally. Ship a (currently empty)
  `Deprecations()` registry so the format has a single home and a
  consumer the moment the first deprecation lands.
- Ship the `Capability` enum + `Capabilities()` set + the
  `VersionHandshake` wire struct — the capability-negotiation shape a
  Protocol client uses to ask the Runtime which surfaces are live. V1
  advertises exactly the surfaces that have shipped (`task_control` —
  the Phase 54 surface); later Protocol-surface phases add their
  capability constant here as they land.
- Keep the change inside `internal/protocol/types` (CLAUDE.md §8: the
  version is pinned there). No second definition site; the Phase 58
  single-source checker stays green (with its lockstep map updated for
  the three new wire structs).

## Non-goals

- **A version bump.** `ProtocolVersion` stays `0.1.0`. CLAUDE.md §8 +
  RFC §5.3: "Bumping the version is an RFC change." Phase 59 ships the
  *mechanism* for living with versions, not a new version.
- **The wire transport / a `harbor version` subcommand.** The
  master-plan acceptance line says the version constant is returned on
  `harbor version` "after phase 63" — a forward reference. Phase 59
  ships the transport-agnostic values; Phase 60 (wire transport) and
  Phase 63 (`harbor` CLI) consume them. Phase 59 binds to no transport
  and adds no CLI subcommand.
- **An actual deprecation.** Phase 59 ships the `Deprecation` *format*
  and an empty `Deprecations()` registry — there is nothing to deprecate
  in a `0.1.0` Protocol that just shipped. The first real deprecation
  lands in the phase that supersedes a Protocol element, populating the
  registry the format already defines.
- **Capability *enforcement*.** Phase 59 ships the capability *set* and
  the *negotiation shape* (`VersionHandshake`). A handler that rejects a
  request for an un-advertised capability is the Phase 60/61 transport +
  auth surface's job — Phase 59 gives them the vocabulary.
- **A §4.4 driver seam.** `Version` / `Deprecation` / `Capability` are
  value types + pure functions in the canonical `types` package — no
  plausible alternate backend, no `drivers/` tree. Same call D-072 §5
  and D-075 made for the Protocol layer's other surfaces.

## Acceptance criteria

- [ ] `internal/protocol/types/version.go` ships `Version` (a
  `Major`/`Minor`/`Patch` struct), `ParseVersion(string) (Version,
  error)`, `Version.String`, `Version.Compare(Version) int`, and
  `Version.Compatible(Version) bool` (same-major). `CurrentVersion` is
  the parsed `ProtocolVersion`; a test pins `CurrentVersion.String() ==
  ProtocolVersion`.
- [ ] `ProtocolVersion` stays the string constant `"0.1.0"` — the
  existing `TestProtocolVersion_Pinned` trip-wire still passes (a bump
  is an RFC change).
- [ ] `ParseVersion` rejects a malformed version loudly with a wrapped
  `ErrInvalidVersion` (no silent zero-Version return — CLAUDE.md §5
  fail-loudly).
- [ ] `Deprecation` ships as a typed struct with the settled fields
  (`Subject`, `Kind`, `DeprecatedIn`, `RemovedIn`, `Replacement`,
  `Note`); `DeprecationKind` is a string enum (`method` / `error_code` /
  `wire_field` / `capability`); `Deprecation.String` renders the
  settled human-readable note format; `Deprecation.Validate` rejects a
  malformed entry (empty subject, invalid `Kind`, `RemovedIn` not after
  `DeprecatedIn`) loudly.
- [ ] `Deprecations()` returns the (currently empty) deterministic
  registry of active Protocol deprecations — the single home the format
  has a consumer in.
- [ ] `Capability` is a string enum; `CapTaskControl` is the one V1
  capability (the Phase 54 surface); `Capabilities()` returns the
  deterministic sorted set; `IsValidCapability` is O(1).
- [ ] `VersionHandshake` ships as a wire struct (`ProtocolVersion
  string` + `Capabilities []Capability`); `CurrentHandshake()` builds
  the Runtime's handshake from `ProtocolVersion` + `Capabilities()`;
  `VersionHandshake.Accepts(Capability) bool` reports whether a
  capability is advertised; round-trips through JSON.
- [ ] `singlesource.CanonicalWireTypes` records the three new exported
  wire structs (`Version`, `Deprecation`, `VersionHandshake`) under home
  `types`; `TestSingleSource_CanonicalWireTypesInLockstep` and
  `TestSingleSource_ProtocolTreeIsClean` stay green.
- [ ] `scripts/smoke/phase-59.sh` runs the versioning unit tests + a
  static guard that `ProtocolVersion` is single-sourced in
  `version.go`, and passes (the wire/HTTP assertions `skip` per the
  404/405/501 → SKIP convention — no wire surface until Phase 60).
- [ ] Coverage on `internal/protocol/types` ≥ 85%.

## Files added or changed

```text
internal/protocol/
  types/
    version.go                 # + Version/ParseVersion/Compatible, Deprecation/Deprecations,
                               #   Capability/Capabilities, VersionHandshake/CurrentHandshake
    version_test.go            # NEW — unit tests for the versioning discipline surface
  singlesource/
    singlesource.go            # CanonicalWireTypes += Version/Deprecation/VersionHandshake (home "types")
docs/plans/phase-59-protocol-versioning.md   # this plan
docs/plans/README.md           # Phase 59 row Pending -> Shipped
docs/decisions.md              # D-077
docs/glossary.md               # "Protocol version", "deprecation window", "capability negotiation"
scripts/smoke/phase-59.sh      # runs the versioning unit tests + a single-source static guard
README.md                      # Phase 59 status row
```

No new top-level directory — everything lands inside `internal/protocol/`
(CLAUDE.md §3). `version.go` is extended in place; `version_test.go` is a
new sibling test file in the same package.

## Public API surface

```go
package types

// --- version ---------------------------------------------------------
const ProtocolVersion = "0.1.0" // unchanged — the RFC-change trip-wire

type Version struct{ Major, Minor, Patch int }
func ParseVersion(s string) (Version, error)   // wraps ErrInvalidVersion on a bad string
var ErrInvalidVersion error
var CurrentVersion Version                     // = MustParseVersion(ProtocolVersion)
func (v Version) String() string
func (v Version) Compare(o Version) int        // -1 / 0 / +1
func (v Version) Compatible(o Version) bool    // same Major

// --- deprecation discipline -----------------------------------------
type DeprecationKind string
const (
    DeprecationMethod     DeprecationKind = "method"
    DeprecationErrorCode  DeprecationKind = "error_code"
    DeprecationWireField  DeprecationKind = "wire_field"
    DeprecationCapability DeprecationKind = "capability"
)
type Deprecation struct {
    Subject      string          `json:"subject"`
    Kind         DeprecationKind `json:"kind"`
    DeprecatedIn string          `json:"deprecated_in"`
    RemovedIn    string          `json:"removed_in"`
    Replacement  string          `json:"replacement,omitempty"`
    Note         string          `json:"note,omitempty"`
}
func (d Deprecation) Validate() error
func (d Deprecation) String() string           // the settled human-readable note format
func Deprecations() []Deprecation              // the active-deprecations registry (empty at 0.1.0)

// --- capability negotiation -----------------------------------------
type Capability string
const CapTaskControl Capability = "task_control"
func Capabilities() []Capability               // deterministic sorted set
func IsValidCapability(c Capability) bool
type VersionHandshake struct {
    ProtocolVersion string       `json:"protocol_version"`
    Capabilities    []Capability `json:"capabilities"`
}
func CurrentHandshake() VersionHandshake
func (h VersionHandshake) Accepts(c Capability) bool
```

## Test plan

- **Unit:** `version_test.go` —
  `TestParseVersion_RoundTrips` (every well-formed `M.N.P` parses and
  `String`-renders back);
  `TestParseVersion_RejectsMalformed` (empty, non-numeric, wrong
  arity, negative — each returns a wrapped `ErrInvalidVersion`, not a
  silent zero-Version);
  `TestCurrentVersion_MatchesProtocolVersion` (`CurrentVersion.String()
  == ProtocolVersion` — the parsed form and the pinned string never
  drift);
  `TestVersion_Compare_Ordering` (lexicographic-by-component ordering);
  `TestVersion_Compatible_SameMajor` (same major ⇒ compatible, different
  major ⇒ not);
  `TestDeprecation_Validate_RejectsMalformed` (empty subject, invalid
  `Kind`, `RemovedIn` ≤ `DeprecatedIn` — each fails loudly);
  `TestDeprecation_String_NoteFormat` (the settled note format renders
  with + without `Replacement`);
  `TestDeprecation_JSONRoundTrip`;
  `TestDeprecations_RegistryIsValidAndEmpty` (the registry is empty at
  `0.1.0` and — defensively — every entry that ever lands `Validate`s);
  `TestCapabilities_DeterministicAndValid` (sorted, every entry
  `IsValidCapability`, `task_control` present);
  `TestVersionHandshake_CurrentAndAccepts` (`CurrentHandshake` carries
  `ProtocolVersion` + `Capabilities()`; `Accepts` is true for an
  advertised capability, false otherwise);
  `TestVersionHandshake_JSONRoundTrip`. The existing
  `TestProtocolVersion_Pinned` (in `types_test.go`) stays the
  RFC-change trip-wire.
  The Phase 58 `internal/protocol/singlesource` suite
  (`TestSingleSource_ProtocolTreeIsClean` +
  `…CanonicalWireTypesInLockstep`) is re-run as part of the gate — it
  proves the three new wire structs are correctly registered and the
  tree stays single-source-clean.
- **Integration:** N/A — Phase 59 is purely additive value types + pure
  functions inside `internal/protocol/types`. It wires no runtime
  driver, opens no cross-subsystem seam, and consumes no shipped
  subsystem's *runtime* surface. The one thing it consumes is the
  *build-time* Phase 58 single-source checker's lockstep map — a
  static-source coupling, not a runtime seam — and the Phase 58 suite
  re-run is the test that proves the coupling holds. Per CLAUDE.md
  §17.1, a phase that wires no runtime drivers and opens no seam is
  exempt from the §17 integration bucket. (Master-plan Phase 59 `Deps`
  is `58`, but Phase 58 ships a build-time checker, not a runtime
  surface — the §17.1 trigger is "consumes a *different subsystem's*
  *runtime* surface".)
- **Conformance:** N/A — no multi-driver subsystem.
- **Concurrency / leak:** N/A — `Version`, `Deprecation`, `Capability`,
  and `VersionHandshake` are immutable value types; `ParseVersion`,
  `Compare`, `Compatible`, `Deprecations`, `Capabilities`,
  `CurrentHandshake`, `Accepts` are pure functions with no
  package-level mutable state and start no goroutines. None is a
  "compiled artifact" in the D-025 sense (no construction-time
  dependencies, no per-invocation goroutines). They are trivially safe
  for concurrent use; there is no reusable-artifact lifecycle to stress.

## Smoke script additions

- `scripts/smoke/phase-59.sh` runs
  `go test -race ./internal/protocol/types/...` (the versioning
  discipline unit tests + the existing control-wire-type tests) and
  `go test -race ./internal/protocol/singlesource/...` (proves the
  three new wire structs are registered in the checker's lockstep map
  and the tree stays single-source-clean).
- Static guard: `ProtocolVersion` appears as a `const` in exactly one
  file — `internal/protocol/types/version.go` — and nowhere else under
  `internal/protocol/` is it re-declared (a cheap grep backstop for the
  CLAUDE.md §8 single-source rule).
- Static guard: `internal/protocol/types/version.go` exists and
  declares the `Version`, `Deprecation`, and `VersionHandshake` types
  (the Phase 59 surface is present).
- Phase 59 ships no HTTP / Protocol-wire surface — the SSE+REST
  transport binding is Phase 60, and the version constant is returned
  on `harbor version` only after Phase 63. The wire/CLI assertions
  `skip` per the 404/405/501 → SKIP convention.

## Coverage target

- `internal/protocol/types`: 85%

## Dependencies

- 58 — the Protocol single-source checker (`internal/protocol/
  singlesource`). Phase 59 adds three exported wire structs to
  `internal/protocol/types`, so the checker's `CanonicalWireTypes`
  lockstep map must be updated in lockstep (D-075 §4). In practice
  Phase 59 also extends the Phase 54 layout (`internal/protocol/types/
  version.go`, the `ProtocolVersion` pin from D-072 §1).

## Risks / open questions

- **Three new exported wire structs land in `internal/protocol/types`,
  so the Phase 58 lockstep test fails until
  `singlesource.CanonicalWireTypes` is updated.** This is by design —
  D-075 §4's `TestSingleSource_CanonicalWireTypesInLockstep` exists
  precisely to fail when a new wire type lands without updating the
  checker. Phase 59 updates the map in the same PR (CLAUDE.md §17.6
  "fix what the lint finds"); the risk is only that a reviewer might
  read the map edit as scope creep — it is not, it is the mandatory
  coupling.
- **`Version` vs `ProtocolVersion` — two representations of one fact.**
  `ProtocolVersion` (the string) stays as the RFC-change trip-wire
  Phase 54 pinned; `CurrentVersion` (the parsed `Version`) is derived
  from it via `MustParseVersion` at package-init. They cannot drift —
  `TestCurrentVersion_MatchesProtocolVersion` pins
  `CurrentVersion.String() == ProtocolVersion`. This is not a
  CLAUDE.md §13 "two parallel implementations" smell: it is one source
  (`ProtocolVersion`) and one derived parse, with a test gating the
  derivation.
- **The capability set is `{task_control}` only at 0.1.0.** That is
  correct — Phase 54 is the only Protocol surface that has shipped.
  RFC §5.2 lists five more surfaces (streaming events, state snapshots,
  topology, artifacts, traces/metrics); each adds its `Capability`
  constant in the phase that ships it. The set growing is expected, not
  a risk — `IsValidCapability` + `TestCapabilities_DeterministicAndValid`
  keep it honest.
- **No version bump means `Compatible` / `Compare` are exercised only
  against synthetic versions in tests.** That is acceptable — the
  mechanism must exist *before* the first bump (a deprecation-window
  discipline with no `Version` type to express the window in is dead
  letter). The first real cross-version interaction is whenever the
  Protocol reaches `0.2.0` / `1.0.0`; the mechanism is ready for it.

## Glossary additions

- **Protocol version** — the parsed, comparable form of the Harbor
  Protocol's pinned version. `ProtocolVersion` (the string constant in
  `internal/protocol/types/version.go`, `"0.1.0"` at V1) is the
  RFC-change trip-wire; `Version` (a `Major`/`Minor`/`Patch` struct,
  Phase 59) is the parsed form a client uses to detect skew via the
  same-major `Compatible` rule. Bumping `ProtocolVersion` is an RFC
  change (RFC §5.3, CLAUDE.md §8). Added to `docs/glossary.md`.
- **Deprecation window** — the discipline (RFC §5.3, CLAUDE.md §8) that
  a breaking change to the Protocol surface must announce a removal
  window before it lands, so third-party Consoles are not whipsawed.
  Phase 59 ships the structured `Deprecation` note format
  (`Subject` / `Kind` / `DeprecatedIn` / `RemovedIn` / `Replacement` /
  `Note`) and the `Deprecations()` registry that is its single home.
  Added to `docs/glossary.md`.
- **Capability negotiation** — the mechanism by which a Protocol client
  asks the Runtime which Protocol surfaces are live. Phase 59 ships the
  `Capability` enum, the `Capabilities()` set (V1: `task_control`
  only — the Phase 54 surface), and the `VersionHandshake` wire struct
  (`ProtocolVersion` + advertised `Capabilities`) the negotiation
  exchanges. Added to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, Phase 59 ships version/deprecation/capability value types; it touches no identity-scoped path.
- [ ] **If this phase builds a reusable artifact:** N/A — `Version` / `Deprecation` / `Capability` / `VersionHandshake` are immutable value types and the functions over them are pure with no construction-time state and no goroutines; not a D-025 compiled artifact. See Test plan "Concurrency / leak".
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** N/A — Phase 59 wires no runtime drivers and opens no cross-subsystem seam; its only coupling is the build-time Phase 58 single-source checker's lockstep map, exercised by re-running the Phase 58 suite. See Test plan "Integration".
- [ ] If new vocabulary: glossary updated — "Protocol version", "deprecation window", "capability negotiation" added to `docs/glossary.md`.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — no departure; D-077 records the settled versioning-discipline design.
