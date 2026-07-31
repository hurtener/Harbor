package types

import "time"

// governance.go — the wire types for the
// `governance.posture` Protocol method. The method surfaces the
// runtime's read-only governance configuration — the consolidated
// `IdentityTiers` shape — so the Console Settings page Governance
// Posture card and any third-party Console implementation
// render the same projection.
//
// Single source of truth (CLAUDE.md §8): these are THE Protocol
// governance wire types. The runtime-side `governance.Config` /
// `governance.TierConfig` structs are NOT re-exported here — the
// posture handler projects the internal shape onto these wire types so
// a future change to the internal config struct does not silently
// reshape the Protocol surface.
//
// The `governance.posture` method is READ-ONLY (tier ceilings still
// change via `harbor.yaml` + restart — RFC §6.15 "Hot-reloadable fields"
// carve-out + RFC §10 default). The admin-scoped tenant-default override
// methods below (`governance.set_tenant_overrides` /
// `governance.get_tenant_overrides`) are the post-V1 `ModelOverride`
// governance seam: an admin sets a tenant's default LLM parameters live
// (no deploy), and the change lands on every session's next run. Key
// rotation (`governance.rotate_*`) remains a separate phase.

// `governance.posture` takes the shared posture request envelope,
// RuntimeInfoRequest — not a type of its own. The whole posture family
// (the five `runtime.*` / `metrics.*` reads plus `governance.posture` and
// `llm.posture`) is decoded into that one envelope by the control
// transport, and the CROSS-TENANT SELECTOR IS `identity.tenant`: a body
// tenant differing from the ctx-verified tenant requires the
// `auth.ScopeAdmin` / `auth.ScopeConsoleFleet` claim and emits the
// `governance.posture.read_admin` audit event.
//
// A `GovernancePostureRequest` carrying a `tenant_id` field used to be
// declared here. Nothing ever decoded it — the field was discarded, so an
// admin naming another tenant silently received its OWN posture with a
// 200. It was removed rather than implemented: the selector it duplicated
// already exists, is gated, is audited, and is tested, so implementing a
// second spelling of it would be the CLAUDE.md §13 parallel-implementation
// shape and would raise an unanswerable question the moment the two
// disagreed.

// GovernancePostureResponse is the `governance.posture` response body —
// the read-only projection of the runtime's governance configuration.
//
// IdentityTiers is keyed by tier name (e.g. `"free"`, `"team"`,
// `"enterprise"`). An empty map (the latent-default boot /
// governance) means no enforcement is configured — the Console renders an
// explicit "No tiers configured" state, never a blank panel. The map is
// always non-nil in the wire JSON (`{}`, never `null`).
type GovernancePostureResponse struct {
	// DefaultTier is the operator-configured default tier name applied
	// to an identity that does not match a custom resolver mapping.
	// Empty when no tiers are configured.
	DefaultTier string `json:"default_tier"`
	// ResolvedTier is the tier name the caller's identity resolves to
	// via the runtime's configured TierResolver. Empty when no tier
	// resolves (latent default).
	ResolvedTier string `json:"resolved_tier"`
	// IdentityTiers maps tier name → tier configuration (the consolidated
	// shape). Always non-nil; an empty map signals no enforcement.
	IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with — same field every Protocol response carries.
	ProtocolVersion string `json:"protocol_version"`
}

// IdentityTierView is the wire projection of one governance tier's
// policy bundle (the `governance.TierConfig` shape). Every field
// zero = latent for that policy.
type IdentityTierView struct {
	// BudgetCeilingUSD is the per-identity (per tier) cost ceiling in
	// USD. 0 = no ceiling.
	BudgetCeilingUSD float64 `json:"budget_ceiling_usd"`
	// RateLimit is the per-(identity, model) token-bucket configuration.
	RateLimit RateLimitView `json:"rate_limit"`
	// MaxTokens is the per-call MaxTokens cap. 0 = no enforcement.
	MaxTokens int `json:"max_tokens"`
}

// RateLimitView is the wire projection of a tier's token-bucket
// configuration. The internal `governance.RateLimitConfig` carries a
// `time.Duration` for the refill interval; the wire type carries the
// equivalent milliseconds as an int64 so the JSON is unambiguous across
// language clients (a `time.Duration` marshals as a raw nanosecond
// integer, which a non-Go client cannot interpret).
type RateLimitView struct {
	// Capacity is the bucket ceiling (max reservable tokens). 0 disables
	// the rate limit even if the refill knobs are set.
	Capacity int `json:"capacity"`
	// RefillTokens are added to the bucket every refill tick.
	RefillTokens int `json:"refill_tokens"`
	// RefillIntervalMS is the refill tick duration in milliseconds. The
	// Go side holds a time.Duration; this is its millisecond projection.
	RefillIntervalMS int64 `json:"refill_interval_ms"`
}

// GovernanceTenantOverrides is the wire projection of a tenant's default
// LLM-parameter overrides — an admin-set, no-deploy default applied to
// every session in the tenant on its NEXT run.
//
// Every field is a pointer so the absent / present distinction is
// preserved on the wire: a nil field means "the tenant inherits the
// runtime's configured default for this dimension"; a non-nil field means
// "use this value." `set_tenant_overrides` is a desired-state REPLACE: the
// supplied record wholly replaces the tenant's prior record, so a field
// that was set before and is nil now is cleared.
type GovernanceTenantOverrides struct {
	// Model, when non-nil, overrides the default model name. The runtime
	// validates it against the configured ModelProfiles at set time and
	// rejects an unknown model with CodeInvalidRequest.
	Model *string `json:"model,omitempty"`
	// ExtraInstructions, when non-nil, is an ADDITIVE block appended to
	// the agent's system prompt (rendered into the additional-guidance
	// section). It extends the prompt; it never replaces it. nil means
	// "no extension."
	ExtraInstructions *string `json:"extra_instructions,omitempty"`
	// Temperature, when non-nil, overrides the sampling temperature. The
	// runtime rejects a value outside the closed range [0, 2] with
	// CodeInvalidRequest.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens, when non-nil, overrides the per-call token ceiling. The
	// runtime rejects a non-positive value with CodeInvalidRequest.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// ReasoningEffort, when non-nil, overrides the reasoning-effort hint.
	// The runtime rejects a value outside {low, medium, high} with
	// CodeInvalidRequest.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// GovernanceSetTenantOverridesRequest is the wire request for the
// admin-scoped `governance.set_tenant_overrides` method. The override
// applies to the tenant named on the IdentityScope (the caller's verified
// tenant); the admin scope claim is required.
type GovernanceSetTenantOverridesRequest struct {
	// Identity is the request's identity scope. The triple is mandatory.
	// The runtime requires the admin scope claim and rejects a non-admin
	// caller with CodeScopeMismatch.
	Identity IdentityScope `json:"identity"`
	// Overrides is the desired-state override record (a full REPLACE of
	// the tenant's prior record).
	Overrides GovernanceTenantOverrides `json:"overrides"`
}

// GovernanceSetTenantOverridesResponse is the wire response for
// `governance.set_tenant_overrides`.
type GovernanceSetTenantOverridesResponse struct {
	// AppliedAt is the runtime timestamp at which the tenant default was
	// persisted. The new default takes effect on each session's next run.
	AppliedAt time.Time `json:"applied_at"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with, so a Console can assert wire compatibility.
	ProtocolVersion string `json:"protocol_version"`
}

// GovernanceGetTenantOverridesRequest is the wire request for the
// admin-scoped `governance.get_tenant_overrides` method.
type GovernanceGetTenantOverridesRequest struct {
	// Identity is the request's identity scope. The triple is mandatory;
	// the admin scope is required (reading a tenant's control-plane
	// defaults is a privileged action).
	Identity IdentityScope `json:"identity"`
}

// GovernanceGetTenantOverridesResponse is the wire response for
// `governance.get_tenant_overrides`.
type GovernanceGetTenantOverridesResponse struct {
	// Overrides is the tenant's current default-override record. When no
	// record exists (Set is false) every field is nil.
	Overrides GovernanceTenantOverrides `json:"overrides"`
	// Set reports whether a tenant-default record exists. False means the
	// tenant inherits every runtime config default.
	Set bool `json:"set"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with.
	ProtocolVersion string `json:"protocol_version"`
}

// GovernanceSetPostureRequest is the `governance.set_posture` request body
// — the admin-scoped WRITE sibling of `governance.posture`. It carries the
// WHOLE identity-tier policy table as a FULL REPLACE (never a partial
// merge) and REUSES the read's `IdentityTierView` + `DefaultTier` shapes.
//
// It carries NO identity / scope field: authority is derived server-side
// from the verified session, never the request body. The runtime
// requires the `auth.ScopeAdmin` claim (a non-admin caller — including one
// bearing only `console:fleet` — is rejected with CodeScopeMismatch, so a
// leaked read-only fleet token cannot widen a budget). A submitted table
// that OMITS or zeroes a ceiling the current effective policy enforces is
// rejected fail-closed with CodeInvalidRequest before any state mutation.
type GovernanceSetPostureRequest struct {
	// DefaultTier is the default-tier assignment applied to an identity
	// that resolves to no explicit tier. Required — an empty value is
	// rejected when any tier is enforced.
	DefaultTier string `json:"default_tier"`
	// IdentityTiers is the complete tier table keyed by tier name. A tier
	// present in the current effective policy but ABSENT here — or present
	// with a zeroed enforced ceiling — is a fail-closed rejection, not a
	// silent widening. Always keyed by tier name; the value is the read's
	// `IdentityTierView` projection.
	IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
}

// GovernanceSetPostureResponse echoes the persisted, resolved posture — it
// is byte-faithful to what the next `governance.posture` read returns.
type GovernanceSetPostureResponse struct {
	// DefaultTier is the persisted default-tier assignment.
	DefaultTier string `json:"default_tier"`
	// IdentityTiers is the persisted tier table (the full-replace result).
	// Always non-nil in the wire JSON (`{}`, never `null`).
	IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
	// EnforcementPendingRestart reports an honest degradation: the policy was
	// persisted and will be enforced, but this runtime booted fully latent (no
	// config tiers AND no prior record) so it composed no governance wrapper —
	// the new policy therefore does NOT enforce until the next restart (which
	// seeds a wrapper from the now-persisted record). False on a runtime that
	// booted with an active governance wrapper (the common case): the write
	// enforces on the next call. Additive field — omitted (false) is the
	// enforce-immediately posture.
	EnforcementPendingRestart bool `json:"enforcement_pending_restart"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with, so a Console can assert wire compatibility.
	ProtocolVersion string `json:"protocol_version"`
}

// GovernanceRotateKeyRequest is the wire request for the admin-scoped
// `governance.rotate_key` method — rotate the LLM provider API key live.
type GovernanceRotateKeyRequest struct {
	// Identity is the request's identity scope. The triple is mandatory;
	// the admin scope is required (rotating a provider credential is a
	// privileged action).
	Identity IdentityScope `json:"identity"`
	// Provider names the provider whose key to rotate. Empty targets the
	// runtime's bound provider (the common single-provider case); a value
	// other than the bound provider is rejected with CodeInvalidRequest
	// (multi-provider rotation is post-V1).
	Provider string `json:"provider,omitempty"`
	// Key is the new API key value. MANDATORY and a SECRET — it travels
	// only on this request leg, is held only in the runtime's atomic key
	// holder, and is NEVER logged, audited, or echoed back. An empty Key
	// is rejected with CodeInvalidRequest before any swap.
	Key string `json:"key"`
}

// GovernanceRotateKeyResponse is the wire response for
// `governance.rotate_key`. It carries NO key — only the provider, a
// non-reversible fingerprint of the new key (for audit correlation), and
// the rotation timestamp.
type GovernanceRotateKeyResponse struct {
	// Provider is the provider whose key was rotated.
	Provider string `json:"provider"`
	// Fingerprint is a non-reversible digest of the new key
	// (`sha256:<prefix>`) — correlatable, never the secret.
	Fingerprint string `json:"fingerprint"`
	// RotatedAt is the runtime timestamp at which the key was swapped. The
	// new key is effective immediately (the next call uses it).
	RotatedAt time.Time `json:"rotated_at"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with.
	ProtocolVersion string `json:"protocol_version"`
}
