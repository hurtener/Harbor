// Package posture builds the live `protocol.PostureDeps.Counters` and
// `protocol.PostureDeps.Metrics` seams over the real runtime
// subsystems.
//
// # Why this package exists (§17.6 F3)
//
// The Phase 72f posture surface takes a `Counters` and a `Metrics`
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
// Metrics projects the Phase 56 telemetry.MetricsRegistry's bus-fed
// counter snapshot onto the Protocol-shaped `types.MetricsSnapshot`.
package posture

import (
	"context"
	"log/slog"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

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
// Round-5 walkthrough fix: pre-fix the MCP counter was hard-coded zero
// (Phase 73i shipped before Phase 83w F6 wired the MCP registry into
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
// projects the Phase 56 telemetry.MetricsRegistry's live counter
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
		return types.MetricsSnapshot{Counters: counters}
	}
}

// HealthFromConfig builds the Phase 72f `runtime.health` seam from the
// resolved config. The in-process dev / devstack assembly is fully wired
// by the time the posture surface is constructed, so every
// persistence-shaped subsystem reports `ready`.
//
// This is the single shared implementation consumed by BOTH the
// `harbor dev` / `harbor console` boot path and the
// `harbortest/devstack` fixture assembler — neither hand-rolls its own
// copy, so the fixture cannot drift from production (CLAUDE.md §17.6;
// D-132 / Wave 13 NIT cleanup).
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

// DriversFromConfig builds the Phase 72f `runtime.drivers` seam — the
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
