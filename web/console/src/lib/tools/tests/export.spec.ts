/**
 * Tools-page Export helper tests (Phase 73f / D-116).
 *
 * The Export control serialises the currently-filtered catalog rows to
 * CSV / JSON. These tests pin the column order, the RFC-4180 cell
 * quoting, and the JSON round-trip.
 */
import { describe, expect, it } from 'vitest';
import { exportToolsCSV, exportToolsJSON } from '../export.js';
import type { Tool } from '../../protocol/tools.js';

function tool(overrides: Partial<Tool> = {}): Tool {
  return {
    id: 'echo',
    name: 'echo',
    version: '1.0.0',
    description: 'echoes input',
    scope: 'tenant',
    transport: 'in-proc',
    oauth_status: 'n/a',
    approval_policy: 'auto',
    reliability_tier: 'standard',
    owner: '',
    last_used_at: '0001-01-01T00:00:00Z',
    ...overrides
  };
}

describe('exportToolsCSV', () => {
  it('emits a header row plus one row per tool', () => {
    const csv = exportToolsCSV([tool(), tool({ id: 'web', name: 'web' })]);
    const lines = csv.split('\r\n');
    expect(lines).toHaveLength(3);
    expect(lines[0]).toBe(
      'id,name,version,scope,transport,oauth_status,approval_policy,reliability_tier,owner,last_used_at'
    );
    expect(lines[1].startsWith('echo,echo,1.0.0,tenant,in-proc')).toBe(true);
  });

  it('quotes cells containing commas, quotes, or newlines', () => {
    const csv = exportToolsCSV([tool({ owner: 'team, alpha' })]);
    expect(csv).toContain('"team, alpha"');
  });

  it('escapes embedded double quotes by doubling them', () => {
    const csv = exportToolsCSV([tool({ description: 'n/a', owner: 'a"b' })]);
    expect(csv).toContain('"a""b"');
  });

  it('emits a header-only document for an empty catalog', () => {
    const csv = exportToolsCSV([]);
    expect(csv.split('\r\n')).toHaveLength(1);
  });
});

describe('exportToolsJSON', () => {
  it('round-trips the catalog rows', () => {
    const rows = [tool(), tool({ id: 'web' })];
    const parsed = JSON.parse(exportToolsJSON(rows)) as Tool[];
    expect(parsed).toEqual(rows);
  });

  it('pretty-prints with two-space indentation', () => {
    expect(exportToolsJSON([tool()])).toContain('\n  ');
  });
});
