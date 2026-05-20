/**
 * Shared test fixtures for the Console DB suite (Phase 72h).
 *
 * Row builders produce schema-valid rows with documented dummy values
 * (CLAUDE.md §13 — no real secrets). Each builder takes the `operator_id`
 * so cross-operator isolation tests can mint rows for distinct operators.
 */
import { deriveMasterKey, encrypt, generateKdfSalt } from '../crypto.js';
import type {
  AuthProfile,
  KeybindingRow,
  NotificationRoutingRow,
  PATEntry,
  Profile,
  RuntimeRegistryRow,
  SavedFilter,
  SavedView
} from '../schema.js';

let ulidCounter = 0;
/** Deterministic monotonic id generator for tests (not a real ULID). */
export function testID(prefix = 'id'): string {
  ulidCounter += 1;
  return `${prefix}-${ulidCounter.toString().padStart(6, '0')}`;
}

const NOW = 1_700_000_000_000;

export function savedFilter(operatorID: string, id = testID('sf')): SavedFilter {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    page: 'tools',
    name: 'My HTTP tools',
    filter_spec_json: JSON.stringify({ transport: 'http' })
  };
}

export function savedView(operatorID: string, id = testID('sv')): SavedView {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    page: 'sessions',
    name: 'Active sessions',
    view_spec_json: JSON.stringify({ columns: ['id', 'status'], sort: 'updated_at' })
  };
}

export function profile(operatorID: string, id = testID('pr')): Profile {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    theme: 'dark',
    density: 'comfortable',
    motion: 'full',
    tz: 'UTC',
    locale: 'en-US',
    kdf_salt: generateKdfSalt()
  };
}

export function runtimeRow(operatorID: string, id = testID('rt')): RuntimeRegistryRow {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    name: 'Local dev runtime',
    base_url: 'http://127.0.0.1:18080',
    transport: 'sse_rest',
    is_default: 1,
    last_connected_at: NOW,
    protocol_version: '1.0.0'
  };
}

/** Builds an encrypted `auth_profiles` row using a real AES-GCM envelope. */
export async function authProfile(
  operatorID: string,
  runtimeID: string,
  id = testID('ap')
): Promise<AuthProfile> {
  const key = await deriveMasterKey('fixture-passphrase', generateKdfSalt());
  // Documented dummy JWT — not a real secret (CLAUDE.md §13).
  const dummyJWT = 'eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJkdW1teSJ9.ZHVtbXk';
  const blob = await encrypt(new TextEncoder().encode(dummyJWT), key);
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    runtime_id: runtimeID,
    issuer: 'https://issuer.example',
    algorithm: 'ES256',
    expires_at: NOW + 3_600_000,
    encrypted_jwt_blob: blob,
    iv: blob.subarray(0, 12)
  };
}

/** Builds an encrypted `pat_store` row using a real AES-GCM envelope. */
export async function patEntry(operatorID: string, id = testID('pat')): Promise<PATEntry> {
  const key = await deriveMasterKey('fixture-passphrase', generateKdfSalt());
  const dummyPAT = 'hbr_pat_dummy_value';
  const blob = await encrypt(new TextEncoder().encode(dummyPAT), key);
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    name: 'CI token',
    runtime_id: null,
    scope_summary: 'read-only',
    encrypted_token_blob: blob,
    iv: blob.subarray(0, 12),
    last_used_at: null
  };
}

export function notificationRouting(
  operatorID: string,
  id = testID('nr')
): NotificationRoutingRow {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    notification_class: 'task_failed',
    transport: 'in_app',
    enabled: 1,
    target_json: null
  };
}

export function keybinding(operatorID: string, id = testID('kb')): KeybindingRow {
  return {
    operator_id: operatorID,
    id,
    created_at: NOW,
    updated_at: NOW,
    action: 'open_command_palette',
    key_chord: 'cmd+k'
  };
}
