import js from '@eslint/js';
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
