/**
 * Console DB error taxonomy (Phase 72h).
 *
 * Every failure mode in the Console DB module surfaces as one of these
 * typed errors — never a silent `null` return (CLAUDE.md §5 fail-loudly).
 */

/**
 * Raised when AES-GCM decryption of an `auth_profiles` / `pat_store` blob
 * fails — typically because the operator's passphrase (and therefore the
 * derived master key) is wrong, or the ciphertext is corrupt.
 *
 * The Settings page MUST distinguish this from a token-expired condition:
 * a decryption failure means the operator must re-enter the passphrase or
 * re-attach the runtime; it is NOT an "auth missing → redirect to login"
 * signal (see the phase plan's Risks section).
 */
export class ErrAuthDecryption extends Error {
  constructor(message = 'console-db: auth blob decryption failed (wrong passphrase or corrupt ciphertext)') {
    super(message);
    this.name = 'ErrAuthDecryption';
  }
}

/**
 * Raised when a driver write is attempted without an `operatorID`. Every
 * Console DB row is per-operator scoped; a missing operator id is a
 * fail-loud condition, never a silent default (acceptance criterion 2).
 */
export class ErrMissingOperator extends Error {
  constructor(message = 'console-db: operatorID is required on every read and write (per-operator scoping is mandatory)') {
    super(message);
    this.name = 'ErrMissingOperator';
  }
}

/**
 * Raised when the migration list is internally inconsistent — a duplicate
 * version, a non-monotonic version sequence, or a destructive shape
 * (`DROP TABLE` / `ALTER COLUMN`) that violates the forward-only rule
 * (CLAUDE.md §9).
 */
export class ErrMigrationConflict extends Error {
  constructor(message = 'console-db: migration list is inconsistent') {
    super(message);
    this.name = 'ErrMigrationConflict';
  }
}

/**
 * Raised when a row fails schema validation (missing `operator_id`,
 * missing `id`, or an out-of-enum value where one is pinned).
 */
export class ErrSchemaValidation extends Error {
  constructor(message = 'console-db: row failed schema validation') {
    super(message);
    this.name = 'ErrSchemaValidation';
  }
}

/**
 * Raised when an unknown driver name is passed to `openConsoleDB`. The
 * message lists the registered drivers so a misconfiguration is obvious
 * (CLAUDE.md §4.4 rule 6).
 */
export class ErrUnknownDriver extends Error {
  constructor(name: string, registered: readonly string[]) {
    super(
      `console-db: unknown driver "${name}"; registered drivers: ${registered.join(', ')}`
    );
    this.name = 'ErrUnknownDriver';
  }
}
