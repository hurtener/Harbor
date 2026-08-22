# Phase 252 — v1.29.2 durable-event backfill compatibility (HA-69 extension)

## Summary

Deliver the v1.29.2 emergency compatibility fix for durable event metadata
backfill. A v1.29.0 session head and entry records are keyed by the session
triple with `RunID=""`, while each persisted event body retains its actual
non-empty run id; restart adoption must preserve that body-owned run id in the
metadata projection and must not confuse it with a storage-key mismatch.

The phase is a narrow extension of HA-69 / Phase 251. It keeps fail-closed
sequence, tenant/user/session, storage-record, malformed-body, and
metadata/body integrity checks, and adds a real PostgreSQL StateStore
acceptance selector for the hosted `state-postgres` job. No downstream
database or fleet is touched by Harbor.

## RFC anchor

- RFC §6.13
- RFC §6.11
- RFC §4.3
- RFC §12

## Briefs informing this phase

- brief 06
- brief 05

## Brief findings incorporated

- brief 06 §1: the event bus remains the canonical projection and metadata is
  an optimization over canonical rows, not a second source of truth.
- brief 06 §4: replay and list reads must use the same durable records and
  preserve identity, cursor, and ordering semantics across restart.
- brief 05 §5: StateStore bytes remain opaque to the generic persistence layer;
  the durable event driver owns its typed body and projection contract.
- brief 05 §6: persisted migration/backfill paths fail loudly on malformed or
  mismatched data rather than silently dropping or re-scoping it.

## Findings I'm departing from (if any)

None.

## Goals

- Treat durable head and entry storage as session-scoped `(tenant, user,
  session, RunID="")` records.
- During legacy metadata adoption, validate the returned StateRecord storage
  identity and entry kind, exact tenant/user/session identity, and sequence.
- Treat the persisted event body's RunID as authoritative metadata; do not
  compare it with the intentionally empty storage-key RunID.
- Reject malformed JSON/bodies, unknown event types, wrong storage identities,
  sequence or triple mismatches, and metadata/body divergence.
- Prove restart/backfill idempotence, metadata and full-event RunID filters,
  checksum authentication, and canonical-body repair using a realistic
  multi-run fixture.
- Exercise the same fixture through the real PostgreSQL StateStore driver
  under `HARBOR_PG_DSN` in hosted CI, with log assertions that reject skips or
  unmatched selectors.

## Non-goals

- Changing the durable event wire format, sequence authority, or storage key
  contract.
- Relaxing tenant/user/session isolation or sequence gap checks.
- Reconfiguring, deleting, migrating, or otherwise mutating any downstream
  fleet database.
- Running local `make preflight`; the hosted workflow owns the live preflight
  gate.

## Release evidence (2026-08-22)

Implementation PR #728 merged as `bbc058e6dcfa30b0903f5546d394bcf2860ba836`.
Focused durable race/vet checks, phase-251/252 smoke checks, drift audit,
documentation build, mirror, and diff checks passed. Two independent Terra
High reviews report P0/P1 clear. Hosted candidate run `32582607218` completed
successfully, including Go on Ubuntu/macOS, exact real PostgreSQL StateStore
acceptance, Playwright, and corrected live preflight. The immutable annotated
`v1.29.2` tag object is `ebe0a907b92a745887fa469bb6e62cd018c53062` and peels to
`bbc058e6dcfa30b0903f5546d394bcf2860ba836`; release workflow `32584633258`
succeeded, publishing 13 assets with verified aggregate/sidecar checksums and
six attestations at
`https://github.com/hurtener/Harbor/releases/tag/v1.29.2`. The native
darwin/arm64 artifact reports v1.29.2, Protocol 0.1.0, build `bbc058e6`.
The scaffold fallback pin and golden fixtures were updated after the tag.
Local `make preflight` was never run; no downstream fleet cutover is claimed.

## Acceptance criteria

- [x] A v1.29.0-shaped head with `RunID=""` and multiple entries with distinct
      non-empty payload RunIDs opens successfully and adopts metadata.
- [x] Restart preserves each payload RunID in `EventMetadata` and full event
      reads; metadata and event list RunID filters select the correct event.
- [x] The adopted head is marked ready only with the canonical ordered
      metadata SHA-256 checksum; a stale projection is repaired from bodies.
- [x] A returned StateRecord with the wrong storage identity or entry kind is
      refused during backfill.
- [x] Tenant, user, session, sequence, malformed-body, and unknown-type
      adversarial fixtures fail boot loudly.
- [x] Existing metadata/body validation still refuses a divergent projection.
- [x] The real PostgreSQL regression is DSN-gated locally and selected with a
      non-vacuous PASS-line assertion in the hosted `state-postgres` job.
- [x] Focused durable race tests, focused vet, phase-252 smoke, relevant
      phase-251 smoke, hosted CI, two Terra High reviews, immutable annotated
      v1.29.2 release provenance, and post-tag cleanup are complete.

## Files added or changed

- `internal/events/drivers/durable/durable.go`
- `internal/events/drivers/durable/record.go`
- `internal/events/drivers/durable/metadata_test.go`
- `internal/events/drivers/durable/postgres_backfill_test.go`
- `.github/workflows/ci.yml`
- `scripts/smoke/phase-252.sh`
- `docs/decisions.md`
- `docs/notes/downstream-asks.md`
- `docs/plans/README.md`
- `CHANGELOG.md`

## Public API surface

None. The fix is internal to durable event recovery and its existing
`events.EventMetadataReplayer` / `events.HistoryReplayer` consumers.

## Test plan

- **Unit:** in-memory v1.29.0-shaped head adoption, checksum repair, metadata
  and event RunID filters, malformed/unknown body, triple/sequence mismatch,
  and wrong returned StateRecord identity/kind.
- **Integration:** real PostgreSQL StateStore with an isolated schema, the
  same multi-run legacy head, restart, checksum, and RunID-list assertions.
- **Conformance:** the hosted state-postgres job supplies `HARBOR_PG_DSN` and
  rejects a skip, no-test selector, or missing exact PASS line.
- **Concurrency / leak:** the package's existing `-race` durable recovery and
  metadata tests remain the focused gate; this phase adds no new goroutine or
  reusable artifact.
- **Fuzz / failure injection:** existing malformed, stale projection,
  cancellation, conditional-checkpoint, and body-authority tests remain in
  the package gate.

## Smoke script additions

`scripts/smoke/phase-252.sh` statically asserts the phase plan, D-432, the
HA-69 v1.29.2 extension, the RunID-independent storage-key comparison, the
real PostgreSQL selector, and the non-vacuous CI PASS-line guard. It never
contacts a database.

## Coverage target

- `internal/events/drivers/durable`: existing package target, with all new
  backfill branches covered under `go test -race`.
- `internal/events/drivers/durable` real PostgreSQL acceptance: exact selector
  must execute in hosted CI; local absence of `HARBOR_PG_DSN` is reported as a
  skip rather than claimed as PostgreSQL evidence.

## Dependencies

- Phase 251 / HA-69 / D-431 durable metadata index and StateStore atomicity.
- Phase 16 PostgreSQL StateStore driver.
- D-294 and D-305 event list/replay and metadata projection contracts.

## Risks / open questions

- Existing production heads may contain additional corruption beyond the
  observed RunID mismatch; the new checks intentionally fail closed and leave
  remediation to the operator rather than guessing.
- A hosted PostgreSQL service is required for the real-driver acceptance; no
  local DSN is assumed.

## Glossary additions

- Session-scoped durable storage key: the durable head/entry StateStore key
  whose RunID is intentionally empty while the event body keeps its run id.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] Hosted preflight/CI passes (local `make preflight` intentionally not run)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages meets the stated target
- [x] Multi-isolation backfill tests cover tenant/user/session and RunID
- [x] No reusable artifact or new goroutine path is introduced; existing
      durable `-race` reuse/leak coverage remains the gate
- [x] Real-driver integration selector is wired to hosted state-postgres
- [x] New vocabulary is documented in this plan
- [x] No brief finding was departed from
