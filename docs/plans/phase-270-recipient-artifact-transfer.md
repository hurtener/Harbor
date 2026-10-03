# Phase 270 — Recipient-admitted artifact transfer

## Summary

Transfer exact immutable artifacts directly between trusted runtime endpoints. Each copy requires source-owner authentication, coordinator-signed exact-content authority, and a durable recipient-owner admission; the coordinator handles references and receipts only.

## RFC anchor

- RFC §6.10
- RFC §6.11
- RFC §5.5

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §1: identity remains tenant/user/session scoped, and durable mechanisms reuse the StateStore driver triad
- brief 05 §1: mandatory artifact storage preserves opaque heavy bytes rather than truncating or embedding them in context
- brief 06: content-free canonical events make data movement observable

## Findings I'm departing from (if any)

None. Cross-session copies are explicit separately authorized operations, not widening the source read key.

## Goals

- Exact-content, recipient-bound direct transfer and reverse-direction symmetry
- Durable idempotency and honest unresolved/revoked/expired outcomes
- No arbitrary URL, redirect, coordinator byte proxy, implicit knowledge ingestion, or credential passthrough

## Non-goals

- Proving network sender identity separately from signed import authority
- Revoking already copied bytes or treating a receipt as current read permission
- General remote fetch, long-lived grants, or model-visible transfer credentials

## Acceptance criteria

- [ ] Both endpoint owners independently admit exact signed authority
- [ ] Digest, MIME, declared size, full source/destination scope, audience, epoch and expiry are checked
- [ ] Conflicting operation replays fail; exact retries converge on one destination artifact
- [ ] A crash after byte storage recovers the content-free receipt without recreating bytes after expiry
- [ ] Revocation before dispatch prevents movement; in-progress outcomes never claim rollback
- [ ] Unknown peers, URL substitutions, redirects, foreign identities and changed bytes are rejected
- [ ] Real HTTP and real storage drivers verify bytes at recipient, with 100 concurrent owners
- [ ] Native Protocol/client, operator config, capability advertisement and generated docs agree

## Files added or changed

- internal/artifacts/transfer/: signed authority, durable service, direct HTTP and tests
- internal/protocol/: typed owner-facing methods, client and canonical joins
- internal/config/ and internal/runtime/serve/: opt-in trusted-peer boot assembly
- sdk/artifacts/transfer/ and sdk/protocolclient/: public signatures and client vocabulary

## Public API surface

- artifacts.export_answer: exact sealed-answer selector → immutable artifact reference and incorporated input revision
- artifacts.prepare_import(ArtifactsTransferRequest) → ArtifactTransferReceipt
- artifacts.transfer(ArtifactsTransferRequest) → ArtifactTransferReceipt
- artifacts.transfer_status(ArtifactsTransferStatusRequest) → ArtifactTransferReceipt
- artifacts.revoke_transfer(ArtifactsTransferStatusRequest) → ArtifactTransferReceipt
- Signed exact import-only HTTP endpoint, requiring a matching durable recipient admission

## Test plan

- **Unit:** signature, shape, validity interval, epoch, MIME/size/hash and conflicting replay
- **Integration:** real StateStore and fence-capable ArtifactStore with direct HTTP source-to-recipient delivery; actual bytes read at destination
- **Conformance:** durable record replay over in-memory, SQLite and PostgreSQL stores
- **Concurrency / leak:** 100 owners and simultaneous exact retries; context cancellation cannot affect sibling owners; HTTP keepalives disabled to avoid background ownership

## Smoke script additions

- scripts/smoke/phase-270.sh exercises the native method and explicit disabled-state refusal against a served runtime; the transfer integration suite supplies configured-peer positive coverage

## Coverage target

- internal/artifacts/transfer: 80%
- Existing Protocol/boot packages: focused new paths plus aggregate regression gates

## Dependencies

- Phase 269 retained context does not provide cross-runtime read authority
- Existing artifact store and StateStore conditional-write contracts

## Risks / open questions

- An importing/exporting state means delivery may have started; cancellation cannot truthfully promise rollback
- Atomic scope fencing is currently supported by inmem, SQLite-blob and Postgres-blob artifacts; fs and s3 transfer enablement fails closed
- Session deletion fences the artifact store before sweeping; every late ordinary/import Put fails atomically
- A completed receipt proves past delivery, not continuing access after deletion or policy changes
- Signed intent identifies authorized source lineage; it does not independently authenticate the network sender
- Maximum transfer is 64 MiB, validity at most fifteen minutes; all endpoints are boot-pinned and require HTTPS outside explicit loopback fixtures

## Glossary additions

- Recipient-admitted artifact transfer
- Transfer receipt
- Transfer policy epoch

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. If this phase does NOT build a reusable artifact, mark this checkbox N/A with a one-line reason.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. If `Dependencies` above is `00` only, mark this checkbox N/A with a one-line reason.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
