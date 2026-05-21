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

// AdminImpersonationReason is the stable sentinel name for an
// `audit.admin_scope_used` event emitted by the Phase 72b
// admin-impersonation path. The Reason field of
// AdminScopeUsedPayload is set to this constant when the bus event
// comes from the impersonation gate (vs. the Phase 05
// events.Subscribe admin-filter emit, which carries the
// events.AdminScopeUsedPayload shape).
//
// Other emit sites under audit.admin_scope_used MAY add new
// sentinels (e.g. delegated-impersonation post-V1); a Protocol
// client branches on Reason, never on the wrapped human-readable
// detail.
const AdminImpersonationReason = "impersonation"

// IdentityTriple is the flat audit-visible shape of an
// IdentityScope (no nested Actor / Requester / Impersonating —
// those collapse to their triple at the payload boundary). Used as
// the Actor / Requester / Impersonating field of
// AdminScopeUsedPayload so the audit shape is purely flat strings;
// no caller-controlled bytes reach the bus.
//
// IdentityTriple is intentionally distinct from
// identity.Identity: the audit payload lives on the wire-adjacent
// bus surface, not on the runtime's identity-quadruple surface.
// Mirroring the runtime type 1:1 would couple the audit shape to
// internal storage refactors (the same anti-pattern RFC §5.1 names
// for the wire IdentityScope). Phase 72b, D-107.
type IdentityTriple struct {
	// Tenant / User / Session are the flattened `(tenant, user, session)`
	// isolation triple the audit payload records — the wire-adjacent
	// mirror of the runtime's identity quadruple (no Run: an audit row
	// records the principal, not the per-execution scope).
	Tenant  string
	User    string
	Session string
}

// AdminScopeUsedPayload is the typed payload on the
// `audit.admin_scope_used` canonical event when the emit source is
// an impersonation request (Phase 72b). The pre-existing emit site
// (the `events.Subscribe` admin-filter, Phase 05 /
// `internal/events/drivers/inmem`) continues to use
// `events.AdminScopeUsedPayload`; this richer typed payload is
// what the Phase 72b impersonation path publishes.
//
// SafePayload by construction: every field is a bounded-string
// shaped identity component plus two enum strings. No
// caller-controlled bytes reach the bus — the wire shape rejects
// any deviation at the Protocol edge before reaching the emit.
//
// Brief 11 §PG-5 verbatim names the three identity fields. The
// `Reason` field is the stable sentinel
// (`AdminImpersonationReason`); the `Method` field is the
// Protocol method that carried the impersonation (one of the ten
// canonical methods, typically `start` but `redirect` /
// `user_message` are accepted too — Phase 72b's non-goal explicitly
// names per-tool-call impersonation downgrade as post-V1, so the
// method stays one of the ten).
//
// Phase 72b, D-107.
type AdminScopeUsedPayload struct {
	events.SafeSealed
	// Actor is the verified admin identity at the Protocol edge.
	// V1 invariant: equals the JWT's verified `(tenant, user,
	// session)` triple.
	Actor IdentityTriple
	// Requester is the originating admin identity. V1 invariant:
	// equals Actor (single-hop impersonation only); diverges
	// post-V1 for delegated-impersonation chains.
	Requester IdentityTriple
	// Impersonating is the target identity the run executes
	// under. Complete `(tenant, user, session)` triple — missing
	// components fail loudly at the Protocol edge with
	// CodeIdentityRequired.
	Impersonating IdentityTriple
	// Reason is the stable sentinel name (e.g.
	// `AdminImpersonationReason = "impersonation"`).
	Reason string
	// Method is the Protocol method that carried the
	// impersonation (e.g. methods.MethodStart,
	// methods.MethodRedirect, methods.MethodUserMessage —
	// canonical method names live in internal/protocol/methods).
	Method string
}
