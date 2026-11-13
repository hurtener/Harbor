// Build-time release metadata for the landing page, derived from the
// repo-root CHANGELOG.md so a release never needs a manual docs edit.
// config.ts injects the result as the `__HARBOR_RELEASE__` define
// constant. A changelog that cannot be parsed throws: the docs build
// fails loudly instead of publishing a stale version.
//
// Plain ESM (JSDoc-typed) so `node --test` can exercise it without a
// TypeScript toolchain.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

export const CHANGELOG_PATH = fileURLToPath(new URL("../../../CHANGELOG.md", import.meta.url));

/** Longest announcement summary, in characters, before trimming. */
export const SUMMARY_MAX = 140;

const RELEASED_HEADING = /^## \[(\d+)\.(\d+)\.(\d+)\]\s+—\s+(\d{4}-\d{2}-\d{2})\s*$/;

/** Strip inline markdown and decision references from a lead phrase. */
function plain(text) {
  return text
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[`*_]/g, "")
    .replace(/\s*\((?:D-\d+(?:[,;/ ]+)?)+\)/g, "")
    .replace(/\s+/g, " ")
    .replace(/[.:;,\s]+$/, "")
    .trim();
}

/** Join lead phrases, trimmed at a phrase (or word) boundary. */
function joinSummary(phrases, max) {
  let out = "";
  for (const p of phrases) {
    const next = out ? `${out}; ${p}` : p;
    if (next.length > max) {
      if (out) return out;
      const cut = p.slice(0, max);
      return `${cut.slice(0, Math.max(cut.lastIndexOf(" "), 1)).replace(/[.:;,\s]+$/, "")}…`;
    }
    out = next;
  }
  return out;
}

/**
 * Parse the newest released version out of changelog text.
 * @param {string} text
 * @returns {{ version: string, short: string, date: string, summary: string, eyebrow: string, announcement: string }}
 */
export function parseRelease(text) {
  const lines = text.split(/\r?\n/);
  const start = lines.findIndex((l) => RELEASED_HEADING.test(l));
  if (start < 0) {
    throw new Error("CHANGELOG.md: no released `## [X.Y.Z] — YYYY-MM-DD` heading found");
  }
  const [, major, minor, patch, date] = RELEASED_HEADING.exec(lines[start]);
  const version = `${major}.${minor}.${patch}`;

  let end = lines.findIndex((l, i) => i > start && l.startsWith("## "));
  if (end < 0) end = lines.length;

  // Collect top-level bullets with their continuation lines joined.
  const bullets = [];
  for (const line of lines.slice(start + 1, end)) {
    if (line.startsWith("- ")) bullets.push(line.slice(2));
    else if (bullets.length && /^\s+\S/.test(line) && !/^\s+[-*] /.test(line)) {
      bullets[bullets.length - 1] += ` ${line.trim()}`;
    } else if (!/^\s/.test(line)) bullets.push(null); // break: heading or prose
  }
  const phrases = bullets
    .filter((b) => b && b.startsWith("**"))
    .map((b) => /^\*\*(.+?)\*\*/.exec(b)?.[1])
    .filter(Boolean)
    .map(plain)
    .filter(Boolean)
    .map((p, i) => (i === 0 || /^[A-Z][A-Z0-9]/.test(p) ? p : p[0].toLowerCase() + p.slice(1)));

  const summary = joinSummary(phrases, SUMMARY_MAX);
  return {
    version,
    short: `${major}.${minor}`,
    date,
    summary,
    eyebrow: `Go-native agent runtime · v${version}`,
    announcement: `v${version} just shipped — ${summary || "see what changed"}`,
  };
}

/** Read and parse the repo CHANGELOG.md; throws on any failure. */
export function loadRelease(path = CHANGELOG_PATH) {
  return parseRelease(readFileSync(path, "utf8"));
}
