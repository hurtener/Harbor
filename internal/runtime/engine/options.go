package engine

import "context"

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

// engineConfig captures the New-time options. Internally consumed by
// the engine's constructor; never exported.
type engineConfig struct {
	queueSize         int
	channelOverrides  map[channelKey]int
	errorEmitToEgress bool
	runErrorHandler   RunErrorHandler
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
