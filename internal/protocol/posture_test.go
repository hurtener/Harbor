package protocol_test

import (
	stderrors "errors"
	"reflect"
	"testing"
	"time"

	"context"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// fixedClock returns a deterministic time so handler assertions are
// byte-stable.
func fixedClock() time.Time {
	return time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
}

// newPostureBus builds a real in-memory event bus for the posture
// fixture — the Phase 72g cross-tenant audit emit path publishes onto
// it. §17.3: real drivers on the seam, no mocks.
func newPostureBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newPostureGovernance / newPostureLLM build the Phase 72g posture
// providers for the fixture.
func newPostureGovernance() *governance.PostureProvider {
	return governance.NewPostureProvider(governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {
				BudgetCeilingUSD: 5.0,
				RateLimit:        governance.RateLimitConfig{Capacity: 100, RefillTokens: 10, RefillInterval: time.Second},
				MaxTokens:        2048,
			},
		},
	})
}

func newPostureLLM() *llm.PostureProvider {
	return llm.NewPostureProvider(llm.ConfigSnapshot{
		Driver:   "bifrost",
		Provider: "openai",
		Model:    "openai/gpt-5.3-chat",
	})
}

// newPostureFixture builds a PostureSurface wired with deterministic
// seams. bootedAt is one hour before the fixed clock so uptime is a
// stable 3600s.
func newPostureFixture(t *testing.T) *protocol.PostureSurface {
	t.Helper()
	deps := protocol.PostureDeps{
		Build: types.RuntimeInfo{
			BuildVersion:   "v0.0.0-dev",
			BuildCommit:    "abc1234",
			BuildDate:      "2026-05-19T00:00:00Z",
			BuildGoVersion: "go1.26.0",
		},
		Clock:    fixedClock,
		BootedAt: fixedClock().Add(-1 * time.Hour),
		Health: func(_ context.Context) []types.SubsystemHealth {
			return []types.SubsystemHealth{
				{Subsystem: "events", Status: types.HealthStatusReady},
				{Subsystem: "state", Status: types.HealthStatusReady},
			}
		},
		Counters: func(_ context.Context, ident identity.Identity) types.RuntimeCounters {
			// Echo the caller's tenant length into TasksRunning so a
			// context-bleed across goroutines surfaces as a wrong count.
			return types.RuntimeCounters{
				TasksRunning:   int64(len(ident.TenantID)),
				SessionsActive: 1,
			}
		},
		Drivers: func() []types.SubsystemDriver {
			return []types.SubsystemDriver{
				{Subsystem: "state", Driver: "inmem"},
				{Subsystem: "artifacts", Driver: "inmem"},
			}
		},
		Metrics: func(_ context.Context) types.MetricsSnapshot {
			return types.MetricsSnapshot{
				Counters: []types.NamedCounter{{Name: "harbor_events_total", Value: 5}},
			}
		},
		Governance:  newPostureGovernance(),
		LLM:         newPostureLLM(),
		Redactor:    patterns.New(),
		Bus:         newPostureBus(t),
		DisplayName: "harbor-test",
		InstanceID:  "inst-test-001",
	}
	s, err := protocol.NewPostureSurface(deps)
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}
	return s
}

func validRequest() *types.RuntimeInfoRequest {
	return &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{
			Tenant:  "tenant-a",
			User:    "user-1",
			Session: "session-x",
		},
	}
}

func TestNewPostureSurface_NilDepFailsLoud(t *testing.T) {
	base := func() protocol.PostureDeps {
		return protocol.PostureDeps{
			Clock:      fixedClock,
			Health:     func(context.Context) []types.SubsystemHealth { return nil },
			Counters:   func(context.Context, identity.Identity) types.RuntimeCounters { return types.RuntimeCounters{} },
			Drivers:    func() []types.SubsystemDriver { return nil },
			Metrics:    func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
			Governance: newPostureGovernance(),
			LLM:        newPostureLLM(),
			Redactor:   patterns.New(),
			Bus:        newPostureBus(t),
			InstanceID: "i",
		}
	}
	cases := map[string]func(*protocol.PostureDeps){
		"nil Clock":        func(d *protocol.PostureDeps) { d.Clock = nil },
		"nil Health":       func(d *protocol.PostureDeps) { d.Health = nil },
		"nil Counters":     func(d *protocol.PostureDeps) { d.Counters = nil },
		"nil Drivers":      func(d *protocol.PostureDeps) { d.Drivers = nil },
		"nil Metrics":      func(d *protocol.PostureDeps) { d.Metrics = nil },
		"nil Governance":   func(d *protocol.PostureDeps) { d.Governance = nil },
		"nil LLM":          func(d *protocol.PostureDeps) { d.LLM = nil },
		"nil Redactor":     func(d *protocol.PostureDeps) { d.Redactor = nil },
		"nil Bus":          func(d *protocol.PostureDeps) { d.Bus = nil },
		"empty InstanceID": func(d *protocol.PostureDeps) { d.InstanceID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := base()
			mutate(&d)
			_, err := protocol.NewPostureSurface(d)
			if err == nil {
				t.Fatalf("NewPostureSurface(%s) = nil error, want ErrPostureMisconfigured", name)
			}
			if !stderrors.Is(err, protocol.ErrPostureMisconfigured) {
				t.Fatalf("NewPostureSurface(%s) error = %v, want wrapped ErrPostureMisconfigured", name, err)
			}
		})
	}
}

func TestPostureDispatch_UnknownMethod(t *testing.T) {
	s := newPostureFixture(t)
	_, err := s.Dispatch(context.Background(), methods.MethodStart, validRequest())
	assertPostureCode(t, err, protoerrors.CodeUnknownMethod)
}

func TestPostureDispatch_NilRequest(t *testing.T) {
	s := newPostureFixture(t)
	_, err := s.Dispatch(context.Background(), methods.MethodRuntimeInfo, nil)
	assertPostureCode(t, err, protoerrors.CodeInvalidRequest)
}

func TestPostureDispatch_WrongTypeRequest(t *testing.T) {
	s := newPostureFixture(t)
	_, err := s.Dispatch(context.Background(), methods.MethodRuntimeInfo, &types.StartRequest{})
	assertPostureCode(t, err, protoerrors.CodeInvalidRequest)
}

func TestPostureDispatch_IncompleteIdentity(t *testing.T) {
	s := newPostureFixture(t)
	for _, method := range []methods.Method{
		methods.MethodRuntimeInfo, methods.MethodRuntimeHealth,
		methods.MethodRuntimeCounters, methods.MethodRuntimeDrivers,
		methods.MethodMetricsSnapshot, methods.MethodGovernancePosture,
		methods.MethodLLMPosture,
	} {
		req := &types.RuntimeInfoRequest{
			Identity: types.IdentityScope{Tenant: "", User: "u", Session: "s"},
		}
		_, err := s.Dispatch(context.Background(), method, req)
		assertPostureCode(t, err, protoerrors.CodeIdentityRequired)
	}
}

func TestPostureDispatch_RuntimeInfo(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodRuntimeInfo, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(runtime.info): %v", err)
	}
	info, ok := out.(*types.RuntimeInfo)
	if !ok {
		t.Fatalf("runtime.info returned %T, want *types.RuntimeInfo", out)
	}
	if info.InstanceID != "inst-test-001" {
		t.Errorf("InstanceID = %q, want inst-test-001", info.InstanceID)
	}
	if info.DisplayName != "harbor-test" {
		t.Errorf("DisplayName = %q, want harbor-test", info.DisplayName)
	}
	if info.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", info.ProtocolVersion, types.ProtocolVersion)
	}
	if info.UptimeSeconds != 3600 {
		t.Errorf("UptimeSeconds = %d, want 3600", info.UptimeSeconds)
	}
	if info.BuildVersion != "v0.0.0-dev" || info.BuildCommit != "abc1234" {
		t.Errorf("build identity wrong: %+v", info)
	}
	var hasPostureCap bool
	for _, c := range info.Capabilities {
		if c == types.CapRuntimePosture {
			hasPostureCap = true
		}
	}
	if !hasPostureCap {
		t.Errorf("runtime.info Capabilities %v missing CapRuntimePosture", info.Capabilities)
	}
}

func TestPostureDispatch_RuntimeHealth(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodRuntimeHealth, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(runtime.health): %v", err)
	}
	h, ok := out.(*types.RuntimeHealth)
	if !ok {
		t.Fatalf("runtime.health returned %T, want *types.RuntimeHealth", out)
	}
	if len(h.Subsystems) != 2 {
		t.Fatalf("runtime.health returned %d subsystems, want 2", len(h.Subsystems))
	}
}

func TestPostureDispatch_RuntimeCounters(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodRuntimeCounters, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(runtime.counters): %v", err)
	}
	c, ok := out.(*types.RuntimeCounters)
	if !ok {
		t.Fatalf("runtime.counters returned %T, want *types.RuntimeCounters", out)
	}
	// The fixture echoes len("tenant-a")=8 into TasksRunning.
	if c.TasksRunning != int64(len("tenant-a")) {
		t.Errorf("TasksRunning = %d, want %d (identity not threaded into the Counters seam)", c.TasksRunning, len("tenant-a"))
	}
	if c.SnapshotAt == 0 {
		t.Error("runtime.counters SnapshotAt = 0, want the clock-stamped value")
	}
}

func TestPostureDispatch_RuntimeDrivers(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodRuntimeDrivers, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(runtime.drivers): %v", err)
	}
	d, ok := out.(*types.RuntimeDrivers)
	if !ok {
		t.Fatalf("runtime.drivers returned %T, want *types.RuntimeDrivers", out)
	}
	if len(d.Subsystems) != 2 {
		t.Fatalf("runtime.drivers returned %d subsystems, want 2", len(d.Subsystems))
	}
}

func TestPostureDispatch_MetricsSnapshot(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodMetricsSnapshot, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(metrics.snapshot): %v", err)
	}
	m, ok := out.(*types.MetricsSnapshot)
	if !ok {
		t.Fatalf("metrics.snapshot returned %T, want *types.MetricsSnapshot", out)
	}
	if len(m.Counters) != 1 {
		t.Fatalf("metrics.snapshot returned %d counters, want 1", len(m.Counters))
	}
	// nil slices are normalised to empty so the wire shape is stable.
	if m.Histograms == nil || m.Gauges == nil {
		t.Error("metrics.snapshot Histograms/Gauges are nil, want empty slices")
	}
	if m.SnapshotAt == 0 {
		t.Error("metrics.snapshot SnapshotAt = 0, want the clock-stamped value")
	}
}

// TestPostureDispatch_CrossTenantRequiresAdmin pins D-079: a request
// whose body Tenant differs from the ctx-verified tenant is rejected
// CodeScopeMismatch without the admin scope, accepted with it.
func TestPostureDispatch_CrossTenantRequiresAdmin(t *testing.T) {
	s := newPostureFixture(t)

	// Verified identity is tenant-a; the request asks for tenant-b.
	verified := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "sess"}
	ctxVerified, err := identity.With(context.Background(), verified)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	crossReq := &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "tenant-b", User: "u", Session: "sess"},
	}

	// (a) no scope → CodeScopeMismatch.
	_, err = s.Dispatch(ctxVerified, methods.MethodRuntimeCounters, crossReq)
	assertPostureCode(t, err, protoerrors.CodeScopeMismatch)

	// (b) admin scope → accepted.
	ctxAdmin := auth.WithScopes(ctxVerified, []auth.Scope{auth.ScopeAdmin})
	out, err := s.Dispatch(ctxAdmin, methods.MethodRuntimeCounters, crossReq)
	if err != nil {
		t.Fatalf("Dispatch with admin scope: %v", err)
	}
	if _, ok := out.(*types.RuntimeCounters); !ok {
		t.Fatalf("admin cross-tenant runtime.counters returned %T", out)
	}

	// (c) console:fleet scope also satisfies the gate (D-079 two-scope set).
	ctxFleet := auth.WithScopes(ctxVerified, []auth.Scope{auth.ScopeConsoleFleet})
	if _, err := s.Dispatch(ctxFleet, methods.MethodRuntimeCounters, crossReq); err != nil {
		t.Fatalf("Dispatch with console:fleet scope: %v", err)
	}

	// (d) same-tenant request needs no scope.
	sameReq := &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "tenant-a", User: "u", Session: "sess"},
	}
	if _, err := s.Dispatch(ctxVerified, methods.MethodRuntimeInfo, sameReq); err != nil {
		t.Fatalf("same-tenant Dispatch unexpectedly rejected: %v", err)
	}
}

// TestPostureDispatch_GovernancePosture pins the Phase 72g (D-112)
// `governance.posture` happy path — the projected IdentityTiers view.
func TestPostureDispatch_GovernancePosture(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodGovernancePosture, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(governance.posture): %v", err)
	}
	gp, ok := out.(*types.GovernancePostureResponse)
	if !ok {
		t.Fatalf("governance.posture returned %T, want *types.GovernancePostureResponse", out)
	}
	if gp.DefaultTier != "free" {
		t.Errorf("DefaultTier = %q, want free", gp.DefaultTier)
	}
	tier, ok := gp.IdentityTiers["free"]
	if !ok {
		t.Fatalf("IdentityTiers missing the 'free' tier: %+v", gp.IdentityTiers)
	}
	if tier.BudgetCeilingUSD != 5.0 || tier.MaxTokens != 2048 {
		t.Errorf("tier projection wrong: %+v", tier)
	}
	if tier.RateLimit.Capacity != 100 || tier.RateLimit.RefillIntervalMS != 1000 {
		t.Errorf("rate-limit projection wrong: %+v", tier.RateLimit)
	}
	if gp.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", gp.ProtocolVersion, types.ProtocolVersion)
	}
}

// TestPostureDispatch_LLMPosture pins the Phase 72g (D-112) `llm.posture`
// happy path — the projected provider/model/region.
func TestPostureDispatch_LLMPosture(t *testing.T) {
	s := newPostureFixture(t)
	out, err := s.Dispatch(context.Background(), methods.MethodLLMPosture, validRequest())
	if err != nil {
		t.Fatalf("Dispatch(llm.posture): %v", err)
	}
	lp, ok := out.(*types.LLMPostureResponse)
	if !ok {
		t.Fatalf("llm.posture returned %T, want *types.LLMPostureResponse", out)
	}
	if lp.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", lp.Provider)
	}
	if lp.Model != "openai/gpt-5.3-chat" {
		t.Errorf("Model = %q, want openai/gpt-5.3-chat", lp.Model)
	}
	if lp.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", lp.ProtocolVersion, types.ProtocolVersion)
	}
}

// TestPostureDispatch_CrossTenantConfigReadEmitsAudit pins the Phase 72g
// (D-112) cross-tenant audit emit: an admin-scoped cross-tenant
// `governance.posture` / `llm.posture` read publishes the
// `*.posture_read_admin` event; an own-tenant read does not.
func TestPostureDispatch_CrossTenantConfigReadEmitsAudit(t *testing.T) {
	for _, tc := range []struct {
		method    methods.Method
		eventType events.EventType
	}{
		{methods.MethodGovernancePosture, governance.EventTypePostureReadAdmin},
		{methods.MethodLLMPosture, llm.EventTypePostureReadAdmin},
	} {
		t.Run(string(tc.method), func(t *testing.T) {
			bus := newPostureBus(t)
			// An admin-scoped subscription observes cross-tenant emits.
			sub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
			if err != nil {
				t.Fatalf("bus.Subscribe: %v", err)
			}
			defer sub.Cancel()

			deps := basePostureDeps(t)
			deps.Bus = bus
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatalf("NewPostureSurface: %v", err)
			}

			verified := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "sess"}
			ctxAdmin := auth.WithScopes(mustCtx(t, verified), []auth.Scope{auth.ScopeAdmin})
			crossReq := &types.RuntimeInfoRequest{
				Identity: types.IdentityScope{Tenant: "tenant-b", User: "u", Session: "sess"},
			}
			if _, err := s.Dispatch(ctxAdmin, tc.method, crossReq); err != nil {
				t.Fatalf("cross-tenant Dispatch: %v", err)
			}
			// The cross-tenant read MUST emit the typed
			// *.posture_read_admin event (the Admin-true subscription
			// may also surface its own admin-scope-used event — only
			// the posture-typed event is asserted).
			deadline := time.After(2 * time.Second)
			var saw bool
			for !saw {
				select {
				case ev := <-sub.Events():
					if ev.Type == tc.eventType {
						saw = true
					}
				case <-deadline:
					t.Fatalf("no %s event emitted for cross-tenant read", tc.eventType)
				}
			}

			// Own-tenant read emits no posture audit event.
			sameReq := &types.RuntimeInfoRequest{
				Identity: types.IdentityScope{Tenant: "tenant-a", User: "u", Session: "sess"},
			}
			if _, err := s.Dispatch(mustCtx(t, verified), tc.method, sameReq); err != nil {
				t.Fatalf("own-tenant Dispatch: %v", err)
			}
			quiet := time.After(400 * time.Millisecond)
			for {
				select {
				case ev := <-sub.Events():
					if ev.Type == governance.EventTypePostureReadAdmin ||
						ev.Type == llm.EventTypePostureReadAdmin {
						t.Fatalf("own-tenant read unexpectedly emitted %q", ev.Type)
					}
				case <-quiet:
					return
				}
			}
		})
	}
}

// basePostureDeps builds a complete PostureDeps for tests that need to
// override one seam.
func basePostureDeps(t *testing.T) protocol.PostureDeps {
	t.Helper()
	return protocol.PostureDeps{
		Clock:    fixedClock,
		BootedAt: fixedClock().Add(-1 * time.Hour),
		Health:   func(context.Context) []types.SubsystemHealth { return nil },
		Counters: func(context.Context, identity.Identity) types.RuntimeCounters {
			return types.RuntimeCounters{}
		},
		Drivers:    func() []types.SubsystemDriver { return nil },
		Metrics:    func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance: newPostureGovernance(),
		LLM:        newPostureLLM(),
		Redactor:   patterns.New(),
		Bus:        newPostureBus(t),
		InstanceID: "inst-test-001",
	}
}

// mustCtx threads identity into a fresh context or fails the test.
func mustCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// assertPostureCode asserts err is a *protoerrors.Error with the given
// code.
func assertPostureCode(t *testing.T, err error, want protoerrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", want)
	}
	var perr *protoerrors.Error
	if !stderrors.As(err, &perr) {
		t.Fatalf("expected *protoerrors.Error, got %T: %v", err, err)
	}
	if perr.Code != want {
		t.Fatalf("error code = %q, want %q (message: %s)", perr.Code, want, perr.Message)
	}
}

// TestPostureSurface_Info_WiredCapabilities — round-8 F1 / phase 84a:
// `runtime.info.capabilities` is the per-instance wired subset of the
// canonical Protocol capability set. Always-on surfaces (task control,
// events subscribe, runtime posture) appear unconditionally;
// `topology_snapshot` appears iff the runtime advertised
// `PostureDeps.TopologyAvailable=true`. The ordering is
// lexicographic — pinned so the wire shape is deterministic across
// future capability additions.
func TestPostureSurface_Info_WiredCapabilities(t *testing.T) {
	t.Parallel()

	mkSurface := func(t *testing.T, topology bool) *protocol.PostureSurface {
		t.Helper()
		deps := protocol.PostureDeps{
			Build:    types.RuntimeInfo{BuildVersion: "v0", BuildGoVersion: "go1.26"},
			Clock:    fixedClock,
			BootedAt: fixedClock(),
			Health: func(_ context.Context) []types.SubsystemHealth {
				return nil
			},
			Counters: func(_ context.Context, _ identity.Identity) types.RuntimeCounters {
				return types.RuntimeCounters{}
			},
			Drivers: func() []types.SubsystemDriver { return nil },
			Metrics: func(_ context.Context) types.MetricsSnapshot {
				return types.MetricsSnapshot{}
			},
			Governance:        newPostureGovernance(),
			LLM:               newPostureLLM(),
			Redactor:          patterns.New(),
			Bus:               newPostureBus(t),
			DisplayName:       "wired-caps-test",
			InstanceID:        "inst-wired-001",
			TopologyAvailable: topology,
		}
		s, err := protocol.NewPostureSurface(deps)
		if err != nil {
			t.Fatalf("NewPostureSurface: %v", err)
		}
		return s
	}

	dispatch := func(t *testing.T, s *protocol.PostureSurface) *types.RuntimeInfo {
		t.Helper()
		ctx, _ := identity.With(context.Background(), identity.Identity{
			TenantID:  "tenant-a",
			UserID:    "user-1",
			SessionID: "session-x",
		})
		resp, err := s.Dispatch(ctx, methods.MethodRuntimeInfo, validRequest())
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		ri, ok := resp.(*types.RuntimeInfo)
		if !ok {
			t.Fatalf("resp type = %T, want *RuntimeInfo", resp)
		}
		return ri
	}

	t.Run("topology-disabled-omits-cap", func(t *testing.T) {
		t.Parallel()
		ri := dispatch(t, mkSurface(t, false))
		want := []types.Capability{
			types.CapEventsSubscribe,
			types.CapRuntimePosture,
			types.CapTaskControl,
		}
		if !reflect.DeepEqual(ri.Capabilities, want) {
			t.Fatalf("capabilities = %v, want %v", ri.Capabilities, want)
		}
		for _, c := range ri.Capabilities {
			if c == types.CapTopologySnapshot {
				t.Fatalf("topology_snapshot leaked into capabilities when TopologyAvailable=false")
			}
		}
	})

	t.Run("topology-enabled-includes-cap-lexicographically", func(t *testing.T) {
		t.Parallel()
		ri := dispatch(t, mkSurface(t, true))
		want := []types.Capability{
			types.CapEventsSubscribe,
			types.CapRuntimePosture,
			types.CapTaskControl,
			types.CapTopologySnapshot,
		}
		if !reflect.DeepEqual(ri.Capabilities, want) {
			t.Fatalf("capabilities = %v, want %v", ri.Capabilities, want)
		}
	})
}
