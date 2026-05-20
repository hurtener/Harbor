/**
 * Console DB schema (Phase 72h) — the eight V1 tables the Console
 * SvelteKit app reads and writes.
 *
 * §13 / D-061 carve-out: every table here holds **Console-local state
 * only** — saved views, filters, per-operator preferences, the operator's
 * runtime address book, encrypted auth blobs. NONE of them mirror a
 * runtime entity (agents, sessions, tasks, tools, events, artifacts). The
 * runtime owns those; the Console renders them via the Protocol; the
 * Console DB never persists them. `tests/schema-carveout.spec.ts`
 * mechanically scans this file for forbidden runtime-entity table names.
 *
 * Each row is per-operator scoped: `operator_id` is `sha256(tenant_id ||
 * ':' || user_id)` base64url-encoded, keyed off the active Protocol
 * identity. The driver filters by `operator_id` at the edge — cross-
 * operator reads/writes are not exposed on the interface.
 */

/* ---------------------------------------------------------------------- */
/* Table-name registry — the single source of truth for what tables exist */
/* ---------------------------------------------------------------------- */

/**
 * The eight V1 Console DB table names. Adding a table is an additive
 * forward migration plus an entry here.
 */
export const TABLE_NAMES = [
  'saved_filters',
  'saved_views',
  'profiles',
  'runtime_registry',
  'auth_profiles',
  'pat_store',
  'notifications_routing',
  'keybindings'
] as const;

export type TableName = (typeof TABLE_NAMES)[number];

/**
 * Runtime-entity table names that MUST NEVER appear in the Console DB
 * schema (§13 / D-061). `tests/schema-carveout.spec.ts` fails the build if
 * any of these is found among {@link TABLE_NAMES}.
 */
export const FORBIDDEN_TABLE_NAMES = [
  'agents',
  'sessions',
  'tasks',
  'tools',
  'events',
  'artifacts',
  'messages',
  'traces',
  'metrics',
  'runs',
  'runtime_entities',
  'agent_list',
  'session_list',
  'task_list',
  'tool_catalog',
  'event_log'
] as const;

/* ---------------------------------------------------------------------- */
/* Shared columns                                                         */
/* ---------------------------------------------------------------------- */

/**
 * Columns every Console DB row carries in addition to its per-table
 * columns. `operator_id` is the per-operator row-scope key.
 */
export interface BaseRow {
  /** `sha256(tenant_id || ':' || user_id)`, base64url-encoded. */
  operator_id: string;
  /** Table-local primary key (ULID). */
  id: string;
  /** Unix epoch millis. */
  created_at: number;
  /** Unix epoch millis; updated on upsert. */
  updated_at: number;
}

/* ---------------------------------------------------------------------- */
/* Per-table row shapes                                                   */
/* ---------------------------------------------------------------------- */

/** List pages the Console exposes saved filters / views on. */
export const LIST_PAGES = [
  'tools',
  'sessions',
  'tasks',
  'events',
  'memory',
  'artifacts',
  'agents',
  'mcp_connections',
  'background_jobs',
  'flows',
  // Phase 73b (D-126) — the Live Runtime page's Console-DB-backed
  // saved-view chips persist topology / trace-tab presets here.
  'live_runtime'
] as const;
export type ListPage = (typeof LIST_PAGES)[number];

/**
 * `saved_filters` — per-operator saved filter chips on list pages.
 * NOT a runtime entity, NOT a server-side saved filter: the spec is
 * *applied* to a runtime list call; it persists Console-side only.
 */
export interface SavedFilter extends BaseRow {
  page: ListPage;
  name: string;
  /** JSON-encoded `events.Filter`-shaped (or page-specific) filter spec. */
  filter_spec_json: string;
}

/**
 * `saved_views` — per-operator dashboard layouts / column-set / sort.
 * NOT a runtime saved query, NOT a shared team view.
 */
export interface SavedView extends BaseRow {
  page: ListPage;
  name: string;
  /** JSON-encoded view spec (columns, sort, group-by, density). */
  view_spec_json: string;
}

export const THEMES = ['light', 'dark', 'system'] as const;
export type Theme = (typeof THEMES)[number];
export const DENSITIES = ['comfortable', 'compact'] as const;
export type Density = (typeof DENSITIES)[number];
export const MOTIONS = ['full', 'reduced'] as const;
export type Motion = (typeof MOTIONS)[number];

/**
 * `profiles` — the per-operator preference record (one row per operator
 * per browser profile). NOT a runtime user record.
 */
export interface Profile extends BaseRow {
  theme: Theme;
  density: Density;
  motion: Motion;
  /** IANA timezone name; null = browser default. */
  tz: string | null;
  /** BCP-47 locale; null = browser default. */
  locale: string | null;
  /** 16-byte PBKDF2 KEK-derivation salt (Brief 12 auth-storage model). */
  kdf_salt: Uint8Array;
}

export const TRANSPORTS = ['sse_rest', 'ws', 'stdio'] as const;
export type Transport = (typeof TRANSPORTS)[number];

/**
 * `runtime_registry` — per-operator address book of Harbor runtimes the
 * Console knows about (Brief 11 §CC-1). The runtime does NOT know it is in
 * this list; this is NOT a runtime-side authorized-controller allowlist.
 */
export interface RuntimeRegistryRow extends BaseRow {
  name: string;
  base_url: string;
  transport: Transport;
  /** 0/1 — single-default model. */
  is_default: number;
  /** Unix epoch millis; null if never connected. */
  last_connected_at: number | null;
  /** Protocol version observed at last handshake; null if unknown. */
  protocol_version: string | null;
}

/** JWT signing algorithms — the asymmetric allowlist (CLAUDE.md §7). */
export const JWT_ALGORITHMS = ['RS256', 'RS384', 'RS512', 'ES256', 'ES384', 'ES512'] as const;
export type JwtAlgorithm = (typeof JWT_ALGORITHMS)[number];

/**
 * `auth_profiles` — per-(operator, runtime) encrypted JWT blob + metadata.
 * NOT a runtime auth record: the runtime issues the JWT; the Console DB
 * stores its operator's at-rest encrypted copy.
 */
export interface AuthProfile extends BaseRow {
  /** FK-shape onto `runtime_registry.id` (driver-enforced; IDB is schemaless). */
  runtime_id: string;
  /** JWT `iss` claim, decoded once at attach-time and cached; null if absent. */
  issuer: string | null;
  algorithm: JwtAlgorithm;
  /** Unix epoch millis; cached at attach-time; null if unknown. */
  expires_at: number | null;
  /** AES-GCM envelope: IV (12B) || ciphertext+authTag. */
  encrypted_jwt_blob: Uint8Array;
  /** 12-byte AES-GCM IV (also embedded in the blob; stored for rotation tests). */
  iv: Uint8Array;
}

/**
 * `pat_store` — per-operator Console-local Personal Access Tokens,
 * encrypted at rest. NOT a runtime-side token table; the runtime owns the
 * canonical PAT registry.
 */
export interface PATEntry extends BaseRow {
  name: string;
  /** null = "all runtimes"; non-null ties the PAT to one runtime row. */
  runtime_id: string | null;
  /** Cached human-readable scope label; the runtime is the source of truth. */
  scope_summary: string | null;
  /** AES-GCM envelope ciphertext. */
  encrypted_token_blob: Uint8Array;
  /** 12-byte AES-GCM IV. */
  iv: Uint8Array;
  /** Unix epoch millis; opportunistically updated; not authoritative. */
  last_used_at: number | null;
}

/** Notification classes the routing matrix covers (Brief 11 §CC-3 starter list). */
export const NOTIFICATION_CLASSES = [
  'governance_budget_exceeded',
  'tool_auth_required',
  'tool_approval_required',
  'task_failed',
  'agent_credentials_expired',
  'runtime_health_degraded'
] as const;
export type NotificationClass = (typeof NOTIFICATION_CLASSES)[number];

/** Notification delivery transports (V1 wires `in_app` only). */
export const NOTIFICATION_TRANSPORTS = ['in_app', 'email', 'webhook', 'web_push'] as const;
export type NotificationTransport = (typeof NOTIFICATION_TRANSPORTS)[number];

/**
 * `notifications_routing` — per-operator routing matrix for `notification.*`
 * classes (Brief 11 §CC-3). NOT the source of `notification.*` events: the
 * runtime emits them via Phase 72d's topic; this table decides which
 * transports light up on receipt.
 */
export interface NotificationRoutingRow extends BaseRow {
  notification_class: NotificationClass;
  transport: NotificationTransport;
  /** 0/1. */
  enabled: number;
  /** Transport-specific target spec; null when transport=in_app. */
  target_json: string | null;
}

/**
 * `keybindings` — per-operator keybinding overrides over the Console
 * default set (Brief 11 §CC-5). NOT a runtime config.
 */
export interface KeybindingRow extends BaseRow {
  /** Keybinding action identifier (e.g. `open_command_palette`, `nav_sessions`). */
  action: string;
  /** Canonical key-chord string (e.g. `"cmd+k"`, `"g s"`). */
  key_chord: string;
}

/** Discriminated map from table name to its row type. */
export interface TableRowMap {
  saved_filters: SavedFilter;
  saved_views: SavedView;
  profiles: Profile;
  runtime_registry: RuntimeRegistryRow;
  auth_profiles: AuthProfile;
  pat_store: PATEntry;
  notifications_routing: NotificationRoutingRow;
  keybindings: KeybindingRow;
}

/* ---------------------------------------------------------------------- */
/* Validation                                                             */
/* ---------------------------------------------------------------------- */

import { ErrSchemaValidation } from './errors.js';

function requireString(row: Record<string, unknown>, field: string, table: TableName): void {
  const v = row[field];
  if (typeof v !== 'string' || v.length === 0) {
    throw new ErrSchemaValidation(`console-db: ${table}.${field} must be a non-empty string`);
  }
}

function requireNumber(row: Record<string, unknown>, field: string, table: TableName): void {
  if (typeof row[field] !== 'number' || Number.isNaN(row[field])) {
    throw new ErrSchemaValidation(`console-db: ${table}.${field} must be a number`);
  }
}

function requireEnum<T extends string>(
  row: Record<string, unknown>,
  field: string,
  allowed: readonly T[],
  table: TableName
): void {
  if (!allowed.includes(row[field] as T)) {
    throw new ErrSchemaValidation(
      `console-db: ${table}.${field} must be one of [${allowed.join(', ')}], got ${String(row[field])}`
    );
  }
}

function requireBytes(row: Record<string, unknown>, field: string, table: TableName): void {
  if (!(row[field] instanceof Uint8Array)) {
    throw new ErrSchemaValidation(`console-db: ${table}.${field} must be a Uint8Array`);
  }
}

/** Validates the `BaseRow` columns common to every table. */
function validateBase(row: Record<string, unknown>, table: TableName): void {
  requireString(row, 'operator_id', table);
  requireString(row, 'id', table);
  requireNumber(row, 'created_at', table);
  requireNumber(row, 'updated_at', table);
}

/**
 * Validates a row against the schema for `table`. Throws
 * {@link ErrSchemaValidation} on a missing `operator_id` / `id`, a missing
 * column, or an out-of-enum value (acceptance criterion 2 + the
 * `schema.spec.ts` contract). Returns the row typed on success.
 */
export function validateRow<T extends TableName>(
  table: T,
  row: unknown
): TableRowMap[T] {
  if (typeof row !== 'object' || row === null) {
    throw new ErrSchemaValidation(`console-db: ${table} row must be an object`);
  }
  const r = row as Record<string, unknown>;
  validateBase(r, table);

  switch (table) {
    case 'saved_filters':
      requireEnum(r, 'page', LIST_PAGES, table);
      requireString(r, 'name', table);
      requireString(r, 'filter_spec_json', table);
      break;
    case 'saved_views':
      requireEnum(r, 'page', LIST_PAGES, table);
      requireString(r, 'name', table);
      requireString(r, 'view_spec_json', table);
      break;
    case 'profiles':
      requireEnum(r, 'theme', THEMES, table);
      requireEnum(r, 'density', DENSITIES, table);
      requireEnum(r, 'motion', MOTIONS, table);
      requireBytes(r, 'kdf_salt', table);
      break;
    case 'runtime_registry':
      requireString(r, 'name', table);
      requireString(r, 'base_url', table);
      requireEnum(r, 'transport', TRANSPORTS, table);
      requireNumber(r, 'is_default', table);
      break;
    case 'auth_profiles':
      requireString(r, 'runtime_id', table);
      requireEnum(r, 'algorithm', JWT_ALGORITHMS, table);
      requireBytes(r, 'encrypted_jwt_blob', table);
      requireBytes(r, 'iv', table);
      break;
    case 'pat_store':
      requireString(r, 'name', table);
      requireBytes(r, 'encrypted_token_blob', table);
      requireBytes(r, 'iv', table);
      break;
    case 'notifications_routing':
      requireEnum(r, 'notification_class', NOTIFICATION_CLASSES, table);
      requireEnum(r, 'transport', NOTIFICATION_TRANSPORTS, table);
      requireNumber(r, 'enabled', table);
      break;
    case 'keybindings':
      requireString(r, 'action', table);
      requireString(r, 'key_chord', table);
      break;
    default: {
      // Exhaustiveness guard — a new table without a validation case fails loudly.
      const _exhaustive: never = table;
      throw new ErrSchemaValidation(`console-db: no validation case for table ${String(_exhaustive)}`);
    }
  }
  // Cast through `unknown`: `T` is a generic table-name and TS reduces the
  // `TableRowMap[T]` intersection to `never`; the switch above has already
  // proven the row satisfies the per-table schema.
  return r as unknown as TableRowMap[T];
}

/**
 * Computes the per-operator row-scope key from a `(tenant, user)` identity.
 * `operator_id = base64url(sha256(tenant_id || ':' || user_id))`.
 */
export async function operatorIdOf(tenantID: string, userID: string): Promise<string> {
  if (tenantID.length === 0 || userID.length === 0) {
    throw new ErrSchemaValidation('console-db: tenantID and userID must both be non-empty');
  }
  const enc = new TextEncoder();
  const digest = await globalThis.crypto.subtle.digest(
    'SHA-256',
    enc.encode(`${tenantID}:${userID}`)
  );
  return base64url(new Uint8Array(digest));
}

/** Base64url-encodes bytes without padding. */
function base64url(bytes: Uint8Array): string {
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
