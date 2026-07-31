#!/usr/bin/env node
/**
 * Protocol TypeScript lockstep guard (the TS half of `make protocol-ts-gen-check`).
 *
 * The Console's Protocol client is HAND-MAINTAINED TypeScript split across
 * per-page wire-type modules (`src/lib/protocol/*.ts`, `src/lib/sessions/types.ts`,
 * `src/lib/flows/types.ts`). This guard pins those hand-written interfaces against
 * the committed, Go-generated wire manifest
 * (`src/lib/protocol/wire-manifest.gen.json`, emitted by
 * `cmd/harbor-protocol-ts-lockstep` from `singlesource.CanonicalWireTypes`). It is
 * the field-level lockstep D-093 asked for, realised as VERIFICATION of the
 * hand-written client rather than GENERATION of it (D-223 — full generation of the
 * per-domain type modules is a deferred future phase).
 *
 * It fails (exit 1, printing every violation) when:
 *
 *   (a) [PRESENCE — mandatory] a canonical wire type in the manifest has neither a
 *       matching exported `interface`/`type` in the scanned TS surface NOR an entry
 *       in the untyped allowlist (`scripts/protocol-ts-untyped-allow.json`). This
 *       catches a NEW Go type the Console never typed, and a REMOVED/RENAMED type.
 *   (b) [FIELDS — mandatory] a typed wire type's TS interface is missing one of the
 *       manifest's JSON field keys. This catches a new/removed/renamed FIELD.
 *   (c) [ALLOWLIST HYGIENE] the allowlist names a type that is no longer in the
 *       manifest, or that DOES have a TS declaration (the exclusion is stale).
 *   (d) [TYPE TOKEN — best-effort] a typed field whose TS-declared type token
 *       disagrees with the manifest token (string/number/boolean/array/object).
 *       Best-effort: only flagged when the TS type is confidently parseable; an
 *       in-place field-type swap the parser cannot resolve is the documented
 *       residual this guard does NOT catch (it surfaces downstream at the
 *       `svelte-check` use site).
 *   (e) [BODY-IDENTITY FOLD — mandatory] a `request()` call site whose Protocol
 *       request type declares NO `identity` field but which does not pass
 *       `omitBodyIdentity: true` (the transport would fold an `identity` key the
 *       wire type does not define, and the Runtime decodes strictly — HTTP 400
 *       `unknown field "identity"`), or the converse: a call site that passes the
 *       flag for a type that DOES declare `identity` (the fold is load-bearing
 *       there, and suppressing it strips the scope the handler reconciles).
 *       See D-374. The route -> request-type join is read from the GENERATED
 *       `docs/site/protocol/methods.md` rather than re-derived here, so there is
 *       exactly one source for it (`cmd/harbor-gen-protocol-docs`), itself kept
 *       fresh by `make protocol-docs-gen-check` and its own Go lockstep tests.
 *
 * Usage:
 *   node scripts/check-protocol-ts-lockstep.mjs       # CLI gate (wired into `npm run lint`)
 *   import { runChecks } from './check-protocol-ts-lockstep.mjs'  # vitest test
 */
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CONSOLE_ROOT = resolve(HERE, '..');
const LIB_DIR = join(CONSOLE_ROOT, 'src', 'lib');
const MANIFEST_PATH = join(LIB_DIR, 'protocol', 'wire-manifest.gen.json');
const ALLOW_PATH = join(HERE, 'protocol-ts-untyped-allow.json');
// The GENERATED Protocol methods reference (repo root -> docs/site/protocol).
// It carries the route -> request-type join check (e) needs; re-deriving that
// join here would be a second source for it (CLAUDE.md §13).
const METHODS_MD_PATH = resolve(CONSOLE_ROOT, '..', '..', 'docs', 'site', 'protocol', 'methods.md');

/**
 * Recursively list `.ts` files under `dir`, skipping declaration-collision-free
 * test files (`*.spec.ts` / `*.test.ts`) and `.d.ts`.
 * @param {string} dir
 * @returns {string[]}
 */
function walkTs(dir) {
  /** @type {string[]} */
  const out = [];
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      out.push(...walkTs(full));
    } else if (
      name.endsWith('.ts') &&
      !name.endsWith('.d.ts') &&
      !/\.(spec|test)\.ts$/.test(name)
    ) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Strip line + block comments so JSDoc field-shaped text (`@param foo:`) never
 * registers as a declaration. String contents are left intact — TS wire types do
 * not embed `//` or `/*` in string literals.
 * @param {string} src
 * @returns {string}
 */
function stripComments(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/[^\n]*/g, '$1');
}

/**
 * A parsed TS type declaration. `kind: 'alias'` carries a non-object
 * `export type N = …` whose right-hand side is kept in `alias` so a bare
 * named reference (a string-enum, an array alias) resolves to a token.
 * @typedef {{ name: string, kind: 'interface'|'type'|'alias', extends: string[], body: string, alias: string, file: string }} TsDecl
 */

/**
 * Find the substring from `open` (index of `{`) to its matching `}`.
 * @param {string} src
 * @param {number} open
 * @returns {string}
 */
function matchBraces(src, open) {
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') {
      depth--;
      if (depth === 0) return src.slice(open + 1, i);
    }
  }
  return src.slice(open + 1);
}

/**
 * Parse every exported object-shaped type declaration in `src`.
 * Handles `export interface N [extends A, B] { … }` and
 * `export type N = { … }`. Non-object `export type N = 'a'|'b'` aliases are
 * skipped (the manifest carries struct wire types only).
 * @param {string} src
 * @param {string} file
 * @returns {TsDecl[]}
 */
function parseDecls(src, file) {
  const clean = stripComments(src);
  /** @type {TsDecl[]} */
  const decls = [];

  const ifaceRe = /export\s+interface\s+([A-Za-z0-9_]+)\s*(?:extends\s+([^{]+))?\{/g;
  for (let m; (m = ifaceRe.exec(clean)); ) {
    const ext = (m[2] ?? '')
      .split(',')
      .map((s) => s.trim().replace(/<.*$/, ''))
      .filter(Boolean);
    decls.push({
      name: m[1],
      kind: 'interface',
      extends: ext,
      body: matchBraces(clean, ifaceRe.lastIndex - 1),
      alias: '',
      file,
    });
  }

  // `export type N = …` — object literal (`{ … }`) or a non-object alias
  // (string-enum union, array alias). The alias text is kept so a named
  // reference to N resolves to a token.
  const typeRe = /export\s+type\s+([A-Za-z0-9_]+)\s*=\s*([\s\S]*?);/g;
  for (let m; (m = typeRe.exec(clean)); ) {
    const rhs = m[2].trim();
    if (rhs.startsWith('{')) {
      // Re-find the `{` to brace-match (the `;` regex stopped at the first
      // semicolon, which for an object literal is an inner one).
      const open = clean.indexOf('{', m.index);
      decls.push({
        name: m[1],
        kind: 'type',
        extends: [],
        body: matchBraces(clean, open),
        alias: '',
        file,
      });
    } else {
      decls.push({ name: m[1], kind: 'alias', extends: [], body: '', alias: rhs, file });
    }
  }

  return decls;
}

/**
 * Extract top-level field declarations from an interface/object-type body.
 * Returns a map of field key → the raw TS type text (trimmed, semicolon-free)
 * for the best-effort token comparison. Only depth-1 members are collected;
 * nested object members and index signatures (`[k: string]: …`) are ignored.
 * @param {string} body
 * @returns {Map<string,string>}
 */
function parseFields(body) {
  /** @type {Map<string,string>} */
  const fields = new Map();
  let depth = 0;
  let i = 0;
  const n = body.length;
  while (i < n) {
    const c = body[i];
    if (c === '{' || c === '(' || c === '<' || c === '[') {
      depth++;
      i++;
      continue;
    }
    if (c === '}' || c === ')' || c === '>' || c === ']') {
      depth--;
      i++;
      continue;
    }
    if (depth === 0) {
      // A member starts at the current position: optional `readonly`, then a
      // key (`name` or `'name'`), then `?:` / `:`.
      const rest = body.slice(i);
      const mm = rest.match(/^\s*(?:readonly\s+)?(?:'([A-Za-z0-9_]+)'|([A-Za-z0-9_]+))\s*(\??)\s*:/);
      if (mm) {
        const key = mm[1] ?? mm[2];
        // Capture the type text up to the member terminator at depth 0.
        const typeStart = i + mm[0].length;
        let j = typeStart;
        let d = 0;
        while (j < n) {
          const cj = body[j];
          if ('{(<['.includes(cj)) d++;
          else if ('})>]'.includes(cj)) {
            if (d === 0) break;
            d--;
          } else if ((cj === ';' || cj === ',' || cj === '\n') && d === 0) break;
          j++;
        }
        fields.set(key, body.slice(typeStart, j).trim());
        i = j;
        continue;
      }
    }
    i++;
  }
  return fields;
}

/**
 * Reduce a raw TS type text to a manifest token when confidently parseable,
 * else '' (unknown — skip the best-effort comparison). `aliases` resolves a
 * bare named reference (a string-enum union, an array alias, an object type)
 * to its token, so a TS field typed `AgentStatus` (a `'a'|'b'` union) is
 * recognised as the wire-level `string` it marshals to.
 * @param {string} ts
 * @param {Map<string,string>} aliases
 * @param {number} [depth]
 * @returns {string}
 */
function tsToken(ts, aliases, depth = 0) {
  let t = ts.trim().replace(/\|\s*null$/, '').replace(/\|\s*undefined$/, '').trim();
  if (/\[\]$/.test(t) || /^Array</.test(t) || /^readonly\s/.test(t)) return 'array';
  if (/^Record</.test(t) || /^\{/.test(t) || /^Map</.test(t)) return 'object';
  if (t === 'string') return 'string';
  if (t === 'number') return 'number';
  if (t === 'boolean') return 'boolean';
  if (t === 'unknown' || t === 'any') return 'any';
  // A bare string-literal union (`'a' | 'b'`) is a string enum.
  if (/^'.*'(\s*\|\s*'.*')*$/.test(t)) return 'string';
  // A single named type — resolve it through the alias table (a string-enum
  // alias → string; an object decl → object); fall back to "object" for an
  // unresolved PascalCase reference (a nested wire type).
  if (/^[A-Za-z][A-Za-z0-9_]*$/.test(t)) {
    if (depth < 4 && aliases.has(t)) return tsToken(aliases.get(t) ?? '', aliases, depth + 1);
    if (/^[A-Z]/.test(t)) return 'object';
  }
  return '';
}

/**
 * Run every lockstep check. Returns a flat list of human-readable violation
 * strings; an empty list means the TS client is in lockstep with the manifest.
 * @returns {string[]}
 */
export function runChecks() {
  /** @type {string[]} */
  const violations = [];

  if (!existsSync(MANIFEST_PATH)) {
    return [`[manifest] ${relative(CONSOLE_ROOT, MANIFEST_PATH)} is missing — run 'make protocol-ts-gen'`];
  }
  const manifest = JSON.parse(readFileSync(MANIFEST_PATH, 'utf8'));
  const allow = JSON.parse(readFileSync(ALLOW_PATH, 'utf8'));
  /** @type {Record<string,string>} */
  const untyped = allow.types ?? {};

  // ---- index every exported TS declaration ---------------------------------
  // A wire type may be declared in more than one page module (each page keeps
  // its own typed view); collect ALL object-shaped declarations per name and
  // union their fields, and resolve named references through an alias table.
  /** @type {Map<string, TsDecl[]>} */
  const objDecls = new Map();
  /** @type {Map<string, string>} */
  const aliases = new Map();
  for (const file of walkTs(LIB_DIR)) {
    const rel = relative(CONSOLE_ROOT, file);
    for (const d of parseDecls(readFileSync(file, 'utf8'), rel)) {
      if (d.kind === 'alias') {
        if (!aliases.has(d.name)) aliases.set(d.name, d.alias);
        continue;
      }
      const list = objDecls.get(d.name) ?? [];
      list.push(d);
      objDecls.set(d.name, list);
    }
  }

  /**
   * Resolve the unioned top-level field set across every declaration of
   * `name`, folding `extends` parents that are themselves declared.
   * @param {string} name
   * @param {Set<string>} seen
   * @returns {Map<string,string>}
   */
  function resolveFields(name, seen = new Set()) {
    if (seen.has(name)) return new Map();
    seen.add(name);
    /** @type {Map<string,string>} */
    const fields = new Map();
    for (const d of objDecls.get(name) ?? []) {
      for (const [k, v] of parseFields(d.body)) if (!fields.has(k)) fields.set(k, v);
      for (const parent of d.extends) {
        for (const [k, v] of resolveFields(parent, seen)) if (!fields.has(k)) fields.set(k, v);
      }
    }
    return fields;
  }

  // ---- (a) presence + (b) fields + (d) token -------------------------------
  for (const [typeName, shape] of Object.entries(manifest.types).sort()) {
    const declList = objDecls.get(typeName);
    if (!declList || declList.length === 0) {
      if (!(typeName in untyped)) {
        violations.push(
          `[presence] canonical wire type "${typeName}" has no exported TS interface/type ` +
            `and is not in the untyped allowlist (scripts/protocol-ts-untyped-allow.json) — ` +
            `add the interface or justify the exclusion`,
        );
      }
      continue;
    }
    const where = declList.map((d) => d.file).join(', ');
    if (typeName in untyped) {
      violations.push(
        `[allowlist] "${typeName}" is in the untyped allowlist but DOES have a TS declaration ` +
          `(${where}) — remove the stale allowlist entry`,
      );
    }
    const tsFields = resolveFields(typeName);
    for (const f of shape.fields) {
      if (!tsFields.has(f.key)) {
        // The `identity` scope every `*Request` carries is folded into the
        // request body by the shared HarborClient transport — the page-level
        // request interfaces deliberately omit it (documented in every
        // request module). That single transport-injected key is the only
        // sanctioned per-field omission.
        if (f.key === 'identity' && /Request$/.test(typeName)) continue;
        violations.push(
          `[field] ${typeName} (${where}): manifest field "${f.key}" (${f.type}) is not declared in TS`,
        );
        continue;
      }
      // (d) best-effort token comparison.
      const raw = tsFields.get(f.key) ?? '';
      const got = tsToken(raw, aliases);
      if (got && got !== 'any' && f.type !== 'any' && got !== f.type) {
        // `object` covers both nested wire types and Record<>; a manifest
        // `array` of refs vs a TS tuple etc. is intentionally lenient.
        violations.push(
          `[token] ${typeName} (${where}): field "${f.key}" is TS "${raw}" ` +
            `(token ${got}) but the manifest token is ${f.type}`,
        );
      }
    }
  }

  // ---- (c) allowlist hygiene: no stale entries -----------------------------
  for (const typeName of Object.keys(untyped).sort()) {
    if (!(typeName in manifest.types)) {
      violations.push(
        `[allowlist] "${typeName}" is in the untyped allowlist but is not a canonical wire type ` +
          `in the manifest — remove the stale entry`,
      );
    }
  }

  // ---- (e) body-identity fold vs. the request type's `identity` field ------
  violations.push(...checkBodyIdentityFold(manifest));

  return violations;
}

/**
 * Parse the GENERATED Protocol methods reference into `route -> {method,
 * request}`. The table shape is fixed by `cmd/harbor-gen-protocol-docs`; a
 * parse that yields nothing is treated as a hard failure by the caller rather
 * than as "no violations", because a guard that silently matches zero rows is
 * indistinguishable from a passing one.
 * @param {string} src
 * @returns {Map<string, {method: string, request: string}>}
 */
function parseMethodsTable(src) {
  /** @type {Map<string, {method: string, request: string}>} */
  const out = new Map();
  const row =
    /^\|\s*`([^`]+)`\s*\|\s*`(?:GET|POST)\s+([^`]+)`\s*\|[^|]*\|\s*\[`([^`]+)`\][^|]*\|/gm;
  let m;
  while ((m = row.exec(src)) !== null) {
    out.set(m[2].trim(), { method: m[1], request: m[3] });
  }
  return out;
}

/**
 * Find every `transport.request(...)` call site under `src/lib`, returning the
 * requested path, whether the call passes `omitBodyIdentity`, and its location.
 *
 * The path regex is MULTILINE-aware deliberately: the formatter routinely wraps
 * the path onto its own line, and a line-oriented scan silently missed a third
 * of the call sites (and the one pre-existing `omitBodyIdentity` case) when this
 * sweep was first written by hand.
 * @returns {{file: string, line: number, path: string, omit: boolean}[]}
 */
function findRequestCallSites() {
  /** @type {{file: string, line: number, path: string, omit: boolean}[]} */
  const sites = [];
  const call = /\.request\s*(?:<[^>()]*>)?\s*\(\s*['"]([^'"]+)['"]/g;
  for (const file of walkTs(LIB_DIR)) {
    const src = readFileSync(file, 'utf8');
    const rel = relative(CONSOLE_ROOT, file);
    let m;
    call.lastIndex = 0;
    while ((m = call.exec(src)) !== null) {
      // Span the whole call expression so a trailing opts object on any
      // following line is seen.
      const open = src.indexOf('(', m.index);
      let depth = 0;
      let j = open;
      for (; j < src.length; j++) {
        if (src[j] === '(') depth++;
        else if (src[j] === ')') {
          depth--;
          if (depth === 0) break;
        }
      }
      sites.push({
        file: rel,
        line: src.slice(0, m.index).split('\n').length,
        path: m[1],
        omit: src.slice(open, j + 1).includes('omitBodyIdentity'),
      });
    }
  }
  return sites;
}

/**
 * Check (e). Every Console call site must fold the body identity if and only if
 * its Protocol request type declares an `identity` field.
 * @param {{types: Record<string, {fields?: {key: string}[]}>}} manifest
 * @returns {string[]}
 */
export function checkBodyIdentityFold(manifest) {
  /** @type {string[]} */
  const violations = [];

  if (!existsSync(METHODS_MD_PATH)) {
    return [
      `[body-identity] the generated methods reference is missing at ` +
        `${METHODS_MD_PATH} — run 'make protocol-docs-gen'. This check cannot run without ` +
        `the route -> request-type join, and skipping it silently is how the ` +
        `artifacts fold regressed for four phases.`,
    ];
  }
  const routes = parseMethodsTable(readFileSync(METHODS_MD_PATH, 'utf8'));
  if (routes.size === 0) {
    return [
      `[body-identity] parsed ZERO rows from the generated methods reference — the table shape ` +
        `changed and this check has gone inert. Fix the parser in ` +
        `scripts/check-protocol-ts-lockstep.mjs rather than leaving it matching nothing.`,
    ];
  }

  for (const site of findRequestCallSites()) {
    const entry = routes.get(site.path);
    if (entry === undefined) {
      violations.push(
        `[body-identity] ${site.file}:${site.line} calls "${site.path}", which is not a route in ` +
          `the generated methods reference — either the path is wrong or the method is not canonical`,
      );
      continue;
    }
    const type = manifest.types[entry.request];
    if (type === undefined) {
      violations.push(
        `[body-identity] ${site.file}:${site.line} calls "${entry.method}", whose request type ` +
          `"${entry.request}" is not in the wire manifest — run 'make protocol-ts-gen'`,
      );
      continue;
    }
    const hasIdentity = (type.fields ?? []).some((f) => f.key === 'identity');
    if (!hasIdentity && !site.omit) {
      violations.push(
        `[body-identity] ${site.file}:${site.line} calls "${entry.method}" without ` +
          `\`omitBodyIdentity: true\`, but ${entry.request} declares no \`identity\` field — the ` +
          `transport's fold would add a member the wire type does not define, and the Runtime ` +
          `decodes strictly, so this is an HTTP 400 \`unknown field "identity"\` at runtime (D-374)`,
      );
    } else if (hasIdentity && site.omit) {
      violations.push(
        `[body-identity] ${site.file}:${site.line} calls "${entry.method}" with ` +
          `\`omitBodyIdentity: true\`, but ${entry.request} DOES declare an \`identity\` field — ` +
          `suppressing the fold strips the scope the handler reconciles; drop the flag`,
      );
    }
  }
  return violations;
}

// CLI entry: run, print, set exit code.
if (resolve(process.argv[1] ?? '') === resolve(fileURLToPath(import.meta.url))) {
  const violations = runChecks();
  if (violations.length > 0) {
    console.error('protocol-ts lockstep guard: FAIL\n');
    for (const v of violations) console.error('  ✗ ' + v);
    console.error(
      `\n${violations.length} violation(s). The hand-maintained Console Protocol client must ` +
        `stay in lockstep with the Go wire manifest (run 'make protocol-ts-gen' after a Go change).`,
    );
    process.exit(1);
  }
  console.log('protocol-ts lockstep guard: OK (wire manifest ↔ TS client in lockstep)');
}
