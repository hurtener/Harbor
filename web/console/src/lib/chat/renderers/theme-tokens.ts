// Host-theme mapping for the MCP Apps renderer — Console `tokens.css` custom
// properties projected onto the ext-apps `McpUiStyleVariableKey` namespace, plus
// the light/dark preference resolved from the OS.
//
// # Why the host produces theme + styles
//
// A rendered `ui://` MCP App adapts to its host through two host-context fields
// the ext-apps App-side SDK consumes with zero per-app work:
//
//   - `theme` (`light` / `dark`) — the App applies it via `applyDocumentTheme`.
//   - `styles.variables` — the App applies them via `applyHostStyleVariables`,
//     so its own components inherit the host's structural design tokens.
//
// The consumption is inert until the HOST populates the values, so producing
// them is the whole job. Both concerns here are pure DOM reads — no bridge, no
// transport — so they are safe to call at bridge construction AND on a live
// theme change (the caller relays the change onto the live bridge separately).
//
// # Theme source
//
// The Console has no applied light/dark theme store yet, so the theme is
// sourced from the OS `prefers-color-scheme` media query. `tokens.css` resolves
// to one coherent palette today; reading the LIVE computed values (rather than
// hard-coding hex) keeps the mapping honest — whatever the token surface
// resolves to is exactly what a rendered app receives, and a future applied
// theme is picked up for free.

import type {
  McpUiHostStyles,
  McpUiStyleVariableKey,
  McpUiStyles,
  McpUiTheme,
} from '@modelcontextprotocol/ext-apps/app-bridge';

/** The media query whose match decides the host's light/dark preference. */
export const PREFERS_DARK_QUERY = '(prefers-color-scheme: dark)';

/**
 * The mapping from an ext-apps style-variable key onto the Console `tokens.css`
 * custom-property name that supplies its value. Keys are typed as the closed
 * `McpUiStyleVariableKey` union, so a key the ext-apps spec does not define
 * fails `svelte-check` — the §17.8 mechanical guard that keeps this table
 * pinned to the vendored schema rather than an implementer's interpretation.
 *
 * The Console token surface is dark-first and does not fill every ext-apps
 * slot; hosts may provide any subset, so this maps the structural tokens the
 * Console actually defines (surfaces, text, borders, status hues, type, radii)
 * and omits the rest.
 */
export const CONSOLE_STYLE_TOKEN_MAP: Partial<Record<McpUiStyleVariableKey, string>> = {
  // Surfaces
  '--color-background-primary': '--color-bg',
  '--color-background-secondary': '--color-surface',
  '--color-background-tertiary': '--color-surface-raised',
  '--color-background-info': '--color-accent-soft',
  '--color-background-danger': '--color-danger-soft',
  '--color-background-success': '--color-success-soft',
  '--color-background-warning': '--color-warning-soft',
  // Text
  '--color-text-primary': '--color-text',
  '--color-text-secondary': '--color-text-muted',
  '--color-text-tertiary': '--color-text-muted',
  '--color-text-info': '--color-accent',
  '--color-text-danger': '--color-danger',
  '--color-text-success': '--color-success',
  '--color-text-warning': '--color-warning',
  // Borders
  '--color-border-primary': '--color-border',
  '--color-border-info': '--color-accent',
  '--color-border-danger': '--color-danger',
  '--color-border-success': '--color-success',
  '--color-border-warning': '--color-warning',
  // Focus rings
  '--color-ring-primary': '--color-accent',
  '--color-ring-info': '--color-accent',
  '--color-ring-danger': '--color-danger',
  '--color-ring-success': '--color-success',
  '--color-ring-warning': '--color-warning',
  // Type
  '--font-sans': '--font-sans',
  '--font-mono': '--font-mono',
  '--font-text-xs-size': '--text-xs',
  '--font-text-sm-size': '--text-sm',
  '--font-text-md-size': '--text-base',
  '--font-text-lg-size': '--text-lg',
  '--font-heading-sm-size': '--text-xl',
  '--font-heading-md-size': '--text-2xl',
  '--font-heading-lg-size': '--text-3xl',
  // Radii
  '--border-radius-sm': '--radius-sm',
  '--border-radius-md': '--radius-md',
  '--border-radius-lg': '--radius-lg',
  '--border-radius-full': '--radius-pill',
};

/** The narrow view of `Window` the theme resolver needs — matchMedia only. */
type MatchMediaView = Pick<Window, 'matchMedia'>;

/**
 * Resolve the host's light/dark preference from the OS `prefers-color-scheme`.
 * Falls back to `dark` (the Console's palette) when `matchMedia` is
 * unavailable — server-side rendering or a jsdom test without a stub — so the
 * host never emits an undefined theme.
 */
export function resolveHostTheme(
  view: MatchMediaView | undefined = typeof window !== 'undefined' ? window : undefined,
): McpUiTheme {
  const matchMedia = view?.matchMedia;
  if (typeof matchMedia === 'function') {
    try {
      return matchMedia.call(view, PREFERS_DARK_QUERY).matches ? 'dark' : 'light';
    } catch {
      // A broken/partial matchMedia stub must not crash the renderer — fall
      // through to the palette default rather than throwing at construction.
    }
  }
  return 'dark';
}

/**
 * The `prefers-color-scheme` {@link MediaQueryList} the caller subscribes to for
 * a LIVE theme change (its `change` event), or `null` when `matchMedia` is
 * unavailable. Kept separate from {@link resolveHostTheme} so the subscription
 * lives entirely in the renderer's reactive layer — this module only reads.
 */
export function hostThemeMediaQuery(
  view: MatchMediaView | undefined = typeof window !== 'undefined' ? window : undefined,
): MediaQueryList | null {
  const matchMedia = view?.matchMedia;
  if (typeof matchMedia !== 'function') return null;
  try {
    return matchMedia.call(view, PREFERS_DARK_QUERY);
  } catch {
    return null;
  }
}

/** The narrow DOM surface the style mapping reads — the computed-style getter. */
type ComputedStyleView = { getComputedStyle: (el: Element) => CSSStyleDeclaration };

/**
 * Project the Console `tokens.css` custom-property VALUES onto the ext-apps
 * `McpUiStyleVariableKey` namespace by reading the live computed values off the
 * given root (the document element by default). A token that resolves to an
 * empty string is omitted, so the app receives only the variables the host
 * actually defines. Returns a `McpUiStyles` — a subset is valid per the spec.
 */
export function mapConsoleStyleVariables(
  root: Element | undefined = typeof document !== 'undefined' ? document.documentElement : undefined,
  view: ComputedStyleView | undefined = typeof window !== 'undefined' ? window : undefined,
): McpUiStyles {
  const out: Partial<Record<McpUiStyleVariableKey, string>> = {};
  if (!root || typeof view?.getComputedStyle !== 'function') return out as McpUiStyles;
  const computed = view.getComputedStyle(root);
  for (const [extKey, consoleVar] of Object.entries(CONSOLE_STYLE_TOKEN_MAP) as [
    McpUiStyleVariableKey,
    string,
  ][]) {
    const value = computed.getPropertyValue(consoleVar).trim();
    if (value !== '') out[extKey] = value;
  }
  return out as McpUiStyles;
}

/**
 * Build the ext-apps `styles` host-context half from the live Console token
 * surface — the value threaded into `ui/initialize` at construction and patched
 * onto the live bridge on a theme change.
 */
export function buildHostStyles(
  root?: Element,
  view?: ComputedStyleView,
): McpUiHostStyles {
  return { variables: mapConsoleStyleVariables(root, view) };
}
