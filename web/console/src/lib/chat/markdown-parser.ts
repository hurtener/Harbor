// Harbor Console — safe-subset markdown parser (Phase 108 / D-167).
//
// Renders an in-house safe subset: headings (# / ## / ###); ordered /
// unordered lists (single-level nesting only); bold (** / __); italic
// (* / _); inline code (`); fenced code blocks (routed to CodeBlock);
// line breaks.
//
// Rejects ALL HTML — < is escaped verbatim. No autolinking, no tables,
// no KaTeX, no Mermaid. Those land with a future RFC-blessed dependency.
//
// The parser is intentionally small (~120 LOC plain TypeScript) so it is
// mechanically auditable: no eval, no innerHTML, no document.write, no
// shell-out. Svelte's auto-escaping is the final safety layer.

/** One rendered markdown node. */
export type MarkdownNode =
	| { type: 'heading'; level: number; text: string }
	| { type: 'paragraph'; children: InlineNode[] }
	| { type: 'list'; ordered: boolean; items: InlineNode[][] }
	| { type: 'code'; lang: string; text: string }
	| { type: 'break' };

/** One inline node inside a paragraph or list item. */
export type InlineNode =
	| { type: 'text'; text: string }
	| { type: 'bold'; text: string }
	| { type: 'italic'; text: string }
	| { type: 'code'; text: string };

const BOLD_RE = /\*\*((?:\\.|[^\\*]|\*(?!\*))+)\*\*|__((?:\\.|[^\\_]|_(?!_))+)__/g;
const ITALIC_RE = /\*((?:\\.|[^\\*])+)\*|_((?:\\.|[^\\_])+)_/g;
const CODE_RE = /`([^`]+)`/g;

function escapeHtml(text: string): string {
	return text
		.replace(/&/g, '&amp;')
		.replace(/</g, '&lt;')
		.replace(/>/g, '&gt;');
}

function unescapeMarkdown(text: string): string {
	return text.replace(/\\([*_`[\]\\])/g, '$1');
}

function parseInline(text: string): InlineNode[] {
	const nodes: InlineNode[] = [];
	const patterns: Array<{ re: RegExp; type: InlineNode['type'] }> = [
		{ re: BOLD_RE, type: 'bold' },
		{ re: ITALIC_RE, type: 'italic' },
		{ re: CODE_RE, type: 'code' }
	];

	// Flatten: find the earliest match among all patterns.
	type Match = { index: number; end: number; type: InlineNode['type']; text: string };

	let pos = 0;
	while (pos < text.length) {
		let best: Match | null = null;
		for (const { re, type } of patterns) {
			re.lastIndex = 0;
			const m = re.exec(text.slice(pos));
			if (m && m.index === 0) {
				const raw = m[1] ?? m[2] ?? '';
				const matchEnd = pos + m[0].length;
				if (!best || pos + m.index < best.index) {
					best = { index: pos, end: matchEnd, type, text: unescapeMarkdown(raw) };
				}
			}
		}

		if (best) {
			if (best.index > pos) {
				nodes.push({ type: 'text', text: unescapeMarkdown(text.slice(pos, best.index)) });
			}
			nodes.push({ type: best.type, text: best.text });
			pos = best.end;
		} else {
			// No more inline matches — consume rest as plain text.
			nodes.push({ type: 'text', text: unescapeMarkdown(text.slice(pos)) });
			break;
		}
	}

	return nodes;
}

function isHeading(line: string): { level: number; text: string } | null {
	const m = /^(#{1,6})\s+(.*)$/.exec(line);
	if (!m) return null;
	return { level: m[1].length, text: m[2].trim() };
}

function isListItem(line: string): { ordered: boolean; text: string } | null {
	// Unordered: - item, * item, + item
	const u = /^\s*[-*+]\s+(.*)$/.exec(line);
	if (u) return { ordered: false, text: u[1].trim() };
	// Ordered: 1. item
	const o = /^\s*(\d+)\.\s+(.*)$/.exec(line);
	if (o) return { ordered: true, text: o[2].trim() };
	return null;
}

function isFencedCodeStart(line: string): { lang: string } | null {
	const m = /^```(\S*)/.exec(line);
	if (!m) return null;
	return { lang: m[1].trim() };
}

/**
 * Parse a markdown source string into a tree of safe-subset nodes.
 * Never produces HTML strings — the output is a typed AST that a
 * Svelte component renders through native elements.
 */
export function parseMarkdown(source: string): MarkdownNode[] {
	const lines = source.split('\n');
	const nodes: MarkdownNode[] = [];
	let i = 0;

	while (i < lines.length) {
		const line = lines[i];

		// Fenced code block.
		const fence = isFencedCodeStart(line);
		if (fence) {
			const lang = fence.lang;
			const body: string[] = [];
			i++;
			while (i < lines.length && !lines[i].startsWith('```')) {
				body.push(lines[i]);
				i++;
			}
			nodes.push({ type: 'code', lang, text: body.join('\n') });
			i++;
			continue;
		}

		// Heading.
		const h = isHeading(line);
		if (h) {
			nodes.push({ type: 'heading', level: h.level, text: escapeHtml(h.text) });
			i++;
			continue;
		}

		// List.
		const firstItem = isListItem(line);
		if (firstItem) {
			const ordered = firstItem.ordered;
			const items: InlineNode[][] = [parseInline(firstItem.text)];
			i++;
			while (i < lines.length) {
				const item = isListItem(lines[i]);
				if (item && item.ordered === ordered) {
					items.push(parseInline(item.text));
					i++;
				} else if (lines[i].trim() === '') {
					// Blank line between list items is OK.
					i++;
				} else {
					break;
				}
			}
			nodes.push({ type: 'list', ordered, items });
			continue;
		}

		// Blank line → paragraph break.
		if (line.trim() === '') {
			nodes.push({ type: 'break' });
			i++;
			continue;
		}

		// Paragraph — accumulate until blank line or block start.
		const paraLines: string[] = [line];
		i++;
		while (i < lines.length) {
			const next = lines[i];
			if (
				next.trim() === '' ||
				isHeading(next) ||
				isFencedCodeStart(next) ||
				isListItem(next)
			) {
				break;
			}
			paraLines.push(next);
			i++;
		}
		// Join with single space (soft line break), but preserve explicit
		// line breaks if the next line is also text.
		const paraText = paraLines.join(' ').trim();
		if (paraText.length > 0) {
			nodes.push({ type: 'paragraph', children: parseInline(paraText) });
		}
	}

	return nodes;
}
