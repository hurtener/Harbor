package engine

// DefaultQueueSize is the bounded per-adjacency channel capacity when
// no override is configured. RFC §6.1 settles the value at 64
// (resolves brief 01 Q-4). Engine-wide override via WithQueueSize(n);
// per-channel override via WithChannelOverride(from, to, n).
const DefaultQueueSize = 64

// engineConfig captures the New-time options. Internally consumed by
// the engine's constructor; never exported.
type engineConfig struct {
	queueSize          int
	channelOverrides   map[channelKey]int
	errorEmitToEgress  bool
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
// (Phase 11's RunError, surfacing in Phase 10 as raw errors via the
// logger) also land on the egress channel as envelopes. Default is
// false: errors go to Phase 04's logger + Phase 05's bus only.
//
// Operators who want to consume errors via Fetch (instead of via the
// bus) opt in here. Phase 11 will solidify the RunError shape.
func WithErrorEmissionToEgress(enabled bool) Option {
	return func(cfg *engineConfig) {
		cfg.errorEmitToEgress = enabled
	}
}

// EmitOption is the Emit-time option type. Phase 12 will add
// WithRunCapacity here; Phase 10 ships the type as an empty seam so
// callers don't need to refactor when later phases land.
type EmitOption func(*emitOptions)

type emitOptions struct {
	// reserved for Phase 12+
}

// FetchOption is the Fetch-time option type. Phase 13 will add
// per-run filtering via FetchByRun (a dedicated method, not an
// option), but the type exists today for fetch-side knobs that may
// land later (e.g. FetchTimeout).
type FetchOption func(*fetchOptions)

type fetchOptions struct {
	// reserved for later phases
}
