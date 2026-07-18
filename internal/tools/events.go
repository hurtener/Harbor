package tools

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// tool-side event types. Registered via init() so the
// canonical events registry stays the single source of truth (see
// internal/events/events.go).
//
// All five are produced by ONE universal lifecycle-emitting shell the
// catalog wraps around every registered descriptor's Invoke (see
// wrapDescriptorLifecycle) — never per-transport-driver — so every
// dispatch path and every transport (in-process, MCP, HTTP, A2A, flow)
// produces them by construction, each carrying the full run quadruple on
// the envelope so the event is turn-attributable. Payloads are
// content-free: tool NAME + transport + status + attempts + duration
// only, never args or results (CLAUDE.md §7 rule 7).
const (
	// EventTypeToolInvoked — emitted by the descriptor-wrap shell at the
	// start of every tool invocation (before the wrapped Invoke's
	// validation + policy shell runs). Carries identity + tool name +
	// transport.
	EventTypeToolInvoked events.EventType = "tool.invoked"
	// EventTypeToolCompleted — emitted when the wrapped Invoke returns a
	// ToolResult with no error. Carries identity + tool name + transport
	// + attempts + duration.
	EventTypeToolCompleted events.EventType = "tool.completed"
	// EventTypeToolFailed — emitted on a terminal invocation
	// failure (policy retries exhausted or a permanent class
	// error). Carries identity + tool name + transport + last
	// error class + attempts taken.
	EventTypeToolFailed events.EventType = "tool.failed"
	// EventTypeToolInvalidArgs — emitted when argument validation
	// fails at the catalog edge. NOT a tool error; the planner is
	// expected to reformulate. Carries identity + tool name + the
	// validation error detail.
	EventTypeToolInvalidArgs events.EventType = "tool.invalid_args"
	// EventTypeToolPolicyExhausted — emitted when the policy's
	// retry budget exhausts. (A distinct shape from tool.failed so
	// operators can quantify "tool was unhealthy" vs "tool was
	// permanently broken".)
	EventTypeToolPolicyExhausted events.EventType = "tool.policy_exhausted"
)

func init() {
	for _, t := range []events.EventType{
		EventTypeToolInvoked,
		EventTypeToolCompleted,
		EventTypeToolFailed,
		EventTypeToolInvalidArgs,
		EventTypeToolPolicyExhausted,
	} {
		events.RegisterEventType(t)
	}
}

// ToolInvokedPayload is the typed payload for EventTypeToolInvoked.
// SafePayload: carries no secret-shaped data (the tool name and
// transport are operator-supplied identifiers, not user input).
type ToolInvokedPayload struct {
	events.SafeSealed
	Identity  identity.Quadruple
	ToolName  string
	Transport TransportKind
	StartedAt time.Time
}

// ToolCompletedPayload is the typed payload for
// EventTypeToolCompleted. SafePayload.
type ToolCompletedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	ToolName   string
	Transport  TransportKind
	Attempts   int
	DurationMS int64
}

// ToolFailedPayload is the typed payload for EventTypeToolFailed.
// SafePayload: ErrorClass is enum-shaped; ErrorMessage is
// operator-controlled (or wraps a sentinel).
type ToolFailedPayload struct {
	events.SafeSealed
	Identity     identity.Quadruple
	ToolName     string
	Transport    TransportKind
	Attempts     int
	ErrorClass   ErrorClass
	ErrorMessage string
	// ScopeShortfall carries the structured downstream insufficient-scope
	// step-up when the terminal failure was an *ErrInsufficientScope. Nil for
	// every other failure. Additive: it enriches the SAME tool.failed event
	// every transport already emits, never a new event type. SafePayload —
	// server/operator-supplied challenge metadata, never a token.
	ScopeShortfall *ScopeShortfallDetail `json:",omitempty"`
}

// ToolInvalidArgsPayload is the typed payload for
// EventTypeToolInvalidArgs. SafePayload: the validation error
// describes the schema mismatch (e.g. "expected string, got int"),
// not the offending arg value. Producers MUST NOT include raw arg
// bytes here — those go through the audit redactor.
type ToolInvalidArgsPayload struct {
	events.SafeSealed
	Identity        identity.Quadruple
	ToolName        string
	Transport       TransportKind
	ValidationError string
}

// ToolPolicyExhaustedPayload is the typed payload for
// EventTypeToolPolicyExhausted. SafePayload.
type ToolPolicyExhaustedPayload struct {
	events.SafeSealed
	Identity  identity.Quadruple
	ToolName  string
	Transport TransportKind
	Attempts  int
	LastClass ErrorClass
	LastError string
}
