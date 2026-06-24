# Phase 127 — Protocol wire-manifest consumability (runtime.info digest) — STRETCH

## Summary

Make the canonical Protocol wire surface **drift-detectable by any
connected Protocol client at connect-time**. Today the lockstep mechanism
(reflected `wire-manifest.gen.json` + the `.mjs` field guard + the
`git diff` / `go test` / `npm run lint` gates) is robust for Harbor's own
Console but lives entirely *inside* `web/console/` and is never served
over the wire: `runtime.info` advertises only `ProtocolVersion` +
`Capabilities`, so a generic Protocol client — a third-party Console, an
IDE/TUI client, an SDK consumer — cannot tell whether the runtime it just
attached to speaks the same wire shapes it was built against. This phase
ships a stable **wire-surface digest** (`sha256:…` over the canonical
name-level surface — Protocol version + method names + error codes +
capability strings + wire-type names), computes it from the canonical Go
single sources, returns it as a new additive `runtime.info` field, and
stamps the same digest into the committed manifest — so a client can
compare the digest it vendored at build time against the live runtime's
reported digest and **surface a loud drift signal** instead of discovering
wire drift field-by-field at runtime. The digest is a coarse, opaque
**name-level fingerprint** (a hash, not the shapes): it deliberately
EXCLUDES field shapes and event-type names. Field-level shapes stay a
build-time concern (the `.mjs` field guard a manifest-vendoring client
runs) and are never exposed over the wire.

This phase exists to satisfy Harbor's own "no primitive without a
consumer" rule (CLAUDE.md §13) read one layer up: the lockstep *build
artifact* exists, but no *runtime* surface exposes it to a connected
client. The consumer that lands in the same wave is the Console's
app-shell status bar, which already calls `runtime.info` at attach.

## RFC anchor

- RFC §5 — Harbor Protocol (the wire contract a client binds to; this
  phase makes the contract's identity observable to a client).
- RFC §5.2 — What the Protocol exposes (`runtime.info` is the
  runtime-posture surface this phase extends additively).
- RFC §5.3 — Versioning. Cited here for exactly one rule: **bumping
  `ProtocolVersion` is an RFC change.** The additive-vs-breaking *change
  taxonomy* this plan reasons with (an additive surface addition is a
  Minor-class change, not a Major break) lives in
  `internal/protocol/types/version.go` (the `Version` struct's
  Major/Minor/Patch godoc) — that is the source for "a new optional wire
  field is backward-compatible," and it is why this phase needs no version
  bump.

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §1: *"it guarantees Console, third-party consoles, and
  `harbor dev` see exactly the same data shape that production
  observability sees. There is no privileged 'internal' view … the event
  bus the single contract that has to stay stable across versions — and
  unlocks remote attach, multi-runtime fleet view, IDE/TUI integrations,
  and observability-vendor adapters as natural extensions."* — the wire
  surface is the single stable contract; a connected client must be able
  to verify it mechanically, not just Harbor's own Console.
- brief 06 §9 Q-4: *"Event schema versioning. Best-effort additive (new
  `EventType`s and new optional fields are non-breaking)? … The latter is
  heavier but matters once third-party Consoles exist."* — a connect-time
  surface digest is the lightweight enabler for the "once third-party
  consoles exist" world: it lets an additive-only world still surface an
  *unexpected* drift loudly.
- brief 06 §1: *"The Runtime owns the bus; everything else is a client."*
  — the digest is owned and computed by the runtime from its own
  canonical sources; clients consume it, they do not re-derive it from
  internals.

## Findings I'm departing from (if any)

None.

## Goals

- A canonical, deterministic **wire-surface digest** computed purely from
  the Go single sources (`methods.Methods()`, `protoerrors.Codes()`,
  `types.Capabilities()`, `singlesource.CanonicalWireTypes` keys, and
  `types.ProtocolVersion`) — no reflection over field shapes, no driver
  seating, no tree scan; a pure function, memoised once per process,
  concurrent-safe by construction.
- The digest is hashed over the **canonical complete capability universe**
  (`types.Capabilities()`, the compile-time `canonicalCapabilities` set),
  NOT a per-instance advertised subset — so the digest is *invariant to
  capability gating*. Two runtimes built from the same sources produce the
  same digest even when one wires `topology_snapshot` and the other does
  not. The digest fingerprints the Protocol *surface*, not a deployment's
  wiring.
- The digest returned as a new additive `runtime.info` wire field
  (`wire_surface_digest`) — one extra field on a method a client already
  calls at attach, so the full connect-time negotiation (version +
  capabilities + surface digest) is one round-trip.
- The same digest stamped into the committed `wire-manifest.gen.json`
  (a new top-level `wire_surface_digest`) via `make protocol-ts-gen`, so a
  client that vendored the manifest at build time has the expected digest
  to compare; the existing `git diff` / `go test` lockstep gates keep the
  two in lockstep automatically.
- A real **consumer in the same wave** (§13): the Console app-shell status
  bar (`AppStatusBar.svelte`, which already fetches `runtime.info` at
  attach) compares the live `runtime.info.wire_surface_digest` against the
  digest baked into the vendored manifest and surfaces a loud,
  operator-visible drift signal on mismatch (never a silent swallow).
- Identity-mandatory + fail-loud throughout: the digest is returned only
  after `runtime.info`'s existing identity gate; the digest function never
  silently returns an empty string (an empty canonical surface is a
  build-time impossibility and is asserted, not tolerated).

## Non-goals

- **Exposing field-level wire shapes over the wire.** The digest is an
  opaque name-level hash; the manifest's per-type field shapes stay a
  build-time artifact inside `web/console/`. Serving shapes would leak
  build-time internals and invert the RFC §5.1 decoupling rule.
- **A new Protocol method.** Evaluated and rejected in favour of an
  additive `runtime.info` field (see Risks) — the digest is posture data
  the info method already carries version + capabilities alongside, and an
  additive field is strictly less surface than a new method+handler+route.
- **Publishing an npm package / module export of the manifest + guard.**
  The vendor-and-gate interim (a generic Protocol client copies the
  committed manifest into its own build) needs zero Harbor code; an npm
  publish pipeline is release-engineering and is out of scope.
- **Generating the per-domain TypeScript type modules** (`cmd/harbor-gen-protocol-ts`
  — that name stays reserved for the deferred full generator). This phase
  is verification/observability of the surface, not generation.
- **Including event-type names in the digest.** Event types cannot be
  enumerated canonically at runtime without seating driver registries (the
  manifest derives them by a build-time textual scan precisely to avoid
  driver blank-imports); the digest is deliberately scoped to the
  request/response/capability contract a client binds to. The name-level
  fingerprint therefore covers method/error/capability/type *names* +
  version, and excludes BOTH field shapes AND event-type names.
  Documented residual in Risks.
- **Bumping `ProtocolVersion`.** The new field is additive — a Minor-class
  change in the `internal/protocol/types/version.go` Major/Minor/Patch
  taxonomy ("a new optional wire field" is backward-compatible), so it
  needs no version bump. RFC §5.3's rule that *bumping the version is an
  RFC change* is exactly why this stays additive.

## Acceptance criteria

- [ ] A new canonical package `internal/protocol/wiresurface` exposes
      `Digest() string` returning `"sha256:" + hex(sha256(…))` over a
      deterministic, format-versioned serialization of: `ProtocolVersion`,
      sorted method names, sorted error codes, sorted **canonical-universe**
      capability strings (`types.Capabilities()`), and sorted canonical
      wire-type names. The function is pure, memoised via `sync.Once`, and
      imports only the light canonical packages (no `internal/protocol`, no
      drivers) so it can be shared by the runtime and the lockstep build
      tool without an import cycle or a dependency-set balloon.
- [ ] `Digest()` is deterministic across processes and runs: the same
      build always produces the same digest; two builds differing only in
      a method/error/capability/wire-type-name produce different digests;
      a change that touches none of those (a field-shape edit, a comment,
      an event-type rename) leaves the digest unchanged — the latter is a
      documented, intended limitation, not a bug (field-shape drift remains
      the build-time `.mjs` gate's job; event types are out of scope).
- [ ] **Invariance to capability gating:** because `Digest()` hashes the
      canonical capability *universe* (`types.Capabilities()`), not a
      per-instance subset, the digest is identical whether or not a given
      runtime wires a conditional capability (e.g. `topology_snapshot`). A
      unit test asserts this explicitly: a digest computed against the full
      canonical set equals the digest the runtime reports regardless of its
      `wiredCaps`.
- [ ] `types.RuntimeInfo` gains a non-optional `WireSurfaceDigest string \`json:"wire_surface_digest"\`` field; `PostureSurface.handleInfo` populates it from `wiresurface.Digest()`. The field is returned only after the existing identity-edge gate (`runtime.info` is identity-mandatory; an incomplete triple still fails closed with `CodeIdentityRequired`).
- [ ] The lockstep manifest gains a top-level `wire_surface_digest`
      (= `wiresurface.Digest()`) AND the reflected `types.RuntimeInfo`
      type-shape automatically gains the new field; `make protocol-ts-gen`
      regenerates both, and `make protocol-ts-gen-check` passes on the
      regenerated tree and FAILS on a stale manifest (the `git diff` half)
      and on an un-mirrored TS interface (the `.mjs` half).
- [ ] The generated Protocol type reference
      (`docs/site/protocol/types.md`, a `CODE GENERATED … DO NOT EDIT`
      page) regains the `RuntimeInfo.wire_surface_digest` row after
      `make protocol-docs-gen`; `make protocol-docs-gen-check` passes
      (`git diff --exit-code` clean) and the generator's lockstep test
      (a new wire-type field without its docs join row fails `go test`)
      stays green. The regenerated page is committed in the same PR
      (CLAUDE.md §18, D-209).
- [ ] A Go lockstep test in `cmd/harbor-protocol-ts-lockstep` pins
      `Manifest.WireSurfaceDigest == wiresurface.Digest()` — the committed
      manifest's digest can never drift from the runtime's within a build.
- [ ] The hand-maintained TS `RuntimeInfo` interface
      (`web/console/src/lib/protocol/settings.ts`) declares
      `wire_surface_digest: string`; the `.mjs` field-level scan passes
      (a dropped/renamed/mistyped field fails it — proven by planted
      drift).
- [ ] The Console app-shell status bar
      (`web/console/src/lib/components/ui/AppStatusBar.svelte`), which
      already fetches `runtime.info` at `onMount`, reads the attached
      runtime's `wire_surface_digest`, compares it against the manifest's
      `wire_surface_digest` (vendored via a `?import` of the committed
      `wire-manifest.gen.json`) using a pure comparator exported from
      `connection.ts`, and surfaces a loud, operator-visible drift signal
      on mismatch — never a silent swallow. The comparator distinguishes
      THREE outcomes: `match`, `drift` (live ≠ vendored — surfaced loud),
      and `unsupported` (the runtime reported no/empty digest — surfaced as
      an informational "runtime predates digest support" note, NOT a drift
      alarm and NOT folded into the existing `catch` swallow). A vitest
      covers all three comparator branches; a component-level test asserts
      the status bar surfaces the drift signal on a planted mismatch.
- [ ] An integration test wires the REAL `PostureSurface` over the REAL
      control transport behind `httptest.Server`, calls `runtime.info` with
      a complete identity triple, asserts the reported
      `wire_surface_digest` equals `wiresurface.Digest()` AND equals the
      committed manifest's top-level `wire_surface_digest`, and covers ≥1
      failure mode (a missing-identity request is rejected `401` /
      `identity_required` before any digest is returned). Runs under
      `-race`.
- [ ] A concurrent-reuse assertion: N≥100 concurrent `runtime.info`
      dispatches against a single shared `PostureSurface` return a
      byte-identical `wire_surface_digest` with no data race, no context
      bleed, no goroutine leak (extends the existing PostureSurface
      concurrent test).
- [ ] `scripts/smoke/phase-127.sh` asserts: the `wiresurface` package +
      `Digest` exist; the manifest carries a top-level
      `wire_surface_digest` matching `^sha256:[0-9a-f]{64}$`; the TS
      `RuntimeInfo` interface declares `wire_surface_digest`;
      `make protocol-ts-gen-check` AND `make protocol-docs-gen-check` pass;
      the new go tests pass under `-race`; and, when the live dev server
      exposes the posture route, a `runtime.info` call returns a
      `wire_surface_digest` matching the manifest's (skips per the
      404/405/501 convention when the route or a dev token is
      unavailable). FAIL = 0.

## Files added or changed

```text
internal/protocol/wiresurface/
  wiresurface.go            # Digest() — pure, sync.Once-memoised, sha256 over the canonical name-level surface
  wiresurface_test.go       # determinism, format-version prefix, sensitivity matrix, capability-gating invariance, N>=100 concurrency
internal/protocol/types/posture.go        # RuntimeInfo gains WireSurfaceDigest
internal/protocol/types/posture_test.go   # JSON round-trip covers the new field
internal/protocol/posture.go              # handleInfo populates WireSurfaceDigest
internal/protocol/posture_test.go         # handleInfo returns the digest; concurrent dispatch returns identical digest
cmd/harbor-protocol-ts-lockstep/manifest.go       # Manifest gains top-level WireSurfaceDigest (= wiresurface.Digest())
cmd/harbor-protocol-ts-lockstep/manifest_test.go  # manifest digest == wiresurface.Digest(); RuntimeInfo field present
cmd/harbor-protocol-ts-lockstep/lockstep_test.go  # lockstep pins the new field + digest
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated (top-level digest + RuntimeInfo field) — GENERATED, do not hand-edit
web/console/src/lib/protocol/settings.ts               # RuntimeInfo interface gains wire_surface_digest: string
web/console/src/lib/connection.ts                      # pure comparator helper: compareWireDigest(live, vendored) -> match|drift|unsupported
web/console/src/lib/connection.test.ts                 # vitest: comparator match / drift / unsupported branches
web/console/src/lib/components/ui/AppStatusBar.svelte  # at-attach digest fetch+compare+surface (loud on drift; informational on unsupported)
web/console/src/lib/components/ui/AppStatusBar.test.ts # vitest: surfaces drift signal on planted mismatch
docs/site/protocol/types.md                            # regenerated — RuntimeInfo.wire_surface_digest row — GENERATED, do not hand-edit (D-209)
test/integration/phase127_wire_digest_test.go          # E2E: runtime.info digest == wiresurface.Digest() == manifest digest; missing-identity 401
scripts/smoke/phase-127.sh
docs/plans/phase-127-protocol-wire-manifest-consumability.md
docs/decisions.md          # D-259
docs/glossary.md           # wire-surface digest
docs/plans/README.md       # Phase 127 row Pending (V1.6) -> (Shipped on land)
README.md                  # Status table Phase 127 row + a one-line pointer on the runtime.info digest
docs/skills/use-the-harbor-protocol/SKILL.md           # if it quotes the runtime.info response shape, add the wire_surface_digest field (§18 same-PR rule)
docs/skills/observe-with-the-console/SKILL.md           # if it quotes the runtime.info response shape, same §18 update
```

No new top-level directory (AGENTS.md §3 unchanged): `internal/protocol/wiresurface`
is a sub-package of the existing `internal/protocol` Protocol tree.

## Public API surface

```go
// internal/protocol/wiresurface

// Digest returns the canonical Harbor Protocol wire-surface digest:
//   "sha256:" + lowercase-hex(sha256(canonical-serialization))
// over a deterministic, format-versioned encoding of the Protocol
// version, method names, error codes, capability strings (the canonical
// capability universe), and canonical wire-type names. It is a coarse
// name-level fingerprint — it covers the SHAPE OF NAMES, not field
// shapes and not event-type names. It is a pure function of the build's
// canonical Go sources, memoised once per process; safe for concurrent
// use.
func Digest() string
```

```go
// internal/protocol/types (RuntimeInfo gains one additive field)
type RuntimeInfo struct {
    // … existing fields …
    // WireSurfaceDigest is the opaque, stable name-level digest of the
    // runtime's canonical Protocol wire surface. A client compares it
    // against the digest it was built against and surfaces a loud drift
    // signal on a mismatch — coarse wire drift is detectable at
    // connect-time without exposing field shapes over the wire.
    WireSurfaceDigest string `json:"wire_surface_digest"`
}
```

```typescript
// web/console/src/lib/connection.ts — a pure, framework-free comparator
// (the only Console-shared seam; the fetch + surfacing lives in
// AppStatusBar.svelte, never in this synchronous localStorage resolver).

export type WireDigestComparison = 'match' | 'drift' | 'unsupported';

/**
 * Compare a live runtime's reported wire-surface digest against the
 * digest this Console build vendored from wire-manifest.gen.json.
 *   - 'match'       — identical; the runtime speaks the build's surface.
 *   - 'drift'       — both present, non-equal — surface LOUD.
 *   - 'unsupported' — the runtime reported no/empty digest (it predates
 *                     digest support) — surface as informational, never a
 *                     drift alarm.
 */
export function compareWireDigest(live: string | undefined, vendored: string): WireDigestComparison;
```

No new Protocol method, error code, or capability. The
`internal/protocol/wiresurface` package and the additive `RuntimeInfo`
field are the only Go surface other phases depend on.

## Test plan

- **Unit (Go):** `wiresurface` — determinism (repeated calls equal), the
  `sha256:` + 64-hex format, the `harbor-wire-surface/v1` serialization
  prefix, a sensitivity matrix (injecting a synthetic method / error /
  capability / type name into the serialization input changes the digest;
  a no-op does not), and **capability-gating invariance** (the digest is
  computed over `types.Capabilities()` — the canonical universe — so it is
  independent of any per-instance wired subset). `types/posture_test.go` —
  `RuntimeInfo` JSON round-trip carries `wire_surface_digest`.
  `posture_test.go` — `handleInfo` returns a non-empty digest equal to
  `wiresurface.Digest()`, and the digest is identical for a surface wired
  with vs without `topology_snapshot` (the per-instance subset does not
  move it). `cmd/harbor-protocol-ts-lockstep` —
  `Manifest.WireSurfaceDigest == wiresurface.Digest()`, and the
  regenerated manifest carries the `RuntimeInfo.wire_surface_digest` field
  shape.
- **Unit (TS):** `connection.test.ts` — `compareWireDigest` returns
  `match` on equal, `drift` on non-equal-both-present, `unsupported` on
  `undefined`/empty live digest. `AppStatusBar.test.ts` — with a stubbed
  `posture.info` returning a mismatched digest, the status bar surfaces the
  loud drift signal; with an absent digest it surfaces the informational
  "predates digest support" note, not a drift alarm.
- **Integration:** `test/integration/phase127_wire_digest_test.go` — real
  `PostureSurface` over the real `internal/protocol/transports/control`
  handler behind `httptest.Server`; a `runtime.info` POST with a complete
  identity triple returns a digest equal to `wiresurface.Digest()` and to
  the committed manifest's top-level `wire_surface_digest` (read from the
  repo file); failure mode: an incomplete-triple request is rejected
  `401` / `identity_required` before any body digest is produced.
  Identity propagation asserted end-to-end. `-race`.
- **Conformance:** N/A — no new method/error/event; the existing
  `internal/protocol/singlesource` checker already gates the new
  `wiresurface` sub-package (it redefines no method string, error code, or
  wire type — `RuntimeInfo` stays single-sourced in `types`).
- **Concurrency / leak:** the `wiresurface` unit test runs N≥100
  goroutines calling `Digest()` concurrently under `-race`, asserting a
  single identical result (the `sync.Once` memoisation is race-free); the
  PostureSurface concurrent test is extended to assert N≥100 concurrent
  `runtime.info` dispatches return a byte-identical `wire_surface_digest`
  with goroutine baseline restored.

## Smoke script additions

`scripts/smoke/phase-127.sh` adds:

- Static: `internal/protocol/wiresurface/wiresurface.go` exists and
  declares `func Digest()`.
- Static: `web/console/src/lib/protocol/wire-manifest.gen.json` contains a
  top-level `"wire_surface_digest"` whose value matches
  `^sha256:[0-9a-f]{64}$` (via `jq`; skip if `jq` absent).
- Static: `web/console/src/lib/protocol/settings.ts` `RuntimeInfo`
  interface declares `wire_surface_digest`.
- Static: `web/console/src/lib/connection.ts` exports `compareWireDigest`.
- Static: `internal/protocol/types/posture.go` `RuntimeInfo` declares
  `WireSurfaceDigest` with the `wire_surface_digest` json tag.
- Build/test: `make protocol-ts-gen-check` passes (manifest + RuntimeInfo
  field in lockstep) AND `make protocol-docs-gen-check` passes (generated
  `types.md` regenerated for the new field, D-209);
  `go test -race ./internal/protocol/wiresurface/...` and
  `go test -race -run TestE2E_Phase127 ./test/integration/...` pass.
- Static single-source guard: no Protocol method string / error `Code` is
  redefined under `internal/protocol/wiresurface/` (defence-in-depth over
  the single-source lint).
- Live (skips per 404/405/501): when the dev server exposes
  `POST /v1/control/runtime.info`, a `runtime.info` call with a dev
  identity returns a `wire_surface_digest` equal to the manifest's
  top-level value. Drive it via the existing `assert_post_status` helper
  (already in `common.sh`, line ~165 — POSTs a JSON body, SKIPs on
  404/405/501) to confirm the route answers `200`, then `jq` the digest
  out of the response body and compare to the manifest digest. SKIP when
  the route 404s or no dev token is available — the same posture
  `phase-72f.sh` holds for the posture surface.

## Coverage target

- `internal/protocol/wiresurface`: ≥ 90% (small pure package).
- `internal/protocol` (posture handler delta): no regression below the
  package's existing ≥ 85%.
- `cmd/harbor-protocol-ts-lockstep`: no regression below ≥ 80%.

## Dependencies

- Phase 58 — `internal/protocol` single-source layout +
  `singlesource.CanonicalWireTypes` (the wire-type-name set the digest
  hashes) + the single-source checker that gates the new sub-package.
- Phase 118 — the `cmd/harbor-protocol-ts-lockstep` manifest generator +
  `make protocol-ts-gen-check` lockstep gate (D-223) the digest piggybacks
  on (top-level manifest field + the git-diff / go-test / `.mjs` gates).
- Phase 60 — the `internal/protocol/transports/control` wire transport the
  integration test drives `runtime.info` through.
- Phase 72f — the `PostureSurface` + `runtime.info` wire type this phase
  extends; `CapRuntimePosture` already ships.

**Staging note (no dependency on 126a).** This phase's `Deps` are
**58, 118, 60, 72f** — all long-shipped. It has **NO dependency on Phase
126a** (or any other Phase 126 sub-phase). It is sequenced into **stage 2
of the V1.6 wave for cadence/merge-train reasons only**, not because any
126 surface gates it; it could equally land in stage 1. Do not infer a
126→127 ordering constraint from the wave staging.

## Risks / open questions

- **STRETCH / does this belong in V1.6?** The vendor-and-gate interim — a
  generic Protocol client copies the committed `wire-manifest.gen.json`
  into its own build and runs the `.mjs` guard there — needs **zero Harbor
  code**, so no client is *blocked* without this phase. The value this
  phase adds is *connect-time* drift detection (a client can catch a
  runtime it attaches to that drifted from its build) rather than only
  *build-time* detection. Recommendation: **include in V1.6 as the minimal
  high-value slice** (one pure package + one additive field + one
  manifest field + one Console consumer) because (a) the cost is small and
  fully additive — no version bump, no new method, no migration; (b) it
  closes the investigation finding's "NOT downstream-consumable" gap with
  the least surface; (c) deferring it leaves Harbor's own
  primitive-with-consumer rule unsatisfied for the lockstep mechanism (the
  manifest exists but no *runtime* consumer exposes it). If V1.6 capacity
  is tight, this is the first phase in the band to cut — but cutting it
  should be a recorded decision, not a silent drop.
- **New method vs additive `runtime.info` field — RESOLVED to additive
  field.** A dedicated `protocol.wire_manifest` method would add a
  method+handler+route+TS-type for no extra value: the digest is opaque
  posture data, `runtime.info` is already the attach-time negotiation
  call (version + capabilities), and an additive field is backward-
  compatible (old clients ignore it). The field path is strictly less
  surface and one fewer round-trip. Recorded in D-259.
- **Digest scope is name-level, not field-level — intended.** The digest
  hashes method/error/capability/type *names* + version, not field shapes.
  A field-type swap on a wire struct (same name) does NOT change the
  digest. This is deliberate: field-shape drift is already caught by the
  build-time `.mjs` gate for any client that vendors the manifest, and
  exposing field shapes over the wire is an explicit non-goal (RFC §5.1).
  The digest's job is the coarse "did the surface's *shape of names*
  change unexpectedly" signal a connected client needs at connect-time.
  Documented in the package godoc and D-259.
- **Old-runtime / absent-digest handling — informational, never a false
  alarm.** A runtime built before this phase returns `runtime.info` with
  no `wire_surface_digest` (the field is absent/empty). The Console
  comparator MUST classify that as `unsupported` ("the runtime predates
  digest support") and surface it as a neutral informational note — NOT a
  `drift` alarm (the surfaces may be identical; absence proves nothing
  about drift) and NOT folded into the existing `try/catch` swallow in
  `AppStatusBar` (which is for transport failure, a different condition).
  The three-way `match` / `drift` / `unsupported` split keeps each
  condition honest.
- **Capability-gating invariance.** The digest hashes the canonical
  capability *universe* (`types.Capabilities()` over `canonicalCapabilities`,
  the compile-time set), not the per-instance `wiredCaps` subset. Without
  this, a runtime that does not wire `topology_snapshot` would report a
  different digest from one that does despite identical Protocol sources —
  a false-positive drift signal. A unit test pins the invariance.
- **Events excluded from the digest — documented residual.** Event-type
  enumeration at runtime would require seating driver registries (the
  manifest avoids this with a build-time textual scan). Including events
  would either re-introduce driver blank-imports into the digest path
  (§13 violation) or force a tree scan into the runtime hot path. The
  digest is scoped to the request/response/capability contract; event
  additivity stays brief 06 §9 Q-4's "best-effort additive" posture.
  Open question for a future phase: a canonical, driver-free event-type
  registry would let events join the digest — out of scope here.
- **Serialization stability.** The digest must be byte-stable across Go
  versions and architectures. Mitigated by: a fixed `harbor-wire-surface/v1`
  format prefix, explicit lexicographic sorting of every set, a labelled
  newline-delimited encoding (no map iteration, no `fmt` of structs), and
  a golden-digest unit test that pins the exact bytes for the current
  surface so an accidental serialization change is caught.
- **Generated docs regen (D-209).** `WireSurfaceDigest` on
  `types.RuntimeInfo` is a canonical wire-type change, so the generated
  `docs/site/protocol/types.md` (a `DO NOT EDIT` page) must be regenerated
  with `make protocol-docs-gen` and committed in the same PR; the docs
  workflow runs `make protocol-docs-gen-check` (`git diff --exit-code`)
  before the VitePress build and the generator's lockstep test fails on an
  un-joined new field. Handled exactly as the `protocol-ts-gen` manifest
  regen is.
- **§18 skill drift (binding).** Two skills name the `runtime.info`
  surface — `docs/skills/use-the-harbor-protocol` and
  `docs/skills/observe-with-the-console` (confirm via
  `grep -rl 'runtime.info' docs/skills/`). The new field is additive and
  the shown call still works; but if either SKILL.md **quotes the
  `runtime.info` response shape**, it MUST add the `wire_surface_digest`
  field in THE SAME PR (CLAUDE.md §18 same-PR rule). If neither quotes the
  shape (only names the verb), the skills are exempt — record which in the
  PR.

## Glossary additions

- **wire-surface digest** — the opaque, stable `sha256:…` name-level
  fingerprint of the runtime's canonical Protocol wire surface (Protocol
  version + method names + error codes + capability strings + wire-type
  names), computed from the Go single sources, returned on `runtime.info`,
  and stamped into the committed wire manifest. It covers the *shape of
  names*, not field shapes and not event-type names. A connected Protocol
  client compares it against the digest it was built against to detect
  coarse wire drift at connect-time. Add to `docs/glossary.md` in the same
  PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits — N/A)
- [ ] `make protocol-ts-gen-check` passes (manifest regenerated, top-level
      digest + RuntimeInfo field in lockstep)
- [ ] `make protocol-docs-gen-check` passes (D-209: `docs/site/protocol/types.md`
      regenerated for the `RuntimeInfo.wire_surface_digest` row and
      committed)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      — N/A (the digest is identity-agnostic build-global data, returned
      only after `runtime.info`'s existing identity gate; no new
      identity-scoped storage path)
- [ ] **Concurrent-reuse test passes — N≥100 against a single shared
      instance under `-race`.** The `wiresurface.Digest()` memoisation
      (N≥100 concurrent callers) AND the shared `PostureSurface`
      (N≥100 concurrent `runtime.info` dispatches, identical digest) are
      both covered.
- [ ] **Integration test exists, real drivers/transport end-to-end,
      identity propagation, ≥1 failure mode, `-race`.**
      `test/integration/phase127_wire_digest_test.go`.
- [ ] §18 skill check: `grep -rl 'runtime.info' docs/skills/` — any skill
      quoting the `runtime.info` response shape updated in this PR
- [ ] If new vocabulary: glossary updated (`wire-surface digest`)
- [ ] If a brief finding was departed from: justified + decisions.md entry
      — N/A, no departures; D-259 records the design decisions

---

## Implementation handoff

Turnkey artifacts for the implementing agent. Operate only inside your
worktree (`pwd` first; STOP if a path resolves outside it). Run
`markdownlint-cli2` repo-wide before committing (blank lines around `---`
and `## D-NNN` headings in `docs/decisions.md`).

### (a) Master-plan `docs/plans/README.md` index row

Append (the table is sorted by phase number; this row sorts after 126c):

```text
|127 | Protocol wire-manifest consumability (runtime.info digest) — STRETCH (a canonical `internal/protocol/wiresurface.Digest()` over the name-level wire surface — Protocol version + methods + errors + capabilities + wire-type names — returned as an additive `runtime.info.wire_surface_digest` field AND stamped into the committed `wire-manifest.gen.json`; a connected Protocol client compares the live digest against the vendored one and surfaces a loud drift signal; NO new method, NO version bump, NO field-shape exposure; Console app-shell status-bar connect-time drift check is the consumer, D-259) | internal/protocol/wiresurface + internal/protocol (additive) + cmd/harbor-protocol-ts-lockstep + web/console | §5, §5.2, §5.3 | 58, 118, 60, 72f | 90% | Pending (V1.6) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — note the blank lines)

Append at end of file:

```markdown

---

## D-259 — Protocol wire-surface digest: connect-time wire-drift detection for any Protocol client via an additive runtime.info field + a manifest-stamped digest

**Date:** 2026-06-24

**Status:** Accepted (planning)

**Context.** The Phase 118 / D-223 lockstep mechanism (`CanonicalWireTypes`
→ reflected `wire-manifest.gen.json` → field-level `.mjs` guard + git-diff
/ go-test / lint gates) is robust for Harbor's OWN Console but is NOT
downstream-consumable: the manifest + guard + allowlist live inside
`web/console/` with no module export, and the manifest is never served
over the wire. `runtime.info` advertises only `ProtocolVersion` +
`Capabilities`, so a connected Protocol client (a third-party Console, an
IDE/TUI client, an SDK consumer) cannot detect, at connect-time, that the
runtime it attached to drifted from the wire shapes it was built against.
This is a §13 primitive-with-consumer gap read one layer up: the lockstep
*build artifact* exists, but no *runtime* surface exposes it to a client.

**Decision.**

1. **A canonical wire-surface digest, computed from the Go single
   sources.** A new light package `internal/protocol/wiresurface` exposes
   `Digest() string` = `"sha256:" + hex(sha256(serialization))` over a
   deterministic, `harbor-wire-surface/v1`-prefixed, lexicographically
   sorted, newline-labelled encoding of: `types.ProtocolVersion`,
   `methods.Methods()`, `protoerrors.Codes()`, `types.Capabilities()`
   (the canonical capability UNIVERSE, not a per-instance subset), and
   the `singlesource.CanonicalWireTypes` keys. The function is pure,
   `sync.Once`-memoised, and imports only the light canonical packages
   (no `internal/protocol`, no drivers) — no import cycle, no dependency
   balloon for the lockstep tool that also calls it. The digest is a
   coarse NAME-LEVEL fingerprint: it covers the shape of names and
   EXCLUDES both field shapes and event-type names.
2. **Additive `runtime.info` field, not a new method.** `types.RuntimeInfo`
   gains `WireSurfaceDigest string \`json:"wire_surface_digest"\``;
   `PostureSurface.handleInfo` populates it. A new `protocol.wire_manifest`
   method was rejected: the digest is opaque posture data, `runtime.info`
   is the attach-time negotiation call already, and an additive field is
   backward-compatible (old clients ignore it) and one fewer round-trip.
3. **Digest stamped into the committed manifest.** The
   `cmd/harbor-protocol-ts-lockstep` `Manifest` gains a top-level
   `wire_surface_digest` (= `wiresurface.Digest()`); a lockstep test pins
   `Manifest.WireSurfaceDigest == wiresurface.Digest()`, and the existing
   `make protocol-ts-gen-check` git-diff + go-test gates keep the manifest
   and the runtime in lockstep automatically. A client vendors the
   manifest at build time and compares its `wire_surface_digest` against
   the live `runtime.info.wire_surface_digest`.
4. **The consumer lands in the same wave (§13).** The Console app-shell
   status bar (`web/console/src/lib/components/ui/AppStatusBar.svelte`),
   which already fetches `runtime.info` at `onMount`, compares the attached
   runtime's digest against the manifest's digest (via a pure
   `compareWireDigest` helper exported from `connection.ts`) and surfaces a
   loud, operator-visible drift signal on a `drift` mismatch — never a
   silent swallow. A runtime that reports no digest is classified
   `unsupported` ("predates digest support") and surfaced as an
   informational note, NOT a drift alarm. `connection.ts` itself stays a
   pure synchronous resolver — the fetch + surfacing live in the component,
   not in the resolver.
5. **Name-level scope; field shapes stay build-time and off the wire.**
   The digest hashes method/error/capability/type *names* + version, not
   field shapes (RFC §5.1: shapes are not exposed over the wire). A
   field-type swap on a same-named struct does not move the digest; that
   drift remains the build-time `.mjs` gate's job for any manifest-vendoring
   client.
6. **Events excluded.** Runtime event-type enumeration would require
   seating driver registries (the manifest uses a build-time textual scan
   to avoid driver blank-imports, §13); including events would re-introduce
   that coupling into the runtime path. The digest is scoped to the
   request/response/capability contract a client binds to. A driver-free
   canonical event registry could let events join the digest later.
7. **No version bump.** The new field is additive — a Minor-class change in
   the `internal/protocol/types/version.go` Major/Minor/Patch taxonomy ("a
   new optional wire field is backward-compatible"). RFC §5.3's rule that
   *bumping `ProtocolVersion` is an RFC change* is precisely why this stays
   additive: `ProtocolVersion` holds at 0.1.0. The generated Protocol type
   reference (`docs/site/protocol/types.md`) is regenerated with
   `make protocol-docs-gen` and committed in the same PR (D-209).

**§4.3 deviations.** STRETCH/optional: the vendor-and-gate interim needs
zero Harbor code, so no client is blocked without this phase; it is
recommended for V1.6 as the minimal high-value slice and is the first in
its band to cut if capacity is tight (a recorded cut, not a silent drop).
The npm-publish pipeline and the full `cmd/harbor-gen-protocol-ts` type
generator (reserved name) stay out of scope.

**Cross-references.** D-223 (the lockstep gate this stamps the digest
into), D-209 (the generated Protocol-docs regen gate), D-093 (the original
generate/verify Protocol-client decision), D-061 (Console is a Protocol
client), D-025 (concurrent-reuse contract for the memoised digest + the
shared PostureSurface). RFC §5, §5.2, §5.3. `internal/protocol/types/version.go`
(additive-vs-breaking taxonomy). brief 06. Plan:
`docs/plans/phase-127-protocol-wire-manifest-consumability.md`.
```

### (c) `scripts/smoke/phase-127.sh` assertions to add

Use `scripts/smoke/common.sh` helpers; no new curl wrappers
(`assert_post_status` for the live POST already exists in `common.sh`).
Each maps 1:1 to an acceptance criterion.

```bash
# Static: the wiresurface package + Digest exist.
assert_file "internal/protocol/wiresurface/wiresurface.go" "phase 127: wiresurface package present"
assert_grep_present "func Digest()" "internal/protocol/wiresurface/wiresurface.go" \
  "phase 127: wiresurface.Digest declared"

# Static: RuntimeInfo (Go) carries the new field + json tag.
assert_grep_present 'wire_surface_digest' "internal/protocol/types/posture.go" \
  "phase 127: RuntimeInfo Go type declares wire_surface_digest"

# Static: the committed manifest carries a top-level sha256 digest.
#   (uses jq; skips if jq absent)
MANIFEST="web/console/src/lib/protocol/wire-manifest.gen.json"
if command -v jq >/dev/null 2>&1; then
  DIGEST=$(jq -r '.wire_surface_digest // empty' "$MANIFEST" 2>/dev/null || echo "")
  if printf '%s' "$DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
    ok "phase 127: manifest wire_surface_digest is a well-formed sha256 digest"
  else
    fail "phase 127: manifest wire_surface_digest missing or malformed (got '${DIGEST}')"
  fi
else
  skip "phase 127: jq not available — manifest digest shape check skipped"
fi

# Static: the hand-maintained TS RuntimeInfo mirrors the field, and the
# pure comparator is exported.
assert_grep_present 'wire_surface_digest' "web/console/src/lib/protocol/settings.ts" \
  "phase 127: TS RuntimeInfo interface declares wire_surface_digest"
assert_grep_present 'compareWireDigest' "web/console/src/lib/connection.ts" \
  "phase 127: connection.ts exports the pure wire-digest comparator"

# Single-source defence: no method string / error Code redefined here.
assert_grep_absent 'protoerrors\.Code(' "internal/protocol/wiresurface/wiresurface.go" \
  "phase 127: no Protocol error Code redefined under wiresurface (single-source preserved)"

# Build/test gates: both the manifest lockstep AND the generated-docs gate.
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 127: make protocol-ts-gen-check passes (manifest + RuntimeInfo field in lockstep)"
else
  fail "phase 127: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS field)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 127: make protocol-docs-gen-check passes (types.md regenerated for the new field, D-209)"
else
  fail "phase 127: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit types.md)"
fi
if go test -race ./internal/protocol/wiresurface/... >/dev/null 2>&1; then
  ok "phase 127: wiresurface unit + concurrency tests pass under -race"
else
  fail "phase 127: wiresurface tests failed (go test -race ./internal/protocol/wiresurface/...)"
fi
if go test -race -run TestE2E_Phase127 ./test/integration/... >/dev/null 2>&1; then
  ok "phase 127: wire-digest E2E passes under -race (runtime.info digest == wiresurface.Digest() == manifest digest; missing-identity 401)"
else
  fail "phase 127: wire-digest E2E failed (go test -race -run TestE2E_Phase127 ./test/integration/...)"
fi

# Live (skips per 404/405/501): runtime.info over the wire returns a digest
# matching the manifest's. Build the {"identity":{...}} body from the dev
# triple and POST it with assert_post_status (already in common.sh; SKIPs
# on 404/405/501). On a 200, jq the digest out of the response and compare
# to the manifest digest — ok on equal, fail on mismatch. SKIP when the
# route 404s or no dev token is available (same posture as phase-72f.sh).
#   body='{"identity":{"tenant":"...","user":"...","session":"..."}}'
#   assert_post_status 200 "$(api_url /v1/control/runtime.info)" "$body" \
#     "phase 127: live runtime.info answers 200"
#   then curl the body, jq '.wire_surface_digest', compare to $DIGEST above.
```

### (d) Master-plan per-phase detail-block stub

Add under the detail section of `docs/plans/README.md` (house format —
mirror the 122/120 blocks):

```markdown
### Phase 127 — Protocol wire-manifest consumability (runtime.info digest) — STRETCH

- **Subsystem:** internal/protocol/wiresurface (new sub-package) +
  internal/protocol (additive RuntimeInfo field + handler) +
  cmd/harbor-protocol-ts-lockstep (manifest digest) + web/console
  (app-shell status-bar connect-time consumer).
- **RFC:** §5, §5.2, §5.3 (§5.3 cited only for "bumping the version is an
  RFC change"; the additive-vs-breaking taxonomy is version.go).
- **Deps:** 58 (single-source + CanonicalWireTypes), 118 (lockstep gate /
  D-223), 60 (wire transport), 72f (PostureSurface / runtime.info). NO dep
  on 126a — staged into wave stage 2 for cadence only.
- **What it delivers:** a canonical `wiresurface.Digest()` over the
  name-level wire surface (version + methods + errors + capabilities +
  wire-type names — EXCLUDES field shapes + event-type names); the digest
  returned as an additive `runtime.info.wire_surface_digest` field AND
  stamped into the committed `wire-manifest.gen.json`; a Console app-shell
  status-bar connect-time drift check (the consumer) that surfaces a loud
  signal on a mismatch and an informational note on an absent (old-runtime)
  digest. NO new method, NO version bump, NO field-shape exposure over the
  wire. Generated `types.md` regenerated (D-209).
- **Why STRETCH:** the vendor-and-gate interim needs zero Harbor code, so
  no client is blocked; this adds connect-time (vs build-time) drift
  detection and closes the lockstep mechanism's missing runtime consumer.
  First in the band to cut if V1.6 capacity is tight (recorded cut, D-259).
- **Decision:** D-259.
- **Status:** Pending (V1.6).
```
