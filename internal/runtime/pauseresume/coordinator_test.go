package pauseresume_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

func TestResume_ReattachesLiveHandle(t *testing.T) {
	t.Parallel()
	reg := trajectory.NewProcessLocalRegistry()
	// A live handle the runtime registered at tool-dispatch time.
	const handleID trajectory.HandleID = "handle-live-1"
	reg.Set(handleID, "a-live-socket")

	c := pauseresume.New(pauseresume.WithHandleRegistry(reg))
	ctx := runCtx(t, testID, "run-1")

	tr := &trajectory.Trajectory{
		Query: "resumable run",
		ToolContext: trajectory.ToolContext{
			Handles: []trajectory.HandleID{handleID},
		},
	}
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity:   testID,
		Reason:     pauseresume.ReasonExternalEvent,
		Trajectory: tr,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// The handle is still live — Resume re-attaches it cleanly.
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume with live handle: %v", err)
	}
}

// TestResume_FailsLoudlyOnLostHandle verifies the second half of the
// fail-loudly contract: a serialised trajectory carrying a HandleID
// whose registry mapping has died surfaces trajectory.ErrToolContextLost
// on Resume — never a silent nil tool context.
func TestResume_FailsLoudlyOnLostHandle(t *testing.T) {
	t.Parallel()
	// A FRESH registry — the handle was never registered here
	// (simulates resume after the owning process restarted).
	reg := trajectory.NewProcessLocalRegistry()
	c := pauseresume.New(pauseresume.WithHandleRegistry(reg))
	ctx := runCtx(t, testID, "run-1")

	tr := &trajectory.Trajectory{
		ToolContext: trajectory.ToolContext{
			Handles: []trajectory.HandleID{"handle-lost-1"},
		},
	}
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity:   testID,
		Reason:     pauseresume.ReasonExternalEvent,
		Trajectory: tr,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	err = c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
	var lost trajectory.ErrToolContextLost
	if !errors.As(err, &lost) {
		t.Fatalf("Resume: err=%v, want trajectory.ErrToolContextLost", err)
	}
	if lost.Handle != "handle-lost-1" {
		t.Fatalf("ErrToolContextLost.Handle = %q, want %q", lost.Handle, "handle-lost-1")
	}

	// The pause is NOT marked resumed — a lost handle leaves the
	// record paused so a later retry (after the handle is re-registered)
	// can still succeed.
	st, serr := c.Status(ctx, p.Token)
	if serr != nil {
		t.Fatalf("Status after failed resume: %v", serr)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q after failed resume, want %q", st.State, pauseresume.StatusPaused)
	}
}

func TestRequest_EmitsPauseRequestedEvent(t *testing.T) {
	t.Parallel()
	bus := newBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: testID.TenantID, User: testID.UserID, Session: testID.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseRequested, pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	c := pauseresume.New(pauseresume.WithBus(bus))
	ctx := runCtx(t, testID, "run-1")
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	requested := waitEvent(t, sub)
	if requested.Type != pauseresume.EventTypePauseRequested {
		t.Fatalf("event #1 type = %q, want %q", requested.Type, pauseresume.EventTypePauseRequested)
	}
	rp, ok := requested.Payload.(pauseresume.PauseRequestedPayload)
	if !ok {
		t.Fatalf("event #1 payload type = %T, want PauseRequestedPayload", requested.Payload)
	}
	if rp.Token != string(p.Token) {
		t.Fatalf("pause.requested Token = %q, want %q", rp.Token, p.Token)
	}

	resumed := waitEvent(t, sub)
	if resumed.Type != pauseresume.EventTypePauseResumed {
		t.Fatalf("event #2 type = %q, want %q", resumed.Type, pauseresume.EventTypePauseResumed)
	}
	// D-096: the resumed payload carries the typed Decision marker so
	// wire consumers do not have to parse free-form Reason strings.
	rpp, ok := resumed.Payload.(pauseresume.PauseResumedPayload)
	if !ok {
		t.Fatalf("event #2 payload type = %T, want PauseResumedPayload", resumed.Payload)
	}
	if rpp.Decision != pauseresume.DecisionApprove {
		t.Errorf("pause.resumed Decision = %q, want %q", rpp.Decision, pauseresume.DecisionApprove)
	}
	if rpp.Token != string(p.Token) {
		t.Errorf("pause.resumed Token = %q, want %q", rpp.Token, p.Token)
	}
}

// TestResume_EmitsTypedDecision pins the D-096 contract: every
// Coordinator.Resume value (approve / reject / resume / timeout) is
// faithfully carried on the emitted PauseResumedPayload so wire
// consumers can switch on the typed marker rather than parse the
// free-form Reason string.
func TestResume_EmitsTypedDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		decision pauseresume.Decision
	}{
		{"approve", pauseresume.DecisionApprove},
		{"reject", pauseresume.DecisionReject},
		{"resume", pauseresume.DecisionResume},
		{"timeout", pauseresume.DecisionTimeout},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bus := newBus(t)
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Tenant: testID.TenantID, User: testID.UserID, Session: testID.SessionID,
				Types: []events.EventType{pauseresume.EventTypePauseResumed},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			c := pauseresume.New(pauseresume.WithBus(bus))
			ctx := runCtx(t, testID, "run-1")
			p, err := c.Request(ctx, pauseresume.PauseRequest{
				Identity: testID,
				Reason:   pauseresume.ReasonApprovalRequired,
			})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if err := c.Resume(ctx, p.Token, tc.decision, nil); err != nil {
				t.Fatalf("Resume(%s): %v", tc.decision, err)
			}
			ev := waitEvent(t, sub)
			payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
			if !ok {
				t.Fatalf("payload type = %T, want PauseResumedPayload", ev.Payload)
			}
			if payload.Decision != tc.decision {
				t.Errorf("Decision = %q, want %q", payload.Decision, tc.decision)
			}
			if payload.Token != string(p.Token) {
				t.Errorf("Token = %q, want %q", payload.Token, p.Token)
			}
		})
	}
}

// TestResume_RejectsUnknownDecision pins the fail-loudly contract: an
// untyped or unknown Decision is rejected loud with ErrInvalidDecision,
// not silently emitted on the pause.resumed event. Closes the
// audit-flagged "overloaded Reason string" anti-pattern (issue #113,
// D-096) — there is no untyped default for the load-bearing marker.
func TestResume_RejectsUnknownDecision(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// An empty Decision is the zero-value gotcha the typed enum closes
	// — a caller that forgets to set Decision must NOT silently emit a
	// pause.resumed with Decision="".
	if err := c.Resume(ctx, p.Token, pauseresume.Decision(""), nil); !errors.Is(err, pauseresume.ErrInvalidDecision) {
		t.Fatalf("Resume(empty): err=%v, want ErrInvalidDecision", err)
	}
	// A typo / forward-incompatible value also fails loud.
	if err := c.Resume(ctx, p.Token, pauseresume.Decision("bogus"), nil); !errors.Is(err, pauseresume.ErrInvalidDecision) {
		t.Fatalf("Resume(bogus): err=%v, want ErrInvalidDecision", err)
	}
	// The pause is NOT marked resumed — the contract violation is a
	// no-op on the record, mirroring the lost-handle test above.
	st, serr := c.Status(ctx, p.Token)
	if serr != nil {
		t.Fatalf("Status: %v", serr)
	}
	if st.State != pauseresume.StatusPaused {
		t.Errorf("Status.State = %q after invalid-Decision Resume, want %q",
			st.State, pauseresume.StatusPaused)
	}
}

func TestRequest_NoStore_NoCheckpointPersisted(t *testing.T) {
	t.Parallel()
	// A Coordinator with no checkpoint store still functions fully
	// process-local — Request / Status / Resume all succeed.
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonAwaitInput,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := c.Status(ctx, p.Token); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestNew_ZeroOptions_FunctionsProcessLocal(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	if c == nil {
		t.Fatal("New() returned nil")
	}
	ctx := runCtx(t, testID, "run-1")
	if _, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonConstraintsConflict,
	}); err != nil {
		t.Fatalf("Request on zero-option coordinator: %v", err)
	}
}

func TestIsValidDecision(t *testing.T) {
	t.Parallel()
	valid := []pauseresume.Decision{
		pauseresume.DecisionApprove,
		pauseresume.DecisionReject,
		pauseresume.DecisionResume,
		pauseresume.DecisionTimeout,
	}
	for _, d := range valid {
		if !pauseresume.IsValidDecision(d) {
			t.Errorf("IsValidDecision(%q) = false, want true", d)
		}
	}
	invalid := []pauseresume.Decision{
		pauseresume.Decision(""),
		pauseresume.Decision("pending"),
		pauseresume.Decision("bogus"),
	}
	for _, d := range invalid {
		if pauseresume.IsValidDecision(d) {
			t.Errorf("IsValidDecision(%q) = true, want false", d)
		}
	}
}

func TestIsValidReason(t *testing.T) {
	t.Parallel()
	valid := []pauseresume.Reason{
		pauseresume.ReasonApprovalRequired,
		pauseresume.ReasonAwaitInput,
		pauseresume.ReasonExternalEvent,
		pauseresume.ReasonConstraintsConflict,
	}
	for _, r := range valid {
		if !pauseresume.IsValidReason(r) {
			t.Errorf("IsValidReason(%q) = false, want true", r)
		}
	}
	if pauseresume.IsValidReason(pauseresume.Reason("bogus")) {
		t.Error("IsValidReason(\"bogus\") = true, want false")
	}
}

func TestRequest_HonoursCancelledContext(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	base := runCtx(t, testID, "run-1")
	ctx, cancel := context.WithCancel(base)
	cancel()

	_, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Request on cancelled ctx: err=%v, want context.Canceled", err)
	}
}

// newBus opens a fresh in-memory event bus with a no-op audit
// redactor (the default AuditConfig).
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

// waitEvent receives the next event from sub with a bounded timeout —
// never an unbounded block (CLAUDE.md §17.4: no time.Sleep as a
// synchronisation primitive; a bounded channel receive is the shape).
func waitEvent(t *testing.T, sub events.Subscription) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before an event arrived")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return events.Event{}
	}
}
