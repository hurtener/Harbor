/**
 * Crypto round-trip tests (Phase 72h acceptance criterion: encryption
 * round-trip). Exercises the real `crypto.subtle` shim jsdom provides.
 */
import { describe, expect, it } from 'vitest';
import {
  DEFAULT_PBKDF2_ITERATIONS,
  decrypt,
  deriveMasterKey,
  encrypt,
  generateKdfSalt,
  ivOf
} from '../crypto.js';
import { ErrAuthDecryption } from '../errors.js';

// Documented dummy fixtures — NOT real secrets (CLAUDE.md §13).
const SAMPLE_JWT =
  'eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkdW1teSJ9.ZHVtbXlzaWduYXR1cmU';
const SAMPLE_PAT = 'hbr_pat_dummy_0123456789abcdef';
const PASSPHRASE = 'correct-horse-battery-staple';
const WRONG_PASSPHRASE = 'incorrect-passphrase';

function hex(bytes: Uint8Array): string {
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
}

describe('crypto: deriveMasterKey', () => {
  it('derives a stable AES-GCM key from a known salt + passphrase', async () => {
    const salt = new Uint8Array(16).fill(7);
    const k1 = await deriveMasterKey(PASSPHRASE, salt);
    const k2 = await deriveMasterKey(PASSPHRASE, salt);
    // The keys are non-extractable; stability is proven by round-tripping
    // a payload encrypted under k1 and decrypted under k2.
    const blob = await encrypt(new TextEncoder().encode('stable'), k1);
    const out = await decrypt(blob, k2);
    expect(new TextDecoder().decode(out)).toBe('stable');
  });

  it('rejects an empty passphrase', async () => {
    await expect(deriveMasterKey('', generateKdfSalt())).rejects.toThrow(/non-empty/);
  });

  it('rejects a wrong-length salt', async () => {
    await expect(deriveMasterKey(PASSPHRASE, new Uint8Array(8))).rejects.toThrow(/16 bytes/);
  });

  it('rejects an iteration count below the OWASP floor', async () => {
    await expect(
      deriveMasterKey(PASSPHRASE, generateKdfSalt(), DEFAULT_PBKDF2_ITERATIONS - 1)
    ).rejects.toThrow(/iterations must be/);
  });
});

describe('crypto: encrypt / decrypt round-trip', () => {
  it('round-trips a sample JWT', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    const plaintext = new TextEncoder().encode(SAMPLE_JWT);
    const blob = await encrypt(plaintext, key);
    const out = await decrypt(blob, key);
    expect(new TextDecoder().decode(out)).toBe(SAMPLE_JWT);
  });

  it('round-trips a sample PAT', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    const plaintext = new TextEncoder().encode(SAMPLE_PAT);
    const blob = await encrypt(plaintext, key);
    const out = await decrypt(blob, key);
    expect(new TextDecoder().decode(out)).toBe(SAMPLE_PAT);
  });

  it('produces opaque ciphertext — no plaintext JWT header leak', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    const blob = await encrypt(new TextEncoder().encode(SAMPLE_JWT), key);
    const ciphertextHex = hex(blob);
    // "eyJ" is the universal base64 JWT-header prefix; it must not survive
    // into the ciphertext.
    const eyJHex = hex(new TextEncoder().encode('eyJ'));
    expect(ciphertextHex.includes(eyJHex)).toBe(false);
  });

  it('emits a fresh IV per encryption (envelope prefix differs)', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    const a = await encrypt(new TextEncoder().encode('same'), key);
    const b = await encrypt(new TextEncoder().encode('same'), key);
    expect(hex(ivOf(a))).not.toBe(hex(ivOf(b)));
  });
});

describe('crypto: wrong-key failure (fails loudly)', () => {
  it('raises ErrAuthDecryption when decrypting with the wrong key', async () => {
    const salt = generateKdfSalt();
    const goodKey = await deriveMasterKey(PASSPHRASE, salt);
    const wrongKey = await deriveMasterKey(WRONG_PASSPHRASE, salt);
    const blob = await encrypt(new TextEncoder().encode(SAMPLE_JWT), goodKey);
    await expect(decrypt(blob, wrongKey)).rejects.toBeInstanceOf(ErrAuthDecryption);
  });

  it('raises ErrAuthDecryption on a truncated blob — never returns null', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    await expect(decrypt(new Uint8Array(4), key)).rejects.toBeInstanceOf(ErrAuthDecryption);
  });

  it('raises ErrAuthDecryption on a corrupt auth tag', async () => {
    const key = await deriveMasterKey(PASSPHRASE, generateKdfSalt());
    const blob = await encrypt(new TextEncoder().encode(SAMPLE_PAT), key);
    blob[blob.length - 1] ^= 0xff; // flip a ciphertext byte
    await expect(decrypt(blob, key)).rejects.toBeInstanceOf(ErrAuthDecryption);
  });
});
