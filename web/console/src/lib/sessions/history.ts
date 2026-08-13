// Harbor Console — session-reopen windowed hydration helper (D-254).
//
// This is the FIRST consumer of the `state.history` Protocol surface
// (CLAUDE.md §13 primitive-with-consumer rule): a tail-first, windowed,
// scroll-up-by-cursor reader of a session's durable event stream, plus a
// pure client-side reduction of the loaded events into reopen turns. The
// Playground session-reopen hydration drives this instead of its former
// full-load `tasks.list` + N×`tasks.get` reconstruction.
//
// The reduction stays client-side (the surface returns flat events, never
// pre-reduced turns): the agent answer + reasoning are reconstructed from the
// `llm.completion.chunk` deltas; the per-turn CONTENT-FREE metadata (usage /
// cost / model from `llm.cost.recorded`, tool-call rows from `planner.decision`
// + `tool.completed`/`tool.failed`, duration from the task lifecycle) is folded
// from the SAME bus events the live SSE stream reduces — so a reopen renders
// the header stats, per-message badges, TOOL CALLS badges, and model chip
// IDENTICAL to the live view. Tool args/results NEVER reach a payload
// (CLAUDE.md §7 rule 7), so none are folded. The user query text is NOT carried
// in the durable event payloads (the task lifecycle payloads omit it), so the
// caller folds it in from a single catalog lookup.

import type { McpUiDisplayMode, MCPAppRefView } from '../chat/renderers/app-bridge-host.js';
import type { ProtocolClient } from '../protocol/client.js';
import type { StateEvent, StateHistoryResponse } from '../protocol/state.js';
import { DEFAULT_STATE_HISTORY_LIMIT } from '../protocol/state.js';

/** Options for {@link loadSessionHistory}. */
export interface LoadHistoryOptions {
	/** Per-page window size (clamped server-side to the max). */
	pageLimit?: number;
	/**
	 * The maximum number of windows to scroll up through. A reopen loads a
	 * bounded recent slice, not the entire (possibly enormous) history; the
	 * operator scrolls further on demand. Defaults to 8 pages.
	 */
	maxPages?: number;
}

/** The merged result of a windowed history load. */
export interface LoadedHistory {
	/** Every loaded event, oldest-first across all scrolled windows. */
	events: StateEvent[];
	/** The session's retained head/tail sequence (from the first page). */
	headSequence: number;
	tailSequence: number;
	/** True when older events remain below the loaded window (page budget hit). */
	hasMore: boolean;
	/** True when the durable log trimmed events older than the retained head. */
	truncated: boolean;
}

/**
 * Loads a session's recent durable event history TAIL-FIRST through the
 * typed `state.history` surface, scrolling up by `next_cursor` until the
 * retained head is reached or the page budget is spent. Returns the merged
 * events oldest-first.
 *
 * No hand-rolled `fetch` — every request goes through the injected typed
 * `ProtocolClient` (CLAUDE.md §4.5 rule 5, §13).
 */
export async function loadSessionHistory(
	client: ProtocolClient,
	sessionID: string,
	opts: LoadHistoryOptions = {}
): Promise<LoadedHistory> {
	const pageLimit = opts.pageLimit ?? DEFAULT_STATE_HISTORY_LIMIT;
	const maxPages = opts.maxPages ?? 8;

	const pages: StateEvent[][] = [];
	let before = 0; // 0 ⇒ from the tail.
	let head = 0;
	let tail = 0;
	let hasMore = false;
	let truncated = false;

	for (let page = 0; page < maxPages; page++) {
		const resp: StateHistoryResponse = await client.state.history({
			session_id: sessionID,
			before,
			limit: pageLimit
		});
		if (page === 0) {
			head = resp.head_sequence;
			tail = resp.tail_sequence;
		}
		truncated = truncated || resp.truncated === true;
		// Each page is oldest-first WITHIN the window; collect pages
		// newest-window-first, then flatten oldest-first below.
		pages.push(resp.events);
		hasMore = resp.has_more;
		if (!resp.has_more || resp.next_cursor === 0) {
			hasMore = false;
			break;
		}
		before = resp.next_cursor;
	}

	// pages[0] is the newest window, pages[n] the oldest scrolled window;
	// each is internally oldest-first. Reverse the page order and
	// concatenate so the merged stream is oldest-first end-to-end.
	const events: StateEvent[] = [];
	for (let i = pages.length - 1; i >= 0; i--) {
		events.push(...pages[i]);
	}

	return { events, headSequence: head, tailSequence: tail, hasMore, truncated };
}

/** A reconstructed reasoning step on a reopened turn — the ordered
 *  per-ReAct-step provider-side thinking trace, mirroring the wire
 *  `ReasoningStep` shape (`$lib/chat/types.ts`) so the hydrated message
 *  field is assignment-compatible with the live path's. `index` is the
 *  0-based position in the FULL trajectory-step sequence (the enricher's
 *  `i`-over-`traj.Steps`), so a reopened accordion renders identically to
 *  the live `parseReasoningSteps(enriched tasks.get)` projection. */
export interface HistoryReasoningStep {
	/** The step's 0-based position in the run's trajectory-step sequence. */
	index: number;
	/** The provider-side thinking trace captured for the step (never empty). */
	reasoning_trace: string;
}

/** A reconstructed tool-call row on a reopened turn (content-free — the
 *  tool NAME + status + a short redacted summary, NEVER raw args/results). */
export interface HistoryToolCall {
	/** The tool the turn invoked. */
	tool: string;
	/** Resolved lifecycle status. */
	status: 'invoked' | 'succeeded' | 'failed';
	/** A short summary — duration for succeeded, error class/message for
	 *  failed, '' for still-invoked. Never raw args/results. */
	summary: string;
}

/** One reopened conversation turn reduced from the event window. */
export interface HistoryTurn {
	/** The run (task) id the turn belongs to. */
	runID: string;
	/** The reconstructed agent answer text (concatenated content deltas). */
	answer: string;
	/** The reconstructed reasoning text (concatenated reasoning deltas), if any. */
	reasoning: string;
	/** The earliest event instant in the run (RFC-3339 UTC). */
	at: string;
	/** Whether the run reached a terminal lifecycle event. */
	terminal: boolean;
	/** Total tokens summed across the run's `llm.cost.recorded` events. */
	tokens: number;
	/** Prompt (input) tokens summed across the run's cost events. */
	promptTokens: number;
	/** Completion (output) tokens summed across the run's cost events. */
	outputTokens: number;
	/** Cost in USD summed across the run's cost events. */
	costUSD: number;
	/** The model id last seen on a cost event (the run's model chip). */
	model: string;
	/** The run's active duration in ms (from task lifecycle timestamps). */
	durationMs: number;
	/** The turn's reconstructed tool-call rows (content-free). */
	toolCalls: HistoryToolCall[];
	/**
	 * The turn's reconstructed ordered reasoning steps — the per-ReAct-step
	 * provider-side thinking, reconstructed from the run's `planner.decision`
	 * events, byte-equivalent to what the live enricher trajectory projection
	 * (`parseReasoningSteps`) produces. Folded ONLY for step-appending
	 * decisions (`DecisionKind ∈ {CallTool, CallParallel, SpawnTask,
	 * AwaitTask}`) with non-empty reasoning — `Finish` / `RequestPause`
	 * decisions append no trajectory step, so they emit no reasoning step.
	 */
	reasoningSteps: HistoryReasoningStep[];
	/**
	 * The interactive MCP App the turn's tool result declared, reconstructed
	 * from the run's durable `mcp.app_available` event. Deliberately the SAME
	 * {@link MCPAppRefView} the LIVE discovery path builds (`applyAppAvailable`
	 * in the Playground page) — NOT a second wire shape — so the hydrated
	 * message field is assignment-compatible with the live path's and the
	 * renderer that already mounts a live App mounts a replayed one unchanged.
	 * Absent when the turn declared no App. Paired with {@link serverID}.
	 */
	app?: MCPAppRefView;
	/**
	 * The MCP server (source id) hosting {@link app} — the renderer reads the
	 * app's `ui://` document from this server, and pairs it with the ref's
	 * `toolCallId` to fetch the persisted tool context. Set exactly when
	 * `app` is set.
	 */
	serverID?: string;
}

/** Reads the first present string field across PascalCase / snake_case keys. */
function readString(obj: Record<string, unknown>, keys: string[]): string {
	for (const k of keys) {
		const v = obj[k];
		if (typeof v === 'string') return v;
	}
	return '';
}

/** Reads the first present finite number across PascalCase / snake_case keys. */
function readNumber(obj: Record<string, unknown>, keys: string[]): number {
	for (const k of keys) {
		const v = obj[k];
		if (typeof v === 'number' && Number.isFinite(v)) return v;
	}
	return 0;
}

/** Reads the first present boolean across PascalCase / snake_case keys. */
function readBool(obj: Record<string, unknown>, keys: string[]): boolean {
	for (const k of keys) {
		if (obj[k] === true) return true;
	}
	return false;
}

/** Reads a nested object field across PascalCase / snake_case keys. */
function readObject(obj: Record<string, unknown>, keys: string[]): Record<string, unknown> {
	for (const k of keys) {
		const v = obj[k];
		if (v !== null && typeof v === 'object') return v as Record<string, unknown>;
	}
	return {};
}

/** The set of task-lifecycle terminal types (mirrors the live decoder). */
const TERMINAL_TASK_TYPES = new Set(['task.completed', 'task.failed', 'task.cancelled']);

/**
 * The STEP-APPENDING `DecisionKind`s — the runloop appends exactly one
 * `traj.Step` per decision of these kinds (its `default`-branch cases),
 * while `Finish` / `RequestPause` return before the append. The enricher
 * indexes reasoning steps over `traj.Steps`, so the reducer must advance
 * the per-run step ordinal ONLY on these kinds to match the live index.
 * An allowlist (not a Finish/RequestPause denylist) so any unknown/future
 * non-step-appending kind auto-excludes.
 */
const STEP_APPENDING_DECISION_KINDS = new Set([
	'CallTool',
	'CallParallel',
	'SpawnTask',
	'AwaitTask'
]);

/**
 * The MCP-Apps display modes the host can negotiate. An unrecognised (or
 * absent) hint normalises to `''` — "the server stated no preference" — which
 * the renderer reads as inline. Mirrors the live decoder's guard so a
 * reopened App negotiates the SAME mode a live one did.
 */
const KNOWN_DISPLAY_MODES = new Set<string>(['inline', 'fullscreen', 'pip']);

/** The run id an event belongs to — the wire `run` field, or a payload TaskID. */
function eventRunID(ev: StateEvent): string {
	if (ev.run) return ev.run;
	if (ev.payload !== null && typeof ev.payload === 'object') {
		return readString(ev.payload as Record<string, unknown>, ['TaskID', 'task_id']);
	}
	return '';
}

/**
 * `reduceHistoryTurns` folds a window of durable events into per-run turns,
 * reconstructing each run's answer + reasoning from its
 * `llm.completion.chunk` deltas, AND its content-free per-turn metadata from
 * the same bus events the live stream reduces:
 *
 *   - `llm.cost.recorded` → usage (`Usage.TotalTokens` / `Usage.PromptTokens`
 *     / `Usage.CompletionTokens`), cost (`Cost.TotalCost`), and the model id
 *     (`Model`) — summed across the run's LLM calls.
 *   - `planner.decision` with `DecisionKind === 'CallTool'` → opens a tool-call
 *     row (status `invoked`) keyed by the tool name.
 *   - `planner.decision` with a STEP-APPENDING `DecisionKind` (`CallTool` /
 *     `CallParallel` / `SpawnTask` / `AwaitTask`) → advances the per-run step
 *     ordinal and, when the decision's `ReasoningTrace` is non-empty, appends
 *     `{index, reasoning_trace}` — reconstructing the ordered reasoning-step
 *     sequence the live enricher trajectory projection serves. `Finish` /
 *     `RequestPause` (and any non-matching kind) append no trajectory step in
 *     the runloop, so they touch NEITHER the ordinal nor the emission — a
 *     reasoning-bearing `Finish`/`RequestPause` never shows a step, exactly
 *     as the live view does not (the one correctness subtlety of the reopen).
 *   - `tool.completed` / `tool.failed` → resolves the matching open row's
 *     status + a short content-free summary (duration / error class), mirroring
 *     the live `applyToolLifecycle` reducer.
 *   - `mcp.app_available` → the turn's interactive MCP App ref (`app` +
 *     `serverID`), reconstructed into the SAME `MCPAppRefView` shape the live
 *     discovery path builds — so a reopened turn re-mounts the App the live
 *     transcript showed instead of degrading to the deliberately-terse
 *     model-facing tool text. LAST-WINS within a run (a turn whose tool
 *     results declared two Apps shows the later one), mirroring the live
 *     reducer, which overwrites the bubble's `app` on each discovery.
 *   - `task.started` → task-terminal timestamps → the run's active `durationMs`
 *     (a fallback; the Playground prefers the `tasks.list` `duration_ms`).
 *
 * NEVER folds tool args or results — those never reach an event payload
 * (CLAUDE.md §7 rule 7). Returns the turns oldest-first by first-seen event.
 * The durable event payloads are the redacted Go structs (PascalCase keys);
 * the reader tolerates both casings.
 */
export function reduceHistoryTurns(events: readonly StateEvent[]): HistoryTurn[] {
	const order: string[] = [];
	const byRun = new Map<string, HistoryTurn>();
	// Per-run task lifecycle instants, for the duration fallback.
	const started = new Map<string, string>();
	// Per-run 0-based trajectory-step ordinal — advanced ONLY on
	// step-appending decisions, mirroring the enricher's `i`-over-`traj.Steps`
	// so a reopened reasoning step's `index` equals the live projection's.
	const stepOrdinal = new Map<string, number>();

	const turnFor = (runID: string, at: string): HistoryTurn => {
		let t = byRun.get(runID);
		if (t === undefined) {
			t = {
				runID,
				answer: '',
				reasoning: '',
				at,
				terminal: false,
				tokens: 0,
				promptTokens: 0,
				outputTokens: 0,
				costUSD: 0,
				model: '',
				durationMs: 0,
				toolCalls: [],
				reasoningSteps: []
			};
			byRun.set(runID, t);
			order.push(runID);
		}
		return t;
	};

	// Resolve an open tool row (status 'invoked') for `tool`; append one if
	// none is open (a terminal event never silently dropped). Mirrors the
	// live `applyToolLifecycle`.
	const resolveToolRow = (
		turn: HistoryTurn,
		tool: string,
		status: HistoryToolCall['status'],
		summary: string
	) => {
		for (const row of turn.toolCalls) {
			if (row.tool === tool && row.status === 'invoked') {
				row.status = status;
				if (summary !== '') row.summary = summary;
				return;
			}
		}
		turn.toolCalls.push({ tool, status, summary });
	};

	for (const ev of events) {
		const runID = eventRunID(ev);
		if (runID === '') continue;
		const turn = turnFor(runID, ev.occurred_at);
		const p =
			ev.payload !== null && typeof ev.payload === 'object'
				? (ev.payload as Record<string, unknown>)
				: {};

		if (ev.type === 'llm.completion.chunk') {
			const delta = readString(p, ['Delta', 'delta']);
			const kind = readString(p, ['Kind', 'kind']);
			if (kind === 'reasoning') {
				turn.reasoning += delta;
			} else {
				turn.answer += delta;
			}
		} else if (ev.type === 'llm.cost.recorded') {
			const usage = readObject(p, ['Usage', 'usage']);
			const cost = readObject(p, ['Cost', 'cost']);
			turn.tokens += readNumber(usage, ['TotalTokens', 'total_tokens']);
			turn.promptTokens += readNumber(usage, ['PromptTokens', 'prompt_tokens']);
			turn.outputTokens += readNumber(usage, ['CompletionTokens', 'completion_tokens']);
			turn.costUSD += readNumber(cost, ['TotalCost', 'total_cost']);
			const model = readString(p, ['Model', 'model']);
			if (model !== '') turn.model = model;
		} else if (ev.type === 'planner.decision') {
			const kind = readString(p, ['DecisionKind', 'decision_kind']);
			const tool = readString(p, ['Tool', 'tool']);
			if (kind === 'CallTool' && tool !== '') {
				resolveToolRow(turn, tool, 'invoked', '');
			}
			// Reconstruct the ordered reasoning step. The runloop appends a
			// `traj.Step` ONLY for step-appending decisions, and the enricher
			// indexes reasoning over those steps; `Finish` / `RequestPause`
			// emit a `planner.decision` event (often with reasoning) but append
			// NO step, so they must advance neither the ordinal nor the
			// emission — folding them would phantom-add a step the live view
			// never shows and shift every later index (the one correctness
			// subtlety). The `ReasoningTrace` bytes are the raw provider
			// reasoning: `DecisionPayload` is a `SafePayload`, so the bus skips
			// the redactor — byte-identical to the live enricher's in-memory
			// value.
			if (STEP_APPENDING_DECISION_KINDS.has(kind)) {
				const index = stepOrdinal.get(runID) ?? 0;
				const trace = readString(p, ['ReasoningTrace', 'reasoning_trace']);
				if (trace !== '') {
					turn.reasoningSteps.push({ index, reasoning_trace: trace });
				}
				stepOrdinal.set(runID, index + 1);
			}
		} else if (ev.type === 'tool.completed') {
			const tool = readString(p, ['ToolName', 'tool_name']);
			const ms = readNumber(p, ['DurationMS', 'duration_ms']);
			const summary = ms > 0 ? `${(ms / 1000).toFixed(1)}s` : '';
			if (tool !== '') resolveToolRow(turn, tool, 'succeeded', summary);
		} else if (ev.type === 'tool.failed' || ev.type === 'tool.policy_exhausted') {
			const tool = readString(p, ['ToolName', 'tool_name']);
			const cls = readString(p, ['ErrorClass', 'error_class', 'LastClass', 'last_class']);
			const msg = readString(p, ['ErrorMessage', 'error_message', 'LastError', 'last_error']);
			const summary = msg !== '' ? (cls !== '' ? `${cls}: ${msg}` : msg) : cls !== '' ? cls : 'failed';
			if (tool !== '') resolveToolRow(turn, tool, 'failed', summary);
		} else if (ev.type === 'mcp.app_available') {
			// An MCP tool result on this turn declared an interactive `ui://`
			// App. Reconstruct the SAME `MCPAppRefView` the live discovery path
			// builds so the renderer mounts a replayed App exactly as it mounts
			// a live one. `serverID` + `resourceUri` are both load-bearing (the
			// renderer fetches the document via `readResource(serverID, uri)`),
			// so a frame missing either declares no App at all — dropped here
			// for the SAME reason the live decoder drops it, rather than
			// hydrating a ref that could only mount broken.
			const serverID = readString(p, ['ServerID', 'server_id']);
			const resourceUri = readString(p, ['ResourceURI', 'resource_uri']);
			if (serverID !== '' && resourceUri !== '') {
				const mode = readString(p, ['DisplayMode', 'display_mode']);
				const agentID = readString(p, ['AgentID', 'agent_id']);
				turn.app = {
					...(agentID !== '' ? { agentId: agentID } : {}),
					resourceUri,
					displayMode: KNOWN_DISPLAY_MODES.has(mode) ? (mode as McpUiDisplayMode) : '',
					rawHtmlTrusted: readBool(p, ['RawHTMLTrusted', 'raw_html_trusted']),
					// The deterministic content-hash correlation key the renderer
					// pairs with `serverID` to read the PERSISTED tool context
					// (`mcp.apps.tool_context`). Nothing new is stored for the
					// replay — the runtime already persisted it under this id.
					toolCallId: readString(p, ['ToolCallID', 'tool_call_id']),
					// Durable replay is read-only. A persisted discovery must never
					// rehydrate live callback authority.
					// The server-side tool name that declared the app — display
					// metadata the host projects onto the `ui/initialize`
					// host-context `toolInfo`. Read here for the same reason the
					// live decoder reads it: the two projections must produce
					// identical refs, and a one-sided omission is the drift the
					// paired spec exists to catch.
					toolName: readString(p, ['ToolName', 'tool_name'])
				};
				turn.serverID = serverID;
			}
		} else if (ev.type === 'task.started') {
			started.set(runID, ev.occurred_at);
		}

		if (TERMINAL_TASK_TYPES.has(ev.type)) {
			turn.terminal = true;
			const startAt = started.get(runID);
			if (startAt !== undefined && turn.durationMs === 0) {
				const ms = Date.parse(ev.occurred_at) - Date.parse(startAt);
				if (Number.isFinite(ms) && ms > 0) turn.durationMs = ms;
			}
		}
	}

	return order.map((id) => byRun.get(id)!).filter((t): t is HistoryTurn => t !== undefined);
}
