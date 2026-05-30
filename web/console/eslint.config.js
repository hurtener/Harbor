import js from '@eslint/js';
import globals from 'globals';
import ts from 'typescript-eslint';
import svelte from 'eslint-plugin-svelte';

/**
 * Harbor Console ESLint flat config. Lints TypeScript + Svelte 5 sources.
 * `npm run lint` runs this alongside Stylelint + svelte-check.
 */
export default ts.config(
  js.configs.recommended,
  ...ts.configs.recommended,
  ...svelte.configs['flat/recommended'],
  {
    languageOptions: {
      // The Console is a browser SPA (D-091) — browser globals (fetch,
      // document, URL, Blob, navigator, btoa, …) are ambient.
      globals: { ...globals.browser },
      parserOptions: {
        extraFileExtensions: ['.svelte']
      }
    }
  },
  {
    files: ['**/*.svelte'],
    languageOptions: {
      parserOptions: {
        parser: ts.parser
      }
    }
  },
  {
    rules: {
      // Allow underscore-prefixed unused variables (Svelte 5 props that
      // are destructured but not yet wired in the component).
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }
      ]
    }
  },
  {
    ignores: [
      'build/',
      '.svelte-kit/',
      'node_modules/',
      'src/lib/protocol.ts',
      '*.cjs',
      'eslint.config.js',
      'svelte.config.js',
      'vite.config.ts'
    ]
  }
);
