// Package auth ships Harbor's tool-side OAuth subsystem — the
// TokenStore + OAuthProvider seam every Phase 27 (HTTP) / Phase 28
// (MCP) / Phase 29 (A2A) southbound driver consults when a tool call
// needs a bearer token. There is one OAuth path: when no usable token
// exists the provider returns a typed ErrAuthRequired carrying a
// structured payload (provider, scope, binding-scope, authorize URL);
// the runtime emits `tool.auth_required` and parks the run via the
// unified pause/resume primitive (Phase 50 / RFC §3.3). Resume
// reattaches the freshly-minted token; A2A `AUTH_REQUIRED` translates
// into the same ErrAuthRequired so the runtime never grows a parallel
// pause path (CLAUDE.md §13 forbids two parallel implementations of
// one conceptual feature).
//
// # The binding-scope dimension
//
// Tool-side OAuth has two production patterns Harbor supports as
// first-class peers (brief 09):
//
//   - ScopeUser — the token belongs to a Harbor user; the upstream
//     service sees that user doing the action. Examples: personal
//     GitHub, Gmail, Drive, Notion. Lookups key by
//     (tenant, user_id, source).
//   - ScopeAgent — the token belongs to a Harbor agent (an
//     admin-configured service-account-style principal — Phase 53a /
//     D-059's `agent_id`); the upstream sees the agent (or a shared
//     service account). Examples: a shared Outlook mailbox, an
//     internal Snowflake service account, a Slack bot user. Lookups
//     key by (tenant, agent_id, source).
//
// BindingScope is a **declared config field on OAuthConfig**, never
// inferred from runtime state. The master-plan acceptance criterion is
// explicit: "BindingScope is a declared config field, not inferred."
//
// # agent_id is NOT an isolation principal
//
// Per CLAUDE.md §6's clarifying note + D-059: agent_id is a
// *registration identity*, not an isolation filter. Storage scopes by
// the triple (tenant, user, session); the triple is mandatory on every
// call. Agent-bound tokens carry the agent_id on the Token value (so
// the persistence-layer composite key is
// (tenant, scope, subject_id, source)) but never substitute agent_id
// for an isolation-tuple element. The session triple still gates which
// agents the call site can see and address.
//
// # Persistence: ride the §4.4 StateStore seam (D-067 pattern)
//
// TokenStore is a typed wrapper around the existing state.StateStore
// (Phase 07) — the same shape Phase 50's pause/resume coordinator and
// Phase 53a's Agent Registry use (D-067, D-068). Driver pluralism (in-
// memory / SQLite / Postgres) lives at the StateStore layer; the
// TokenStore is a single concrete type that consumes whatever
// StateStore the binary opened at boot. This avoids the §13
// two-parallel-implementations smell ("a token-store driver registry
// AND a state-store driver registry, both saying 'three V1 drivers'")
// and inherits the §4.3 deviation language Phase 50 / Phase 53a set as
// precedent. See phase-30-tool-oauth.md §"Findings I'm departing
// from".
//
// # Encryption at rest
//
// Token plaintext NEVER hits the StateStore unless wrapped through the
// package-local AES-256-GCM envelope. The KEK is operator-supplied via
// config (32 raw bytes hex-encoded); a missing / wrong-length KEK
// fails the boot loud (CLAUDE.md §13 amendment — operator-facing seam
// must demand explicit configuration; PR #91 / D-082). The encryption
// envelope carries a fresh 12-byte nonce per-Save and a 4-byte version
// header so KEK rotation (post-V1) can decrypt legacy records before
// re-encrypting under the new key.
//
// # Audit redaction
//
// Token plaintext never appears in events. The ErrAuthRequired payload
// is SafePayload by construction (provider name + authorize URL +
// scopes + binding scope + opaque state token — all caller-controllable
// surface, no token bytes). Every token-bearing audit emission flows
// through audit.Redactor; the payload type embeds events.SafeSealed so
// the bus accepts it under the typed path. Refresh tokens encrypt
// independently of access tokens — a compromised cache of access
// tokens does not yield refresh capability.
//
// # Concurrent reuse (D-025)
//
// Every constructed artifact in this package — TokenStore, the
// concrete *Provider, the AESGCMSealer — is safe to share across N
// concurrent goroutines. Mutable per-call state lives in ctx and
// arguments; per-source single-flight refresh is documented and
// guarded by a small map of *singleflight.Group keyed on
// (tenant, subject, source).
//
// # §13 primitive-with-consumer
//
// Phase 30 ships the primitive (OAuthProvider + TokenStore +
// ErrAuthRequired + the tool.auth_required event). The §13
// primitive-with-consumer obligation is discharged in-PR by:
//
//   - integration test `test/integration/phase30_tool_oauth_test.go`
//     exercising the full pause/resume cycle against a real Phase 50
//     Coordinator + a real events.EventBus + a real audit.Redactor +
//     an httptest-backed authorization server doing PKCE +
//     RFC 7591 dynamic client registration + metadata discovery, for
//     BOTH binding scopes, plus the A2A AUTH_REQUIRED convergence
//     assertion.
//   - the package-local concurrent_test.go pinning D-025 (N≥100
//     concurrent operations against one shared *Provider).
//
// Phase 31 (tool-side approval gates) will be the next consumer
// layered on the same primitives.
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// BindingScope discriminates who an OAuth token belongs to. Configured
// per OAuth attachment (per MCP server / HTTP tool / A2A peer); the
// provider routes lookups accordingly. RFC §6.4 + brief 09.
type BindingScope string

const (
	// ScopeUser — token belongs to a Harbor user. Lookups key by
	// (tenant, user, source). Each user authenticates individually.
	// The upstream service applies the user's ACLs.
	ScopeUser BindingScope = "user"

	// ScopeAgent — token belongs to a Harbor agent (admin-configured
	// service-account-style principal — Phase 53a / D-059's agent_id).
	// Lookups key by (tenant, agent_id, source). The admin
	// authenticates once during agent setup; every user invoking that
	// agent reuses the agent's token. The upstream sees the agent
	// doing the action; Harbor's audit captures
	// (originating user, agent-as-actor).
	ScopeAgent BindingScope = "agent"
)

// IsValidBindingScope reports whether s is one of the canonical
// BindingScope values.
func IsValidBindingScope(s BindingScope) bool {
	return s == ScopeUser || s == ScopeAgent
}

// OAuthConfig is the per-source OAuth attachment. The caller stores
// one OAuthConfig per (Source, BindingScope) tuple — the same source
// may have BOTH a ScopeUser AND a ScopeAgent attachment (rare but
// legal — e.g. an agent's shared mailbox AND a user's personal
// inbox); brief 09 §"Mixed-scope coexistence test" makes this a
// requirement.
type OAuthConfig struct {
	// Source is the ToolSourceID this attachment binds to. Required.
	Source tools.ToolSourceID
	// SourceName is the human-readable name surfaced in
	// ErrAuthRequired payloads. Optional.
	SourceName string
	// BindingScope is ScopeUser or ScopeAgent. Required.
	BindingScope BindingScope
	// AgentID, when BindingScope == ScopeAgent, names the
	// agent_id this attachment is bound to. Phase 53a / D-059's
	// registration identity — runtime-instance-local; not part of the
	// isolation tuple. Required when BindingScope == ScopeAgent;
	// ignored otherwise.
	AgentID string
	// ClientID is the OAuth client_id. Optional: when empty, RFC 7591
	// dynamic client registration is attempted via RegistrationURL or
	// discovered via ServerURL/.well-known/oauth-authorization-server.
	ClientID string
	// ClientSecret is the OAuth client_secret. Optional: PKCE-only
	// public clients leave it empty. NEVER logged; redacted on every
	// audit emission.
	ClientSecret string
	// AuthorizeURL is the authorization endpoint. Optional: when
	// empty, discovered from ServerURL.
	AuthorizeURL string
	// TokenURL is the token endpoint. Optional: when empty, discovered
	// from ServerURL.
	TokenURL string
	// RegistrationURL is the RFC 7591 dynamic-client-registration
	// endpoint. Optional: when set, the provider attempts dynamic
	// registration on first use when ClientID is empty.
	RegistrationURL string
	// ServerURL is the authorization server base URL for OAuth
	// metadata discovery (.well-known/oauth-authorization-server).
	// Required when AuthorizeURL / TokenURL / RegistrationURL are
	// empty and dynamic resolution is needed; optional otherwise.
	ServerURL string
	// RedirectURI is the redirect_uri the Harbor Protocol callback
	// handler exposes. Required.
	RedirectURI string
	// Scopes is the requested OAuth scopes list.
	Scopes []string
}

// Token is what TokenStore persists. The OAuthProvider is the only
// component that ever sees AccessToken / RefreshToken in plaintext —
// callers receive a copy of this struct only through Provider.Token
// (which is itself called only by trusted tool drivers immediately
// before composing the upstream request).
type Token struct {
	// Source is the ToolSourceID this token authorises.
	Source tools.ToolSourceID
	// BindingScope is ScopeUser or ScopeAgent — matches the
	// originating OAuthConfig.BindingScope.
	BindingScope BindingScope
	// TenantID is always set.
	TenantID string
	// UserID is set when BindingScope == ScopeUser; empty otherwise.
	UserID string
	// AgentID is set when BindingScope == ScopeAgent; empty otherwise.
	// Phase 53a registration identity — never substituted for an
	// isolation-tuple element.
	AgentID string
	// AccessToken — bearer credential. NEVER logged, NEVER emitted on
	// the bus, NEVER persisted in plaintext (encryption-at-rest is
	// enforced in the store layer).
	AccessToken string
	// RefreshToken — refresh credential. NEVER logged. Encrypted
	// independently of AccessToken so a compromised access cache does
	// not yield refresh capability.
	RefreshToken string
	// TokenType is conventionally "Bearer".
	TokenType string
	// ExpiresAt is the wall-clock expiry of AccessToken. Zero means
	// "no expiry advertised" — the provider treats this as
	// long-lived but still validates via 401 retry.
	ExpiresAt time.Time
	// Scopes is the scope list granted by the authorization server.
	// May be a subset of the requested scopes.
	Scopes []string
	// LastRefreshedAt is the wall-clock time of the most recent
	// successful refresh; zero on first issuance.
	LastRefreshedAt time.Time
}

// SubjectID returns the principal-side half of the persistence
// composite key: UserID for ScopeUser, AgentID for ScopeAgent. Used by
// TokenStore to construct the StateStore Kind suffix.
func (t Token) SubjectID() string {
	if t.BindingScope == ScopeUser {
		return t.UserID
	}
	if t.BindingScope == ScopeAgent {
		return t.AgentID
	}
	return ""
}

// FlowInitiation is what InitiateFlow returns: the AuthorizeURL the
// caller hands to the user / admin to complete OAuth out-of-band, plus
// the State token the callback handler quotes back to CompleteFlow.
// State doubles as the pause-record correlation key — see brief 09
// "State-as-resume-key idea."
type FlowInitiation struct {
	// AuthorizeURL is the URL the user / admin visits to grant
	// access. Includes the PKCE code_challenge query parameter.
	AuthorizeURL string
	// State is the CSRF token. The provider persists
	// (state → flow record) at InitiateFlow time and consults the
	// map at CompleteFlow time.
	State string
	// PauseToken is the unified pause/resume primitive's opaque Token
	// for the run that called InitiateFlow. The runtime uses this on
	// CompleteFlow to resume the parked run. Set when the provider
	// was constructed with a pause coordinator; empty when called
	// out-of-band (admin setup flow with no live run).
	PauseToken string
	// ExpiresAt is when the flow record (and the corresponding pause
	// record) expires. State arriving after this time is rejected
	// with ErrFlowExpired.
	ExpiresAt time.Time
	// BindingScope echoes the OAuthConfig.BindingScope for caller
	// convenience.
	BindingScope BindingScope
	// Source echoes the OAuthConfig.Source.
	Source tools.ToolSourceID
}

// ErrAuthRequired is the typed sentinel returned by Provider.Token
// when no usable token exists for (identity, source). It is also
// emitted as the `tool.auth_required` event payload (audit-redacted
// before emit).
//
// Field set is a SafePayload by construction: every field is
// caller-controllable / runtime-stamped; NEVER contains AccessToken /
// RefreshToken plaintext.
type ErrAuthRequired struct {
	// Source is the ToolSourceID that needs auth.
	Source tools.ToolSourceID
	// SourceName is the human-readable name (echoed from
	// OAuthConfig.SourceName); the Console renders this in the
	// "Connect <SourceName>" prompt.
	SourceName string
	// BindingScope discriminates user-bound vs agent-bound — drives
	// the Console UX target (user prompt vs admin banner).
	BindingScope BindingScope
	// AuthorizeURL is the URL to visit to complete OAuth.
	AuthorizeURL string
	// State is the CSRF token / flow-correlation key. NOT a secret
	// (it's a one-time-use nonce); safe to surface in events for
	// callback correlation.
	State string
	// Scopes is the scope list requested.
	Scopes []string
	// Message is human-readable advisory text. Never includes raw
	// upstream-response bytes.
	Message string
}

// Error implements the error interface.
func (e *ErrAuthRequired) Error() string {
	if e == nil {
		return "auth: <nil ErrAuthRequired>"
	}
	if e.Message != "" {
		return "auth: " + e.Message
	}
	return "auth: authentication required for source " + string(e.Source)
}

// Is supports errors.Is comparisons against the sentinel ErrAuthRequiredSentinel.
func (e *ErrAuthRequired) Is(target error) bool {
	return errors.Is(target, ErrAuthRequiredSentinel)
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrAuthRequiredSentinel — comparison target for
	// errors.Is(err, auth.ErrAuthRequiredSentinel). The typed
	// *ErrAuthRequired's Is() method matches this sentinel.
	ErrAuthRequiredSentinel = errors.New("auth: authentication required")

	// ErrIdentityRequired — a Provider / TokenStore method was called
	// with a context whose identity triple is missing or incomplete.
	// Fails closed (CLAUDE.md §6 rule 9).
	ErrIdentityRequired = errors.New("auth: identity triple incomplete")

	// ErrInvalidBindingScope — OAuthConfig.BindingScope is not one
	// of the two canonical values.
	ErrInvalidBindingScope = errors.New("auth: invalid binding scope")

	// ErrAgentIDRequired — OAuthConfig.BindingScope == ScopeAgent but
	// AgentID is empty.
	ErrAgentIDRequired = errors.New("auth: agent_id required for ScopeAgent binding")

	// ErrConfigInvalid — the OAuthConfig fails structural validation
	// (missing Source, missing RedirectURI, etc.).
	ErrConfigInvalid = errors.New("auth: oauth config invalid")

	// ErrKEKMissing — the configured TokenStore was given an empty
	// or wrong-length KEK. Fail-loud per CLAUDE.md §13 amendment.
	ErrKEKMissing = errors.New("auth: encryption KEK missing or invalid length")

	// ErrTokenNotFound — TokenStore.Get returned no record for
	// (identity, scope, source).
	ErrTokenNotFound = errors.New("auth: token not found")

	// ErrTokenCipherCorrupt — a stored token blob failed AES-GCM
	// authentication on decrypt. Surfaces tampering or wrong-KEK
	// loudly rather than returning a half-decoded record.
	ErrTokenCipherCorrupt = errors.New("auth: stored token cipher corrupt")

	// ErrFlowNotFound — CompleteFlow called with a State that has no
	// initiating record (or was already completed).
	ErrFlowNotFound = errors.New("auth: oauth flow not found for state")

	// ErrFlowExpired — CompleteFlow called after the initiating
	// record's ExpiresAt. The pause record is also cleaned up.
	ErrFlowExpired = errors.New("auth: oauth flow expired")

	// ErrStateMismatch — CompleteFlow called with a State that
	// belongs to a different identity / source than the resuming ctx.
	ErrStateMismatch = errors.New("auth: oauth flow state mismatch")

	// ErrExchangeFailed — the authorization server rejected the
	// token exchange (HTTP 4xx / non-OAuth error body).
	ErrExchangeFailed = errors.New("auth: token exchange failed")

	// ErrDiscoveryFailed — metadata discovery against
	// .well-known/oauth-authorization-server failed.
	ErrDiscoveryFailed = errors.New("auth: oauth metadata discovery failed")

	// ErrRegistrationFailed — RFC 7591 dynamic client registration
	// failed.
	ErrRegistrationFailed = errors.New("auth: dynamic client registration failed")

	// ErrProviderClosed — any operation called after Close.
	ErrProviderClosed = errors.New("auth: provider closed")

	// ErrAdminScopeRequired — a ScopeAgent flow was initiated /
	// completed / revoked without the admin scope claim. Phase 30
	// uses the existing registry.HasControlScope (Phase 53a) as the
	// in-process admin discriminator until Phase 61 wires JWT-side
	// scope claims; the call site flips the bit deliberately.
	ErrAdminScopeRequired = errors.New("auth: admin scope required for ScopeAgent flow")
)

// Validate reports whether the OAuthConfig is structurally valid.
// Returns wrapped sentinels on failure.
func (c OAuthConfig) Validate() error {
	if c.Source == "" {
		return wrap(ErrConfigInvalid, "Source empty")
	}
	if !IsValidBindingScope(c.BindingScope) {
		return wrap(ErrInvalidBindingScope, "got %q", string(c.BindingScope))
	}
	if c.BindingScope == ScopeAgent && c.AgentID == "" {
		return ErrAgentIDRequired
	}
	if c.RedirectURI == "" {
		return wrap(ErrConfigInvalid, "RedirectURI empty")
	}
	// Either ServerURL is set (discovery is possible) OR both
	// AuthorizeURL + TokenURL are set (operator pre-resolved).
	if c.ServerURL == "" && (c.AuthorizeURL == "" || c.TokenURL == "") {
		return wrap(ErrConfigInvalid, "either ServerURL OR both AuthorizeURL+TokenURL must be set")
	}
	return nil
}

// SubjectID extracts the principal-side composite-key component the
// store uses to scope lookups: UserID for ScopeUser, AgentID for
// ScopeAgent. Returns the empty string when the binding scope demands
// the missing field.
func (c OAuthConfig) SubjectID(id identity.Identity) string {
	if c.BindingScope == ScopeUser {
		return id.UserID
	}
	if c.BindingScope == ScopeAgent {
		return c.AgentID
	}
	return ""
}

// TokenStore persists access + refresh tokens with encryption at rest.
// Identity is mandatory: a missing triple fails closed
// (ErrIdentityRequired). Storage scopes by (tenant, user, session);
// the composite-key suffix includes (BindingScope, SubjectID, Source)
// — never substitutes agent_id for an isolation-tuple element.
type TokenStore interface {
	// Get returns the Token matching (ctx identity, scope, subject,
	// source). Returns (Token{}, false, nil) on miss;
	// (Token{}, false, err) on store failure or cipher corruption.
	// The store decrypts the stored ciphertext before returning;
	// callers see plaintext.
	Get(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) (Token, bool, error)

	// Put encrypts t.AccessToken / t.RefreshToken and persists. The
	// composite key is (ctx identity, t.BindingScope, t.SubjectID(),
	// t.Source). When a token already exists for that key, it is
	// overwritten (the token model is upsert, not append).
	Put(ctx context.Context, t Token) error

	// Delete removes the token at (ctx identity, scope, subject,
	// source). Idempotent — no error when the record does not exist.
	Delete(ctx context.Context, scope BindingScope, subjectID string, source tools.ToolSourceID) error
}

// OAuthProvider is the canonical contract for tool-side OAuth. It is
// transport-agnostic: Phase 27 (HTTP) / Phase 28 (MCP) / Phase 29 (A2A)
// drivers all call the same Token method. The provider routes between
// user-bound and agent-bound lookups based on the source's
// OAuthConfig.BindingScope.
//
// Identity is mandatory on every method; a missing triple fails
// closed (ErrIdentityRequired).
type OAuthProvider interface {
	// Token returns a fresh access token for (ctx identity, source).
	// The token's ownership is determined by the source's
	// OAuthConfig.BindingScope:
	//   - ScopeUser: lookup keyed by (tenant, user_id, source)
	//   - ScopeAgent: lookup keyed by (tenant, agent_id, source)
	// When no token exists or refresh fails irrecoverably, returns a
	// typed *ErrAuthRequired (which the runtime catches to emit
	// `tool.auth_required` and park the run via Phase 50). When the
	// stored token is expired, the provider attempts a single
	// in-flight refresh per (subject, source) before falling through
	// to *ErrAuthRequired.
	Token(ctx context.Context, source tools.ToolSourceID) (Token, error)

	// InitiateFlow begins an authorization-code flow. Returns the
	// AuthorizeURL the caller hands to the user / admin and the
	// State value the callback handler will quote back. For
	// ScopeAgent sources, the calling ctx MUST carry the admin scope
	// (via registry.WithControlScope) — fails ErrAdminScopeRequired
	// otherwise.
	//
	// When a pause coordinator was wired into the provider at
	// construction (the production path) AND the calling ctx carries
	// the pause request payload, InitiateFlow allocates a pause
	// record via the unified pause/resume primitive (Phase 50) and
	// returns its opaque PauseToken on FlowInitiation.PauseToken.
	InitiateFlow(ctx context.Context, source tools.ToolSourceID) (FlowInitiation, error)

	// CompleteFlow exchanges (state, code) for tokens, persists them
	// via TokenStore, and (when a pause was allocated by
	// InitiateFlow) resumes the parked run via the coordinator.
	// Returns the persisted Token (caller almost never needs it but
	// it is returned for confirmation / testing).
	CompleteFlow(ctx context.Context, state, code string) (Token, error)

	// Revoke removes the token for (ctx identity, source). For
	// ScopeAgent sources, ctx MUST carry the admin scope. Idempotent.
	Revoke(ctx context.Context, source tools.ToolSourceID) error

	// Close releases provider resources (in-flight singleflights,
	// HTTP client connections, cached metadata). Idempotent.
	Close(ctx context.Context) error
}

// wrap formats a sentinel error with %w plus a contextual detail
// message; keeps call sites compact.
func wrap(sentinel error, format string, args ...any) error {
	return joinFmt(sentinel, format, args...)
}
