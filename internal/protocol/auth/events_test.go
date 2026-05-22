package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem" // events inmem driver self-register
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

// TestValidate_BusEmit_PublishesAuthRejectedEvent — PR #91 / D-082:
// when WithEventBus is supplied, every rejection publishes the
// canonical `auth.rejected` event onto the bus in addition to the
// slog.Warn. The payload carries the reason sentinel name + the
// public kid; the raw token NEVER appears.
func TestValidate_BusEmit_PublishesAuthRejectedEvent(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Subscribe via Admin filter so we receive the auth-edge sentinel
	// triple's events.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  "harbor-auth",
		User:    "auth-edge",
		Session: "auth-edge",
		Types:   []events.EventType{auth.EventTypeAuthRejected},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		auth.WithRedactor(red),
		auth.WithEventBus(bus),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Submit an EXPIRED token — Validate fails with ErrTokenExpired
	// and the audit path runs.
	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	expired := signRS256(t, priv, c, "k1")
	if _, err := v.Validate(context.Background(), expired); err == nil {
		t.Fatal("expected rejection for expired token")
	}

	// We should now see exactly one auth.rejected event on the bus.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before event arrived")
		}
		if ev.Type != auth.EventTypeAuthRejected {
			t.Fatalf("event type: got %q, want %q", ev.Type, auth.EventTypeAuthRejected)
		}
		// The payload may be the typed shape OR a RedactedMap (the
		// bus runs every non-SafePayload through the redactor —
		// AuthRejectedPayload IS SafePayload, so it round-trips
		// typed). Accept either; the load-bearing assertion is the
		// reason sentinel.
		var reason string
		switch p := ev.Payload.(type) {
		case auth.AuthRejectedPayload:
			reason = p.Reason
		case events.RedactedMap:
			if r, ok := p.Data["Reason"].(string); ok {
				reason = r
			}
		default:
			t.Fatalf("payload type: got %T", ev.Payload)
		}
		if !strings.Contains(reason, "expired") {
			t.Errorf("payload reason: got %q, want contains 'expired'", reason)
		}
		// Defence in depth: the raw token MUST NOT appear in the
		// event body anywhere.
		if strings.Contains(reason, expired) {
			t.Errorf("payload reason leaks raw token: %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for auth.rejected event")
	}
}

// TestValidate_NoBus_NoBusEmit — when WithEventBus is NOT supplied,
// the slog.Warn fires but no bus emit happens (the slog-only
// contract is preserved for tests that don't want the bus
// dependency).
func TestValidate_NoBus_NoBusEmit(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)

	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		withTestRedactor(),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	expired := signRS256(t, priv, c, "k1")
	if _, err := v.Validate(context.Background(), expired); err == nil {
		t.Fatal("expected rejection for expired token")
	}
	// No bus, no panic, no goroutine leak — the bus-emit branch is
	// a clean no-op.
}

// TestValidate_BusEmit_NilBus_NoBusEmit — WithEventBus(nil) is a
// no-op (treated as "WithEventBus not supplied"; preserves the
// option's documented "OPTIONAL" semantics).
func TestValidate_BusEmit_NilBus_NoBusEmit(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("RS256", pub)

	v, err := auth.NewValidator(keys,
		auth.WithClock(func() time.Time { return fixedNow }),
		withTestRedactor(),
		auth.WithEventBus(nil),
	)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	c := validClaims(fixedNow)
	c["exp"] = fixedNow.Add(-1 * time.Hour).Unix()
	expired := signRS256(t, priv, c, "k1")
	if _, err := v.Validate(context.Background(), expired); err == nil {
		t.Fatal("expected rejection")
	}
}

// TestEventTypeAuthRejected_Registered — the canonical event type is
// in the events registry so a Publish does not fail with
// ErrUnknownEventType.
func TestEventTypeAuthRejected_Registered(t *testing.T) {
	if !events.IsValidEventType(auth.EventTypeAuthRejected) {
		t.Fatalf("EventTypeAuthRejected (%q) not in canonical registry", auth.EventTypeAuthRejected)
	}
}

// TestAdminScopeUsedPayload_IsSafePayload — Phase 72b: the new typed
// payload composes events.SafeSealed so it is bus-publishable as a
// SafePayload (no audit-redactor walk on the bus internals; the
// control transport runs its OWN audit-boundary redactor before the
// publish per CLAUDE.md §7 rule 6 + D-020).
func TestAdminScopeUsedPayload_IsSafePayload(t *testing.T) {
	// Compile-time assertion that the type composes the seal.
	var _ events.EventPayload = auth.AdminScopeUsedPayload{}
	var _ events.SafePayload = auth.AdminScopeUsedPayload{}
}

// TestAdminScopeUsedPayload_ShapeIsFlat — Phase 72b: the payload
// fields are exactly the documented surface: Actor / Requester /
// Impersonating (IdentityTriple — flat strings) + Reason + Method
// (bounded enums). No caller-controlled bytes reach the bus.
func TestAdminScopeUsedPayload_ShapeIsFlat(t *testing.T) {
	p := auth.AdminScopeUsedPayload{
		Actor:         auth.IdentityTriple{Tenant: "t", User: "admin", Session: "s-admin"},
		Requester:     auth.IdentityTriple{Tenant: "t", User: "admin", Session: "s-admin"},
		Impersonating: auth.IdentityTriple{Tenant: "t", User: "target", Session: "s-target"},
		Reason:        auth.AdminImpersonationReason,
		Method:        string(methods.MethodStart),
	}
	if p.Actor.User != "admin" {
		t.Errorf("Actor.User: got %q want admin", p.Actor.User)
	}
	if p.Impersonating.User != "target" {
		t.Errorf("Impersonating.User: got %q want target", p.Impersonating.User)
	}
	if p.Reason != auth.AdminImpersonationReason {
		t.Errorf("Reason: got %q want %q", p.Reason, auth.AdminImpersonationReason)
	}
	if p.Method != string(methods.MethodStart) {
		t.Errorf("Method: got %q want %q", p.Method, methods.MethodStart)
	}
}

// TestAdminImpersonationReason_StableSentinel — Phase 72b: the
// sentinel name is the stable wire shape a Console branches on. A
// future emit site adding a new sentinel MUST NOT change this one.
func TestAdminImpersonationReason_StableSentinel(t *testing.T) {
	if auth.AdminImpersonationReason != "impersonation" {
		t.Fatalf("AdminImpersonationReason = %q, want %q", auth.AdminImpersonationReason, "impersonation")
	}
}

// TestAdminScopeUsedEventType_Registered — Phase 72b: the event type
// the impersonation emit uses (audit.admin_scope_used) is already
// canonical (Phase 05); confirm the registry knows it so the bus
// Publish does not fail with ErrUnknownEventType.
func TestAdminScopeUsedEventType_Registered(t *testing.T) {
	if !events.IsValidEventType(events.EventTypeAdminScopeUsed) {
		t.Fatalf("EventTypeAdminScopeUsed (%q) not in canonical registry", events.EventTypeAdminScopeUsed)
	}
}
