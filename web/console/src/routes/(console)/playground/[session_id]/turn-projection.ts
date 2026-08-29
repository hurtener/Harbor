// The projections that build a turn's rendered agent bubble.
//
// A turn's rendered result is shared by the current durable projection and
// the live App path. The history reducer below remains a standalone legacy
// utility for explicit `state.history` consumers; current Playground reopen
// does not use it as a transcript fallback.
//
//   - LIVE — an `mcp.app_available` SSE frame is decoded (`decodeAppAvailable`)
//     and projected onto the run's existing bubble (`appViewFromDiscovery`).
//   - DURABLE PROJECTION (the normal open path, HA-64 / D-425) — one
//     `SessionTurnRow` from `sessions.turns.list` is projected into the user +
//     agent bubbles (`turnRowMessages`). The row is already the runtime's
//     consumer-safe read model: query, answer union, attachments metadata,
//     lifecycle, DERIVED reasoning summary, agent binding, per-measure usage
//     availability, MCP App references with component availability, pause
//     state, and the bounded inline activity window. It is structurally
//     unable to carry raw thinking / tool arguments / App render authority.
//
// They live in ONE module on purpose. Keeping the live, durable, and explicit
// legacy utility projections together makes their boundaries visible and
// keeps each independently reachable by a spec without making the legacy
// reducer an answer authority for current chat reopen.

import type { MCPAppRefView, McpUiDisplayMode } from '$lib/chat/renderers/app-bridge-host.js';
import type { ChatArtifactRef, ChatMessage, ChatToolCall } from '$lib/chat/types.js';
import type { HistoryTurn } from '$lib/sessions/history.js';
import type {
	SessionTurnActivityRow,
	SessionTurnAnswer,
	SessionTurnAppRef,
	SessionTurnReasoning,
	SessionTurnRow,
	SessionTurnUsage
} from '$lib/protocol/session-turns.js';

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
 * The explicit history reducer (`reduceHistoryTurns`) reconstructs the same
 * attachment shape from its durable event window; a spec pins that utility
 * independently, but current chat reopen remains on `sessions.turns.*`.
 */
export function appViewFromDiscovery(ev: AppAvailableEvent): AppAttachment {
	const known = (KNOWN_DISPLAY_MODES as readonly string[]).includes(ev.displayMode);
	return {
		app: {
			...(ev.binding !== '' ? { binding: ev.binding } : {}),
			...(ev.agentID !== '' ? { agentId: ev.agentID } : {}),
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
 * The explicit history projection: turn one reduced `HistoryTurn` into an
 * agent bubble for a caller that opted into `state.history`, or `null` when
 * the turn has nothing to render. Current Playground reopen does not call it.
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

/* ===================================================================== */
/* The DURABLE PROJECTION (HA-64 / D-425) — one `SessionTurnRow` to the  */
/* rendered bubbles + the honest per-component state the page folds in.   */
/* ===================================================================== */

/** The closed per-measure usage availability states the wire serves. */
const USAGE_AVAILABLE = new Set(['exact', 'estimated']);

/** The terminal lifecycle statuses. */
const TERMINAL_STATUSES = new Set(['complete', 'failed', 'cancelled']);

/** Reads one usage measure's exact value, or undefined when unavailable —
 *  never a fabricated zero (CLAUDE.md §13, D-425). */
function measureValue(m: { state?: string; value?: number | null } | undefined): number | undefined {
	if (m === undefined) return undefined;
	if (m.state !== undefined && !USAGE_AVAILABLE.has(m.state)) return undefined;
	return typeof m.value === 'number' ? m.value : undefined;
}

/** Cost is exact integer micro-dollars (1e-6 USD) — never float64. */
function costUSD(usage: SessionTurnUsage): number | undefined {
	const micro = measureValue(usage.cost_micro_usd);
	return micro === undefined ? undefined : micro / 1e6;
}

/** Maps a durable answer component to the bubble's text + by-reference
 *  artifact (D-026). The closed union is rendered honestly: an evicted or
 *  unavailable answer is captioned as such, never as "(no answer recorded)"
 *  and never as a fabricated empty. */
export function answerFromRow(answer: SessionTurnAnswer): { text: string; ref?: ChatArtifactRef } {
	switch (answer.state) {
		case 'inline':
			return { text: answer.inline ?? '' };
		case 'artifact_ref':
			return {
				text: '',
				ref: answer.ref
					? {
							id: answer.ref.id,
							mime: answer.ref.mime_type ?? 'application/octet-stream',
							filename: answer.ref.filename || 'answer',
							sizeBytes: answer.ref.size_bytes
						}
					: undefined
			};
		case 'empty':
			// A definite, complete empty answer — renders as an empty bubble,
			// not the "(no answer recorded)" failure caption.
			return { text: '' };
		case 'evicted':
			return { text: '(answer evicted — no longer retained)' };
		case 'unavailable':
			return { text: '(answer unavailable)' };
		default:
			return { text: '' };
	}
}

/** Maps one bounded activity row to a content-free tool-call row. */
function toolCallFromRow(row: SessionTurnActivityRow): ChatToolCall {
	const status: ChatToolCall['status'] =
		row.status === 'succeeded'
			? 'succeeded'
			: row.status === 'failed' || row.status === 'policy_exhausted' || row.status === 'cancelled'
				? 'failed'
				: 'invoked';
	return { tool: row.tool, status, summary: row.summary ?? '' };
}

/** Builds the derived reasoning summary text from the durable steps — the
 *  steps carry a closed kind only (structurally NO raw thinking), so the
 *  render is a bounded summary plus the honest partial/dropped overflow. */
export function derivedReasoningSummary(reasoning: SessionTurnReasoning): string | undefined {
	const steps = reasoning.steps ?? [];
	if (reasoning.complete === 'unavailable' || steps.length === 0) {
		return undefined;
	}
	const kinds = new Map<string, number>();
	for (const step of steps) {
		const kind = step.kind || 'step';
		kinds.set(kind, (kinds.get(kind) ?? 0) + 1);
	}
	const parts = [...kinds.entries()].map(([kind, n]) => (n > 1 ? `${kind} ×${n}` : kind));
	let text = `Derived reasoning · ${steps.length} step${steps.length === 1 ? '' : 's'}${parts.length > 0 ? ` (${parts.join(', ')})` : ''}`;
	const dropped = reasoning.dropped ?? 0;
	if (reasoning.complete === 'partial' && dropped > 0) {
		text += ` · ${dropped} older step${dropped === 1 ? '' : 's'} not retained`;
	}
	return text;
}

/** The honest per-component state the page folds in from a durable row. */
export interface TurnRowMessages {
	/** The rendered user bubble (null when the row carried no query). */
	user: ChatMessage | null;
	/** The rendered agent bubble (null when the row has nothing to render). */
	agent: ChatMessage | null;
	/** The wire lifecycle status. */
	status: string;
	/** True when the row is paused (durable pause state). */
	paused: boolean;
	/** The pause reason, when the durable pause component reports one. */
	pauseReason?: string;
	/** The agent binding (id/name/provenance/availability). */
	agentBinding: { id?: string; name?: string; bindingSource?: string; complete?: string };
	/** Per-measure usage — present ONLY when the measure is available. */
	usage: { tokens?: number; costUSD?: number; promptTokens?: number; outputTokens?: number; latencyMs?: number; model?: string };
	/** Honest activity overflow: the inline window is bounded, `more` marks
	 *  older rows beyond it and `dropped` counts them. */
	activityOverflow: { more: boolean; dropped: number };
	/** Honest reasoning overflow. */
	reasoningPartial: boolean;
	reasoningDropped: number;
	/** Number of attachments the projection reports unavailable. */
	attachmentsUnavailable: number;
	/** The durable MCP App references — durable metadata + component
	 *  availability ONLY. Never an HA-56 render admission: the view carries
	 *  no `binding`, and the page never derives one; the separate live
	 *  renderer lane obtains admission fresh after reopen authorization. */
	apps: Array<{ serverID: string; view: MCPAppRefView; availability?: string; complete?: string }>;
}

const KNOWN_APP_DISPLAY_MODES = new Set(['inline', 'fullscreen', 'pip']);

/** Maps one durable App ref to the renderer view — metadata only, no
 *  `binding` (render admission is minted fresh by the live lane, never
 *  serialized into a turn row or page state). */
export function appViewFromRow(ref: SessionTurnAppRef): MCPAppRefView {
	return {
		...(ref.effective_agent_id !== undefined && ref.effective_agent_id !== ''
			? { agentId: ref.effective_agent_id }
			: {}),
		resourceUri: ref.resource_uri,
		displayMode:
			ref.display_mode !== undefined && KNOWN_APP_DISPLAY_MODES.has(ref.display_mode)
				? (ref.display_mode as McpUiDisplayMode)
				: '',
		rawHtmlTrusted: ref.raw_html_trusted === true,
		...(ref.tool_call_id !== undefined && ref.tool_call_id !== ''
			? { toolCallId: ref.tool_call_id }
			: {}),
		...(ref.tool_name !== undefined && ref.tool_name !== '' ? { toolName: ref.tool_name } : {})
		// NOTE: `binding` is deliberately absent — never carry/derive a
		// render admission on the durable path (HA-56).
	};
}

/** Maps one attachment metadata row to a by-reference artifact. */
function artifactFromAttachment(a: {
	id: string;
	filename?: string;
	mime_type?: string;
	size_bytes?: number;
	availability?: string;
}): ChatArtifactRef {
	return {
		id: a.id,
		mime: a.mime_type ?? 'application/octet-stream',
		filename: a.filename || '(unnamed attachment)',
		sizeBytes: a.size_bytes
	};
}

/**
 * The DURABLE projection: one consumer `SessionTurnRow` (from
 * `sessions.turns.list`) → the user + agent bubbles, plus the honest
 * per-component state the page folds into the header/KPI/render (lifecycle,
 * pause, agent binding, usage availability, activity overflow, reasoning
 * overflow, attachment availability, App availability).
 *
 * Honesty rules (D-425, CLAUDE.md §13):
 *   - usage values appear ONLY when their measure is exact/estimated — an
 *     unavailable measure is omitted, never a fabricated zero;
 *   - an evicted/unavailable answer is captioned as such;
 *   - reasoning renders a DERIVED summary (the row structurally cannot carry
 *     raw thinking) with the honest partial/dropped overflow;
 *   - the bounded activity window maps 1:1; rows beyond it are reported via
 *     `activityOverflow`, never invented;
 *   - App refs carry durable metadata + availability ONLY — no render
 *     admission (`binding`), which the live renderer lane mints fresh after
 *     reopen authorization;
 *   - attachments with `availability: "unavailable"` are counted, not dropped
 *     silently, and never rendered as if complete.
 */
export function turnRowMessages(row: SessionTurnRow): TurnRowMessages {
	const taskID = row.task_id || row.turn_id;
	const at = row.started_at || row.updated_at;

	// ---- user bubble (query + input attachment metadata) ----
	const queryText = row.query?.complete === 'complete' ? (row.query.text ?? '') : '';
	const inputs = (row.inputs ?? []).filter((a) => a.availability !== 'unavailable');
	const unavailableInputs = (row.inputs ?? []).filter((a) => a.availability === 'unavailable').length;
	const user: ChatMessage | null =
		queryText !== '' || inputs.length > 0
			? {
					id: `t-${row.turn_id}-u`,
					role: 'user',
					text: queryText,
					taskID,
					at: row.query?.at || at,
					...(inputs.length > 0 ? { artifacts: inputs.map(artifactFromAttachment) } : {})
				}
			: null;

	// ---- agent bubble ----
	const answer = answerFromRow(row.answer);
	const apps: TurnRowMessages['apps'] = (row.apps ?? []).map((ref) => ({
		serverID: ref.server_id,
		view: appViewFromRow(ref),
		availability: ref.availability,
		complete: ref.complete
	}));
	// The bubble carries ONE app slot (the live lane does too). Prefer the
	// last AVAILABLE ref (the collection is ordered; repeats replace in
	// place), falling back to the last overall so an unavailable ref still
	// reaches the renderer's honest "no longer available" path.
	const attachable =
		apps.filter((a) => a.availability !== 'unavailable').length > 0
			? apps.filter((a) => a.availability !== 'unavailable')
			: apps;
	const appSlot = attachable.length > 0 ? attachable[attachable.length - 1] : undefined;

	const outputs = (row.outputs ?? []).filter((a) => a.availability !== 'unavailable');
	const unavailableOutputs = (row.outputs ?? []).filter((a) => a.availability === 'unavailable').length;

	const activity = row.activity;
	const toolCalls = (activity.rows ?? []).map(toolCallFromRow);

	const reasoningText = derivedReasoningSummary(row.reasoning);

	const usage = row.usage;
	const tokens = measureValue(usage.total_tokens);
	const cost = costUSD(usage);
	const promptTokens = measureValue(usage.prompt_tokens);
	const outputTokens = measureValue(usage.completion_tokens);
	const latencyNs = measureValue(usage.latency_ns);
	const model = usage.model ?? '';

	// Duration: prefer the run's own wall-clock when terminal (the wire has
	// no duration field; started→finished is the honest elapsed span).
	let elapsedMs: number | undefined;
	if (TERMINAL_STATUSES.has(row.status) && row.finished_at && row.started_at) {
		const ms = Date.parse(row.finished_at) - Date.parse(row.started_at);
		if (Number.isFinite(ms) && ms > 0) elapsedMs = ms;
	}

	const terminal = TERMINAL_STATUSES.has(row.status);
	const hasRenderable = answer.text !== '' || answer.ref !== undefined || terminal || apps.length > 0;
	const agent: ChatMessage | null = hasRenderable
		? {
				id: `t-${row.turn_id}-a`,
				role: 'agent',
				text: answer.text,
				taskID,
				at,
				// A reopened running/paused turn stays pending until the live
				// lane converges it (the durable row is its mutable snapshot).
				pending: !terminal,
				...(reasoningText !== undefined ? { reasoningText } : {}),
				...(toolCalls.length > 0 ? { toolCalls } : {}),
				...(answer.ref !== undefined
					? { artifacts: [answer.ref, ...outputs.map(artifactFromAttachment)] }
					: outputs.length > 0
						? { artifacts: outputs.map(artifactFromAttachment) }
						: {}),
				...(appSlot !== undefined ? { app: appSlot.view, serverID: appSlot.serverID } : {}),
				meta: {
					...(elapsedMs !== undefined ? { elapsedMs } : {}),
					...(tokens !== undefined ? { tokens } : {}),
					...(cost !== undefined ? { costUSD: cost } : {})
				}
			}
		: null;

	return {
		user,
		agent,
		status: row.status,
		paused:
			row.status === 'paused' ||
			(row.pause?.availability === 'complete' && row.pause.lifecycle !== 'resolved'),
		...(row.pause?.reason !== undefined && row.pause.reason !== '' ? { pauseReason: row.pause.reason } : {}),
		agentBinding: {
			id: row.agent?.id,
			name: row.agent?.name,
			bindingSource: row.agent?.binding_source,
			complete: row.agent?.complete
		},
		usage: {
			...(tokens !== undefined ? { tokens } : {}),
			...(cost !== undefined ? { costUSD: cost } : {}),
			...(promptTokens !== undefined ? { promptTokens } : {}),
			...(outputTokens !== undefined ? { outputTokens } : {}),
			...(latencyNs !== undefined ? { latencyMs: latencyNs / 1e6 } : {}),
			...(model !== '' ? { model } : {})
		},
		activityOverflow: { more: activity.more === true, dropped: activity.dropped ?? 0 },
		reasoningPartial: row.reasoning.complete === 'partial',
		reasoningDropped: row.reasoning.dropped ?? 0,
		attachmentsUnavailable: unavailableInputs + unavailableOutputs,
		apps
	};
}
