---
description: Three drivers — in-memory, SQLite, and Postgres — form the persistence core behind Harbor's storage-shaped interfaces, pass one conformance suite, and carry no per-driver features. How they differ, why there are exactly three, and how the operator chooses at config time.
---

# The persistence triad

Harbor never builds a subsystem against one storage backend. Every persistence-shaped interface in the Runtime — `StateStore`, `ArtifactStore`, `MemoryStore`, `SkillStore` — is defined as an interface first, and every driver behind it passes **one** conformance suite. At the center is the **triad**: in-memory, SQLite, and Postgres. There is no Postgres-only feature and no SQLite-only shortcut — drivers differ in durability and scale, never in behavior.

Designing against three backends from `t=0` is a forcing function, not a convenience: it keeps any single backend's assumptions from leaking into the contract. If a capability cannot be expressed cleanly on an in-memory map, a file-backed SQLite database, and a networked Postgres cluster at the same time, the abstraction is wrong — and you find that out the day you write it, not the day you try to scale.

::: tip Where this is decided
The persistence contract is settled in the RFC. This page distills it; for the binding design read [the RFC](/reference/rfc) (persistence triad in §9, the `StateStore` in §6.11).
:::

## Three drivers, one interface, one conformance suite

| Driver | Backing | Dependency | Use it for |
| --- | --- | --- | --- |
| `inmem` | in-process maps | zero, no CGo | dev loops, tests, and headless embedding |
| `sqlite` | a single file | `modernc.org/sqlite` (pure Go, CGo-free) | the single-binary deployment with durable state on one node |
| `postgres` | a networked database | `pgx` (pure Go) | multi-node deployments that share state across instances |

All three implement the **full** interface and pass the same `conformance.RunSuite`. That suite is the contract — it proves not just round-trip correctness but the multi-isolation guarantee: cross-session and cross-tenant no-leak under concurrent stress. A driver is not "shipped" until it is green against every scenario the other two are green against. Both `sqlite` and `postgres` are compiled into the default binary; the operator selects one at config time without rebuilding.

::: tip Two stores carry extra backends
`StateStore` and `MemoryStore` use the triad directly. `ArtifactStore` adds a filesystem (`fs`) and an S3-compatible (`s3`) driver for large binary payloads, and `SkillStore` uses the `localdb` driver. These are still single-interface, single-conformance-suite seams — the triad is the SQL-persistence core, not a ceiling on artifact- or skill-specific backends. The [configuration reference](/reference/config) lists each subsystem's exact driver set.
:::

::: tip Identity rides through every driver
Each of these stores is identity-scoped. Every method takes the mandatory `(tenant_id, user_id, session_id)` triple and filters on it; a missing component fails closed. The conformance suite enforces that on every driver equally — see [Identity and isolation](/concepts/identity-and-isolation).
:::

## No per-driver features

This is the rule that keeps the triad honest:

> A new optional capability is **a new interface method plus a new conformance scenario** — never per-driver hand-waving.

There is no `Supports*` capability protocol, no `hasattr`-style duck-typing, no "this only works on Postgres" footnote. When all the drivers will implement everything, optional-capability ceremony is a smell. If a feature genuinely cannot land on every driver, that is an RFC conversation about whether it belongs in the interface at all — not a quiet per-driver divergence.

The payoff is operator trust: switching `inmem` to `sqlite` to `postgres` changes durability and scale characteristics, never behavior. The agent that worked on your laptop works the same way in production.

::: details Which driver, when?
A clean progression covers almost every path from first run to production:

- **`inmem` — dev and embed.** Zero dependencies, fastest startup, nothing to clean up. This is what `harbor dev` and headless embedding reach for first. State vanishes on restart, which is exactly what you want while iterating.
- **`sqlite` — the single binary.** One CGo-free static binary plus one file gives you durable state, durable artifacts, durable memory, and durable skills on a single node. No external service to operate. This is the default productionization target for a single-instance deployment.
- **`postgres` — multi-node.** When more than one Runtime instance must share state — horizontal scale, rolling deploys, a shared durable task or event surface — point every instance at the same Postgres database. The `pgx` driver is pure Go, so the binary stays CGo-free.

The progression is the prescriptive default, not a mandate: you can run `sqlite` in dev or `postgres` from day one. But `inmem` → `sqlite` → `postgres` is the path that almost never surprises you.
:::

## Migrations are forward-only

Schema evolution follows two non-negotiable rules:

- **Forward-only, per-driver.** Each backend has its own migration directory; migrations are never edited after they merge — new schema is a new migration. There is no down-migration path. A clean database starts cleanly; an existing one runs only the migrations it has not seen.
- **Apply directly; verify through a transaction pool.** Every Postgres-backed store defaults `migration_mode` to `apply`, which takes a session advisory lock and needs a direct/session-capable endpoint. After a separate apply succeeds, steady-state replicas may use a transaction-pooled DSN with `migration_mode: verify`; that mode only reads `schema_migrations`, performs no DDL/transaction/advisory lock, and fails loud if the ledger is missing or behind the binary's embedded migrations.
- **SQLite uses WAL journal mode.** Write-Ahead Logging is the configured journal mode for the `sqlite` driver, chosen for concurrent-reader behavior under the Runtime's load. Changing it is an RFC decision, not a config knob.

All queries across the SQL drivers are parameterized — no string concatenation into SQL, ever.

::: warning One durability anchor: `state.driver`
Under the executor-delegation model, the memory strategies persist their state through the `StateStore`. That means `state.driver` is the real durability anchor: an `inmem` memory driver on top of a SQL `StateStore` is already durable, while a SQL `memory.dsn` on top of an `inmem` `state.driver` is not. When you productionize, move `state.driver` first. The [config reference](/reference/config) spells out the exact field semantics.
:::

## Configure it

The choice is a handful of `harbor.yaml` fields. Each persistence-shaped subsystem names its driver; the SQL drivers take a connection string (a file path for `sqlite`, a libpq URL for `postgres`).

```yaml
# Durable on a single node: the SQLite single-binary path.
state:
  driver: sqlite
  dsn: ./data/harbor.db        # file path for sqlite

memory:
  driver: sqlite
  dsn: ./data/harbor.db        # but state.driver is the durability anchor

skills:
  driver: localdb              # SkillStore uses the localdb driver
  dsn: ./data/harbor.db
```

```yaml
# Multi-node: every instance shares one Postgres database.
state:
  driver: postgres
  dsn: postgres://harbor:***@db.internal:5432/harbor  # libpq URL
  migration_mode: apply                              # default; direct endpoint

# After the apply deployment succeeds, steady-state replicas can switch both:
# dsn: postgres://harbor:***@pooler.internal:6432/harbor
# migration_mode: verify
```

The defaults are `inmem` everywhere (and the `skills` block is fully optional), so a fresh clone runs with zero storage configuration. A `dsn` is required the moment a driver is anything other than `inmem`, and `dsn` values are treated as secrets — redacted in logs and audit.

For the full field-by-field semantics (defaults, validation, secret handling), see the [configuration reference](/reference/config). For the end-to-end "take this from dev to production" sequence — including which driver to move first and what to verify — follow the [productionization playbook](/reference/productionization-playbook).

## Related

- [The configuration reference](/reference/config) — every persistence field, with defaults and validation.
- [The productionization playbook](/reference/productionization-playbook) — the operator's path from `inmem` to a durable, multi-node deployment.
- [Identity and isolation](/concepts/identity-and-isolation) — the triple every store filters on, and why missing identity fails closed.
- [Memory and skills](/concepts/memory-and-skills) — two subsystems that ride the triad through the `StateStore`.
- [The RFC](/reference/rfc) — the authoritative persistence design.
