/**
 * Governance admin-control wire types — the `governance.set_tenant_overrides`
 * / `governance.get_tenant_overrides` Protocol shapes the Console admin
 * control consumes.
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * These mirror `internal/protocol/types/governance.go` field-for-field —
 * the Go side is the single source (D-002 / D-093), kept in lockstep with
 * `wire-manifest.gen.json` by `npm run lint`
 * (`check-protocol-ts-lockstep.mjs`). A dropped/renamed/mistyped field
 * fails the guard. The `GovernanceNamespace` (in `client.ts`) issues the
 * calls; these are the request/response shapes it sends and narrows.
 *
 * The admin tenant-default overrides are a no-deploy reconfiguration of a
 * tenant's default LLM parameters, applied to every session in the tenant
 * on its next run (Phase 92, D-231). This typed surface is the Phase 92b
 * Console consumer (D-232) that retires the prior allow-list entries.
 */

import type { IdentityScope } from './memory-types';
import type { IdentityTierView } from './settings';

/**
 * A tenant's default LLM-parameter overrides — every field optional (a nil
 * field means "inherit the runtime's configured default"). `set_tenant_overrides`
 * is a desired-state REPLACE. Mirrors `types.GovernanceTenantOverrides`.
 */
export interface GovernanceTenantOverrides {
	/** Default model name (validated against configured ModelProfiles). */
	model?: string;
	/** Additive extra-instructions appended to the agent's system prompt. */
	extra_instructions?: string;
	/** Sampling temperature, [0, 2]. */
	temperature?: number;
	/** Per-call token ceiling. */
	max_tokens?: number;
	/** Reasoning-effort hint ({low, medium, high}). */
	reasoning_effort?: string;
}

/** `governance.set_tenant_overrides` request — admin-scoped. */
export interface GovernanceSetTenantOverridesRequest {
	identity: IdentityScope;
	overrides: GovernanceTenantOverrides;
}

/** `governance.set_tenant_overrides` response. */
export interface GovernanceSetTenantOverridesResponse {
	/** Runtime timestamp the tenant default was persisted (RFC3339). */
	applied_at: string;
	protocol_version: string;
}

/** `governance.get_tenant_overrides` request — admin-scoped. */
export interface GovernanceGetTenantOverridesRequest {
	identity: IdentityScope;
}

/** `governance.get_tenant_overrides` response. */
export interface GovernanceGetTenantOverridesResponse {
	/** The tenant's current default-override record (all fields nil when unset). */
	overrides: GovernanceTenantOverrides;
	/** Whether a tenant-default record exists; false = inherits config. */
	set: boolean;
	protocol_version: string;
}

/**
 * `governance.set_posture` request — the admin-scoped WRITE sibling of
 * `governance.posture`. Mirrors `types.GovernanceSetPostureRequest`.
 *
 * The write is a FULL REPLACE of the whole identity-tier policy table
 * (never a partial merge) and carries NO identity/scope field — authority is
 * server-side from the verified session (D-219). Requires the `admin` scope
 * claim ONLY (a `console:fleet`-only token — the read's second gate — is
 * rejected 403). A table that omits or zeroes a currently-enforced ceiling
 * is rejected fail-closed (400) before any state mutation.
 */
export interface GovernanceSetPostureRequest {
	/** Default-tier assignment; required when any tier is enforced. */
	default_tier: string;
	/** The complete tier table (a full replace), keyed by tier name. */
	identity_tiers: Record<string, IdentityTierView>;
}

/**
 * `governance.set_posture` response — echoes the persisted, resolved posture
 * (byte-faithful to what the next `governance.posture` read returns).
 * Mirrors `types.GovernanceSetPostureResponse`.
 */
export interface GovernanceSetPostureResponse {
	default_tier: string;
	identity_tiers: Record<string, IdentityTierView>;
	/**
	 * True when the policy persisted but this runtime booted fully latent (no
	 * config tiers AND no prior record) so it composed no governance wrapper —
	 * the new policy does NOT enforce until the next restart. False (the common
	 * case) means the write enforces on the next call.
	 */
	enforcement_pending_restart: boolean;
	protocol_version: string;
}
