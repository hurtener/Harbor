package auth

import (
	"github.com/hurtener/Harbor/internal/events"
)

// EventTypeAuthRejected is the canonical EventType emitted whenever
// the Phase 61 JWT auth pipeline rejects a request at the transport
// edge (a missing token, an algorithm-confusion attack, an expired
// bearer, an unknown kid, a scope mismatch, etc.). The event lives on
// the bus alongside every other rejection-class signal — the same
// observability surface Phase 05 + Phase 57 ship for the rest of the
// runtime — so a Console (or any Protocol client) can subscribe to
// auth rejections through the canonical events.EventBus rather than
// scraping slog output.
//
// Payload is AuthRejectedPayload; the body NEVER carries the raw
// token (CLAUDE.md §7 rule 7), only the reason sentinel name, the
// `kid` (a public header), and the optional `iss` / `sub` audited
// identifiers — all run through the audit.Redactor at the middleware
// edge before the publish.
//
// PR #91 / D-082: surfaced by the Wave 10 audit's WARN-3. Before
// this addition, auth rejections only emitted a structured
// `slog.Warn` — observable to an operator with log access but NOT to
// a Console subscribing through the Protocol's canonical event
// channel.
const EventTypeAuthRejected events.EventType = "auth.rejected"

// AuthRejectedPayload is the wire-side audit body for the
// auth.rejected event. The fields mirror what `Validator.audit`
// already emits to slog, so a subscriber sees the same redacted
// surface a log-scraping operator sees.
//
// Subject + Issuer are zero-value strings when the JWT was rejected
// before claim extraction (e.g. a malformed token / algorithm
// confusion). KID is zero-value when the rejection happened before
// the keyfunc was consulted (e.g. an `Authorization` header that
// didn't carry a Bearer scheme at all).
//
// Reason is the stable sentinel name from `reasonForWire` — one of
// `token_missing` / `token_malformed` / `alg_not_allowed` /
// `signature_invalid` / `token_expired` / `token_not_yet_valid` /
// `unknown_key` / `identity_claim_missing` / `audience_mismatch` /
// `issuer_mismatch` / `verification_failed`. A Protocol client
// branches on Reason; never on the wrapped human-readable detail
// (which may include operator-specific data we deliberately do not
// echo to an unauthenticated caller).
type AuthRejectedPayload struct {
	events.SafeSealed
	// Reason is the stable sentinel name (e.g. "token_expired").
	Reason string
	// KID is the JWT's `kid` header when known. Empty when the
	// rejection fired before the keyfunc was consulted.
	KID string
	// Issuer is the JWT's `iss` claim when known. Empty when the
	// rejection fired before claim extraction.
	Issuer string
	// Subject is the JWT's `sub` claim when known. Empty when the
	// rejection fired before claim extraction. The triple
	// (tenant/user/session) is NEVER emitted on this payload — a
	// rejected request never carries a verified identity, and
	// echoing back unverified claims would let an attacker confirm
	// which (tenant, user, session) triples are valid.
	Subject string
}

func init() {
	events.RegisterEventType(EventTypeAuthRejected)
}
