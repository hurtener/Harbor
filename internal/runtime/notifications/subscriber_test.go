package notifications_test

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed is the
// §13 Stage-1 BINDING test consumer per
// docs/plans/wave-13-decomposition.md §12 item 5 + D-109.
//
// Wire shape:
//
//   1. Boot a fresh in-mem EventBus + audit redactor.
//   2. Register a separately-scoped subscriber on
//      notification.task_failed (Admin scope so it sees the
//      synthesised event regardless of which identity the trigger
//      carried).
//   3. Launch a notifications.Subscriber.Run goroutine. This is the
//      §13 primitive-with-consumer wiring — the runtime-internal
//      mapper + subscriber pair that proves the notification.* topic
//      is alive without depending on the Stage-2 Console UI.
//   4. Publish a deliberate task.failed event with a well-formed
//      TaskFailedPayload + identity.
//   5. Assert: the synthesised notification.task_failed arrives at the
//      step-2 subscriber within a bounded wait, with the originating
//      event's identity preserved, the correct class + severity, and a
//      correlation back to the trigger (OriginEventType, OriginEventSequence).
//
// This test is NOT deferred to 73a Overview. It lands in this PR per
// the operator amendment in §12 item 5 — a Stage-2 UI consumer cannot
// substitute for a Stage-1 runtime test consumer.
func TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	bus := newBus(t)

	// Step 2a — admin-scope probe so we can observe when the
	// Subscriber's own Subscribe has registered (the bus emits
	// audit.admin_scope_used on every Admin-true Subscribe; receiving
	// that event from the Subscriber proves its subscription is live,
	// avoiding a race between Run starting and the test publishing
	// the trigger).
	adminProbe, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("Subscribe (admin probe): %v", err)
	}
	defer adminProbe.Cancel()

	// Step 2b — separately-scoped subscriber filtering for the
	// notification class. Admin scope so it sees the synthesised event
	// regardless of which identity the trigger carries. Subscribe
	// BEFORE the Subscriber goroutine so admin-scope ordering is
	// deterministic on adminProbe.
	notifSub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{notifications.EventTypeNotificationTaskFailed},
	})
	if err != nil {
		t.Fatalf("Subscribe (notification): %v", err)
	}
	defer notifSub.Cancel()
	// Drain the two AdminScopeUsed events seen so far (adminProbe's own
	// + notifSub's). Each Admin-true Subscribe causes the bus to emit
	// one AdminScopeUsed sibling event; adminProbe's filter includes
	// the type so it receives all three (its own + notifSub's + the
	// notifications.Subscriber's, drained one-by-one below).
	waitEvent(t, adminProbe, 2*time.Second)
	waitEvent(t, adminProbe, 2*time.Second)

	// Step 3 — launch the notifications.Subscriber. Run blocks until
	// runCtx is cancelled OR the bus closes the channel; the test
	// cancels at the end via t.Cleanup.
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	s := notifications.NewSubscriber(bus, discardLogger())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = s.Run(runCtx)
	}()

	// Wait for the Subscriber's own Subscribe to fire — observable as
	// the third AdminScopeUsed event on adminProbe. This eliminates the
	// race between Run starting and the test publishing the trigger.
	waitEvent(t, adminProbe, 2*time.Second)

	// Step 4 — fire the deliberate task.failed. Use a fresh identity
	// to prove the synthesised notification preserves it.
	triggerIdentity := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-bind-1",
			UserID:    "u-bind-1",
			SessionID: "s-bind-1",
		},
		RunID: "r-bind-1",
	}
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskFailed,
		Identity: triggerIdentity,
		Payload: tasks.TaskFailedPayload{
			TaskID:    "task-binding",
			ErrorCode: "binding_test",
		},
	}); err != nil {
		t.Fatalf("Publish task.failed: %v", err)
	}

	// Step 5 — wait for the notification with a bounded receive (no
	// time.Sleep, per CLAUDE.md §17.4).
	notif := waitEvent(t, notifSub, 5*time.Second)
	if notif.Type != notifications.EventTypeNotificationTaskFailed {
		t.Fatalf("notification type=%q, want %q", notif.Type, notifications.EventTypeNotificationTaskFailed)
	}
	if notif.Identity != triggerIdentity {
		t.Errorf("identity bled: got %v, want %v", notif.Identity, triggerIdentity)
	}
	// NotificationPayload embeds events.Sealed (NOT SafeSealed) per
	// the plan's acceptance criteria — Summary is caller-controlled
	// and the bus walks the payload through the audit redactor. The
	// subscriber therefore receives a RedactedMap shape. Assert on
	// the redacted fields rather than the typed shape.
	rm, ok := notif.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("payload type=%T, want RedactedMap (NotificationPayload is non-SafeSealed and walks the redactor)", notif.Payload)
	}
	// fieldName in the audit redactor uses strings.ToLower on the Go
	// field name when there's no yaml/json tag; so "Severity" becomes
	// "severity", "OriginEventType" becomes "origineventtype", etc.
	// The reflective walk preserves the typed values — events.EventType
	// stays events.EventType, Severity stays Severity, uint64 stays
	// uint64. Type assertions therefore use the typed shape.
	if got, _ := rm.Data["severity"].(notifications.Severity); got != notifications.SeverityError {
		t.Errorf("severity=%v, want %v (full data=%v)", got, notifications.SeverityError, rm.Data)
	}
	if got, _ := rm.Data["origineventtype"].(events.EventType); got != tasks.EventTypeTaskFailed {
		t.Errorf("origineventtype=%v, want %v (full data=%v)", got, tasks.EventTypeTaskFailed, rm.Data)
	}
	seq, _ := rm.Data["origineventsequence"].(uint64)
	if seq == 0 {
		t.Errorf("origineventsequence=0 (bus should have assigned a non-zero sequence to the trigger); data=%v", rm.Data)
	}
	if got, _ := rm.Data["class"].(events.EventType); got != notifications.EventTypeNotificationTaskFailed {
		t.Errorf("class=%v, want %v (full data=%v)", got, notifications.EventTypeNotificationTaskFailed, rm.Data)
	}
	if summary, _ := rm.Data["summary"].(string); summary == "" {
		t.Errorf("summary missing or non-string: %v", rm.Data["summary"])
	}

	// Cancel + join the Subscriber's Run goroutine before the test
	// ends so the leak test that follows starts from a clean baseline.
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Subscriber.Run did not return within 2s of ctx cancel")
	}
}

// TestSubscriber_Run_GoroutineLeak verifies the goroutine-leak
// contract on the Subscriber's long-lived component (CLAUDE.md §11).
// Start a Subscriber, cancel the ctx, assert Run returns within a
// bounded wait, then assert runtime.NumGoroutine has returned to
// baseline.
func TestSubscriber_Run_GoroutineLeak(t *testing.T) {
	t.Parallel()
	bus := newBus(t)
	baseline := stableNumGoroutine(t)

	// Probe BEFORE launching the Subscriber so we can deterministically
	// observe the Subscriber's Subscribe via AdminScopeUsed.
	probeSub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("probe Subscribe: %v", err)
	}
	// Drain the AdminScopeUsed our own probe Subscribe emitted (the
	// probe sees its own admin sibling because it was the only
	// matching subscriber at emit time).
	waitEvent(t, probeSub, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	s := notifications.NewSubscriber(bus, discardLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()

	// Wait for the Subscriber's Subscribe to fire — observable as the
	// next AdminScopeUsed event on the probe.
	waitEvent(t, probeSub, 2*time.Second)
	probeSub.Cancel()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of ctx cancel")
	}

	if got := eventualBaseline(t, baseline); got > baseline {
		t.Errorf("goroutine leak: baseline=%d post=%d", baseline, got)
	}
}

// TestNewSubscriber_RejectsNilBus and RejectsNilLog cover the
// constructor's fail-loudly guarantees (CLAUDE.md §13 — no silent
// degradation; a nil bus or logger would silently turn the subscriber
// into a no-op).
func TestNewSubscriber_RejectsNilBus(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewSubscriber(nil, log) did not panic")
		}
	}()
	notifications.NewSubscriber(nil, discardLogger())
}

func TestNewSubscriber_RejectsNilLog(t *testing.T) {
	t.Parallel()
	bus := newBus(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewSubscriber(bus, nil) did not panic")
		}
	}()
	notifications.NewSubscriber(bus, nil)
}

// --- helpers ---

// newBus opens a fresh in-memory EventBus with a production audit
// redactor (the patterns driver — same as memory_state_test.go uses)
// so the §17.3 "real drivers everywhere on the seam" rule is honoured.
func newBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// testCtx builds an identity-scoped context for tests. Mirrors the
// wave2 helper but local so this package's tests don't take a
// dependency on integration-test helpers.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "t-test", UserID: "u-test", SessionID: "s-test"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "r-test")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// waitEvent receives the next event from sub with a bounded timeout.
// Mirrors the pauseresume test helper — bounded receive instead of a
// time.Sleep, per CLAUDE.md §17.4.
func waitEvent(t *testing.T, sub events.Subscription, timeout time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before an event arrived")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for an event", timeout)
		return events.Event{}
	}
}

// discardLogger builds a no-op slog.Logger. Used in tests where we
// don't want log output competing with test output but do need to
// exercise the production code path (which logs at Info / Warn /
// Error).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// _ = runtime.NumGoroutine is referenced indirectly via the helpers
// shared with mapper_test.go (stableNumGoroutine, eventualBaseline).
// The blank reference keeps imports honest if a future refactor moves
// those helpers.
var _ = runtime.NumGoroutine
