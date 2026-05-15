// Package transports is the Harbor Protocol wire-transport seam — the
// Phase 60 binding of RFC §5.4's resolved transport choice (SSE for the
// event stream + REST/JSON for the control surface) onto net/http.
//
// # The seam (CLAUDE.md §3, §4.4)
//
// Each wire transport is its own sub-package:
//
//   - internal/protocol/transports/control — REST/JSON over the
//     transport-agnostic protocol.ControlSurface (Phase 54).
//   - internal/protocol/transports/stream — SSE over the events.EventBus
//     (Phase 05).
//
// This package composes them: NewMux wires both handlers into one
// *http.ServeMux a future server (the `harbor dev` subcommand — Phase
// 64) mounts. The layout is the §4.4-style seam read for transports
// rather than drivers: RFC §5.4 explicitly leaves WebSocket as an
// additive alternate transport. Adding it is a third sub-package
// (internal/protocol/transports/websocket) plus one more mux entry here
// — neither `control` nor `stream` is reshaped, and no caller outside
// this package changes. There is NO driver-registry / factory ceremony:
// the transport set is small, closed, and mounted in code at boot, not
// resolved by name from config — the same posture Phase 54 took for the
// ControlSurface (D-072).
//
// # Why no http.Server here
//
// Phase 60 ships the transport HANDLERS, not the server that listens.
// `harbor dev` (Phase 64) owns the net.Listener, the graceful-shutdown
// lifecycle, and the /healthz + /readyz endpoints; it calls NewMux and
// serves the result. Keeping the listen/shutdown lifecycle out of this
// package means the transports are exercised end-to-end today via
// httptest (the package + integration tests) without waiting on the
// server phase — the same decoupling Phase 54 used to stay testable
// ahead of its wire binding.
//
// # Concurrent reuse (D-025)
//
// The *http.ServeMux NewMux returns is immutable after construction and
// both mounted handlers are themselves D-025-safe compiled artifacts
// (control.Handler, stream.Handler). One mux serves N concurrent
// requests safely; concurrent_test.go pins it under -race.
package transports

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
)

// muxConfig holds the optional knobs NewMux threads into the two
// transport handlers. Set once at construction; never mutated after.
type muxConfig struct {
	logger    *slog.Logger
	keepalive time.Duration
}

// Option configures NewMux.
type Option func(*muxConfig)

// WithLogger sets the slog.Logger both transport handlers log to. A nil
// logger is ignored; the handlers fall back to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *muxConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithKeepalive overrides the SSE keepalive-comment interval on the
// stream transport. A non-positive value is ignored.
func WithKeepalive(d time.Duration) Option {
	return func(c *muxConfig) {
		if d > 0 {
			c.keepalive = d
		}
	}
}

// ErrMisconfigured — NewMux was called with a nil ControlSurface or a
// nil EventBus. Both are mandatory: the former feeds the REST control
// transport, the latter feeds the SSE event transport. Fails closed
// (CLAUDE.md §5) rather than mounting a half-built mux.
var ErrMisconfigured = errors.New("transports: NewMux missing a mandatory dependency")

// NewMux composes the Protocol wire transports into a single
// *http.ServeMux:
//
//   - POST /v1/control/{method} — the REST/JSON control surface.
//   - GET  /v1/events           — the SSE event stream.
//
// Both dependencies are mandatory; a nil either fails loud with
// ErrMisconfigured. The returned mux is immutable after construction
// (D-025) and safe to share across N concurrent requests — a future
// server (`harbor dev`, Phase 64) mounts it and owns the listen /
// shutdown lifecycle.
func NewMux(cs *protocol.ControlSurface, bus events.EventBus, opts ...Option) (*http.ServeMux, error) {
	if cs == nil {
		return nil, fmt.Errorf("%w: protocol.ControlSurface is nil", ErrMisconfigured)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrMisconfigured)
	}

	cfg := muxConfig{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}

	controlOpts := []control.Option{control.WithLogger(cfg.logger)}
	controlHandler, err := control.NewHandler(cs, controlOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build control handler: %w", err)
	}

	streamOpts := []stream.Option{stream.WithLogger(cfg.logger)}
	if cfg.keepalive > 0 {
		streamOpts = append(streamOpts, stream.WithKeepalive(cfg.keepalive))
	}
	streamHandler, err := stream.NewHandler(bus, streamOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build stream handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, controlHandler)
	mux.Handle(stream.RoutePattern, streamHandler)
	return mux, nil
}
