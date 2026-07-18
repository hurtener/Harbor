package notifications_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestV1Registration_IncludesWakeClasses pins the two new background-wake
// classes into the V1 class + trigger sets so the Subscriber subscribes to
// them and downstream consumers can filter on them positively.
func TestV1Registration_IncludesWakeClasses(t *testing.T) {
	t.Parallel()
	classes := notifications.V1NotificationClasses()
	wantClasses := []events.EventType{
		notifications.EventTypeNotificationTaskGroupResolved,
		notifications.EventTypeNotificationTaskCompleted,
	}
	for _, want := range wantClasses {
		if !containsType(classes, want) {
			t.Errorf("V1NotificationClasses missing %q", want)
		}
	}
	triggers := notifications.V1TriggerEventTypes()
	for _, want := range []events.EventType{tasks.EventTypeTaskGroupResolved, tasks.EventTypeTaskCompleted} {
		if !containsType(triggers, want) {
			t.Errorf("V1TriggerEventTypes missing %q", want)
		}
	}
}

func containsType(hay []events.EventType, needle events.EventType) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// startWakeSubscriber wires a notifications.Subscriber onto bus and returns
// once its Admin-scope subscription is live (observed via an AdminScopeUsed
// sibling on the admin probe). No time.Sleep synchronisation.
func startWakeSubscriber(t *testing.T, bus events.EventBus, ctx context.Context) {
	t.Helper()
	adminProbe, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("Subscribe admin probe: %v", err)
	}
	// Drain the admin probe's OWN AdminScopeUsed.
	waitEvent(t, adminProbe, 2*time.Second)

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	s := notifications.NewSubscriber(bus, discardLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Subscriber.Run did not return within 2s of ctx cancel")
		}
	})
	// The Subscriber's own Subscribe fires the next AdminScopeUsed.
	waitEvent(t, adminProbe, 2*time.Second)
	adminProbe.Cancel()
}

// TestSubscriber_WakeClassesRoundTripThroughBus drives the two new triggers
// through the real Subscriber and asserts the conversational-mirror classes
// arrive, and that a NotifyOnComplete=false completion produces nothing.
func TestSubscriber_WakeClassesRoundTripThroughBus(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	bus := newBus(t)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{
			notifications.EventTypeNotificationTaskGroupResolved,
			notifications.EventTypeNotificationTaskCompleted,
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	startWakeSubscriber(t, bus, ctx)

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t-w", UserID: "u-w", SessionID: "s-w"}, RunID: "r-w"}

	// Quiet completion FIRST (must produce nothing).
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: id,
		Payload:  tasks.TaskCompletedPayload{TaskID: "t-quiet", NotifyOnComplete: false},
	}); err != nil {
		t.Fatalf("Publish quiet: %v", err)
	}
	// Group resolution.
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: id,
		Payload: tasks.TaskGroupResolvedPayload{Completion: tasks.GroupCompletion{
			GroupID:     "g-w",
			FinalStatus: tasks.GroupCompleted,
			Members: []tasks.MemberOutcome{
				{TaskID: "m-1", Status: tasks.StatusComplete, Description: "one"},
				{TaskID: "m-2", Status: tasks.StatusFailed, Description: "two"},
			},
		}},
	}); err != nil {
		t.Fatalf("Publish resolved: %v", err)
	}
	// Loud completion.
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: id,
		Payload:  tasks.TaskCompletedPayload{TaskID: "t-loud", NotifyOnComplete: true},
	}); err != nil {
		t.Fatalf("Publish loud: %v", err)
	}

	got := map[events.EventType]bool{}
	for len(got) < 2 {
		ev := waitEvent(t, sub, 5*time.Second)
		got[ev.Type] = true
		if ev.Identity != id {
			t.Errorf("identity bled on %q: got %v, want %v", ev.Type, ev.Identity, id)
		}
	}
	if !got[notifications.EventTypeNotificationTaskGroupResolved] {
		t.Error("no notification.task_group_resolved arrived")
	}
	if !got[notifications.EventTypeNotificationTaskCompleted] {
		t.Error("no notification.task_completed arrived for the NotifyOnComplete=true task")
	}
	// Bounded negative: nothing extra (the quiet completion produced none).
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected extra notification %q — NotifyOnComplete=false must produce none", ev.Type)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSubscriber_WakeTrigger_MissingIdentityFailsLoudly proves the
// fail-loud path on a wake trigger: a task.completed whose identity carries
// the `<missing>` sentinel emits notification.identity_rejected and NOT a
// malformed notification.task_completed (CLAUDE.md §13).
func TestSubscriber_WakeTrigger_MissingIdentityFailsLoudly(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	bus := newBus(t)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{
			notifications.EventTypeNotificationIdentityRejected,
			notifications.EventTypeNotificationTaskCompleted,
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	startWakeSubscriber(t, bus, ctx)

	if err := bus.Publish(ctx, events.Event{
		Type: tasks.EventTypeTaskCompleted,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: "<missing>", UserID: "u-x", SessionID: "s-x",
		}},
		Payload: tasks.TaskCompletedPayload{TaskID: "t-x", NotifyOnComplete: true},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ev := waitEvent(t, sub, 5*time.Second)
	if ev.Type != notifications.EventTypeNotificationIdentityRejected {
		t.Fatalf("type=%q, want notification.identity_rejected", ev.Type)
	}
	if p, ok := ev.Payload.(notifications.IdentityRejectedPayload); ok {
		if p.OriginEventType != tasks.EventTypeTaskCompleted {
			t.Errorf("OriginEventType=%q, want %q", p.OriginEventType, tasks.EventTypeTaskCompleted)
		}
	} else {
		t.Errorf("payload type=%T, want IdentityRejectedPayload", ev.Payload)
	}
}

// TestSubscriber_WakeTrigger_MalformedPayloadEmitsRuntimeError proves the
// mapper-error fail-loud path: a known trigger type delivering the wrong
// payload shape (a RedactedMap where the typed payload is expected) makes
// Map return ErrUnmappable, and the Subscriber emits a runtime.error rather
// than silently dropping the trigger.
func TestSubscriber_WakeTrigger_MalformedPayloadEmitsRuntimeError(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	bus := newBus(t)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{events.EventTypeRuntimeError},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	startWakeSubscriber(t, bus, ctx)

	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}},
		Payload:  events.RedactedMap{Data: map[string]any{"completion": "not-a-struct"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ev := waitEvent(t, sub, 5*time.Second)
	if ev.Type != events.EventTypeRuntimeError {
		t.Fatalf("type=%q, want runtime.error", ev.Type)
	}
}
