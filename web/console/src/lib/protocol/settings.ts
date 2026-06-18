/**
 * Settings-page wire types — the posture + auth Protocol shapes the
 * Console Settings page (Phase 73m / D-129) consumes.
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * This module is the wire-type surface only: the response shapes the
 * `PostureNamespace` / `AuthNamespace` methods (in `client.ts`) return.
 * They mirror `internal/protocol/types/posture.go`, `governance.go`,
 * `llm.go`, and `auth.go` field-for-field — the Go side is the single
 * source (D-002 / D-093). When `cmd/harbor-gen-protocol-ts` (D-093)
 * ships, these fold into the generated `protocol.ts`.
 *
 * # Phase 73m is a pure consumer on the posture surface
 *
 * Phase 73m ships exactly ONE net-new Protocol method —
 * `auth.rotate_token`. The five `runtime.*` reads are shipped by Phase
 * 72f (D-111); `governance.posture` / `llm.posture` by Phase 72g
 * (D-112). The Settings page consumes them; it ships none of them.
 */

/** One advertised Protocol capability. */
export interface Capability {
	name: string;
	version?: string;
}

/** `runtime.info` response — the Runtime's build identity + posture. */
export interface RuntimeInfo {
	instance_id: string;
	display_name?: string;
	build_version: string;
	build_commit: string;
	build_date?: string;
	build_go_version: string;
	protocol_version: string;
	capabilities: Capability[];
	uptime_seconds: number;
	/**
	 * The MCP App (`io.modelcontextprotocol/ui`) display modes this host can
	 * render, declared by the deployment's `tools.mcp_app_host.display_modes`.
	 * The Playground reads these to seed the `ui/initialize` host-context
	 * `availableDisplayModes` it advertises to a rendered app — display modes
	 * are a host-context value, not an MCP capability. Absent when none.
	 */
	mcp_app_display_modes?: string[];
}

/** One subsystem's readiness in the `runtime.health` rollup. */
export interface SubsystemHealth {
	subsystem: string;
	status: string;
	detail?: string;
}

/** `runtime.health` response — the per-subsystem readiness rollup. */
export interface RuntimeHealth {
	subsystems: SubsystemHealth[];
}

/** One configured driver in the `runtime.drivers` response. */
export interface SubsystemDriver {
	subsystem: string;
	driver: string;
	mode?: string;
}

/** `runtime.drivers` response — the configured driver per subsystem. */
export interface RuntimeDrivers {
	subsystems: SubsystemDriver[];
}

/** Token-bucket rate-limit view in one governance tier — mirrors `types.RateLimitView`. */
export interface RateLimitView {
	capacity?: number;
	refill_tokens?: number;
	/** Refill tick duration in milliseconds (the Go side holds a `time.Duration`). */
	refill_interval_ms?: number;
}

/**
 * One identity tier's governance posture — mirrors `types.IdentityTierView`.
 * The tier NAME is the `identity_tiers` map key, not a field on the value.
 */
export interface IdentityTierView {
	budget_ceiling_usd?: number;
	max_tokens?: number;
	rate_limit?: RateLimitView;
}

/**
 * `governance.posture` response — the read-only IdentityTiers view.
 * `identity_tiers` maps tier name → tier configuration; an empty/absent map
 * signals the latent default (no enforcement).
 */
export interface GovernancePostureResponse {
	default_tier?: string;
	resolved_tier?: string;
	identity_tiers?: Record<string, IdentityTierView>;
	/** Echoes the Protocol version the Runtime answered with. */
	protocol_version?: string;
}

/** `llm.posture` response — the bound LLM provider posture. */
export interface LLMPostureResponse {
	provider?: string;
	model?: string;
	region?: string;
	/** True iff the runtime booted with HARBOR_DEV_ALLOW_MOCK=1 (D-089). */
	mock_mode?: boolean;
	/** Echoes the Protocol version the Runtime answered with. */
	protocol_version?: string;
}

/** `auth.rotate_token` response — the one-time-revealed re-minted token. */
export interface AuthRotateTokenResponse {
	new_token: string;
	expires_at: string;
}

/** The verbatim §13 / D-089 dev-mock banner text. */
export const MOCK_MODE_BANNER = 'DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION';
