# Phase 208 — reconciled artifact read key

## Summary

`ArtifactScope.TaskID` becomes a provenance annotation and the artifact read key becomes the isolation triple: `Get` / `GetRef` / `Exists` / `Delete` resolve on `(tenant, user, session, id)` across all five drivers, while `List` keeps `TaskID` as a filter and gains the tenant precondition it was the only method to lack. This closes the enumerate-then-fail divergence where the `<session_artifacts>` manifest listed refs a read resolved as not-found, and it ships the §17.6 fix that makes that manifest's invitation true rather than deferring it.

## RFC anchor

- RFC §6.5 — the heavy-content safety net whose write half this gives a coherent read side.
- RFC §6.10 — the Artifacts subsystem: the `ArtifactStore` interface, the content-addressed id, the scope tuple.
- RFC §9 — persistence: five drivers, conformance parity, forward-only migrations.

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- **brief 05 (artifact scoping):** artifacts are keyed on the identity tuple, and a task is a runtime entity that lives inside that tuple rather than beside it. The phase takes this literally: the read key becomes the tuple + id, and the task moves onto the ref as metadata. The flat scope struct decoupled from `internal/identity` is preserved unchanged.
- **brief 05 (content-addressed identity):** `ID = {namespace}_{sha256_hex[:12]}` is derived from the bytes. This is the finding that makes the narrowing safe rather than merely convenient — within one session two artifacts sharing an id ARE the same bytes, so removing a field from the read key cannot merge distinct content. It is also the finding that makes first-writer provenance inherent rather than a shortfall: "which run produced these bytes" has no single answer once two runs produce them.
- **brief 05 (driver conformance parity):** a behaviour that holds on one driver and not another is a design smell, not a driver limitation. This is why the `s3` concurrency divergence was fixed at the layout rather than excused in the suite — see "Findings I'm departing from".
- **brief 06 (isolation-triple filtering by default):** a query that can span tenants is an explicit, audited operation. `List`'s missing precondition made a store-boundary all-tenants filter reachable by omission rather than by decision; the phase closes that at the store while leaving the audited cross-tenant path exactly where the Protocol edge already put it.

## Findings I'm departing from

Two, both from D-347, both recorded in `docs/decisions.md` under D-352.

1. **D-347 says `List` "validates `(tenant, user, session)` like its siblings". This phase requires the TENANT only.** The stricter rule would break two live surfaces rather than close a gap: `internal/protocol/artifacts.go::handleList` deliberately permits an empty User / Session ("only Tenant is mandatory for a list"), `types.ArtifactsListRequest.Scope`'s wire godoc publishes that contract to every Protocol client, and `internal/search/artifacts/index.go` lists with a tenant-only scope. Requiring the triple would silently turn the Console's artifacts page and the artifact search index into empty answers — a Protocol behaviour change this phase has no mandate for and phase 209 owns the surface for. The named bug (a scope with NO tenant reaching storage as a cross-tenant filter) is closed either way.
2. **D-347 says "the other four hold the task in an index key or a `WHERE` clause" and sizes `fs` as the sole outlier. `s3` is a second path-encoding driver, and its answer had to be stronger.** `s3` encodes the task in the object key exactly as `fs` encodes it in the path. Unlike `fs` it has no index to indirect through and no atomic compare-and-set, so a probe-then-write dedup lets N concurrent writers of identical bytes create N objects. Its key layout therefore MOVES to `.../<session>/<namespace>/<id>`; `fs` keeps its layout and resolves across task directories through its index, as D-347 describes.

## Goals

- `Get` / `GetRef` / `Exists` / `Delete` resolve on the isolation triple plus the id on all five drivers; `scope.TaskID` takes no part in resolution.
- `List` stays a predicate: `TaskID` remains a filter, every field below the tenant remains a wildcard, and the tenant becomes mandatory.
- The interface godoc distinguishes a predicate over a result set from an identity, and states the first-writer property AT the filter rather than leaving it to be inferred.
- `ScopedArtifacts.GetRef` compares the triple (a code change), and its godoc stops claiming five methods scope-check when one does.
- Artifacts already on disk / in a bucket / in a table stay readable, and a `Delete` on the triple leaves no copy a later read can resolve.
- The session-artifact manifest's invitation is true end-to-end, proven by a test that fetches every listed row.

## Non-goals

- **The `artifacts.get` Protocol method, the operator fetch ceiling and byte-offset windowing** — D-347 parts 1/3/4/8, owned by phase 209. This phase is ZERO-WIRE: no method, wire type, error code, canonical event, or `ProtocolVersion` movement.
- **Pass-by-reference routing and the substitution invariant** — D-347 parts 5/6/7, owned by phase 210.
- **Making `TaskID` revisable, or adding a metadata-update method.** First-writer provenance is accepted as a property of content-addressed storage, not worked around.
- **Changing the content-addressed id shape.** MIME joining the id is named in D-347 as an RFC-level prerequisite for something else entirely.
- **An `fs` layout change.** `fs` has an index and a mutex; it does not need one.

## Acceptance criteria

- [x] `Get` / `GetRef` / `Exists` / `Delete` return the same artifact for a caller with an empty `TaskID`, the producer's `TaskID`, and a third run's `TaskID`, on every driver.
- [x] A re-Put of identical bytes under a differing `TaskID` yields ONE artifact whose `Scope.TaskID` is the first writer's, and a `TaskID` filter naming the second writer returns zero rows.
- [x] A cross-session id answers not-found on `Get` / `GetRef` / `Exists`, and a cross-session `Delete` is a no-op that leaves the owner's artifact intact.
- [x] `List` returns wrapped `ErrIdentityRequired` for a filter with no tenant, and keeps wildcard semantics for every field below it.
- [x] `ScopedArtifacts.GetRef` resolves a ref carrying a sibling run's stamp, and still returns `ErrScopeMismatch` for a ref from a different triple.
- [x] `ScopedArtifacts.List` returns an artifact a sibling run in its session wrote.
- [x] `fs`: duplicate task directories collapse deterministically (smallest `TaskID`) and identically across restarts; `Delete` sweeps every task directory so a restart cannot resurrect the artifact.
- [x] `s3`: an object written under the legacy task-nested key stays readable, dedups a later Put, and is swept by `Delete`; N=128 concurrent same-bytes Puts under distinct tasks converge on ONE object.
- [x] `sqlite` / `postgres`: migration `0002` applied over a hand-seeded v1 schema collapses a duplicate pair onto the smallest task and leaves a sibling session's row untouched.
- [x] D-025: N=128 concurrent Puts of identical bytes under distinct tasks against one shared instance, under `-race`, converge on one artifact with no goroutine leak.
- [x] `test/integration/artifact_read_key_test.go` fetches every manifest row through a real catalog on two real drivers, proves cross-session / cross-user / cross-tenant refusal through the same tool, and runs 32 concurrent sessions with no cross-talk.
- [x] `go test -race ./...` green; `internal/artifacts` coverage ≥ 90%.

## Files added or changed

```text
internal/artifacts/
├── artifacts.go                       # Triple / EqualTriple / ValidateFilter; key-vs-filter godoc
├── artifacts_test.go                  # + 3 scope-contract tests
├── scoped.go                          # GetRef compares the triple; List drops the facade task
├── scoped_test.go                     # + sibling-stamp + facade-List tests
├── conformancetest/conformancetest.go # + 6 rows (see Test plan)
└── drivers/
    ├── inmem/inmem.go                 # indexKey drops Task; List precondition
    ├── fs/fs.go                       # index keys on the triple; paths from the stored ref;
    │                                  #   removeEveryCopy + taskDirs; deterministic rebuild collapse
    ├── fs/legacy_layout_test.go       # NEW — hand-built duplicate layout + delete sweep
    ├── sqlite/sqlite.go               # WHERE / ON CONFLICT on the triple; List precondition
    ├── sqlite/migrations/0002_read_key_is_the_triple.sql   # NEW
    ├── sqlite/migration_test.go       # + hand-seeded v1 schema collapse test
    ├── postgres/postgres.go           # same as sqlite
    ├── postgres/migrations/0002_read_key_is_the_triple.sql # NEW
    ├── postgres/migration_test.go     # + hand-seeded v1 schema collapse test
    ├── s3/s3.go                       # object key drops the task; resolveBlobKey(s); Delete sweep
    └── s3/legacy_layout_test.go       # NEW — legacy key resolution, dedup, delete sweep
internal/llm/materialize.go            # comment only — the run is provenance, not a key
internal/tools/builtin/artifact_fetch.go # comments only — why the empty TaskID is now correct
test/integration/artifact_read_key_test.go # NEW — the §17.6 end-to-end proof
docs/plans/phase-208-reconciled-artifact-read-key.md  # this file
docs/plans/README.md                   # row 208 → Shipped (v1.23) + deviation note
docs/decisions.md                      # D-352
docs/glossary.md                       # ArtifactScope / read key / ArtifactStore updated; + first-writer provenance
scripts/smoke/phase-208.sh             # real assertions
```

## Public API surface

```go
// internal/artifacts
func (s ArtifactScope) Triple() ArtifactScope            // the read key: TaskID cleared
func (s ArtifactScope) EqualTriple(other ArtifactScope) bool
func (s ArtifactScope) ValidateFilter() error            // List's precondition: tenant required
func ValidateFilter(filter ArtifactScope) error
```

`ArtifactStore`'s eight method signatures are unchanged; their CONTRACT changed and the interface godoc carries it. `ArtifactScope.Equal` is unchanged and stays exact.

## Test plan

- **Unit:** `ArtifactScope.Triple` (clears only the stamp, does not mutate the receiver, idempotent); `EqualTriple` vs `Equal` disagreeing on exactly the stamp-only class and agreeing on every isolation boundary; `ValidateFilter` accepting tenant-only and rejecting the four tenant-less shapes, with `Validate` still stricter; `ScopedArtifacts.GetRef` accepting a sibling stamp and refusing three cross-triple shapes; `ScopedArtifacts.List` seeing a sibling run's artifact and not another session's.
- **Integration:** `test/integration/artifact_read_key_test.go` — real `inmem` + `fs` drivers, the production `planner.BuildArtifactManifest`, and `artifact_fetch` reached through a real `tools.ToolCatalog`. Three tests: every manifest row fetches (all three production writer shapes, fetched by a fourth run); cross-session / cross-user / cross-tenant refusal through the same tool with no byte leak (the failure mode); 32 concurrent sessions with per-session content assertions and a goroutine-baseline check.
- **Conformance:** rows added to the shared suite, run by all five drivers — `List_WildcardsWithinTenant` (replacing `List_NilFieldsAreWildcards`), `List_RequiresTenant`, `ReadKey_IgnoresTaskID`, `ReadKey_RePutUnderDifferingTask_FirstWriterWins`, `ReadKey_CrossSession_NotFound`, `Scoped_GetRef_AcceptsSiblingTaskStamp`, `Concurrent_ReconciledKey_DifferingTasks`. Plus per-driver storage-layout tests that construct inputs the interface can no longer produce: `fs` duplicate task directories + delete sweep, `s3` legacy task-nested key resolution + dedup + sweep, `sqlite` / `postgres` hand-seeded v1 schema migration.
- **Concurrency / leak:** `Concurrent_ReconciledKey_DifferingTasks` at N=128 against one shared instance under `-race` (D-025), asserting convergence on one artifact, a settled stamp a racer supplied, every racer able to read the bytes, and a goroutine baseline; the integration test's 32-session run covers the cross-package half (§17.3).

**Verification posture (§17.8).** The `s3` and `postgres` suites are env-gated and skip on a bare `go test`. They were run for real against a throwaway MinIO and Postgres in Docker rather than left skipped — which is the only reason the `s3` concurrency divergence was found at all: it passed every static check and every local run, and failed on the first real backend.

## Smoke script additions

`scripts/smoke/phase-208.sh` (`unit-tests` classification; the phase has no HTTP surface — that is phase 209):

- Runs `go test -race ./internal/artifacts/...` and the integration test's `TestE2E_ArtifactReadKey_*`.
- Static guards, each mutation-verified to go to FAIL (never SKIP) when the thing it guards is broken: no driver's single-row `WHERE` clause carries a `task` predicate; `inmem`'s and `fs`'s `indexKey` carry no `Task` field; the `s3` object key builder does not fold in a task segment; `ScopedArtifacts.GetRef` compares `EqualTriple`; every driver's `List` calls `ValidateFilter`; both `0002` migrations exist and re-key the primary key onto the triple.

## Coverage target

- `internal/artifacts`: 90% (achieved 93.7%).
- `internal/artifacts/drivers/inmem`: 93.0%; `.../fs`: 83.8%; `.../sqlite`: 81.2%; `.../postgres`: 74.8% and `.../s3`: 75.9% with their backends present (1.5% / 0.9% when env-skipped).

## Dependencies

- 17 (the `ArtifactStore` interface + inmem/fs), 18 (sqlite/postgres blob drivers), 19 (s3), 107f (the `<session_artifacts>` manifest that consumes the result).

## Risks / open questions

- **A deployment upgrading a SQLite or Postgres artifact store runs a table rebuild / constraint swap.** Forward-only and transactional, and the collapse is bounded to genuine duplicates, but it is a schema migration on a table that can be large. Called out so an operator sizes it rather than meets it.
- **`s3` pays one session-prefix listing per `Delete` AND per NEW `Put`.** The Delete listing is the sweep; the Put listing is the dedup probe's fallback, reached exactly when the canonical key misses — which is every genuinely new artifact. Skipping it would be cheaper and wrong on a bucket written by an earlier version: the same bytes would be stored again under the new key while the task-nested copy remained, so `List` would return two rows for one id — the divergence this phase exists to remove. The listing is scoped to one session's prefix. If a deployment ever makes this hot, the fix is a driver-side index, not a narrower probe.
- **`s3` buckets written by an earlier build are migrated by being read, never rewritten.** A legacy object stays under its task-nested key indefinitely and is resolved by the fallback scan. That is deliberate — a boot-time bucket rewrite is not something a driver should do — but it means the fallback path is load-bearing for as long as such objects exist, which is why it has its own real-backend tests.
- **First-writer provenance under a concurrent tie is undetermined.** Stated in the interface godoc, the glossary and D-352, and the conformance row is written to assert only what the contract has. No open question; recorded so a future reader does not read the sequential row as a concurrent guarantee.

## Glossary additions

- **First-writer provenance** (new) — added to `docs/glossary.md`.
- Updated in the same PR: **ArtifactScope** (`List`'s tenant precondition), **Artifact read key** (shipped status), **ArtifactStore** (the per-driver layout answers).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`.** `Concurrent_ReconciledKey_DifferingTasks` at N=128 on every driver, plus the pre-existing `Concurrent_PutGet_NoRace`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists.** `test/integration/artifact_read_key_test.go` — real drivers, identity propagation, a failure mode, `-race`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (D-352)
