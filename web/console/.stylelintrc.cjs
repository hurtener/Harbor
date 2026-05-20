/**
 * Harbor Console Stylelint config (CLAUDE.md §4.5 rule 3).
 *
 * Design tokens live in `src/lib/tokens.css` as CSS custom properties.
 * Components reference tokens, never raw values. This config mechanically
 * rejects raw hex / rgb() / named colors and arbitrary px / rem / em
 * literals in `.svelte` files and any `.css` file other than `tokens.css`.
 */
module.exports = {
  rules: {
    'color-no-hex': true,
    'color-named': 'never',
    'declaration-property-value-disallowed-list': {
      '/^(color|background|background-color|border|border-color|fill|stroke|box-shadow)$/':
        [/rgb/, /rgba/, /hsl/, /hsla/, /#[0-9a-fA-F]/],
      '/.*/': [/\d+px/, /\d*\.?\d+rem/, /\d*\.?\d+em/]
    }
  },
  overrides: [
    {
      // `.svelte` <style> blocks need the HTML-aware syntax parser.
      files: ['**/*.svelte'],
      customSyntax: 'postcss-html'
    },
    {
      // tokens.css is the single place raw values are allowed — it DEFINES
      // the token surface every other file consumes.
      files: ['src/lib/tokens.css'],
      rules: {
        'color-no-hex': null,
        'color-named': null,
        'declaration-property-value-disallowed-list': null
      }
    }
  ]
};
