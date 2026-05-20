import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

/**
 * Harbor Console Vite + Vitest config.
 *
 * The Vitest block runs the `web/console/src/lib/db/` Phase 72h test suite:
 * - `environment: 'jsdom'` provides `crypto.subtle` (WebCrypto) for `crypto.ts`.
 * - `setupFiles` installs the `fake-indexeddb` polyfill the IndexedDB driver
 *   tests need (jsdom does not ship IndexedDB).
 */
export default defineConfig({
  plugins: [sveltekit()],
  test: {
    environment: 'jsdom',
    include: ['src/**/*.{test,spec}.ts'],
    setupFiles: ['src/lib/db/tests/setup.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/lib/db/**/*.ts'],
      exclude: ['src/lib/db/tests/**'],
      thresholds: {
        lines: 85,
        functions: 85,
        branches: 80,
        statements: 85
      }
    }
  }
});
