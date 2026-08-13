package turns

import "time"

// This file holds the CONSUMER-SAFE DTO family: TurnRow and its
// component types — the read surface the future `sessions.turns.list`
// / `sessions.turns.get` Protocol methods project. Every component
// carries a Completeness state, every list is bounded, and the types
// deliberately carry only derived content (rendered query, inline or
// referenced answer, attachment metadata, lifecycle/agent/usage,
// bounded ordered reasoning and activity, App refs plus availability).
//
// Structurally absent from this family: raw tool arguments / results,
// raw transcripts, App-correlation tokens (the toolCallId-style
// correlation key that would rehydrate live callback authority), and
// pause/resume tokens. A future field that carries content must
// survive the ops allowlist review first (ops_safety_test.go).

// TurnRow is one projected conversation turn — the durable,
// consumer-safe read model a caller renders. Rows are keyed by
// (identity triple, TurnID); Sequence + TieBreaker are the immutable
// ordering keys newest-first paging pages over.
type TurnRow struct {
	// TurnID is the root foreground run's task id — the row key.
	TurnID TurnID
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
	Version int
	// StartedAt is the run's start instant (the append time when the
	// runtime did not supply one).
	StartedAt time.Time
	// UpdatedAt is the instant of the most recent accepted write.
	UpdatedAt time.Time
	// FinishedAt is the terminal instant; zero while the row is
	// mutable.
	FinishedAt time.Time
	// FinishReason is the terminal finish reason (the planner's
	// Finish.Reason verbatim when supplied); empty while mutable.
	FinishReason string
	// ErrorClass is the content-free terminal error class of a failed
	// run (a stable low-cardinality code — never an error message, and
	// never caller content). Empty unless the run failed.
	ErrorClass string

	// Agent is the agent binding the run executed under. Unavailable
	// when the runtime supplied no binding.
	Agent Agent
	// Query is the renderable user query. Unavailable when the run had
	// no user-visible query (e.g. an injected-context run).
	Query Query
	// Answer is the assistant answer — inline text below the
	// heavy-content threshold, or an artifact reference. Unavailable
	// while the run has not produced one.
	Answer Answer
	// Inputs are the operator-attached input attachment metadata
	// (never bytes).
	Inputs []Attachment
	// Outputs are the turn's output attachment metadata (artifact
	// references; never bytes).
	Outputs []Attachment
	// Usage is the cumulative token/cost/model rollup. Unavailable
	// when no cost source reported.
	Usage Usage
	// Reasoning is the bounded ordered reasoning step sequence.
	Reasoning Reasoning
	// Activity is the bounded content-free activity (tool dispatch)
	// window.
	Activity Activity
	// App is the interactive MCP App reference the turn declared, with
	// component availability; nil when the turn declared none.
	App *AppRef
}

// Agent is the agent binding component.
type Agent struct {
	// ID is the registered agent id the run executed under; empty when
	// the runtime bound none (the run defaulted).
	ID string
	// Name is the agent display name when known.
	Name string
	// Complete is the honest state: Complete when the runtime supplied
	// a binding, Unavailable when it did not.
	Complete Completeness
}

// Query is the renderable user query component.
type Query struct {
	// Text is the rendered query text (markdown-safe display text,
	// bounded by MaxQueryRunes). Never the raw transcript.
	Text string
	// Complete is the honest state: Complete when a query was
	// supplied, Unavailable when the run had none.
	Complete Completeness
}

// Answer is the assistant answer component: exactly one of Inline or
// Ref carries the answer when Complete is Complete.
type Answer struct {
	// Inline carries the answer text when it is below the
	// heavy-content threshold ("" is a legitimate complete inline
	// answer — the model finished with goal but produced no text).
	Inline string
	// Ref carries the artifact reference when the answer is heavy
	// (>= MaxInlineAnswerBytes) and routed through the artifact store.
	// Never both Inline and Ref; never neither when Complete.
	Ref *AnswerRef
	// Complete is the honest state: Complete when the run produced an
	// answer, Unavailable while it has not (a running turn has no
	// answer yet — never a fabricated empty one).
	Complete Completeness
}

// AnswerRef is the metadata-only reference to a heavy answer routed
// through the artifact store. Bytes are fetched by id through the
// artifact surface; the projection carries metadata only (D-026).
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

// Attachment is input / output attachment METADATA — never bytes.
// The content lives in the artifact store under ID.
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
}

// Usage is the cumulative token / cost / model rollup. The runtime
// feeds CUMULATIVE totals (never deltas); each accepted update
// replaces the component wholesale.
type Usage struct {
	// PromptTokens is the cumulative prompt-side token count.
	PromptTokens int64
	// CompletionTokens is the cumulative completion-side token count.
	CompletionTokens int64
	// ReasoningTokens is the cumulative reasoning-channel token count.
	ReasoningTokens int64
	// TotalTokens is the cumulative total token count.
	TotalTokens int64
	// CostUSD is the cumulative dollar cost.
	CostUSD float64
	// Model is the model id last reported by the cost source.
	Model string
	// Complete is the honest state: Complete when a cost source
	// reported, Unavailable when none did (a run whose provider
	// reports no usage is honestly "no data", not zero spend).
	Complete Completeness
}

// Reasoning is the bounded ORDERED reasoning component. Steps are in
// chronological trajectory order (index strictly increasing); the
// component is the consumer-safe ordered form the live trajectory
// projection and the reopened transcript both render.
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
}

// ReasoningStep is one provider thinking trace at one trajectory
// position. The trace is the provider's raw reasoning text — the same
// bytes the live trajectory projection serves — so it reaches the row
// ONLY through the separately named AttachReasoning channel, never
// through the generic ops (operations-safe DTO contract).
type ReasoningStep struct {
	// Index is the step's 0-based position in the run's trajectory
	// step sequence (positions are kept, gaps allowed — steps without
	// reasoning are not fed).
	Index int
	// Trace is the provider-side thinking trace (bounded by
	// MaxStepTraceRunes).
	Trace string
}

// Activity is the bounded content-free activity component: one row per
// tool dispatch, oldest first as fed.
type Activity struct {
	// Rows are the retained rows (at most MaxActivityRows, the LAST of
	// the fed sequence — the recent window).
	Rows []ActivityRow
	// Complete is Complete when every fed row was retained, Partial
	// when the fed sequence exceeded MaxActivityRows.
	Complete Completeness
	// More is the EXPLICIT LOWER-BOUND marker: true when activity rows
	// older than the retained window exist (the fed sequence exceeded
	// MaxActivityRows). The full activity is read through the
	// separately named optional activity-read contract the runtime
	// wires (over the durable event log) — there is no generic
	// subresource framework.
	More bool
	// Dropped is the number of fed rows NOT retained (the oldest
	// ones). Zero unless More is true.
	Dropped int
}

// ActivityRow is one content-free tool dispatch row. It NEVER carries
// raw arguments or results (CLAUDE.md §7 rule 7); Summary is a short
// derived string (duration, error class) bounded by
// MaxActivitySummaryRunes.
type ActivityRow struct {
	// Tool is the invoked tool name.
	Tool string
	// Status is the dispatch's lifecycle.
	Status ActivityStatus
	// Summary is the short content-free summary ("" while invoked).
	Summary string
	// At is the dispatch instant when the runtime supplied one; zero
	// otherwise.
	At time.Time
}

// AppRef is the interactive MCP App reference the turn declared, with
// component availability. It is the RENDER metadata only: it never
// carries the App-correlation token (the toolCallId-style key that
// would rehydrate live callback authority — durable replay is
// read-only and must never rehydrate it), so a replayed App renders
// its honest availability placeholder instead of mounting live.
type AppRef struct {
	// ServerID is the MCP server (source id) hosting the App's
	// `ui://` document.
	ServerID string
	// ResourceURI is the App's `ui://` document URI.
	ResourceURI string
	// DisplayMode is the negotiated display mode (`inline` /
	// `fullscreen` / `pip`); empty means the server stated no
	// preference.
	DisplayMode string
	// RawHTMLTrusted reports whether the server's raw-HTML opt-in is
	// in force (default-deny posture).
	RawHTMLTrusted bool
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
