package turns

import "time"

// This file holds the MUTATION DTO family — the projector's write
// surface. They are NOT the operations READ projection and do not
// satisfy any consumer-vs-operations authority matrix: the authority
// matrix is about READ projections (TurnRow vs OpsTurnRow, row.go).
// The mutation DTOs are minimal write shapes, and the contract is
// structural and binding:
//
//	Append / Update / Seal are STRUCTURALLY UNABLE to contain
//	transcript, reasoning traces, App correlation, or pause /
//	resume / approval tokens.
//
// Concretely the types have no fields for those categories:
//
//   - transcript — no message-history field; only the bounded rendered
//     Query and the derived Answer exist.
//   - reasoning — no reasoning field; reasoning reaches a row ONLY
//     through the separately named AttachReasoning channel
//     (projector.go), never through the generic ops.
//   - App correlation — no App field at all; App refs reach a row ONLY
//     through the separately named AttachAppRefs channel, whose input
//     (AppRefInput) is pinned to the ordered AppRef collection.
//   - pause tokens — no pause / resume / approval token field anywhere;
//     the Pause component carries class / reason / lifecycle /
//     availability only, and resuming stays the runtime's concern.
//
// opsFieldSet pins the exact field set of each ops struct (see
// ops_safety_test.go): a future content field cannot be added
// silently — it must survive the allowlist review first. The
// operations-safe READ projection (OpsTurnRow / AppOpsRef, row.go) is
// pinned too, so its structural omissions cannot silently regress.
//
// Slice semantics: on Update, a nil slice means "leave unchanged"; a
// non-nil slice (including an empty one) means "replace wholesale".

// opsFieldSet is the authoritative allowlist of the DTO field sets,
// keyed by type name. The pin test (ops_safety_test.go) holds each
// type's reflected field set to exactly its documented slice — a
// future content field cannot be added silently; it must survive the
// allowlist review first. The mutation ops (Append / Update / Seal),
// the two separately named content channels (ReasoningInput /
// AppRefInput), the component types they build on, and the
// operations-safe READ projection (OpsTurnRow / AppOpsRef) are pinned
// so their shapes stay deliberate.
var opsFieldSet = map[string][]string{
	"Append":         {"TurnID", "TaskID", "RunID", "Query", "QueryAt", "AgentID", "AgentName", "AgentBindingSource", "Status", "StartedAt", "Activity", "Inputs", "Outputs", "Pause", "EventSeq"},
	"Update":         {"Status", "Answer", "Usage", "Activity", "Inputs", "Outputs", "Pause", "EventSeq"},
	"Seal":           {"Status", "FinishReason", "ErrorClass", "FinishedAt", "EventSeq"},
	"ReasoningInput": {"Steps", "EventSeq"},
	"AppRefInput":    {"Refs", "EventSeq"},
	"Answer":         {"State", "Inline", "Ref", "Seq", "Complete"},
	"Usage":          {"PromptTokens", "CompletionTokens", "ReasoningTokens", "TotalTokens", "CostUSD", "Model", "Complete"},
	"Attachment":     {"ID", "Filename", "MimeType", "SizeBytes", "SHA256", "Disposition", "Availability"},
	"ActivityRow":    {"Position", "Tool", "Status", "Summary", "At"},
	"ReasoningStep":  {"Index", "Trace"},
	"Reasoning":      {"Steps", "Complete", "Dropped", "Seq"},
	"AppRef":         {"EffectiveAgentID", "ServerID", "ResourceURI", "DisplayMode", "RawHTMLTrusted", "ToolCallID", "ToolName", "Availability", "Complete"},
	"AppRefKey":      {"EffectiveAgentID", "ServerID", "ResourceURI"},
	"Agent":          {"ID", "Name", "BindingSource", "Complete"},
	"Query":          {"Text", "At", "Complete"},
	"AnswerRef":      {"ID", "MimeType", "SizeBytes", "Filename", "SHA256"},
	"Pause":          {"Class", "Reason", "Lifecycle", "Availability"},
	"OpsTurnRow":     {"TurnID", "TaskID", "RunID", "SessionID", "Sequence", "TieBreaker", "Status", "Sealed", "Version", "StartedAt", "UpdatedAt", "FinishedAt", "FinishReason", "ErrorClass", "AgentID", "AgentName", "AgentBindingSource", "Usage", "Activity", "ReasoningSteps", "Inputs", "Outputs", "Apps", "Pause", "LastAppliedEventSeq"},
	"AppOpsRef":      {"EffectiveAgentID", "ServerID", "ToolName", "Availability"},
}

// Append creates the MUTABLE row for a root foreground run. It is the
// only way a turn enters the projection, and it carries only derived
// content: the renderable query with its timestamp, the agent binding
// with its honest provenance, the optional initial lifecycle state,
// and the optional initial attachment / activity / pause metadata.
// Structurally absent: transcript, reasoning traces, App correlation,
// and pause / resume / approval tokens.
type Append struct {
	// TurnID is the row key — the root foreground run's task id.
	// Mandatory and unique within the session. A reserved cursor
	// separator ("|") is rejected (it would break the opaque page
	// cursor encoding).
	TurnID TurnID
	// TaskID is the AUTHORITATIVE root foreground task id when the
	// runtime reports it separately from the row key; empty derives
	// from TurnID (the row key IS the task id). Never erases the run
	// id: RunID is carried independently.
	TaskID string
	// RunID is the ACTUAL runtime run id of the root foreground run.
	// Empty means UNAVAILABLE (a legacy record or a runtime that did
	// not report one) — it is never silently equated with TaskID or
	// TurnID.
	RunID string
	// Query is the renderable user query (bounded by MaxQueryRunes).
	// Empty is legitimate — the completeness state reports
	// Unavailable for a run with no user-visible query.
	Query string
	// QueryAt is the instant the query / input was made; zero means
	// the projector stamps the run's start instant.
	QueryAt time.Time
	// AgentID is the registered agent id the run executes under; empty
	// when the run binds the runtime default or no binding is known.
	AgentID string
	// AgentName is the agent display name when known.
	AgentName string
	// AgentBindingSource is the honest binding provenance
	// (explicit / defaulted / unknown). Empty is derived by the
	// projector: a non-empty AgentID derives explicit, an empty one
	// derives unknown — defaulted must be reported explicitly by the
	// runtime.
	AgentBindingSource AgentBindingSource
	// Status is the initial mutable lifecycle state: StatusRunning
	// (default when empty) or StatusPaused. A terminal status here is
	// invalid (ErrInvalidStatus) — terminal rows are reached only
	// through Seal.
	Status Status
	// StartedAt is the run's start instant; zero means the projector
	// stamps the append instant.
	StartedAt time.Time
	// Activity is the optional initial activity window (cumulative
	// feed semantics as Update.Activity; usually empty at append).
	Activity []ActivityRow
	// Inputs is the optional initial input attachment metadata.
	Inputs []Attachment
	// Outputs is the optional initial output attachment metadata.
	Outputs []Attachment
	// Pause is the optional initial pause component (e.g. a turn that
	// starts paused); nil means no pause episode is recorded.
	Pause *Pause
	// EventSeq is the durable event-log sequence of the observation
	// this append applies (0 = none recorded). Stamped on the row as
	// LastAppliedEventSeq.
	EventSeq uint64
}

// Update mutates a MUTABLE (running / paused) row in place. Each
// non-nil component replaces the stored component wholesale; nil
// leaves it unchanged. The runtime feeds CUMULATIVE values (usage
// totals, the full activity list) so a replacement is always
// self-consistent. Structurally absent: transcript, reasoning traces,
// App correlation, and pause / resume / approval tokens.
type Update struct {
	// Status is the new mutable lifecycle state when non-empty:
	// StatusRunning or StatusPaused only. A terminal status here is
	// invalid (ErrInvalidStatus) — terminal rows are reached only
	// through Seal.
	Status Status
	// Answer replaces the answer component when non-nil.
	Answer *Answer
	// Usage replaces the usage component when non-nil (cumulative
	// totals).
	Usage *Usage
	// Activity replaces the retained activity window when non-nil
	// (cumulative feed, oldest first; the projector keeps the newest
	// configured-window rows and reports the lower-bound overflow).
	Activity []ActivityRow
	// Inputs replaces the input attachment list when non-nil (an empty
	// non-nil slice clears).
	Inputs []Attachment
	// Outputs replaces the output attachment list when non-nil (an
	// empty non-nil slice clears).
	Outputs []Attachment
	// Pause replaces the pause component when non-nil (class / reason /
	// lifecycle / availability — never a token).
	Pause *Pause
	// EventSeq is the durable event-log sequence of the observation
	// this update applies (0 = none recorded). Stamped on the row's
	// LastAppliedEventSeq and on any replaced accumulated snapshot
	// (Answer.Seq).
	EventSeq uint64
}

// Seal transitions a MUTABLE row to its SEALED terminal form. The
// store refuses the seal until the terminal status's REQUIRED sources
// are present on the current row:
//
//   - StatusComplete requires the Answer component in a definite
//     state (inline / artifact_ref / empty — a completed turn always
//     has an answer; an empty answer is a legitimate complete answer,
//     and an evicted or unavailable answer is NOT — the seal is
//     refused naming the source).
//   - StatusFailed requires a non-empty ErrorClass (a failed run
//     always carries its content-free error class).
//   - StatusCancelled requires nothing beyond the terminal lifecycle.
//
// A missing required source fails loud with ErrSealIncomplete naming
// the source. After a successful seal the row is immutable:
// every later mutation fails with ErrTurnSealed.
//
// Structurally absent: transcript, reasoning traces, App correlation,
// and pause / resume / approval tokens.
type Seal struct {
	// Status is the terminal status: StatusComplete, StatusFailed, or
	// StatusCancelled. Anything else fails with ErrNotTerminal (or
	// ErrInvalidStatus for an unknown enum).
	Status Status
	// FinishReason is the terminal finish reason (the planner's
	// Finish.Reason verbatim when supplied).
	FinishReason string
	// ErrorClass is the content-free terminal error class of a failed
	// run; empty for complete / cancelled seals.
	ErrorClass string
	// FinishedAt is the terminal instant; zero means the projector
	// stamps the seal instant.
	FinishedAt time.Time
	// EventSeq is the durable event-log sequence of the observation
	// this seal applies (0 = none recorded). Stamped on the row's
	// LastAppliedEventSeq.
	EventSeq uint64
}

// ReasoningInput is the SEPARATELY NAMED input channel for the
// bounded ordered reasoning component. It is NOT part of the generic
// ops (which are structurally unable to contain reasoning): the
// runtime wiring attaches reasoning steps through
// Projector.AttachReasoning only. Steps are fed in chronological
// trajectory order with strictly increasing (gap-tolerant) indices;
// the projector retains the first MaxReasoningSteps and reports the
// tail drop as Partial + Dropped.
type ReasoningInput struct {
	// Steps are the ordered reasoning steps, oldest first, indices
	// strictly increasing.
	Steps []ReasoningStep
	// EventSeq is the durable event-log sequence of the observation
	// this attach applies (0 = none recorded). Stamped on the
	// Reasoning.Seq component snapshot and the row's
	// LastAppliedEventSeq.
	EventSeq uint64
}

// AppRefInput is the SEPARATELY NAMED input channel for the App
// reference component. It is NOT part of the generic ops: the runtime
// wiring attaches App refs through Projector.AttachAppRefs only.
//
// Refs is an ORDERED upsert in declaration order: the replacement
// identity is exactly (EffectiveAgentID, ServerID, ResourceURI). A
// ref whose identity is already on the row replaces it IN PLACE (its
// position in the ordered collection is fixed by the FIRST
// declaration) with the latest correlation metadata; a new identity
// appends at the end. Refs never carry App context / input / result,
// and the optional ToolCallID is correlation metadata only — never
// authority.
type AppRefInput struct {
	// Refs are the ordered App refs to upsert, first declaration
	// fixes position.
	Refs []AppRef
	// EventSeq is the durable event-log sequence of the observation
	// this attach applies (0 = none recorded). Stamped on the row's
	// LastAppliedEventSeq.
	EventSeq uint64
}
