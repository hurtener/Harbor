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

/** Token-bucket rate-limit view in one governance tier. */
export interface RateLimitView {
	capacity?: number;
	refill_tokens?: number;
	refill_interval?: string;
}

/** One identity tier's governance posture. */
export interface IdentityTierView {
	tier: string;
	budget_ceiling_usd?: number;
	max_tokens?: number;
	rate_limit?: RateLimitView;
}

/** `governance.posture` response — the read-only D-081 IdentityTiers view. */
export interface GovernancePostureResponse {
	default_tier?: string;
	resolved_tier?: string;
	tiers?: IdentityTierView[];
	/** True when the governance config is the latent default (no tiers). */
	latent?: boolean;
}

/** `llm.posture` response — the bound LLM provider posture. */
export interface LLMPostureResponse {
	provider?: string;
	model?: string;
	region?: string;
	/** True iff the runtime booted with HARBOR_DEV_ALLOW_MOCK=1 (D-089). */
	mock_mode?: boolean;
}

/** `auth.rotate_token` response — the one-time-revealed re-minted token. */
export interface AuthRotateTokenResponse {
	new_token: string;
	expires_at: string;
}

/** The verbatim §13 / D-089 dev-mock banner text. */
export const MOCK_MODE_BANNER = 'DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION';
