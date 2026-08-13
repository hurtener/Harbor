package turns

import "time"

// This file holds the OPERATIONS-SAFE DTO family — the projector's
// mutation surface. The contract is structural and binding:
//
//	Append / Update / Seal are STRUCTURALLY UNABLE to contain
//	transcript, reasoning, App-correlation, or pause tokens.
//
// Concretely the types have no fields for those categories:
//
//   - transcript — no message-history field; only the bounded rendered
//     Query and the derived Answer exist.
//   - reasoning — no reasoning field; reasoning reaches a row ONLY
//     through the separately named AttachReasoning channel
//     (projector.go), never through the generic ops.
//   - App-correlation — no toolCallId-style correlation field; the
//     App reference (AppRef, row.go) is written ONLY through the
//     separately named AttachAppRef channel and carries render
//     metadata plus availability, never a correlation token.
//   - pause tokens — no pause/resume token field anywhere; a paused
//     row's lifecycle shows the state, and resuming stays the
//     runtime's concern.
//
// opsFieldSet pins the exact field set of each ops struct (see
// ops_safety_test.go): a future content field cannot be added
// silently — it must survive the allowlist review first.
//
// Slice semantics: on Update, a nil slice means "leave unchanged"; a
// non-nil slice (including an empty one) means "replace wholesale".

// opsFieldSet is the authoritative allowlist of the DTO field sets,
// keyed by type name. The pin test (ops_safety_test.go) holds each
// type's reflected field set to exactly its documented slice — a
// future content field cannot be added silently; it must survive the
// allowlist review first. The operations-safe ops (Append / Update /
// Seal) and the two separately named content channels (Reasoning /
// AppRefInput) are pinned, and the shared component types they build
// on are pinned too so their shapes stay deliberate.
var opsFieldSet = map[string][]string{
	"Append":         {"TurnID", "Query", "AgentID", "AgentName", "Status", "StartedAt", "Activity", "Inputs", "Outputs"},
	"Update":         {"Status", "Answer", "Usage", "Activity", "Inputs", "Outputs"},
	"Seal":           {"Status", "FinishReason", "ErrorClass", "FinishedAt"},
	"ReasoningInput": {"Steps"},
	"AppRefInput":    {"Ref"},
	"Answer":         {"Inline", "Ref", "Complete"},
	"Usage":          {"PromptTokens", "CompletionTokens", "ReasoningTokens", "TotalTokens", "CostUSD", "Model", "Complete"},
	"Attachment":     {"ID", "Filename", "MimeType", "SizeBytes", "SHA256", "Disposition"},
	"ActivityRow":    {"Tool", "Status", "Summary", "At"},
	"ReasoningStep":  {"Index", "Trace"},
	"AppRef":         {"ServerID", "ResourceURI", "DisplayMode", "RawHTMLTrusted", "ToolName", "Availability", "Complete"},
	"Agent":          {"ID", "Name", "Complete"},
	"Query":          {"Text", "Complete"},
	"AnswerRef":      {"ID", "MimeType", "SizeBytes", "Filename", "SHA256"},
}

// Append creates the MUTABLE row for a root foreground run. It is the
// only way a turn enters the projection, and it carries only derived
// content: the renderable query (never the raw transcript), the agent
// binding, the optional initial lifecycle state, and the optional
// initial attachment / activity metadata. Structurally absent:
// transcript, reasoning, App-correlation, and pause tokens.
type Append struct {
	// TurnID is the root foreground run's task id. Mandatory and
	// unique within the session.
	TurnID TurnID
	// Query is the renderable user query (bounded by MaxQueryRunes).
	// Empty is legitimate — the completeness state reports
	// Unavailable for a run with no user-visible query.
	Query string
	// AgentID is the registered agent id the run executes under; empty
	// when the run binds the runtime default.
	AgentID string
	// AgentName is the agent display name when known.
	AgentName string
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
}

// Update mutates a MUTABLE (running / paused) row in place. Each
// non-nil component replaces the stored component wholesale; nil
// leaves it unchanged. The runtime feeds CUMULATIVE values (usage
// totals, the full activity list) so a replacement is always
// self-consistent. Structurally absent: transcript, reasoning,
// App-correlation, and pause tokens.
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
	// MaxActivityRows and reports the lower-bound overflow).
	Activity []ActivityRow
	// Inputs replaces the input attachment list when non-nil (an empty
	// non-nil slice clears).
	Inputs []Attachment
	// Outputs replaces the output attachment list when non-nil (an
	// empty non-nil slice clears).
	Outputs []Attachment
}

// Seal transitions a MUTABLE row to its SEALED terminal form. The
// store refuses the seal until the terminal status's REQUIRED sources
// are present on the current row:
//
//   - StatusComplete requires the Answer component Complete (a
//     completed turn always has an answer — inline or referenced; an
//     empty inline answer is a legitimate complete answer).
//   - StatusFailed requires a non-empty ErrorClass (a failed run
//     always carries its content-free error class).
//   - StatusCancelled requires nothing beyond the terminal lifecycle.
//
// A missing required source fails loud with ErrSealIncomplete naming
// the source. After a successful seal the row is immutable:
// every later mutation fails with ErrTurnSealed.
//
// Structurally absent: transcript, reasoning, App-correlation, and
// pause tokens.
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
}

// AppRefInput is the SEPARATELY NAMED input channel for the App
// reference component. It is NOT part of the generic ops: the runtime
// wiring attaches an App ref through Projector.AttachAppRef only. The
// ref carries render metadata plus availability and NEVER a
// correlation token (App-correlation is structurally excluded from
// the DTO family — see row.go's AppRef).
type AppRefInput struct {
	// Ref is the App reference to attach (replaces any prior one;
	// last-wins within a turn, mirroring the live discovery reducer).
	Ref AppRef
}
