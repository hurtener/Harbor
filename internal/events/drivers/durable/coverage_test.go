package durable_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
)

// fixedClock is a deterministic Clock for the WithClock seam.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func TestDurable_WithClock_StampsOccurredAt(t *testing.T) {
	store := newInmemStore(t)
	fixed := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	bus, err := durable.New(durableCfg(), auditpatterns.New(), store,
		durable.WithClock(fixedClock{t: fixed}))
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := quad("t1", "u1", "s1")
	if err := bus.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("x"),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	rp := bus.(events.Replayer)
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if !got[0].OccurredAt.Equal(fixed) {
		t.Fatalf("expected OccurredAt %v, got %v", fixed, got[0].OccurredAt)
	}
}

// ---------------------------------------------------------------------------
// Admin-scope path: replay + subscribe emit audit.admin_scope_used
// ---------------------------------------------------------------------------

func TestDurable_AdminSubscribe_EmitsAdminScopeUsed(t *testing.T) {
	store := newInmemStore(t)
	bus, _ := newDurableBus(t, store)

	// An admin subscriber sees the audit.admin_scope_used event the
	// bus emits for ITS OWN admin Subscribe.
	adminSub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("admin Subscribe: %v", err)
	}
	defer adminSub.Cancel()

	select {
	case ev := <-adminSub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("expected admin_scope_used, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for admin_scope_used event")
	}
}

func TestDurable_AdminReplay_AcrossSessions(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")
	publishN(t, bus, id, 3)

	// Admin replay still requires the full triple to resolve the
	// session-keyed storage record, but bypasses the identity match.
	got, err := rp.Replay(context.Background(),
		events.Cursor{SessionID: "s1"},
		events.Filter{Admin: true, Tenant: "t1", User: "u1", Session: "s1"})
	if err != nil {
		t.Fatalf("admin Replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("admin replay: expected 3 events, got %d", len(got))
	}
}

func TestDurable_AdminReplay_WithoutSession_Rejected(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	_ = bus
	_, err := rp.Replay(context.Background(), events.Cursor{}, events.Filter{Admin: true})
	if !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("expected ErrIdentityScopeRequired for sessionless admin replay, got %v", err)
	}
}

func TestDurable_AdminReplay_PartialTriple_Rejected(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	_ = bus
	// Admin filter with only a session — cannot resolve the
	// triple-keyed head record.
	_, err := rp.Replay(context.Background(),
		events.Cursor{SessionID: "s1"},
		events.Filter{Admin: true, Session: "s1"})
	if !errors.Is(err, events.ErrIdentityScopeRequired) {
		t.Fatalf("expected ErrIdentityScopeRequired for partial-triple admin replay, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Subscriber saturation: drop-oldest + bus.dropped notice
// ---------------------------------------------------------------------------

func TestDurable_Subscriber_DropOldest_EmitsBusDropped(t *testing.T) {
	store := newInmemStore(t)
	cfg := durableCfg()
	cfg.SubscriberBufferSize = 4 // tiny buffer to force saturation
	cfg.DropWindow = time.Nanosecond
	bus, err := durable.New(cfg, auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := quad("t1", "u1", "s1")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Publish far more than the buffer holds without draining — the
	// subscription drops oldest and accumulates a drop window.
	publishN(t, bus, id, 40)

	// Drain everything available; at least one bus.dropped notice
	// must have been delivered into the stream.
	sawDropped := false
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break drain
			}
			if ev.Type == events.EventTypeBusDropped {
				sawDropped = true
			}
		case <-deadline:
			break drain
		default:
			break drain
		}
	}
	if !sawDropped {
		t.Fatalf("expected at least one bus.dropped notice after saturation")
	}
}

// ---------------------------------------------------------------------------
// Redaction-failure path: emits audit.redaction_failed + returns error
// ---------------------------------------------------------------------------

// failingRedactor always fails — drives the redaction-failure branch.
type failingRedactor struct{ err error }

func (f failingRedactor) Redact(context.Context, any) (any, error) { return nil, f.err }

func TestDurable_RedactionFailure_EmitsSiblingAndReturnsError(t *testing.T) {
	store := newInmemStore(t)
	cfg := durableCfg()
	sentinel := errors.New("redactor refused")
	bus, err := durable.New(cfg, failingRedactor{err: sentinel}, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := quad("t1", "u1", "s1")
	// Subscribe BEFORE the failing publish so the sibling
	// audit.redaction_failed event is observable.
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	err = bus.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("doomed"),
	})
	if err == nil {
		t.Fatalf("expected Publish to return the redaction failure")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeAuditRedactionFailed {
			t.Fatalf("expected audit.redaction_failed sibling, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for audit.redaction_failed sibling")
	}
}

// ---------------------------------------------------------------------------
// SafePayload bypasses the redactor (no RedactedMap wrap before persist)
// ---------------------------------------------------------------------------

func TestDurable_SafePayload_PersistsAndReplays(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := quad("t1", "u1", "s1")

	// RunCancelledPayload is a SafePayload — it bypasses the redactor.
	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Identity: id,
		Payload: events.RunCancelledPayload{
			RunID: "run-1", CancelledAt: 123, DroppedEnvelopeCount: 2,
		},
	}); err != nil {
		t.Fatalf("Publish SafePayload: %v", err)
	}
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	rm, ok := got[0].Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("expected RedactedMap payload on replay, got %T", got[0].Payload)
	}
	if rm.Data["RunID"] != "run-1" {
		t.Fatalf("expected RunID round-tripped, got %v", rm.Data["RunID"])
	}
}

// ---------------------------------------------------------------------------
// Extra map + RunID round-trip through persistence
// ---------------------------------------------------------------------------

func TestDurable_RunIDAndExtra_RoundTrip(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "run-77",
	}
	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  runtimeWarn("x"),
		Extra:    map[string]string{"label": "value"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "s1"},
		events.Filter{Tenant: "t1", User: "u1", Session: "s1"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Identity.RunID != "run-77" {
		t.Fatalf("RunID not round-tripped: got %q", got[0].Identity.RunID)
	}
	if got[0].Extra["label"] != "value" {
		t.Fatalf("Extra not round-tripped: got %v", got[0].Extra)
	}
}

// ---------------------------------------------------------------------------
// Replay of an unknown session returns nil (no head record)
// ---------------------------------------------------------------------------

func TestDurable_Replay_UnknownSession_ReturnsNil(t *testing.T) {
	store := newInmemStore(t)
	bus, rp := newDurableBus(t, store)
	_ = bus
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: "nope"},
		events.Filter{Tenant: "t1", User: "u1", Session: "nope"})
	if err != nil {
		t.Fatalf("Replay unknown session: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for unknown session, got %d events", len(got))
	}
}
