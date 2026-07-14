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
 * One durable surface's OBSERVED oldest-retained timestamp — the honest
 * answer to "how far back does this runtime actually hold data?" a fleet
 * consumer reads BEFORE a windowed read. OBSERVED, never a configured
 * claim (Harbor has no retention knob). Mirrors `types.RetentionHorizon`.
 */
export interface RetentionHorizon {
	/** The durable surface — `events`, `tasks`, or `sessions`. */
	surface: string;
	/**
	 * The identity scope this horizon was measured at — `runtime`
	 * (identity-free, the whole retained set), `tenant`, or `session`. It
	 * makes an absent timestamp representable: `scope:"runtime"` +
	 * no-timestamp is a trustworthy empty ("the runtime retains nothing"),
	 * while `scope:"session"`/`"tenant"` + no-timestamp means the
	 * runtime-wide truth is simply not observable at the caller's scope.
	 * The `events` horizon is always `runtime`; the `tasks`/`sessions`
	 * horizons widen to `runtime` for a verified admin/console:fleet
	 * fleet-observe caller. Absent for an older/headless wiring.
	 */
	scope?: string;
	/**
	 * RFC-3339 wall-clock time of the oldest retained row at `scope`.
	 * Absent when the surface holds no rows at that scope — never a
	 * fabricated value. Read WITH `scope`.
	 */
	oldest_retained_at?: string;
}

/**
 * `runtime.health` response — a per-subsystem readiness rollup across
 * the runtime's registered subsystems, plus the observed per-surface
 * retention horizons. Mirrors `types.RuntimeHealth`.
 */
export interface RuntimeHealth {
	/** The per-subsystem readiness slice. */
	subsystems: SubsystemHealth[];
	/**
	 * The observed retention horizon per durable surface (events / tasks
	 * / sessions). Additive; absent when the runtime surfaces none.
	 */
	retention?: RetentionHorizon[];
}

// ── metrics.snapshot family ───────────────────────────────────────────
// The `metrics.snapshot` Protocol projection over the telemetry
// MetricsRegistry — counters, histograms, and the runtime observability
// gauges (active runs, engine capacity-map entries, governance cache
// entries, bus messages dropped). Mirrors `types.MetricsSnapshot` &
// friends; kept in lockstep with the wire manifest (D-223). Labels are
// the cardinality-firewalled low-cardinality set (no run_id/trace_id).

/** One counter data point. Mirrors `types.NamedCounter`. */
export interface NamedCounter {
	/** The metric name (e.g. `harbor_events_total`). */
	name: string;
	/** The counter's current cumulative value. */
	value: number;
	/** The data point's low-cardinality label set. */
	labels?: Record<string, string>;
}

/** One gauge data point. Mirrors `types.NamedGauge`. */
export interface NamedGauge {
	/** The metric name (e.g. `harbor_runtime_active_runs`). */
	name: string;
	/** The gauge's current reading. */
	value: number;
	/** The data point's low-cardinality label set (empty for runtime gauges). */
	labels?: Record<string, string>;
}

/** One cumulative histogram bucket. Mirrors `types.HistogramBucket`. */
export interface HistogramBucket {
	/** The inclusive upper bound of the bucket. */
	upper_bound: number;
	/** The cumulative count at or below `upper_bound`. */
	count: number;
}

/** One histogram data point. Mirrors `types.NamedHistogram`. */
export interface NamedHistogram {
	/** The metric name. */
	name: string;
	/** The total observation count. */
	count: number;
	/** The sum of all observed values. */
	sum: number;
	/** The cumulative buckets, if exported. */
	buckets?: HistogramBucket[];
	/** The data point's low-cardinality label set. */
	labels?: Record<string, string>;
}

/**
 * `metrics.snapshot` response — a Protocol-shaped projection over the
 * telemetry MetricsRegistry. Mirrors `types.MetricsSnapshot`.
 */
export interface MetricsSnapshot {
	/** The flat counter-data-point slice. */
	counters: NamedCounter[];
	/** The flat histogram-data-point slice. */
	histograms: NamedHistogram[];
	/** The flat gauge-data-point slice (runtime observability gauges). */
	gauges: NamedGauge[];
	/** The unix-millis timestamp the metrics were read. */
	snapshot_at: number;
}
