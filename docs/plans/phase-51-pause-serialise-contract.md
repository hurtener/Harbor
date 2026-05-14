# Phase 51 — Pause-state serialise contract (fail-loud)

> Wave 9, Stage 2. Closes the fail-loudly serialise contract for the
> pause-record envelope on top of the Phase 50 `Coordinator` +
> `checkpointRecord`.

## Summary

Phase 51 closes the **fail-loudly serialise contract** for the pause
record's own wire envelope. Phase 43 (`internal/planner/trajectory`)
already closed it for the trajectory: `Trajectory.Serialize` fails loud
with `ErrUnserializable{Field}` on any non-JSON-encodable leaf, driven
by a reflective field-path walker. Phase 50 (`internal/runtime/pauseresume`)
propagated `trajectory.ErrUnserializable` verbatim out of `Coordinator.Request`
for the *trajectory* — but the pause record carries one more
caller-controlled, JSON-tree-shaped field, `Payload map[string]any`, and
Phase 50's checkpoint save reached it via a bare `json.Marshal`. A bare
`json.Marshal` on a non-encodable `Payload` leaf returns a plain
`*json.UnsupportedTypeError`: technically loud, but **without the
actionable dotted field path** RFC §3.4 mandates. Phase 51 closes that
gap.

Concretely, Phase 51:

1. **Exports the Phase 43 reflective walker** as
   `trajectory.ValidateEncodable(v any, root string) error` — a reusable
   primitive, so the pause-record contract walks the **same** walker
   rather than re-implementing it (CLAUDE.md §13 anti-parallel rule).
   `Trajectory.Serialize` itself is re-pointed at the exported entry so
   there is exactly one walker entry point.
2. **Ships `pauseresume.SerializeRecord` / `DeserializeRecord`** — the
   fail-loudly pair for the `checkpointRecord` envelope.
   `SerializeRecord` pre-flight-walks the whole envelope via
   `trajectory.ValidateEncodable` (surfacing `trajectory.ErrUnserializable`
   rooted at `PauseRecord.payload.<key>`), and stamps the
   `format_version: 1` field. `DeserializeRecord` enforces that version
   on load — an unknown version surfaces `ErrUnsupportedFormatVersion`.
3. **Routes Phase 50's `saveCheckpoint` / `loadCheckpoint` through the
   pair**, and adds an **unconditional** Payload-encodability check in
   `Coordinator.Request` so a non-encodable Payload fails loud even on
   the no-checkpoint-store path.

The master plan is explicit: **negative tests are the gate**. They are
the acceptance criterion of this phase.

## RFC anchor

- RFC §6.3
- RFC §3.4

## Briefs informing this phase

- brief 02

## Brief findings incorporated

- **brief 02 §4 (Pause-state serialisation — the contract that MUST
  FAIL LOUDLY).** "When a pause record is serialised, `tool_context` is
  wrapped in `try: json.loads(json.dumps(...)) except ...: return None`
  … It silently drops non-serialisable tool context on resume … Bugs
  that follow are extremely hard to diagnose because no error is
  logged." Phase 51 closes this for the pause record's **own** envelope:
  `SerializeRecord` fails loud with `trajectory.ErrUnserializable`
  naming the offending `PauseRecord.payload.<key>` path — there is no
  `try { ... } catch { return nil }`-shaped path. `Coordinator.Request`
  rejects the pause before minting a `Token` or writing a checkpoint;
  the negative tests (`pauserecord_test.go`,
  `pauserecord_contract_test.go`,
  `test/integration/phase51_pause_serialise_test.go`) are the gate.
- **brief 02 §4 (Harbor's contract #1).** "`Trajectory.Serialize()` must
  succeed only if every entry is JSON-encodable; on failure it returns
  `(nil, ErrUnserializable)` naming the offending field path." Phase 51
  extends the *exact same* contract — same error struct, same dotted
  field path, same reflective walker — to the pause-record envelope.
  The walker is **shared** (`trajectory.ValidateEncodable`), not
  forked: a second fail-loudly serialiser would be the CLAUDE.md §13
  two-parallel-implementations anti-pattern that the Wave 8 audit's
  `capfilter` extraction just spent a chore PR killing.
- **brief 02 §4 (Harbor's contract #3 — drivers round-trip the
  trajectory byte-for-byte; a conformance test asserts this).** Phase
  51's `SerializeRecord → DeserializeRecord → SerializeRecord`
  round-trip is byte-stable (`pauserecord_test.go::TestSerializeRecord_RoundTrip_ByteStable`),
  including when the envelope embeds an already-canonical
  `TrajectoryBytes` blob — the byte-stable property is inherited from
  the same canonical-ordering discipline (declaration-order struct
  fields + alphabetised map keys) D-049 pins for the trajectory.

## Findings I'm departing from (if any)

- **No departure from any brief finding.** Phase 51 is a strict
  deepening of brief 02 §4's "MUST FAIL LOUDLY" contract onto a surface
  Phase 50 left half-closed. The one design call that warrants a
  decision entry is the **reuse-vs-fork** call for the Phase 43 walker —
  Phase 51 *exports and shares* the walker (`trajectory.ValidateEncodable`)
  rather than copy-pasting it. That call, plus the `format_version: 1`
  envelope shape and the negative-test-is-the-gate stance, is recorded
  as **D-069**.

## Goals

- Export the Phase 43 reflective JSON-encodability walker as
  `trajectory.ValidateEncodable(v any, root string) error` — the
  reusable primitive behind `Trajectory.Serialize`'s pre-flight pass,
  shared (not forked) by the pause-record contract.
- Ship `pauseresume.SerializeRecord(checkpointRecord) ([]byte, error)`
  and `pauseresume.DeserializeRecord([]byte) (checkpointRecord, error)`
  — the fail-loudly serialise pair for the pause-record envelope.
  `SerializeRecord` fails loud with `trajectory.ErrUnserializable` on
  any non-encodable leaf and stamps `format_version: 1`;
  `DeserializeRecord` rejects a corrupt envelope (`ErrCheckpointCorrupt`)
  or an unrecognised `format_version` (`ErrUnsupportedFormatVersion`).
- Ship the `pauseresume.FormatVersion` constant (= 1, RFC §6.3) and the
  `ErrUnsupportedFormatVersion` sentinel.
- Route Phase 50's `saveCheckpoint` / `loadCheckpoint` through the
  fail-loudly pair (no bare `json.Marshal` / `json.Unmarshal` on the
  envelope).
- Add an **unconditional** Payload-encodability check in
  `Coordinator.Request` — a non-encodable `Payload` fails loud whether
  or not a checkpoint store is configured (the Payload is the pause
  record's wire shape regardless).
- Negative tests are the acceptance gate: non-encodable leaves in the
  Payload (function / channel / complex / nested) surface
  `trajectory.ErrUnserializable` with a field path; never a silent
  drop, never a half-persisted checkpoint;
  `DeserializeRecord` rejects unknown / zero `format_version` and
  corrupt bytes loud.
- Byte-stable round-trip
  (`SerializeRecord → DeserializeRecord → SerializeRecord`) including an
  embedded `TrajectoryBytes` blob — conformance with Phase 43's
  byte-stable contract.
- Integration test (`Deps: 50, 43` → CLAUDE.md §17.1): real
  `state.StateStore` (in-mem + SQLite) wired into the real
  `Coordinator`; conformance-with-phase-43 (the same error type out of
  both the trajectory leg and the Payload leg), no-half-persist, and
  the load-side `format_version` guard.
- Coverage on `internal/runtime/pauseresume` ≥ 90%.

## Non-goals

- **No new `Coordinator` method / no Protocol surface.** Phase 51
  deepens the *serialise contract* on the existing `checkpointRecord`
  envelope; it adds no new `Coordinator` interface method and no
  HTTP/Protocol endpoint. The `task.pause` / `task.resume` Protocol
  methods remain a later phase. `scripts/smoke/phase-51.sh` runs the
  package test suite and skips the (absent) Protocol surface per the
  404/405/501 → SKIP convention.
- **No second persistence-driver seam.** Durability still rides on the
  existing `state.StateStore` (D-067). Phase 51 adds no
  `internal/runtime/pauseresume/drivers/` tree.
- **No second fail-loudly serialiser.** Phase 51 *shares* the Phase 43
  walker via the newly-exported `trajectory.ValidateEncodable`. A
  parallel implementation is the §13 anti-pattern (D-069).
- **No `format_version` migration machinery.** V1 ships exactly
  `format_version: 1`; a future bump is an RFC change. Phase 51 ships
  the *guard* (`ErrUnsupportedFormatVersion`), not a multi-version
  decoder.
- **No fuzzing.** Happy-path / round-trip / negative / format-guard /
  conformance are the gate.
- **No change to the trajectory's own `Serialize` / `Deserialize`
  behaviour.** Phase 51 only *exports* the walker and re-points
  `Serialize`'s internal call at the exported entry — the trajectory's
  observable contract is byte-for-byte unchanged (the full Phase 43
  test suite still passes verbatim).

## Acceptance criteria

- [ ] `internal/planner/trajectory/serialize.go` exports
  `ValidateEncodable(v any, root string) error` — the reflective
  encodability walker, rooted at the caller-supplied `root` prefix.
  `Trajectory.Serialize`'s pre-flight pass is re-pointed at it (one
  walker entry point). The full Phase 43 trajectory test suite still
  passes unchanged.
- [ ] `internal/runtime/pauseresume/pauserecord.go` defines
  `FormatVersion` (= 1), `SerializeRecord(checkpointRecord) ([]byte, error)`,
  and `DeserializeRecord([]byte) (checkpointRecord, error)`.
- [ ] `SerializeRecord` pre-flight-walks the envelope via
  `trajectory.ValidateEncodable(rec, "PauseRecord")` and propagates
  `trajectory.ErrUnserializable` **verbatim** on a non-encodable leaf
  (the load-bearing case: a non-encodable `Payload` value). It stamps
  `format_version` to `FormatVersion` on every write (the version
  field is single-sourced there). On the happy path it returns
  canonical JSON bytes.
- [ ] `DeserializeRecord` rejects empty / malformed bytes with
  `ErrCheckpointCorrupt`, and a `format_version` that is not
  `FormatVersion` (zero/absent **or** a higher unknown version) with
  `ErrUnsupportedFormatVersion` — never a half-decoded record.
- [ ] `internal/runtime/pauseresume/errors.go` adds the
  `ErrUnsupportedFormatVersion` sentinel.
- [ ] `internal/runtime/pauseresume/checkpoint.go`'s `saveCheckpoint`
  routes through `SerializeRecord`; `loadCheckpoint` routes through
  `DeserializeRecord`. The Phase 50 `currentFormatVersion` private
  const is retired in favour of the exported `FormatVersion`.
- [ ] `Coordinator.Request` validates `req.Payload` is JSON-encodable
  via `trajectory.ValidateEncodable` **unconditionally** (before a
  `Token` is minted), so a non-encodable Payload fails loud even with
  no checkpoint store configured. No `Token` minted, no pause recorded,
  no checkpoint written on a failed serialise.
- [ ] **Negative-test gate** (`pauserecord_test.go`, in-package): a
  function / channel / complex / nested-function leaf in the
  `checkpointRecord.Payload` makes `SerializeRecord` return
  `(nil, trajectory.ErrUnserializable)` with a field path naming
  `PauseRecord` + `payload` + the offending key — never half-encoded
  bytes, never `(nil, nil)`.
- [ ] **Format-version guard tests**: `SerializeRecord` stamps the
  current `FormatVersion` regardless of the caller's value;
  `DeserializeRecord` rejects an unknown-higher version, a zero/absent
  version, corrupt bytes, and empty bytes — all loud.
- [ ] **Byte-stable round-trip test**:
  `SerializeRecord → DeserializeRecord → SerializeRecord` is
  byte-identical, including when the envelope embeds an already-canonical
  `TrajectoryBytes` blob (preserved verbatim, not re-marshalled).
- [ ] **§11 mandatory pause/resume serialisation test** through the
  exported `Coordinator` surface (`pauserecord_contract_test.go`,
  black-box): a `PauseRequest` whose `Payload` carries a non-encodable
  leaf → `Coordinator.Request` returns `trajectory.ErrUnserializable`
  (via `errors.As`), mints no `Token`, persists no checkpoint — with
  AND without a checkpoint store configured.
- [ ] **Conformance with Phase 43** (`test/integration/phase51_pause_serialise_test.go`):
  real `state.StateStore` (in-mem + SQLite) wired into the real
  `Coordinator`; a non-encodable leaf in **either** the trajectory
  (Phase 43's surface) or the pause record's `Payload` (Phase 51's
  surface) surfaces the **same** `trajectory.ErrUnserializable` struct
  sentinel out of `Request`; no-half-persist across both drivers; the
  load-side `format_version` guard fires
  `ErrUnsupportedFormatVersion` when a tampered envelope is rehydrated.
  Runs under `-race`.
- [ ] `scripts/smoke/phase-51.sh` runs
  `go test -race ./internal/runtime/pauseresume/...` + the Phase 51
  integration test, asserts `SerializeRecord` / `DeserializeRecord` /
  `FormatVersion` are declared, asserts the import-graph guard (no
  Console import; no parallel persistence-driver tree), and skips the
  (absent) Protocol surface with a reason.
- [ ] `docs/plans/README.md` Phase 51 row flipped `Pending` → `Shipped`;
  root `README.md` status table updated in sorted position.
- [ ] `docs/decisions.md` D-069 filed; `docs/glossary.md` gains
  `format_version (pause record)`, `SerializeRecord / DeserializeRecord`,
  and `trajectory.ValidateEncodable`.
- [ ] Coverage on `internal/runtime/pauseresume` ≥ 90%.

## Files added or changed

```text
docs/plans/phase-51-pause-serialise-contract.md            (new — this file)
docs/plans/README.md                                       (Phase 51 row → Shipped)
docs/decisions.md                                          (D-069 appended)
docs/glossary.md                                           (3 new terms)
README.md                                                  (status table row)
internal/planner/trajectory/serialize.go                   (export ValidateEncodable; re-point Serialize's pre-flight at it)
internal/runtime/pauseresume/pauserecord.go                (new — FormatVersion + SerializeRecord + DeserializeRecord)
internal/runtime/pauseresume/errors.go                     (add ErrUnsupportedFormatVersion)
internal/runtime/pauseresume/checkpoint.go                 (route save/load through the fail-loud pair; retire currentFormatVersion)
internal/runtime/pauseresume/coordinator.go                (unconditional Payload-encodability check in Request; FormatVersion ref)
internal/runtime/pauseresume/pauserecord_test.go           (new — in-package negative-test gate + round-trip + format guard)
internal/runtime/pauseresume/pauserecord_contract_test.go  (new — black-box section 11 test through Coordinator.Request)
internal/planner/trajectory/serialize_walker_test.go       (add ValidateEncodable coverage — exported-entry tests)
test/integration/phase51_pause_serialise_test.go           (new — conformance-with-phase-43 + no-half-persist + format guard)
scripts/smoke/phase-51.sh                                  (new — package test run + symbol/import-graph guards)
```

## Public API surface

```go
package trajectory

// ValidateEncodable reports whether v is fully JSON-encodable, failing
// loud with ErrUnserializable{Field: <dotted.path>} on the FIRST
// non-encodable leaf. The reusable primitive behind Trajectory.Serialize's
// pre-flight pass; root is the field-path prefix the error is rooted at.
func ValidateEncodable(v any, root string) error
```

```go
package pauseresume

// FormatVersion is the pause-record wire-format version (RFC §6.3:
// "JSON with format_version: 1"). Bumping it is an RFC change.
const FormatVersion = 1

// SerializeRecord returns the canonical JSON bytes of a pause-record
// envelope, failing loud (trajectory.ErrUnserializable) on any
// non-JSON-encodable leaf and stamping format_version.
func SerializeRecord(rec checkpointRecord) ([]byte, error)

// DeserializeRecord parses canonical pause-record bytes back into a
// checkpointRecord, failing loud on corrupt bytes (ErrCheckpointCorrupt)
// or an unsupported format_version (ErrUnsupportedFormatVersion).
func DeserializeRecord(b []byte) (checkpointRecord, error)

var (
    // ErrUnsupportedFormatVersion — a pause record carries a
    // format_version this Runtime does not recognise.
    ErrUnsupportedFormatVersion = errors.New("pauseresume: unsupported pause-record format_version")
)
```

(`SerializeRecord` / `DeserializeRecord` take the unexported
`checkpointRecord` — they are exported for the in-package negative-test
gate and for the runtime's own checkpoint path; external callers reach
the contract through `Coordinator.Request` / `Status` / `Resume`.)

## Test plan

- **Negative-test gate (in-package, `pauserecord_test.go`):**
  function / channel / complex / nested-function leaves in
  `checkpointRecord.Payload` → `SerializeRecord` returns
  `(nil, trajectory.ErrUnserializable)` with a `PauseRecord.payload.<key>`
  field path; `TestSerializeRecord_NeverHalfEncodes` asserts no
  partial byte slice escapes alongside an error.
- **Format-version guard (in-package):** `SerializeRecord` stamps
  `FormatVersion` regardless of the caller's value;
  `DeserializeRecord` rejects unknown-higher / zero-absent versions
  (`ErrUnsupportedFormatVersion`) and corrupt / empty bytes
  (`ErrCheckpointCorrupt`).
- **Byte-stable round-trip (in-package):**
  `SerializeRecord → DeserializeRecord → SerializeRecord` is
  byte-identical, with and without an embedded `TrajectoryBytes` blob.
- **Shared-walker proof (in-package):**
  `TestSerializeRecord_SharesTrajectoryWalker` asserts a Payload leaf
  and a trajectory leaf produce the *same* `trajectory.ErrUnserializable`
  struct type — the observable proof the walker is shared, not forked.
- **Section 11 mandatory pause/resume serialisation test (black-box,
  `pauserecord_contract_test.go`):** `Coordinator.Request` with a
  non-encodable `Payload` → `trajectory.ErrUnserializable`, no `Token`,
  no checkpoint — with AND without a checkpoint store. Happy-path
  round-trip through Request → checkpoint → restart → Status.
- **Integration / conformance-with-Phase-43
  (`test/integration/phase51_pause_serialise_test.go`):** real
  `state.StateStore` (in-mem + SQLite) into the real `Coordinator`;
  the trajectory leg and the Payload leg both surface the same
  `trajectory.ErrUnserializable`; no-half-persist across both drivers;
  the `format_version` guard fires on a tampered rehydrated envelope.
  Under `-race`.
- **Trajectory regression:** the full Phase 43 trajectory test suite
  (`serialize_negative_test.go`, `serialize_walker_test.go`,
  `trajectory_test.go`, `toolcontext_test.go`, `concurrent_test.go`)
  passes unchanged — `ValidateEncodable` only *exports* the existing
  walker.
- **Concurrency / leak:** Phase 51 ships **no new reusable artifact** —
  `SerializeRecord` / `DeserializeRecord` / `ValidateEncodable` are
  pure stateless functions (no receiver, no package-level mutable
  state), concurrent-safe by construction. The Phase 50
  `concurrent_test.go` (N≥100 against one shared `Coordinator`) already
  covers the `Coordinator`, and it now exercises the Phase 51
  serialise path on every `Request` — it still passes under `-race`.
  No new D-025 test is required (no new artifact); the existing one is
  the gate.

## Smoke script additions

- `go test -race -count=1 ./internal/runtime/pauseresume/...` passes
  (in-package negative gate + format guard + round-trip + black-box
  `Coordinator` contract + Phase 50's D-025 concurrent-reuse) → `ok`.
- `go test -race -count=1 -run TestE2E_PauseSerialise ./test/integration/...`
  passes (conformance-with-Phase-43 + no-half-persist + format guard,
  real `state.StateStore`) → `ok`.
- Static guard: `internal/runtime/pauseresume/pauserecord.go` declares
  `SerializeRecord`, `DeserializeRecord`, and the `FormatVersion`
  constant → `ok`.
- Import-graph guard: `internal/runtime/pauseresume/` imports no
  Console package and declares no `drivers/` persistence tree
  (durability rides on `state.StateStore` — D-067) → `ok`.
- Skip: Phase 51 ships no Protocol/HTTP surface (the `task.pause` /
  `task.resume` Protocol methods land in a later phase) → `skip` with
  reason, per the 404/405/501 → SKIP convention.

## Coverage target

- `internal/runtime/pauseresume`: 90% (master-plan Phase 51 target).
  Achieved: 94.0%.

## Dependencies

- Phase 50 — `internal/runtime/pauseresume` (the `Coordinator`, the
  opaque `Token`, the `checkpointRecord` envelope with its
  `FormatVersion` hinge, the `state.StateStore`-backed checkpoint path,
  D-067). Phase 51 deepens that envelope's serialise contract.
- Phase 43 — `internal/planner/trajectory` (the fail-loudly `Serialize`
  contract, `ErrUnserializable` / `ErrToolContextLost` struct
  sentinels, the reflective walker, D-049). Phase 51 exports and
  *shares* that walker.

## Risks / open questions

- **§13 two-parallel-implementations risk.** Phase 51's whole job is a
  fail-loudly serialiser, and Phase 43 already ships one. The risk is
  re-implementing a second walker. Mitigation: Phase 51 *exports* the
  Phase 43 walker (`trajectory.ValidateEncodable`) and shares it; the
  shared error type (`trajectory.ErrUnserializable`) out of both the
  trajectory leg and the Payload leg is asserted in
  `TestSerializeRecord_SharesTrajectoryWalker` and
  `TestE2E_PauseSerialise_ConformsWithPhase43` — the observable proof
  the contract is one shape. Recorded as **D-069**.
- **`SerializeRecord` / `DeserializeRecord` take the unexported
  `checkpointRecord`.** Exporting a function with an unexported
  parameter type means external packages cannot *call* it with a
  literal — which is intentional: the contract is reached through the
  `Coordinator`, and the exported functions exist for the runtime's
  own checkpoint path + the in-package negative-test gate. This is not
  a leak; it is the §4.4 "callers depend on the interface" discipline
  applied to a serialise helper.
- **Pre-existing `TestE2E_Wave4_Concurrent_MultiTenant` flake.** A
  Wave 4 streaming/cancellation concurrency test flakes under heavy
  parallel load (`go test -race ./...` with the whole suite competing
  for CPU); it passes 5/5 in isolation and on repeated
  `./test/integration/...` runs. It touches no pauseresume or
  trajectory code path. Per CLAUDE.md §17.6 this is noted in the PR
  description as a genuinely pre-existing, out-of-scope Wave 4 timing
  issue — not masked, not introduced by Phase 51.

## Glossary additions

- **`format_version` (pause record)** — the `format_version` int field
  on the pause-record JSON envelope (RFC §6.3: "JSON with
  `format_version: 1`"). `pauseresume.SerializeRecord` stamps the
  current `pauseresume.FormatVersion` (= 1) on every write;
  `DeserializeRecord` rejects any other value with
  `ErrUnsupportedFormatVersion`. Bumping it is an RFC change.
- **`SerializeRecord` / `DeserializeRecord`** — the Phase 51
  fail-loudly serialise pair for the `pauseresume` pause-record
  envelope. `SerializeRecord` pre-flight-walks the envelope via the
  shared `trajectory.ValidateEncodable` walker and fails loud with
  `trajectory.ErrUnserializable` on any non-encodable leaf;
  `DeserializeRecord` enforces `format_version` on load.
- **`trajectory.ValidateEncodable`** — the Phase 43 reflective
  JSON-encodability walker, exported by Phase 51 as a reusable
  primitive: `ValidateEncodable(v any, root string) error` fails loud
  with `ErrUnserializable{Field}` on the first non-encodable leaf.
  Shared by `Trajectory.Serialize` and `pauseresume.SerializeRecord` —
  one walker, no parallel implementation (§13, D-069).

(All three added to `docs/glossary.md` in this PR.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (94.0% ≥ 90%)
- [ ] If multi-isolation paths changed: cross-session isolation test
  passes — N/A: Phase 51 changes the serialise contract, not the
  identity-scoping; the identity triple still flows unchanged through
  `Request` / the envelope / `Resume`. The Phase 50 + Phase 51
  integration tests assert identity propagation through the envelope.
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test
  passes (N≥100).** N/A — Phase 51 ships only pure stateless functions
  (`SerializeRecord` / `DeserializeRecord` / `ValidateEncodable`).
  Phase 50's `concurrent_test.go` (N≥100 against one shared
  `Coordinator`) now exercises the Phase 51 serialise path on every
  `Request` and still passes under `-race` — the existing test is the
  gate.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
  a cross-subsystem seam: an integration test exists.** Phase 51
  consumes Phase 50 + Phase 43;
  `test/integration/phase51_pause_serialise_test.go` wires real
  `state.StateStore` drivers into the real `Coordinator`.
- [ ] If new vocabulary: glossary updated (3 terms)
- [ ] If a brief finding was departed from: justified above +
  decisions.md entry filed — no brief departure; D-069 records the
  reuse-vs-fork call + the `format_version: 1` envelope + the
  negative-test-is-the-gate stance.
