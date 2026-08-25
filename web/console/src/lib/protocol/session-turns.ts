// Harbor Console — the `sessions.turns.*` wire types (HA-63/64).
//
// Hand-synced field-for-field from
// `internal/protocol/types/session_turns.go` (the Go-side single source,
// D-002). Two methods consume these:
//   - sessions.turns.list — SessionTurnsListRequest → SessionTurnsListResponse
//   - sessions.turns.get  — SessionTurnsGetRequest  → SessionTurnsGetResponse
//
// The routes are PINNED EXPLICITLY (`POST /v1/sessions/turns/{list,get}`)
// — nested session routes are never derived generically. The request
// bodies fold identity at the Transport choke point, so the TS request
// interfaces omit `identity` (the single sanctioned per-field omission).
//
// The operations DTO (`SessionOpsTurnRow`) and the consumer usage DTO
// (`SessionUsageTurnRow`) are STRUCTURALLY DISTINCT
// type: it omits the query, the answer (inline and reference), reasoning
// summaries, the App resource URI and tool_call_id, App context / input /
// result, and pause tokens. A widened operations read is gated + audited
// server-side.

/** One newest-first page of the caller's exact session (consumer lane). */
export interface SessionTurnsListRequest {
	/** The effective session to page — must be the caller's own exact session. */
	session_id: string;
	/** The opaque exclusive older-page cursor; empty means the newest page. */
	older_cursor?: string;
	/** Page bound: 0 means the Protocol default (20); above 50 fails loud. */
	limit?: number;
	/** The read lane — only the default conversation projection is a list
	 * surface; every other projection is rejected here (get-only). */
	projection?: string;
}

/** The per-session header: session id, snapshot generation, as-of. */
export interface SessionTurnHeader {
	session_id: string;
	/** The projection snapshot generation the page — and its cursors — bind to. */
	snapshot_id: number;
	/** The instant the page was read. */
	as_of: string;
}

/** `sessions.turns.list` response — one newest-first consumer page. */
export interface SessionTurnsListResponse {
	header: SessionTurnHeader;
	/** The page's turns, newest first, effective-agent-gated. */
	turns: SessionTurnRow[];
	/** The declared stable order — always "newest_first". */
	order: string;
	/** Opaque older-page cursor; empty when HasMore is false. */
	next_older_cursor?: string;
	/** Whether older turns remain within the retained window. */
	has_more: boolean;
	/** Exact count of older RETAINED turns when known exactly (nil otherwise —
	 * never a fabricated count). */
	remaining_older_count?: number | null;
	/** Whether RemainingOlderCount is exact. */
	count_exact: boolean;
	/** The durable event-log sequence of the newest observation reflected in
	 * this page — the exclusive live-resume cursor (subscribe from +1). */
	live_resume_seq: number;
	/** Explicit page completeness: "complete" or "partial" (retention
	 * eviction). Never a fabricated empty. */
	page_completeness: string;
	/** Why the page is partial ("retention_eviction"); empty when complete. */
	partial_reason?: string;
	protocol_version: string;
}

/** `sessions.turns.get` request — one (session, task) read. */
export interface SessionTurnsGetRequest {
	/** The effective session. Own exact session on the consumer lane; any
	 * session of the caller's own (tenant, user) on the operations lane. */
	session_id: string;
	/** The authoritative root foreground task id of the turn — the row key. */
	task_id: string;
	/** The read lane: "conversation" (default), "operations" (admin/fleet), or
	 * "usage" (consumer exact-session). */
	projection?: string;
}

/** `sessions.turns.get` response — exactly one of `turn` / `ops_turn` /
 * `usage_turn`. */
export interface SessionTurnsGetResponse {
	session_id: string;
	/** The consumer-safe turn row (conversation lane). */
	turn?: SessionTurnRow;
	/** The structurally distinct operations DTO (operations lane). */
	ops_turn?: SessionOpsTurnRow;
	/** The structurally distinct content-free usage DTO (usage lane). */
	usage_turn?: SessionUsageTurnRow;
	protocol_version: string;
}

/** A content-free consumer usage observation. This type deliberately has no
 * query, answer, reasoning, activity, pause, app, attachment, run, terminal
 * message, user, tenant, or content fields. */
export interface SessionUsageTurnRow {
	turn_id: string;
	task_id: string;
	session_id: string;
	agent_id?: string;
	status: string;
	sealed: boolean;
	version: number;
	last_applied_event_seq: number;
	started_at: string;
	updated_at: string;
	finished_at?: string;
	usage: SessionTurnUsage;
}

/** The agent binding component of a turn row. */
export interface SessionTurnAgent {
	id?: string;
	name?: string;
	/** Honest provenance: explicit | defaulted | unknown. */
	binding_source?: string;
	/** "complete" when a binding was supplied, "unavailable" otherwise. */
	complete?: string;
}

/** The renderable user query component. */
export interface SessionTurnQuery {
	/** Markdown-safe display text — never the raw transcript. */
	text?: string;
	at?: string;
	/** "complete" when a query was supplied, "unavailable" otherwise. */
	complete?: string;
}

/** The metadata-only reference to a heavy answer (bytes via artifacts). */
export interface SessionTurnAnswerRef {
	id: string;
	mime_type?: string;
	size_bytes?: number;
	filename?: string;
	sha256?: string;
}

/** The assistant answer — closed union of inline | artifact_ref | empty |
 * evicted | unavailable. */
export interface SessionTurnAnswer {
	state?: string;
	/** Answer text when State is "inline". */
	inline?: string;
	/** Artifact reference when State is "artifact_ref". */
	ref?: SessionTurnAnswerRef;
	/** The durable event-log sequence of the observation that produced this
	 * accumulated snapshot (0 when none was recorded). */
	seq: number;
	complete?: string;
}

/** The durable pause component — NEVER carries a pause/resume/approval token. */
export interface SessionTurnPause {
	class?: string;
	reason?: string;
	lifecycle?: string;
	availability?: string;
}

/** Input / output attachment METADATA — never bytes. */
export interface SessionTurnAttachment {
	id: string;
	filename?: string;
	mime_type?: string;
	size_bytes?: number;
	sha256?: string;
	disposition?: string;
	availability?: string;
}

/** One cumulative usage measure: a closed availability state plus the exact
 * integer amount. Value is omitted exactly when the measure is unavailable —
 * never a fabricated zero. */
export interface SessionTurnUsageMeasure {
	/** unavailable | exact | estimated. */
	state?: string;
	value?: number | null;
}

/** The cumulative per-measure honest token / cost / latency rollup. Cost is
 * exact integer micro-dollars (1e-6 USD) — never float64. */
export interface SessionTurnUsage {
	prompt_tokens: SessionTurnUsageMeasure;
	completion_tokens: SessionTurnUsageMeasure;
	reasoning_tokens: SessionTurnUsageMeasure;
	cache_read_tokens: SessionTurnUsageMeasure;
	cache_write_tokens: SessionTurnUsageMeasure;
	total_tokens: SessionTurnUsageMeasure;
	cost_micro_usd: SessionTurnUsageMeasure;
	latency_ns: SessionTurnUsageMeasure;
	/** Empty means unavailable (no model reported). */
	model?: string;
}

/** One DERIVED reasoning step — a closed kind only, no provider thinking. */
export interface SessionTurnReasoningStep {
	index: number;
	/** tool_call | spawn | await. */
	kind?: string;
}

/** The bounded ORDERED DERIVED reasoning summary. */
export interface SessionTurnReasoning {
	steps?: SessionTurnReasoningStep[];
	/** "complete" | "partial" (overflow dropped) | "unavailable". */
	complete?: string;
	/** Number of fed steps NOT retained (overflow). Zero unless partial. */
	dropped?: number;
	seq: number;
}

/** Compact EXACT per-status counts across the FULL fed activity. */
export interface SessionTurnActivityTotals {
	invoked: number;
	succeeded: number;
	failed: number;
	cancelled: number;
	retried: number;
	policy_exhausted: number;
}

/** One content-free tool dispatch row — never raw arguments or results. */
export interface SessionTurnActivityRow {
	position: number;
	invocation_id?: string;
	tool: string;
	step_sequence: number;
	batch_id?: string;
	status: string;
	terminal_class?: string;
	started_at?: string;
	finished_at?: string;
	duration?: number;
	attempt_count?: number;
	retryable: boolean;
	policy_exhausted: boolean;
	summary?: string;
}

/** The bounded content-free activity window + exact turn-level totals. */
export interface SessionTurnActivity {
	rows?: SessionTurnActivityRow[];
	/** "complete" | "partial" (inline window truncated). */
	complete?: string;
	/** Explicit lower-bound marker: older rows exist beyond the window. */
	more: boolean;
	/** Number of fed rows NOT retained (the oldest). Zero unless more. */
	dropped?: number;
	totals: SessionTurnActivityTotals;
}

/** One interactive MCP App reference in a turn (consumer lane). */
export interface SessionTurnAppRef {
	effective_agent_id?: string;
	server_id: string;
	/** The App's `ui://` document URI. */
	resource_uri: string;
	display_mode?: string;
	raw_html_trusted: boolean;
	/** OPTIONAL correlation metadata — never authority, never rehydrated. */
	tool_call_id?: string;
	tool_name?: string;
	/** available | unavailable (replay) | degraded (missing dependency). */
	availability?: string;
	complete?: string;
}

/** One consumer conversation turn — the durable, consumer-safe read model. */
export interface SessionTurnRow {
	turn_id: string;
	task_id: string;
	/** Empty = unavailable, never silently equated with task_id. */
	run_id?: string;
	session_id: string;
	sequence: number;
	tie_breaker: string;
	/** pending | running | paused | complete | failed | cancelled. */
	status: string;
	sealed: boolean;
	version: number;
	last_applied_event_seq: number;
	started_at: string;
	updated_at: string;
	finished_at?: string;
	/** goal | no_path | cancelled | deadline_exceeded | constraints_conflict. */
	finish_reason?: string;
	error_class?: string;
	finish_message?: string;
	error_message?: string;
	agent: SessionTurnAgent;
	query: SessionTurnQuery;
	answer: SessionTurnAnswer;
	pause: SessionTurnPause;
	inputs?: SessionTurnAttachment[];
	outputs?: SessionTurnAttachment[];
	usage: SessionTurnUsage;
	reasoning: SessionTurnReasoning;
	activity: SessionTurnActivity;
	apps?: SessionTurnAppRef[];
}

/** The operations-safe App summary — structurally no resource URI and no
 * tool_call_id. */
export interface SessionOpsAppRef {
	effective_agent_id?: string;
	server_id: string;
	tool_name?: string;
	availability?: string;
}

/** The OPERATIONS-SAFE READ DTO of one turn — the structurally distinct type
 * the elevated admin/fleet lane serves. It structurally omits the query, the
 * answer (inline and reference), reasoning summaries, pause/resume/approval
 * tokens, the App resource URI and tool_call_id, and App context. */
export interface SessionOpsTurnRow {
	turn_id: string;
	task_id: string;
	run_id?: string;
	session_id: string;
	sequence: number;
	tie_breaker: string;
	status: string;
	sealed: boolean;
	version: number;
	started_at: string;
	updated_at: string;
	finished_at?: string;
	finish_reason?: string;
	error_class?: string;
	finish_message?: string;
	error_message?: string;
	agent_id?: string;
	agent_name?: string;
	agent_binding_source?: string;
	usage: SessionTurnUsage;
	activity: SessionTurnActivity;
	/** The COUNT of retained derived reasoning steps (never the steps). */
	reasoning_steps: number;
	/** Attachment COUNTS (never the metadata). */
	inputs: number;
	outputs: number;
	apps?: SessionOpsAppRef[];
	pause: SessionTurnPause;
	last_applied_event_seq: number;
}
