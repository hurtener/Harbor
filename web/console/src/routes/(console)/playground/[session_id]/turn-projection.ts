// The two projections that build a turn's rendered agent bubble.
//
// A turn reaches the transcript by one of two routes, and BOTH must produce
// the same rendered result:
//
//   - LIVE — an `mcp.app_available` SSE frame is decoded (`decodeAppAvailable`)
//     and projected onto the run's existing bubble (`appViewFromDiscovery`).
//   - REPLAY — the durable event window is reduced into `HistoryTurn`s
//     (`reduceHistoryTurns`) and each turn is projected into a fresh bubble
//     (`hydratedAgentMessage`).
//
// They live in ONE module on purpose. While both were inline in `+page.svelte`
// neither was importable, so neither was testable: deleting the App fields
// from the hydration mapping left the whole replay feature inert with every
// test still green, and the live projection could only be compared against a
// hand-copied re-implementation that would agree with a one-sided change.
// Colocating them keeps the pair visible to a reader and reachable by a spec.

import type { MCPAppRefView, McpUiDisplayMode } from '$lib/chat/renderers/app-bridge-host.js';
import type { ChatMessage, ChatToolCall } from '$lib/chat/types.js';
import type { HistoryTurn } from '$lib/sessions/history.js';

import type { AppAvailableEvent } from './wire-events.js';

/** The MCP-Apps display modes the host can negotiate. */
const KNOWN_DISPLAY_MODES = ['inline', 'fullscreen', 'pip'] as const;

/** An App reference plus the server that hosts its `ui://` document. */
export interface AppAttachment {
	app: MCPAppRefView;
	serverID: string;
}

/**
 * The LIVE projection: turn a decoded `mcp.app_available` frame into the
 * `MCPAppRefView` the bubble carries. An unrecognised display-mode hint
 * normalises to `''` — "the server stated no preference" — which the renderer
 * reads as inline.
 *
 * The replay reducer (`reduceHistoryTurns`) reconstructs the identical shape
 * from the identical durable event; a spec pins the two against each other, so
 * a one-sided change here (a dropped `toolCallId`, a widened mode set) fails.
 */
export function appViewFromDiscovery(ev: AppAvailableEvent): AppAttachment {
	const known = (KNOWN_DISPLAY_MODES as readonly string[]).includes(ev.displayMode);
	return {
		app: {
			resourceUri: ev.resourceUri,
			displayMode: known ? (ev.displayMode as McpUiDisplayMode) : '',
			rawHtmlTrusted: ev.rawHtmlTrusted,
			// The correlation key for the after-init Data Delivery push.
			toolCallId: ev.toolCallId,
			// The originating tool name, projected onto the host-context
			// `toolInfo` so the app can name the call it belongs to.
			toolName: ev.toolName
		},
		serverID: ev.serverID
	};
}

/** The per-turn context `hydratedAgentMessage` cannot read off the turn. */
export interface HydratedTurnContext {
	/** The turn instant (the catalog's `started_at`, falling back to the turn's). */
	at: string;
	/** The turn's elapsed wall-clock, preferring the catalog's `duration_ms`. */
	durationMs: number;
	/** The turn's reconstructed tool-call rows, already run-stamped. */
	toolCalls: ChatToolCall[];
}

/**
 * The REPLAY projection: turn one reduced `HistoryTurn` into the reopened
 * agent bubble, or `null` when the turn has nothing to render.
 *
 * The render gate admits a turn with an answer, a terminal lifecycle event, OR
 * an App: an App-bearing turn is worth rendering on its own, because the App
 * IS the turn's output — the model-facing text is deliberately terse — so it
 * must not fall through and vanish, which is the exact bug the replay closes.
 * For the same reason the "(no answer recorded)" placeholder is suppressed
 * when an App is present: the live view showed no text there either, and
 * captioning a perfectly good App with "no answer recorded" would be a worse
 * lie than the silence.
 */
export function hydratedAgentMessage(
	turn: HistoryTurn,
	ctx: HydratedTurnContext
): ChatMessage | null {
	if (!turn.answer && !turn.terminal && !turn.app) return null;
	return {
		id: `h-${turn.runID}-a`,
		role: 'agent',
		text: turn.answer || (turn.app ? '' : '(no answer recorded)'),
		taskID: turn.runID,
		at: ctx.at,
		// The interactive MCP App the turn declared, reconstructed from the
		// durable `mcp.app_available` event. MessageBubble mounts the SAME
		// renderer the live path mounts (it needs both `app` and `serverID`), so
		// a reopened App renders equivalently to the live view; the renderer
		// re-reads the PERSISTED tool context by the ref's deterministic
		// `toolCallId` and renders an honest "no longer available" placeholder
		// when it cannot be resolved.
		app: turn.app,
		serverID: turn.serverID,
		reasoningText: turn.reasoning || undefined,
		// The structured, ordered reasoning steps reconstructed from the durable
		// planner.decision stream — byte-equivalent to the live path's
		// parseReasoningSteps(enriched tasks.get). MessageBubble prefers these
		// over reasoningText, so a reopened turn renders the ordered
		// reasoning↔tool accordion identically to the live view; a turn with no
		// non-empty steps cleanly falls back to reasoningText.
		reasoningSteps: turn.reasoningSteps.length > 0 ? turn.reasoningSteps : undefined,
		toolCalls: ctx.toolCalls.length > 0 ? ctx.toolCalls : undefined,
		meta: {
			elapsedMs: ctx.durationMs > 0 ? ctx.durationMs : undefined,
			tokens: turn.tokens > 0 ? turn.tokens : undefined,
			costUSD: turn.costUSD > 0 ? turn.costUSD : undefined
		}
	};
}
