package types

import "time"

// Session-turns wire types — the canonical contract for the
// `sessions.turns.list` / `sessions.turns.get` read surface (the turn
// projection the v1.28 Protocol pages over). The wire shapes are FLAT:
// they mirror the domain projection (`internal/sessions/turns`) as
// self-contained JSON types, so the Console never reads a runtime Go
// struct (RFC §5.1 / CLAUDE.md §13 single-source rule).
//
// Two methods consume these types:
//
//   - sessions.turns.list — SessionTurnsListRequest →
//     SessionTurnsListResponse. One newest-first keyset page of the
//     caller's EXACT session's conversation projection (the consumer
//     lane). The operations projection is GET-ONLY and rejected here.
//   - sessions.turns.get — SessionTurnsGetRequest →
//     SessionTurnsGetResponse. One (session, task) read on either the
//     consumer conversation lane (exact-session, effective-agent-gated)
//     or — under a verified admin OR console:fleet claim — the
//     structurally distinct operations DTO lane (SessionOpsTurnRow).
//
// The operations DTO is a DISTINCT structural type: it omits the query,
// the answer (inline and reference), reasoning summaries, the App
// resource URI and tool_call_id, App context / input / result, and
// pause tokens. A widened operations read emits one
// `audit.admin_scope_used` event before the projection read.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): an incomplete IdentityScope fails closed at the wire edge
// with CodeIdentityRequired. The consumer lane is exact-session; a
// foreign-session read answers typed not-found (non-oracular).

// SessionTurnsListRequest is the `sessions.turns.list` request body.
// The identity triple is verified from the request context; the body
// carries the session id, the opaque older-page cursor, the bounded
// limit, and the projection selector (only the default conversation
// projection is a list surface).
type SessionTurnsListRequest struct {
	// Identity is the (tenant, user, session) scope the read runs under.
	// Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// SessionID is the effective session to page. On the consumer lane
	// it must be the caller's own exact session; anything else answers
	// typed not-found (non-oracular).
	SessionID string `json:"session_id"`
	// OlderCursor is the opaque exclusive older-page cursor returned by
	// the previous page; empty means the newest page. A malformed /
	// foreign / stale-snapshot / retention-expired cursor surfaces as
	// its distinct typed refusal.
	OlderCursor string `json:"older_cursor,omitempty"`
	// Limit bounds the page: 0 means the Protocol-mandated default (20);
	// values above the maximum (50) fail loud.
	Limit int `json:"limit,omitempty"`
	// Projection selects the read lane. Only the conversation projection
	// is a list surface; "operations" is rejected here (get-only).
	Projection string `json:"projection,omitempty"`
}

// SessionTurnHeader is the lightweight per-session header the list
// response carries: the owning session id, the projection snapshot id
// (as-of retention generation) the page — and its cursors — bind to,
// and the as-of instant the page was read.
type SessionTurnHeader struct {
	// SessionID is the session the page belongs to.
	SessionID string `json:"session_id"`
	// SnapshotID is the projection snapshot generation the page binds
	// to; erasure advances it, and a cursor minted against an older
	// generation is rejected as stale.
	SnapshotID uint64 `json:"snapshot_id"`
	// AsOf is the instant the page was read.
	AsOf time.Time `json:"as_of"`
}

// SessionTurnsListResponse is the `sessions.turns.list` response — one
// newest-first page of consumer turn rows, the paging contract fields,
// and the explicit page-completeness (complete, or partial with the
// retention-eviction reason — never a fabricated empty).
type SessionTurnsListResponse struct {
	// Header is the session header: session id, snapshot id, as-of.
	Header SessionTurnHeader `json:"header"`
	// Turns are the page's turns, newest first, already run through the
	// effective-agent gate (gated-out rows are not present).
	Turns []SessionTurnRow `json:"turns"`
	// Order is the declared stable order — always "newest_first".
	Order string `json:"order"`
	// NextOlderCursor is the opaque exclusive older-page cursor for the
	// next page; empty when HasMore is false. The client passes it back
	// verbatim as the next request's OlderCursor.
	NextOlderCursor string `json:"next_older_cursor,omitempty"`
	// HasMore reports whether older turns remain within the retained
	// window beyond this page.
	HasMore bool `json:"has_more"`
	// RemainingOlderCount is the exact number of older RETAINED turns
	// beyond this page when the store knows it exactly and no row of
	// this page was gated by the effective-agent filter; omitted
	// otherwise (unknown — never a fabricated count).
	RemainingOlderCount *int `json:"remaining_older_count,omitempty"`
	// CountExact reports whether RemainingOlderCount is exact.
	CountExact bool `json:"count_exact"`
	// LiveResumeSeq is the durable event-log sequence of the newest
	// observation reflected in this page — the exclusive live-resume
	// cursor. A consumer subscribes to the session's event stream from
	// LiveResumeSeq+1 (subscribe-before-page) for a gap-free
	// page-to-live handoff.
	LiveResumeSeq uint64 `json:"live_resume_seq"`
	// PageCompleteness is the explicit page completeness: "complete", or
	// "partial" (retention eviction — older turns exist in the durable
	// event log but were evicted from this projection). Never a
	// fabricated empty.
	PageCompleteness string `json:"page_completeness"`
	// PartialReason names why the page is partial ("retention_eviction");
	// empty when PageCompleteness is complete.
	PartialReason string `json:"partial_reason,omitempty"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under.
	ProtocolVersion string `json:"protocol_version"`
}

// SessionTurnsGetRequest is the `sessions.turns.get` request body: one
// (session, task) read. The identity triple is verified from the request
// context; the body carries the session id, the task (turn) id, and the
// projection selector.
type SessionTurnsGetRequest struct {
	// Identity is the (tenant, user, session) scope the read runs under.
	// Mandatory — an incomplete triple fails closed.
	Identity IdentityScope `json:"identity"`
	// SessionID is the effective session. On the consumer lane it must
	// be the caller's own exact session; on the operations lane it is
	// any session of the caller's own (tenant, user).
	SessionID string `json:"session_id"`
	// TaskID is the authoritative root foreground task id of the turn —
	// the turn row key.
	TaskID string `json:"task_id"`
	// Projection selects the read lane: "conversation" (default) or
	// "operations" (admin/fleet-gated, returns the operations DTO).
	Projection string `json:"projection,omitempty"`
}

// SessionTurnsGetResponse is the `sessions.turns.get` response. Exactly
// one of Turn / OpsTurn is populated, per the request's projection:
// Turn for the consumer conversation lane, OpsTurn for the elevated
// operations lane.
type SessionTurnsGetResponse struct {
	// SessionID is the session the turn was read from.
	SessionID string `json:"session_id"`
	// Turn is the consumer-safe turn row (conversation projection).
	// Nil on the operations lane.
	Turn *SessionTurnRow `json:"turn,omitempty"`
	// OpsTurn is the structurally distinct operations DTO (operations
	// projection) — no query / answer / reasoning / App URI /
	// tool_call_id / App context / pause tokens. Nil on the consumer
	// lane.
	OpsTurn *SessionOpsTurnRow `json:"ops_turn,omitempty"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under.
	ProtocolVersion string `json:"protocol_version"`
}

// SessionTurnRow is the flat wire projection of one consumer
// conversation turn — the durable, consumer-safe read model a caller
// renders. Component availability is honest per component (a component
// that was not reported is unavailable, never a fabricated zero).
type SessionTurnRow struct {
	// TurnID is the row key — the root foreground run's task id.
	TurnID string `json:"turn_id"`
	// TaskID is the AUTHORITATIVE root foreground task id of the run the
	// row projects.
	TaskID string `json:"task_id"`
	// RunID is the ACTUAL runtime run id (empty = unavailable, never
	// silently equated with TaskID or TurnID).
	RunID string `json:"run_id,omitempty"`
	// SessionID is the owning session.
	SessionID string `json:"session_id"`
	// Sequence is the immutable per-session order key.
	Sequence int64 `json:"sequence"`
	// TieBreaker is the immutable secondary order key (the TurnID).
	TieBreaker string `json:"tie_breaker"`
	// Status is the lifecycle state: pending | running | paused |
	// complete | failed | cancelled.
	Status string `json:"status"`
	// Sealed is true exactly when Status is terminal and the row has
	// been sealed.
	Sealed bool `json:"sealed"`
	// Version is the row's write generation — the consistency anchor of
	// the accumulated snapshots.
	Version int `json:"version"`
	// LastAppliedEventSeq is the durable event-log sequence of the most
	// recent observation applied to this row (0 when none was recorded).
	LastAppliedEventSeq uint64 `json:"last_applied_event_seq"`
	// StartedAt / UpdatedAt / FinishedAt are the run's timing.
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// FinishReason is the CLOSED terminal finish reason (goal | no_path
	// | cancelled | deadline_exceeded | constraints_conflict); empty
	// while mutable and empty when the runtime reported none.
	FinishReason string `json:"finish_reason,omitempty"`
	// ErrorClass is the CLOSED content-free terminal error class;
	// empty unless the run failed.
	ErrorClass string `json:"error_class,omitempty"`
	// FinishMessage / ErrorMessage are the bounded redacted
	// consumer-safe terminal messages ("" = none available).
	FinishMessage string `json:"finish_message,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	// Agent is the agent binding (id / name / provenance / availability).
	Agent SessionTurnAgent `json:"agent"`
	// Query is the renderable user query with the instant it was made.
	Query SessionTurnQuery `json:"query"`
	// Answer is the assistant answer — the closed union of inline text,
	// artifact reference, definite empty, evicted reference, or
	// unavailable.
	Answer SessionTurnAnswer `json:"answer"`
	// Pause is the durable pause component (class / reason / lifecycle /
	// availability). It NEVER carries a pause/resume/approval token.
	Pause SessionTurnPause `json:"pause"`
	// Inputs / Outputs are the operator-attached attachment metadata
	// (never bytes).
	Inputs  []SessionTurnAttachment `json:"inputs,omitempty"`
	Outputs []SessionTurnAttachment `json:"outputs,omitempty"`
	// Usage is the cumulative per-measure honest token/cost/latency
	// rollup (cost is exact integer micro-units of USD).
	Usage SessionTurnUsage `json:"usage"`
	// Reasoning is the bounded ordered DERIVED reasoning summary — steps
	// carry a closed kind only; raw provider thinking never enters the
	// row.
	Reasoning SessionTurnReasoning `json:"reasoning"`
	// Activity is the bounded content-free activity (tool dispatch)
	// window plus the exact turn-level totals.
	Activity SessionTurnActivity `json:"activity"`
	// Apps is the turn's ORDERED collection of interactive MCP App
	// references, with component availability.
	Apps []SessionTurnAppRef `json:"apps,omitempty"`
}

// SessionTurnAgent is the agent binding component.
type SessionTurnAgent struct {
	// ID is the registered agent id the run executed under (empty when
	// none was bound).
	ID string `json:"id,omitempty"`
	// Name is the agent display name when known.
	Name string `json:"name,omitempty"`
	// BindingSource is the honest provenance: explicit | defaulted |
	// unknown.
	BindingSource string `json:"binding_source,omitempty"`
	// Complete is the honest state: "complete" when the runtime supplied
	// a binding, "unavailable" when it did not.
	Complete string `json:"complete,omitempty"`
}

// SessionTurnQuery is the renderable user query component.
type SessionTurnQuery struct {
	// Text is the rendered query text (markdown-safe display text). Never
	// the raw transcript.
	Text string `json:"text,omitempty"`
	// At is the instant the query / input was made.
	At time.Time `json:"at,omitempty"`
	// Complete is "complete" when a query was supplied, "unavailable"
	// when the run had none.
	Complete string `json:"complete,omitempty"`
}

// SessionTurnAnswer is the assistant answer component. State is the
// closed union: inline | artifact_ref | empty | evicted | unavailable.
type SessionTurnAnswer struct {
	// State is the closed union.
	State string `json:"state,omitempty"`
	// Inline carries the answer text when State is "inline" ("" is a
	// legitimate complete inline answer).
	Inline string `json:"inline,omitempty"`
	// Ref carries the artifact reference when State is "artifact_ref".
	Ref *SessionTurnAnswerRef `json:"ref,omitempty"`
	// Seq is the durable event-log sequence of the observation that
	// produced this accumulated answer snapshot (0 when none was
	// recorded).
	Seq uint64 `json:"seq"`
	// Complete is the derived honesty: "complete" for inline /
	// artifact_ref / empty; "unavailable" for evicted / unavailable.
	Complete string `json:"complete,omitempty"`
}

// SessionTurnAnswerRef is the metadata-only reference to a heavy answer
// routed through the artifact store. Bytes are fetched by id through the
// artifact surface; the projection carries metadata only.
type SessionTurnAnswerRef struct {
	// ID is the content-addressed artifact identifier.
	ID string `json:"id"`
	// MimeType is the stored answer's MIME type.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the stored byte length.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Filename is display metadata only.
	Filename string `json:"filename,omitempty"`
	// SHA256 is the full hex digest.
	SHA256 string `json:"sha256,omitempty"`
}

// SessionTurnPause is the durable pause component: the class / reason /
// lifecycle of the current pause episode plus its component
// availability. It NEVER carries a pause / resume / approval token.
type SessionTurnPause struct {
	// Class is the pause producer class (hitl_approval, oauth,
	// a2a_auth_required, a2a_input_required, steering, operator).
	Class string `json:"class,omitempty"`
	// Reason is a short content-free reason string.
	Reason string `json:"reason,omitempty"`
	// Lifecycle is the episode's durable lifecycle (requested, active,
	// resolved).
	Lifecycle string `json:"lifecycle,omitempty"`
	// Availability is "complete" while a pause source is reporting (the
	// row is paused), "unavailable" when no pause episode is recorded.
	Availability string `json:"availability,omitempty"`
}

// SessionTurnAttachment is input / output attachment METADATA — never
// bytes. The content lives in the artifact store under ID.
type SessionTurnAttachment struct {
	// ID is the content-addressed artifact identifier.
	ID string `json:"id"`
	// Filename is display metadata only.
	Filename string `json:"filename,omitempty"`
	// MimeType is the attachment's MIME type.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the byte length of the stored content.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// SHA256 is the full hex digest.
	SHA256 string `json:"sha256,omitempty"`
	// Disposition is the per-attachment input hint (ref / inline /
	// provider_native / tool:<name>); empty on output attachments.
	Disposition string `json:"disposition,omitempty"`
	// Availability is "complete" when the referenced content resolves,
	// "unavailable" when it cannot be fetched.
	Availability string `json:"availability,omitempty"`
}

// SessionTurnUsage is the cumulative per-measure honest token / cost /
// latency / model rollup. Honesty is PER MEASURE: an absent measure is
// "unavailable" with NO value (never a fabricated zero). Cost is exact
// integer micro-dollars (1e-6 USD) — never float64.
type SessionTurnUsage struct {
	// PromptTokens / CompletionTokens / ReasoningTokens /
	// CacheReadTokens / CacheWriteTokens / TotalTokens / CostMicroUSD /
	// LatencyNS are the cumulative per-measure amounts.
	PromptTokens     SessionTurnUsageMeasure `json:"prompt_tokens"`
	CompletionTokens SessionTurnUsageMeasure `json:"completion_tokens"`
	ReasoningTokens  SessionTurnUsageMeasure `json:"reasoning_tokens"`
	CacheReadTokens  SessionTurnUsageMeasure `json:"cache_read_tokens"`
	CacheWriteTokens SessionTurnUsageMeasure `json:"cache_write_tokens"`
	TotalTokens      SessionTurnUsageMeasure `json:"total_tokens"`
	CostMicroUSD     SessionTurnUsageMeasure `json:"cost_micro_usd"`
	LatencyNS        SessionTurnUsageMeasure `json:"latency_ns"`
	// Model is the model identifier last reported; empty means
	// unavailable (no model reported).
	Model string `json:"model,omitempty"`
}

// SessionTurnUsageMeasure is ONE cumulative usage measure: a closed
// availability / exactness state (unavailable | exact | estimated) plus
// the exact integer amount in the measure's unit. Value is omitted
// exactly when the measure is unavailable — never a fabricated zero.
type SessionTurnUsageMeasure struct {
	// State is unavailable | exact | estimated.
	State string `json:"state,omitempty"`
	// Value is the exact integer amount (omitted iff State is
	// unavailable).
	Value *int64 `json:"value,omitempty"`
}

// SessionTurnReasoning is the bounded ORDERED reasoning component — a
// DERIVED, consumer-safe summary of the run's trajectory thinking.
// Steps carry a closed ReasoningKind only; the projection is
// structurally unable to carry raw provider thinking.
type SessionTurnReasoning struct {
	// Steps are the retained ordered steps (at most the protocol bound,
	// the FIRST of the fed sequence).
	Steps []SessionTurnReasoningStep `json:"steps,omitempty"`
	// Complete is "complete" when every fed step was retained,
	// "partial" when the fed sequence exceeded the bound (see Dropped),
	// "unavailable" when no reasoning source was wired.
	Complete string `json:"complete,omitempty"`
	// Dropped is the number of fed steps NOT retained (overflow beyond
	// the bound). Zero unless Complete is "partial".
	Dropped int `json:"dropped,omitempty"`
	// Seq is the durable event-log sequence of the observation that
	// produced this accumulated reasoning snapshot (0 when none was
	// recorded).
	Seq uint64 `json:"seq"`
}

// SessionTurnReasoningStep is one DERIVED reasoning step summary at one
// trajectory position: its immutable index and its closed kind
// (tool_call | spawn | await). It deliberately has NO text field — raw
// provider thinking cannot be represented in the row.
type SessionTurnReasoningStep struct {
	// Index is the step's 0-based position in the run's trajectory step
	// sequence (positions are kept, gaps allowed).
	Index int `json:"index"`
	// Kind is the closed derived step class.
	Kind string `json:"kind,omitempty"`
}

// SessionTurnActivity is the bounded content-free activity component:
// one row per tool dispatch, oldest first as fed. The inline window is
// the recent window; the exact turn-level totals survive window
// truncation.
type SessionTurnActivity struct {
	// Rows are the retained rows (at most the configured inline activity
	// limit, the LAST of the fed sequence).
	Rows []SessionTurnActivityRow `json:"rows,omitempty"`
	// Complete is "complete" when every fed row was retained, "partial"
	// when the fed sequence exceeded the inline activity limit.
	Complete string `json:"complete,omitempty"`
	// More is the EXPLICIT LOWER-BOUND marker: true when activity rows
	// older than the retained window exist.
	More bool `json:"more"`
	// Dropped is the number of fed rows NOT retained (the oldest ones).
	// Zero unless More is true.
	Dropped int `json:"dropped,omitempty"`
	// Totals are the compact EXACT counts across the FULL fed activity.
	Totals SessionTurnActivityTotals `json:"totals"`
}

// SessionTurnActivityTotals are the compact EXACT per-status counts
// across the full fed activity of the turn.
type SessionTurnActivityTotals struct {
	// Invoked is the count of dispatch rows currently in flight on their
	// first attempt.
	Invoked int64 `json:"invoked"`
	// Succeeded is the count of dispatch rows that completed
	// successfully.
	Succeeded int64 `json:"succeeded"`
	// Failed is the count of dispatch rows that failed terminally
	// (excluding policy exhaustion).
	Failed int64 `json:"failed"`
	// Cancelled is the count of dispatch rows cancelled.
	Cancelled int64 `json:"cancelled"`
	// Retried is the count of dispatch rows currently in flight on a
	// retry attempt.
	Retried int64 `json:"retried"`
	// PolicyExhausted is the count of dispatch rows whose retry policy
	// budget was exhausted.
	PolicyExhausted int64 `json:"policy_exhausted"`
}

// SessionTurnActivityRow is one content-free tool dispatch row. It NEVER
// carries raw arguments or results — only the bounded content-free
// lifecycle and retry data.
type SessionTurnActivityRow struct {
	// Position is the row's IMMUTABLE 0-based ordinal in the turn's
	// cumulative tool-dispatch sequence.
	Position int `json:"position"`
	// InvocationID is the stable invocation / tool-call identity (never
	// a tool argument); empty when the runtime reported none.
	InvocationID string `json:"invocation_id,omitempty"`
	// Tool is the invoked tool name.
	Tool string `json:"tool"`
	// StepSequence is the planner step / event sequence that produced
	// this dispatch when known; 0 means not recorded.
	StepSequence uint64 `json:"step_sequence"`
	// BatchID is the batch / group correlation id when known.
	BatchID string `json:"batch_id,omitempty"`
	// Status is the dispatch's lifecycle (invoked | retried | succeeded
	// | failed | cancelled | policy_exhausted).
	Status string `json:"status"`
	// TerminalClass is the DERIVED closed terminal classification (none
	// while in flight).
	TerminalClass string `json:"terminal_class,omitempty"`
	// StartedAt / FinishedAt are the dispatch timing instants.
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Duration is the dispatch duration when reported (0 = not reported).
	Duration time.Duration `json:"duration,omitempty"`
	// AttemptCount is the 1-based attempt number when reported (0 = not
	// reported).
	AttemptCount int `json:"attempt_count,omitempty"`
	// Retryable reports whether the dispatch is retryable per the
	// runtime's retry policy.
	Retryable bool `json:"retryable"`
	// PolicyExhausted reports whether the dispatch's retry policy budget
	// was exhausted.
	PolicyExhausted bool `json:"policy_exhausted"`
	// Summary is the short content-free summary ("" while invoked) —
	// never raw arguments or results.
	Summary string `json:"summary,omitempty"`
}

// SessionTurnAppRef is one entry in the turn's ORDERED collection of
// interactive MCP App references, with component availability. ToolCallID
// is OPTIONAL correlation metadata — never authority, never rehydrated
// live on replay.
type SessionTurnAppRef struct {
	// EffectiveAgentID is the agent whose context the App reference is
	// scoped to.
	EffectiveAgentID string `json:"effective_agent_id,omitempty"`
	// ServerID is the MCP server (source id) hosting the App's `ui://`
	// document.
	ServerID string `json:"server_id"`
	// ResourceURI is the App's `ui://` document URI.
	ResourceURI string `json:"resource_uri"`
	// DisplayMode is the negotiated display mode (inline | fullscreen |
	// pip); empty means the server stated no preference.
	DisplayMode string `json:"display_mode,omitempty"`
	// RawHTMLTrusted reports whether the server's raw-HTML opt-in is in
	// force (default-deny posture).
	RawHTMLTrusted bool `json:"raw_html_trusted"`
	// ToolCallID is the OPTIONAL correlation metadata for the
	// identity-scoped mcp.apps.tool_context lazy context-delivery
	// channel.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the originating server-side tool name (display
	// metadata).
	ToolName string `json:"tool_name,omitempty"`
	// Availability is the component availability: available |
	// unavailable (replay) | degraded (missing dependency).
	Availability string `json:"availability,omitempty"`
	// Complete is the honest state of the reference itself.
	Complete string `json:"complete,omitempty"`
}

// SessionOpsTurnRow is the flat wire projection of the OPERATIONS-SAFE
// READ DTO of one turn — the structurally distinct type the elevated
// admin/fleet observation lane serves. It retains the lifecycle, the
// task/run identity, the agent binding, timing, per-measure usage/cost,
// content-free activity, component COUNTS, App availability summaries,
// and the pause class/reason/lifecycle/availability. It structurally
// omits the query, the answer (inline and reference), reasoning
// summaries, pause/resume/approval tokens, the App resource URI and
// tool_call_id, and App context / input / result.
type SessionOpsTurnRow struct {
	// TurnID is the row key.
	TurnID string `json:"turn_id"`
	// TaskID is the authoritative root foreground task id.
	TaskID string `json:"task_id"`
	// RunID is the actual runtime run id (empty = unavailable).
	RunID string `json:"run_id,omitempty"`
	// SessionID is the owning session.
	SessionID string `json:"session_id"`
	// Sequence / TieBreaker are the immutable ordering keys.
	Sequence   int64  `json:"sequence"`
	TieBreaker string `json:"tie_breaker"`
	// Status / Sealed / Version are the lifecycle and its write
	// generation.
	Status  string `json:"status"`
	Sealed  bool   `json:"sealed"`
	Version int    `json:"version"`
	// StartedAt / UpdatedAt / FinishedAt are the run's timing.
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// FinishReason / ErrorClass are the CLOSED content-free terminal
	// descriptors; FinishMessage / ErrorMessage are the bounded redacted
	// terminal messages when available.
	FinishReason  string `json:"finish_reason,omitempty"`
	ErrorClass    string `json:"error_class,omitempty"`
	FinishMessage string `json:"finish_message,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	// AgentID / AgentName / AgentBindingSource are the agent binding.
	AgentID            string `json:"agent_id,omitempty"`
	AgentName          string `json:"agent_name,omitempty"`
	AgentBindingSource string `json:"agent_binding_source,omitempty"`
	// Usage is the cumulative per-measure honest token / cost rollup.
	Usage SessionTurnUsage `json:"usage"`
	// Activity is the retained content-free activity window plus the
	// exact turn-level totals.
	Activity SessionTurnActivity `json:"activity"`
	// ReasoningSteps is the COUNT of retained derived reasoning steps
	// (never the steps themselves).
	ReasoningSteps int `json:"reasoning_steps"`
	// Inputs / Outputs are the attachment COUNTS (never the metadata).
	Inputs  int `json:"inputs"`
	Outputs int `json:"outputs"`
	// Apps are the App availability summaries — effective agent id,
	// server id, tool name, availability; structurally no resource URI
	// and no tool_call_id.
	Apps []SessionOpsAppRef `json:"apps,omitempty"`
	// Pause is the pause class / reason / lifecycle / availability
	// (structurally no tokens).
	Pause SessionTurnPause `json:"pause"`
	// LastAppliedEventSeq is the row's last-applied event sequence.
	LastAppliedEventSeq uint64 `json:"last_applied_event_seq"`
}

// SessionOpsAppRef is the operations-safe App summary: the identity of
// the App (effective agent id, server id), its originating tool name,
// and its availability. It structurally omits the resource URI, the
// tool_call_id, and any App context / input / result.
type SessionOpsAppRef struct {
	// EffectiveAgentID is the agent the App context is scoped to.
	EffectiveAgentID string `json:"effective_agent_id,omitempty"`
	// ServerID is the MCP server hosting the App's document.
	ServerID string `json:"server_id"`
	// ToolName is the originating server-side tool name.
	ToolName string `json:"tool_name,omitempty"`
	// Availability is the component availability.
	Availability string `json:"availability,omitempty"`
}
