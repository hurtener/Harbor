package engine

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// DefaultQueueSize is the bounded per-adjacency channel capacity when
// no override is configured. RFC §6.1 settles the value at 64
// (resolves brief 01 Q-4). Engine-wide override via WithQueueSize(n);
// per-channel override via WithChannelOverride(from, to, n).
const DefaultQueueSize = 64

// RunErrorHandler is the callback the engine fires on terminal node
// failure. The engine's default (when no handler is configured) is a
// no-op AFTER the engine's slog.Logger has logged the failure — the
// handler is the seam production wiring uses to route the structured
// RunError to Phase 04's *telemetry.Logger.Error so the wave-2
// BusEmitter adapter publishes a runtime.error event.
//
// Decoupled by design: the engine package does not import
// internal/telemetry. Callers who want bus-side runtime.error events
// pass a callback that invokes Logger.Error with the RunError's
// structured attrs.
type RunErrorHandler func(ctx context.Context, re *RunError)

// RunCancelledNotice is the payload the engine hands to a
// RunCancelledHandler after Cancel(runID). The handler is the seam
// production wiring uses to publish runtime.run_cancelled on the bus
// without the engine importing the events package.
//
// CancelledAt is wall-clock; DroppedEnvelopeCount counts the
// envelopes Cancel drained from channels (step 2 of the four-step
// propagation). Useful for operators measuring "how loaded was the
// cancelled run."
type RunCancelledNotice struct {
	CancelledAt          time.Time
	RunID                string
	DroppedEnvelopeCount int64
}

// RunCancelledHandler is the callback the engine fires after a Cancel
// observed an active run. Same hook pattern as RunErrorHandler:
// production wiring (cmd/harbor) installs a handler that publishes a
// runtime.run_cancelled event on the bus; tests can install a
// recording callback.
//
// Best-effort: a panic is recovered; bus errors must not block Cancel
// from returning.
type RunCancelledHandler func(ctx context.Context, n RunCancelledNotice)

// engineConfig captures the New-time options. Internally consumed by
// the engine's constructor; never exported.
type engineConfig struct {
	eventBus            events.EventBus
	channelOverrides    map[channelKey]int
	runErrorHandler     RunErrorHandler
	runCancelledHandler RunCancelledHandler
	queueSize           int
	cancelTTL           time.Duration
	errorEmitToEgress   bool
}

// channelKey is the (from, to) pair used to key per-channel queue
// overrides. Internal only.
type channelKey struct {
	from string
	to   string
}

// Option configures an engine at construction.
type Option func(*engineConfig)

// WithQueueSize overrides the engine-wide bounded per-adjacency
// channel capacity (default DefaultQueueSize). n must be > 0; New
// returns ErrInvalidQueueSize otherwise.
func WithQueueSize(n int) Option {
	return func(cfg *engineConfig) {
		cfg.queueSize = n
	}
}

// WithChannelOverride sets a per-channel queue size for the
// (from -> to) edge. Wins over WithQueueSize for that specific edge.
func WithChannelOverride(from, to NodeRef, n int) Option {
	return func(cfg *engineConfig) {
		if cfg.channelOverrides == nil {
			cfg.channelOverrides = make(map[channelKey]int)
		}
		cfg.channelOverrides[channelKey{from: from.Name, to: to.Name}] = n
	}
}

// WithErrorEmissionToEgress toggles whether internal worker errors
// (Phase 11's RunError) ALSO land on the egress channel as a special
// error-shaped envelope. Default is false: errors go to Phase 04's
// logger + Phase 05's bus (via the configured RunErrorHandler) only.
//
// Operators who want to consume errors via Fetch (instead of via the
// bus) opt in here. The egress envelope's Payload is the *RunError;
// callers Fetch and type-assert.
func WithErrorEmissionToEgress(enabled bool) Option {
	return func(cfg *engineConfig) {
		cfg.errorEmitToEgress = enabled
	}
}

// WithRunErrorHandler installs the callback the engine fires on
// terminal node failure. Production wiring passes a callback that
// invokes telemetry.Logger.Error so the wave-2 BusEmitter adapter
// publishes a runtime.error event. Tests can install a recording
// callback to assert the structured RunError shape directly.
//
// When unset, the engine logs the failure via its slog.Logger only
// (Phase 10 behavior). The handler is invoked AFTER the slog log so
// both paths see the failure regardless of the handler's outcome.
func WithRunErrorHandler(h RunErrorHandler) Option {
	return func(cfg *engineConfig) {
		cfg.runErrorHandler = h
	}
}

// WithRunCancelledHandler installs the callback the engine fires
// after Cancel(runID) observed an active run. Production wiring
// translates the notice to a runtime.run_cancelled bus event;
// tests can use a recording callback. Phase 13.
func WithRunCancelledHandler(h RunCancelledHandler) Option {
	return func(cfg *engineConfig) {
		cfg.runCancelledHandler = h
	}
}

// WithEventBus wires the canonical events.EventBus the engine
// publishes its construction-time `topology.changed` event onto
// (Phase 74 / D-114). The event carries the engine's initial
// TopologyProjection so a Protocol consumer that subscribed before
// the engine was built catches the graph the moment it exists.
//
// The option is additive: an engine constructed WITHOUT WithEventBus
// (the Phase 02 default) publishes nothing — every existing engine
// test sees zero behavioural change and Phase 02 callers gain no new
// mandatory dependency. A nil bus passed to WithEventBus is treated
// as "WithEventBus not supplied".
//
// When a bus IS supplied and it rejects the construction-time event,
// New fails loud (CLAUDE.md §5) rather than building an engine whose
// topology surface silently never reached the bus.
func WithEventBus(b events.EventBus) Option {
	return func(cfg *engineConfig) {
		if b != nil {
			cfg.eventBus = b
		}
	}
}

// EmitOption is the Emit-time option type. Phase 12 added
// WithRunCapacity for per-run streaming backpressure overrides.
type EmitOption func(*emitOptions)

type emitOptions struct {
	// runCapacity, when non-zero, overrides Policy.RunCapacity for
	// the run initiated by this Emit. Set via WithRunCapacity.
	runCapacity int
}

// WithRunCapacity overrides the default per-run capacity for the run
// initiated by this Emit. Pass to Engine.Emit at run start. Default
// is the originating node's Policy.RunCapacity (when > 0) falling back
// to the engine's DefaultQueueSize (64).
//
// The override is recorded under the envelope's RunID and consulted by
// the first EmitChunk for that run. Useful for tighter streaming
// budgets on cost-sensitive runs (e.g. a 16-frame cap for a chat run
// vs the default 64 for a batch).
//
// n must be > 0; non-positive values are ignored (the engine falls
// back to the next resolution step).
func WithRunCapacity(n int) EmitOption {
	return func(opts *emitOptions) {
		opts.runCapacity = n
	}
}

// FetchOption is the Fetch-time option type. Phase 13 will add
// per-run filtering via FetchByRun (a dedicated method, not an
// option), but the type exists today for fetch-side knobs that may
// land later (e.g. FetchTimeout).
type FetchOption func(*fetchOptions)

type fetchOptions struct {
	// reserved for later phases
}
