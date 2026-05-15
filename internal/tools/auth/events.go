package auth

import "github.com/hurtener/Harbor/internal/events"

// Canonical tool-auth event types. Registered from this package's
// init() so a Publish never trips events.ErrUnknownEventType.
//
// `tool.auth_required` and `tool.auth_completed` are the two events
// Phase 30 emits onto the bus. Both carry caller-controllable surface
// only (URLs, scopes, source identifiers) — NEVER access / refresh
// token bytes. Both payload types embed events.SafeSealed so the bus
// accepts them under the typed path and the redactor is not run on a
// payload that already contains zero secret-shaped data.
const (
	// EventTypeToolAuthRequired — emitted when a tool invocation
	// requires OAuth (no usable token; refresh failed; A2A reported
	// AUTH_REQUIRED). Payload is ToolAuthRequiredPayload.
	EventTypeToolAuthRequired events.EventType = "tool.auth_required"

	// EventTypeToolAuthCompleted — emitted by CompleteFlow on
	// successful token exchange. Payload is ToolAuthCompletedPayload.
	EventTypeToolAuthCompleted events.EventType = "tool.auth_completed"
)

func init() {
	events.RegisterEventType(EventTypeToolAuthRequired)
	events.RegisterEventType(EventTypeToolAuthCompleted)
}

// ToolAuthRequiredPayload is the typed payload for a
// `tool.auth_required` event. SafePayload by construction: every
// field is the runtime's own bookkeeping or operator-supplied
// configuration metadata; no token plaintext, no upstream-response
// bytes.
type ToolAuthRequiredPayload struct {
	events.SafeSealed
	// Source is the ToolSourceID that needs auth.
	Source string
	// SourceName is the human-readable name (from
	// OAuthConfig.SourceName); the Console renders this in the
	// "Connect <SourceName>" prompt.
	SourceName string
	// BindingScope is "user" or "agent" — drives the Console UX
	// target (per-user prompt vs admin-targeted banner).
	BindingScope string
	// AuthorizeURL is the URL to visit to complete OAuth.
	AuthorizeURL string
	// State is the CSRF / flow-correlation key. Not a secret; used
	// by the callback handler to correlate the resume with the
	// pause record.
	State string
	// PauseToken is the unified pause/resume Coordinator's Token —
	// the runtime uses this to find the parked run on resume.
	PauseToken string
	// Scopes is the OAuth scopes requested.
	Scopes []string
}

// ToolAuthCompletedPayload is the typed payload for a
// `tool.auth_completed` event. SafePayload by construction — token
// bytes never appear.
type ToolAuthCompletedPayload struct {
	events.SafeSealed
	// Source is the ToolSourceID for which auth completed.
	Source string
	// BindingScope echoes the originating attachment.
	BindingScope string
	// State is the CSRF / flow-correlation key used by callers
	// observing the matching `tool.auth_required` event.
	State string
	// PauseToken is the unified pause/resume Coordinator's Token —
	// observers can correlate this to the pause.resumed event.
	PauseToken string
}
