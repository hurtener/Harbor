// Package steering is the public SDK facade over Harbor's
// internal/runtime/steering package — the run loop that drives a
// planner, the per-run control inbox/registry, and the steering
// taxonomy (RFC §3.6, §6.3). Alias-based re-exports only: no
// behavior lives here. RunLoop construction stays internal (the
// assembled Stack carries the production RunLoop); control-history
// internals and event payload structs are deliberately private.
package steering

import (
	internal "github.com/hurtener/Harbor/internal/runtime/steering"
)

// Run-loop + control vocabulary — aliases of the internal types.
type (
	// RunLoop is the shared planner-driving loop (one instance backs
	// every spawned task; obtain it from the assembled Stack).
	RunLoop = internal.RunLoop
	// RunSpec is the per-run input to RunLoop.Run.
	RunSpec = internal.RunSpec
	// ToolExecutor dispatches the planner's non-Finish decisions.
	ToolExecutor = internal.ToolExecutor
	// Registry is the per-run steering inbox registry.
	Registry = internal.Registry
	// Option customises NewRegistry.
	Option = internal.Option
	// Clock is the controllable time source seam.
	Clock = internal.Clock
	// Inbox is one run's control inbox.
	Inbox = internal.Inbox
	// ControlType names one steering control (the nine-entry taxonomy).
	ControlType = internal.ControlType
	// ControlEvent is one validated, scoped control instruction.
	ControlEvent = internal.ControlEvent
	// AppliedControl records one applied control on the history.
	AppliedControl = internal.AppliedControl
	// Scope is the caller-privilege scope of a control.
	Scope = internal.Scope
)

// DefaultMaxSteps is the run loop's default planner-step cap.
const DefaultMaxSteps = internal.DefaultMaxSteps

// ControlType values — the nine-entry steering taxonomy (RFC §6.3).
const (
	// ControlInjectContext — INJECT_CONTEXT.
	ControlInjectContext = internal.ControlInjectContext
	// ControlRedirect — REDIRECT.
	ControlRedirect = internal.ControlRedirect
	// ControlCancel — CANCEL.
	ControlCancel = internal.ControlCancel
	// ControlPrioritize — PRIORITIZE.
	ControlPrioritize = internal.ControlPrioritize
	// ControlPause — PAUSE.
	ControlPause = internal.ControlPause
	// ControlResume — RESUME.
	ControlResume = internal.ControlResume
	// ControlApprove — APPROVE.
	ControlApprove = internal.ControlApprove
	// ControlReject — REJECT.
	ControlReject = internal.ControlReject
	// ControlUserMessage — USER_MESSAGE.
	ControlUserMessage = internal.ControlUserMessage
)

// Scope values.
const (
	// ScopeSessionUser — the session's own user.
	ScopeSessionUser = internal.ScopeSessionUser
	// ScopeOwnerUser — the owning user across their sessions.
	ScopeOwnerUser = internal.ScopeOwnerUser
	// ScopeAdmin — the elevated admin scope.
	ScopeAdmin = internal.ScopeAdmin
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrIdentityRequired — the run's identity quadruple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrUnknownControlType — the control type is not in the taxonomy.
	ErrUnknownControlType = internal.ErrUnknownControlType
	// ErrPayloadInvalid — the control payload failed validation.
	ErrPayloadInvalid = internal.ErrPayloadInvalid
	// ErrScopeMismatch — the caller scope is insufficient.
	ErrScopeMismatch = internal.ErrScopeMismatch
	// ErrInvalidScope — the caller scope is not a valid Scope.
	ErrInvalidScope = internal.ErrInvalidScope
	// ErrInboxNotFound — no inbox is open for the run.
	ErrInboxNotFound = internal.ErrInboxNotFound
	// ErrNoPlanner — RunSpec.Planner is nil.
	ErrNoPlanner = internal.ErrNoPlanner
	// ErrRunLoopMisconfigured — the RunLoop misses a mandatory dependency.
	ErrRunLoopMisconfigured = internal.ErrRunLoopMisconfigured
	// ErrNoOutstandingPause — the control targets a run with no pause.
	ErrNoOutstandingPause = internal.ErrNoOutstandingPause
	// ErrMaxStepsExceeded — the run loop hit its step cap.
	ErrMaxStepsExceeded = internal.ErrMaxStepsExceeded
	// ErrDecisionShapeUnsupported — the executor cannot dispatch the shape.
	ErrDecisionShapeUnsupported = internal.ErrDecisionShapeUnsupported
)

// NewRegistry constructs a steering registry.
var NewRegistry = internal.NewRegistry

// WithClock injects a controllable clock into NewRegistry.
var WithClock = internal.WithClock

// IsValidControlType reports whether t is in the taxonomy.
var IsValidControlType = internal.IsValidControlType

// ControlTypes lists the taxonomy.
var ControlTypes = internal.ControlTypes

// IsValidScope reports whether s is a valid Scope.
var IsValidScope = internal.IsValidScope

// RequiredScope returns the minimum scope a control type demands.
var RequiredScope = internal.RequiredScope

// ValidatePayload validates a control payload's shape budget.
var ValidatePayload = internal.ValidatePayload
