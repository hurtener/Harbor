package types

// governance.go — the Phase 72g (D-112) wire types for the
// `governance.posture` Protocol method. The method surfaces the
// runtime's read-only governance configuration — the D-081
// `IdentityTiers` shape — so the Console Settings page Governance
// Posture card (Phase 73m) and any third-party Console implementation
// render the same projection.
//
// Single source of truth (CLAUDE.md §8): these are THE Protocol
// governance wire types. The runtime-side `governance.Config` /
// `governance.TierConfig` structs are NOT re-exported here — the
// posture handler projects the internal shape onto these wire types so
// a future change to the internal config struct does not silently
// reshape the Protocol surface.
//
// The surface is READ-ONLY. There is no `governance.set_*` mutation
// method at V1 — operators change ceilings by editing `harbor.yaml` and
// restarting (RFC §6.15 "Hot-reloadable fields" carve-out + RFC §10
// default). Post-V1 admin methods (`governance.rotate_key`,
// `governance.swap_model`) are separate phases.

// GovernancePostureRequest is the `governance.posture` request body.
//
// TenantID is forward-looking: an empty value (the default) reads the
// caller's own tenant — no scope claim required. A non-empty value that
// differs from the caller's identity-resolved tenant is a cross-tenant
// read and requires the `auth.ScopeAdmin` scope claim (D-079 closed
// two-scope set). V1 ships a single tenant per Harbor instance, so the
// cross-tenant path is reachable in code but the value space is a
// singleton; the field exists so a post-V1 multi-tenant deployment
// finds the surface ready.
type GovernancePostureRequest struct {
	// TenantID — empty = the caller's own tenant; non-empty + different
	// from the caller's resolved tenant = requires auth.ScopeAdmin.
	TenantID string `json:"tenant_id,omitempty"`
}

// GovernancePostureResponse is the `governance.posture` response body —
// the read-only projection of the runtime's governance configuration.
//
// IdentityTiers is keyed by tier name (e.g. `"free"`, `"team"`,
// `"enterprise"`). An empty map (the latent-default boot per D-044 /
// Phase 36a) means no enforcement is configured — the Console renders an
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
	// IdentityTiers maps tier name → tier configuration (the D-081
	// shape). Always non-nil; an empty map signals no enforcement.
	IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with — same field every Protocol response carries.
	ProtocolVersion string `json:"protocol_version"`
}

// IdentityTierView is the wire projection of one governance tier's
// policy bundle (the D-081 `governance.TierConfig` shape). Every field
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
