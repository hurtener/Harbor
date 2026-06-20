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
