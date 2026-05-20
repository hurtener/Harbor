/**
 * Schema validation tests (Phase 72h test plan — `schema.spec.ts`).
 *
 * Each of the eight tables validates a happy-path record and rejects
 * (a) a missing `operator_id`, (b) a missing `id`, (c) an unknown enum
 * value where one is pinned.
 */
import { describe, expect, it } from 'vitest';
import { TABLE_NAMES, operatorIdOf, validateRow, type TableName } from '../schema.js';
import { ErrSchemaValidation } from '../errors.js';
import {
  authProfile,
  keybinding,
  notificationRouting,
  patEntry,
  profile,
  runtimeRow,
  savedFilter,
  savedView
} from './fixtures.js';

const OP = 'op-test';

async function happyRow(table: TableName): Promise<Record<string, unknown>> {
  switch (table) {
    case 'saved_filters':
      return savedFilter(OP) as unknown as Record<string, unknown>;
    case 'saved_views':
      return savedView(OP) as unknown as Record<string, unknown>;
    case 'profiles':
      return profile(OP) as unknown as Record<string, unknown>;
    case 'runtime_registry':
      return runtimeRow(OP) as unknown as Record<string, unknown>;
    case 'auth_profiles':
      return (await authProfile(OP, 'rt-1')) as unknown as Record<string, unknown>;
    case 'pat_store':
      return (await patEntry(OP)) as unknown as Record<string, unknown>;
    case 'notifications_routing':
      return notificationRouting(OP) as unknown as Record<string, unknown>;
    case 'keybindings':
      return keybinding(OP) as unknown as Record<string, unknown>;
  }
}

describe('schema: every table validates a happy-path record', () => {
  for (const table of TABLE_NAMES) {
    it(`${table}: accepts a valid row`, async () => {
      const row = await happyRow(table);
      expect(() => validateRow(table, row)).not.toThrow();
    });

    it(`${table}: rejects a missing operator_id`, async () => {
      const row = await happyRow(table);
      delete row.operator_id;
      expect(() => validateRow(table, row)).toThrow(ErrSchemaValidation);
    });

    it(`${table}: rejects a missing id`, async () => {
      const row = await happyRow(table);
      delete row.id;
      expect(() => validateRow(table, row)).toThrow(ErrSchemaValidation);
    });
  }
});

describe('schema: rejects out-of-enum values where one is pinned', () => {
  it('saved_filters: rejects an unknown page', async () => {
    const row = { ...savedFilter(OP), page: 'not_a_page' };
    expect(() => validateRow('saved_filters', row)).toThrow(ErrSchemaValidation);
  });

  it('profiles: rejects an unknown theme', () => {
    const row = { ...profile(OP), theme: 'neon' };
    expect(() => validateRow('profiles', row)).toThrow(ErrSchemaValidation);
  });

  it('runtime_registry: rejects an unknown transport', () => {
    const row = { ...runtimeRow(OP), transport: 'carrier_pigeon' };
    expect(() => validateRow('runtime_registry', row)).toThrow(ErrSchemaValidation);
  });

  it('auth_profiles: rejects a non-allowlist algorithm', async () => {
    const row = { ...(await authProfile(OP, 'rt-1')), algorithm: 'HS256' };
    expect(() => validateRow('auth_profiles', row)).toThrow(ErrSchemaValidation);
  });

  it('notifications_routing: rejects an unknown transport', () => {
    const row = { ...notificationRouting(OP), transport: 'carrier_pigeon' };
    expect(() => validateRow('notifications_routing', row)).toThrow(ErrSchemaValidation);
  });
});

describe('schema: operatorIdOf', () => {
  it('produces a stable base64url hash for a (tenant, user) pair', async () => {
    const a = await operatorIdOf('tenant-1', 'user-1');
    const b = await operatorIdOf('tenant-1', 'user-1');
    expect(a).toBe(b);
    expect(a).toMatch(/^[A-Za-z0-9_-]+$/);
  });

  it('produces distinct hashes for distinct operators', async () => {
    const a = await operatorIdOf('tenant-1', 'user-1');
    const b = await operatorIdOf('tenant-1', 'user-2');
    const c = await operatorIdOf('tenant-2', 'user-1');
    expect(new Set([a, b, c]).size).toBe(3);
  });

  it('rejects an empty identity component', async () => {
    await expect(operatorIdOf('', 'user-1')).rejects.toThrow(ErrSchemaValidation);
  });
});
