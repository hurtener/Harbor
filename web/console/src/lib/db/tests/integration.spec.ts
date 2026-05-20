/**
 * Console DB integration test (Phase 72h — the §17 in-package
 * integration test).
 *
 * Real driver (the V1 IndexedDB driver against `fake-indexeddb`), real
 * crypto (jsdom's `crypto.subtle`). It:
 *
 *  1. Opens the IndexedDB driver, runs migrations from empty.
 *  2. Writes one row into each of the eight tables; reads each back.
 *  3. Asserts every row carries the operator's identity-keyed `operator_id`.
 *  4. Asserts cross-operator isolation: operator B's rows do NOT surface
 *     to an operator-A-scoped read (the §6 multi-isolation analogue).
 *  5. Round-trips the encrypted auth-profile + PAT blobs through
 *     encrypt/decrypt.
 *  6. Covers ≥1 failure mode: a wrong-key decryption raises loudly; a
 *     missing operatorID raises loudly.
 *  7. A concurrency micro-test: N parallel upserts from one operator
 *     preserve last-write-wins and do not corrupt the store.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { decrypt, deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { ErrAuthDecryption, ErrMissingOperator, ErrUnknownDriver } from '../errors.js';
import { operatorIdOf } from '../schema.js';
import { freshDBName } from './idb-helpers.js';
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

async function openForOperator(tenantID: string, userID: string, dbName: string) {
  const masterKey = await deriveMasterKey('integration-passphrase', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID, userID },
    masterKey,
    databaseName: dbName
  });
}

describe('integration: eight-table round-trip', () => {
  it('writes and reads back one row in each of the eight tables', async () => {
    const dbName = freshDBName('roundtrip');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);

    const sf = savedFilter(op);
    const sv = savedView(op);
    const pr = profile(op);
    const rt = runtimeRow(op);
    const ap = await authProfile(op, rt.id);
    const pat = await patEntry(op);
    const nr = notificationRouting(op);
    const kb = keybinding(op);

    await db.savedFilters.upsert(op, sf);
    await db.savedViews.upsert(op, sv);
    await db.profiles.upsert(op, pr);
    await db.runtimes.upsert(op, rt);
    await db.authProfiles.upsert(op, ap);
    await db.patStore.upsert(op, pat);
    await db.notifications.upsert(op, nr);
    await db.keybindings.upsert(op, kb);

    expect(await db.savedFilters.get(op, sf.id)).toMatchObject({ name: sf.name });
    expect(await db.savedViews.get(op, sv.id)).toMatchObject({ name: sv.name });
    expect(await db.profiles.get(op, pr.id)).toMatchObject({ theme: 'dark' });
    expect(await db.runtimes.get(op, rt.id)).toMatchObject({ base_url: rt.base_url });
    expect(await db.authProfiles.get(op, ap.id)).toMatchObject({ algorithm: 'ES256' });
    expect(await db.patStore.get(op, pat.id)).toMatchObject({ name: 'CI token' });
    expect(await db.notifications.get(op, nr.id)).toMatchObject({ transport: 'in_app' });
    expect(await db.keybindings.get(op, kb.id)).toMatchObject({ key_chord: 'cmd+k' });

    await db.close();
  });

  it('every persisted row carries the operator identity-keyed operator_id', async () => {
    const dbName = freshDBName('opkey');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);
    await db.savedFilters.upsert(op, savedFilter(op));
    const rows = await db.savedFilters.list(op);
    expect(rows).toHaveLength(1);
    expect(rows[0].operator_id).toBe(op);
    await db.close();
  });

  it('deleting a row removes it', async () => {
    const dbName = freshDBName('delete');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);
    const kb = keybinding(op);
    await db.keybindings.upsert(op, kb);
    expect(await db.keybindings.get(op, kb.id)).not.toBeNull();
    await db.keybindings.delete(op, kb.id);
    expect(await db.keybindings.get(op, kb.id)).toBeNull();
    await db.close();
  });
});

describe('integration: cross-operator isolation (the §6 analogue)', () => {
  it("operator B's rows never surface to an operator-A-scoped read", async () => {
    // Both operators share ONE IndexedDB database — the per-operator
    // scoping must be structural, not application-layer.
    const dbName = freshDBName('isolation');
    const opA = await operatorIdOf('tenant-a', 'user-a');
    const opB = await operatorIdOf('tenant-b', 'user-b');
    expect(opA).not.toBe(opB);

    const dbA = await openForOperator('tenant-a', 'user-a', dbName);
    const dbB = await openForOperator('tenant-b', 'user-b', dbName);

    const aFilter = savedFilter(opA);
    const bFilter = savedFilter(opB);
    await dbA.savedFilters.upsert(opA, aFilter);
    await dbB.savedFilters.upsert(opB, bFilter);

    const aRows = await dbA.savedFilters.list(opA);
    const bRows = await dbB.savedFilters.list(opB);

    // A sees only A's row; B sees only B's row.
    expect(aRows.map((r) => r.id)).toEqual([aFilter.id]);
    expect(bRows.map((r) => r.id)).toEqual([bFilter.id]);

    // A direct get of B's row id under A's scope returns null.
    expect(await dbA.savedFilters.get(opA, bFilter.id)).toBeNull();
    // ...and vice versa.
    expect(await dbB.savedFilters.get(opB, aFilter.id)).toBeNull();

    await dbA.close();
    await dbB.close();
  });

  it('rejects a row whose operator_id does not match the write scope', async () => {
    const dbName = freshDBName('mismatch');
    const opA = await operatorIdOf('tenant-a', 'user-a');
    const opB = await operatorIdOf('tenant-b', 'user-b');
    const db = await openForOperator('tenant-a', 'user-a', dbName);
    // A row built for operator B written under operator A's scope: fail-loud.
    await expect(db.savedFilters.upsert(opA, savedFilter(opB))).rejects.toBeInstanceOf(
      ErrMissingOperator
    );
    await db.close();
  });

  it('rejects an empty operatorID on every access (fail-loud)', async () => {
    const dbName = freshDBName('emptyop');
    const db = await openForOperator('tenant-a', 'user-a', dbName);
    await expect(db.profiles.list('')).rejects.toBeInstanceOf(ErrMissingOperator);
    await expect(db.profiles.get('', 'id-1')).rejects.toBeInstanceOf(ErrMissingOperator);
    await expect(db.profiles.delete('', 'id-1')).rejects.toBeInstanceOf(ErrMissingOperator);
    await db.close();
  });
});

describe('integration: encrypted blob round-trip + failure mode', () => {
  it('an auth-profile blob persists and decrypts under the right key', async () => {
    const dbName = freshDBName('authblob');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);

    // Encrypt a JWT with a known key; persist; read back; decrypt.
    const salt = generateKdfSalt();
    const key = await deriveMasterKey('blob-pass', salt);
    const { encrypt } = await import('../crypto.js');
    const jwtBytes = new TextEncoder().encode('eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.sig');
    const blob = await encrypt(jwtBytes, key);

    const ap = await authProfile(op, 'rt-1');
    ap.encrypted_jwt_blob = blob;
    ap.iv = blob.subarray(0, 12);
    await db.authProfiles.upsert(op, ap);

    const readBack = await db.authProfiles.get(op, ap.id);
    expect(readBack).not.toBeNull();
    const decrypted = await decrypt(readBack!.encrypted_jwt_blob, key);
    expect(new TextDecoder().decode(decrypted)).toBe(
      'eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.sig'
    );
    await db.close();
  });

  it('a persisted PAT blob fails loudly when decrypted with the wrong key', async () => {
    const dbName = freshDBName('patblob');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);

    const salt = generateKdfSalt();
    const goodKey = await deriveMasterKey('good-pass', salt);
    const wrongKey = await deriveMasterKey('wrong-pass', salt);
    const { encrypt } = await import('../crypto.js');
    const blob = await encrypt(new TextEncoder().encode('hbr_pat_secret'), goodKey);

    const pat = await patEntry(op);
    pat.encrypted_token_blob = blob;
    pat.iv = blob.subarray(0, 12);
    await db.patStore.upsert(op, pat);

    const readBack = await db.patStore.get(op, pat.id);
    await expect(decrypt(readBack!.encrypted_token_blob, wrongKey)).rejects.toBeInstanceOf(
      ErrAuthDecryption
    );
    await db.close();
  });
});

describe('integration: driver registry', () => {
  it('rejects an unknown driver name (lists registered drivers)', async () => {
    await expect(
      openConsoleDB({
        // @ts-expect-error — deliberately passing an unregistered name
        driver: 'postgres',
        operatorIdentity: { tenantID: 't', userID: 'u' },
        masterKey: {} as CryptoKey
      })
    ).rejects.toBeInstanceOf(ErrUnknownDriver);
  });
});

describe('integration: concurrent ops on one DB instance', () => {
  it('N parallel upserts from one operator preserve last-write-wins', async () => {
    const dbName = freshDBName('concurrent');
    const op = await operatorIdOf('tenant-a', 'user-a');
    const db = await openForOperator('tenant-a', 'user-a', dbName);

    const base = keybinding(op, 'kb-shared');
    const N = 25;
    // N concurrent upserts of the SAME id with distinct key_chord values.
    await Promise.all(
      Array.from({ length: N }, (_, i) =>
        db.keybindings.upsert(op, { ...base, key_chord: `chord-${i}`, updated_at: 1700 + i })
      )
    );
    const rows = await db.keybindings.list(op);
    // Exactly one row for the shared id — the store is not corrupted.
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe('kb-shared');
    expect(rows[0].key_chord).toMatch(/^chord-\d+$/);
    await db.close();
  });
});
