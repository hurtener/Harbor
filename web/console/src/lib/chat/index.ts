// Harbor Console — the shared chat module's public surface (Phase 73n /
// D-130, D-091).
//
// # The encapsulate-first / extract-on-second-consumer contract
//
// This module is self-contained at `$lib/chat/`. Per CLAUDE.md §4.5 #11
// it imports NOTHING from outside `$lib/chat/`: no `$lib/protocol`, no
// `$lib/connection`, no `$lib/components/ui`. The Playground page (the
// FIRST consumer) injects a `ChatProtocolClient` adapter; the future
// packed `harbor dev` UI (the SECOND consumer) triggers a mechanical
// `git mv $lib/chat → web/shared/chat`.
//
// The public surface is deliberately narrow — the panel, the composer,
// and the injected `ChatProtocolClient` interface plus the chat data
// shapes. Internal components (`MessageBubble`, the cards, `CodeBlock`,
// `StreamingIndicator`) are NOT re-exported; a consumer composes
// `<ChatPanel>`.
//
// Importing this module registers the chat-bubble renderers into the
// Phase 73l canonical renderer registry (side-effect import below) —
// the §4.5 #11 "extend, do not fork" discharge.

import { registerChatBubbleRenderers } from './renderers/chat_bubble.js';

// Side-effect: extend the Phase 73l renderer registry with the
// chat-bubble / tool-call / diff renderers. Idempotent.
registerChatBubbleRenderers();

export { default as ChatPanel } from './ChatPanel.svelte';
export { default as ChatComposer } from './ChatComposer.svelte';

export {
	registerChatBubbleRenderers,
	MIME_TOOL_CALL_TRACE,
	MIME_DIFF,
	MIME_ARTIFACT_REFERENCE
} from './renderers/chat_bubble.js';

export type {
	ChatRole,
	ChatMessage,
	ChatToolCall,
	ChatDiff,
	ChatArtifactRef,
	ChatOverrides,
	ChatProtocolClient,
	ChatSendMode,
	SendMessageResult
} from './types.js';
