# Phase 72h — Console DB local schema

## Summary

Phase 72h lands the Console-local schema the Console SvelteKit app reads
and writes to persist preferences, saved views, runtime registry rows,
encrypted auth profiles, and PATs — and **only** those Console-local
concerns. The schema sits behind the D-061 carve-out: it is never a
shadow source of truth for runtime entities (agents, sessions, tasks,
tools, events, artifacts), which flow exclusively through the Protocol.
Auth profiles + PATs are encrypted at rest via WebCrypto (AES-GCM,
PBKDF2-derived KEK) per the Brief 12 auth-storage threat model; every
row is keyed by the operator's identity from the active Protocol session.
The schema is the foundation every Stage-2 page that has saved-filter
chips, saved views, notification routing, or per-operator preferences
builds on top of; Phase 73f Tools is the first consumer (saved-filter
chips), and Phase 73m Settings is the heaviest consumer (every Console-
local card lives here).

## RFC anchor

- RFC §7
- RFC §5.5

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §CC-1 (multi-runtime context):** the Console connects to N
  runtimes simultaneously; each connected runtime is its own auth
  context, its own scope-claim set, its own Protocol-version handshake.
  Phase 72h's `runtime_registry` + `auth_profiles` tables persist the
  per-runtime endpoint + transport + per-runtime JWT so the Console can
  rehydrate the operator's runtime list on reload. Each row is keyed
  by the operator's identity from the active Protocol session — never
  globally — so two operators sharing a browser profile do not see
  each other's runtime lists.
- **brief 11 §CC-3 (notifications):** notification routing
  (email / webhook / web-push per notification class) is per-operator
  config, NOT runtime state — Phase 72h's `notifications_routing` table
  holds the matrix the Console reads at boot and writes when the
  operator toggles a transport in Settings. The runtime owns the
  `notification.*` topic (Phase 72d); the Console owns who hears it.
- **brief 11 §CC-4 (global search) + §CC-5 (keyboard navigation):** the
  saved-filter chips on every list page (Tools, Sessions, Tasks,
  Events, Memory, Artifacts) and the per-operator keybinding overrides
  live in `saved_filters` + `keybindings` respectively. Both are
  Console-local by construction: they describe the operator's view,
  not the runtime's state.
- **brief 12 §"`harbor console` subcommand" — auth-storage threat model:**
  per-runtime JWTs and PATs are stored in browser
  `localStorage` / `IndexedDB`, encrypted at rest via the **WebCrypto
  API**. The KEK is derived via PBKDF2 from a passphrase the operator
  enters at first runtime-attach; the DEK encrypts the secret blob via
  **AES-GCM** with a per-row random IV. Loss of the passphrase
  invalidates stored tokens but does NOT corrupt the rest of the
  Console DB. This is the exact pin Brief 12 §"What this brief does
  NOT do" left to this phase plan.
- **brief 12 §"Two surfaces, one stack":** the Console DB shape MUST be
  the same in both the `harbor console`-served full Console and the
  future packed `harbor dev --ui` subset (post-V1). The driver
  interface (§"Driver shape" below) keeps the schema portable: same
  table shapes, same row-key discipline, same encryption envelope —
  regardless of whether IndexedDB or a future Console-side server
  backs it.

## Findings I'm departing from (if any)

None.

## Goals

- Define the **eight V1 tables** the Console DB must expose to the
  SvelteKit app: `saved_filters`, `saved_views`, `profiles`,
  `runtime_registry`, `auth_profiles`, `pat_store`,
  `notifications_routing`, `keybindings`. Each table's column set is
  pinned in the plan; the introducing phase's migration is forward-only.
- Land a **driver-shaped seam** (`internal/console/db/ifaces`-style on
  the TypeScript side: `web/console/src/lib/db/`) so the default
  IndexedDB driver is one of N. The V1 default driver is IndexedDB
  (Dexie-style typed shape); the design is portable to a future
  Console-side server driver without re-shaping callers.
- **Encryption at rest** for `auth_profiles.encrypted_jwt_blob` and
  `pat_store.encrypted_token_blob`: AES-GCM with PBKDF2-derived KEK
  per Brief 12. The DEK is derived once per operator session from a
  passphrase the operator enters on first runtime-attach; the
  derivation parameters (iteration count, salt, IV size, KDF hash) are
  pinned in `web/console/src/lib/db/crypto.ts`. The encrypt/decrypt
  round-trip is testable in isolation (no DB needed).
- **Per-operator scoping** on every row: every table has an
  `operator_id` column (or composite key) keyed by the operator's
  identity from the active Protocol session — `(tenant_id, user_id)`,
  hashed and namespaced. The driver's read/write APIs filter by
  `operator_id` at the edge; cross-operator queries are forbidden by
  construction (no method exposes them).
- **Forward-only migrations** per CLAUDE.md §9: each migration carries
  a monotonic `version` integer and a description; the driver runs
  pending migrations on open; the migration table is the same shape
  the runtime persistence triad uses (`schema_migrations(version
  INTEGER PRIMARY KEY)`).
- **§13 Console-DB carve-out compliance:** the plan enumerates exactly
  what each table holds, and asserts none of them mirror runtime
  state. The smoke + an in-package unit test scan the schema
  vocabulary for forbidden table names (`agents`, `sessions`,
  `tasks`, `tools`, `events`, `artifacts`) — a future drift attempt
  fails loudly at build time.
- **First-consumer wired**: Phase 73f Tools' saved-filter chips are
  the §13 primitive-with-consumer obligation; Phase 73m Settings is
  the heaviest aggregate consumer. Both downstream phase plans
  reference this schema by table name + column shape.

## Non-goals

- The `harbor console` subcommand itself (D-091; bundled into Phase 73m
  Settings per `wave-13-decomposition.md` §9 item 8). 72h ships the
  schema + driver + crypto envelope; the subcommand that boots the
  Console is downstream.
- The notification **delivery transports** (email / webhook / web-push
  endpoints). 72h persists the routing matrix; the runtime-side
  `notification.*` event topic (Phase 72d) and the delivery sidecar
  (post-V1) are separate phases.
- Cross-operator views or sharing of Console-local state. Each
  operator's Console DB is private; "share my saved filter" is
  post-V1 (and if it ever lands, it ships as a Protocol surface —
  not as a Console-DB cross-row read).
- A Console-side server-backed driver. V1 ships the IndexedDB driver
  only; the seam exists for a future server-backed driver but no
  second driver lands in 72h.
- Any UI rendering. 72h is schema + driver + crypto + tests + the
  baseline TypeScript module the SvelteKit pages will import. Pages
  ship in their respective Stage-2 phases (73f first).

## Acceptance criteria

- [ ] `web/console/src/lib/db/` lands as a self-contained TypeScript
      module: `index.ts` (public API), `schema.ts` (table shapes +
      column types), `migrations.ts` (forward-only migration list),
      `crypto.ts` (WebCrypto envelope), `driver.ts` (driver
      interface), `drivers/indexeddb.ts` (default V1 driver),
      `tests/*.spec.ts` (Vitest + jsdom or Playwright-component tests).
- [ ] The eight V1 tables exist with the columns pinned in §"Table
      shapes" below. Each table includes `operator_id` (TEXT, NOT
      NULL) keyed by `hash(tenant_id || ':' || user_id)` from the
      active Protocol identity; rows missing `operator_id` are
      rejected at write time (fail-loud, no silent default).
- [ ] `auth_profiles.encrypted_jwt_blob` and
      `pat_store.encrypted_token_blob` are opaque `Uint8Array`
      (AES-GCM ciphertext + 12-byte IV + 16-byte auth tag).
      `crypto.ts` exposes `encrypt(plaintext, masterKey) → blob` and
      `decrypt(blob, masterKey) → plaintext`; the master key is
      derived via `crypto.subtle.deriveKey(PBKDF2, ...)` from the
      operator's session passphrase (min 100k iterations, 16-byte
      random salt persisted on `profiles.kdf_salt`). Decryption with
      the wrong key fails loudly with `ErrAuthDecryption` — no silent
      `null` returns.
- [ ] **Forward-only migrations**: `migrations.ts` exposes
      `runMigrations(driver) → number` that applies every pending
      migration in version order, records each into the
      `schema_migrations` table, and is idempotent on a clean DB. A
      migration that tries to mutate (rather than extend) a previous
      migration is rejected by an in-package lint
      (`migrations.spec.ts` parses the migration list, asserts no
      `DROP` / `ALTER COLUMN` shapes after the first commit of any
      table).
- [ ] **Per-operator scoping is mandatory**: every driver method takes
      `operator_id` as the first argument; cross-operator reads /
      writes are not exposed on the interface. A unit test verifies
      operator A's rows are invisible to operator B (the row-scope
      filter is identity-keyed, not application-layer).
- [ ] **§13 Console-DB carve-out**: a unit test scans `schema.ts` and
      fails if any table name from the forbidden set appears
      (`agents`, `sessions`, `tasks`, `tools`, `events`, `artifacts`,
      `messages`, `traces`, `metrics`, `runs`). The plan's "Table
      shapes" section names exactly what each table holds and
      explicitly contrasts it against runtime entities it does NOT
      mirror.
- [ ] **Driver shape** is interface-first: `driver.ts` declares a
      `ConsoleDB` interface (open / close + per-table CRUD typed by
      schema); `drivers/indexeddb.ts` is the default V1
      implementation; the factory is in `index.ts` and dispatches by
      driver name (V1: only `"indexeddb"` is registered; the seam is
      ready for `"server"` post-V1).
- [ ] **An integration-shape test** (`tests/integration.spec.ts`)
      lands in 72h: it opens the IndexedDB driver against the
      jsdom + fake-indexeddb fixture, runs migrations from empty,
      writes one row into each of the eight tables (saved-filter,
      saved-view, profile, runtime row, encrypted auth profile,
      encrypted PAT, notifications routing, keybinding), reads them
      back, and asserts identity scoping by writing operator B's row
      and verifying operator A's read returns ONLY operator A's data.
- [ ] **An encryption round-trip test** (`crypto.spec.ts`) lands in
      72h: encrypts a sample JWT + a sample PAT; asserts the
      ciphertext is opaque (`assertNoSubstring("eyJ", ciphertextHex)`
      — no plaintext JWT header leak); decrypts with the correct key
      (round-trip succeeds); decrypts with a wrong key (raises
      `ErrAuthDecryption` loudly — no `null` return).
- [ ] **A migration test** (`migrations.spec.ts`) lands in 72h: opens
      a clean DB (asserts all eight tables created); re-opens (asserts
      `runMigrations` is a no-op); manually downgrades a pre-existing
      DB to version N-1 and re-opens (asserts the missing migration
      runs); asserts no `DROP TABLE` / `ALTER COLUMN` shapes in the
      migration list.
- [ ] `scripts/smoke/phase-72h.sh` (PREFLIGHT_REQUIRES: static-only)
      asserts the migration file exists and the schema module shape
      is intact (`schema.ts` references all 8 tables; the forbidden-
      table scan passes). FAIL = 0.
- [ ] `docs/glossary.md` adds entries for the 8 tables plus
      "encrypted-at-rest auth profile" (Console DB already exists in
      the glossary).

## Files added or changed

```text
web/console/src/lib/db/
  index.ts                       # public ConsoleDB factory + re-exports
  schema.ts                      # table shapes + column types (Zod-style typed records)
  migrations.ts                  # forward-only migration list + runMigrations
  crypto.ts                      # WebCrypto envelope (AES-GCM + PBKDF2)
  driver.ts                      # ConsoleDB interface
  errors.ts                      # ErrAuthDecryption, ErrMissingOperator, ErrMigrationConflict
  drivers/
    indexeddb.ts                 # default V1 driver (Dexie or native IDB API — pinned in plan)
  tests/
    integration.spec.ts          # eight-table round-trip + cross-operator isolation
    crypto.spec.ts               # encrypt/decrypt round-trip + wrong-key failure
    migrations.spec.ts           # clean-start, re-open no-op, downgrade-then-upgrade, forbidden-shape lint
    schema-carveout.spec.ts      # §13 D-061 scan (forbidden table names)
scripts/smoke/phase-72h.sh
docs/plans/phase-72h-console-db-schema.md
docs/glossary.md                 # 8 table entries + encrypted-at-rest auth profile
docs/plans/README.md             # Phase 72h row Pending -> Shipped (when shipped)
```

(All `web/console/` paths are net-new with this phase; the SvelteKit
project root lands with the first Console SvelteKit phase per Brief 12
§"Re-discussion checklist". 72h is purely module-level — no routes,
no UI, no Skeleton, no Svelte components — only TypeScript + Vitest.)

## Public API surface

```ts
// web/console/src/lib/db/driver.ts
export interface ConsoleDB {
  open(): Promise<void>;
  close(): Promise<void>;

  savedFilters: TableScope<SavedFilter>;
  savedViews:   TableScope<SavedView>;
  profiles:     TableScope<Profile>;
  runtimes:     TableScope<RuntimeRegistryRow>;
  authProfiles: TableScope<AuthProfile>;     // encrypted_jwt_blob is Uint8Array
  patStore:     TableScope<PATEntry>;        // encrypted_token_blob is Uint8Array
  notifications:TableScope<NotificationRoutingRow>;
  keybindings:  TableScope<KeybindingRow>;
}

export interface TableScope<T> {
  list(operatorID: string): Promise<T[]>;
  get(operatorID: string, id: string): Promise<T | null>;
  upsert(operatorID: string, row: T): Promise<void>;
  delete(operatorID: string, id: string): Promise<void>;
}

// web/console/src/lib/db/index.ts
export function openConsoleDB(opts: ConsoleDBOptions): Promise<ConsoleDB>;
export type ConsoleDBOptions = {
  driver?: "indexeddb";             // V1: only one registered
  operatorIdentity: OperatorIdentity;  // {tenantID, userID}; hashed to operator_id
  masterKey: CryptoKey;             // derived via crypto.ts from operator passphrase
};

// web/console/src/lib/db/crypto.ts
export function deriveMasterKey(
  passphrase: string,
  salt: Uint8Array,
  iterations?: number,
): Promise<CryptoKey>;
export function encrypt(plaintext: Uint8Array, masterKey: CryptoKey): Promise<Uint8Array>;
export function decrypt(ciphertext: Uint8Array, masterKey: CryptoKey): Promise<Uint8Array>;
export class ErrAuthDecryption extends Error {}
```

### Table shapes (binding)

Every table includes (in addition to per-table columns):

- `operator_id` TEXT NOT NULL — `sha256(tenant_id || ':' || user_id)`, base64url-encoded.
- `id` TEXT NOT NULL — table-local primary key (ULID).
- `created_at` INTEGER NOT NULL — Unix epoch millis.
- `updated_at` INTEGER NOT NULL — Unix epoch millis; updated on upsert.

Per-table columns (V1 binding):

1. **`saved_filters`** — per-operator saved filter chips on list
   pages (Tools, Sessions, Tasks, Events, Memory, Artifacts).
   - `page` TEXT NOT NULL (one of: `tools`, `sessions`, `tasks`,
     `events`, `memory`, `artifacts`, `agents`, `mcp_connections`,
     `background_jobs`, `flows`).
   - `name` TEXT NOT NULL.
   - `filter_spec_json` TEXT NOT NULL — JSON-encoded
     `events.Filter`-shaped spec (or page-specific filter).
   - **What this is NOT:** not a runtime entity, not a server-side
     saved filter. The filter spec is *applied* to a runtime list
     call; the spec persists on the Console side only.

2. **`saved_views`** — per-operator dashboard layouts, column-set
   preferences, sort orders on list pages.
   - `page` TEXT NOT NULL.
   - `name` TEXT NOT NULL.
   - `view_spec_json` TEXT NOT NULL — JSON-encoded view spec
     (columns, sort, group-by, density).
   - **What this is NOT:** not a runtime saved query, not a shared
     team view.

3. **`profiles`** — the per-operator preference record (one row per
   operator per browser profile).
   - `theme` TEXT NOT NULL (one of: `light`, `dark`, `system`).
   - `density` TEXT NOT NULL (one of: `comfortable`, `compact`).
   - `motion` TEXT NOT NULL (one of: `full`, `reduced`).
   - `tz` TEXT NULL — IANA timezone name; NULL = browser default.
   - `locale` TEXT NULL — BCP-47 locale; NULL = browser default.
   - `kdf_salt` BLOB NOT NULL — 16-byte salt for PBKDF2 KEK
     derivation (per Brief 12 auth-storage threat model).
   - **What this is NOT:** not a runtime user record, not a runtime
     authorization profile.

4. **`runtime_registry`** — the per-operator list of Harbor
   runtimes the Console knows about (Brief 11 §CC-1 multi-runtime
   context).
   - `name` TEXT NOT NULL — operator-picked friendly name.
   - `base_url` TEXT NOT NULL — Protocol endpoint.
   - `transport` TEXT NOT NULL (one of: `sse_rest`, `ws`,
     `stdio`) — V1: `sse_rest` per Phase 60.
   - `is_default` INTEGER NOT NULL (0/1).
   - `last_connected_at` INTEGER NULL — Unix epoch millis.
   - `protocol_version` TEXT NULL — observed at last handshake.
   - **What this is NOT:** the runtime does NOT know it is in this
     list. This is a Console-local address book, not an
     authorized-controller allowlist (that is D-066, a runtime-side
     concern).

5. **`auth_profiles`** — per-(operator, runtime) **encrypted** JWT
   blob and JWT metadata.
   - `runtime_id` TEXT NOT NULL — FK-shape onto
     `runtime_registry.id` (foreign-key constraints not enforced at
     the schema level since IndexedDB is schemaless; the driver
     enforces at the API layer).
   - `issuer` TEXT NULL — JWT `iss` claim (decoded ONCE at
     attach-time and cached; never re-derived from the encrypted
     blob).
   - `algorithm` TEXT NOT NULL — one of the asymmetric allowlist
     (`RS256` | `RS384` | `RS512` | `ES256` | `ES384` | `ES512`,
     per CLAUDE.md §7).
   - `expires_at` INTEGER NULL — Unix epoch millis (cached at
     attach-time; refreshed on rotation).
   - `encrypted_jwt_blob` BLOB NOT NULL — AES-GCM(plaintext_jwt) +
     12-byte IV prefix + 16-byte auth tag suffix.
   - `iv` BLOB NOT NULL — 12-byte AES-GCM IV (also embedded in
     `encrypted_jwt_blob` for envelope completeness; stored
     separately to make rotation tests easy).
   - **What this is NOT:** not a runtime auth record. The runtime
     issues the JWT; the Console DB stores its operator's copy at
     rest, encrypted. Loss of the operator's passphrase invalidates
     stored tokens but does not impact the runtime's auth subsystem.

6. **`pat_store`** — per-operator Console-local Personal Access
   Tokens (one-time-revealed at create; **encrypted** at rest).
   - `name` TEXT NOT NULL — operator-picked label.
   - `runtime_id` TEXT NULL — NULL = "all runtimes"; non-NULL ties
     the PAT to a specific runtime row.
   - `scope_summary` TEXT NULL — human-readable scope summary
     captured at create (the runtime is the source of truth; this
     is a cached display label).
   - `encrypted_token_blob` BLOB NOT NULL — AES-GCM ciphertext.
   - `iv` BLOB NOT NULL — 12-byte IV.
   - `created_at`, `last_used_at` INTEGER columns (latter is
     opportunistically updated by the Console after a successful
     Protocol call; not authoritative).
   - **What this is NOT:** not a runtime-side token table. The
     runtime owns the canonical PAT registry (post-V1 Protocol
     surface); the Console DB caches the encrypted token blob so
     the operator does not have to paste it on every page load.

7. **`notifications_routing`** — the per-operator routing matrix
   for `notification.*` event classes (Brief 11 §CC-3).
   - `notification_class` TEXT NOT NULL (one of:
     `governance_budget_exceeded`, `tool_auth_required`,
     `tool_approval_required`, `task_failed`,
     `agent_credentials_expired`, `runtime_health_degraded` — the
     starter list from Brief 11 §CC-3; new classes ship as
     additive forward migrations).
   - `transport` TEXT NOT NULL (one of: `in_app`, `email`,
     `webhook`, `web_push` — V1 wires `in_app` only; the others
     are post-V1 deliverer phases).
   - `enabled` INTEGER NOT NULL (0/1).
   - `target_json` TEXT NULL — transport-specific target spec
     (e.g. email address, webhook URL). NULL when `transport=in_app`.
   - **What this is NOT:** not the source of `notification.*`
     events. The runtime emits them via Phase 72d's topic; the
     Console reads this table to decide which transports light up
     on receipt.

8. **`keybindings`** — per-operator keybinding overrides over the
   Console default set (Brief 11 §CC-5).
   - `action` TEXT NOT NULL — keybinding action identifier (one
     of the documented action constants, e.g.
     `open_command_palette`, `focus_search`, `nav_sessions`,
     `pause_live_updates`).
   - `key_chord` TEXT NOT NULL — canonical key-chord string
     (e.g. `"cmd+k"`, `"g s"`).
   - **What this is NOT:** not a runtime config. Keybindings are
     Console-local UI shortcuts.

### Forbidden table names (mechanically enforced)

The §13 D-061 carve-out is enforced by a unit test
(`schema-carveout.spec.ts`) that scans `schema.ts` and FAILS the
build if any of these table names appear:

- `agents`, `sessions`, `tasks`, `tools`, `events`, `artifacts`,
  `messages`, `traces`, `metrics`, `runs`, `runtime_entities`,
  `agent_list`, `session_list`, `task_list`, `tool_catalog`,
  `event_log`.

These are all runtime entities. The runtime owns them; the Console
renders them via the Protocol; the Console DB never persists them.

## Test plan

- **Unit:**
  - `crypto.spec.ts` — `deriveMasterKey` with known
    salt + passphrase produces a stable key; `encrypt` →
    `decrypt` round-trip; `decrypt` with the wrong key raises
    `ErrAuthDecryption`; ciphertext is opaque (no plaintext-JWT
    substring leak).
  - `schema.spec.ts` — each of the 8 tables' Zod schema validates
    a happy-path record and rejects (a) a missing `operator_id`,
    (b) a missing `id`, (c) an unknown enum value where one is
    pinned.
  - `schema-carveout.spec.ts` — §13 forbidden-table-name lint.
  - `migrations.spec.ts` — clean-start applies every migration;
    re-open is a no-op; downgrade then re-open re-applies missing
    migrations; the migration list contains no `DROP TABLE` /
    `ALTER COLUMN` shapes (mutating migrations are rejected per
    CLAUDE.md §9).

- **Integration:**
  - `tests/integration.spec.ts` — opens `drivers/indexeddb.ts`
    against `fake-indexeddb` (jsdom-compatible IndexedDB fake),
    runs migrations from empty, writes one row into each of the 8
    tables, reads each back, asserts: (a) every row carries the
    operator's identity-keyed `operator_id`; (b) operator B
    writes do NOT surface to operator A reads (cross-operator
    isolation); (c) auth-profile + PAT blobs round-trip through
    encrypt/decrypt; (d) deleting a row removes it.
  - This is the §17 in-package integration test for 72h: real
    driver (the V1 IndexedDB driver), real crypto (the actual
    `crypto.subtle` shim provided by jsdom or the
    `@peculiar/webcrypto` polyfill), identity propagation, ≥1
    failure mode (wrong-key decryption raises loudly).

- **Conformance:** N/A — V1 ships one driver. The interface is
  defined so a post-V1 server-backed driver can be added under
  the same `ConsoleDB` shape with a shared conformance suite
  (post-V1 RFC PR + new phase).

- **Concurrency / leak:** N/A — Console DB is browser-local; the
  driver is single-thread (IndexedDB transactions are serialized
  by the browser runtime). A "concurrent ops on one DB instance"
  micro-test still lands in `integration.spec.ts` to assert that
  N parallel `upsert` calls from the same operator preserve the
  last-write-wins shape and do not corrupt the migration table.
  Mark the §11 concurrent-reuse checkbox N/A with this reason in
  the pre-merge checklist.

## Smoke script additions

- Assert `web/console/src/lib/db/schema.ts` exists once the phase
  ships (file-existence check; SKIPs until the file lands).
- Assert `web/console/src/lib/db/migrations.ts` exists once the
  phase ships.
- Assert `web/console/src/lib/db/crypto.ts` exists once the phase
  ships.
- Forbidden-name scan: `assert_grep_absent` for each forbidden
  table name on `schema.ts` once it exists (D-061 carve-out
  defence in depth).
- Until the phase lands, the smoke script `skip`s with a clear
  reason (per the §4.2 convention).
- Header: `# PREFLIGHT_REQUIRES: static-only` (frontend-only
  schema phase; no live-server check needed).

## Coverage target

- `web/console/src/lib/db/`: 85% line coverage across `schema.ts`,
  `crypto.ts`, `migrations.ts`, `driver.ts`, `drivers/indexeddb.ts`.
  Vitest's coverage reporter feeds this; the `frontend` CI job
  asserts the threshold.

## Dependencies

- **Phase 60** — Protocol wire transport (Shipped). 72h's
  `auth_profiles` rows store an encrypted JWT the operator used
  with a Phase 60 Protocol session; the operator identity scoping
  every Console-DB row comes from the active Protocol session.
  Without Phase 60's wire transport, there is no operator
  identity to key rows on.

(72h has no other Stage-1 dependencies; per `wave-13-decomposition.md`
§4, 72h is parallel-able alongside 72a–72g + 74 + 75.)

## Risks / open questions

- **IndexedDB vs Console-side server backing.** V1 picks IndexedDB
  because Brief 12 §"`harbor console` subcommand" and D-091 already
  pin browser-local encrypted storage as the multi-runtime auth
  posture. A future Console-side server driver (post-V1) would
  let operators sync Console-local preferences across browsers /
  machines; the seam exists, but no second driver lands in 72h.
  Risk: if operator pressure post-V1 forces a server-backed
  driver fast, the existing `ConsoleDB` interface must absorb it
  without reshaping callers — design check: every method takes
  `operatorID` and returns `Promise<...>`, so an HTTP-backed
  driver fits the same shape.
- **PBKDF2 iteration count + KDF hash.** 72h pins
  `iterations >= 100_000` and `hash = "SHA-256"` per current
  OWASP guidance. A future audit may bump iterations; the
  `crypto.ts` API takes `iterations` as an optional argument so
  rotation is a config bump, not a schema bump. The `profiles.kdf_salt`
  column is per-operator so a single iteration-count change does
  NOT invalidate all stored blobs at once.
- **Passphrase loss.** Per Brief 12, loss of passphrase
  invalidates stored auth blobs. The Console UI MUST surface this
  loudly on first-runtime-attach: "if you forget this passphrase
  you will need to re-enter your JWT for each connected runtime."
  72h's `errors.ts` exposes `ErrAuthDecryption` as the failure
  shape the Settings page consumes to drive the "re-enter
  passphrase" / "re-attach runtime" flows. Risk: if a future UI
  silently treats `ErrAuthDecryption` as "auth missing" and
  redirects to login, the operator's encrypted blobs become
  inaccessible state — the Settings page MUST distinguish
  decryption-failed from token-expired.
- **`runtime_registry.is_default`.** A boolean default-flag column
  is the simplest single-default model. If operators want
  "different default per workspace" (e.g. context-aware
  defaulting), that ships post-V1 as a new column on
  `profiles` (forward-only migration).
- **§13 carve-out drift via a future phase.** A future page-phase
  author may be tempted to "cache the agents list locally for
  offline browsing." The `schema-carveout.spec.ts` scan catches
  that at build time; the §13 forbidden-table-name list is the
  enforcement seam. If a legitimate Console-local cache need
  arises (e.g. caching the user's own avatar URL), it lands as a
  new column on `profiles` — never as a new table named after a
  runtime entity.

## Glossary additions

This phase introduces 9 new vocabulary entries. They land in
`docs/glossary.md` in the same PR (alphabetical placement under
the indicated section):

- `auth_profiles` (under A) — Console DB encrypted-JWT table.
- `encrypted-at-rest auth profile` (under E) — the encryption
  envelope.
- `keybindings` (Console DB table) (under K) — Console DB
  keybinding-overrides table (the existing
  `keybindings-help` skill entry is unrelated).
- `notifications_routing` (under N) — Console DB per-operator
  notification routing matrix.
- `pat_store` (under P) — Console DB encrypted-PAT table.
- `profiles` (Console DB table) (under P) — Console DB
  per-operator preferences table.
- `runtime_registry` (Console DB table) (under R) — Console DB
  per-operator runtime address book.
- `saved_filters` (under S) — Console DB per-operator saved
  filter chip table.
- `saved_views` (under S) — Console DB per-operator dashboard
  layout table.

Each entry explicitly disclaims it is NOT a runtime entity and
points to RFC §7 + D-061 + this phase plan.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (85% on
      `web/console/src/lib/db/`)
- [ ] If multi-isolation code paths changed: cross-session
      isolation test passes — N/A: Console DB is per-operator
      browser-local; cross-operator isolation IS the
      multi-isolation analogue here and is covered by
      `integration.spec.ts`.
- [ ] **If this phase builds a reusable artifact** (engine, tool,
      planner, driver, redactor, client, catalog, etc.):
      concurrent-reuse test passes — N/A: the IndexedDB driver
      is a browser-runtime-serialized API (the browser
      transaction layer serializes concurrent ops); the
      cross-operator isolation test in `integration.spec.ts`
      covers the analogous "no context bleed" guarantee.
- [ ] **If this phase consumes a shipped subsystem's surface OR
      closes a cross-subsystem seam**: an integration test
      exists, wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, and runs in the
      `frontend` CI job — `tests/integration.spec.ts` covers
      this against the real IndexedDB driver via
      `fake-indexeddb` + real WebCrypto via
      `@peculiar/webcrypto` (or jsdom's built-in shim).
- [ ] If new vocabulary: glossary updated — 9 entries land in
      this PR.
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — N/A, no departures.
