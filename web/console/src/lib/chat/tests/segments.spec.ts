// Vitest unit test for the chat-module body segmentation
// (Phase 73n / D-130). Pins the pure `splitChatSegments` helper the
// `<MessageBubble>` uses to split a message into plain-text + fenced-
// code segments.

import { describe, expect, it } from 'vitest';

import { splitChatSegments } from '../segments.js';

describe('splitChatSegments', () => {
	it('returns a single text segment for a plain message', () => {
		const segs = splitChatSegments('hello world');
		expect(segs).toEqual([{ kind: 'text', value: 'hello world' }]);
	});

	it('returns an empty list for an empty message', () => {
		expect(splitChatSegments('')).toEqual([]);
	});

	it('extracts a fenced code block', () => {
		const segs = splitChatSegments('before\n```go\nfmt.Println("hi")\n```\nafter');
		expect(segs).toHaveLength(3);
		expect(segs[0]).toEqual({ kind: 'text', value: 'before\n' });
		expect(segs[1]).toEqual({ kind: 'code', value: 'fmt.Println("hi")\n', lang: 'go' });
		expect(segs[2]).toEqual({ kind: 'text', value: '\nafter' });
	});

	it('handles a fenced block with no language tag', () => {
		const segs = splitChatSegments('```\nraw code\n```');
		expect(segs).toEqual([{ kind: 'code', value: 'raw code\n', lang: '' }]);
	});

	it('extracts multiple fenced blocks in order', () => {
		const segs = splitChatSegments('a\n```js\nx\n```\nb\n```py\ny\n```');
		const kinds = segs.map((s) => s.kind);
		expect(kinds).toEqual(['text', 'code', 'text', 'code']);
		expect((segs[1] as { lang: string }).lang).toBe('js');
		expect((segs[3] as { lang: string }).lang).toBe('py');
	});

	it('is pure — the same input yields an identical segment list', () => {
		const input = 'msg\n```ts\nconst a = 1;\n```';
		expect(splitChatSegments(input)).toEqual(splitChatSegments(input));
	});
});
