# Consolidate split PostgreSQL projections safely

Harbor v1.29.1 supports a two-stage rollout. First deploy the release with
the existing distinct `state`, `memory`, `artifacts`, `skills`,
`sessions.turns`, and `observability.rollups` DSNs. The runtime-wide pool
budget applies across those separate pools, so the first boot is safe without
moving data. Consolidate one runtime at a time only after its source and
destination manifests reconcile.

This procedure is non-destructive. Harbor never drops, truncates, or deletes
the source databases. Removing an old database is a separate operator action
after the release has been observed and the rollback window has closed.

## Connection budget

The supported Render Basic-4GB ceiling is `max_connections=103` (the live
server advertises 103). The v1.29.1 defaults are an aggregate runtime budget
of three open connections, one idle connection target, a five-minute maximum
connection lifetime, a finite 30-second idle lifetime, and at most six direct
migration sessions:

```yaml
postgres:
  pool:
    max_open: 3
    max_idle: 1
    conn_max_lifetime: 5m
    conn_max_idle_time: 30s
```

The six direct migration sessions below are an operator/orchestrator rollout
ceiling, not a Harbor runtime configuration field. The planned worst overlap
is nine runtimes × two generations × three open
permits = 54, plus six direct migration sessions, 12 connections reserved for
Pengui/capabilities, and a 25-connection operator reserve: 97 total. Six
connections remain below the hard cap. Steady state is 70. `max_open` is not
multiplied by six when the six DSNs differ; equal canonical DSNs reuse one
runtime-owned pool. Do not mitigate this incident by requiring a plan upgrade.

## 1. Inspect actual source schemas

Use the DSN that currently serves a subsystem. The tool classifies from
`information_schema`, not from an environment-variable name. Inspect each
source before starting any migration session and retain the JSON manifests as
the source-classification evidence:

```bash
harbor postgres cutover \
  --source "$HARBOR_STATE_DSN" \
  --subsystem state \
  --mode inspect \
  --manifest state-source.json

harbor postgres cutover --source "$HARBOR_MEMORY_DSN" \
  --subsystem memory --mode inspect --manifest memory-source.json
harbor postgres cutover --source "$HARBOR_ARTIFACTS_DSN" \
  --subsystem artifacts --mode inspect --manifest artifacts-source.json
harbor postgres cutover --source "$HARBOR_SKILLS_DSN" \
  --subsystem skills --mode inspect --manifest skills-source.json
harbor postgres cutover --source "$HARBOR_TURNS_DSN" \
  --subsystem sessions.turns --mode inspect --manifest turns-source.json
harbor postgres cutover --source "$HARBOR_ROLLUPS_DSN" \
  --subsystem observability.rollups --mode inspect --manifest rollups-source.json
```

The equivalent all-source inspection is available for an operator that has
already confirmed each variable maps to the intended source:

```bash
harbor postgres cutover \
  --source "$HARBOR_STATE_DSN" \
  --subsystem all \
  --mode inspect \
  --manifest runtime-source.json
```

The recognized physical tables are:

| Projection | Required tables |
| --- | --- |
| `state` | `state_records` |
| `memory` | `memory_state` |
| `artifacts` | `artifacts_blobs` |
| `skills` | `skills`, `installed_packages`, `installed_package_supports` |
| `sessions.turns` | `turn_rows`, `turn_sessions` |
| `observability.rollups` | `rollup_rows`, `rollup_checkpoint`, `rollup_fence` |

The exact v1.29.0 false-readiness shape (`schema_migrations(version=1)` plus
`state_records`, without `memory_state`) is reported as
`misprovisioned` for memory. It is treated as empty/misprovisioned memory
only after the operator verifies that no stronger source exists; state rows
are never copied as memory rows.

## 2. Directly apply/adopt each correctly classified split source

Existing v1.29.0 split databases must be adopted before a pooled `verify`
boot. A `verify` boot cannot create the namespaced ledger, identity, or legacy
mirror, and a transaction-pooled connection cannot safely hold Harbor's
session advisory lock. For every source whose inspect manifest is
`legacy_adoptable` (or `empty`), run the normal Harbor boot once with the
corresponding direct `5432` DSNs and `migration_mode: apply`:

```yaml
# /etc/harbor/v1291-direct-apply.yaml
# Merge these blocks into the otherwise normal, non-public migration config.
# Each *_DIRECT_DSN must resolve to the session-capable PostgreSQL endpoint
# on port 5432; replace placeholders through the deployment secret manager.
state:
  driver: postgres
  dsn: ${HARBOR_STATE_DIRECT_DSN}
  migration_mode: apply
memory:
  driver: postgres
  dsn: ${HARBOR_MEMORY_DIRECT_DSN}
  migration_mode: apply
artifacts:
  driver: postgres
  dsn: ${HARBOR_ARTIFACTS_DIRECT_DSN}
  migration_mode: apply
skills:
  driver: postgres
  dsn: ${HARBOR_SKILLS_DIRECT_DSN}
  migration_mode: apply
sessions:
  turns:
    driver: postgres
    dsn: ${HARBOR_TURNS_DIRECT_DSN}
    migration_mode: apply
observability:
  rollups:
    driver: postgres
    dsn: ${HARBOR_ROLLUPS_DIRECT_DSN}
    migration_mode: apply
```

Run this as an isolated, drained Harbor process using the deployment's normal
boot command, keep it bound to loopback, and terminate it after all configured
store migration success lines have been observed:

```bash
harbor serve --config /etc/harbor/v1291-direct-apply.yaml \
  --bind 127.0.0.1:8689
# After state, memory, artifacts, skills, sessions.turns, and (when enabled)
# observability.rollups each report apply/adoption success, send SIGTERM and
# wait for the process to exit cleanly.
```

This one isolated boot runs no more than one migration runner per configured
subsystem (six direct PostgreSQL sessions at most); do not run overlapping
apply boots against the same runtime. The runner adopts a legacy database
only when its observed required tables/columns and generic ledger match the
expected subsystem, then records the canonical ledger, store identity, and
legacy mirror in one transaction. An interruption rolls all three back and a
restart retries the adoption. If any manifest is `misprovisioned` or
`unknown`—including `schema_migrations(version=1)` plus `state_records` when
the requested subsystem is memory—stop. Do not point apply at that source,
do not switch it to verify, and do not copy it; remediate the DSN or perform
an audited cutover from the actual owning database.

After each direct apply/adoption boot succeeds, switch the steady-state DSNs
to their transaction-pooled `6432` endpoints and set `migration_mode: verify`.
The six stores may remain distinct during the first hotfix deployment:

```yaml
state:     { driver: postgres, dsn: ${HARBOR_STATE_POOLED_DSN},     migration_mode: verify }
memory:    { driver: postgres, dsn: ${HARBOR_MEMORY_POOLED_DSN},    migration_mode: verify }
artifacts: { driver: postgres, dsn: ${HARBOR_ARTIFACTS_POOLED_DSN}, migration_mode: verify }
skills:    { driver: postgres, dsn: ${HARBOR_SKILLS_POOLED_DSN},    migration_mode: verify }
sessions:
  turns:   { driver: postgres, dsn: ${HARBOR_TURNS_POOLED_DSN},    migration_mode: verify }
observability:
  rollups: { driver: postgres, dsn: ${HARBOR_ROLLUPS_POOLED_DSN},  migration_mode: verify }
```

`verify` is read-only and may use `6432` only after the direct apply/adoption
step. Keep the six DSNs distinct for this first boot if desired; the runtime
still enforces one aggregate pool budget. Same-DSN consolidation is optional
until the source/destination reconciliation below completes.

## 3. Prepare the destination through direct PostgreSQL

Create the destination database and point all six compatible stores for this
runtime at its one canonical DSN. Run migration apply/bootstrap through a
direct, session-affine PostgreSQL endpoint on port `5432`. The migration
runner creates and verifies the namespaced ledgers:

- `harbor_schema_migrations(subsystem, version, filename, checksum_sha256,
  applied_at)` with primary key `(subsystem, version)`;
- `harbor_store_identity(subsystem, schema_version,
  contract_checksum_sha256, created_at, updated_at)`.

Use the same isolated direct-apply boot above with all six compatible
destination DSNs set to the one `HARBOR_UNIFIED_DIRECT_DSN` on `5432`, then
terminate it after all six runners succeed. Do not apply advisory-lock
migrations through transaction-pooled PgBouncer `6432`; if the endpoint is
not provably direct or session-affine, fail closed and obtain the `5432` DSN.
The cutover tool itself never applies migrations.

## 4. Copy and reconcile one runtime

Freeze/drain the runtime so no source writes can race the copy. The command
requires an explicit acknowledgement and rejects a destination that resolves
to transaction-pooled `6432`:

```bash
harbor postgres cutover \
  --source "$HARBOR_STATE_DSN" \
  --destination "$HARBOR_UNIFIED_DIRECT_DSN" \
  --subsystem state \
  --mode copy \
  --freeze-ack \
  --batch-size 256 \
  --manifest state-cutover.json
```

Repeat for each source DSN or run the six-source orchestration wrapper used by
the deployment system. The tool copies raw `BYTEA` bodies and typed columns,
preserves identity keys, revisions, receipts, turn ordering/cursors,
activity/usage payloads, and rollup watermarks/fences. Inserts are resumable
with `ON CONFLICT DO NOTHING`; each bounded destination batch runs in a direct
PostgreSQL transaction with transaction-local `statement_timeout = 2min` and
`lock_timeout = 10s`. The source stream is read in a read-only transaction
with the same server-side bounds. A cancellation or interruption returns a
failure and never emits a successful manifest.

Then verify the destination through read-only traffic. This may use PgBouncer
`6432`:

```bash
harbor postgres cutover \
  --source "$HARBOR_STATE_DSN" \
  --destination "$HARBOR_UNIFIED_VERIFY_DSN" \
  --subsystem state \
  --mode verify \
  --manifest state-verify.json
```

Success requires equal row counts and canonical SHA-256 hashes for every table
and projection. A mismatch, omitted table, moving source, wrong ledger, or
misprovisioned source is a non-zero result. Preserve the source and manifests
for incident/release evidence; do not remove the source until the operator's
independent rollback window is complete.

## Rollback

Rollback is configuration-only: restore the runtime's previous DSNs, keep the
destination and source databases intact, and investigate the manifest or
migration diagnostic. Do not reverse migrations or delete copied rows as part
of this Harbor procedure. A later consolidation retry starts with inspect and
reconciliation; the idempotent copy can resume after the source is frozen
again.

The release hotfix was motivated by production scans that loaded durable
counter events through sequence 1,586–10,403 before cancellation and then
PgBouncer/server `53300` errors (2026-08-22 03:20–03:25Z); artifacts/state
health pings crash-looped during the same exhaustion window at 03:31–03:32Z.
The cutover and pool budget are intended to preserve data while reducing the
connection fan-out that made those unrelated health paths fail.
