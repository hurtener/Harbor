#!/usr/bin/env node
/**
 * Chat-module encapsulation guard.
 *
 * The chat module (`web/console/src/lib/chat/`) is a self-contained component
 * library: per D-091 it must render WITHOUT the Console's app shell or global
 * stylesheet, so a second framework surface (the packed dev UI named by D-091 /
 * brief 12) can mount it supplying only the design-token contract. This guard
 * makes that boundary MECHANICALLY ENFORCED so it cannot silently regress.
 *
 * It fails (exit 1, printing every violation) when the chat module:
 *
 *   (a) imports a non-chat Console internal — any `$lib/…` outside `$lib/chat`,
 *       any `$app/…`, or a relative specifier that resolves OUTSIDE the chat
 *       directory. (The chat module may only import from within itself + true
 *       third-party packages.)
 *   (b) references a `var(--…)` design token that is NOT declared in the token
 *       contract (`src/lib/chat/tokens.contract.json`) — an undeclared ambient
 *       dependency a second surface would not know to supply.
 *   (c) declares a token in the contract that no longer resolves in the single
 *       token surface (`src/lib/tokens.css`) — a broken contract.
 *   (d) [backstop to stylelint] contains a raw hex / rgb()/rgba()/hsl() colour
 *       literal in a `.svelte <style>` block or `.css` file. `npm run lint`'s
 *       stylelint pass is the primary enforcement; this catches the same shape
 *       so the guard is self-contained and provable in isolation.
 *   (e) declares a contract token whose VALUE (in tokens.css) references another
 *       `var(--…)` not in the contract — a transitive dependency a second
 *       surface supplying only the contract would not get (resolved one level).
 *
 * Usage:
 *   node scripts/check-chat-encapsulation.mjs        # CLI gate (wired into `npm run lint`)
 *   import { runChecks } from './check-chat-encapsulation.mjs'  # vitest test
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CONSOLE_ROOT = resolve(HERE, '..');
const CHAT_DIR = join(CONSOLE_ROOT, 'src', 'lib', 'chat');
const CONTRACT_PATH = join(CHAT_DIR, 'tokens.contract.json');
const TOKENS_CSS = join(CONSOLE_ROOT, 'src', 'lib', 'tokens.css');

/**
 * Recursively list files under `dir` matching one of `exts`. Test files
 * (`*.spec.*` / `*.test.*`) are EXEMPT — the boundary governs the production
 * module surface, not its tests (mirrors the `_test.go` carve-outs in the Go
 * rules). A spec legitimately imports the guard itself + mocks third-party
 * packages; scanning it would self-trip.
 *
 * @param {string} dir
 * @param {string[]} exts
 * @returns {string[]}
 */
function walk(dir, exts) {
  /** @type {string[]} */
  const out = [];
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      out.push(...walk(full, exts));
    } else if (exts.some((e) => name.endsWith(e)) && !/\.(spec|test)\.[cm]?[jt]s$/.test(name)) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Extract every import specifier (static, type, and dynamic) from source.
 *
 * @param {string} src
 * @returns {string[]}
 */
function importSpecifiers(src) {
  /** @type {string[]} */
  const specs = [];
  // `import … from 'x'` / `export … from 'x'` (covers `import type`).
  for (const m of src.matchAll(/\b(?:import|export)\b[^;]*?\bfrom\s*['"]([^'"]+)['"]/g)) {
    specs.push(m[1]);
  }
  // Bare side-effect import: `import 'x'`.
  for (const m of src.matchAll(/\bimport\s*['"]([^'"]+)['"]/g)) {
    specs.push(m[1]);
  }
  // Dynamic `import('x')` — accept single/double quotes AND backticks, so a
  // template-literal specifier `import(`$lib/x`)` cannot dodge the boundary.
  for (const m of src.matchAll(/\bimport\s*\(\s*['"`]([^'"`]+)['"`]\s*\)/g)) {
    specs.push(m[1]);
  }
  return specs;
}

/**
 * Pull the concatenated contents of every `<style>` block out of a .svelte file.
 *
 * @param {string} src
 * @returns {string}
 */
function styleBlocks(src) {
  return [...src.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map((m) => m[1]).join('\n');
}

/**
 * Run every encapsulation check. Returns a flat list of human-readable
 * violation strings; an empty list means the boundary holds.
 */
export function runChecks() {
  const violations = [];
  const codeFiles = walk(CHAT_DIR, ['.ts', '.js', '.svelte']);

  // ---- (a) import boundary -------------------------------------------------
  for (const file of codeFiles) {
    const rel = relative(CONSOLE_ROOT, file);
    const src = readFileSync(file, 'utf8');
    for (const spec of importSpecifiers(src)) {
      if (spec.startsWith('$app/') || spec === '$app') {
        violations.push(`[import] ${rel} imports SvelteKit app internal "${spec}"`);
        continue;
      }
      if (spec === '$lib' || (spec.startsWith('$lib/') && !spec.startsWith('$lib/chat'))) {
        violations.push(`[import] ${rel} imports non-chat Console internal "${spec}"`);
        continue;
      }
      if (spec.startsWith('.')) {
        const resolved = resolve(dirname(file), spec);
        if (resolved !== CHAT_DIR && !resolved.startsWith(CHAT_DIR + '/')) {
          violations.push(
            `[import] ${rel} imports "${spec}" which resolves OUTSIDE the chat module`,
          );
        }
      }
    }
  }

  // ---- load the contract + the token surface -------------------------------
  const contract = JSON.parse(readFileSync(CONTRACT_PATH, 'utf8'));
  const declared = new Set(contract.tokens);
  const tokensCss = readFileSync(TOKENS_CSS, 'utf8');
  const defined = new Set(
    [...tokensCss.matchAll(/^\s*(--[a-z0-9-]+)\s*:/gim)].map((m) => m[1]),
  );

  // ---- (b) every used token must be declared in the contract ---------------
  const usedUndeclared = new Set();
  for (const file of codeFiles.concat(walk(CHAT_DIR, ['.css']))) {
    const src = readFileSync(file, 'utf8');
    for (const m of src.matchAll(/var\(\s*(--[a-z0-9-]+)/g)) {
      if (!declared.has(m[1])) usedUndeclared.add(`${m[1]}  (in ${relative(CONSOLE_ROOT, file)})`);
    }
  }
  for (const t of [...usedUndeclared].sort()) {
    violations.push(`[token] chat module uses token not in tokens.contract.json: ${t}`);
  }

  // ---- (c) every declared token must resolve in tokens.css -----------------
  for (const t of [...declared].sort()) {
    if (!defined.has(t)) {
      violations.push(`[token] contract declares "${t}" but it is not defined in src/lib/tokens.css`);
    }
  }

  // ---- (e) transitive deps: a contract token whose VALUE references another
  // `var(--…)` drags that token into the handoff surface. If a contract
  // token's definition depends on a token NOT in the contract, a second
  // surface supplying only the contract gets an undefined inner var and the
  // declaration breaks (e.g. `--border-hairline: 1px solid var(--color-border)`
  // is useless without `--color-border`). Resolve one level so this class of
  // silent gap can't recur. ----------------------------------------------------
  const tokenDefs = new Map(
    [...tokensCss.matchAll(/^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);/gim)].map((m) => [m[1], m[2]]),
  );
  const transitiveMissing = new Set();
  for (const t of declared) {
    const def = tokenDefs.get(t);
    if (!def) continue;
    for (const m of def.matchAll(/var\(\s*(--[a-z0-9-]+)/g)) {
      if (!declared.has(m[1])) transitiveMissing.add(`${m[1]}  (required by ${t})`);
    }
  }
  for (const t of [...transitiveMissing].sort()) {
    violations.push(`[token] contract token transitively depends on a token not in the contract: ${t}`);
  }

  // ---- (d) raw colour literals (backstop to stylelint) ---------------------
  const RAW_COLOUR = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/;
  for (const file of walk(CHAT_DIR, ['.svelte', '.css'])) {
    const rel = relative(CONSOLE_ROOT, file);
    const src = readFileSync(file, 'utf8');
    const css = file.endsWith('.svelte') ? styleBlocks(src) : src;
    for (const line of css.split('\n')) {
      if (RAW_COLOUR.test(line) && !line.trimStart().startsWith('/*') && !line.includes('* ')) {
        violations.push(`[literal] ${rel}: raw colour literal in CSS — "${line.trim()}"`);
      }
    }
  }

  return violations;
}

// CLI entry: run, print, set exit code.
if (resolve(process.argv[1] ?? '') === resolve(fileURLToPath(import.meta.url))) {
  const violations = runChecks();
  if (violations.length > 0) {
    console.error('chat-module encapsulation guard: FAIL\n');
    for (const v of violations) console.error('  ✗ ' + v);
    console.error(`\n${violations.length} violation(s). The chat module (D-091) must stay self-contained.`);
    process.exit(1);
  }
  console.log('chat-module encapsulation guard: OK (boundary + token contract hold)');
}
