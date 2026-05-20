/**
 * Console DB WebCrypto envelope (Phase 72h).
 *
 * `auth_profiles.encrypted_jwt_blob` and `pat_store.encrypted_token_blob`
 * are encrypted at rest per the Brief 12 auth-storage threat model:
 *
 *   - The KEK (master key) is derived via PBKDF2 from a passphrase the
 *     operator enters on first runtime-attach, salted with a per-operator
 *     16-byte random salt persisted on `profiles.kdf_salt`.
 *   - The DEK encrypts each secret blob via AES-GCM with a per-row random
 *     12-byte IV.
 *   - The wire envelope is `IV (12 bytes) || ciphertext+authTag`, so the
 *     blob is self-describing and a separately-stored `iv` column is only
 *     a convenience for rotation tests.
 *
 * Loss of the passphrase invalidates stored tokens (decryption fails
 * loudly with `ErrAuthDecryption`) but does NOT corrupt the rest of the
 * Console DB.
 *
 * Parameters are pinned here (acceptance criterion 3); a future audit may
 * bump `DEFAULT_PBKDF2_ITERATIONS` — `deriveMasterKey` takes `iterations`
 * as an optional argument so rotation is a config bump, not a schema bump.
 */
import { ErrAuthDecryption } from './errors.js';

/** Minimum PBKDF2 iteration count (current OWASP guidance for SHA-256). */
export const DEFAULT_PBKDF2_ITERATIONS = 100_000;

/** AES-GCM IV length in bytes. */
export const IV_LENGTH = 12;

/** PBKDF2 salt length in bytes (persisted on `profiles.kdf_salt`). */
export const KDF_SALT_LENGTH = 16;

/** PBKDF2 KDF hash. */
const KDF_HASH = 'SHA-256';

/** AES key length in bits. */
const AES_KEY_BITS = 256;

/**
 * Returns the platform `SubtleCrypto` implementation, failing loudly if
 * WebCrypto is unavailable (CLAUDE.md §5 — no silent degradation).
 */
function subtle(): SubtleCrypto {
  const c = globalThis.crypto;
  if (!c || !c.subtle) {
    throw new Error('console-db: WebCrypto (crypto.subtle) is unavailable in this environment');
  }
  return c.subtle;
}

/**
 * Generates a cryptographically-random 16-byte PBKDF2 salt. Persisted on
 * `profiles.kdf_salt` so a single iteration-count change does not
 * invalidate every operator's stored blobs at once.
 */
export function generateKdfSalt(): Uint8Array {
  return globalThis.crypto.getRandomValues(new Uint8Array(KDF_SALT_LENGTH));
}

/**
 * Derives the AES-GCM master key (KEK) from the operator's passphrase via
 * PBKDF2. The derived key is non-extractable — it never leaves WebCrypto.
 *
 * @param passphrase  the operator's session passphrase
 * @param salt        the per-operator salt from `profiles.kdf_salt`
 * @param iterations  PBKDF2 iteration count; defaults to {@link DEFAULT_PBKDF2_ITERATIONS}
 */
export async function deriveMasterKey(
  passphrase: string,
  salt: Uint8Array,
  iterations: number = DEFAULT_PBKDF2_ITERATIONS
): Promise<CryptoKey> {
  if (passphrase.length === 0) {
    throw new Error('console-db: passphrase must be non-empty');
  }
  if (salt.byteLength !== KDF_SALT_LENGTH) {
    throw new Error(`console-db: kdf salt must be ${KDF_SALT_LENGTH} bytes, got ${salt.byteLength}`);
  }
  if (iterations < DEFAULT_PBKDF2_ITERATIONS) {
    throw new Error(
      `console-db: PBKDF2 iterations must be >= ${DEFAULT_PBKDF2_ITERATIONS}, got ${iterations}`
    );
  }
  const enc = new TextEncoder();
  const baseKey = await subtle().importKey(
    'raw',
    enc.encode(passphrase),
    'PBKDF2',
    false,
    ['deriveKey']
  );
  return subtle().deriveKey(
    {
      name: 'PBKDF2',
      salt: salt as BufferSource,
      iterations,
      hash: KDF_HASH
    },
    baseKey,
    { name: 'AES-GCM', length: AES_KEY_BITS },
    false,
    ['encrypt', 'decrypt']
  );
}

/**
 * Encrypts `plaintext` under `masterKey` via AES-GCM with a fresh random
 * IV. The returned blob is `IV (12 bytes) || ciphertext+authTag`.
 */
export async function encrypt(plaintext: Uint8Array, masterKey: CryptoKey): Promise<Uint8Array> {
  const iv = globalThis.crypto.getRandomValues(new Uint8Array(IV_LENGTH));
  const ciphertext = await subtle().encrypt(
    { name: 'AES-GCM', iv: iv as BufferSource },
    masterKey,
    plaintext as BufferSource
  );
  const blob = new Uint8Array(IV_LENGTH + ciphertext.byteLength);
  blob.set(iv, 0);
  blob.set(new Uint8Array(ciphertext), IV_LENGTH);
  return blob;
}

/**
 * Decrypts a blob produced by {@link encrypt}. Fails loudly with
 * {@link ErrAuthDecryption} on a wrong key or corrupt ciphertext — never
 * a silent `null` return (acceptance criterion 3 + CLAUDE.md §5).
 */
export async function decrypt(ciphertext: Uint8Array, masterKey: CryptoKey): Promise<Uint8Array> {
  if (ciphertext.byteLength <= IV_LENGTH) {
    throw new ErrAuthDecryption('console-db: ciphertext too short to contain an IV');
  }
  const iv = ciphertext.subarray(0, IV_LENGTH);
  const body = ciphertext.subarray(IV_LENGTH);
  try {
    const plaintext = await subtle().decrypt(
      { name: 'AES-GCM', iv: iv as BufferSource },
      masterKey,
      body as BufferSource
    );
    return new Uint8Array(plaintext);
  } catch {
    // AES-GCM auth-tag mismatch (wrong key / corrupt blob) surfaces as a
    // loud typed error — the Settings page distinguishes this from a
    // token-expired condition (phase plan Risks section).
    throw new ErrAuthDecryption();
  }
}

/**
 * Extracts the 12-byte IV prefix from an envelope blob. The `iv` column on
 * `auth_profiles` / `pat_store` mirrors this for rotation-test ergonomics.
 */
export function ivOf(blob: Uint8Array): Uint8Array {
  if (blob.byteLength <= IV_LENGTH) {
    throw new ErrAuthDecryption('console-db: blob too short to contain an IV');
  }
  return blob.subarray(0, IV_LENGTH);
}
