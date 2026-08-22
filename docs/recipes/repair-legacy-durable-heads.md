# Repair legacy durable heads safely

Harbor v1.29.3 adds an offline repair command for a narrow, fail-closed
condition: a legacy durable head contains redundant references to an immutable
entry. The command never repairs ambiguous data and never changes or deletes
immutable entry bodies.

Repair is an operator procedure, not a runtime startup hook. Do not admit a
v1.29.2-or-newer event writer until the affected heads have been inspected,
repaired, and verified.

## Safety preconditions

1. Stop every process that can publish events to the StateStore scope and
   record the explicit freeze/drain acknowledgement. A normal
   rolling deployment is not sufficient; use stop-before-start,
   suspend-then-resume, or an equivalent platform guarantee.
2. Take the operator-approved backup/snapshot and record the rollback path.
3. Obtain the direct, session-affine PostgreSQL DSN on port `5432` for apply.
   The command refuses URL and keyword forms identifying PgBouncer, transaction
   pooling, or port `6432` before opening a write handle.
4. Preserve the command's receipt/manifest with the deployment evidence. It
   contains hashes, counts, positions, generations, and outcomes—not payload
   bytes or identity values.

Migration-only processes may overlap the repair within the separately budgeted
connection envelope; event writers may not.

## 1. Inspect (default dry-run)

Point the command at the offline StateStore driver and DSN. The command opens
only the selected store and does not assemble or boot a Harbor Runtime.

```bash
harbor events repair-legacy-heads \
  --driver postgres \
  --dsn "$HARBOR_STATE_DSN" \
  --mode inspect \
  --json > /var/lib/harbor/legacy-heads.inspect.json
```

`inspect` is also the default when `--mode` is omitted. It performs no writes.
The report is content-free and includes aggregate affected-head and
duplicate-reference counts plus stable head/entry identifiers or hashes,
positions, generations, and validation outcomes. It does not print raw event
bodies, identity values, or payload bytes. Scanning is bounded and
cancellable through the StateStore bounded-enumeration contract. Set
`--max-heads` (and, when needed, `--max-duplicates`) to an operator-approved
bound; the scan reads at most one item beyond the bound and fails closed before
retaining an oversized inventory. A large source is never loaded into memory
solely to hash it.

The durable receipt schema is versioned and content-free: it binds
`receipt_version`, `receipt_id`, `tool_version`,
`head_identity_hash_sha256`, `before_head_hash_sha256`,
`after_head_hash_sha256`, `expected_generation`, `applied_generation`,
`duplicates[]` (`sequence`, zero-based `positions`, `entry_hash_sha256`,
`payload_metadata_hash_sha256`, `payload_type`, `payload_sequence`), and
`outcome`. Identity hashes are lowercase SHA-256; raw tenant/user/session/
RunID values and payload/body bytes never appear.

If the result is empty, retain the receipt and run `verify`; an empty scan is
not permission to admit a writer unless the normal writer-admission contract
has also been satisfied.

## 2. Apply only exact, unambiguous duplicates

After reviewing the inspect receipt, run apply against the direct `5432` DSN:

```bash
harbor events repair-legacy-heads \
  --driver postgres \
  --dsn "$HARBOR_STATE_DIRECT_DSN" \
  --mode apply \
  --freeze-ack \
  --json > /var/lib/harbor/legacy-heads.apply.json
```

The explicit acknowledgement is required even when the inspect report found
no duplicates. A missing acknowledgement or an unsafe DSN fails before any
mutation. If the source generation changes, the command rereads the head and
revalidates it; it does not overwrite a concurrent writer.

A duplicate is repairable only when every occurrence in one head points to the
same single immutable entry slot and all of these agree exactly:

- padded entry kind and decoded sequence;
- storage tenant/user/session identity;
- event tenant/user/session identity and event type;
- existing metadata projection, when present; and
- body/metadata checksum and canonical ordering.

The v1.29.2 compatibility rule remains important: a session-scoped durable
storage key may intentionally have `RunID=""`, while the persisted event body
has a non-empty authoritative RunID. The body RunID is preserved and is not
compared to that empty storage key.

Missing entries, sequence or identity mismatch, wrong kind, malformed JSON,
unknown type, conflicting metadata, non-canonical ordering, checksum failure,
or any other ambiguity fails closed with no partial write.

On success, apply retains the first occurrence of each validated duplicate and
removes only redundant sequence references and their duplicate metadata
projection. Immutable entry records are untouched. The canonical head and a
durable, content-free receipt are committed through StateStore CAS.

## 3. Verify and replay safely

Run verify against the same stopped scope after apply:

```bash
harbor events repair-legacy-heads \
  --driver postgres \
  --dsn "$HARBOR_STATE_VERIFY_DSN" \
  --mode verify \
  --json > /var/lib/harbor/legacy-heads.verify.json
```

Verification proves that affected heads are canonical, hashes and generations
match the receipt, immutable entry bodies were not changed, and no ambiguous
duplicate remains. A second apply is an idempotent no-op with the same
content-free receipt. The same guarantee covers a response lost after the
database commit.

Only after verify succeeds should the operator resume the event writer and
allow v1.29.2+ recovery to boot. Ordinary runtime recovery remains fail-closed
for corruption the tool cannot prove safe to repair.

## Rollback

Rollback is configuration- and backup-based. Keep the source and receipts
intact, stop writers again, restore the approved snapshot if the operator's
rollback decision requires it, and investigate the content-free diagnostic.
Do not delete immutable entries or attempt a hand-written SQL rewrite.
