// Vitest unit test for the Phase 73n chat-bubble renderer extension
// (D-130). Proves the three chat-bubble renderers register into the
// SAME Phase 73l canonical registry via `registerRenderer` — they
// EXTEND the dispatch core, they do not fork it (CLAUDE.md §4.5 #11).

import { describe, expect, it } from 'vitest';

import { dispatchRenderer, registeredSources } from './index.js';
import {
	MIME_ARTIFACT_REFERENCE,
	MIME_DIFF,
	MIME_TOOL_CALL_TRACE,
	registerChatBubbleRenderers
} from './chat_bubble.js';

describe('chat-bubble renderer extension', () => {
	it('registers the three chat-bubble renderers into the canonical registry', () => {
		registerChatBubbleRenderers();
		const sources = registeredSources();
		for (const expected of ['tool-call-trace', 'diff', 'artifact-reference']) {
			expect(sources).toContain(expected);
		}
	});

	it('keeps the Phase 73l MIME renderers alongside the chat-bubble ones', () => {
		registerChatBubbleRenderers();
		const sources = registeredSources();
		// The dispatch core is EXTENDED, not replaced — the six 73l MIME
		// renderers still resolve.
		for (const builtin of ['markdown', 'code', 'image', 'pdf', 'audio', 'json']) {
			expect(sources).toContain(builtin);
		}
	});

	it('dispatches the tool-call-trace MIME to the tool-call-trace renderer', () => {
		registerChatBubbleRenderers();
		expect(dispatchRenderer(MIME_TOOL_CALL_TRACE).source).toBe('tool-call-trace');
	});

	it('dispatches the diff MIME to the diff renderer', () => {
		registerChatBubbleRenderers();
		expect(dispatchRenderer(MIME_DIFF).source).toBe('diff');
	});

	it('dispatches the artifact-reference MIME to the artifact-reference renderer', () => {
		registerChatBubbleRenderers();
		expect(dispatchRenderer(MIME_ARTIFACT_REFERENCE).source).toBe('artifact-reference');
	});

	it('is idempotent — repeated registration does not duplicate rules', () => {
		registerChatBubbleRenderers();
		const before = registeredSources().filter((s) => s === 'diff').length;
		registerChatBubbleRenderers();
		registerChatBubbleRenderers();
		const after = registeredSources().filter((s) => s === 'diff').length;
		expect(after).toBe(before);
	});

	it('leaves an unknown MIME on the fallback renderer', () => {
		registerChatBubbleRenderers();
		expect(dispatchRenderer('application/x-not-a-real-type').source).toBe('fallback');
	});
});
