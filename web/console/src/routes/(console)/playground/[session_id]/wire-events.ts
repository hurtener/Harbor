// Playground — SSE wire-event decoders (Phase 108 follow-up / D-167).
//
// The `events.subscribe` SSE wire (`internal/protocol/transports/stream/
// frame.go`) projects each event as a flat `wireEvent`:
//
//   { type, sequence, occurred_at, tenant, user, session, run, payload }
//
// The per-event typed payload is nested under `payload` and marshalled
// WITHOUT json tags — so its keys are the Go struct field names in
// **PascalCase** (`payload.TaskID`, `payload.Delta`, `payload.Usage.
// TotalTokens`), NOT the snake_case json tags the request/response RPC
// bodies use. The first Playground streaming cut read top-level
// snake_case fields (`parsed.task_id` / `parsed.delta`) which are always
// `undefined` on this wire — so every `llm.completion.chunk` was
// silently dropped and the chat never streamed.
//
// These decoders are the single place that knows the wire shape. They
// are pure (string in → typed value | null out) so the grammar is
// unit-tested without an EventSource. A frame that does not match the
// expected shape decodes to `null` and the caller ignores it (no
// silent-degradation of REAL data — only genuinely-irrelevant frames
// are dropped).

/** The flat SSE frame envelope shared by every event type. */
interface WireFrame {
	type?: string;
	run?: string;
	payload?: Record<string, unknown>;
}

/** A decoded `llm.completion.chunk` — one streamed token delta. */
export interface ChunkEvent {
	taskID: string;
	delta: string;
	done: boolean;
	/** `content` (answer text) or `reasoning` (thinking trace). */
	kind: 'content' | 'reasoning';
}

/** A decoded `llm.cost.recorded` — per-LLM-call usage + cost rollup. */
export interface CostEvent {
	taskID: string;
	/** The provider/model string (e.g. `anthropic/claude-haiku-4.5`). */
	model: string;
	totalTokens: number;
	promptTokens: number;
	outputTokens: number;
	usd: number;
	/** The model's input-token window (0 when not configured). */
	contextWindow: number;
}

/** A decoded task-lifecycle terminal event. */
export interface LifecycleEvent {
	taskID: string;
	/** Normalised terminal kind. */
	kind: 'completed' | 'failed' | 'cancelled';
}

/** A decoded `governance.budget_exceeded` — the cost ceiling + spend. */
export interface BudgetEvent {
	ceilingUSD: number;
	totalCostUSD: number;
}

/** A decoded `planner.decision` — one ReAct step's decision. */
export interface PlannerDecisionEvent {
	taskID: string;
	/** `CallTool` / `Finish` / … */
	decisionKind: string;
	/** The tool name when `decisionKind === 'CallTool'`; '' otherwise. */
	tool: string;
}

/**
 * A decoded pending-intervention request — one of the three runtime
 * events that park a run awaiting an operator decision. The payload
 * shapes are the unified-pause family: `tool.approval_requested`
 * (`internal/tools/approval/events.go`), `tool.auth_required`
 * (`internal/tools/auth/events.go`), `pause.requested`
 * (`internal/runtime/pauseresume/events.go`). None of those payloads
 * carry a TaskID/RunID field — the run is on the frame envelope's
 * top-level `run`, which is the correlation key the Console's
 * approve/reject calls target.
 */
export interface InterventionEvent {
	runID: string;
	/** Operator-facing one-line summary of what is being asked. */
	reason: string;
	/** The source event that created this intervention. */
	source: 'tool.approval_requested' | 'tool.auth_required' | 'pause.requested';
}

function parseFrame(data: string): WireFrame | null {
	try {
		const v = JSON.parse(data) as unknown;
		if (typeof v !== 'object' || v === null) return null;
		return v as WireFrame;
	} catch {
		return null;
	}
}

function num(v: unknown): number {
	return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function str(v: unknown): string {
	return typeof v === 'string' ? v : '';
}

/**
 * Resolve the task id a payload pins. The chunk / cost payloads carry
 * `payload.TaskID`; the lifecycle payloads carry `payload.TaskID` too;
 * the frame's top-level `run` is the run id, which equals the task id
 * for a foreground Playground turn — used as the fallback.
 */
function taskIDOf(frame: WireFrame): string {
	const p = frame.payload ?? {};
	return str(p.TaskID) || str(frame.run);
}

/** Decode an `llm.completion.chunk` frame. Returns null if not one. */
export function decodeChunk(data: string): ChunkEvent | null {
	const frame = parseFrame(data);
	if (frame === null || frame.payload === undefined) return null;
	const taskID = taskIDOf(frame);
	const delta = str(frame.payload.Delta);
	if (taskID === '') return null;
	const kind = str(frame.payload.Kind) === 'reasoning' ? 'reasoning' : 'content';
	return {
		taskID,
		delta,
		done: frame.payload.Done === true,
		kind
	};
}

/** Decode an `llm.cost.recorded` frame. Returns null if not one. */
export function decodeCost(data: string): CostEvent | null {
	const frame = parseFrame(data);
	if (frame === null || frame.payload === undefined) return null;
	const taskID = taskIDOf(frame);
	if (taskID === '') return null;
	const usage = (frame.payload.Usage ?? {}) as Record<string, unknown>;
	const cost = (frame.payload.Cost ?? {}) as Record<string, unknown>;
	return {
		taskID,
		model: str(frame.payload.Model),
		totalTokens: num(usage.TotalTokens),
		promptTokens: num(usage.PromptTokens),
		outputTokens: num(usage.CompletionTokens),
		usd: num(cost.TotalCost),
		contextWindow: num(frame.payload.ContextWindowTokens)
	};
}

const TERMINAL: Record<string, LifecycleEvent['kind']> = {
	'task.completed': 'completed',
	'task.failed': 'failed',
	'task.cancelled': 'cancelled'
};

/** Decode a terminal task-lifecycle frame. Returns null if not one. */
export function decodeLifecycle(data: string): LifecycleEvent | null {
	const frame = parseFrame(data);
	if (frame === null) return null;
	const kind = TERMINAL[str(frame.type)];
	if (kind === undefined) return null;
	const taskID = taskIDOf(frame);
	if (taskID === '') return null;
	return { taskID, kind };
}

/** Decode a `planner.decision` frame. Returns null if not one. */
export function decodePlannerDecision(data: string): PlannerDecisionEvent | null {
	const frame = parseFrame(data);
	if (frame === null || frame.payload === undefined) return null;
	if (str(frame.type) !== 'planner.decision') return null;
	const taskID = taskIDOf(frame);
	if (taskID === '') return null;
	return {
		taskID,
		decisionKind: str(frame.payload.DecisionKind),
		tool: str(frame.payload.Tool)
	};
}

const INTERVENTION_TYPES = new Set([
	'tool.approval_requested',
	'tool.auth_required',
	'pause.requested'
]);

/**
 * Decode one of the three intervention-request frames into a
 * `PendingIntervention`-shaped value. Returns null for any other frame.
 * The reason string is composed from the payload's meaningful fields
 * (the tool name, the OAuth source name, the canonical pause reason) so
 * the right-rail card reads as a human sentence, not an enum token.
 */
export function decodeIntervention(data: string): InterventionEvent | null {
	const frame = parseFrame(data);
	if (frame === null || frame.payload === undefined) return null;
	const type = str(frame.type);
	if (!INTERVENTION_TYPES.has(type)) return null;
	const runID = str(frame.run);
	if (runID === '') return null;
	const p = frame.payload;
	if (type === 'tool.approval_requested') {
		const tool = str(p.Tool);
		const why = str(p.Reason);
		const reason = why !== '' ? `Approve call to ${tool} — ${why}` : `Approve call to ${tool}`;
		return { runID, reason, source: 'tool.approval_requested' };
	}
	if (type === 'tool.auth_required') {
		const name = str(p.SourceName) || str(p.Source);
		return { runID, reason: `Connect ${name}`, source: 'tool.auth_required' };
	}
	// pause.requested
	return {
		runID,
		reason: str(p.Reason) || 'Paused — awaiting operator',
		source: 'pause.requested'
	};
}

const INTERVENTION_CLEAR_TYPES = new Set([
	'pause.resumed',
	'tool.approved',
	'tool.rejected',
	'tool.auth_completed'
]);

/**
 * Decode an intervention-clearing frame to the run id it resolves.
 * `pause.resumed` / `tool.approved` / `tool.rejected` /
 * `tool.auth_completed` all terminate a parked run; the Console drops
 * the matching pending intervention. Returns null for any other frame.
 */
export function decodeInterventionClear(data: string): string | null {
	const frame = parseFrame(data);
	if (frame === null) return null;
	if (!INTERVENTION_CLEAR_TYPES.has(str(frame.type))) return null;
	const runID = str(frame.run);
	return runID === '' ? null : runID;
}

/** Decode a `governance.budget_exceeded` frame. Returns null if not one. */
export function decodeBudget(data: string): BudgetEvent | null {
	const frame = parseFrame(data);
	if (frame === null || frame.payload === undefined) return null;
	if (str(frame.type) !== 'governance.budget_exceeded') return null;
	return {
		ceilingUSD: num(frame.payload.Ceiling),
		totalCostUSD: num(frame.payload.TotalCost)
	};
}
