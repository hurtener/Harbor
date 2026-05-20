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

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// muxConfig holds the optional knobs NewMux threads into the two
// transport handlers. Set once at construction; never mutated after.
//
// withoutAuth is the explicit, test-only escape hatch — see
// WithoutValidator. A nil validator + withoutAuth=false fails closed
// at NewMux per CLAUDE.md §13 ("Test stubs as production defaults on
// operator-facing seams"; PR #91 amendment).
type muxConfig struct {
	logger          *slog.Logger
	keepalive       time.Duration
	validator       auth.Validator
	withoutAuth     bool
	aggregatorClock events.AggregatorClock
	// redactor is the audit.Redactor wired into the control transport
	// for the Phase 72b admin-impersonation audit emit. Optional in the
	// mux config so existing call-sites compile unchanged, but
	// production wiring (the `harbor dev` boot path) SHOULD supply it
	// so impersonation works end-to-end. When unsupplied, the control
	// transport refuses impersonation requests fail-closed with
	// CodeRuntimeError (CLAUDE.md §13 "Silent degradation").
	redactor audit.Redactor
	// postureSurface is the Phase 72f / 72g (D-111 / D-112) posture
	// dispatcher wired into the control transport so the seven posture
	// methods — the five `runtime.*` / `metrics.*` reads plus the two
	// `governance.posture` / `llm.posture` reads — route through it.
	// Optional — when unsupplied, the control transport rejects posture
	// calls with CodeUnknownMethod (the 404 → SKIP path the smoke
	// script relies on).
	postureSurface control.PostureSurface
	// artifactsSurface is the Phase 73l (D-120) artifacts dispatcher
	// wired into the control transport so the three `artifacts.*`
	// methods route through it. Optional — when unsupplied, the control
	// transport rejects artifacts calls with CodeUnknownMethod (the 404
	// → SKIP path the smoke script relies on).
	artifactsSurface control.ArtifactsSurface
	// pauseCoordinator + artifactStore + heavyThreshold feed the Phase
	// 72e `pause.list` snapshot handler. All three are OPTIONAL in the
	// mux config so existing call-sites compile unchanged — when the
	// coordinator or store is unsupplied the `pause.list` route is NOT
	// mounted, so the smoke script's `skip_if_404` keeps preflight
	// green on a partial build. Production wiring (`harbor dev`) SHOULD
	// supply all three so the Console intervention queue works.
	pauseCoordinator pauseresume.Coordinator
	artifactStore    artifacts.ArtifactStore
	heavyThreshold   int
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

// WithValidator wires the Phase 61 JWT auth.Validator into NewMux.
// BOTH transport handlers (REST control + SSE stream) are wrapped in
// auth.Middleware: every request must carry a verified
// `Authorization: Bearer <jwt>`; the middleware injects the verified
// identity + scopes into the request context.Context before the
// underlying handler runs.
//
// A validator is **mandatory** — `NewMux` returns `ErrMisconfigured`
// when neither `WithValidator` nor `WithoutValidator` is supplied
// (PR #91 amendment to D-078 / CLAUDE.md §13 "Test stubs as
// production defaults on operator-facing seams"). A nil validator is
// treated as "WithValidator not supplied"; tests that legitimately
// need the unauthenticated path use `WithoutValidator()` explicitly.
func WithValidator(v auth.Validator) Option {
	return func(c *muxConfig) {
		if v != nil {
			c.validator = v
		}
	}
}

// WithAggregateClock injects a deterministic clock into the
// events.aggregate handler's underlying *events.Aggregator. Production
// callers do not use this; the default real clock (UTC) is correct.
// Tests that exercise the aggregate path with backdated events use
// this to anchor the aggregator's "now" deterministically — the same
// posture WithKeepalive takes for the SSE keepalive interval.
func WithAggregateClock(c events.AggregatorClock) Option {
	return func(cfg *muxConfig) {
		if c != nil {
			cfg.aggregatorClock = c
		}
	}
}

// WithRedactor wires the audit.Redactor into the control transport so
// the Phase 72b admin-impersonation gate can publish a redacted
// `audit.admin_scope_used` event onto the bus on every accepted
// impersonation. The bus is already mandatory at NewMux (it feeds the
// SSE event transport); the redactor is the second half of the pair the
// control transport needs to enable impersonation.
//
// The option is OPTIONAL at the type level so existing call-sites
// compile unchanged. When the redactor is not supplied, the control
// transport refuses impersonation requests fail-closed with
// CodeRuntimeError (CLAUDE.md §13 "Silent degradation"). Production
// wiring (the `harbor dev` boot path) SHOULD supply it.
//
// A nil redactor is treated as "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(c *muxConfig) {
		if r != nil {
			c.redactor = r
		}
	}
}

// WithPostureSurface wires the Phase 72f / 72g (D-111 / D-112) posture
// dispatcher into the control transport. When supplied, the control
// handler routes the seven posture methods — the five `runtime.*` /
// `metrics.*` reads plus `governance.posture` / `llm.posture` — to the
// posture surface instead of falling through to the task-control
// ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects posture calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the
// Console Settings page (Phase 73m) has a live surface. A nil surface
// is treated as "WithPostureSurface not supplied".
func WithPostureSurface(s control.PostureSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.postureSurface = s
		}
	}
}

// WithPauseList wires the Phase 72e `pause.list` snapshot handler into
// NewMux. coord is the unified pause/resume Coordinator (Phase 50) the
// snapshot projects from; store is the ArtifactStore the D-026
// heavy-content bypass routes oversized pause payloads through;
// heavyThreshold is the configured heavy-content byte size
// (cfg.Artifacts.HeavyOutputThresholdBytes).
//
// All three are required together — supplying the option with a nil
// coord, a nil store, or a non-positive threshold leaves the
// `pause.list` route UN-mounted (the route's smoke `skip_if_404` keeps
// preflight green on a partial build). When supplied correctly the
// route `POST /v1/pause/list` is mounted and, when WithValidator is
// also set, wrapped in auth.Middleware like every other transport.
//
// pause.list is READ-ONLY against the Coordinator (CLAUDE.md §7 rule 4
// / §13) — it reads the shipped pause-coordinator state, it does not
// reinvent pause coordination.
func WithPauseList(coord pauseresume.Coordinator, store artifacts.ArtifactStore, heavyThreshold int) Option {
	return func(c *muxConfig) {
		c.pauseCoordinator = coord
		c.artifactStore = store
		c.heavyThreshold = heavyThreshold
	}
}

// WithArtifactsSurface wires the Phase 73l (D-120) artifacts dispatcher
// into the control transport. When supplied, the control handler routes
// the three artifacts methods — `artifacts.list`, `artifacts.put`,
// `artifacts.get_ref` — to the artifacts surface instead of falling
// through to the task-control ControlSurface.
//
// The option is OPTIONAL so existing call-sites compile unchanged. When
// not supplied, the control transport rejects artifacts calls with
// CodeUnknownMethod (HTTP 404) — the 404 → SKIP path the smoke script
// relies on. Production wiring (`harbor dev`) supplies it so the Console
// Artifacts page (Phase 73l) has a live surface. A nil surface is
// treated as "WithArtifactsSurface not supplied".
func WithArtifactsSurface(s control.ArtifactsSurface) Option {
	return func(c *muxConfig) {
		if s != nil {
			c.artifactsSurface = s
		}
	}
}

// WithoutValidator is the explicit, test-only escape hatch for cases
// that legitimately need the Phase 60 trust-based posture (the REST
// handler inherits `ControlSurface.Dispatch`'s identity-from-body
// gate, the SSE handler resolves identity from the `X-Harbor-*`
// carrier headers via `resolveIdentity`). It is used by Phase 60's
// own package tests + `test/integration/phase60_wire_transport_test.go`
// to assert the pre-auth transport surface still works.
//
// PRODUCTION CODE MUST NEVER USE THIS OPTION. A Runtime that boots
// with `WithoutValidator` exposes an unauthenticated Protocol surface,
// which violates CLAUDE.md §13's "Test stubs as production defaults"
// rule. The option is named for grepability: an audit can find every
// production-side WithoutValidator call site at compile time.
func WithoutValidator() Option {
	return func(c *muxConfig) { c.withoutAuth = true }
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
// All three configuration choices are mandatory:
//   - a non-nil ControlSurface,
//   - a non-nil EventBus,
//   - and EITHER `WithValidator(v)` (the production posture — JWT
//     bearer auth at the edge) OR `WithoutValidator()` (the explicit,
//     test-only escape hatch for the Phase 60 trust-based posture).
//
// A missing dependency — including the auth choice — fails loud with
// ErrMisconfigured rather than mounting a half-built mux or an
// unauthenticated production surface (CLAUDE.md §13 "Test stubs as
// production defaults on operator-facing seams"; PR #91).
//
// The returned mux is immutable after construction (D-025) and safe
// to share across N concurrent requests — a future server
// (`harbor dev`, Phase 64) mounts it and owns the listen / shutdown
// lifecycle.
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

	// Auth posture is mandatory: WithValidator OR WithoutValidator.
	// Neither = fail loud (CLAUDE.md §13 / PR #91).
	if cfg.validator == nil && !cfg.withoutAuth {
		return nil, fmt.Errorf("%w: WithValidator is required (or WithoutValidator() for the explicit test-only escape hatch — see CLAUDE.md §13)", ErrMisconfigured)
	}

	controlOpts := []control.Option{
		control.WithLogger(cfg.logger),
		// Phase 72b: thread the bus and (optional) redactor into the
		// control transport so the admin-impersonation gate can emit
		// `audit.admin_scope_used` events. The bus is mandatory at
		// NewMux already; the redactor is optional at the type level
		// but mandatory in practice for impersonation (the control
		// handler refuses impersonation paths fail-closed when either
		// is missing).
		control.WithEventBus(bus),
	}
	if cfg.redactor != nil {
		controlOpts = append(controlOpts, control.WithRedactor(cfg.redactor))
	}
	if cfg.postureSurface != nil {
		controlOpts = append(controlOpts, control.WithPostureSurface(cfg.postureSurface))
	}
	if cfg.artifactsSurface != nil {
		controlOpts = append(controlOpts, control.WithArtifactsSurface(cfg.artifactsSurface))
	}
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

	// Wave 13 (Phase 72a): the events.aggregate handler shares the
	// bus and lives in the same package. It is built once per Runtime
	// process; if the bus does not implement events.Replayer, the
	// handler still mounts — the per-request error path returns
	// CodeRuntimeError + HTTP 500 with a clear "no historical
	// aggregation" message.
	aggregatorOpts := []events.AggregatorOption{}
	if cfg.aggregatorClock != nil {
		aggregatorOpts = append(aggregatorOpts, events.WithAggregatorClock(cfg.aggregatorClock))
	}
	aggregator, err := events.NewAggregator(bus, aggregatorOpts...)
	if err != nil {
		return nil, fmt.Errorf("transports: build events aggregator: %w", err)
	}
	aggregateHandler, err := stream.NewAggregateHandler(aggregator, stream.WithAggregateLogger(cfg.logger))
	if err != nil {
		return nil, fmt.Errorf("transports: build events.aggregate handler: %w", err)
	}

	// Wave 13 (Phase 72e): the pause.list snapshot handler. Built only
	// when WithPauseList supplied all three dependencies (coordinator,
	// store, positive threshold). When any is missing the route is left
	// un-mounted — the smoke `skip_if_404` keeps preflight green on a
	// partial build.
	var pauseListHandler *stream.PauseListHandler
	if cfg.pauseCoordinator != nil && cfg.artifactStore != nil && cfg.heavyThreshold > 0 {
		plh, err := stream.NewPauseListHandler(
			cfg.pauseCoordinator, cfg.artifactStore, cfg.heavyThreshold,
			stream.WithPauseListLogger(cfg.logger),
			stream.WithPauseListBus(bus),
		)
		if err != nil {
			return nil, fmt.Errorf("transports: build pause.list handler: %w", err)
		}
		pauseListHandler = plh
	}

	mux := http.NewServeMux()

	// Phase 61: when WithValidator was supplied, wrap both transport
	// handlers in auth.Middleware. The middleware enforces the JWT
	// bearer at the edge, injects the verified identity + scopes into
	// r.Context(), and then calls the wrapped handler — which reads
	// identity from ctx (preferred) or the Phase 60 trust-based
	// carriers (fallback when WithValidator is not set).
	var (
		mountedControl   http.Handler = controlHandler
		mountedStream    http.Handler = streamHandler
		mountedAggregate http.Handler = aggregateHandler
	)
	if cfg.validator != nil {
		mw := auth.Middleware(cfg.validator, auth.MWLogger(cfg.logger))
		mountedControl = mw(controlHandler)
		mountedStream = mw(streamHandler)
		mountedAggregate = mw(aggregateHandler)
	}

	mux.Handle(control.RoutePattern, mountedControl)
	mux.Handle(stream.RoutePattern, mountedStream)
	mux.Handle(stream.AggregateRoutePattern, mountedAggregate)

	if pauseListHandler != nil {
		var mountedPauseList http.Handler = pauseListHandler
		if cfg.validator != nil {
			mw := auth.Middleware(cfg.validator, auth.MWLogger(cfg.logger))
			mountedPauseList = mw(pauseListHandler)
		}
		mux.Handle(stream.PauseListRoutePattern, mountedPauseList)
	}
	return mux, nil
}
