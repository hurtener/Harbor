# Phase 201 — Skills Postgres driver (durable/shared storage)

## Summary

Add a `postgres` driver to the skills subsystem so skills persist in shared, durable Postgres instead of only a per-instance SQLite file. The driver sits behind the existing `SkillStore` interface, self-registers into the `internal/drivers/prod` aggregator, carries its own forward-only migrations, and passes the existing `internal/skills/conformancetest` suite unchanged — bringing skills to the §9 three-driver parity every other persistence-shaped subsystem already has.

## RFC anchor

- RFC §6.7
- RFC §9

## Briefs informing this phase

- brief 04
- brief 05

## Brief findings incorporated

- brief 04 §4.3: the skills store schema carries the `Origin / OriginRef / ContentHash` lifecycle columns + a full-text search surface over `name | title | trigger | description | tags`; the Postgres driver mirrors this schema (Postgres FTS `tsvector`/`GIN` in place of SQLite FTS5) so search parity holds.
- brief 04 §4.4 + §5.7: the three-tier ranking ladder (FTS → regex → exact) must rank deterministically regardless of backend; the Postgres driver's FTS path feeds the same ranking ladder + is gated by the shared golden-ranking conformance test (no backend-specific ranking).
- brief 05 §"Persistence triad — interfaces + conformance": a persistence subsystem's drivers prove parity through ONE shared conformance suite, not per-driver bespoke tests; the Postgres driver is validated by running `conformancetest.Run` against it — no new suite.

## Findings I'm departing from (if any)

None. Straight §9 parity add behind the existing seam.

## Goals

- Skills persist durably in shared Postgres for multi-instance deployments.
- The Postgres driver passes the existing skills conformance suite unchanged (behavioral parity with `localdb`).
- Search + conflict-policy + identity-scoping behave identically to `localdb` (proven by the shared suite + golden-ranking test).

## Non-goals

- No `SkillStore` interface change; no `Supports*` capability ceremony (§4.4 — every driver implements everything).
- No change to `localdb` (stays the default backend; backward-compatible).
- No new Protocol / wire surface (persistence-internal).

## Acceptance criteria

- [x] `internal/skills/drivers/postgres/` implements `SkillStore` fully (upsert/get/list/search/delete + the conflict policy), `pgx`, all queries parameterized, identity-triple-scoped `WHERE` filters (§6).
- [x] The driver self-registers from `init()` and is blank-imported in `internal/drivers/prod/prod.go` (next to the `localdb` line, D-196); nothing else imports it.
- [x] Forward-only per-driver migrations (§9), each idempotent (`... ON CONFLICT DO NOTHING` into `schema_migrations`); clean DB starts cleanly; existing DB runs the new migration — both via test.
- [x] The driver PASSES `internal/skills/conformancetest.Run` unchanged (parity), including the golden-ranking / FTS-fallback assertions.
- [x] D-025 concurrent-reuse test: N≥100 concurrent invocations against one shared driver instance under `-race` — no races, no cross-identity bleed, no goroutine leak.
- [x] `scripts/smoke/phase-201.sh` asserts the `postgres` skills driver registers (present in the aggregator; the conformance suite covers behavior).

## Files added or changed

```text
internal/skills/drivers/postgres/postgres.go            # SkillStore impl (pgx)
internal/skills/drivers/postgres/search.go              # tsvector/GIN FTS feeding the shared ranking ladder
internal/skills/drivers/postgres/migrations/            # forward-only per-driver migrations
internal/skills/drivers/postgres/migrations.go          # embed + apply
internal/skills/drivers/postgres/postgres_test.go       # conformancetest.Run(t, factory) + migration + D-025 concurrent-reuse
internal/drivers/prod/prod.go                           # blank import of the postgres skills driver (next to localdb)
examples/harbor.yaml                                    # documented skills backend: postgres option
docs/skills/configure-memory-and-skills/SKILL.md        # note the durable Postgres skills backend (surface: memory)
scripts/smoke/phase-201.sh                              # NEW — postgres skills driver registered
docs/plans/phase-201-skills-postgres-driver.md          # this plan
```

## Public API surface

No new exported API. A new registered driver name (`postgres`) on the existing skills backend seam.

## Test plan

- **Unit:** driver-local edge cases (migration idempotency, FTS availability, conflict policy) not already covered by the shared suite.
- **Integration:** `conformancetest.Run(t, postgresFactory)` — the full shared suite against a real Postgres (`dockertest`/`postgres:16`, the same harness state/memory Postgres drivers use); identity-scoping + ranking parity proven here.
- **Conformance:** the shared `internal/skills/conformancetest` suite IS the conformance gate — the driver must pass it unchanged.
- **Concurrency / leak:** D-025 N≥100 shared-instance under `-race`.

## Smoke script additions

- `scripts/smoke/phase-201.sh` (`PREFLIGHT_REQUIRES: static-only`): assert `internal/skills/drivers/postgres` exists and its blank import is present in `internal/drivers/prod/prod.go`; the behavioral gate is the conformance suite (CI), not the smoke.

## Coverage target

- `internal/skills/drivers/postgres`: ≥ 80% (the shared conformance suite drives most of it).

## Dependencies

- Gate-0 (D-344). Builds on the shipped skills `SkillStore` + `conformancetest` + `localdb` driver, and the established Postgres driver harness used by state/memory (§9). All on `dev-experimental`.

## Risks / open questions

- SQLite FTS5 vs Postgres FTS ranking must produce the SAME ordering the golden-ranking conformance test asserts — the ranking ladder is backend-agnostic by design (scores normalized 0..1); the Postgres FTS path feeds raw scores into the same ladder. Verified by the shared golden test running against both drivers.

## Glossary additions

- N/A — no new vocabulary (a driver behind an existing, glossary-covered seam).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: identity-scoped `WHERE` filters proven by the conformance suite's isolation assertions
- [x] **Concurrent-reuse test passes** — N≥100 shared-instance under `-race` (the driver is a reusable artifact). See §5 + D-025.
- [x] **Integration test exists** — `conformancetest.Run` against real Postgres (parity gate)
- [x] If migrations added: clean DB starts cleanly; existing DB runs the migration — both via tests
- [x] If config schema changed: `examples/harbor.yaml` updated; backward compatible (`localdb` stays default)
- [x] Skill `configure-memory-and-skills` updated same-PR (§18)
