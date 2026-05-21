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
// task counts and the session registry's active-session count.
// EventsPerSecond and MCPConnectionsHealthy stay zero — the runtime
// exposes no bus-rate meter or MCP health roll-up at V1; reporting zero
// for a counter the runtime genuinely cannot measure is honest, not a
// silent degradation of a known value.
//
// Metrics projects the Phase 56 telemetry.MetricsRegistry's bus-fed
// counter snapshot onto the Protocol-shaped `types.MetricsSnapshot`.
package posture

import (
	"context"
	"log/slog"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// CountersProvider returns a `protocol.PostureDeps.Counters` seam that
// reads live runtime state. taskReg supplies the running / background
// task counts for the caller's session; lister supplies the
// active-session count scoped to the requested identity's tenant.
//
// The returned func never panics: a registry read error degrades that
// one counter to its zero value while the others still report — a
// posture read is a best-effort observability snapshot, never a
// load-bearing control path. A genuinely missing dependency (a nil
// taskReg / lister) is a wiring bug the caller must catch at boot, so
// CountersProvider returns a func that simply reports zeros for the
// missing subsystem rather than nil-panicking on first request.
func CountersProvider(taskReg tasks.TaskRegistry, lister sessions.SessionLister) func(context.Context, identity.Identity) types.RuntimeCounters {
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
