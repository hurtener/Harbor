package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// PostureSurface is the transport-agnostic Harbor Protocol runtime-
// posture handler (Phase 72f / D-111). It owns the five read-only
// posture methods — runtime.info, runtime.health, runtime.counters,
// runtime.drivers, metrics.snapshot — and is a sibling of the Phase 54
// ControlSurface, not an extension: the posture methods are READ
// methods (no runtime mutation), and threading the build / health /
// counters / drivers / metrics seams through NewControlSurface would
// balloon its dependency set (see the phase plan's "Risks" section).
//
// PostureSurface is built once per Runtime process via NewPostureSurface
// and shared across every Protocol request; Dispatch is safe for
// concurrent use by N goroutines (D-025). Every field is set once at
// construction and never mutated — Dispatch reads its request-specific
// data from ctx + the request argument, never from the surface struct.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// Every handler fails closed on an incomplete identity triple with
// CodeIdentityRequired. A cross-tenant query — the request's
// Identity.Tenant differing from the caller's ctx-verified tenant —
// requires the admin scope per D-079; without it the response is
// CodeScopeMismatch. When no auth middleware ran (Phase 60 trust-based
// posture, no identity on ctx), the request's body identity is
// authoritative and the cross-tenant gate is a no-op — the same
// posture every other Protocol surface holds.
//
// # The Console never reads internal Runtime objects (CLAUDE.md §8/§13)
//
// The posture data flows as canonical Protocol wire types
// (internal/protocol/types) — never as a re-export of an internal
// Runtime Go struct. The PostureDeps seams are read-only adapters the
// Runtime wires at boot; PostureSurface translates their output into
// the wire shape and returns it.
type PostureSurface struct {
	build       types.RuntimeInfo
	clock       func() time.Time
	health      func(ctx context.Context) []types.SubsystemHealth
	counters    func(ctx context.Context, ident identity.Identity) types.RuntimeCounters
	drivers     func() []types.SubsystemDriver
	metrics     func(ctx context.Context) types.MetricsSnapshot
	bootedAt    time.Time
	displayName string
	instanceID  string
}

// PostureDeps bundles the runtime-side seams a PostureSurface reads
// through. Every dependency is read-only — the surface mutates none of
// them. The Runtime wires these at boot (Stage-2 page consumers — the
// Overview counter cards, the Settings Runtime Info card — read the
// resulting Protocol methods, never the seams directly).
type PostureDeps struct {
	// Build carries the static build identity (BuildVersion /
	// BuildCommit / BuildDate / BuildGoVersion). The Capabilities,
	// ProtocolVersion, InstanceID, DisplayName, and UptimeSeconds
	// fields of the eventual RuntimeInfo are filled by the surface —
	// callers leave them zero here.
	Build types.RuntimeInfo
	// Clock returns the current wall-clock time. Used to compute
	// uptime and the SnapshotAt timestamps. Mandatory.
	Clock func() time.Time
	// BootedAt is the instant the Runtime process started — uptime is
	// Clock() minus this. When zero, the surface treats construction
	// time as boot time.
	BootedAt time.Time
	// Health returns the per-subsystem readiness rollup. Mandatory.
	Health func(ctx context.Context) []types.SubsystemHealth
	// Counters returns the low-cardinality live counters for the
	// caller's identity scope. Mandatory.
	Counters func(ctx context.Context, ident identity.Identity) types.RuntimeCounters
	// Drivers returns the configured driver names per persistence-
	// shaped subsystem. Mandatory.
	Drivers func() []types.SubsystemDriver
	// Metrics returns the Protocol-shaped projection over the Phase 56
	// MetricsRegistry. Mandatory.
	Metrics func(ctx context.Context) types.MetricsSnapshot
	// DisplayName is the operator-configured friendly name for this
	// Runtime. Optional — empty when the operator configured none.
	DisplayName string
	// InstanceID is the stable per-deployment identifier minted at
	// boot. Mandatory — a Console attached to multiple Runtimes keys
	// each attachment by it.
	InstanceID string
}

// ErrPostureMisconfigured — NewPostureSurface was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5) rather than building
// a surface that would nil-panic on the first Dispatch.
var ErrPostureMisconfigured = stderrors.New("protocol: PostureSurface missing a mandatory dependency")

// NewPostureSurface builds the Protocol runtime-posture surface. Every
// PostureDeps seam except Build / DisplayName / BootedAt is mandatory; a
// missing one fails loud with a wrapped ErrPostureMisconfigured.
//
// The returned PostureSurface is immutable after construction (D-025)
// and safe for concurrent use by N goroutines.
func NewPostureSurface(deps PostureDeps) (*PostureSurface, error) {
	if deps.Clock == nil {
		return nil, fmt.Errorf("%w: Clock is nil", ErrPostureMisconfigured)
	}
	if deps.Health == nil {
		return nil, fmt.Errorf("%w: Health is nil", ErrPostureMisconfigured)
	}
	if deps.Counters == nil {
		return nil, fmt.Errorf("%w: Counters is nil", ErrPostureMisconfigured)
	}
	if deps.Drivers == nil {
		return nil, fmt.Errorf("%w: Drivers is nil", ErrPostureMisconfigured)
	}
	if deps.Metrics == nil {
		return nil, fmt.Errorf("%w: Metrics is nil", ErrPostureMisconfigured)
	}
	if deps.InstanceID == "" {
		return nil, fmt.Errorf("%w: InstanceID is empty", ErrPostureMisconfigured)
	}
	bootedAt := deps.BootedAt
	if bootedAt.IsZero() {
		bootedAt = deps.Clock()
	}
	return &PostureSurface{
		build:       deps.Build,
		clock:       deps.Clock,
		health:      deps.Health,
		counters:    deps.Counters,
		drivers:     deps.Drivers,
		metrics:     deps.Metrics,
		bootedAt:    bootedAt,
		displayName: deps.DisplayName,
		instanceID:  deps.InstanceID,
	}, nil
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// posture-method call. A Phase 60 REST handler decodes a request, calls
// Dispatch, and encodes the response — Dispatch IS the surface.
//
// method selects the handler; it MUST be one of the five posture
// methods (methods.IsPostureMethod). req MUST be a *types.RuntimeInfoRequest
// — the five posture methods share the one read-only request shape.
//
// The return is always a *types.<Method>Response or a *protoerrors.Error
// so the wire layer never sees an unstructured runtime error:
//
//   - CodeUnknownMethod   — method is not a posture method.
//   - CodeInvalidRequest  — req is nil or not a *types.RuntimeInfoRequest.
//   - CodeIdentityRequired — the request's identity triple is incomplete.
//   - CodeScopeMismatch   — a cross-tenant query without the admin scope.
//
// Dispatch holds no per-call state on the PostureSurface — it reads
// everything from ctx + req (D-025). One PostureSurface serves N
// concurrent Dispatch goroutines safely.
func (s *PostureSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsPostureMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol posture method", string(method))
	}

	pr, ok := req.(*types.RuntimeInfoRequest)
	if !ok || pr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.RuntimeInfoRequest", string(method))
	}

	// Identity at the edge (RFC §5.5). The triple is mandatory for
	// every posture method — fails closed on an incomplete triple.
	id := identity.Identity{
		TenantID:  pr.Identity.Tenant,
		UserID:    pr.Identity.User,
		SessionID: pr.Identity.Session,
	}
	if err := identity.Validate(id); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}

	// Cross-tenant gate (D-079). When auth middleware ran, ctx carries
	// the verified identity; a request whose body Tenant differs from
	// the verified tenant requires the admin (or console:fleet) scope.
	// When no middleware ran (Phase 60 trust-based posture), there is
	// no ctx-identity and the gate is a no-op — the body identity is
	// authoritative, same posture every other Protocol surface holds.
	if verified, hasVerified := identity.From(ctx); hasVerified {
		if id.TenantID != verified.TenantID {
			if !auth.HasScope(ctx, auth.ScopeAdmin) && !auth.HasScope(ctx, auth.ScopeConsoleFleet) {
				return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
					"method %q: cross-tenant posture read requires the admin scope claim", string(method))
			}
		}
	}

	switch method {
	case methods.MethodRuntimeInfo:
		return s.handleInfo(), nil
	case methods.MethodRuntimeHealth:
		return s.handleHealth(ctx), nil
	case methods.MethodRuntimeCounters:
		return s.handleCounters(ctx, id), nil
	case methods.MethodRuntimeDrivers:
		return s.handleDrivers(), nil
	case methods.MethodMetricsSnapshot:
		return s.handleMetrics(ctx), nil
	default:
		// Unreachable: IsPostureMethod already gated the method set.
		// Fail loud rather than silently no-op (CLAUDE.md §5).
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no posture handler (Protocol-surface invariant violated)", string(method))
	}
}

// handleInfo builds the runtime.info response from the static build
// identity plus the surface's instance / display / uptime / capability
// projection. The Capabilities + ProtocolVersion are read from the
// canonical types package — never hardcoded.
func (s *PostureSurface) handleInfo() *types.RuntimeInfo {
	out := s.build
	out.InstanceID = s.instanceID
	out.DisplayName = s.displayName
	out.ProtocolVersion = types.ProtocolVersion
	out.Capabilities = types.Capabilities()
	uptime := s.clock().Sub(s.bootedAt)
	if uptime < 0 {
		uptime = 0
	}
	out.UptimeSeconds = int64(uptime / time.Second)
	return &out
}

// handleHealth builds the runtime.health response from the Health seam.
// A nil seam return is normalised to an empty slice so the wire shape
// is stable.
func (s *PostureSurface) handleHealth(ctx context.Context) *types.RuntimeHealth {
	subs := s.health(ctx)
	if subs == nil {
		subs = []types.SubsystemHealth{}
	}
	return &types.RuntimeHealth{Subsystems: subs}
}

// handleCounters builds the runtime.counters response from the Counters
// seam. The SnapshotAt timestamp is filled by the surface (from the
// clock) when the seam left it zero, so a seam that does not stamp the
// time still produces a complete wire shape.
func (s *PostureSurface) handleCounters(ctx context.Context, id identity.Identity) *types.RuntimeCounters {
	c := s.counters(ctx, id)
	if c.SnapshotAt == 0 {
		c.SnapshotAt = s.clock().UnixMilli()
	}
	return &c
}

// handleDrivers builds the runtime.drivers response from the Drivers
// seam. A nil seam return is normalised to an empty slice.
func (s *PostureSurface) handleDrivers() *types.RuntimeDrivers {
	subs := s.drivers()
	if subs == nil {
		subs = []types.SubsystemDriver{}
	}
	return &types.RuntimeDrivers{Subsystems: subs}
}

// handleMetrics builds the metrics.snapshot response from the Metrics
// seam. The SnapshotAt timestamp is filled by the surface when the seam
// left it zero; the per-kind slices are normalised to empty (never nil)
// so the wire shape is stable.
func (s *PostureSurface) handleMetrics(ctx context.Context) *types.MetricsSnapshot {
	m := s.metrics(ctx)
	if m.SnapshotAt == 0 {
		m.SnapshotAt = s.clock().UnixMilli()
	}
	if m.Counters == nil {
		m.Counters = []types.NamedCounter{}
	}
	if m.Histograms == nil {
		m.Histograms = []types.NamedHistogram{}
	}
	if m.Gauges == nil {
		m.Gauges = []types.NamedGauge{}
	}
	return &m
}
