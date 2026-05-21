/**
 * Runtime-posture wire types — the `runtime.*` Protocol shapes the
 * Console Overview page consumes (Phase 73a / D-127).
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * This module is the wire-type surface only: the response shapes the
 * `RuntimeNamespace` methods (in `client.ts`) return. They mirror
 * `internal/protocol/types/posture.go` field-for-field — the Go side is
 * the single source (D-002 / D-093). When `cmd/harbor-gen-protocol-ts`
 * (D-093) ships, these absorb into the generated `protocol.ts`.
 *
 * # No new Protocol method (Phase 73a)
 *
 * Phase 73a ships NO new Protocol method. `runtime.counters` and
 * `runtime.health` are already shipped (Phase 72f / D-111) on the
 * `PostureSurface`, routed through the control transport at
 * `POST /v1/control/runtime.{counters,health}`. The Overview page is a
 * pure UI consumer of that surface (CLAUDE.md §13 — the page IS the
 * Stage-2 consumer Phase 72f's primitives waited for).
 */

/**
 * `runtime.counters` response — the low-cardinality live counters the
 * Overview counter row renders. Every field is a roll-up; never a
 * per-run / per-task breakdown (the Phase 56 cardinality firewall,
 * mirrored at the Protocol boundary). Mirrors `types.RuntimeCounters`.
 */
export interface RuntimeCounters {
	/** The recent bus-emit rate, events per second. */
	events_per_second: number;
	/** The count of foreground/background tasks currently running. */
	tasks_running: number;
	/** The count of background jobs in flight. */
	background_jobs_active: number;
	/** The count of healthy MCP southbound connections. */
	mcp_connections_healthy: number;
	/** The count of currently-active sessions. */
	sessions_active: number;
	/** The unix-millis timestamp the counters were read. */
	snapshot_at: number;
}

/** The closed set of subsystem-readiness states. Mirrors the Go constants. */
export type HealthStatus = 'ready' | 'degraded' | 'unavailable';

/**
 * One subsystem's readiness entry in a {@link RuntimeHealth} rollup.
 * Mirrors `types.SubsystemHealth`.
 */
export interface SubsystemHealth {
	/** The runtime subsystem name (e.g. `events`, `state`, `tasks`). */
	subsystem: string;
	/** The structural readiness — one of the three {@link HealthStatus}. */
	status: HealthStatus;
	/** An optional human-readable explanation for a non-ready subsystem. */
	detail?: string;
}

/**
 * `runtime.health` response — a per-subsystem readiness rollup across
 * the runtime's registered subsystems. Mirrors `types.RuntimeHealth`.
 */
export interface RuntimeHealth {
	/** The per-subsystem readiness slice. */
	subsystems: SubsystemHealth[];
}
