import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * Harbor Console SvelteKit config.
 *
 * - `compilerOptions.runes: true` — Svelte 5 runes mode is mandatory (D-092);
 *   legacy Svelte 4 reactivity is rejected by `svelte-check --fail-on-warnings`.
 * - `adapter-static` — the Console is a client-side SPA served by the
 *   `harbor console` subcommand (D-091); no SSR.
 *
 * @type {import('@sveltejs/kit').Config}
 */
const config = {
  preprocess: vitePreprocess(),
  compilerOptions: {
    runes: true
  },
  kit: {
    adapter: adapter({
      fallback: 'index.html'
    }),
    alias: {
      $lib: 'src/lib'
    }
  }
};

export default config;
