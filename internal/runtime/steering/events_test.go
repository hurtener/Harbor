package steering

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

func TestEventTypeControlRejected_Registered(t *testing.T) {
	if !events.IsValidEventType(EventTypeControlRejected) {
		t.Errorf("EventTypeControlRejected %q is not registered in the events registry", EventTypeControlRejected)
	}
	if EventTypeControlRejected != "control.rejected" {
		t.Errorf("EventTypeControlRejected = %q, want %q", EventTypeControlRejected, "control.rejected")
	}
}

func TestControlRejectedPayload_IsSafePayload(t *testing.T) {
	// Compile-time assertion: the payload satisfies both EventPayload
	// and SafePayload (the bus skips the redactor for it).
	var _ events.EventPayload = ControlRejectedPayload{}
	var _ events.SafePayload = ControlRejectedPayload{}
}

func TestControlLifecycleEventTypes_Registered(t *testing.T) {
	// Phase 53 adds control.received + control.applied — the run-loop
	// lifecycle events brief 02 §3 names.
	for _, et := range []events.EventType{EventTypeControlReceived, EventTypeControlApplied} {
		if !events.IsValidEventType(et) {
			t.Errorf("event type %q is not registered in the events registry", et)
		}
	}
	if EventTypeControlReceived != "control.received" {
		t.Errorf("EventTypeControlReceived = %q, want %q", EventTypeControlReceived, "control.received")
	}
	if EventTypeControlApplied != "control.applied" {
		t.Errorf("EventTypeControlApplied = %q, want %q", EventTypeControlApplied, "control.applied")
	}
}

func TestControlLifecyclePayload_IsSafePayload(t *testing.T) {
	var _ events.EventPayload = ControlLifecyclePayload{}
	var _ events.SafePayload = ControlLifecyclePayload{}
}

func TestClassifyRejection(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrUnknownControlType, reasonUnknownType},
		{ErrScopeMismatch, reasonScopeMismatch},
		{ErrInvalidScope, reasonScopeMismatch},
		{ErrIdentityRequired, reasonIdentityInvalid},
		{ErrPayloadInvalid, reasonPayloadInvalid},
		{ErrUnsupportedPayloadValue, reasonPayloadInvalid},
		{errors.New("some other failure"), reasonPayloadInvalid},
	}
	for _, c := range cases {
		if got := classifyRejection(c.err); got != c.want {
			t.Errorf("classifyRejection(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// fakeBus is a minimal in-test EventBus used only by the nil-arg /
// publish-failure unit cases below. The auth-scope-per-event
// integration test uses the REAL inmem bus (test/integration).
type fakeBus struct {
	failWith  error
	published []events.Event
}

func (b *fakeBus) Publish(_ context.Context, ev events.Event) error {
	if b.failWith != nil {
		return b.failWith
	}
	b.published = append(b.published, ev)
	return nil
}
func (b *fakeBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("not implemented")
}
func (b *fakeBus) Close(context.Context) error { return nil }

func TestEmitRejection_NilArgsFailLoud(t *testing.T) {
	if err := EmitRejection(context.Background(), nil, scopeTestRun, ControlCancel, ScopeOwnerUser, ErrScopeMismatch); err == nil {
		t.Error("EmitRejection(nil bus) = nil, want error")
	}
	bus := &fakeBus{}
	if err := EmitRejection(context.Background(), bus, scopeTestRun, ControlCancel, ScopeOwnerUser, nil); err == nil {
		t.Error("EmitRejection(nil rejectErr) = nil, want error")
	}
}

func TestEmitRejection_PublishFailurePropagates(t *testing.T) {
	bus := &fakeBus{failWith: errors.New("bus down")}
	err := EmitRejection(context.Background(), bus, scopeTestRun, ControlCancel, ScopeOwnerUser, ErrScopeMismatch)
	if err == nil {
		t.Fatal("EmitRejection with a failing bus = nil, want the publish error wrapped")
	}
}

func TestEmitRejection_HappyPath_PayloadShape(t *testing.T) {
	bus := &fakeBus{}
	err := EmitRejection(context.Background(), bus, scopeTestRun, ControlPrioritize, ScopeOwnerUser, ErrScopeMismatch)
	if err != nil {
		t.Fatalf("EmitRejection: %v", err)
	}
	if len(bus.published) != 1 {
		t.Fatalf("published %d events, want 1", len(bus.published))
	}
	ev := bus.published[0]
	if ev.Type != EventTypeControlRejected {
		t.Errorf("event Type = %q, want %q", ev.Type, EventTypeControlRejected)
	}
	if ev.Identity != (identity.Quadruple{}) && ev.Identity.RunID != scopeTestRun.RunID {
		t.Errorf("event Identity RunID = %q, want %q", ev.Identity.RunID, scopeTestRun.RunID)
	}
	p, ok := ev.Payload.(ControlRejectedPayload)
	if !ok {
		t.Fatalf("event Payload type = %T, want ControlRejectedPayload", ev.Payload)
	}
	if p.Type != string(ControlPrioritize) {
		t.Errorf("payload Type = %q, want %q", p.Type, ControlPrioritize)
	}
	if p.Reason != reasonScopeMismatch {
		t.Errorf("payload Reason = %q, want %q", p.Reason, reasonScopeMismatch)
	}
	if p.CallerScope != string(ScopeOwnerUser) {
		t.Errorf("payload CallerScope = %q, want %q", p.CallerScope, ScopeOwnerUser)
	}
}
