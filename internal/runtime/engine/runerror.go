package engine

import (
	"errors"
	"fmt"
)

// RunError is the structured error envelope emitted on terminal node
// failure. Carries the full identity quadruple via RunID + the
// per-invocation context (NodeName, NodeID). Cause carries one level
// of wrapping; deeper chains use errors.Unwrap on Cause.
//
// Identity propagation: every RunError carries the failing envelope's
// (TenantID, UserID, SessionID, RunID) so audit logs and bus
// subscribers can scope by the multi-isolation triple.
type RunError struct {
	// RunID identifies the run the failed envelope belonged to.
	// Empty when the envelope had no RunID (Phase 10 allows that;
	// Phase 13 will tighten when FetchByRun arrives).
	RunID string
	// TenantID, UserID, SessionID complete the identity triple from
	// the failing envelope. Used by audit subscribers + slog
	// attribute set.
	TenantID  string
	UserID    string
	SessionID string
	// NodeName is the unique node identifier within the engine.
	NodeName string
	// NodeID is reserved for future stable runtime identifiers; for
	// Phase 11 it mirrors NodeName (engines are single-process and
	// the Name is the stable id).
	NodeID string
	// Code categorises the failure. See RunErrorCode constants.
	Code RunErrorCode
	// Message is a short human-readable summary. The redactor sees
	// it via the bus emit path; do NOT include raw tool args or
	// secrets.
	Message string
	// Cause is the wrapped underlying error (if any). errors.Unwrap
	// follows this; deeper chains compose via the underlying error's
	// Unwrap method.
	Cause error
	// Metadata carries policy-relevant context (e.g. attempt count,
	// timeout in ms, validate side). Bounded; free-form.
	Metadata map[string]any
}

// Error implements the error interface. Format includes the code +
// node name + message; downstream consumers should errors.As to a
// *RunError to read structured fields.
func (e *RunError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("engine: %s on node %q (run=%q): %s: %v",
			e.Code, e.NodeName, e.RunID, e.Message, e.Cause)
	}
	return fmt.Sprintf("engine: %s on node %q (run=%q): %s",
		e.Code, e.NodeName, e.RunID, e.Message)
}

// Unwrap exposes the immediate cause for errors.Is / errors.As walks.
// One level only; deeper chains follow Cause.Unwrap().
func (e *RunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ToPayload returns the RunError as a map[string]any suitable for the
// bus's RuntimeErrorPayload.Fields slot or slog attributes. Keys
// match the canonical attribute set Harbor uses everywhere
// (tenant_id, user_id, session_id, run_id, node, code, error).
func (e *RunError) ToPayload() map[string]any {
	if e == nil {
		return nil
	}
	out := map[string]any{
		"tenant_id":  e.TenantID,
		"user_id":    e.UserID,
		"session_id": e.SessionID,
		"run_id":     e.RunID,
		"node":       e.NodeName,
		"node_id":    e.NodeID,
		"code":       string(e.Code),
		"error":      e.Message,
	}
	if e.Cause != nil {
		out["cause"] = e.Cause.Error()
	}
	for k, v := range e.Metadata {
		// Don't allow Metadata keys to overwrite the canonical set;
		// prefix with "meta_" if collision detected.
		if _, taken := out[k]; taken {
			out["meta_"+k] = v
			continue
		}
		out[k] = v
	}
	return out
}

// RunErrorCode categorises the failure mode. Stable across Harbor's
// runtime — downstream consumers (audit subscribers, Console) match
// against these constants.
type RunErrorCode string

const (
	// CodeNodeTimeout — the node's invocation exceeded NodePolicy.TimeoutMS.
	CodeNodeTimeout RunErrorCode = "node_timeout"
	// CodeNodeException — the node's Func returned a non-nil error
	// or panicked. The most common terminal-failure code.
	CodeNodeException RunErrorCode = "node_exception"
	// CodeRunCancelled — the engine's context (or the per-run cancel
	// flag, Phase 13) was triggered before invocation completed.
	CodeRunCancelled RunErrorCode = "run_cancelled"
	// CodeDeadlineExceeded — the envelope's wall-clock DeadlineAt
	// expired before the worker invoked the node. Distinct from
	// CodeNodeTimeout (per-invocation) — DeadlineExceeded is per-
	// envelope.
	CodeDeadlineExceeded RunErrorCode = "deadline_exceeded"
	// CodeValidationFailed — NodePolicy.ValidateFunc rejected the
	// input or output envelope.
	CodeValidationFailed RunErrorCode = "validation_failed"
)

// asRunError extracts a *RunError from err if present. Convenience
// for downstream consumers that want to reach for structured fields.
func asRunError(err error) (*RunError, bool) {
	var re *RunError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
