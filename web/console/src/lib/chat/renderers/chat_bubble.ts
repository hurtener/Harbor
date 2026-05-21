// Chat-bubble renderer registration — Phase 73n / D-130.
//
// This module EXTENDS the Phase 73l canonical renderer registry
// (`./index.ts`) with the three chat-bubble-specific renderers the
// Playground's chat stream needs: tool-call traces, unified diffs, and
// artifact references.
//
// # The extend-on-second-consumer contract (CLAUDE.md §4.5 #11)
//
// Phase 73l shipped the dispatch core (`index.ts`) plus the six MIME
// renderers (markdown / code / image / pdf / audio / json). It is open
// for registration, closed for modification. Phase 73n adds renderers
// by calling `registerRenderer` from THIS module's init — it does NOT
// edit the dispatch core, and it does NOT create a second registry.
// Importing this module once (the chat module's `index.ts` does) runs
// the registrations.
//
// The three renderers register under synthetic Harbor MIME types so
// they never collide with a real IANA media type a Phase 73l renderer
// already claims:
//
//   - application/vnd.harbor.tool-call-trace → ToolCallTrace.svelte
//   - application/vnd.harbor.diff            → DiffView.svelte
//   - application/vnd.harbor.artifact-reference → ArtifactReference.svelte
//
// A chat bubble maps a `ChatToolCall` / `ChatDiff` / `ChatArtifactRef`
// onto the matching synthetic MIME and dispatches through the SAME
// `dispatchRenderer` the Phase 73l Artifacts preview uses.

import { mimeIs, registerRenderer } from './index.js';

import ArtifactReferenceRenderer from './ArtifactReference.svelte';
import DiffViewRenderer from './DiffView.svelte';
import ToolCallTraceRenderer from './ToolCallTrace.svelte';

/** The synthetic MIME the tool-call-trace renderer dispatches on. */
export const MIME_TOOL_CALL_TRACE = 'application/vnd.harbor.tool-call-trace';
/** The synthetic MIME the diff-view renderer dispatches on. */
export const MIME_DIFF = 'application/vnd.harbor.diff';
/** The synthetic MIME the artifact-reference renderer dispatches on. */
export const MIME_ARTIFACT_REFERENCE = 'application/vnd.harbor.artifact-reference';

// `registered` guards against a double-registration if this module is
// imported more than once (the registry is a process-wide singleton;
// re-running `registerRenderer` would append duplicate rules). The
// dispatch core is first-match-wins, so duplicates would not change
// behaviour — but the guard keeps `registeredSources()` clean for the
// encapsulation / dispatch tests.
let registered = false;

/**
 * Registers the three chat-bubble renderers into the canonical
 * registry. Idempotent — safe to call from multiple module inits.
 * Called once at the chat module's load (see `$lib/chat/index.ts`).
 */
export function registerChatBubbleRenderers(): void {
	if (registered) {
		return;
	}
	registered = true;
	registerRenderer(mimeIs(MIME_TOOL_CALL_TRACE), {
		source: 'tool-call-trace',
		component: ToolCallTraceRenderer
	});
	registerRenderer(mimeIs(MIME_DIFF), {
		source: 'diff',
		component: DiffViewRenderer
	});
	registerRenderer(mimeIs(MIME_ARTIFACT_REFERENCE), {
		source: 'artifact-reference',
		component: ArtifactReferenceRenderer
	});
}
