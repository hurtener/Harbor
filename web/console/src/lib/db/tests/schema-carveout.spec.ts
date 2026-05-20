/**
 * §13 / D-061 Console-DB carve-out lint (Phase 72h).
 *
 * The Console DB holds Console-local state only — never a shadow source of
 * truth for runtime entities. This test FAILS the build if any forbidden
 * runtime-entity table name appears among the Console DB table names, OR
 * appears as a quoted table-name literal in `schema.ts`. A future page-
 * phase author tempted to "cache the agents list locally" is stopped here.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { FORBIDDEN_TABLE_NAMES, TABLE_NAMES } from '../schema.js';

// Resolved from the Vitest project root (web/console) — `import.meta.url`
// is not a `file:` URL under the jsdom environment.
const SCHEMA_TS = resolve(process.cwd(), 'src/lib/db/schema.ts');

describe('§13 D-061: Console DB carve-out', () => {
  it('no V1 table name is a forbidden runtime-entity name', () => {
    const forbidden = new Set<string>(FORBIDDEN_TABLE_NAMES);
    for (const t of TABLE_NAMES) {
      expect(forbidden.has(t), `table "${t}" is a forbidden runtime-entity name`).toBe(false);
    }
  });

  it('the TABLE_NAMES registry declares no forbidden table-name literal', () => {
    // The carve-out is about TABLE NAMES — the `TABLE_NAMES` array is the
    // single source of truth for what object stores (tables) exist. A
    // forbidden name as a *page enum value* (`LIST_PAGES` legitimately
    // includes `'agents'` — the Console renders an Agents list PAGE from
    // Protocol data) is NOT a violation; a forbidden name as a TABLE is.
    // Scan only the `export const TABLE_NAMES = [...] as const;` block.
    const src = readFileSync(SCHEMA_TS, 'utf8');
    const block = src.match(/export const TABLE_NAMES\s*=\s*\[([\s\S]*?)\]\s*as const;/);
    expect(block, 'schema.ts must declare a TABLE_NAMES array').not.toBeNull();
    const tableNamesSrc = block![1];
    for (const name of FORBIDDEN_TABLE_NAMES) {
      const quoted = new RegExp(`['"]${name}['"]`);
      expect(
        quoted.test(tableNamesSrc),
        `TABLE_NAMES declares forbidden runtime-entity table name "${name}" (§13 / D-061)`
      ).toBe(false);
    }
  });

  it('exactly eight V1 tables ship', () => {
    expect(TABLE_NAMES.length).toBe(8);
    expect(new Set(TABLE_NAMES).size).toBe(8);
  });
});
