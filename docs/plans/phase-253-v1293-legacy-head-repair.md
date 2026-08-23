# Phase 253 — v1.29.3 offline legacy-head integrity repair (HA-69 compatibility extension)

## Summary

Deliver a Harbor-native, offline repair surface for legacy durable event heads
that contain redundant sequence references. The command inspects a stopped and
explicitly frozen StateStore by default, reports only content-free evidence,
and applies a validated first-occurrence canonicalization through the shared
StateStore conditional-write contract; it never edits immutable event bodies.

This is the second compatibility extension of HA-69. It preserves the
v1.29.2 fail-closed recovery contract: ambiguous, malformed, mismatched, or
moving data is refused rather than guessed at, and the same operation is safe
to replay after a lost response.

## RFC anchor

- RFC §4.3
- RFC §6.11
- RFC §6.13
- RFC §8
- RFC §12

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §5: StateStore bytes remain opaque to generic persistence code;
  durable event repair must validate its typed body and storage contract at the
  durable driver boundary.
- brief 05 §6: each persistence driver needs conformance coverage for
  identity, conditional writes, cancellation, and failure behavior rather than
  relying on one backend's implementation.
- brief 06 §4: canonical durable event records remain the source of truth;
  metadata and repair projections must not invent or rewrite event bodies.
- brief 06 §5: malformed or ambiguous durable data fails loudly and leaves the
  operator an auditable remediation path instead of silently dropping history.

## Findings I'm departing from (if any)

None.

## Goals

- Provide `harbor events repair-legacy-heads` as an offline operator command
  that does not boot the runtime or event writer.
- Require an explicit freeze/drain acknowledgement for any mutating mode and
  refuse every detectable transaction-pooled/PgBouncer destination; apply
  mutations only through a direct, session-affine PostgreSQL `5432` endpoint.
- Make inspect/dry-run the default and keep all command output and receipts
  content-free: counts, positions, generations, stable record identifiers or
  hashes, entry hashes, and integrity outcomes only.
- Validate every duplicate occurrence against one immutable entry slot,
  canonical event sequence, exact storage and event tenant/user/session
  identity, kind, event type, and any existing metadata projection. Preserve a
  non-empty payload RunID even when the session-scoped storage key uses
  `RunID=""`.
- Canonicalize only redundant head references and their duplicate metadata
  projections with StateStore conditional generation/CAS. Never mutate or
  delete immutable entry bodies, and retry only after a clean bounded re-read.
- Emit a durable, content-free receipt/manifest with before/after head hashes,
  generations, duplicate sequences/positions, immutable entry hashes, tool
  version, and outcome. A second apply is a byte-identical no-op.
- Keep repair behavior conformant across in-memory, SQLite, and PostgreSQL
  StateStores, including cancellation, concurrent repairers, response-loss
  replay, and real direct-PostgreSQL acceptance.

## Non-goals

- Automatically healing ambiguous corruption, missing entries, body or
  identity mismatches, non-canonical ordering, or concurrent changes.
- Rewriting, deleting, or decoding-and-reencoding immutable event bodies.
- Relaxing durable boot/recovery fail-closed behavior.
- Mutating downstream databases, changing deployment/platform settings, or
  requiring a database plan upgrade.
- Running local preflight; hosted CI owns broad preflight and real-Postgres
  acceptance.

## Release evidence (2026-08-22)

Implementation PR #730 merged at `dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`.
Hosted candidate run `32600083120` and documentation run `32600083113`
completed successfully. The immutable annotated `v1.29.3` tag object is
`eeae1f44f4fb7d862f581f9cbbabb40a7827146a` and peels to
`dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`; release workflow `32602553267`
succeeded, publishing 13 assets with verified aggregate and six sidecar
checksums and six attestations at
`https://github.com/hurtener/Harbor/releases/tag/v1.29.3`. The native
darwin/arm64 artifact reports v1.29.3, Protocol 0.1.0, build `dabbcff4`;
module provenance records
`Sum=h1:uRya1FQV+hu4YKH5jzQDVP0z0wqOnH6DnsOe8M7oxog=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=dabbcff4f0bbadf7d5710d0b8844b639512ca4ac`, and
`Origin.Ref=refs/tags/v1.29.3`. The post-tag scaffold pin and golden fixtures
are complete. Local `make preflight` was never run, and no downstream fleet
repair or cutover is claimed.

## Acceptance criteria

- [x] The command loads only explicit driver/DSN and repair options; it never
      assembles or boots a Harbor Runtime/EventBus before inspection.
- [x] Inspect is the default and performs no StateStore writes. Apply requires
      an explicit freeze/drain acknowledgement and refuses missing or invalid
      acknowledgement before opening a mutating database handle.
- [x] Apply refuses URL and keyword DSNs that identify PgBouncer/transaction
      pooling or port `6432`; direct session-affine PostgreSQL `5432` is
      required for PostgreSQL mutation.
- [x] A whole-store scan is bounded, cancellable, and content-free. It uses
      the StateStore bounded-enumeration seam with a `max-heads+1` refusal
      guard, and reports aggregate affected-head, duplicate-sequence, and
      redundant-reference counts without retaining or logging event bodies or
      identity values.
- [x] The exact legacy fixture with two adjacent duplicate sequence values in
      one head is detected, repaired by retaining the first occurrence, and
      leaves immutable entry bodies unchanged.
- [x] Non-adjacent duplicates, multiple duplicate values, metadata-ready heads,
      missing/mismatched entries, sequence/triple/type/storage-kind mismatch,
      malformed JSON, checksum divergence, non-canonical ordering, and moving
      generations all fail closed with zero partial writes.
- [x] The session-scoped storage `RunID=""` exception remains compatible with
      v1.29.2: each event body's authoritative RunID survives repair and is
      never compared to the empty storage-key RunID.
- [x] Apply writes a content-free durable receipt/manifest atomically with the
      canonical head update, records expected/applied generations and hashes,
      and replays idempotently after response loss or a second invocation.
- [x] In-memory, SQLite, and PostgreSQL StateStore conformance covers CAS,
      cancellation, N concurrent attempts, and no partial publication; the
      direct-5432 PostgreSQL acceptance is selected by hosted CI with an exact
      non-vacuous PASS assertion and no skip accepted.
- [x] Operator documentation requires stop-before-repair, backup/rollback,
      inspect → apply → verify, and repair before admitting any v1.29.2+
      event writer.

## Files added or changed

- `internal/events/drivers/durable/repair.go`, `record.go`, and focused durable
  repair/recovery tests (including the driver-specific acceptance fixture)
- `internal/state/state.go` and bounded-enumeration driver conformance/tests
  (`internal/state/conformancetest`, `internal/state/drivers/{inmem,sqlite,postgres}`)
- StateStore compatibility expectations (`internal/governance/statefail_test.go`)
- `internal/distributed/drivers/durable/internal_test.go` and the affected
  StateStore/task/integration compatibility fixtures
- `cmd/harbor/root.go`, `cmd/harbor/` repair command, focused CLI tests, and
  golden help/scaffold fixtures
- `cmd/harbor/testdata/golden/help.txt`
- `docs/plans/phase-253-v1293-legacy-head-repair.md`
- `docs/decisions.md`
- `docs/notes/downstream-asks.md`
- `docs/recipes/repair-legacy-durable-heads.md`
- `docs/recipes/README.md`
- `docs/site/recipes/repair-legacy-durable-heads.md`
- `docs/site/.vitepress/config.ts`
- `docs/CONFIG.md`
- `examples/harbor.yaml`
- `cmd/harbor/init/templates/default/harbor.yaml.tmpl`
- `scripts/smoke/phase-253.sh`
- `.github/workflows/ci.yml`
- `CHANGELOG.md`

## Public API surface

The operator-facing CLI command is:

```text
harbor events repair-legacy-heads
```

The command accepts explicit `--driver`, `--dsn`, `--mode`, `--freeze-ack`,
`--timeout`, `--max-heads`, `--max-duplicates`, and global `--json` options.
It is an offline administrative surface. It does not add a Protocol method,
wire type, public SDK interface, or runtime boot path. Its durable receipt and
JSON report are versioned and content-free so operators can retain provenance
without persisting payload bytes or identity values in repair artifacts.

The durable receipt binds the following exact content-free fields:

```json
{
  "receipt_version": 1,
  "receipt_id": "<state-record-id>",
  "tool_version": "v1.29.3",
  "head_identity_hash_sha256": "<64 lowercase hex>",
  "before_head_hash_sha256": "<64 lowercase hex>",
  "after_head_hash_sha256": "<64 lowercase hex>",
  "expected_generation": "<state-record-id>",
  "applied_generation": "<state-record-id>",
  "duplicates": [
    {
      "sequence": 21247,
      "positions": [12270, 12271],
      "entry_hash_sha256": "<64 lowercase hex>",
      "payload_metadata_hash_sha256": "<64 lowercase hex>",
      "payload_type": "<event type>",
      "payload_sequence": 21247
    }
  ],
  "outcome": "applied"
}
```

The identity hash is length-delimited and the receipt never contains raw
tenant, user, session, RunID, payload, or body bytes.

## Test plan

- **Unit:** duplicate classification, exact entry/body/identity validation,
  first-occurrence canonicalization, metadata/checksum recomputation, receipt
  schema, DSN/acknowledgement validation, and content-free output.
- **Integration:** CLI option/DSN loading without Runtime boot; bounded
  in-memory and SQLite StateStore repair through the shared contract;
  inspect/apply/verify workflow and restart/idempotent replay.
- **Conformance:** shared repair contract across in-memory, SQLite, and
  PostgreSQL StateStores, including the exact legacy fixture and refusal cases.
- **Concurrency / leak:** bounded scan cancellation, rows/connection close,
  changed-generation CAS race, response-loss replay, and N concurrent repair
  attempts under `-race`.
- **Real PostgreSQL:** hosted `state-postgres` runs the exact direct-5432
  repair acceptance selector and rejects an omitted, renamed, skipped, or
  otherwise vacuous selector.

## Smoke script additions

`scripts/smoke/phase-253.sh` statically verifies the phase plan, D-433,
HA-69's second compatibility-extension registration, the bounded
`ListKindBounded` StateStore seam, the operator recipe and config/scaffold
guidance, the content-free/direct-5432/freeze acknowledgement contract, and
the exact hosted state-postgres selector and PASS-line guard. It never opens a
database or runs local preflight.

## Coverage target

- `internal/events/drivers/durable`: existing package floor, with all repair
  branches and adversarial validation covered under `go test -race`.
- `cmd/harbor`: existing package floor, with command invocation, no-runtime
  boot, acknowledgement, and DSN refusal paths covered.
- Hosted PostgreSQL repair acceptance: exact selector must execute with a
  non-vacuous PASS line; a missing DSN may skip only in local developer runs,
  never in the hosted `state-postgres` job.
- Documentation/smoke: `make drift-audit`, markdown validation, mirror, and
  phase smoke pass; local `make preflight` is intentionally not run.

## Dependencies

- Phase 251 / HA-69 / D-431 durable index, pool, migration, and StateStore
  atomicity contracts.
- Phase 252 / D-432 session-scoped legacy backfill compatibility.
- Phase 16 PostgreSQL StateStore driver and Phase 233 conditional StateStore
  writes.
- D-294 and D-305 event-list/replay and metadata projection contracts.

## Risks / open questions

- Large fleets may have thousands of legacy heads. The scan must not use an
  unbounded row/body materialization path; if the existing StateStore
  enumeration cannot provide a global bounded cursor, the implementation must
  add a driver-neutral bounded maintenance seam or document the hard bound and
  fail before exceeding it.
- A source can contain more corruption than duplicate references. The repair
  tool must preserve fail-closed recovery and produce a diagnostic identifying
  the content-free reason without attempting a best-effort rewrite.
- The hosted real-Postgres selector is fixed at
  `./internal/events/drivers/durable -run
  '^TestLegacyRepair_PostgresDirect5432Acceptance$'`; the workflow must retain
  the exact PASS-line assertion and fail on skipped/no-test execution rather
  than widening to a vacuous package pattern.

## Glossary additions

- **Legacy-head repair:** an offline, conditional canonicalization of a
  historical durable head that removes only validated redundant references.
- **Content-free repair receipt:** a durable repair result containing hashes,
  counts, positions, generations, and outcome, but no payload or identity
  bytes.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] Hosted preflight/CI passes (local `make preflight` intentionally not run)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages meets the stated target
- [x] Multi-isolation repair tests cover tenant/user/session and RunID
- [x] No reusable artifact or new goroutine path is introduced without the
      required concurrent-reuse/leak coverage
- [x] Shared StateStore integration/conformance test runs under `-race`
- [x] New vocabulary is documented in this plan and the glossary if retained
- [x] No brief finding was departed from
