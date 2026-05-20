// Phase 72d integration test — the notification.* event topic +
// rules-engine-lite mapper + Subscriber exercised end-to-end against
// real audit + events + (tasks/auth/approval/governance/pauseresume)
// payload drivers. No mocks at the seam, per CLAUDE.md §17.
//
// What this proves end-to-end:
//
//   - For each V1 mapping (task.failed, tool.approval_requested,
//     governance.budget_exceeded, tool.auth_required, pause.requested):
//     a deliberate publish through the real inmem EventBus + real audit
//     redactor flows through the notifications.Subscriber and the
//     corresponding notification.<class> arrives at a separately-scoped
//     subscriber.
//   - Identity propagation: every synthesised notification carries the
//     trigger's identity.Quadruple unchanged (cross-tenant isolation
//     lives or dies at this seam).
//   - Failure mode: a trigger event whose Identity carries the D-033
//     `<missing>` sentinel produces a notification.identity_rejected
//     event (fail-loudly per CLAUDE.md §13) and does NOT silently emit
//     a malformed notification.
//   - Concurrency stress: N=20 concurrent producers each fire a
//     mix of trigger event types against a single shared bus +
//     Subscriber; assert no cross-talk, no dropped notifications,
//     baseline goroutine count restored after teardown (§17.3 long-
//     lived-wiring requirement).
package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// TestE2E_NotificationsTopic_AllV1Mappings asserts every V1 mapping
// works end-to-end against the real bus + Subscriber, with identity
// propagation enforced on each synthesised notification.
func TestE2E_NotificationsTopic_AllV1Mappings(t *testing.T) {
	t.Parallel()

	bus, _ := openNotificationsBus(t)

	// Subscribe an admin-scope listener for the entire notification.*
	// family so we can confirm each mapping by class.
	listener, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: notifications.V1NotificationClasses(),
	})
	if err != nil {
		t.Fatalf("Subscribe listener: %v", err)
	}
	defer listener.Cancel()

	// Launch the Subscriber.
	startSubscriber(t, bus)

	// Build one trigger per V1 mapping, each with a distinct identity
	// so we can assert per-trigger identity propagation.
	type triggerCase struct {
		name        string
		identity    identity.Quadruple
		trigger     events.Event
		wantClass   events.EventType
		wantSev     notifications.Severity
		mustContain string // substring assertion on the redacted summary
	}
	cases := []triggerCase{
		{
			name:     "task.failed",
			identity: triple("a"),
			trigger: events.Event{
				Type:     tasks.EventTypeTaskFailed,
				Identity: triple("a"),
				Payload:  tasks.TaskFailedPayload{TaskID: "task-a", ErrorCode: "ec-a"},
			},
			wantClass:   notifications.EventTypeNotificationTaskFailed,
			wantSev:     notifications.SeverityError,
			mustContain: "task-a",
		},
		{
			name:     "tool.approval_requested",
			identity: triple("b"),
			trigger: events.Event{
				Type:     approval.EventTypeToolApprovalRequested,
				Identity: triple("b"),
				Payload: approval.ToolApprovalRequestedPayload{
					Tool:       "fs.delete",
					PauseToken: "pt-b",
					Reason:     "destructive",
				},
			},
			wantClass:   notifications.EventTypeNotificationToolApprovalRequested,
			wantSev:     notifications.SeverityWarning,
			mustContain: "fs.delete",
		},
		{
			name:     "governance.budget_exceeded",
			identity: triple("c"),
			trigger: events.Event{
				Type:     governance.EventTypeBudgetExceeded,
				Identity: triple("c"),
				Payload: governance.BudgetExceededPayload{
					Identity:  triple("c"),
					Tier:      "pro",
					Model:     "claude-sonnet",
					TotalCost: 9.99,
					Ceiling:   5.00,
					Currency:  "USD",
				},
			},
			wantClass:   notifications.EventTypeNotificationGovernanceBudgetExceeded,
			wantSev:     notifications.SeverityError,
			mustContain: "pro",
		},
		{
			name:     "tool.auth_required",
			identity: triple("d"),
			trigger: events.Event{
				Type:     auth.EventTypeToolAuthRequired,
				Identity: triple("d"),
				Payload: auth.ToolAuthRequiredPayload{
					Source:       "src-d",
					SourceName:   "DriveD",
					BindingScope: "user",
					State:        "csrf-d",
				},
			},
			wantClass:   notifications.EventTypeNotificationAuthRequired,
			wantSev:     notifications.SeverityWarning,
			mustContain: "DriveD",
		},
		{
			name:     "pause.requested",
			identity: triple("e"),
			trigger: events.Event{
				Type:     pauseresume.EventTypePauseRequested,
				Identity: triple("e"),
				Payload: pauseresume.PauseRequestedPayload{
					Token:  "pt-e",
					Reason: "approval_required",
				},
			},
			wantClass:   notifications.EventTypeNotificationPauseRequested,
			wantSev:     notifications.SeverityInfo,
			mustContain: "approval_required",
		},
	}

	for _, tc := range cases {
		if err := bus.Publish(context.Background(), tc.trigger); err != nil {
			t.Fatalf("Publish(%s): %v", tc.name, err)
		}
	}

	// Collect five notifications keyed by class. Bounded wait per
	// CLAUDE.md §17.4 — no time.Sleep as a synchronisation primitive.
	got := make(map[events.EventType]events.Event, len(cases))
	deadline := time.After(5 * time.Second)
	for len(got) < len(cases) {
		select {
		case ev, ok := <-listener.Events():
			if !ok {
				t.Fatalf("listener channel closed early; got=%d/%d", len(got), len(cases))
			}
			got[ev.Type] = ev
		case <-deadline:
			t.Fatalf("deadline before all notifications arrived (got %d/%d): %v",
				len(got), len(cases), keysOf(got))
		}
	}

	for _, tc := range cases {
		ev, ok := got[tc.wantClass]
		if !ok {
			t.Errorf("[%s] no notification of class %q arrived", tc.name, tc.wantClass)
			continue
		}
		// Identity propagation across the seam (no mocks at the
		// boundary, per §17.3).
		if ev.Identity != tc.identity {
			t.Errorf("[%s] identity bled: got %v, want %v", tc.name, ev.Identity, tc.identity)
		}
		// Payload is RedactedMap (NotificationPayload is non-SafeSealed)
		// — assert on the redacted shape.
		rm, ok := ev.Payload.(events.RedactedMap)
		if !ok {
			t.Errorf("[%s] payload type=%T, want RedactedMap", tc.name, ev.Payload)
			continue
		}
		if sev, _ := rm.Data["severity"].(notifications.Severity); sev != tc.wantSev {
			t.Errorf("[%s] severity=%v, want %v", tc.name, sev, tc.wantSev)
		}
		if summary, _ := rm.Data["summary"].(string); !contains(summary, tc.mustContain) {
			t.Errorf("[%s] summary=%q must contain %q (full data=%v)",
				tc.name, summary, tc.mustContain, rm.Data)
		}
		if oet, _ := rm.Data["origineventtype"].(events.EventType); oet != tc.trigger.Type {
			t.Errorf("[%s] origineventtype=%v, want %v", tc.name, oet, tc.trigger.Type)
		}
	}
}

// TestE2E_NotificationsTopic_MissingIdentityFailsLoudly proves the
// fail-loudly contract on the Subscriber's identity-rejection path:
// when a trigger event arrives with the D-033 `<missing>` sentinel
// substituted into any identity component, the Subscriber emits a
// notification.identity_rejected event and does NOT publish a
// malformed notification.<class>.
//
// This is the ≥1 failure-mode requirement (§17.3) — boundary-level
// proof that the seam fails loud rather than degrading silently.
func TestE2E_NotificationsTopic_MissingIdentityFailsLoudly(t *testing.T) {
	t.Parallel()

	bus, _ := openNotificationsBus(t)

	// Listen for BOTH the rejection event AND any notification.task_failed
	// that might be (erroneously) synthesised.
	listener, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{
			notifications.EventTypeNotificationIdentityRejected,
			notifications.EventTypeNotificationTaskFailed,
		},
	})
	if err != nil {
		t.Fatalf("Subscribe listener: %v", err)
	}
	defer listener.Cancel()

	startSubscriber(t, bus)

	// Publish a task.failed event whose Identity carries the `<missing>`
	// sentinel in tenant_id. The bus's ValidateEvent accepts this
	// (the sentinel is non-empty) but the Subscriber rejects it loud.
	trigger := events.Event{
		Type: tasks.EventTypeTaskFailed,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  "<missing>",
				UserID:    "u-x",
				SessionID: "s-x",
			},
		},
		Payload: tasks.TaskFailedPayload{TaskID: "task-x", ErrorCode: "ec-x"},
	}
	if err := bus.Publish(context.Background(), trigger); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var rejected, notified bool
	for !rejected {
		select {
		case ev, ok := <-listener.Events():
			if !ok {
				t.Fatal("listener closed early")
			}
			switch ev.Type {
			case notifications.EventTypeNotificationIdentityRejected:
				rejected = true
				// IdentityRejectedPayload is SafePayload, so the typed
				// shape survives the bus.
				payload, ok := ev.Payload.(notifications.IdentityRejectedPayload)
				if !ok {
					t.Errorf("rejection payload type=%T, want IdentityRejectedPayload", ev.Payload)
				} else if payload.OriginEventType != tasks.EventTypeTaskFailed {
					t.Errorf("rejection OriginEventType=%q, want %q",
						payload.OriginEventType, tasks.EventTypeTaskFailed)
				}
			case notifications.EventTypeNotificationTaskFailed:
				notified = true
			}
		case <-deadline:
			t.Fatalf("deadline before rejection (rejected=%v notified=%v)", rejected, notified)
		}
	}
	if notified {
		t.Error("Subscriber synthesised a notification.task_failed for a malformed-identity trigger; expected identity-rejection only (fail-loudly per CLAUDE.md §13)")
	}
}

// TestE2E_NotificationsTopic_ConcurrencyStress is the §17.3 long-lived-
// wiring requirement: N=20 concurrent producers each fire a mix of
// trigger event types against a single shared bus + Subscriber, all
// under -race. Asserts:
//
//   - Every trigger produces exactly one notification (no drops, no
//     duplicates).
//   - Identity propagation holds under contention (no cross-talk
//     between concurrent triggers).
//   - Baseline goroutine count is restored after teardown (no leaks
//     from the long-lived Subscriber's bus subscription).
func TestE2E_NotificationsTopic_ConcurrencyStress(t *testing.T) {
	t.Parallel()

	baseline := stableGoroutines(t)

	bus, _ := openNotificationsBus(t)

	const N = 20
	const triggersPerProducer = 5

	listener, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: notifications.V1NotificationClasses(),
	})
	if err != nil {
		t.Fatalf("Subscribe listener: %v", err)
	}
	defer listener.Cancel()

	startSubscriber(t, bus)

	// Generate distinct identities + correlate them with their
	// notifications so we can assert no cross-talk.
	producerIdentities := make([]identity.Quadruple, N)
	for i := 0; i < N; i++ {
		producerIdentities[i] = identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("t-stress-%d", i),
				UserID:    fmt.Sprintf("u-stress-%d", i),
				SessionID: fmt.Sprintf("s-stress-%d", i),
			},
			RunID: fmt.Sprintf("r-stress-%d", i),
		}
	}

	// Counters per identity track expected vs. received notifications.
	var (
		wgPub        sync.WaitGroup
		publishedSum int64
	)
	wgPub.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wgPub.Done()
			id := producerIdentities[i]
			for j := 0; j < triggersPerProducer; j++ {
				// Rotate over the five V1 trigger types so the bus
				// sees a balanced mix.
				ev := buildTrigger(id, j, i)
				if err := bus.Publish(context.Background(), ev); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
				atomic.AddInt64(&publishedSum, 1)
			}
		}()
	}
	wgPub.Wait()
	expected := int(publishedSum)

	// Drain all notifications and bucket by identity.
	received := make(map[string]int, N)
	deadline := time.After(10 * time.Second)
	for total := 0; total < expected; {
		select {
		case ev, ok := <-listener.Events():
			if !ok {
				t.Fatalf("listener channel closed before all notifications arrived (got %d/%d)", total, expected)
			}
			key := ev.Identity.TenantID
			received[key]++
			total++
		case <-deadline:
			t.Fatalf("deadline before all notifications arrived (got %d/%d): %v",
				len(received), expected, received)
		}
	}
	// Per-identity assertion: every producer's triggers map 1:1 to
	// notifications carrying that producer's identity. Cross-talk
	// would show up as a non-equal count.
	for i := 0; i < N; i++ {
		want := triggersPerProducer
		got := received[producerIdentities[i].TenantID]
		if got != want {
			t.Errorf("producer %d (tenant=%s): got %d notifications, want %d",
				i, producerIdentities[i].TenantID, got, want)
		}
	}

	// Cancel subscriptions and prove goroutine baseline restored.
	listener.Cancel()
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("bus.Close: %v", err)
	}
	if got := eventualGoroutineBaseline(t, baseline); got > baseline {
		t.Errorf("goroutine leak: baseline=%d post=%d", baseline, got)
	}
}

// --- helpers (kept local — distinct from the wave2/wave3 helpers so
//      this file doesn't grow cross-test coupling) ---

// openNotificationsBus opens a fresh in-memory bus with the production
// audit redactor. Returns both the bus and the underlying audit redactor
// so callers can sanity-check on the §17.3 "real drivers everywhere"
// rule if needed.
func openNotificationsBus(t *testing.T) (events.EventBus, *auditpatterns.Driver) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus, red
}

// startSubscriber launches notifications.Subscriber and waits until its
// bus.Subscribe has been observed via an AdminScopeUsed sibling. Returns
// once the Subscriber is wired and ready to receive triggers — no time-
// based synchronisation.
func startSubscriber(t *testing.T, bus events.EventBus) {
	t.Helper()
	probe, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("probe Subscribe: %v", err)
	}
	// Drain probe's own AdminScopeUsed.
	waitFirstEvent(t, probe, 2*time.Second)

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := notifications.NewSubscriber(bus, discardLog())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Subscriber.Run did not return within 3s of ctx cancel")
		}
	})
	// Wait for Subscriber's Subscribe — observable as the next
	// AdminScopeUsed event on the probe.
	waitFirstEvent(t, probe, 2*time.Second)
	probe.Cancel()
}

func waitFirstEvent(t *testing.T, sub events.Subscription, timeout time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before delivery")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("waitFirstEvent timed out after %v", timeout)
		return events.Event{}
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func triple(suffix string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-" + suffix,
			UserID:    "u-" + suffix,
			SessionID: "s-" + suffix,
		},
		RunID: "r-" + suffix,
	}
}

func buildTrigger(id identity.Quadruple, j, i int) events.Event {
	// Rotate over the five V1 triggers for balanced coverage.
	switch (j + i) % 5 {
	case 0:
		return events.Event{
			Type:     tasks.EventTypeTaskFailed,
			Identity: id,
			Payload: tasks.TaskFailedPayload{
				TaskID:    "task-stress",
				ErrorCode: "ec-stress",
			},
		}
	case 1:
		return events.Event{
			Type:     approval.EventTypeToolApprovalRequested,
			Identity: id,
			Payload: approval.ToolApprovalRequestedPayload{
				Tool:       "stress.tool",
				PauseToken: "pt-stress",
				Reason:     "stress",
			},
		}
	case 2:
		return events.Event{
			Type:     governance.EventTypeBudgetExceeded,
			Identity: id,
			Payload: governance.BudgetExceededPayload{
				Identity:  id,
				Tier:      "tier-stress",
				Model:     "model-stress",
				TotalCost: 1.0,
				Ceiling:   0.5,
				Currency:  "USD",
			},
		}
	case 3:
		return events.Event{
			Type:     auth.EventTypeToolAuthRequired,
			Identity: id,
			Payload: auth.ToolAuthRequiredPayload{
				Source:       "stress-src",
				SourceName:   "StressSrc",
				BindingScope: "user",
				State:        "csrf-stress",
			},
		}
	default:
		return events.Event{
			Type:     pauseresume.EventTypePauseRequested,
			Identity: id,
			Payload: pauseresume.PauseRequestedPayload{
				Token:  "pt-stress",
				Reason: "external_event",
			},
		}
	}
}

func keysOf(m map[events.EventType]events.Event) []events.EventType {
	out := make([]events.EventType, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	// stdlib `strings.Contains` would be the obvious choice; we use a
	// small inline helper here to avoid adding a single-use import in
	// this test file. The shape mirrors strings.Index for clarity.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func stableGoroutines(t *testing.T) int {
	t.Helper()
	for i := 0; i < 16; i++ {
		a := runtime.NumGoroutine()
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
		b := runtime.NumGoroutine()
		if a == b {
			return a
		}
	}
	return runtime.NumGoroutine()
}

func eventualGoroutineBaseline(t *testing.T, baseline int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := runtime.NumGoroutine()
		if got <= baseline {
			return got
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}
