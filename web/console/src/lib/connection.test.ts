/**
 * Wire-surface digest comparator unit tests.
 *
 * `compareWireDigest` is the pure, framework-free seam the Console uses to
 * detect connect-time wire-surface drift: it compares a live runtime's
 * reported digest against the digest this Console build vendored from
 * `wire-manifest.gen.json`. The three branches each carry a distinct
 * operator meaning, so each is pinned here.
 */
import { describe, expect, it } from 'vitest';
import { compareWireDigest } from './connection.js';

const SAMPLE = 'sha256:' + 'a'.repeat(64);

describe('compareWireDigest — the three-way drift verdict', () => {
	it("returns 'match' when the live and vendored digests are identical", () => {
		expect(compareWireDigest(SAMPLE, SAMPLE)).toBe('match');
	});

	it("returns 'drift' when both are present and differ", () => {
		const other = 'sha256:' + 'b'.repeat(64);
		expect(compareWireDigest(other, SAMPLE)).toBe('drift');
	});

	it("returns 'unsupported' when the live digest is undefined (runtime predates the field)", () => {
		expect(compareWireDigest(undefined, SAMPLE)).toBe('unsupported');
	});

	it("returns 'unsupported' for an empty live digest, never a false 'drift' alarm", () => {
		expect(compareWireDigest('', SAMPLE)).toBe('unsupported');
		expect(compareWireDigest('   ', SAMPLE)).toBe('unsupported');
	});

	it('is whitespace-insensitive on a genuine match (surrounding whitespace trimmed)', () => {
		expect(compareWireDigest(`  ${SAMPLE}  `, SAMPLE)).toBe('match');
	});

	it("an absent digest is classified 'unsupported' even when the vendored digest is itself empty", () => {
		// Absence proves nothing about drift — the verdict is unsupported, not match.
		expect(compareWireDigest(undefined, '')).toBe('unsupported');
	});

	it("returns 'unsupported' (never a false 'drift') when only the vendored digest is empty", () => {
		// A present live digest against an empty vendored one must not alarm:
		// the build simply has nothing to compare against.
		expect(compareWireDigest(SAMPLE, '')).toBe('unsupported');
		expect(compareWireDigest(SAMPLE, '   ')).toBe('unsupported');
	});
});
