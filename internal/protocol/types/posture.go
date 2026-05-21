package types

// Phase 72f (D-111) — the runtime-posture wire types.
//
// Phase 72f ships five read-only Protocol methods that expose the live
// Runtime's posture to a Protocol client (Console, CLI, third-party).
// The wire types below are the response shapes; RuntimeInfoRequest is
// the shared request shape (the methods are read-only and take no
// payload beyond the identity scope).
//
// Every type here is a flat, Protocol-owned struct — never a re-export
// of an internal Runtime Go type (RFC §5.1 reject-on-sight smell). In
// particular, MetricsSnapshot is a Protocol-shaped projection over the
// Phase 56 telemetry.MetricsRegistry: it carries flat numbers, NOT the
// OpenTelemetry SDK's metric types. internal/protocol/types/posture.go
// deliberately imports no OTel package; the static smoke guard pins it.

// RuntimeInfoRequest is the shared request shape for the five Phase 72f
// posture methods. It carries only the identity scope — the methods are
// read-only and take no payload.
//
// Identity is mandatory at the Protocol edge (RFC §5.5). The Tenant
// field is required for every posture method; the User and Session
// fields are required when the projection is session-scoped
// (runtime.counters returns tenant-wide rollups for a Tenant-only
// scope, the session's slice when a Session is supplied). A cross-tenant
// request — Identity.Tenant differing from the caller's verified tenant
// — requires the admin scope per D-079.
type RuntimeInfoRequest struct {
	// Identity is the caller's identity scope. The PostureSurface
	// validates the triple and gates cross-tenant reads on the admin
	// scope claim.
	Identity IdentityScope `json:"identity"`
}

// RuntimeInfo is the runtime.info response: the Runtime's build
// identity, Protocol version, advertised capabilities, uptime, and
// operator-facing identifiers. A Console attached to multiple Runtimes
// uses InstanceID as the per-attachment stable key.
type RuntimeInfo struct {
	// InstanceID is a stable per-deployment identifier minted at boot.
	// A Console managing several Runtimes keys each attachment by it.
	InstanceID string `json:"instance_id"`
	// DisplayName is the operator-configured friendly name for this
	// Runtime. Empty when the operator configured none — never the
	// host's machine name without explicit operator config.
	DisplayName string `json:"display_name,omitempty"`
	// BuildVersion is the Harbor build version string (e.g. a release
	// tag or a `v0.0.0-dev` dev marker).
	BuildVersion string `json:"build_version"`
	// BuildCommit is the VCS revision the binary was built from, or
	// "unknown" when the build carried no VCS stamp.
	BuildCommit string `json:"build_commit"`
	// BuildDate is the build timestamp (RFC 3339), or empty when the
	// build carried no date stamp.
	BuildDate string `json:"build_date,omitempty"`
	// BuildGoVersion is the Go toolchain version the binary was built
	// with.
	BuildGoVersion string `json:"build_go_version"`
	// ProtocolVersion is the pinned Harbor Protocol version the Runtime
	// speaks (the ProtocolVersion constant). A client parses it and
	// checks Version.Compatible against its own.
	ProtocolVersion string `json:"protocol_version"`
	// Capabilities is the set of Protocol surfaces the Runtime
	// advertises as live — the same set CurrentHandshake() carries.
	Capabilities []Capability `json:"capabilities"`
	// UptimeSeconds is the number of whole seconds since the Runtime
	// process started.
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// SubsystemHealth is one subsystem's readiness entry in a RuntimeHealth
// rollup.
type SubsystemHealth struct {
	// Subsystem names the runtime subsystem (e.g. "events", "state",
	// "tasks").
	Subsystem string `json:"subsystem"`
	// Status is the structural readiness — one of "ready", "degraded",
	// or "unavailable". Read from the subsystem's own posture seam, not
	// from a synthetic deep-check probe.
	Status string `json:"status"`
	// Detail is an optional human-readable explanation — populated for
	// a degraded / unavailable subsystem with the reason.
	Detail string `json:"detail,omitempty"`
}

// The canonical SubsystemHealth.Status values. The set is closed: a
// posture seam reports one of exactly these three.
const (
	// HealthStatusReady — the subsystem is registered and healthy.
	HealthStatusReady = "ready"
	// HealthStatusDegraded — the subsystem is registered and partially
	// functional.
	HealthStatusDegraded = "degraded"
	// HealthStatusUnavailable — the subsystem is not registered, or is
	// registered but non-functional.
	HealthStatusUnavailable = "unavailable"
)

// RuntimeHealth is the runtime.health response: a per-subsystem
// readiness rollup across the runtime's registered subsystems.
type RuntimeHealth struct {
	// Subsystems is the per-subsystem readiness slice — one entry per
	// subsystem the runtime knows about.
	Subsystems []SubsystemHealth `json:"subsystems"`
}

// RuntimeCounters is the runtime.counters response: the low-cardinality
// live counters the Console footer / sidebar chips render. Every field
// is a roll-up — never a per-run / per-task / per-session breakdown
// (the Phase 56 cardinality firewall posture, mirrored at the Protocol
// boundary).
type RuntimeCounters struct {
	// EventsPerSecond is the recent bus-emit rate.
	EventsPerSecond float64 `json:"events_per_second"`
	// TasksRunning is the count of foreground/background tasks
	// currently in a running state.
	TasksRunning int64 `json:"tasks_running"`
	// BackgroundJobsActive is the count of background jobs in flight.
	BackgroundJobsActive int64 `json:"background_jobs_active"`
	// MCPConnectionsHealthy is the count of healthy MCP southbound
	// connections.
	MCPConnectionsHealthy int64 `json:"mcp_connections_healthy"`
	// SessionsActive is the count of currently-active sessions.
	SessionsActive int64 `json:"sessions_active"`
	// SnapshotAt is the unix-millis timestamp the counters were read.
	SnapshotAt int64 `json:"snapshot_at"`
}

// SubsystemDriver is one persistence-shaped subsystem's configured
// driver entry in a RuntimeDrivers readout.
type SubsystemDriver struct {
	// Subsystem names the persistence-shaped subsystem ("state",
	// "artifacts", "memory", "eventlog").
	Subsystem string `json:"subsystem"`
	// Driver is the configured driver name ("inmem", "sqlite",
	// "postgres", ...). Never the DSN / connection string — only the
	// driver name leaks across the Protocol boundary.
	Driver string `json:"driver"`
	// Mode is an optional posture detail ("readwrite", "readonly",
	// "embedded"). Empty when the subsystem reports none.
	Mode string `json:"mode,omitempty"`
}

// RuntimeDrivers is the runtime.drivers response: the configured driver
// names per persistence-shaped subsystem so an operator can see whether
// the Runtime is dev (in-mem) or production (Postgres) without grepping
// config.
type RuntimeDrivers struct {
	// Subsystems is the per-subsystem driver slice.
	Subsystems []SubsystemDriver `json:"subsystems"`
}

// NamedCounter is one counter metric in a MetricsSnapshot: a name, its
// current monotonic value, and its low-cardinality labels.
type NamedCounter struct {
	// Name is the metric name.
	Name string `json:"name"`
	// Value is the counter's current value.
	Value float64 `json:"value"`
	// Labels carries the metric's low-cardinality label set. A
	// high-cardinality label (run_id / trace_id / span_id) never appears
	// here — the Phase 56 cardinality firewall gates it on the SDK side,
	// and MetricsSnapshot.HasHighCardinalityLabel is the wire-boundary
	// cardinalitylint guard (exercised by the posture type tests).
	Labels map[string]string `json:"labels,omitempty"`
}

// HistogramBucket is one cumulative bucket in a NamedHistogram.
type HistogramBucket struct {
	// UpperBound is the inclusive upper bound of the bucket.
	UpperBound float64 `json:"upper_bound"`
	// Count is the cumulative observation count at or below
	// UpperBound.
	Count uint64 `json:"count"`
}

// NamedHistogram is one histogram metric in a MetricsSnapshot.
type NamedHistogram struct {
	// Name is the metric name.
	Name string `json:"name"`
	// Count is the total observation count.
	Count uint64 `json:"count"`
	// Sum is the sum of all observed values.
	Sum float64 `json:"sum"`
	// Buckets is the cumulative per-bucket count slice.
	Buckets []HistogramBucket `json:"buckets,omitempty"`
	// Labels carries the metric's low-cardinality label set.
	Labels map[string]string `json:"labels,omitempty"`
}

// NamedGauge is one gauge metric in a MetricsSnapshot.
type NamedGauge struct {
	// Name is the metric name.
	Name string `json:"name"`
	// Value is the gauge's current value.
	Value float64 `json:"value"`
	// Labels carries the metric's low-cardinality label set.
	Labels map[string]string `json:"labels,omitempty"`
}

// MetricsSnapshot is the metrics.snapshot response: a Protocol-shaped
// projection over the Phase 56 telemetry.MetricsRegistry. The wire
// shape is a flat slice per metric kind — counters, histograms, gauges
// — carrying plain numbers. NO OpenTelemetry SDK type crosses the
// Protocol boundary (RFC §5.1 / CLAUDE.md §13 single-source rule).
type MetricsSnapshot struct {
	// Counters is the flat counter-metric slice.
	Counters []NamedCounter `json:"counters"`
	// Histograms is the flat histogram-metric slice.
	Histograms []NamedHistogram `json:"histograms"`
	// Gauges is the flat gauge-metric slice.
	Gauges []NamedGauge `json:"gauges"`
	// SnapshotAt is the unix-millis timestamp the metrics were read.
	SnapshotAt int64 `json:"snapshot_at"`
}

// HighCardinalityLabelKeys is the closed set of label keys that must
// NEVER appear on a metric crossing the Protocol boundary — they are
// unbounded per-run identifiers and would explode the metric series
// cardinality. The Phase 56 telemetry cardinality firewall gates these
// on the SDK side; this set lets the wire boundary re-check (D-132 /
// Wave 13 NIT cleanup, mirroring the Phase 56 label-lint pattern).
var HighCardinalityLabelKeys = []string{"run_id", "trace_id", "span_id"}

// HasHighCardinalityLabel reports the first (metric-name, label-key)
// pair in the snapshot that carries a forbidden high-cardinality label
// key, or empty strings + false when the snapshot is clean. It is the
// `cardinalitylint`-style guard for the metrics.snapshot wire boundary:
// a projection that lets a `run_id` / `trace_id` / `span_id` label reach
// the wire is a cardinality-explosion bug.
func (m MetricsSnapshot) HasHighCardinalityLabel() (metric, label string, found bool) {
	check := func(name string, labels map[string]string) (string, string, bool) {
		for _, forbidden := range HighCardinalityLabelKeys {
			if _, ok := labels[forbidden]; ok {
				return name, forbidden, true
			}
		}
		return "", "", false
	}
	for _, c := range m.Counters {
		if mn, lk, ok := check(c.Name, c.Labels); ok {
			return mn, lk, true
		}
	}
	for _, h := range m.Histograms {
		if mn, lk, ok := check(h.Name, h.Labels); ok {
			return mn, lk, true
		}
	}
	for _, g := range m.Gauges {
		if mn, lk, ok := check(g.Name, g.Labels); ok {
			return mn, lk, true
		}
	}
	return "", "", false
}
