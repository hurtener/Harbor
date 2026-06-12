// internal/events/emitter.go — the per-run identity-stamping Emit
// constructor.
//
// The planner's telemetry seam is `planner.RunContext.Emit
// (func(events.Event))`: the planner emits `planner.decision` /
// `planner.finish` / `planner.repair_guidance_injected` through it
// and the runtime guarantees the run's identity quadruple is attached
// before the event reaches the bus. Previously that guarantee lived
// as a per-run closure hand-written in `cmd/harbor`'s run loop (and
// absent from the devstack mirror, leaving planner telemetry silently
// dead on the official test surface). The constructor below makes the
// rule un-forgettable at every call site.

package events

import (
	"context"
	"log/slog"

	"github.com/hurtener/Harbor/internal/identity"
)

// IdentityStampingEmitter returns the per-run Emit closure a run-loop
// driver hands to `planner.RunContext.Emit`. The
// closure:
//
//   - stamps the run's identity quadruple `q` on any event whose
//     Identity is missing (TenantID empty) — a pre-set identity is
//     preserved so a planner that already scoped its event wins;
//   - publishes onto `bus`;
//   - Warns loudly on publish failure (bus publishing
//     failures must be surfaced, not swallowed) — a closed bus mid-run
//     logs rather than races or panics.
//
// The returned type matches `planner.RunContext.Emit`
// (`func(events.Event)`) without this package importing `planner`;
// the compile-shaped assertion lives in `internal/runtime/runctx`'s
// tests (a package that may import both).
//
// The publish context is `context.Background()` — the documented
// bridge across an unmanaged async boundary (CLAUDE.md §5) for callers
// that genuinely have no lifetime ctx. Run-loop drivers are NOT that
// caller: they own a driver-lifetime ctx and MUST use
// [IdentityStampingEmitterContext] so publishes are bounded by it —
// the durable bus driver drives its `store.Save` with the publish ctx,
// and an unbounded Background ctx silently outlived driver Close
// (a recorded correction, since resolved).
//
// Concurrent reuse: the constructor allocates no shared
// mutable state — each run constructs its own closure over the run's
// quadruple; N concurrent runs see N independent closures over one
// shared (concurrent-safe) bus.
//
// A nil logger defaults to slog.Default() so the failure path stays
// loud for every caller.
func IdentityStampingEmitter(bus EventBus, q identity.Quadruple, logger *slog.Logger) func(Event) {
	return IdentityStampingEmitterContext(context.Background(), bus, q, logger)
}

// IdentityStampingEmitterContext is [IdentityStampingEmitter] with a
// caller-supplied base context bounding every publish (closing
// the correction note). Run-loop drivers pass their
// driver-lifetime ctx (the pre-110b `d.subCtx` semantics): on the
// durable bus driver — which persists each event via `store.Save`
// under the publish ctx — cancelling baseCtx stops persistence at
// driver teardown instead of letting late emits write past Close.
// Publish failures (including baseCtx cancellation) Warn loudly,
// never silently drop.
//
// A nil baseCtx falls back to context.Background() (the ctx-less
// constructor's documented bridge).
func IdentityStampingEmitterContext(baseCtx context.Context, bus EventBus, q identity.Quadruple, logger *slog.Logger) func(Event) {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(ev Event) {
		if ev.Identity.TenantID == "" {
			ev.Identity = q
		}
		if pubErr := bus.Publish(baseCtx, ev); pubErr != nil {
			logger.Warn("events: emitter publish failed",
				slog.String("type", string(ev.Type)),
				slog.String("run_id", q.RunID),
				slog.String("err", pubErr.Error()))
		}
	}
}
