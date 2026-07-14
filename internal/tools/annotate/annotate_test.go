package annotate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/annotate"
	"github.com/hurtener/Harbor/internal/tools/approval"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// baseTime is the fixed clock instant the annotator's metrics windows are
// computed relative to; every published fixture event is dated relative to it.
var baseTime = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return baseTime } }

func newBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		ReplayBufferSize:         512,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newCatalog(t *testing.T, tls ...tools.Tool) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	for _, tl := range tls {
		if tl.Loading == "" {
			tl.Loading = tools.LoadingAlways
		}
		if err := cat.Register(tools.ToolDescriptor{
			Tool:   tl,
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
		}); err != nil {
			t.Fatalf("register %q: %v", tl.Name, err)
		}
	}
	return cat
}

func newPolicyStore(t *testing.T) approval.PolicyStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	ps, err := approval.NewStatePolicyStore(st)
	if err != nil {
		t.Fatalf("NewStatePolicyStore: %v", err)
	}
	return ps
}

func publish(t *testing.T, bus events.EventBus, typ events.EventType, id identity.Identity, payload events.EventPayload, at time.Time) {
	t.Helper()
	if err := bus.Publish(context.Background(), events.Event{
		Type:       typ,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: at,
		Payload:    payload,
	}); err != nil {
		t.Fatalf("publish %s: %v", typ, err)
	}
}

func idA() identity.Identity {
	return identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-A"}
}

func mkAnnotator(t *testing.T, deps annotate.Deps) *annotate.Annotator {
	t.Helper()
	if deps.Clock == nil {
		deps.Clock = fixedClock()
	}
	a, err := annotate.NewAnnotator(deps)
	if err != nil {
		t.Fatalf("NewAnnotator: %v", err)
	}
	return a
}

func TestNewAnnotator_MissingDeps_FailLoud(t *testing.T) {
	if _, err := annotate.NewAnnotator(annotate.Deps{}); err == nil {
		t.Fatal("nil Catalog should fail loud")
	}
	if _, err := annotate.NewAnnotator(annotate.Deps{Catalog: newCatalog(t)}); err == nil {
		t.Fatal("nil Approval should fail loud")
	}
}

func TestAnnotator_Metrics_RealErrorRateAndStatus(t *testing.T) {
	id := idA()
	bus := newBus(t)
	a := mkAnnotator(t, annotate.Deps{
		Catalog:  newCatalog(t, tools.Tool{Name: "alpha", Transport: tools.TransportHTTP}),
		Approval: newPolicyStore(t),
		Events:   bus,
	})
	// 3 completed + 1 failed for alpha, all within the last 1h.
	for i := range 3 {
		publish(t, bus, tools.EventTypeToolCompleted, id,
			tools.ToolCompletedPayload{ToolName: "alpha", DurationMS: int64(10 + i)},
			baseTime.Add(-time.Duration(i+1)*time.Minute))
	}
	publish(t, bus, tools.EventTypeToolFailed, id,
		tools.ToolFailedPayload{ToolName: "alpha", ErrorClass: "boom"},
		baseTime.Add(-30*time.Second))
	// A completed for a DIFFERENT tool must not contaminate alpha's gauge.
	publish(t, bus, tools.EventTypeToolCompleted, id,
		tools.ToolCompletedPayload{ToolName: "beta"}, baseTime.Add(-time.Minute))

	m := a.Metrics(context.Background(), id, "alpha", prototypes.ToolWindow1h)
	if m.Invocations != 4 {
		t.Errorf("Invocations = %d, want 4", m.Invocations)
	}
	if m.Failures != 1 {
		t.Errorf("Failures = %d, want 1", m.Failures)
	}
	if m.ErrorRate1h < 0.24 || m.ErrorRate1h > 0.26 {
		t.Errorf("ErrorRate1h = %v, want ~0.25", m.ErrorRate1h)
	}
	if m.Status != prototypes.ToolStatusDegraded {
		t.Errorf("Status = %q, want Degraded", m.Status)
	}
	if m.ID != "alpha" || m.Window != prototypes.ToolWindow1h {
		t.Errorf("echo fields wrong: %+v", m)
	}
}

func TestAnnotator_Metrics_NoEvents_HealthyZero(t *testing.T) {
	id := idA()
	a := mkAnnotator(t, annotate.Deps{
		Catalog:  newCatalog(t, tools.Tool{Name: "alpha"}),
		Approval: newPolicyStore(t),
		Events:   newBus(t),
	})
	m := a.Metrics(context.Background(), id, "alpha", prototypes.ToolWindow24h)
	if m.Invocations != 0 || m.Failures != 0 {
		t.Errorf("want zero observations, got %+v", m)
	}
	if m.Status != prototypes.ToolStatusHealthy {
		t.Errorf("Status = %q, want Healthy (no failures observed)", m.Status)
	}
}

func TestAnnotator_LastUsedAt(t *testing.T) {
	id := idA()
	bus := newBus(t)
	a := mkAnnotator(t, annotate.Deps{
		Catalog:  newCatalog(t, tools.Tool{Name: "alpha"}),
		Approval: newPolicyStore(t),
		Events:   bus,
	})
	first := baseTime.Add(-2 * time.Hour)
	last := baseTime.Add(-5 * time.Minute)
	publish(t, bus, tools.EventTypeToolInvoked, id, tools.ToolInvokedPayload{ToolName: "alpha", StartedAt: first}, first)
	publish(t, bus, tools.EventTypeToolCompleted, id, tools.ToolCompletedPayload{ToolName: "alpha"}, last)

	got := a.LastUsedAt(context.Background(), id, "alpha")
	if !got.Equal(last) {
		t.Errorf("LastUsedAt = %v, want %v", got, last)
	}
	// A never-used tool reads the zero value.
	if lu := a.LastUsedAt(context.Background(), id, "unused"); !lu.IsZero() {
		t.Errorf("LastUsedAt(unused) = %v, want zero", lu)
	}
}

func TestAnnotator_ContentStats_HistogramFromOffload(t *testing.T) {
	id := idA()
	bus := newBus(t)
	const threshold = 32 * 1024
	a := mkAnnotator(t, annotate.Deps{
		Catalog:             newCatalog(t, tools.Tool{Name: "alpha", Transport: tools.TransportMCP}),
		Approval:            newPolicyStore(t),
		Events:              bus,
		HeavyThresholdBytes: threshold,
	})
	publish(t, bus, mcpdrv.EventTypeMCPResourceOffloaded, id,
		mcpdrv.ResourceOffloadedPayload{Source: "alpha", SizeBytes: 2000, ArtifactID: "a1"}, baseTime.Add(-time.Minute))
	publish(t, bus, mcpdrv.EventTypeMCPResourceOffloaded, id,
		mcpdrv.ResourceOffloadedPayload{Source: "alpha", SizeBytes: 40000, ArtifactID: "a2"}, baseTime.Add(-2*time.Minute))
	// A different tool's offload must not contaminate alpha's histogram.
	publish(t, bus, mcpdrv.EventTypeMCPResourceOffloaded, id,
		mcpdrv.ResourceOffloadedPayload{Source: "beta", SizeBytes: 999999, ArtifactID: "a3"}, baseTime.Add(-time.Minute))

	cs := a.ContentStats(context.Background(), id, "alpha")
	if cs.HeavyThresholdBytes != threshold {
		t.Errorf("HeavyThresholdBytes = %d, want %d", cs.HeavyThresholdBytes, threshold)
	}
	if cs.HeavyCount != 1 {
		t.Errorf("HeavyCount = %d, want 1 (the 40000-byte result)", cs.HeavyCount)
	}
	if len(cs.Histogram) != 2 {
		t.Fatalf("Histogram buckets = %d, want 2: %+v", len(cs.Histogram), cs.Histogram)
	}
	// 2000 → 2048 bucket, 40000 → 65536 bucket, ascending.
	if cs.Histogram[0].MaxBytes != 2048 || cs.Histogram[0].Count != 1 {
		t.Errorf("bucket[0] = %+v, want {2048,1}", cs.Histogram[0])
	}
	if cs.Histogram[1].MaxBytes != 65536 || cs.Histogram[1].Count != 1 {
		t.Errorf("bucket[1] = %+v, want {65536,1}", cs.Histogram[1])
	}
}

func TestAnnotator_ApprovalPolicy_RoundTrip(t *testing.T) {
	id := idA()
	a := mkAnnotator(t, annotate.Deps{
		Catalog:  newCatalog(t, tools.Tool{Name: "alpha"}),
		Approval: newPolicyStore(t),
		Events:   newBus(t),
	})
	// Unset → semantic default auto.
	if p := a.ApprovalPolicy(context.Background(), id, "alpha"); p != prototypes.ToolApprovalAuto {
		t.Errorf("default policy = %q, want auto", p)
	}
	// Set gated → read-back reflects it.
	if err := a.SetApprovalPolicy(context.Background(), id, "alpha", prototypes.ToolApprovalGated); err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	if p := a.ApprovalPolicy(context.Background(), id, "alpha"); p != prototypes.ToolApprovalGated {
		t.Errorf("policy after set = %q, want gated", p)
	}
	// An invalid policy fails loud (fail-closed, never a silent no-op).
	if err := a.SetApprovalPolicy(context.Background(), id, "alpha", prototypes.ToolApprovalPolicy("bogus")); err == nil {
		t.Fatal("invalid policy should fail loud")
	}
}

// fakeOAuthReader is a deterministic OAuthReader for the annotator's OAuth
// delegation unit test. The REAL provider-backed reader is exercised by the
// integration test (real tools/auth).
type fakeOAuthReader struct {
	mu      sync.Mutex
	revoked int
}

func (f *fakeOAuthReader) Status(_ context.Context, _ identity.Identity, source tools.ToolSourceID) prototypes.ToolOAuthStatus {
	if source == "needs-oauth" {
		return prototypes.ToolOAuthRequired
	}
	return prototypes.ToolOAuthNotApplicable
}

func (f *fakeOAuthReader) Revoke(_ context.Context, _ identity.Identity, source tools.ToolSourceID) (int64, error) {
	if source != "needs-oauth" {
		return 0, nil
	}
	f.mu.Lock()
	f.revoked++
	f.mu.Unlock()
	return 2, nil
}

func TestAnnotator_OAuthStatus_ResolvesBySource(t *testing.T) {
	id := idA()
	a := mkAnnotator(t, annotate.Deps{
		Catalog: newCatalog(t,
			tools.Tool{Name: "bound_tool", Source: "needs-oauth", Transport: tools.TransportMCP},
			tools.Tool{Name: "plain_tool", Transport: tools.TransportInProcess},
		),
		Approval: newPolicyStore(t),
		Events:   newBus(t),
		OAuth:    &fakeOAuthReader{},
	})
	if s := a.OAuthStatus(context.Background(), id, "bound_tool"); s != prototypes.ToolOAuthRequired {
		t.Errorf("bound_tool OAuth = %q, want Required", s)
	}
	if s := a.OAuthStatus(context.Background(), id, "plain_tool"); s != prototypes.ToolOAuthNotApplicable {
		t.Errorf("plain_tool OAuth = %q, want n/a", s)
	}
	// Revoke routes through the reader by the tool's source.
	n, err := a.RevokeOAuth(context.Background(), id, "bound_tool")
	if err != nil {
		t.Fatalf("RevokeOAuth: %v", err)
	}
	if n != 2 {
		t.Errorf("RevokeOAuth count = %d, want 2", n)
	}
	// A tool with no OAuth source revokes zero (honest), never fabricated.
	if n, _ := a.RevokeOAuth(context.Background(), id, "plain_tool"); n != 0 {
		t.Errorf("RevokeOAuth(plain) = %d, want 0", n)
	}
}

func TestAnnotator_Isolation_MetricsDoNotBleedAcrossSessions(t *testing.T) {
	sessA := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-A"}
	sessB := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-B"}
	bus := newBus(t)
	a := mkAnnotator(t, annotate.Deps{
		Catalog:  newCatalog(t, tools.Tool{Name: "alpha"}),
		Approval: newPolicyStore(t),
		Events:   bus,
	})
	// Only session A records invocations.
	publish(t, bus, tools.EventTypeToolCompleted, sessA, tools.ToolCompletedPayload{ToolName: "alpha"}, baseTime.Add(-time.Minute))
	publish(t, bus, tools.EventTypeToolFailed, sessA, tools.ToolFailedPayload{ToolName: "alpha"}, baseTime.Add(-2*time.Minute))

	if m := a.Metrics(context.Background(), sessA, "alpha", prototypes.ToolWindow1h); m.Invocations != 2 {
		t.Fatalf("session A invocations = %d, want 2", m.Invocations)
	}
	// Session B must see NONE of session A's activity.
	mB := a.Metrics(context.Background(), sessB, "alpha", prototypes.ToolWindow1h)
	if mB.Invocations != 0 || mB.Failures != 0 {
		t.Fatalf("session B saw session A's metrics: %+v — cross-session bleed", mB)
	}
	if lu := a.LastUsedAt(context.Background(), sessB, "alpha"); !lu.IsZero() {
		t.Fatalf("session B LastUsedAt = %v, want zero — cross-session bleed", lu)
	}
	// A tenant-B session with a distinct tenant sees nothing either.
	sessC := identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s-A"}
	if m := a.Metrics(context.Background(), sessC, "alpha", prototypes.ToolWindow1h); m.Invocations != 0 {
		t.Fatalf("tenant-B session saw tenant-A metrics: %+v — cross-tenant bleed", m)
	}
}

// TestAnnotator_ConcurrentReuse pins the D-025 concurrent-reuse contract: one
// shared *Annotator is exercised by N=128 goroutines under -race, each driving
// a distinct identity + a mix of the read + admin methods. Asserts no data
// race and no context bleed (a session's set policy reads back its own value).
func TestAnnotator_ConcurrentReuse(t *testing.T) {
	bus := newBus(t)
	a := mkAnnotator(t, annotate.Deps{
		Catalog: newCatalog(t,
			tools.Tool{Name: "alpha", Source: "needs-oauth", Transport: tools.TransportMCP},
		),
		Approval: newPolicyStore(t),
		Events:   bus,
		OAuth:    &fakeOAuthReader{},
	})
	const workers = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)
	for i := range workers {
		go func(n int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", n%5),
				UserID:    fmt.Sprintf("u-%d", n),
				SessionID: fmt.Sprintf("s-%d", n),
			}
			ctx := context.Background()
			_ = a.OAuthStatus(ctx, id, "alpha")
			_ = a.Metrics(ctx, id, "alpha", prototypes.ToolWindow24h)
			_ = a.LastUsedAt(ctx, id, "alpha")
			_ = a.ContentStats(ctx, id, "alpha")
			want := prototypes.ToolApprovalGated
			if n%2 == 0 {
				want = prototypes.ToolApprovalDenied
			}
			if err := a.SetApprovalPolicy(ctx, id, "alpha", want); err != nil {
				errCh <- fmt.Errorf("worker %d set: %w", n, err)
				return
			}
			if got := a.ApprovalPolicy(ctx, id, "alpha"); got != want {
				errCh <- fmt.Errorf("worker %d policy = %q, want %q — context bleed", n, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	// Allow the runtime a moment to reap transient goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+10 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
