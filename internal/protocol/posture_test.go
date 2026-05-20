package protocol_test

import (
	stderrors "errors"
	"testing"
	"time"

	"context"
	"github.com/hurtener/Harbor/internal/identity"
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
			InstanceID: "i",
		}
	}
	cases := map[string]func(*protocol.PostureDeps){
		"nil Clock":        func(d *protocol.PostureDeps) { d.Clock = nil },
		"nil Health":       func(d *protocol.PostureDeps) { d.Health = nil },
		"nil Counters":     func(d *protocol.PostureDeps) { d.Counters = nil },
		"nil Drivers":      func(d *protocol.PostureDeps) { d.Drivers = nil },
		"nil Metrics":      func(d *protocol.PostureDeps) { d.Metrics = nil },
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
		methods.MethodMetricsSnapshot,
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
