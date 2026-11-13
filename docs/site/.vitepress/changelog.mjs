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
    .replace(/`([^`]*)`/g, "$1")
    .replace(/\*\*?([^*]+)\*\*?/g, "$1")
    .replace(/(^|[\s(])_([^_\s][^_]*)_(?=$|[\s).,;:])/g, "$1$2")
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

/** First sentence of plain text, trimmed at a word boundary. */
function firstSentence(text, max) {
  const s = plain(text).split(/(?<=[.!?])\s+(?=[A-Z0-9])/)[0] ?? "";
  if (s.length <= max) return s && !/[.!?]$/.test(s) ? `${s}.` : s;
  const cut = s.slice(0, max);
  return `${cut.slice(0, Math.max(cut.lastIndexOf(" "), 1)).replace(/[.:;,\s]+$/, "")}…`;
}

/** Released sections in file order: heading fields plus raw body lines. */
function releasedSections(lines) {
  const sections = [];
  for (let i = 0; i < lines.length; i++) {
    const m = RELEASED_HEADING.exec(lines[i]);
    if (!m) continue;
    let end = lines.findIndex((l, j) => j > i && l.startsWith("## "));
    if (end < 0) end = lines.length;
    sections.push({ m, body: lines.slice(i + 1, end) });
  }
  return sections;
}

/** Top-level bullets of a section, continuation lines joined. */
function bulletsOf(body) {
  const bullets = [];
  let open = false;
  for (const line of body) {
    if (line.startsWith("- ")) {
      bullets.push(line.slice(2));
      open = true;
    } else if (open && /^\s+\S/.test(line) && !/^\s+[-*] /.test(line)) {
      bullets[bullets.length - 1] += ` ${line.trim()}`;
    } else if (!/^\s/.test(line)) {
      open = false;
    }
  }
  return bullets;
}

/** Bold-lead items of a section: { title, body }. */
function itemsOf(body) {
  return bulletsOf(body)
    .map((b) => /^\*\*(.+?)\*\*\s*(.*)$/.exec(b))
    .filter(Boolean)
    .map(([, lead, rest]) => ({ title: plain(lead), body: firstSentence(rest, ITEM_BODY_MAX) }))
    .filter((it) => it.title);
}

/** Longest What's-new item body, in characters, before trimming. */
export const ITEM_BODY_MAX = 200;

/** How many released versions the What's-new strip carries. */
export const RECENT_RELEASES = 3;

/**
 * Parse the newest released version out of changelog text.
 * @param {string} text
 * @returns {{ version: string, short: string, date: string, summary: string, eyebrow: string, announcement: string, recent: { version: string, date: string, summary: string, items: { title: string, body: string }[] }[] }}
 */
export function parseRelease(text) {
  const sections = releasedSections(text.split(/\r?\n/));
  if (sections.length === 0) {
    throw new Error("CHANGELOG.md: no released `## [X.Y.Z] — YYYY-MM-DD` heading found");
  }
  const recent = sections.slice(0, RECENT_RELEASES).map(({ m, body }) => {
    const items = itemsOf(body);
    const phrases = items.map((it, i) =>
      i === 0 || /^[A-Z][A-Z0-9]/.test(it.title) ? it.title : it.title[0].toLowerCase() + it.title.slice(1),
    );
    return { version: `${m[1]}.${m[2]}.${m[3]}`, date: m[4], summary: joinSummary(phrases, SUMMARY_MAX), items };
  });
  const [, major, minor, patch, date] = sections[0].m;
  const version = `${major}.${minor}.${patch}`;
  const { summary } = recent[0];
  return {
    version,
    short: `${major}.${minor}`,
    date,
    summary,
    eyebrow: `Go-native agent runtime · v${version}`,
    announcement: `v${version} just shipped — ${summary || "see what changed"}`,
    recent,
  };
}

/** Read and parse the repo CHANGELOG.md; throws on any failure. */
export function loadRelease(path = CHANGELOG_PATH) {
  return parseRelease(readFileSync(path, "utf8"));
}
