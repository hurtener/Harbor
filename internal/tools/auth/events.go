package auth

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// Canonical tool-auth event types. Registered from this package's
// init() so a Publish never trips events.ErrUnknownEventType.
//
// `tool.auth_required` and `tool.auth_completed` are the two events
// Harbor emits onto the bus. Both carry caller-controllable surface
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

	// EventTypeToolCredentialExchanged — emitted once per ACTUAL
	// downstream-credential exchange against an external credential
	// broker (the pull-based, non-interactive acquisition strategy;
	// cache hits emit nothing). Payload is
	// ToolCredentialExchangedPayload. SafePayload by construction: it
	// carries the source, binding scope, subject kind, broker host,
	// granted scopes, and expiry — NEVER access / refresh token bytes.
	// The emission satisfies §7's "external credential provisioning
	// requires explicit configuration AND emits audit events".
	EventTypeToolCredentialExchanged events.EventType = "tool.credential_exchanged" //nolint:gosec // G101 false positive: this is a canonical event-type name, not a credential
)

func init() {
	events.RegisterEventType(EventTypeToolAuthRequired)
	events.RegisterEventType(EventTypeToolAuthCompleted)
	events.RegisterEventType(EventTypeToolCredentialExchanged)
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

// ToolCredentialExchangedPayload is the typed payload for a
// `tool.credential_exchanged` event — one downstream-credential
// exchange against an external credential broker (the pull-based
// `tokenexchange` acquisition strategy). SafePayload by construction:
// every field is the runtime's own bookkeeping or operator-supplied
// configuration metadata; NO access / refresh token plaintext, NO
// broker-response bytes. Emitted once per ACTUAL exchange — a cache
// hit emits nothing.
type ToolCredentialExchangedPayload struct {
	events.SafeSealed
	// Source is the ToolSourceID the exchanged credential authorises.
	Source string
	// BindingScope is "user" or "agent" — echoes the attachment's
	// binding scope (the V1 `tokenexchange` driver serves "user").
	BindingScope string
	// SubjectKind names which principal the broker minted the token
	// for: "user" for a user-bound exchange. Never the subject value
	// itself (that is identity, carried on the event's Identity
	// quadruple), only the kind.
	SubjectKind string
	// BrokerHost is the host of the configured broker `token_url` —
	// the external authority the exchange targeted. Host only; never
	// the full URL with query material.
	BrokerHost string
	// GrantedScopes is the scope list the broker granted (may be a
	// subset of the requested scopes).
	GrantedScopes []string
	// ExpiresAt is the broker-advertised wall-clock validity of the
	// exchanged token. Zero when the broker advertised no expiry. The
	// runtime's in-memory cache-serve horizon is bounded by (never
	// exceeds) this value.
	ExpiresAt time.Time
	// AudienceVerified reports whether the exchanged token's `aud` claim was
	// checked against the boot-declared RFC 8707 resource indicator. True only
	// when a resource indicator is declared AND the returned token was
	// JWT-shaped (so its `aud` could be read). False is an HONEST no-op — an
	// opaque bearer (RFC 8693 does not require a JWT) or an undeclared resource
	// leaves it false rather than a fabricated pass. A JWT whose `aud` excludes
	// the resource fails the exchange loud and emits nothing.
	AudienceVerified bool
	// ActorAsserted reports whether an RFC 8693 `actor_token` (the run's
	// verified acting principal — `agent_id`) rode the exchange. True only when
	// the provider opted in AND the ctx carried an invoking agent id. Never a
	// client-named value.
	ActorAsserted bool
}
