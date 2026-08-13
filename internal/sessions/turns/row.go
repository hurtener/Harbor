package turns

import "time"

// This file holds the CONSUMER-SAFE READ DTO family (TurnRow and its
// component types) plus the pure, STRUCTURALLY DISTINCT OPERATIONS-SAFE
// READ DTO (OpsTurnRow). The mutation DTOs live in ops.go and are the
// write surface — they are NOT the operations read projection and do
// not satisfy any consumer-vs-operations authority matrix (see the
// package doc in turns.go).
//
// Every component carries a Completeness state, every list is bounded,
// and the consumer types deliberately carry only derived content
// (rendered query, inline or referenced answer, attachment metadata,
// lifecycle/agent, per-measure honest usage, bounded ordered DERIVED
// reasoning summaries — never raw provider thinking — bounded
// content-free activity with exact turn-level totals, an ordered App
// reference collection plus availability, and the durable pause
// component).
//
// Structurally absent from the CONSUMER family: raw provider thinking
// (no trace / chain-of-thought / transcript of any kind), raw tool
// arguments / results, and pause/resume/approval tokens. The App
// reference DOES carry an optional tool_call_id — as correlation
// metadata for the identity-scoped `mcp.apps.tool_context` lazy
// context-delivery channel, never as authority (an absent id still
// mounts the App without Data Delivery, and a replayed projection
// never rehydrates live callback authority).

// TurnRow is one projected conversation turn — the durable,
// consumer-safe read model a caller renders. Rows are keyed by
// (identity triple, TurnID); Sequence + TieBreaker are the immutable
// ordering keys newest-first paging pages over.
type TurnRow struct {
	// TurnID is the row key — the root foreground run's task id (the
	// runtime may derive it from TaskID). Unique within the session's
	// identity triple.
	TurnID TurnID
	// TaskID is the AUTHORITATIVE root foreground task id of the run
	// the row projects. Distinct from TurnID as a matter of contract
	// honesty: the row key may be TaskID-derived, but TaskID is the
	// authoritative task identity. Always populated on rows created by
	// the projector (derived from TurnID when the runtime did not
	// report one separately).
	TaskID string
	// RunID is the ACTUAL runtime run id of the root foreground run —
	// the per-execution scope inside the session (identity.Quadruple's
	// RunID), carried on the row so the projection never erases it.
	// EMPTY means the run id is UNAVAILABLE (a legacy record, or a
	// runtime that did not report one) — it is NEVER silently equated
	// with TaskID or TurnID.
	RunID string
	// SessionID is the owning session (the triple's SessionID),
	// denormalised onto the row for rendering.
	SessionID string
	// Sequence is the immutable per-session order key minted at
	// append. Never changes for the row's lifetime.
	Sequence Seq
	// TieBreaker is the immutable secondary order key (the TurnID).
	// Sequences are unique within a session; the tie-breaker makes the
	// keyset order total even against a driver that minted duplicate
	// sequences (defensive — see Cursor).
	TieBreaker TurnID
	// Status is the lifecycle state. Running/paused rows are MUTABLE
	// and versioned; terminal rows are SEALED and immutable.
	Status Status
	// Sealed is true exactly when Status is terminal and the row has
	// been sealed. Sealed rows never change.
	Sealed bool
	// Version is the row's write generation. Every accepted append /
	// update / seal bumps it by one; mutations carry the expected
	// version and are refused with ErrStaleVersion on mismatch.
	// Because each accepted write replaces the accumulated answer /
	// reasoning / activity snapshots atomically under one version
	// bump, Version is the consistency anchor of those snapshots.
	Version int
	// LastAppliedEventSeq is the durable event-log sequence of the
	// most recent observation applied to this row (0 when none was
	// recorded — e.g. a fresh append or a store fed directly). It
	// lets a consumer correlate the row's snapshot with the durable
	// event log and detect a stale render.
	LastAppliedEventSeq uint64
	// StartedAt is the run's start instant (the append time when the
	// runtime did not supply one).
	StartedAt time.Time
	// UpdatedAt is the instant of the most recent accepted write.
	UpdatedAt time.Time
	// FinishedAt is the terminal instant; zero while the row is
	// mutable.
	FinishedAt time.Time
	// FinishReason is the CLOSED terminal finish reason (a stable
	// low-cardinality code — see FinishReason); empty while mutable and
	// empty on a terminal row when the runtime reported none (honest
	// "not reported", never fabricated).
	FinishReason FinishReason
	// ErrorClass is the CLOSED content-free terminal error class of a
	// failed run (a stable low-cardinality code — see ErrorClass;
	// never an error message, and never caller content). Empty unless
	// the run failed.
	ErrorClass ErrorClass
	// FinishMessage is the bounded redacted consumer-safe finish
	// message when the runtime supplied one ("" = none available). The
	// runtime MUST pass already-redacted text; the projection bounds
	// it (MaxTerminalMessageRunes) and refuses over-bound or
	// control-laden input loudly.
	FinishMessage string
	// ErrorMessage is the bounded redacted consumer-safe error message
	// when the runtime supplied one ("" = none available). Same
	// redaction obligation and bound as FinishMessage.
	ErrorMessage string

	// Agent is the agent binding the run executed under, with the
	// honest provenance of that binding. Unavailable when the runtime
	// supplied no binding.
	Agent Agent
	// Query is the renderable user query with the instant it was
	// made. Unavailable when the run had no user-visible query (e.g.
	// an injected-context run).
	Query Query
	// Answer is the assistant answer — a closed union of inline text,
	// artifact reference, definite empty, evicted reference, or
	// unavailable. Unavailable while the run has not produced one.
	Answer Answer
	// Pause is the durable pause component (class / reason /
	// lifecycle / availability). Unavailable while the run is not
	// paused. It NEVER carries a pause/resume/approval token, and
	// actionability is not stored — the Protocol computes it from the
	// verified caller's control tier.
	Pause Pause
	// Inputs are the operator-attached input attachment metadata
	// (never bytes), each with reference availability.
	Inputs []Attachment
	// Outputs are the turn's output attachment metadata (artifact
	// references; never bytes), each with reference availability.
	Outputs []Attachment
	// Usage is the cumulative per-measure honest token/cost/latency/
	// model rollup. Every measure states its own availability /
	// exactness — an absent measure is unavailable, never a fabricated
	// zero. The runtime feeds CUMULATIVE snapshots (never deltas).
	Usage Usage
	// Reasoning is the bounded ordered DERIVED reasoning summary, with
	// the event sequence that produced the current accumulated
	// snapshot (0 when none was recorded). Steps carry a closed kind
	// only — raw provider thinking never enters the row.
	Reasoning Reasoning
	// Activity is the bounded content-free activity (tool dispatch)
	// window plus the exact turn-level totals across the full fed
	// activity.
	Activity Activity
	// Apps is the turn's ORDERED collection of interactive MCP App
	// references, with component availability. Order is first-
	// declaration order: a repeat of an identity
	// (effective_agent_id, server_id, resource_uri) replaces in place
	// and never moves. Empty when the turn declared none.
	Apps []AppRef
}

// Agent is the agent binding component.
type Agent struct {
	// ID is the registered agent id the run executed under; empty when
	// the runtime bound none (the run defaulted or no binding is
	// known).
	ID string
	// Name is the agent display name when known.
	Name string
	// BindingSource is the honest provenance of the binding:
	// explicit, defaulted, or unknown. Never fabricated stronger than
	// what the runtime reported.
	BindingSource AgentBindingSource
	// Complete is the honest state: Complete when the runtime supplied
	// a binding, Unavailable when it did not.
	Complete Completeness
}

// Query is the renderable user query component.
type Query struct {
	// Text is the rendered query text (markdown-safe display text,
	// bounded by MaxQueryRunes). Never the raw transcript.
	Text string
	// At is the instant the query / input was made (the append's
	// QueryAt, or the run's start instant when the runtime supplied
	// none). Zero only when no timestamp was ever reported.
	At time.Time
	// Complete is the honest state: Complete when a query was
	// supplied, Unavailable when the run had none.
	Complete Completeness
}

// Answer is the assistant answer component. State is the closed union
// of what the answer carries; Complete is derived from State for the
// uniform component model and must stay consistent with it (the
// projector derives it). A failed read NEVER becomes Empty — it
// surfaces as Evicted (the reference's content is gone) or
// Unavailable.
type Answer struct {
	// State is the closed union: inline | artifact_ref | empty |
	// evicted | unavailable.
	State AnswerState
	// Inline carries the answer text when State is inline ("" is a
	// legitimate complete inline answer — the model finished with
	// goal but produced no text).
	Inline string
	// Ref carries the artifact reference when State is artifact_ref.
	// Never both Inline and Ref.
	Ref *AnswerRef
	// Seq is the durable event-log sequence of the observation that
	// produced this accumulated answer snapshot (0 when none was
	// recorded). Together with the row Version it is the
	// component/version consistency anchor of the running accumulated
	// answer.
	Seq uint64
	// Complete is the derived honesty: Complete for inline /
	// artifact_ref / empty; Unavailable for evicted / unavailable.
	Complete Completeness
}

// AnswerRef is the metadata-only reference to a heavy answer routed
// through the artifact store. Bytes are fetched by id through the
// artifact surface; the projection carries metadata only (the
// heavy-content discipline — an inline heavy answer is refused at the
// projection edge with ErrContextLeak).
type AnswerRef struct {
	// ID is the content-addressed artifact identifier.
	ID string
	// MimeType is the stored answer's MIME type.
	MimeType string
	// SizeBytes is the stored byte length.
	SizeBytes int64
	// Filename is display metadata only.
	Filename string
	// SHA256 is the full hex digest.
	SHA256 string
}

// Pause is the durable pause component of a row: the class / reason /
// lifecycle of the current pause episode plus its component
// availability. It NEVER carries a pause / resume / approval token —
// the projection must never become a pause-token warehouse, resuming
// stays the runtime's concern, and actionability is NOT stored (the
// Protocol computes it from the verified caller's control tier).
type Pause struct {
	// Class is the pause producer class (hitl_approval, oauth,
	// a2a_auth_required, a2a_input_required, steering, operator).
	Class PauseClass
	// Reason is a short content-free reason string (bounded by
	// MaxPauseReasonRunes) — never raw approval context.
	Reason string
	// Lifecycle is the episode's durable lifecycle (requested, active,
	// resolved).
	Lifecycle PauseLifecycle
	// Availability is the honest state: Complete while a pause source
	// is reporting (the row is paused), Unavailable when no pause
	// episode is recorded.
	Availability Completeness
}

// Attachment is input / output attachment METADATA — never bytes.
// The content lives in the artifact store under ID; Availability is
// the honest reference availability (whether the referenced content
// can be fetched).
type Attachment struct {
	// ID is the content-addressed artifact identifier.
	ID string
	// Filename is display metadata only.
	Filename string
	// MimeType is the attachment's MIME type.
	MimeType string
	// SizeBytes is the byte length of the stored content.
	SizeBytes int64
	// SHA256 is the full hex digest.
	SHA256 string
	// Disposition is the per-attachment input hint (`ref` / `inline` /
	// `provider_native` / `tool:<name>`); empty on output attachments.
	Disposition string
	// Availability is the honest reference availability: Complete when
	// the referenced content resolves, Unavailable when it cannot be
	// fetched or no availability was reported.
	Availability Completeness
}

// Usage is the cumulative per-measure honest token / cost / latency /
// model rollup. The runtime feeds CUMULATIVE snapshots (never deltas);
// each accepted update replaces the component wholesale. Honesty is
// PER MEASURE: each numeric measure carries its own closed
// availability / exactness state (UsageMeasure) — an absent source is
// `unavailable` with NO value (never a fabricated zero), and an exact
// integer amount is only claimed when the source actually reported
// one. Money is never accumulated in float64: cost is exact integer
// micro-dollars. Model is a bounded identifier; an empty Model is the
// honest "no model reported".
type Usage struct {
	// PromptTokens is the cumulative prompt-side token measure.
	PromptTokens UsageMeasure
	// CompletionTokens is the cumulative completion-side token
	// measure.
	CompletionTokens UsageMeasure
	// ReasoningTokens is the cumulative reasoning-channel token
	// measure (when the provider reports a separate reasoning channel).
	ReasoningTokens UsageMeasure
	// CacheReadTokens is the cumulative cache-read token measure (when
	// the provider reports one).
	CacheReadTokens UsageMeasure
	// CacheWriteTokens is the cumulative cache-write token measure
	// (when the provider reports one).
	CacheWriteTokens UsageMeasure
	// TotalTokens is the cumulative total token measure.
	TotalTokens UsageMeasure
	// CostMicroUSD is the cumulative cost measure in EXACT INTEGER
	// micro-dollars (1e-6 USD) — never float64. 2_500_000 = $2.50.
	CostMicroUSD UsageMeasure
	// LatencyNS is the cumulative active-duration measure in
	// nanoseconds (when the source reports one).
	LatencyNS UsageMeasure
	// Model is the model identifier last reported by the usage source,
	// bounded by MaxModelRunes; empty means unavailable (no model
	// reported). A model identifier is exact when present.
	Model string
}

// UsageMeasure is ONE cumulative usage measure: a closed
// availability / exactness state plus the exact integer amount in the
// measure's unit (tokens, micro-dollars, nanoseconds). Value is nil
// exactly when the measure is unavailable — a missing measure is
// unavailable, never a fabricated zero — and non-negative when
// present. Cumulative snapshot semantics: the value is the run's
// total so far, replaced wholesale on each accepted update.
type UsageMeasure struct {
	// State states the availability / exactness: unavailable (no
	// source), exact, or estimated.
	State UsageState
	// Value is the exact integer amount (nil iff State is
	// unavailable).
	Value *int64
}

// Reasoning is the bounded ORDERED reasoning component — a DERIVED,
// consumer-safe summary of the run's trajectory thinking. Steps are in
// chronological trajectory order (index strictly increasing); the
// component is the ordered form a reopened transcript renders. Steps
// carry a closed ReasoningKind only: the projection is structurally
// unable to carry raw provider thinking (no trace, no
// chain-of-thought, no transcript), and a source that cannot classify
// a step into the closed set omits the step — the component reports
// the honest gap through its availability / dropped counts.
type Reasoning struct {
	// Steps are the retained ordered steps (at most MaxReasoningSteps,
	// the FIRST of the fed sequence). Empty when the source reported
	// none.
	Steps []ReasoningStep
	// Complete is Complete when every fed step was retained,
	// Partial when the fed sequence exceeded MaxReasoningSteps (the
	// tail was dropped — see Dropped), Unavailable when no reasoning
	// source was wired.
	Complete Completeness
	// Dropped is the number of fed steps NOT retained (overflow
	// beyond MaxReasoningSteps). Zero unless Complete is Partial.
	Dropped int
	// Seq is the durable event-log sequence of the observation that
	// produced this accumulated reasoning snapshot (0 when none was
	// recorded). Together with the row Version it is the
	// component/version consistency anchor of the running accumulated
	// reasoning.
	Seq uint64
}

// ReasoningStep is one DERIVED reasoning step summary at one
// trajectory position: its immutable index and its closed kind. It
// deliberately has NO text field — raw provider thinking (traces,
// chain-of-thought, transcripts, arbitrary provider text) cannot be
// represented in the row, only this structurally safe derived summary.
type ReasoningStep struct {
	// Index is the step's 0-based position in the run's trajectory
	// step sequence (positions are kept, gaps allowed — steps without
	// a safe derivative are not fed).
	Index int
	// Kind is the closed derived step class (tool_call / spawn /
	// await). A source that cannot classify a step into the closed set
	// omits the step honestly.
	Kind ReasoningKind
}

// Activity is the bounded content-free activity component: one row per
// tool dispatch, oldest first as fed. The inline window is configured
// on the projector (WithActivityLimit) — it must cover the runtime's
// configured per-turn tool-call budget and is capped at the absolute
// Protocol ceiling MaxActivityRows. The v1.28 Protocol surface is
// exactly `sessions.turns.list/get` — there is no third activity /
// analytics read and the projection never reads `state.history` from a
// consumer — so when the source activity exceeds the inline window the
// row exposes the honest partial / dropped / lower-bound state AND the
// exact turn-level ActivityTotals, which keep the turn summary
// renderable in list/get even though the older rows themselves are not
// retained.
type Activity struct {
	// Rows are the retained rows (at most the configured inline
	// activity limit, the LAST of the fed sequence — the recent
	// window).
	Rows []ActivityRow
	// Complete is Complete when every fed row was retained, Partial
	// when the fed sequence exceeded the inline activity limit.
	Complete Completeness
	// More is the EXPLICIT LOWER-BOUND marker: true when activity rows
	// older than the retained window exist (the fed sequence exceeded
	// the inline limit — a turn that outran its configured tool-call
	// budget, or an over-budget replay). Those older rows are NOT
	// exposed by list/get (the v1.28 surface has no activity
	// subresource); the exact Totals keep the turn-level summary
	// intact.
	More bool
	// Dropped is the number of fed rows NOT retained (the oldest
	// ones). Zero unless More is true.
	Dropped int
	// Totals are the compact EXACT counts across the FULL fed
	// activity (source-backed: counted from the cumulative feed the
	// runtime reported, by each row's current lifecycle status).
	// Truncating the inline window never erases the turn summary.
	Totals ActivityTotals
}

// ActivityTotals are the compact EXACT per-status counts across the
// full fed activity of the turn. They are source-backed (counted from
// the cumulative feed the runtime reported), bounded (six int64
// counters), and survive inline-window truncation so a turn that
// outran its tool-call budget still renders its exact activity summary
// through list/get.
type ActivityTotals struct {
	// Invoked is the count of dispatch rows currently in flight on
	// their first attempt.
	Invoked int64
	// Succeeded is the count of dispatch rows that completed
	// successfully.
	Succeeded int64
	// Failed is the count of dispatch rows that failed terminally
	// (excluding policy exhaustion — see PolicyExhausted).
	Failed int64
	// Cancelled is the count of dispatch rows cancelled.
	Cancelled int64
	// Retried is the count of dispatch rows currently in flight on a
	// retry attempt.
	Retried int64
	// PolicyExhausted is the count of dispatch rows whose retry policy
	// budget was exhausted.
	PolicyExhausted int64
}

// ActivityRow is one content-free tool dispatch row. It NEVER carries
// raw arguments or results (the runtime's audit-redaction discipline
// covers the source side; the projection validates shape only), and it
// retains enough bounded content-free data to render the dispatch's
// lifecycle and retry behaviour after a restart: stable identity,
// immutable ordinal, planner step / event sequence when known, batch
// correlation when known, status, timing, attempt count, terminal
// class, retryability / policy exhaustion, and a bounded safe summary.
type ActivityRow struct {
	// Position is the row's IMMUTABLE 0-based ordinal in the turn's
	// cumulative tool-dispatch sequence (the feed index at the moment
	// the row entered the projection; feeds are cumulative, so a
	// position never changes and appends only ever add HIGHER
	// positions).
	Position int
	// InvocationID is the stable invocation / tool-call identity of
	// the dispatch (an opaque correlation id the runtime mints once per
	// dispatch — never a tool argument); empty when the runtime
	// reported none.
	InvocationID string
	// Tool is the invoked tool name.
	Tool string
	// StepSequence is the planner step / event sequence that produced
	// this dispatch when the runtime knows it; 0 means not recorded.
	StepSequence uint64
	// BatchID is the batch / group correlation id when the runtime
	// groups this dispatch with siblings; empty when none.
	BatchID string
	// Status is the dispatch's lifecycle.
	Status ActivityStatus
	// TerminalClass is the DERIVED closed terminal classification of
	// the dispatch (none while in flight). Derived by the projector
	// from Status, so it is always consistent.
	TerminalClass ActivityTerminalClass
	// StartedAt is the dispatch start instant when the runtime
	// supplied one; zero otherwise.
	StartedAt time.Time
	// FinishedAt is the dispatch finish instant when the runtime
	// supplied one; zero while in flight or when not reported.
	FinishedAt time.Time
	// Duration is the dispatch duration when the runtime reported it;
	// 0 means not reported (never a fabricated instant result).
	Duration time.Duration
	// AttemptCount is the 1-based attempt number when the runtime
	// reported it; 0 means not reported.
	AttemptCount int
	// Retryable reports whether the dispatch is retryable per the
	// runtime's retry policy (true only when the runtime reported it).
	Retryable bool
	// PolicyExhausted reports whether the dispatch's retry policy
	// budget was exhausted (true iff Status is policy_exhausted).
	PolicyExhausted bool
	// Summary is the short content-free summary ("" while invoked).
	// Bounded by MaxActivitySummaryRunes — never raw arguments or
	// results.
	Summary string
}

// AppRefKey is the COMPARABLE TYPED replacement identity of one MCP
// App reference: exactly (effective_agent_id, server_id, resource_uri).
// It replaces the NUL-concatenated string form, which was ambiguous
// when a field itself contained a NUL byte: a struct of three strings
// is comparable and therefore a safe map key, and the identity fields
// are validated free of NUL / control bytes before they reach it.
type AppRefKey struct {
	// EffectiveAgentID is the agent whose context the App reference is
	// scoped to (empty when not agent-scoped).
	EffectiveAgentID string
	// ServerID is the MCP server (source id) hosting the App's
	// `ui://` document.
	ServerID string
	// ResourceURI is the App's `ui://` document URI.
	ResourceURI string
}

// Key returns the comparable typed replacement identity of the App
// reference — the map key ordered in-place replacement runs on.
func (r AppRef) Key() AppRefKey {
	return AppRefKey{
		EffectiveAgentID: r.EffectiveAgentID,
		ServerID:         r.ServerID,
		ResourceURI:      r.ResourceURI,
	}
}

// AppRef is one entry in the turn's ORDERED collection of interactive
// MCP App references, with component availability. Order is
// first-declaration order; a repeat of the identity (AppRefKey) replaces
// in place with the latest correlation metadata and never moves.
//
// ToolCallID is OPTIONAL correlation metadata: it lets the existing
// identity-scoped `mcp.apps.tool_context` channel lazily deliver the
// App's context to the host. It is NOT authority — it never rehydrates
// live callback authority on replay (a replayed App renders its honest
// availability placeholder instead of mounting live), and an absent id
// still mounts the App without Data Delivery.
type AppRef struct {
	// EffectiveAgentID is the agent whose context the App reference is
	// scoped to (empty when the App context is not agent-scoped). Part
	// of the replacement identity.
	EffectiveAgentID string
	// ServerID is the MCP server (source id) hosting the App's
	// `ui://` document. Part of the replacement identity.
	ServerID string
	// ResourceURI is the App's `ui://` document URI. Part of the
	// replacement identity. Omitted from the operations READ
	// projection (OpsTurnRow).
	ResourceURI string
	// DisplayMode is the negotiated display mode (`inline` /
	// `fullscreen` / `pip`); empty means the server stated no
	// preference.
	DisplayMode string
	// RawHTMLTrusted reports whether the server's raw-HTML opt-in is
	// in force (default-deny posture).
	RawHTMLTrusted bool
	// ToolCallID is the OPTIONAL correlation metadata for the
	// identity-scoped `mcp.apps.tool_context` lazy context-delivery
	// channel — never authority, never rehydrated live. Omitted from
	// the operations READ projection (OpsTurnRow).
	ToolCallID string
	// ToolName is the originating server-side tool name (display
	// metadata projected onto the App's host context).
	ToolName string
	// Availability is the component availability: Available when the
	// App's persisted tool context resolves, Unavailable when it
	// cannot be resolved (replay), Degraded when a required dependency
	// is missing.
	Availability AppAvailability
	// Complete is the honest state of the reference itself.
	Complete Completeness
}

// OpsTurnRow is the OPERATIONS-SAFE READ projection of one turn: the
// pure, STRUCTURALLY DISTINCT read DTO the operations / observability
// surface reads when it must not see consumer transcript content. It
// is served by Projector.OpsTurn and is deliberately a different type
// from TurnRow — an operations reader cannot reach the consumer fields
// through it.
//
// Retained: lifecycle (status / sealed / version / closed finish
// reason / closed error class / bounded redacted terminal messages),
// the task/run identity (TaskID + RunID — the operations surface
// needs both), agent binding (id / name / provenance), timing (started
// / updated / finished), per-measure honest usage / cost, content-free
// activity (tool names, statuses, summaries, exact totals), component
// COUNTS (derived reasoning steps, input / output attachments), App
// availability summaries (effective agent id, server id, tool name,
// availability), the pause class / reason / lifecycle / availability,
// and the row's last-applied event sequence.
//
// Structurally omitted (no field can reach them): the renderable
// query, the answer (inline and reference), raw provider thinking
// (reasoning summaries), pause / resume / approval tokens, the App
// resource URI and tool_call_id, and App context / input / result.
type OpsTurnRow struct {
	// TurnID is the row key.
	TurnID TurnID
	// TaskID is the authoritative root foreground task id.
	TaskID string
	// RunID is the actual runtime run id (empty = unavailable, never
	// equated with TaskID).
	RunID string
	// SessionID is the owning session, denormalised for rendering.
	SessionID string
	// Sequence and TieBreaker are the immutable ordering keys.
	Sequence   Seq
	TieBreaker TurnID
	// Status / Sealed / Version are the lifecycle and its write
	// generation.
	Status  Status
	Sealed  bool
	Version int
	// StartedAt / UpdatedAt / FinishedAt are the run's timing.
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
	// FinishReason / ErrorClass are the CLOSED content-free terminal
	// descriptors; FinishMessage / ErrorMessage are the bounded
	// redacted consumer-safe terminal messages when available.
	FinishReason  FinishReason
	ErrorClass    ErrorClass
	FinishMessage string
	ErrorMessage  string
	// AgentID / AgentName / AgentBindingSource are the agent binding.
	AgentID            string
	AgentName          string
	AgentBindingSource AgentBindingSource
	// Usage is the cumulative per-measure honest token / cost rollup.
	Usage Usage
	// Activity is the retained content-free activity window plus the
	// exact turn-level totals (tool names / statuses / summaries /
	// totals — the content-free shape the operations surface retains).
	Activity Activity
	// ReasoningSteps is the COUNT of retained derived reasoning steps
	// (never the steps themselves).
	ReasoningSteps int
	// Inputs / Outputs are the attachment COUNTS (never the metadata).
	Inputs  int
	Outputs int
	// Apps are the App availability summaries — effective agent id,
	// server id, tool name, availability; structurally no resource URI
	// and no tool_call_id.
	Apps []AppOpsRef
	// Pause is the pause class / reason / lifecycle / availability
	// (structurally no tokens).
	Pause Pause
	// LastAppliedEventSeq is the row's last-applied event sequence.
	LastAppliedEventSeq uint64
}

// AppOpsRef is the operations-safe App summary: the identity of the
// App (effective agent id, server id), its originating tool name, and
// its availability. It structurally omits the resource URI, the
// tool_call_id, and any App context / input / result.
type AppOpsRef struct {
	// EffectiveAgentID is the agent the App context is scoped to.
	EffectiveAgentID string
	// ServerID is the MCP server hosting the App's document.
	ServerID string
	// ToolName is the originating server-side tool name.
	ToolName string
	// Availability is the component availability.
	Availability AppAvailability
}
