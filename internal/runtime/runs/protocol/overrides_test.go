package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// newTestBus builds an in-memory events bus for the audit-emit tests.
func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         100,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	return bus
}

// fixedClock returns a deterministic instant — CLAUDE.md §11 time-
// sensitive tests use a controllable clock, never time.Now in an
// assertion.
func fixedClock(at time.Time) runsprotocol.Clock {
	return func() time.Time { return at }
}

func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }
func intPtr(i int) *int         { return &i }

const (
	testTenant  = "tenant-runs"
	testUser    = "user-runs"
	testSession = "session-runs"
)

func newService(t *testing.T, opts ...runsprotocol.Option) (*runsprotocol.Service, *runsprotocol.Store) {
	t.Helper()
	store := runsprotocol.NewStore()
	svc, err := runsprotocol.NewService(store, opts...)
	if err != nil {
		t.Fatalf("NewService: unexpected error: %v", err)
	}
	return svc, store
}

func wireReq(o prototypes.RunOverrides) prototypes.RunSetOverridesRequest {
	return prototypes.RunSetOverridesRequest{
		Identity:  prototypes.IdentityScope{Tenant: testTenant, User: testUser, Session: testSession},
		Overrides: o,
	}
}

func TestNewService_FailsLoudOnNilStore(t *testing.T) {
	_, err := runsprotocol.NewService(nil)
	if !errors.Is(err, runsprotocol.ErrMisconfigured) {
		t.Fatalf("NewService(nil) error = %v, want ErrMisconfigured", err)
	}
}

func TestSetOverrides_RecordsOverrideForNextMessage(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	svc, store := newService(t, runsprotocol.WithClock(fixedClock(at)))

	req := wireReq(prototypes.RunOverrides{
		SessionID:       testSession,
		ReasoningEffort: strPtr("high"),
		Temperature:     f64Ptr(0.7),
		MaxTokens:       intPtr(2048),
	})
	resp, err := svc.SetOverrides(context.Background(), req)
	if err != nil {
		t.Fatalf("SetOverrides: unexpected error: %v", err)
	}
	if !resp.AppliedAt.Equal(at) {
		t.Errorf("AppliedAt = %v, want %v", resp.AppliedAt, at)
	}
	if resp.ProtocolVersion != prototypes.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", resp.ProtocolVersion, prototypes.ProtocolVersion)
	}

	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}
	po, ok := store.Peek(id)
	if !ok {
		t.Fatal("override not recorded in the Store")
	}
	if po.ReasoningEffort == nil || *po.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %v, want high", po.ReasoningEffort)
	}
	if po.Temperature == nil || *po.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", po.Temperature)
	}
	if po.MaxTokens == nil || *po.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %v, want 2048", po.MaxTokens)
	}
}

func TestSetOverrides_AppliesToNextMessageOnly_NotRetroactive(t *testing.T) {
	svc, store := newService(t)
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}

	if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID: testSession, ReasoningEffort: strPtr("low"),
	})); err != nil {
		t.Fatalf("SetOverrides: %v", err)
	}
	// The next message consumes the override — it is one-shot.
	po, ok := store.Consume(id)
	if !ok || po.ReasoningEffort == nil || *po.ReasoningEffort != "low" {
		t.Fatalf("Consume returned %v, %v — want the recorded override", po, ok)
	}
	// A second message (a "past message" relative to a fresh override)
	// sees NOTHING — the override did not apply retroactively and was
	// consumed.
	if _, ok := store.Consume(id); ok {
		t.Error("Consume returned a stale override — the slot should be empty after one-shot consume")
	}
}

func TestSetOverrides_LastWriteWins(t *testing.T) {
	svc, store := newService(t)
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}

	if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID: testSession, ReasoningEffort: strPtr("low"),
	})); err != nil {
		t.Fatalf("SetOverrides #1: %v", err)
	}
	if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID: testSession, ReasoningEffort: strPtr("high"),
	})); err != nil {
		t.Fatalf("SetOverrides #2: %v", err)
	}
	po, ok := store.Consume(id)
	if !ok || po.ReasoningEffort == nil || *po.ReasoningEffort != "high" {
		t.Errorf("Consume = %v, %v — want the second (last) override", po, ok)
	}
}

func TestSetOverrides_RejectsIncompleteIdentity(t *testing.T) {
	svc, _ := newService(t)
	cases := map[string]prototypes.IdentityScope{
		"missing tenant":  {User: testUser, Session: testSession},
		"missing user":    {Tenant: testTenant, Session: testSession},
		"missing session": {Tenant: testTenant, User: testUser},
	}
	for name, idscope := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
				Identity:  idscope,
				Overrides: prototypes.RunOverrides{SessionID: testSession},
			})
			if !errors.Is(err, runsprotocol.ErrIdentityRequired) {
				t.Fatalf("error = %v, want ErrIdentityRequired", err)
			}
		})
	}
}

func TestSetOverrides_RejectsCrossSessionOverride(t *testing.T) {
	svc, store := newService(t)
	// The verified session is testSession; the override names a
	// DIFFERENT session — a cross-session escalation attempt.
	_, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID:       "some-other-session",
		ReasoningEffort: strPtr("high"),
	}))
	if !errors.Is(err, runsprotocol.ErrCrossSessionScope) {
		t.Fatalf("error = %v, want ErrCrossSessionScope", err)
	}
	// Nothing was recorded for either session.
	if _, ok := store.Peek(identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}); ok {
		t.Error("a rejected cross-session override must not be recorded")
	}
}

func TestSetOverrides_RejectsEmptyOverrideSessionID(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID: "", ReasoningEffort: strPtr("high"),
	}))
	if !errors.Is(err, runsprotocol.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

func TestSetOverrides_ValidatesOverridePayload(t *testing.T) {
	svc, _ := newService(t)
	cases := map[string]prototypes.RunOverrides{
		"unknown reasoning effort": {SessionID: testSession, ReasoningEffort: strPtr("ultra")},
		"temperature too high":     {SessionID: testSession, Temperature: f64Ptr(3.0)},
		"temperature negative":     {SessionID: testSession, Temperature: f64Ptr(-0.1)},
		"non-positive max tokens":  {SessionID: testSession, MaxTokens: intPtr(0)},
		"negative max tokens":      {SessionID: testSession, MaxTokens: intPtr(-5)},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.SetOverrides(context.Background(), wireReq(o))
			if !errors.Is(err, runsprotocol.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestSetOverrides_AcceptsSystemPromptOverride(t *testing.T) {
	svc, store := newService(t)
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}
	if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID:            testSession,
		SystemPromptOverride: strPtr("You are a terse assistant."),
	})); err != nil {
		t.Fatalf("SetOverrides: %v", err)
	}
	po, ok := store.Peek(id)
	if !ok || po.SystemPromptOverride == nil || *po.SystemPromptOverride != "You are a terse assistant." {
		t.Errorf("SystemPromptOverride = %v, want the recorded prompt", po.SystemPromptOverride)
	}
}

func TestSetOverrides_HonoursContextCancellation(t *testing.T) {
	svc, _ := newService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.SetOverrides(ctx, wireReq(prototypes.RunOverrides{
		SessionID: testSession, ReasoningEffort: strPtr("high"),
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSetOverrides_EmitsAuditEventOnBus(t *testing.T) {
	bus := newTestBus(t)
	defer func() { _ = bus.Close(context.Background()) }()

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: testTenant, User: testUser, Session: testSession,
		Types: []events.EventType{events.EventTypeRunOverridesSet},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	svc, _ := newService(t, runsprotocol.WithBus(bus))
	if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
		SessionID:       testSession,
		ReasoningEffort: strPtr("high"),
		Temperature:     f64Ptr(0.5),
	})); err != nil {
		t.Fatalf("SetOverrides: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeRunOverridesSet {
			t.Fatalf("event type = %q, want runs.overrides_set", ev.Type)
		}
		payload, ok := ev.Payload.(events.RunOverridesSetPayload)
		if !ok {
			t.Fatalf("payload type = %T, want RunOverridesSetPayload", ev.Payload)
		}
		if payload.SessionID != testSession {
			t.Errorf("payload SessionID = %q, want %q", payload.SessionID, testSession)
		}
		if !payload.SetReasoningEffort || !payload.SetTemperature {
			t.Errorf("payload set-flags = %+v, want reasoning+temperature set", payload)
		}
		if payload.SetMaxTokens || payload.SetSystemPrompt {
			t.Errorf("payload set-flags = %+v, want max-tokens+system-prompt unset", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runs.overrides_set event")
	}
}
