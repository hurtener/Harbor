// Unit tests for the Console → ext-apps host-theme mapping.
//
// Covers the light/dark resolution (OS `prefers-color-scheme`, with a fallback
// when `matchMedia` is unavailable) and the token projection (Console
// `tokens.css` custom-property values → the ext-apps `McpUiStyleVariableKey`
// namespace, reading live computed values). The KEY typing (a wrong ext-apps
// key fails svelte-check) is the §17.8 mechanical guard; these tests cover the
// runtime behaviour that typing cannot.

import { describe, expect, it } from 'vitest';
import type { McpUiStyleVariableKey } from '@modelcontextprotocol/ext-apps/app-bridge';

import {
  CONSOLE_STYLE_TOKEN_MAP,
  buildHostStyles,
  hostThemeMediaQuery,
  mapConsoleStyleVariables,
  resolveHostTheme,
} from './theme-tokens.js';

/** A minimal matchMedia stub whose match is fixed. */
function matchMediaView(matches: boolean): Pick<Window, 'matchMedia'> {
  return {
    matchMedia: (() =>
      ({
        matches,
        addEventListener() {},
        removeEventListener() {},
      }) as unknown as MediaQueryList) as Window['matchMedia'],
  };
}

describe('resolveHostTheme', () => {
  it('returns dark when prefers-color-scheme: dark matches', () => {
    expect(resolveHostTheme(matchMediaView(true))).toBe('dark');
  });

  it('returns light when the dark query does not match', () => {
    expect(resolveHostTheme(matchMediaView(false))).toBe('light');
  });

  it('falls back to dark (the Console palette) when matchMedia is unavailable', () => {
    expect(resolveHostTheme({} as Pick<Window, 'matchMedia'>)).toBe('dark');
    expect(resolveHostTheme(undefined)).toBe('dark');
  });

  it('falls back to dark when matchMedia throws (partial stub)', () => {
    const broken = {
      matchMedia: (() => {
        throw new Error('nope');
      }) as Window['matchMedia'],
    };
    expect(resolveHostTheme(broken)).toBe('dark');
  });
});

describe('hostThemeMediaQuery', () => {
  it('returns the MediaQueryList when matchMedia is present', () => {
    const mql = hostThemeMediaQuery(matchMediaView(true));
    expect(mql).not.toBeNull();
    expect(mql?.matches).toBe(true);
  });

  it('returns null when matchMedia is unavailable', () => {
    expect(hostThemeMediaQuery({} as Pick<Window, 'matchMedia'>)).toBeNull();
  });
});

describe('CONSOLE_STYLE_TOKEN_MAP', () => {
  it('every key is a valid ext-apps McpUiStyleVariableKey (closed union) and maps a --token', () => {
    for (const [extKey, consoleVar] of Object.entries(CONSOLE_STYLE_TOKEN_MAP)) {
      // The cast is only meaningful because the map is typed as
      // Partial<Record<McpUiStyleVariableKey, string>> — a wrong key would have
      // failed svelte-check before this test ran.
      const key: McpUiStyleVariableKey = extKey as McpUiStyleVariableKey;
      expect(key.startsWith('--')).toBe(true);
      expect(consoleVar.startsWith('--color-') || consoleVar.startsWith('--font') || consoleVar.startsWith('--text-') || consoleVar.startsWith('--radius-')).toBe(true);
    }
  });
});

describe('mapConsoleStyleVariables', () => {
  it('reads live computed values off the root and omits empty tokens', () => {
    const values: Record<string, string> = {
      '--color-bg': '#0d1117',
      '--color-text': '#e6edf3',
      '--color-border': '', // empty → omitted
    };
    const view = {
      getComputedStyle: () =>
        ({ getPropertyValue: (name: string) => values[name] ?? '' }) as unknown as CSSStyleDeclaration,
    };
    const styles = mapConsoleStyleVariables({} as Element, view);
    expect(styles['--color-background-primary']).toBe('#0d1117');
    expect(styles['--color-text-primary']).toBe('#e6edf3');
    // A token that resolves to '' is omitted, never emitted as an empty value.
    expect('--color-border-primary' in styles).toBe(false);
  });

  it('returns an empty object when getComputedStyle is unavailable', () => {
    expect(mapConsoleStyleVariables(undefined, undefined)).toEqual({});
  });
});

describe('buildHostStyles', () => {
  it('wraps the mapped variables under { variables }', () => {
    const view = {
      getComputedStyle: () =>
        ({ getPropertyValue: (name: string) => (name === '--color-bg' ? '#000' : '') }) as unknown as CSSStyleDeclaration,
    };
    const styles = buildHostStyles({} as Element, view);
    expect(styles.variables?.['--color-background-primary']).toBe('#000');
  });
});
