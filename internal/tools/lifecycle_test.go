package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// lifecycleTestID is the canonical test identity (documented dummy
// values per CLAUDE.md §7 rule 2).
var lifecycleTestID = identity.Identity{
	TenantID:  "tenant-lifecycle",
	UserID:    "user-lifecycle",
	SessionID: "session-lifecycle",
}

const lifecycleRunID = "run-lifecycle-1"

// newLifecycleBus builds a real inmem event bus for lifecycle-emit tests.
func newLifecycleBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// lifecycleCtx returns a ctx carrying the FULL run quadruple, mirroring
// the run-loop dispatch edge (identity.WithRun stamps the quadruple).
func lifecycleCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), lifecycleTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, lifecycleTestID, lifecycleRunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// registerInvoke registers a tool whose Invoke returns (res, err).
func registerInvoke(t *testing.T, cat tools.ToolCatalog, name string, transport tools.TransportKind, invoke func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error)) {
	t.Helper()
	if err := cat.Register(tools.ToolDescriptor{
		Tool:   tools.Tool{Name: name, Transport: transport},
		Invoke: invoke,
	}); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

// collectTypes drains the subscription until `want` events arrive or the
// deadline passes, returning them keyed by type (last-write-wins per type).
func collectTypes(t *testing.T, sub events.Subscription, want int) []events.Event {
	t.Helper()
	got := make([]events.Event, 0, want)
	deadline := time.After(2 * time.Second)
	for len(got) < want {
		select {
		case ev := <-sub.Events():
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

// TestCatalogLifecycle_SuccessEmitsQuadrupleStamped asserts the universal
// descriptor-wrap shell publishes tool.invoked + tool.completed with the
// FULL run quadruple on the envelope (the b3 attribution fix) and the
// content-free payload keys — for a catalog constructed with a bus.
func TestCatalogLifecycle_SuccessEmitsQuadrupleStamped(t *testing.T) {
	t.Parallel()
	bus := newLifecycleBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	registerInvoke(t, cat, "ok_tool", tools.TransportMCP, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
	})

	desc, ok := cat.Resolve("ok_tool")
	if !ok {
		t.Fatal("Resolve: not found")
	}
	if _, err := desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	evs := collectTypes(t, sub, 2)
	if len(evs) != 2 {
		t.Fatalf("observed %d events, want 2 (invoked+completed): %+v", len(evs), evs)
	}
	var sawInvoked, sawCompleted bool
	for _, ev := range evs {
		// Envelope carries the FULL quadruple (run id populated).
		if ev.Identity.RunID != lifecycleRunID {
			t.Errorf("event %s envelope RunID=%q, want %q (attribution-dead)", ev.Type, ev.Identity.RunID, lifecycleRunID)
		}
		switch p := ev.Payload.(type) {
		case tools.ToolInvokedPayload:
			sawInvoked = true
			if p.ToolName != "ok_tool" || p.Transport != tools.TransportMCP {
				t.Errorf("invoked payload = %+v", p)
			}
			if p.Identity.RunID != lifecycleRunID {
				t.Errorf("invoked payload RunID=%q", p.Identity.RunID)
			}
		case tools.ToolCompletedPayload:
			sawCompleted = true
			if p.ToolName != "ok_tool" || p.Transport != tools.TransportMCP {
				t.Errorf("completed payload = %+v", p)
			}
			if p.Attempts != 1 {
				t.Errorf("completed Attempts=%d want 1", p.Attempts)
			}
		}
	}
	if !sawInvoked || !sawCompleted {
		t.Errorf("sawInvoked=%v sawCompleted=%v", sawInvoked, sawCompleted)
	}
}

// TestCatalogLifecycle_TerminalShapes tables the failure classifications
// the shell maps: nil→completed, ErrToolInvalidArgs→invalid_args,
// ErrToolPolicyExhausted→policy_exhausted, other→failed.
func TestCatalogLifecycle_TerminalShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		retErr   error
		wantTerm events.EventType
	}{
		{"success", nil, tools.EventTypeToolCompleted},
		{"invalid_args", tools.ErrToolInvalidArgs, tools.EventTypeToolInvalidArgs},
		{"policy_exhausted", tools.ErrToolPolicyExhausted, tools.EventTypeToolPolicyExhausted},
		{"failed", context.DeadlineExceeded, tools.EventTypeToolFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bus := newLifecycleBus(t)
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{tools.EventTypeToolInvoked, tc.wantTerm},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			cat := tools.NewCatalog(tools.WithCatalogBus(bus))
			registerInvoke(t, cat, "tool", tools.TransportInProcess, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, tc.retErr
			})
			desc, _ := cat.Resolve("tool")
			_, _ = desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`))

			evs := collectTypes(t, sub, 2)
			var sawInvoked, sawTerm bool
			for _, ev := range evs {
				if ev.Type == tools.EventTypeToolInvoked {
					sawInvoked = true
				}
				if ev.Type == tc.wantTerm {
					sawTerm = true
				}
				if ev.Identity.RunID != lifecycleRunID {
					t.Errorf("event %s RunID=%q not turn-attributable", ev.Type, ev.Identity.RunID)
				}
			}
			if !sawInvoked {
				t.Error("no tool.invoked emitted")
			}
			if !sawTerm {
				t.Errorf("terminal %s not emitted; got %+v", tc.wantTerm, evs)
			}
		})
	}
}

// TestCatalogLifecycle_NoBusNoEmit asserts a bus-less catalog wraps
// nothing — the descriptor's own Invoke runs verbatim (a pure no-op).
func TestCatalogLifecycle_NoBusNoEmit(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog() // no bus
	called := false
	registerInvoke(t, cat, "tool", tools.TransportInProcess, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		called = true
		return tools.ToolResult{Value: 1}, nil
	})
	desc, _ := cat.Resolve("tool")
	if _, err := desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !called {
		t.Error("inner Invoke not called through the no-op catalog")
	}
}

// TestCatalogLifecycle_NoDoubleShellAfterReplace pins the reviewer-verified
// invariant: the wiring builder's Replace (approval/oauth re-install)
// swaps in a descriptor whose inner Invoke is the ALREADY-shelled one, so
// a single invocation emits EXACTLY ONE tool.invoked — never two.
func TestCatalogLifecycle_NoDoubleShellAfterReplace(t *testing.T) {
	t.Parallel()
	bus := newLifecycleBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolInvoked},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	registerInvoke(t, cat, "tool", tools.TransportInProcess, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: 1}, nil
	})

	// Simulate the wiring builder: resolve the (shelled) descriptor, wrap
	// its Invoke in an outer passthrough (as WrapWithApproval/OAuth do),
	// and Replace. Replace must NOT re-shell.
	resolved, _ := cat.Resolve("tool")
	inner := resolved.Invoke
	wrapped := resolved
	wrapped.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return inner(ctx, args)
	}
	replacer, ok := cat.(tools.CatalogReplacer)
	if !ok {
		t.Fatal("catalog is not a CatalogReplacer")
	}
	if err := replacer.Replace([]tools.ToolDescriptor{wrapped}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	desc, _ := cat.Resolve("tool")
	if _, err := desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Exactly one tool.invoked — collect for a bounded window and count.
	count := 0
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == tools.EventTypeToolInvoked {
				count++
			}
		case <-deadline:
			break loop
		}
	}
	if count != 1 {
		t.Fatalf("tool.invoked emitted %d times after Replace, want exactly 1 (double-shell)", count)
	}
}

// TestCatalogLifecycle_ConcurrentReuse_NoAttributionBleed extends the D-025
// concurrent-reuse contract to the descriptor-wrap shell: N≥100 concurrent
// invocations of ONE shared descriptor on ONE shared bus-carrying catalog,
// each under a DISTINCT run quadruple, must each emit events stamped with THAT
// run's id — no cross-run attribution bleed — under -race.
func TestCatalogLifecycle_ConcurrentReuse_NoAttributionBleed(t *testing.T) {
	t.Parallel()
	// A large subscriber buffer so the N*2 burst of lifecycle events is not
	// dropped by the bus's bounded drop-on-overflow policy (the drop policy is
	// correct production behaviour; here it would mask the attribution check
	// this test exists for). The contract under test is no cross-run
	// attribution BLEED, not the bus's backpressure behaviour.
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     8192,
		IdleTimeout:              10 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	registerInvoke(t, cat, "shared", tools.TransportInProcess, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: 1}, nil
	})
	desc, _ := cat.Resolve("shared")

	const n = 128
	// Collector: every event's RunID must be one we launched, and each run's
	// events must carry ITS OWN run id (no bleed).
	seen := make(map[string]int)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.After(5 * time.Second)
		count := 0
		for count < n*2 { // invoked + completed per run
			select {
			case ev := <-sub.Events():
				mu.Lock()
				seen[ev.Identity.RunID]++
				mu.Unlock()
				count++
			case <-deadline:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("s-%d", i)}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				t.Errorf("identity.With: %v", err)
				return
			}
			runID := fmt.Sprintf("run-%d", i)
			ctx, err = identity.WithRun(ctx, id, runID)
			if err != nil {
				t.Errorf("identity.WithRun: %v", err)
				return
			}
			if _, err := desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
				t.Errorf("Invoke: %v", err)
			}
		}(i)
	}
	wg.Wait()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for i := range n {
		runID := fmt.Sprintf("run-%d", i)
		if seen[runID] != 2 {
			t.Errorf("run %s: observed %d events, want 2 (invoked+completed) — attribution bleed or drop", runID, seen[runID])
		}
	}
}
