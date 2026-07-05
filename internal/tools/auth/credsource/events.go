package credsource

import (
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// ErrCredentialSourceUnavailable is the typed sentinel a remote
// credential fetch fails with — unreachable endpoint, non-200 status,
// or a malformed / strict-parse-rejected response. Callers `errors.Is`
// against it; the tool call fails loud (never a fallback to env, to an
// unauthenticated call, or to the interactive flow — §13).
//
// The wrapped message carries the provider name and the endpoint URL
// (operator configuration, not a secret) but NEVER the runtime's service
// token, the fetched client_id, or the fetched client_secret. The
// no-secret-bytes property is asserted by test.
var ErrCredentialSourceUnavailable = errors.New("credsource: remote credential source unavailable")

// Canonical credential-fetch event types. Registered from this
// package's init() so a Publish never trips events.ErrUnknownEventType.
// Both carry caller-safe surface only (provider name, endpoint host,
// expiry, a redacted reason) — NEVER the fetched client_id /
// client_secret / service-token bytes. Both payloads embed
// events.SafeSealed so the bus accepts them on the typed SafePayload
// path.
//
// These describe the fetch of a provider's OWN client credential (the
// runtime→authority leg) and are distinct from `tool.credential_exchanged`
// (the runtime→broker downstream-token exchange the credential enables).
const (
	// EventTypeProviderCredentialFetched — emitted once per ACTUAL
	// remote credential fetch that succeeds (a cache hit emits nothing).
	// Payload is ProviderCredentialFetchedPayload.
	EventTypeProviderCredentialFetched events.EventType = "tool.provider_credential_fetched"

	// EventTypeProviderCredentialFetchFailed — emitted when a remote
	// credential fetch fails (unreachable / non-200 / malformed). Payload
	// is ProviderCredentialFetchFailedPayload. The tool call fails loud
	// with ErrCredentialSourceUnavailable; this event makes the failure
	// observable.
	EventTypeProviderCredentialFetchFailed events.EventType = "tool.provider_credential_fetch_failed"
)

func init() {
	events.RegisterEventType(EventTypeProviderCredentialFetched)
	events.RegisterEventType(EventTypeProviderCredentialFetchFailed)
}

// ProviderCredentialFetchedPayload is the typed payload for a
// `tool.provider_credential_fetched` event. SafePayload by construction:
// every field is the runtime's own bookkeeping or operator-supplied
// configuration metadata; NO fetched-credential bytes, NO service-token
// bytes.
type ProviderCredentialFetchedPayload struct {
	events.SafeSealed
	// Provider is the operator-facing provider name whose client
	// credential was fetched.
	Provider string
	// Endpoint is the HOST of the configured remote `url` — never the
	// full URL with query material.
	Endpoint string
	// FormatVersion echoes the response's `format_version` — lets an
	// operator confirm the coordinator contract version in the audit
	// trail.
	FormatVersion int
	// ExpiresAt is the coordinator-advertised validity of the fetched
	// credential. Zero when the coordinator advertised no expiry. The
	// in-memory serve horizon never exceeds this value.
	ExpiresAt time.Time
}

// ProviderCredentialFetchFailedPayload is the typed payload for a
// `tool.provider_credential_fetch_failed` event. SafePayload by
// construction — no secret bytes.
type ProviderCredentialFetchFailedPayload struct {
	events.SafeSealed
	// Provider is the operator-facing provider name whose fetch failed.
	Provider string
	// Endpoint is the HOST of the configured remote `url`.
	Endpoint string
	// Reason is a short, redaction-safe classification of the failure
	// (e.g. "unreachable", "status 503", "malformed response"). NEVER
	// carries response bodies or credential bytes.
	Reason string
}
