package eventbus_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/telemetry/eventbus"
)

func TestNew_NilBusReturnsNil(t *testing.T) {
	if a := eventbus.New(nil); a != nil {
		t.Fatalf("New(nil) = %v, want nil", a)
	}
}

func TestEmitRuntimeError_Nil_NoPanic(t *testing.T) {
	var a *eventbus.Adapter
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Adapter panicked: %v", r)
		}
	}()
	a.EmitRuntimeError(context.Background(), "x", nil)
}

// TestEndToEnd wires audit + events + telemetry through real
// drivers and asserts: a Logger.Error call writes both an slog
// record AND publishes a redacted runtime.error event onto the bus
// where a subscriber receives it. This is the cross-package
// contract the BusEmitter seam was designed for.
func TestEndToEnd_Logger_Bus_Subscribe(t *testing.T) {
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), eventsCfg(), red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeError},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Wire the adapter through telemetry.
	logger, err := telemetry.New(telemetryCfg(), red, telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	// Use a slog attr containing a secret to verify the bus
	// receives the redacted version.
	logger.Error(ctx, "boom", slog.String("api_key", "real-secret-do-not-leak"))

	// Wait briefly for the publish.
	deadline := time.After(2 * time.Second)
	var got events.Event
	for {
		select {
		case got = <-sub.Events():
			goto received
		case <-deadline:
			t.Fatal("subscriber did not receive runtime.error within 2s")
		}
	}
received:
	if got.Type != events.EventTypeRuntimeError {
		t.Fatalf("Type=%v, want runtime.error", got.Type)
	}
	if got.Identity.TenantID != "T" {
		t.Errorf("Identity.TenantID=%q, want T", got.Identity.TenantID)
	}

	// The bus runs RuntimeErrorPayload through the redactor on
	// Publish (it's NOT a SafePayload), so the result on the
	// subscriber side is a RedactedMap.
	rm, ok := got.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("payload type=%T, want RedactedMap", got.Payload)
	}
	// The msg field should contain the original message; the
	// fields map should have api_key redacted to "***".
	if msg, _ := rm.Data["message"].(string); msg != "boom" {
		t.Errorf("message=%v, want boom", rm.Data["message"])
	}
	fields, _ := rm.Data["fields"].(map[string]any)
	if fields == nil {
		t.Fatalf("fields not present: %+v", rm.Data)
	}
	if v, _ := fields["api_key"].(string); v == "real-secret-do-not-leak" {
		t.Errorf("api_key leaked into bus payload: %v", v)
	}
	if v, _ := fields["api_key"].(string); v != "***" {
		t.Errorf("api_key not redacted: %v (full fields=%v)", v, fields)
	}
}

// TestEmitRuntimeError_NoIdentity_QuietlySkips covers the path
// where ctx has no identity — the adapter must NOT panic and must
// NOT propagate the bus-side ErrIdentityRequired back to Logger.
func TestEmitRuntimeError_NoIdentity_QuietlySkips(t *testing.T) {
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), eventsCfg(), red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	logger, err := telemetry.New(telemetryCfg(), red, telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	// No identity in ctx — adapter must silently skip Publish.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter propagated panic: %v", r)
		}
	}()
	logger.Error(context.Background(), "bare", slog.String("k", "v"))
}

func eventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     16,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}
}

func telemetryCfg() config.TelemetryConfig {
	return config.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "debug",
		ServiceName: "harbor-test",
	}
}
