# Phase 122 — persistence-and-driver-registry-dedup

## Summary

Consolidates three copy-paste clusters the 2026-06 audit confirmed: the SQLite forward-only migration runner duplicated across the **four conformant** drivers (`state`, `memory`, `artifacts` sqlite + `skills/drivers/localdb`), the Postgres advisory-lock runner across three, and the driver registry/factory `(registered: %s)` boilerplate repeated across **all fifteen** subsystems that carry a per-package `ErrUnknownDriver` sentinel. Extracts a shared `internal/persistence/sqlmigrate` runner and a generic `internal/driverreg.Registry[T]` (Go 1.26 generics), leaving each driver/subsystem a thin caller. The fifth SQLite copy — `tools/drivers/searchcache` — is **materially divergent** (its own `tool_cache_migrations` table, no `BeginTx`, body-side version recording, silent skip on malformed filenames) and is handled separately (see Risks): either conformed first or explicitly out of scope. Pure maintainability — no behaviour change, no new operator surface; the win is one place to fix a migration bug and one canonical factory message **format** (each subsystem keeps its exported sentinel, which `driverreg` wraps so `errors.Is` keeps holding).

## RFC anchor

- RFC §9

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- brief 05 §"Persistence triad — conformance parity": all three drivers per subsystem implement the full interface and run the same conformance suite; their migration machinery is identical-by-intent, so a single shared runner reduces the surface where the three can silently diverge.
- brief 05 §"Forward-only migrations": the runner's invariant (each migration ends with `INSERT OR IGNORE INTO schema_migrations`) and the Postgres advisory-lock key derivation are subtle and currently re-stated in every driver; centralising them makes the invariant enforceable in one place.
- brief 05 §"Driver registry seam (§4.4)": the factory + registry + dispatch-by-name pattern is identical across subsystems; a generic registry yields one canonical message **format** ("unknown driver, registered: [...]") instead of fifteen hand-rolled variants that have already drifted — while each subsystem keeps its own exported `ErrUnknownDriver` sentinel (wrapped by `driverreg`) so `errors.Is(err, <pkg>.ErrUnknownDriver)` still holds.

## Findings I'm departing from (if any)

None.

## Goals

- A single `internal/persistence/sqlmigrate` package runs SQLite forward-only migrations from an `embed.FS` + a name prefix, with the partial-apply-precheck logic in one place; the **four conformant** SQLite drivers (`state`, `memory`, `artifacts`, `skills/localdb`) call it instead of carrying their own copy. The runner preserves their shared contract: `BeginTx` wrapping, `schema_migrations` table, runner-side version recording, and a **loud** error on an unparseable migration filename (`state/drivers/sqlite/migrations.go:95` — never the silent `continue` `searchcache` does).
- The Postgres advisory-lock migration runner is folded into the same package (as a sibling, since the SQLite runner records the version runner-side via `INSERT OR IGNORE` while the Postgres `applyOne` relies on a body-side `INSERT` — opposite version-recording contracts that must stay distinct and each be pinned by a test), with the signed-int64 FNV-64a advisory-key derivation defined once; the three Postgres drivers call it.
- A generic `internal/driverreg.Registry[T]` provides register / resolve / list-registered, emitting one canonical message **format** that names the registered drivers; each subsystem keeps its exported `ErrUnknownDriver` sentinel and `driverreg` **wraps** it so `errors.Is(err, <pkg>.ErrUnknownDriver)` keeps holding. The subsystems migrate onto it without changing their public registration ergonomics (drivers still self-register from `init()`, pulled in via the `internal/drivers/prod` aggregator, D-196).
- Behaviour is provably unchanged: every existing conformance suite, migration test, and registry test passes against the extracted code with no acceptance-criteria changes — including sentinel **identity** (`errors.Is`), not just message format.

## Non-goals

- Any schema change, new migration, or change to migration *content* — this is a runner extraction, not a data change. Existing migrations are immutable (CLAUDE.md §13).
- Changing the `internal/drivers/prod` blank-import aggregation model or the §4.4 seam contract — drivers still self-register; only the shared machinery they call is consolidated.
- Touching the `bootDevStack` decomposition unless it falls out naturally — it is listed as a stretch item below, not a gate.
- Adding `Supports*` capability ceremony — forbidden (§4.4); all drivers implement everything.

## Acceptance criteria

- [ ] `internal/persistence/sqlmigrate` exists and is the only implementation of the SQLite forward-only runner; the **four conformant** copies (`state`, `memory`, `artifacts` sqlite + `skills/drivers/localdb`) call it and no longer carry their own runner body. `searchcache` is EITHER conformed first (its `tool_cache_migrations` table + no-transaction semantics reconciled to the shared contract, proven by its unit test) OR explicitly excluded with a one-line reason — the plan names which, and never claims it "differs only in error-prefix strings."
- [ ] The Postgres advisory-lock runner is consolidated as a sibling; the three Postgres drivers call it; the advisory-key derivation exists in exactly one place; a test pins both version-recording contracts (SQLite runner-side `INSERT OR IGNORE` vs Postgres body-side `INSERT`).
- [ ] `internal/driverreg.Registry[T]` exists; the named subsystem registries use it; the canonical message **format** lists registered names AND every subsystem's `errors.Is(err, <pkg>.ErrUnknownDriver)` still resolves (sentinel wrapped, not replaced).
- [ ] No behaviour change: all persistence conformance suites, migration tests (clean-DB start + existing-DB upgrade), and registry/factory tests pass under `-race`; `searchcache`'s unit test (it has **no** conformance suite — its only gate) is strengthened to assert `tool_cache_migrations` contents + no-transaction semantics; coverage on touched packages stays ≥ target.
- [ ] A clean DB starts cleanly and an existing DB runs the new (extracted) runner identically — proven per-driver, not just for one.
- [ ] The §4.4 seam is preserved: drivers self-register from `init()`; `internal/drivers/prod` remains the single blank-import home; no concrete driver is imported outside the sanctioned places.

## Files added or changed

- `internal/persistence/sqlmigrate/` (new) — shared SQLite runner + Postgres advisory-lock sibling runner + tests.
- `internal/state/drivers/sqlite/migrations.go`, `internal/memory/drivers/sqlite/migrations.go`, `internal/artifacts/drivers/sqlite/migrations.go`, `internal/skills/drivers/localdb/migrations.go` — reduced to thin callers. (`internal/tools/drivers/searchcache/migrations.go` — only if conformed; see Risks.)
- `internal/state/drivers/postgres/migrations.go`, `internal/memory/drivers/postgres/migrations.go`, `internal/artifacts/drivers/postgres/migrations.go` — thin callers.
- `internal/driverreg/` (new) — generic `Registry[T]` + canonical message format that wraps each caller's sentinel.
- The subsystem registry files — these live in per-subsystem `registry.go` (e.g. `internal/state/registry.go`, `internal/memory/registry.go`, `internal/artifacts/registry.go`) and `internal/skills/skills.go` for skills, **not** a uniform `internal/<area>/<area>.go`; the plan lists the real files for each subsystem migrated. Name the exact subsystems migrated (and the rationale for any of the fifteen left out).
- **(stretch, optional)** `cmd/harbor/cmd_dev.go` — decompose `bootDevStack` along its existing section comments if low-risk; otherwise deferred to a follow-up `refactor` PR.

## Public API surface

- `sqlmigrate.Run(ctx, db, fs embed.FS, prefix string) error` (exact signature finalised at implementation) — internal, consumed by drivers only. If `searchcache` is conformed in, the signature gains options for table name / transaction wrapping / version-recording owner / bad-filename policy rather than hard-coding the conformant-driver contract.
- `driverreg.Registry[T]` with `Register(name string, factory func(...) (T, error))`, `Resolve(name string, ...) (T, error)`, `Registered() []string` — internal; preserves each subsystem's existing public registration entry points as thin wrappers so external behaviour is unchanged.

## Test plan

- **Unit:** `sqlmigrate` runs an `embed.FS` fixture to completion, is idempotent on re-run, and prechecks partial application; `driverreg` resolves, lists, and produces the canonical unknown-driver message; advisory-key derivation is deterministic and signed-int64-safe.
- **Integration:** each touched persistence driver's existing conformance + migration tests pass unchanged against the extracted runner (this is the real regression gate); clean-DB and existing-DB paths per driver, under `-race`.
- **Conformance:** the existing per-subsystem conformance suites are the binding proof of no-behaviour-change; they must pass verbatim.
- **Concurrency / leak:** advisory-lock runner under concurrent start (two instances racing the same DB) acquires the lock correctly — the property the Postgres path exists to guarantee — under `-race`.

## Smoke script additions

- `scripts/smoke/phase-122.sh` (static-only): assert the four conformant SQLite drivers and three Postgres drivers no longer contain a private runner body (grep for the duplicated marker that should now live only in `sqlmigrate`); assert `internal/driverreg` is referenced by the migrated subsystem registry files. This is a refactor with no live surface — behaviour is gated by `go test`, the smoke asserts the dedup actually happened.

## Coverage target

- `internal/persistence/sqlmigrate`: 90% (persistence-driver tier).
- `internal/driverreg`: 85%.
- Touched driver/factory packages: maintain existing targets (no regression).

## Dependencies

- 15 / 16 (SQLite + Postgres StateStore drivers — the canonical migration-runner shape)
- 18 (artifact SQLite/Postgres-blob drivers)
- 25 (SQLite + Postgres memory drivers)
- 37 / 41 (skills `localdb` driver — a migrated SQLite caller)
- 107c (`tools/drivers/searchcache` — only if conformed into the shared runner in this phase)
- 110c (the `internal/drivers/prod` aggregator the registries feed into, D-196)

## Risks / open questions

- **searchcache is materially divergent, not "error-prefix only" (MAJOR):** `searchcache/migrations.go` uses a bare `db.ExecContext` (no `BeginTx`), its own `tool_cache_migrations` table (no `applied_at`), records the version from the SQL body (not the runner), and silently `continue`s on a malformed filename — versus the conformant drivers' `BeginTx` + `schema_migrations` + runner-side recording + a **loud** `fmt.Errorf` on an unparseable version. The proposed `Run(ctx, db, fs, prefix)` signature cannot express table-name / transaction / version-owner / bad-filename policy. **Resolution:** EITHER exclude searchcache (scope the phase to "four SQLite copies") OR conform it first in a separate step and expand the signature to parameterise (table name, txn wrapping, version-recording owner, bad-filename = error). The plan must pick one before dispatch and must not erase searchcache's behaviour to fit the runner. **Proposed D-253** records the extraction, the chosen searchcache disposition, and the sentinel-wrapping registry contract.
- **Registry scope is the fifteen `(registered: %s)` factories, not "five":** name the exact subsystems migrated this phase and the rationale for any deferred. "One canonical message" is only literally true if all fifteen migrate — so the goal is one canonical message **FORMAT**, with each subsystem's exported sentinel preserved and wrapped (assert `errors.Is` identity, not just rendered text).
- **Generics ergonomics:** `Registry[T]` must not force callers to change their `init()`-time registration ergonomics; preserve each subsystem's existing register function as a thin typed wrapper so the §4.4 self-registration pattern and the `prod` aggregator are untouched.
- **No migration edits:** the extraction moves runner *code*, never migration *files*; the append-only migration rule (§13) is not in play, but reviewers must confirm no migration content moved or changed.
- **`bootDevStack` is stretch, not gate:** if decomposing it risks the live devstack wiring, defer it — it has no correctness urgency.
- RFC §11: none directly.

## Glossary additions

- **`sqlmigrate`** — the shared forward-only SQLite/Postgres migration runner; the single home of the partial-apply precheck and the Postgres advisory-key derivation. (Add to `docs/glossary.md`.)
- **`driverreg.Registry[T]`** — the generic driver registry/factory backing the §4.4 seam across subsystems; source of the canonical unknown-driver message **format**, wrapping each subsystem's own exported `ErrUnknownDriver` sentinel so `errors.Is` still resolves per-package. (Add to `docs/glossary.md`.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — no identity-scoping logic changes; the conformance suites already assert isolation and must still pass
- [ ] **If this phase builds a reusable artifact:** `sqlmigrate` and `driverreg.Registry[T]` are construct-once/use-many — a concurrent-use test (registry resolved + runner invoked from N goroutines under `-race`) asserts no race; the advisory-lock concurrent-start test covers the Postgres path.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** the per-driver conformance + migration suites are the integration gate and pass unchanged under `-race`, including the existing-DB-upgrade failure path.
- [ ] If new vocabulary: glossary updated (`sqlmigrate`, `driverreg.Registry[T]`)
- [ ] If a brief finding was departed from: justified + decisions.md entry — N/A; proposed D-253 filed at implementation
