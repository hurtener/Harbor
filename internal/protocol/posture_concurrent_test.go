package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestConcurrentReuse_PostureSurface pins the D-025 concurrent-reuse
// contract: N≥100 goroutines run Dispatch against ONE shared
// PostureSurface under -race. Each goroutine drives a distinct identity
// triple, so a context bleed surfaces as a foreign tenant length in the
// runtime.counters response (the fixture's Counters seam echoes the
// caller's tenant length into TasksRunning). The test asserts:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each runtime.counters response carries the
//     caller's own tenant length, never a sibling's;
//   - no goroutine leak — baseline runtime.NumGoroutine restored after
//     all Dispatch calls return.
//
// PostureSurface is a compiled artifact: every field is set once at
// NewPostureSurface; Dispatch reads run-specific data from ctx + the
// request argument, never from the surface struct.
func TestConcurrentReuse_PostureSurface(t *testing.T) {
	const n = 150 // ≥100 per the D-025 contract

	// Build a surface whose Counters seam echoes the caller's tenant
	// length — so a context bleed is observable.
	s, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Build:    types.RuntimeInfo{BuildVersion: "v0", BuildGoVersion: "go1.26"},
		Clock:    func() time.Time { return time.Unix(1_747_000_000, 0).UTC() },
		BootedAt: time.Unix(1_746_000_000, 0).UTC(),
		Health: func(context.Context) []types.SubsystemHealth {
			return []types.SubsystemHealth{{Subsystem: "events", Status: types.HealthStatusReady}}
		},
		Counters: func(_ context.Context, ident identity.Identity) types.RuntimeCounters {
			return types.RuntimeCounters{TasksRunning: int64(len(ident.TenantID))}
		},
		Drivers: func() []types.SubsystemDriver {
			return []types.SubsystemDriver{{Subsystem: "state", Driver: "inmem"}}
		},
		Metrics:    func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance: newPostureGovernance(),
		LLM:        newPostureLLM(),
		Redactor:   patterns.New(),
		Bus:        newPostureBus(t),
		InstanceID: "inst-concurrent",
	})
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n*5)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct per-goroutine tenant; the length varies so a
			// bleed shows up as a wrong TasksRunning count.
			tenant := fmt.Sprintf("tenant-%d", i)
			req := &types.RuntimeInfoRequest{
				Identity: types.IdentityScope{
					Tenant:  tenant,
					User:    fmt.Sprintf("user-%d", i),
					Session: fmt.Sprintf("session-%d", i),
				},
			}

			// Exercise every posture method once per goroutine —
			// including the Phase 72g governance / llm reads.
			for _, m := range []methods.Method{
				methods.MethodRuntimeInfo, methods.MethodRuntimeHealth,
				methods.MethodRuntimeCounters, methods.MethodRuntimeDrivers,
				methods.MethodMetricsSnapshot, methods.MethodGovernancePosture,
				methods.MethodLLMPosture,
			} {
				out, derr := s.Dispatch(context.Background(), m, req)
				if derr != nil {
					errs <- fmt.Errorf("goroutine-%d %s: %w", i, m, derr)
					return
				}
				if out == nil {
					errs <- fmt.Errorf("goroutine-%d %s: nil response", i, m)
					return
				}
			}

			// Context-bleed check: runtime.counters must echo THIS
			// goroutine's tenant length.
			out, derr := s.Dispatch(context.Background(), methods.MethodRuntimeCounters, req)
			if derr != nil {
				errs <- fmt.Errorf("goroutine-%d counters: %w", i, derr)
				return
			}
			c := out.(*types.RuntimeCounters)
			if c.TasksRunning != int64(len(tenant)) {
				errs <- fmt.Errorf("goroutine-%d counters: TasksRunning = %d, want %d — context bleed",
					i, c.TasksRunning, len(tenant))
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// No goroutine leak: the baseline is restored once every Dispatch
	// has returned.
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// TestConcurrentReuse_PostureHealth_WidenNoCrossTalk pins D-310's
// concurrent-reuse invariant on the widened `runtime.health` path: N≥10
// goroutines mixing elevated (admin / console:fleet) and ordinary callers
// run against ONE shared PostureSurface under -race. An ordinary caller
// must NEVER observe a widened (runtime-scope) tasks horizon, and an
// elevated caller must ALWAYS observe one — no widening state bleeds
// across concurrent reads (Dispatch derives `widened` from each call's
// ctx, never from the surface). No goroutine leak after teardown.
func TestConcurrentReuse_PostureHealth_WidenNoCrossTalk(t *testing.T) {
	const n = 120 // ≥10 per §17.3; ≥100 keeps parity with the D-025 leg

	at := time.Unix(1_747_000_000, 0).UTC()
	deps := basePostureDeps(t)
	deps.Retention = func(_ context.Context, _ identity.Identity, widened bool) []types.RetentionHorizon {
		scope := types.RetentionScopeSession
		if widened {
			scope = types.RetentionScopeRuntime
		}
		return []types.RetentionHorizon{{Surface: "tasks", Scope: scope, OldestRetainedAt: &at}}
	}
	s, err := protocol.NewPostureSurface(deps)
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			verified := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx := mustCtx(t, verified)
			elevated := i%2 == 0
			if elevated {
				ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeConsoleFleet})
			}
			req := &types.RuntimeInfoRequest{Identity: types.IdentityScope{
				Tenant: verified.TenantID, User: verified.UserID, Session: verified.SessionID,
			}}
			out, derr := s.Dispatch(ctx, methods.MethodRuntimeHealth, req)
			if derr != nil {
				errs <- fmt.Errorf("goroutine-%d: %w", i, derr)
				return
			}
			h := out.(*types.RuntimeHealth)
			var got string
			for _, r := range h.Retention {
				if r.Surface == "tasks" {
					got = r.Scope
				}
			}
			want := types.RetentionScopeSession
			if elevated {
				want = types.RetentionScopeRuntime
			}
			if got != want {
				errs <- fmt.Errorf("goroutine-%d (elevated=%v): tasks scope = %q, want %q — widen bleed",
					i, elevated, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// TestPostureSurface_CancellationNoCrossTalk pins that cancelling one
// run's ctx does not affect a concurrent run — Dispatch reads no shared
// engine-level ctx.
func TestPostureSurface_CancellationNoCrossTalk(t *testing.T) {
	s := newPostureFixture(t)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel run A's ctx immediately

	// Run B uses a live ctx; its Dispatch must succeed despite A's
	// cancellation.
	out, err := s.Dispatch(context.Background(), methods.MethodRuntimeInfo, validRequest())
	if err != nil {
		t.Fatalf("run B Dispatch unexpectedly failed after run A's ctx cancel: %v", err)
	}
	if _, ok := out.(*types.RuntimeInfo); !ok {
		t.Fatalf("run B runtime.info returned %T", out)
	}

	// Run A with the cancelled ctx still produces a structured result —
	// the posture handlers do no blocking I/O, so a cancelled ctx does
	// not corrupt the response (the identity gate still passes).
	if _, err := s.Dispatch(cancelledCtx, methods.MethodRuntimeInfo, validRequest()); err != nil {
		t.Fatalf("run A Dispatch with cancelled ctx: %v", err)
	}
}
