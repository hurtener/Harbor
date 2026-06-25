/*
 * A tiny, dependency-free syntax highlighter for the landing page's curated
 * code snippets. VitePress highlights fenced markdown blocks with Shiki at
 * build time, but the bespoke home renders code from Vue components (the home
 * markdown can't host components — config.ts sets markdown.html:false). Rather
 * than pull Shiki into the client bundle for a handful of static snippets, this
 * tokenizes the controlled snippets we ship and wraps tokens in classed spans.
 *
 * Token classes map to CSS in CodeBlock.vue: .tok-com / .tok-str / .tok-kw /
 * .tok-num / .tok-fn / .tok-prompt.
 */

const GO_KEYWORDS = new Set([
  "import", "func", "return", "if", "err", "nil", "defer", "var", "const",
  "package", "range", "for", "go", "chan", "select", "type", "struct",
  "interface", "map", "string", "error", "bool", "int",
]);

function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function span(cls: string, text: string): string {
  return `<span class="${cls}">${esc(text)}</span>`;
}

// Master tokenizer: order matters — comments and strings win over words.
const TOKEN_RE =
  /(\/\/[^\n]*|#[^\n]*)|("(?:[^"\\]|\\.)*"|`[^`]*`|'(?:[^'\\]|\\.)*')|(\b\d[\d.]*\b)|([A-Za-z_][A-Za-z0-9_.]*)/g;

function highlightLine(line: string, lang: string): string {
  // Terminal prompts / status glyphs read better with their own colour.
  if (lang === "bash") {
    const m = line.match(/^(\s*\$\s)(.*)$/);
    if (m) return span("tok-prompt", m[1]) + highlightCode(m[2], lang);
    if (/^\s*(✓|→|✗)/.test(line)) return span("tok-com", line);
  }
  return highlightCode(line, lang);
}

function highlightCode(code: string, lang: string): string {
  let out = "";
  let last = 0;
  let m: RegExpExecArray | null;
  TOKEN_RE.lastIndex = 0;
  while ((m = TOKEN_RE.exec(code)) !== null) {
    if (m.index > last) out += esc(code.slice(last, m.index));
    const [full, comment, str, num, word] = m;
    if (comment !== undefined) {
      // In go a "#" is not a comment; only "//" is. The regex catches both,
      // so only treat "#" as a comment for shell/yaml.
      if (comment.startsWith("#") && lang === "go") out += esc(full);
      else out += span("tok-com", comment);
    } else if (str !== undefined) {
      out += span("tok-str", str);
    } else if (num !== undefined) {
      out += span("tok-num", num);
    } else if (word !== undefined) {
      if (lang === "go" && GO_KEYWORDS.has(word)) out += span("tok-kw", word);
      else if (lang === "go" && /^[A-Z]/.test(word)) out += span("tok-fn", word);
      else out += esc(word);
    }
    last = m.index + full.length;
  }
  if (last < code.length) out += esc(code.slice(last));
  return out;
}

/** Highlight a multi-line snippet into HTML (one <span class="ln"> per line). */
export function highlight(code: string, lang: string): string {
  return code
    .split("\n")
    .map((line) => `<span class="ln">${highlightLine(line, lang) || "&nbsp;"}</span>`)
    .join("\n");
}
