package steering

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// fakeClock is a controllable Clock so tests never sleep for
// synchronisation (CLAUDE.md §11).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// runA / runB are documented dummy run quadruples — no secrets.
var (
	runA = identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"},
		RunID:    "run-a",
	}
	runB = identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "session-b"},
		RunID:    "run-b",
	}
)

// validEvent builds a ControlEvent that passes Enqueue for the given
// run: a CANCEL (owner_user min) submitted by an owner_user from the
// run's own tenant.
func validEvent(q identity.Quadruple) ControlEvent {
	return ControlEvent{
		Type:         ControlCancel,
		Identity:     q,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"mode": "soft"},
	}
}

func newInbox(t *testing.T, q identity.Quadruple, clk Clock) *Inbox {
	t.Helper()
	reg := NewRegistry(WithClock(clk))
	in, err := reg.Open(q)
	if err != nil {
		t.Fatalf("Registry.Open(%+v): %v", q, err)
	}
	return in
}

func TestInbox_EnqueueDrain_FIFO(t *testing.T) {
	clk := newFakeClock()
	in := newInbox(t, runA, clk)

	for i := 0; i < 3; i++ {
		ev := validEvent(runA)
		ev.EventID = string(rune('0' + i))
		if err := in.Enqueue(ev); err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
		clk.advance(time.Second)
	}
	if in.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", in.Len())
	}

	drained, err := in.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 3 {
		t.Fatalf("Drain returned %d events, want 3", len(drained))
	}
	for i, ev := range drained {
		if ev.EventID != string(rune('0'+i)) {
			t.Errorf("Drain order broken at %d: EventID=%q", i, ev.EventID)
		}
		if ev.EnqueuedAt.IsZero() {
			t.Errorf("event %d EnqueuedAt not stamped", i)
		}
	}
	// Drain emptied the inbox.
	if in.Len() != 0 {
		t.Errorf("Len() after Drain = %d, want 0", in.Len())
	}
	again, err := in.Drain()
	if err != nil {
		t.Fatalf("Drain (empty): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("Drain on empty inbox returned %d events, want 0", len(again))
	}
	if again == nil {
		t.Error("Drain on empty inbox returned nil, want non-nil empty slice")
	}
}

func TestInbox_Enqueue_StampsFromClock(t *testing.T) {
	clk := newFakeClock()
	in := newInbox(t, runA, clk)
	want := clk.Now()
	if err := in.Enqueue(validEvent(runA)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	drained, _ := in.Drain()
	if !drained[0].EnqueuedAt.Equal(want) {
		t.Errorf("EnqueuedAt = %v, want %v (from injected clock)", drained[0].EnqueuedAt, want)
	}
}

func TestInbox_Enqueue_RejectsPrefilledEnqueuedAt(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	ev := validEvent(runA)
	ev.EnqueuedAt = time.Now()
	err := in.Enqueue(ev)
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Errorf("Enqueue(prefilled EnqueuedAt) = %v, want ErrPayloadInvalid", err)
	}
}

// TestInbox_Enqueue_RejectsForeignRunIdentity is the per-run
// isolation gate: an event for run A must never land on run B's
// inbox (CLAUDE.md §6).
func TestInbox_Enqueue_RejectsForeignRunIdentity(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	ev := validEvent(runB) // event targets run B
	err := in.Enqueue(ev)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("Enqueue(foreign run identity) = %v, want ErrIdentityRequired", err)
	}
	if in.Len() != 0 {
		t.Error("foreign-identity event was enqueued — cross-run bleed")
	}
}

func TestInbox_Enqueue_RejectsIncompleteIdentity(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	cases := []identity.Quadruple{
		{Identity: identity.Identity{UserID: "u", SessionID: "s"}, RunID: "r"}, // no tenant
		{Identity: identity.Identity{TenantID: "t", SessionID: "s"}, RunID: "r"},
		{Identity: identity.Identity{TenantID: "t", UserID: "u"}, RunID: "r"},
		{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, // no run
	}
	for i, q := range cases {
		ev := validEvent(runA)
		ev.Identity = q
		if err := in.Enqueue(ev); !errors.Is(err, ErrIdentityRequired) {
			t.Errorf("case %d: Enqueue(incomplete identity) = %v, want ErrIdentityRequired", i, err)
		}
	}
}

func TestInbox_Enqueue_RejectsUnknownControlType(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	ev := validEvent(runA)
	ev.Type = "STOP"
	if err := in.Enqueue(ev); !errors.Is(err, ErrUnknownControlType) {
		t.Errorf("Enqueue(unknown type) = %v, want ErrUnknownControlType", err)
	}
}

func TestInbox_Enqueue_RejectsScopeMismatch(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	// PRIORITIZE needs admin; submit it as owner_user.
	ev := validEvent(runA)
	ev.Type = ControlPrioritize
	ev.CallerScope = ScopeOwnerUser
	if err := in.Enqueue(ev); !errors.Is(err, ErrScopeMismatch) {
		t.Errorf("Enqueue(PRIORITIZE as owner_user) = %v, want ErrScopeMismatch", err)
	}
	if in.Len() != 0 {
		t.Error("scope-mismatched event was enqueued")
	}
}

func TestInbox_Enqueue_RejectsOversizePayload(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	ev := validEvent(runA)
	ev.Payload = map[string]any{"s": strings.Repeat("x", MaxPayloadStringLen+1)}
	if err := in.Enqueue(ev); !errors.Is(err, ErrPayloadInvalid) {
		t.Errorf("Enqueue(oversize payload) = %v, want ErrPayloadInvalid", err)
	}
	if in.Len() != 0 {
		t.Error("oversize-payload event was enqueued — fail-loud contract violated")
	}
}

func TestInbox_Enqueue_NilPayloadIsValid(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	ev := validEvent(runA)
	ev.Payload = nil
	if err := in.Enqueue(ev); err != nil {
		t.Errorf("Enqueue(nil payload) = %v, want nil (a bare CANCEL carries no payload)", err)
	}
}

func TestInbox_RetiredInbox_FailsClosed(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := in.Enqueue(validEvent(runA)); err != nil {
		t.Fatalf("Enqueue before retire: %v", err)
	}
	if err := reg.Retire(runA); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if err := in.Enqueue(validEvent(runA)); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("Enqueue after retire = %v, want ErrInboxNotFound", err)
	}
	if _, err := in.Drain(); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("Drain after retire = %v, want ErrInboxNotFound", err)
	}
}

func TestInbox_Identity(t *testing.T) {
	in := newInbox(t, runA, newFakeClock())
	if in.Identity() != runA {
		t.Errorf("Identity() = %+v, want %+v", in.Identity(), runA)
	}
}
