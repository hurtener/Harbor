// Chat-module encapsulation + portability tests (D-091 hardening).
//
// Two guarantees, both runnable in `npm run test` so a regression fails CI
// even if a developer skips `npm run lint`:
//
//   1. Encapsulation guard — the chat module imports no non-chat Console
//      internal and references only tokens declared in its contract (which all
//      resolve in tokens.css). This re-runs the SAME scanner the lint gate
//      wires in, so the two gates can never disagree.
//   2. Portability — the chat panel self-applies its typeface at its root, so
//      it renders correctly WITHOUT the Console global stylesheet (the
//      `html, body { font-family }` rule a second framework surface would not
//      ship). jsdom does not compute the full CSS cascade, so this is a
//      structural assertion that the root rule EXISTS — documented limitation.

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// The lint gate's scanner, imported directly so test + lint share one source.
import { runChecks } from '../../../../scripts/check-chat-encapsulation.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CHAT_DIR = join(HERE, '..');
const TOKENS_CSS = join(HERE, '..', '..', 'tokens.css');

describe('chat-module encapsulation guard', () => {
  it('reports zero boundary / token-contract violations', () => {
    const violations = runChecks();
    expect(violations, violations.join('\n')).toEqual([]);
  });
});

describe('chat-module portability', () => {
  it('the chat panel self-applies font-family AND color at its root (no Console global needed)', () => {
    const src = readFileSync(join(CHAT_DIR, 'ChatPanel.svelte'), 'utf8');
    // Isolate the `.chat-panel` rule block and assert it declares the sans
    // typeface AND the text colour from tokens. This is the litmus test that
    // the module is self-contained: rendered outside the Console shell it must
    // NOT fall back to the UA serif OR inherit text colour from an app-shell
    // rule it can't rely on. (jsdom can't compute the cascade — structural
    // assertion that the root rule carries both inherited properties.)
    const block = src.match(/\.chat-panel\s*\{([\s\S]*?)\}/);
    expect(block, '.chat-panel style rule must exist').not.toBeNull();
    expect(block![1]).toMatch(/font-family:\s*var\(--font-sans\)/);
    expect(block![1]).toMatch(/color:\s*var\(--color-text\)/);
  });

  it('every token the module depends on resolves in the single token surface', () => {
    const contract = JSON.parse(
      readFileSync(join(CHAT_DIR, 'tokens.contract.json'), 'utf8'),
    ) as { tokens: string[] };
    const tokensCss = readFileSync(TOKENS_CSS, 'utf8');
    const defined = new Set(
      [...tokensCss.matchAll(/^\s*(--[a-z0-9-]+)\s*:/gim)].map((m) => m[1]),
    );
    const unresolved = contract.tokens.filter((t) => !defined.has(t));
    expect(unresolved, `unresolved tokens: ${unresolved.join(', ')}`).toEqual([]);
    // --font-sans is the portability litmus token — it MUST be in the contract.
    expect(contract.tokens).toContain('--font-sans');
  });
});
