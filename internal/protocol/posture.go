package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// PostureSurface is the transport-agnostic Harbor Protocol posture
// handler. It owns the seven read-only posture methods:
//
//   - runtime.info, runtime.health, runtime.counters, runtime.drivers,
//     metrics.snapshot — the Phase 72f (D-111) runtime-posture cluster.
//   - governance.posture, llm.posture — the Phase 72g (D-112) config-
//     posture pair (the D-081 governance `IdentityTiers` view and the
//     bound LLM provider/model/region + D-089 `MockMode` flag).
//
// PostureSurface is a sibling of the Phase 54 ControlSurface, not an
// extension: the posture methods are READ methods (no runtime
// mutation), and threading the build / health / counters / drivers /
// metrics / governance / llm seams through NewControlSurface would
// balloon its dependency set.
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
// # Audit on the cross-tenant config reads (Phase 72g / D-112)
//
// An accepted cross-tenant `governance.posture` / `llm.posture` read
// emits a `*.posture_read_admin` audit event through the wired Redactor
// + Bus — the same posture the Phase 72b admin-scope path takes. An
// own-tenant read does NOT emit (matches the sessions.inspect
// convention). The five `runtime.*` / `metrics.*` reads never emit
// audit. A failed audit emit fails loudly (CodeRuntimeError) — the read
// already succeeded, so the operator MUST see the audit drift.
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
	governance  *governance.PostureProvider
	llm         *llm.PostureProvider
	redactor    audit.Redactor
	bus         events.EventBus
	bootedAt    time.Time
	displayName string
	instanceID  string
	// wiredCaps is the per-instance subset of canonical Protocol
	// capabilities this Runtime actually wires (Phase 84a F1 / round-8).
	// `handleInfo` projects it as `RuntimeInfo.Capabilities`. The
	// always-on capabilities (task_control, events_subscribe,
	// runtime_posture) are added at construction; conditional ones
	// (currently `topology_snapshot`) come in via the matching deps flag.
	// Sorted lexicographically so the wire shape is deterministic.
	wiredCaps []types.Capability
}

// PostureDeps bundles the runtime-side seams a PostureSurface reads
// through. Every dependency is read-only — the surface mutates none of
// them. The Runtime wires these at boot (Stage-2 page consumers — the
// Overview counter cards, the Settings Runtime Info / Governance /
// LLM-Provider cards — read the resulting Protocol methods, never the
// seams directly).
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
	// Governance is the Phase 72g (D-112) read-only governance posture
	// accessor — the source the `governance.posture` method projects
	// onto types.GovernancePostureResponse. Mandatory.
	Governance *governance.PostureProvider
	// LLM is the Phase 72g (D-112) read-only LLM posture accessor — the
	// source the `llm.posture` method projects onto
	// types.LLMPostureResponse. Mandatory.
	LLM *llm.PostureProvider
	// Redactor is the audit Redactor every cross-tenant
	// `*.posture_read_admin` payload runs through before the bus
	// publish (CLAUDE.md §7 rule 6). Mandatory.
	Redactor audit.Redactor
	// Bus is the canonical event bus the cross-tenant
	// `*.posture_read_admin` audit events are published onto.
	// Mandatory.
	Bus events.EventBus
	// DisplayName is the operator-configured friendly name for this
	// Runtime. Optional — empty when the operator configured none.
	DisplayName string
	// InstanceID is the stable per-deployment identifier minted at
	// boot. Mandatory — a Console attached to multiple Runtimes keys
	// each attachment by it.
	InstanceID string
	// TopologyAvailable indicates this Runtime hosts an engine-graph
	// projection — when true, `runtime.info.capabilities` advertises
	// `topology_snapshot` so Protocol clients gate their topology
	// fetches at attach time (round-8 F1 / phase 84a). The
	// `topology.snapshot` method itself stays gated by the matching
	// `ControlSurface.topology` accessor; this flag is the *advertised*
	// projection of that wiring decision. Optional — defaults false
	// (planner/RunLoop runtimes like `harbor dev` against an agent
	// yaml).
	TopologyAvailable bool
}

// ErrPostureMisconfigured — NewPostureSurface was called with a missing
// mandatory dependency. Fails closed (CLAUDE.md §5) rather than building
// a surface that would nil-panic on the first Dispatch.
var ErrPostureMisconfigured = stderrors.New("protocol: PostureSurface missing a mandatory dependency")

// NewPostureSurface builds the Protocol posture surface. Every
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
	if deps.Governance == nil {
		return nil, fmt.Errorf("%w: Governance is nil", ErrPostureMisconfigured)
	}
	if deps.LLM == nil {
		return nil, fmt.Errorf("%w: LLM is nil", ErrPostureMisconfigured)
	}
	if deps.Redactor == nil {
		return nil, fmt.Errorf("%w: Redactor is nil", ErrPostureMisconfigured)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Bus is nil", ErrPostureMisconfigured)
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
		governance:  deps.Governance,
		llm:         deps.LLM,
		redactor:    deps.Redactor,
		bus:         deps.Bus,
		bootedAt:    bootedAt,
		displayName: deps.DisplayName,
		instanceID:  deps.InstanceID,
		wiredCaps:   wiredCapabilitiesFor(deps.TopologyAvailable),
	}, nil
}

// wiredCapabilitiesFor returns the lexicographically-sorted subset of
// canonical Protocol capabilities this Runtime instance has actually
// wired. Always-on surfaces (task control, events subscribe, runtime
// posture) are unconditional in the dev binary; conditional ones come
// in via the matching deps flag (round-8 F1 / phase 84a). Adding a new
// conditional capability extends this function in tandem with the
// matching `PostureDeps` field — pure projection, no global state.
func wiredCapabilitiesFor(topologyAvailable bool) []types.Capability {
	caps := []types.Capability{
		types.CapTaskControl,
		types.CapEventsSubscribe,
		types.CapRuntimePosture,
	}
	if topologyAvailable {
		caps = append(caps, types.CapTopologySnapshot)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// posture-method call. A Phase 60 REST handler decodes a request, calls
// Dispatch, and encodes the response — Dispatch IS the surface.
//
// method selects the handler; it MUST be one of the seven posture
// methods (methods.IsPostureMethod). req MUST be a
// *types.RuntimeInfoRequest — all seven posture methods share the one
// read-only request envelope (the governance / llm reads are also
// identity-only, so they reuse the same shape).
//
// The return is always a *types.<Method>Response or a *protoerrors.Error
// so the wire layer never sees an unstructured runtime error:
//
//   - CodeUnknownMethod   — method is not a posture method.
//   - CodeInvalidRequest  — req is nil or not a *types.RuntimeInfoRequest.
//   - CodeIdentityRequired — the request's identity triple is incomplete.
//   - CodeScopeMismatch   — a cross-tenant query without the admin scope.
//   - CodeRuntimeError    — a posture-accessor or audit-emit failure.
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
	// The actor — the audit anchor for the Phase 72g cross-tenant
	// config reads — is the ctx-verified identity when present, else
	// the body identity.
	crossTenant := false
	actor := id
	if verified, hasVerified := identity.From(ctx); hasVerified {
		actor = verified
		if id.TenantID != verified.TenantID {
			if !auth.HasScope(ctx, auth.ScopeAdmin) && !auth.HasScope(ctx, auth.ScopeConsoleFleet) {
				return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
					"method %q: cross-tenant posture read requires the admin scope claim", string(method))
			}
			crossTenant = true
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
	case methods.MethodGovernancePosture:
		return s.handleGovernancePosture(ctx, method, id, actor, crossTenant)
	case methods.MethodLLMPosture:
		return s.handleLLMPosture(ctx, method, id, actor, crossTenant)
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
	// Per-instance wired subset (round-8 F1 / phase 84a). Conditional
	// surfaces (currently `topology_snapshot`) appear here only when
	// the matching seam was wired at construction; the static
	// `types.Capabilities()` is the handshake/registry surface, not
	// the per-instance advertisement.
	out.Capabilities = append([]types.Capability(nil), s.wiredCaps...)
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

// handleGovernancePosture builds the governance.posture response (Phase
// 72g / D-112) by reading the governance PostureProvider and projecting
// its Snapshot onto the wire type. The validated request identity is
// threaded into ctx so the provider's identity-mandatory gate (it reads
// the triple from ctx) is satisfied. An accepted cross-tenant read
// emits a `governance.posture_read_admin` audit event.
func (s *PostureSurface) handleGovernancePosture(
	ctx context.Context,
	method methods.Method,
	id identity.Identity,
	actor identity.Identity,
	crossTenant bool,
) (any, error) {
	govCtx, err := identity.With(ctx, id)
	if err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	snap, err := s.governance.Posture(govCtx)
	if err != nil {
		return nil, mapPostureError(string(method), err)
	}
	if crossTenant {
		if emitErr := s.emitPostureReadAdmin(ctx, method, actor, id.TenantID); emitErr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
				"method %q: cross-tenant read succeeded but audit emit failed: %v", string(method), emitErr)
		}
	}
	return projectGovernancePosture(snap), nil
}

// handleLLMPosture builds the llm.posture response (Phase 72g / D-112)
// by reading the llm PostureProvider and projecting its PostureSnapshot
// onto the wire type. An accepted cross-tenant read emits an
// `llm.posture_read_admin` audit event.
func (s *PostureSurface) handleLLMPosture(
	ctx context.Context,
	method methods.Method,
	id identity.Identity,
	actor identity.Identity,
	crossTenant bool,
) (any, error) {
	snap, err := s.llm.Posture(ctx)
	if err != nil {
		return nil, mapPostureError(string(method), err)
	}
	if crossTenant {
		if emitErr := s.emitPostureReadAdmin(ctx, method, actor, id.TenantID); emitErr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
				"method %q: cross-tenant read succeeded but audit emit failed: %v", string(method), emitErr)
		}
	}
	return projectLLMPosture(snap), nil
}

// emitPostureReadAdmin publishes the typed `*.posture_read_admin` audit
// event onto the wired bus. The audit payload runs through the wired
// audit.Redactor BEFORE the publish (CLAUDE.md §7 rule 6 + D-020) — the
// posture surface reports provider/model/region/tier metadata, never an
// API key, but the redactor pass is mandatory regardless.
//
// The event's Identity is the ACTOR's quadruple — the admin caller is
// the audit anchor. The RequestedTenant is on the payload for
// correlation. The event type is governance- or llm-namespaced to match
// the dispatched method.
func (s *PostureSurface) emitPostureReadAdmin(
	ctx context.Context,
	method methods.Method,
	actor identity.Identity,
	requestedTenant string,
) error {
	actorQuad := identity.Quadruple{Identity: actor}

	// Run the audit-visible fields through the redactor before building
	// the typed payload (mirrors the Phase 72b admin_scope_used
	// pattern).
	auditView := map[string]any{
		"actor_tenant":     actor.TenantID,
		"actor_user":       actor.UserID,
		"actor_session":    actor.SessionID,
		"requested_tenant": requestedTenant,
		"method":           string(method),
	}
	if _, err := s.redactor.Redact(ctx, auditView); err != nil {
		// Fail loud — never emit unredacted (CLAUDE.md §13).
		return fmt.Errorf("redactor refused posture_read_admin payload: %w", err)
	}

	var ev events.Event
	switch method {
	case methods.MethodGovernancePosture:
		ev = events.Event{
			Type:       governance.EventTypePostureReadAdmin,
			Identity:   actorQuad,
			OccurredAt: s.clock(),
			Payload: governance.PostureReadAdminPayload{
				Actor:           actorQuad,
				RequestedTenant: requestedTenant,
				OccurredAt:      s.clock(),
			},
		}
	case methods.MethodLLMPosture:
		ev = events.Event{
			Type:       llm.EventTypePostureReadAdmin,
			Identity:   actorQuad,
			OccurredAt: s.clock(),
			Payload: llm.PostureReadAdminPayload{
				Actor:           actorQuad,
				RequestedTenant: requestedTenant,
			},
		}
	default:
		return fmt.Errorf("emitPostureReadAdmin: unsupported method %q", string(method))
	}

	if err := s.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("publish %s: %w", ev.Type, err)
	}
	return nil
}

// projectGovernancePosture maps a governance.Snapshot onto the
// GovernancePostureResponse wire type. The internal TierConfig shape is
// projected — never re-exported — so a future change to the internal
// struct does not silently reshape the Protocol surface (single-source
// per CLAUDE.md §8). The IdentityTiers map is always non-nil in the
// wire JSON.
func projectGovernancePosture(snap governance.Snapshot) *types.GovernancePostureResponse {
	tiers := make(map[string]types.IdentityTierView, len(snap.IdentityTiers))
	for name, tc := range snap.IdentityTiers {
		tiers[name] = types.IdentityTierView{
			BudgetCeilingUSD: tc.BudgetCeilingUSD,
			RateLimit: types.RateLimitView{
				Capacity:         tc.RateLimit.Capacity,
				RefillTokens:     tc.RateLimit.RefillTokens,
				RefillIntervalMS: tc.RateLimit.RefillInterval.Milliseconds(),
			},
			MaxTokens: tc.MaxTokens,
		}
	}
	return &types.GovernancePostureResponse{
		DefaultTier:     snap.DefaultTier,
		ResolvedTier:    snap.ResolvedTier,
		IdentityTiers:   tiers,
		ProtocolVersion: types.ProtocolVersion,
	}
}

// projectLLMPosture maps an llm.PostureSnapshot onto the
// LLMPostureResponse wire type.
func projectLLMPosture(snap llm.PostureSnapshot) *types.LLMPostureResponse {
	return &types.LLMPostureResponse{
		Provider:        snap.Provider,
		Model:           snap.Model,
		Region:          snap.Region,
		MockMode:        snap.MockMode,
		ProtocolVersion: types.ProtocolVersion,
	}
}

// mapPostureError translates a posture-accessor error onto a canonical
// Protocol error code. The mapping closes the wire surface — every error
// shape is observable as a Code (CLAUDE.md §13).
func mapPostureError(method string, err error) error {
	switch {
	case err == nil:
		return nil
	case stderrors.Is(err, governance.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: %v", method, err)
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: posture read failed: %v", method, err)
	}
}
