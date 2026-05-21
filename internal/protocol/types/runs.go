package types

import "time"

// Package-level note (runs.go): the `runs.*` Protocol wire types
// (Phase 73n / D-130). Today the surface holds a single method —
// `runs.set_overrides` — the Console Playground page's reasoning-effort
// / temperature / max-tokens / system-prompt override recorder.
//
// `RunOverrides` is the single-source wire projection (D-002) of the
// next-message override parameters. It is declared HERE and nowhere
// else; `internal/runtime/runs/protocol` consumes this type rather than
// redeclaring it.

// RunOverrides is the wire projection of the next-message override
// parameters the Console Playground page records via `runs.set_overrides`.
//
// Every tuning field is a pointer so the absent / present distinction is
// preserved on the wire: a nil field means "leave the runtime default in
// place"; a non-nil field means "apply this value to the next message".
// This is deliberate — an operator who sets only reasoning effort must
// not implicitly zero the temperature.
//
// The override is session-scoped and one-shot: it applies to the NEXT
// `user_message` / `start` in the session and is consumed at that point.
// It does NOT apply retroactively to past messages.
type RunOverrides struct {
	// SessionID is the session the override applies to. Mandatory — an
	// empty SessionID fails the request closed at the Protocol edge
	// (identity is mandatory, CLAUDE.md §6 rule 9). The tenant and user
	// components of the isolation triple are inferred from the verified
	// JWT; only the session is named on the wire.
	SessionID string `json:"session_id"`
	// ReasoningEffort, when non-nil, overrides the LLM reasoning-effort
	// hint for the next message. Values follow the bound LLM provider's
	// taxonomy (e.g. `low` / `medium` / `high`); the runtime validates
	// the string against the provider's accepted set and rejects an
	// unknown value with CodeInvalidRequest.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	// Temperature, when non-nil, overrides the sampling temperature for
	// the next message. The runtime rejects a value outside the closed
	// range [0, 2] with CodeInvalidRequest.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens, when non-nil, overrides the per-message MaxTokens
	// governance ceiling for the next message. The runtime rejects a
	// non-positive value with CodeInvalidRequest.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// SystemPromptOverride, when non-nil, replaces the agent's system
	// prompt for the next message only. An empty string is a valid
	// override (it clears the system prompt for that one message);
	// nil means "leave the agent's configured system prompt in place".
	SystemPromptOverride *string `json:"system_prompt_override,omitempty"`
}

// RunSetOverridesRequest is the wire request for the `runs.set_overrides`
// Protocol method. It carries the request's identity scope plus the
// override payload.
type RunSetOverridesRequest struct {
	// Identity is the request's identity scope. The triple is mandatory;
	// the runtime cross-checks Identity.Session against
	// Overrides.SessionID and rejects a mismatch with CodeScopeMismatch
	// (a caller cannot set an override for a session outside its own
	// verified scope).
	Identity IdentityScope `json:"identity"`
	// Overrides is the next-message override payload.
	Overrides RunOverrides `json:"overrides"`
}

// RunSetOverridesResponse is the wire response for the
// `runs.set_overrides` Protocol method.
type RunSetOverridesResponse struct {
	// AppliedAt is the runtime timestamp at which the override was
	// recorded. It is the moment the override entered the session's
	// pending-override slot — not the moment a subsequent message
	// consumed it.
	AppliedAt time.Time `json:"applied_at"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with, so a Console can assert wire compatibility.
	ProtocolVersion string `json:"protocol_version"`
}
