/**
 * Vitest setup for the Console DB suite (Phase 72h).
 *
 * jsdom provides `crypto.subtle` (WebCrypto) but NOT IndexedDB — the
 * `fake-indexeddb` polyfill installs a spec-compliant in-memory IndexedDB
 * so `drivers/indexeddb.ts` runs against a real (faked) IDB engine, not a
 * mock (CLAUDE.md §17.3 — real drivers everywhere on the seam).
 */
import 'fake-indexeddb/auto';
