// Harbor Console — chat message body segmentation (Phase 73n / D-130).
//
// A pure, deterministic helper the `<MessageBubble>` uses to split a
// message body into plain-text + fenced-code segments. Kept separate
// from the component so the Vitest spec can pin the parsing without
// rendering Svelte. Lives inside `$lib/chat/` — the chat module imports
// no Console internal (CLAUDE.md §4.5 #11).

/** One segment of a chat message body. */
export type ChatSegment =
	| { kind: 'text'; value: string }
	| { kind: 'code'; value: string; lang: string };

/**
 * Splits a chat message body into ordered plain-text + fenced-code
 * segments. A fenced block is a ```lang\n…``` triple. The function is
 * pure and deterministic — the same input always yields the same
 * segment list — so the Vitest spec pins it directly.
 */
export function splitChatSegments(text: string): ChatSegment[] {
	const out: ChatSegment[] = [];
	const fence = /```([^\n]*)\n([\s\S]*?)```/g;
	let last = 0;
	let m: RegExpExecArray | null;
	while ((m = fence.exec(text)) !== null) {
		if (m.index > last) {
			out.push({ kind: 'text', value: text.slice(last, m.index) });
		}
		out.push({ kind: 'code', value: m[2], lang: m[1].trim() });
		last = m.index + m[0].length;
	}
	if (last < text.length) {
		out.push({ kind: 'text', value: text.slice(last) });
	}
	return out;
}
