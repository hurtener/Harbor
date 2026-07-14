// Package posture builds the live `protocol.PostureDeps.Counters` and
// `protocol.PostureDeps.Metrics` seams over the real runtime
// subsystems.
//
// # Why this package exists (§17.6 F3)
//
// The posture surface takes a `Counters` and a `Metrics`
// callback. The `harbor dev` / `harbor console` boot path and the
// `harbortest/devstack` test-fixture assembler BOTH need to wire those
// callbacks to live state — not to an empty `types.RuntimeCounters{}` /
// `types.MetricsSnapshot{}` stub. A stub passes a fabricated-seam
// integration test while production returns all-zero; that is exactly
// the test↔production divergence CLAUDE.md §17.6 forbids. This package
// is the single shared implementation both call sites consume so the
// fixture cannot drift from production.
//
// Counters reads the task registry's per-identity running / background
// task counts, the session registry's active-session count, and (when
// supplied) the MCP registry's healthy-server count. EventsPerSecond
// stays zero — the runtime exposes no bus-rate meter at V1; reporting
// zero for a counter the runtime genuinely cannot measure is honest,
// not a silent degradation of a known value.
//
// Metrics projects the telemetry.MetricsRegistry's bus-fed
// counter snapshot onto the Protocol-shaped `types.MetricsSnapshot`.
package posture

import (
	"context"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// retention surface names — the durable surfaces `runtime.health`'s
// retention block reports an observed oldest-retained horizon for.
const (
	retentionSurfaceEvents   = "events"
	retentionSurfaceTasks    = "tasks"
	retentionSurfaceSessions = "sessions"
)

// utcPtr returns a pointer to t normalised to UTC — the shape
// RetentionHorizon.OldestRetainedAt uses so a nil pointer is genuinely
// omitted from the wire JSON (a zero time.Time value would not be).
func utcPtr(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

// oldestRetainedReader is the optional identity-free runtime-wide
// retention reader the tasks registry and the session lister may
// implement — the runtime-wide analogue of events.RetentionReporter.
// It reports the OBSERVED oldest-retained instant across the WHOLE
// retained set (all tenants), a bare wall-clock instant with no
// per-tenant / per-session content. It is discovered by TYPE ASSERTION
// at the wiring seam (no `Supports*` capability protocol — CLAUDE.md
// §4.4 no-ceremony; the events.RetentionReporter as-built precedent): a
// store that does not implement it contributes no runtime-wide entry
// (its horizon is honestly absent, never fabricated).
type oldestRetainedReader interface {
	// OldestRetainedAt returns the oldest-retained instant across the
	// whole store and present=true; (zero, false, nil) when the store
	// retains nothing. An error degrades that one surface to absent.
	OldestRetainedAt(ctx context.Context) (oldest time.Time, present bool, err error)
}

// RetentionProvider returns a `protocol.PostureDeps.Retention` seam that
// reports the OBSERVED oldest-retained timestamp per durable surface —
// the honest, forward-looking retention horizon a fleet consumer reads
// before issuing a windowed read. Every value is read live from the
// store; NONE is a configured claim (Harbor has no retention knob — the
// durable log is gap-free and untrimmed).
//
// Each entry carries a `scope` marker naming the identity scope the
// horizon was measured at, and — for a WIRED surface — an entry is
// ALWAYS emitted, with `OldestRetainedAt` omitted when the surface holds
// no rows AT THAT SCOPE. This makes the absence of a value representable:
// a consumer distinguishes `scope:"runtime"` + no-timestamp
// ("the runtime retains nothing — trustworthy empty") from
// `scope:"session"` / `scope:"tenant"` + no-timestamp ("nothing at your
// scope — the runtime-wide truth is not observable here").
//
//   - events: the bus's runtime-wide oldest retained event (a bare
//     wall-clock instant, no identity content), sourced through the
//     optional events.RetentionReporter capability both V1 bus drivers
//     implement. Always `scope:"runtime"`. A bus WITHOUT the reader seam
//     contributes no events entry (the reader being absent is not the
//     same as "observable and empty" — honest absence, the headless /
//     third-party-driver path); a bus WITH the reader but holding no
//     events emits `scope:"runtime"` + no timestamp.
//   - tasks: when `widened`, the runtime-wide oldest CreatedAt read
//     through the optional identity-free reader (`scope:"runtime"`); a
//     registry that omits that reader contributes no runtime-wide entry.
//     Otherwise the oldest CreatedAt across the caller's session scope
//     (`scope:"session"`).
//   - sessions: when `widened`, the runtime-wide oldest OpenedAt through
//     the optional identity-free reader (`scope:"runtime"`); a lister
//     that omits it contributes no runtime-wide entry. Otherwise the
//     oldest OpenedAt across the caller's tenant (`scope:"tenant"`;
//     open OR closed-but-retained — "oldest retained", not "oldest ever",
//     since the registry's idle GC reaps old sessions).
//
// `widened` is a SERVER-DERIVED Go input the posture surface computes
// from the caller's verified elevated scope and threads in — it is NEVER
// a wire field and NEVER read from a request body. The ordinary
// (non-widened) fold is unchanged apart from the new scope label — no
// widening, no downgrade knob (CLAUDE.md §13).
//
// A registry read error degrades that one surface to absent while the
// others still report — a posture read is a best-effort observability
// snapshot, never a load-bearing path. A nil dependency simply omits
// that surface; a nil-everything seam returns nil (the whole block is
// omitted on the wire).
func RetentionProvider(bus events.EventBus, taskReg tasks.TaskRegistry, lister sessions.SessionLister) func(context.Context, identity.Identity, bool) []types.RetentionHorizon {
	return func(ctx context.Context, id identity.Identity, widened bool) []types.RetentionHorizon {
		out := make([]types.RetentionHorizon, 0, 3)

		// events — always runtime-wide, identity-free. Present only when
		// the bus implements the reader seam (else honest absence).
		if reporter, ok := bus.(events.RetentionReporter); ok {
			entry := types.RetentionHorizon{
				Surface: retentionSurfaceEvents,
				Scope:   types.RetentionScopeRuntime,
			}
			if oldest, present, err := reporter.OldestRetainedAt(ctx); err == nil && present {
				entry.OldestRetainedAt = utcPtr(oldest)
			}
			out = append(out, entry)
		}

		if taskReg != nil {
			if h, ok := tasksHorizon(ctx, taskReg, id, widened); ok {
				out = append(out, h)
			}
		}

		if lister != nil {
			if h, ok := sessionsHorizon(ctx, lister, id, widened); ok {
				out = append(out, h)
			}
		}

		if len(out) == 0 {
			return nil
		}
		return out
	}
}

// tasksHorizon computes the `tasks` retention entry. When `widened`, it
// reads the runtime-wide oldest CreatedAt through the optional
// identity-free reader — a registry that does not implement the reader
// contributes no entry (honest absence). Otherwise it folds the caller's
// session scope via List. A read error degrades the surface to absent
// (ok=false). A wired-but-empty-at-scope surface returns an entry with
// the scope stamped and no timestamp.
func tasksHorizon(ctx context.Context, taskReg tasks.TaskRegistry, id identity.Identity, widened bool) (types.RetentionHorizon, bool) {
	if widened {
		reader, ok := taskReg.(oldestRetainedReader)
		if !ok {
			return types.RetentionHorizon{}, false
		}
		oldest, present, err := reader.OldestRetainedAt(ctx)
		if err != nil {
			return types.RetentionHorizon{}, false
		}
		entry := types.RetentionHorizon{Surface: retentionSurfaceTasks, Scope: types.RetentionScopeRuntime}
		if present {
			entry.OldestRetainedAt = utcPtr(oldest)
		}
		return entry, true
	}

	summaries, err := taskReg.List(ctx, id, tasks.TaskFilter{})
	if err != nil {
		return types.RetentionHorizon{}, false
	}
	var oldest int64
	for _, s := range summaries {
		if s.CreatedAt == 0 {
			continue
		}
		if oldest == 0 || s.CreatedAt < oldest {
			oldest = s.CreatedAt
		}
	}
	entry := types.RetentionHorizon{Surface: retentionSurfaceTasks, Scope: types.RetentionScopeSession}
	if oldest > 0 {
		entry.OldestRetainedAt = utcPtr(time.Unix(0, oldest))
	}
	return entry, true
}

// sessionsHorizon computes the `sessions` retention entry. When
// `widened`, it reads the runtime-wide oldest OpenedAt through the
// optional identity-free reader — a lister that does not implement the
// reader contributes no entry (honest absence). Otherwise it folds the
// caller's tenant via ListSnapshots. A read error degrades the surface
// to absent (ok=false). A wired-but-empty-at-scope surface returns an
// entry with the scope stamped and no timestamp.
func sessionsHorizon(ctx context.Context, lister sessions.SessionLister, id identity.Identity, widened bool) (types.RetentionHorizon, bool) {
	if widened {
		reader, ok := lister.(oldestRetainedReader)
		if !ok {
			return types.RetentionHorizon{}, false
		}
		oldest, present, err := reader.OldestRetainedAt(ctx)
		if err != nil {
			return types.RetentionHorizon{}, false
		}
		entry := types.RetentionHorizon{Surface: retentionSurfaceSessions, Scope: types.RetentionScopeRuntime}
		if present {
			entry.OldestRetainedAt = utcPtr(oldest)
		}
		return entry, true
	}

	snaps, err := lister.ListSnapshots(ctx, sessions.SessionListFilter{
		TenantIDs:     []string{id.TenantID},
		IncludeClosed: true,
	})
	if err != nil {
		return types.RetentionHorizon{}, false
	}
	var oldest time.Time
	for _, s := range snaps {
		if s.OpenedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || s.OpenedAt.Before(oldest) {
			oldest = s.OpenedAt
		}
	}
	entry := types.RetentionHorizon{Surface: retentionSurfaceSessions, Scope: types.RetentionScopeTenant}
	if !oldest.IsZero() {
		entry.OldestRetainedAt = utcPtr(oldest)
	}
	return entry, true
}

// CountersProvider returns a `protocol.PostureDeps.Counters` seam that
// reads live runtime state. taskReg supplies the running / background
// task counts for the caller's session; lister supplies the
// active-session count scoped to the requested identity's tenant;
// mcpReg supplies the healthy-MCP-connection count (server-wide, not
// per-identity — MCP servers are a runtime-shared resource).
//
// The returned func never panics: a registry read error degrades that
// one counter to its zero value while the others still report — a
// posture read is a best-effort observability snapshot, never a
// load-bearing control path. A genuinely missing dependency (a nil
// taskReg / lister / mcpReg) is a wiring bug the caller must catch at
// boot, so CountersProvider returns a func that simply reports zeros
// for the missing subsystem rather than nil-panicking on first request.
//
// a walkthrough fix: pre-fix the MCP counter was hard-coded zero
// (An earlier phase shipped previously F6 wired the MCP registry into
// the Console-facing surface). With the registry now reachable from
// the dev boot path, threading it into CountersProvider makes the
// Overview page's MCP CONNECTIONS pillar honest — it reports the
// actual count of `state == Online` servers, not a placeholder zero.
func CountersProvider(taskReg tasks.TaskRegistry, lister sessions.SessionLister, mcpReg *mcpdrv.Registry) func(context.Context, identity.Identity) types.RuntimeCounters {
	return func(ctx context.Context, id identity.Identity) types.RuntimeCounters {
		var c types.RuntimeCounters

		if taskReg != nil {
			summaries, err := taskReg.List(ctx, id, tasks.TaskFilter{})
			if err == nil {
				for _, s := range summaries {
					if s.Status != tasks.StatusRunning {
						continue
					}
					c.TasksRunning++
					if s.Kind == tasks.KindBackground {
						c.BackgroundJobsActive++
					}
				}
			}
		}

		if lister != nil {
			// IncludeClosed defaults false — ListSnapshots returns only
			// open sessions, so every returned row is an active session.
			snaps, err := lister.ListSnapshots(ctx, sessions.SessionListFilter{
				TenantIDs: []string{id.TenantID},
			})
			if err == nil {
				c.SessionsActive = int64(len(snaps))
			}
		}

		if mcpReg != nil {
			// ListServers is identity-mandatory; the caller's identity is
			// already in ctx via the request pipeline. The filter is
			// empty (every server the caller can see). MCP servers are
			// not isolation-scoped resources, but ListServers respects
			// the identity gate, so we propagate the caller's ctx.
			snaps, _, err := mcpReg.ListServers(ctx, mcpdrv.ListFilter{})
			if err == nil {
				for _, s := range snaps {
					if s.State == mcpdrv.ServerStateOnline {
						c.MCPConnectionsHealthy++
					}
				}
			}
		}

		return c
	}
}

// MetricsProvider returns a `protocol.PostureDeps.Metrics` seam that
// projects the telemetry.MetricsRegistry's live counter
// snapshot onto the Protocol-shaped `types.MetricsSnapshot`.
//
// A registry Snapshot failure is logged at Warn and degrades to an
// empty (but non-nil) snapshot — a metrics read failure must not fail
// the whole posture request, but it is never silent (CLAUDE.md §13:
// the failure is surfaced in the log, not swallowed).
func MetricsProvider(reg *telemetry.MetricsRegistry, log *slog.Logger) func(context.Context) types.MetricsSnapshot {
	return func(ctx context.Context) types.MetricsSnapshot {
		snap, err := reg.Snapshot(ctx)
		if err != nil {
			if log != nil {
				log.WarnContext(ctx, "metrics.snapshot: registry collect failed",
					slog.Any("error", err))
			}
			return types.MetricsSnapshot{Counters: []types.NamedCounter{}}
		}
		counters := make([]types.NamedCounter, 0, len(snap.Counters))
		for _, cp := range snap.Counters {
			counters = append(counters, types.NamedCounter{
				Name:   cp.Name,
				Value:  cp.Value,
				Labels: cp.Labels,
			})
		}
		gauges := make([]types.NamedGauge, 0, len(snap.Gauges))
		for _, gp := range snap.Gauges {
			gauges = append(gauges, types.NamedGauge{
				Name:   gp.Name,
				Value:  gp.Value,
				Labels: gp.Labels,
			})
		}
		return types.MetricsSnapshot{Counters: counters, Gauges: gauges}
	}
}

// HealthFromConfig builds the `runtime.health` seam from the
// resolved config. The in-process dev / devstack assembly is fully wired
// by the time the posture surface is constructed, so every
// persistence-shaped subsystem reports `ready`.
//
// This is the single shared implementation consumed by BOTH the
// `harbor dev` / `harbor console` boot path and the
// `harbortest/devstack` fixture assembler — neither hand-rolls its own
// copy, so the fixture cannot drift from production (CLAUDE.md §17.6;
// a checkpoint cleanup).
func HealthFromConfig(cfg *config.Config) []types.SubsystemHealth {
	subs := []string{"state", "events"}
	if cfg.Artifacts.Driver != "" {
		subs = append(subs, "artifacts")
	}
	if cfg.Memory.Driver != "" {
		subs = append(subs, "memory")
	}
	out := make([]types.SubsystemHealth, 0, len(subs))
	for _, s := range subs {
		out = append(out, types.SubsystemHealth{Subsystem: s, Status: types.HealthStatusReady})
	}
	return out
}

// DriversFromConfig builds the `runtime.drivers` seam — the
// configured driver name per persistence-shaped subsystem. Never the
// DSN (CLAUDE.md §7) — the driver name only.
//
// Like HealthFromConfig, this is the single shared implementation both
// the production boot path and the devstack fixture assembler consume.
func DriversFromConfig(cfg *config.Config) []types.SubsystemDriver {
	out := []types.SubsystemDriver{
		{Subsystem: "state", Driver: cfg.State.Driver},
	}
	if cfg.Artifacts.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "artifacts", Driver: cfg.Artifacts.Driver})
	}
	if cfg.Memory.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "memory", Driver: cfg.Memory.Driver})
	}
	if cfg.Events.Driver != "" {
		out = append(out, types.SubsystemDriver{Subsystem: "events", Driver: cfg.Events.Driver})
	}
	return out
}
