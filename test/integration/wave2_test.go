// Package integration_test holds Harbor's cross-subsystem E2E tests
// per AGENTS.md §17. The Wave 2 file pins the entire shipped surface
// together: config → audit → events → telemetry → state. Real
// drivers everywhere; no mocks at the boundary.
//
// If the wiring this test exercises ever breaks, the surface that
// downstream waves build on isn't actually alive — and the same
// kind of cross-package wiring gap PR #11 closed will resurface.
package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/telemetry/eventbus"
)

// TestE2E_Wave2_FullSurface_Aliveness wires every Wave-2 subsystem
// together and asserts the canonical paths work end-to-end:
//
//   - config validation succeeds for the shipped example shape.
//   - audit redactor scrubs an api_key and passes through ArtifactRefs.
//   - events bus accepts a Publish, fans out to a Subscribe, and
//     redacts non-SafePayload payloads to a RedactedMap.
//   - telemetry logger writes a slog record AND emits a runtime.error
//     event onto the bus via the eventbus adapter.
//   - state store round-trips a record under identity-mandatory rules.
//
// Failure modes covered:
//
//   - bus redaction error → audit.redaction_failed sibling event.
//   - state Save with empty identity → ErrIdentityRequired.
//
// This test is the wave-end smoke gate: if anything here breaks,
// the foundation downstream phases consume isn't actually alive.
func TestE2E_Wave2_FullSurface_Aliveness(t *testing.T) {
	ctx := identityCtx(t)
	red := auditpatterns.New()

	// 1. Config: validate the canonical shape.
	cfg := wave2Config()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate canonical wave2 config: %v", err)
	}

	// 2. Audit: redactor returns a redacted secret AND passes through ArtifactRef.
	in := map[string]any{
		"api_key": "real-secret",
		"image":   audit.ArtifactRef{Ref: "art://x", MIME: "image/png", SizeBytes: 1024},
	}
	out, err := red.Redact(ctx, in)
	if err != nil {
		t.Fatalf("audit.Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["api_key"] != audit.Placeholder {
		t.Errorf("audit didn't redact api_key: %v", m["api_key"])
	}
	if m["image"] != in["image"] {
		t.Errorf("audit didn't pass ArtifactRef through: %+v", m["image"])
	}

	// 3. Events bus: open + Subscribe + Publish round-trip.
	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := identity.MustFrom(ctx)
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

	// 4. Telemetry: build the logger with the eventbus adapter wired
	//    in. This is the seam PR #11 added — Logger.Error must emit
	//    BOTH the slog record AND a runtime.error bus event.
	logger, err := telemetry.New(cfg.Telemetry, red,
		telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	logger.Error(ctx, "wave2 e2e",
		slog.String("api_key", "must-be-redacted"),
		slog.String("note", "audit log: Bearer xxx.yyy.zzz"))

	// 5. Subscriber receives the runtime.error event with a redacted
	//    RedactedMap payload (RuntimeErrorPayload is NOT SafePayload,
	//    so the bus runs it through the redactor).
	got := mustReceive(t, sub, 2*time.Second)
	if got.Type != events.EventTypeRuntimeError {
		t.Fatalf("type=%v, want runtime.error", got.Type)
	}
	if got.Identity.TenantID != id.TenantID {
		t.Errorf("identity tenant mismatch: %q vs %q", got.Identity.TenantID, id.TenantID)
	}
	rm, ok := got.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("payload type=%T, want RedactedMap", got.Payload)
	}
	fields, _ := rm.Data["fields"].(map[string]any)
	if fields == nil {
		t.Fatalf("fields missing in payload: %+v", rm.Data)
	}
	if v, _ := fields["api_key"].(string); v == "must-be-redacted" {
		t.Errorf("api_key not redacted in bus payload: %v", v)
	}
	if v, _ := fields["api_key"].(string); v != "***" {
		t.Errorf("api_key redaction wrong: %v", v)
	}
	noteStr, _ := fields["note"].(string)
	if !strings.Contains(noteStr, "Bearer ***") {
		t.Errorf("bearer-in-value not redacted: %v", noteStr)
	}

	// 6. State store: round-trip a record under identity.
	store, err := state.Open(ctx, cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.MustQuadrupleFrom(ctx),
		Kind:     "session.lifecycle",
		Bytes:    []byte("wave2-payload"),
	}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	loaded, err := store.Load(ctx, rec.Identity, rec.Kind)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if string(loaded.Bytes) != "wave2-payload" {
		t.Errorf("state round-trip Bytes mismatch: %q", loaded.Bytes)
	}

	// 7. Failure mode: state.Save with empty identity is rejected.
	bad := rec
	bad.ID = state.NewEventID()
	bad.Identity = identity.Quadruple{}
	if err := store.Save(ctx, bad); err == nil {
		t.Error("state.Save accepted empty identity")
	}
}

// TestE2E_Wave2_Concurrent_Stress runs N concurrent producers
// through the full Wave 2 pipeline (logger.Error → bus → subscriber)
// while M concurrent state-store operations run alongside on the
// same identity. The test asserts:
//
//   - No -race hits (cross-package contract).
//   - Cross-tenant isolation holds: a tenant-A subscriber receives
//     ZERO events emitted with tenant-B's identity.
//   - No goroutine leak after Close.
//
// This is the cross-package concurrency hygiene the audit asked for:
// per-package D-025 tests prove intra-package safety; this one
// proves the SEAM is safe at scale.
func TestE2E_Wave2_Concurrent_Stress(t *testing.T) {
	red := auditpatterns.New()
	cfg := wave2Config()

	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	logger, err := telemetry.New(cfg.Telemetry, red,
		telemetry.WithBusEmitter(eventbus.New(bus)))
	if err != nil {
		t.Fatal(err)
	}

	const tenants = 8
	const opsPerTenant = 32

	baseline := runtime.NumGoroutine()

	// One subscriber per tenant; they only see their tenant's events.
	subs := make([]events.Subscription, tenants)
	for i := 0; i < tenants; i++ {
		id := tripleN(i)
		sub, err := bus.Subscribe(context.Background(), events.Filter{
			Tenant:  id.TenantID,
			User:    id.UserID,
			Session: id.SessionID,
			Types:   []events.EventType{events.EventTypeRuntimeError},
		})
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		subs[i] = sub
	}

	// Drainers per tenant — verify cross-tenant isolation.
	var drainWG sync.WaitGroup
	mismatches := atomic.Int64{}
	for i, sub := range subs {
		drainWG.Add(1)
		go func(tenantSeed int, sub events.Subscription) {
			defer drainWG.Done()
			want := fmt.Sprintf("t-%d", tenantSeed)
			for ev := range sub.Events() {
				if ev.Identity.TenantID != want {
					mismatches.Add(1)
				}
			}
		}(i, sub)
	}

	// Producers: each tenant emits opsPerTenant log entries AND
	// state.Saves in parallel.
	var prodWG sync.WaitGroup
	for ti := 0; ti < tenants; ti++ {
		prodWG.Add(1)
		go func(seed int) {
			defer prodWG.Done()
			id := tripleN(seed)
			ctx, err := identity.WithRun(context.Background(), id.Identity, id.RunID)
			if err != nil {
				t.Errorf("WithRun: %v", err)
				return
			}
			for j := 0; j < opsPerTenant; j++ {
				logger.Error(ctx, "stress",
					slog.Int("iter", j),
					slog.String("tenant_marker", id.TenantID))
				_ = store.Save(ctx, state.StateRecord{
					ID:       state.NewEventID(),
					Identity: id,
					Kind:     "stress.checkpoint",
					Bytes:    []byte(fmt.Sprintf("t=%d j=%d", seed, j)),
				})
			}
		}(ti)
	}
	prodWG.Wait()

	// Cancel subscribers — drainers exit, then we can check goroutines.
	for _, sub := range subs {
		sub.Cancel()
	}
	drainWG.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Errorf("%d cross-tenant deliveries observed", n)
	}

	// Tear down both subsystems before checking the leak.
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestE2E_Wave2_RedactionFailure_BusEmitsSibling pins the failure
// mode where a non-SafePayload publish hits a redactor that errors.
// The bus must (a) NOT enqueue the original payload to subscribers,
// AND (b) emit an audit.redaction_failed sibling event so an admin
// subscriber sees the failure.
func TestE2E_Wave2_RedactionFailure_BusEmitsSibling(t *testing.T) {
	cfg := wave2Config()
	bus, err := events.Open(context.Background(), cfg.Events, boomRedactor{})
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	adminSub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer adminSub.Cancel()

	ctx := identityCtx(t)
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: identity.MustQuadrupleFrom(ctx),
		Payload:  notSafePayload{APIKey: "sk-leak"},
	}
	if err := bus.Publish(ctx, ev); err == nil {
		t.Fatal("Publish accepted with failing redactor")
	}

	// Admin sees: AdminScopeUsed (from Subscribe) + AuditRedactionFailed.
	deadline := time.After(2 * time.Second)
	sawFailure := false
	for !sawFailure {
		select {
		case got := <-adminSub.Events():
			if got.Type == events.EventTypeAuditRedactionFailed {
				sawFailure = true
				p, ok := got.Payload.(events.AuditRedactionFailedPayload)
				if !ok {
					t.Errorf("payload type=%T, want AuditRedactionFailedPayload", got.Payload)
				}
				if p.OriginalType != events.EventTypeRuntimeError {
					t.Errorf("OriginalType=%v, want runtime.error", p.OriginalType)
				}
			}
		case <-deadline:
			t.Fatal("did not observe audit.redaction_failed within 2s")
		}
	}
}

// --- helpers ---

func wave2Config() *config.Config {
	c := &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			RepairAttempts: 2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
		},
		// SessionsConfig populated by Phase 08; the validator now
		// requires non-zero values, so Wave 2's config helper is
		// updated to keep validating after Phase 08 lands.
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		// ArtifactsConfig populated by Phase 17; the validator now
		// requires a non-empty driver. inmem is the floor and adds no
		// new dependencies for wave-2 surfaces (no artifact code path
		// is exercised in the wave-2 test, but Validate still runs).
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		// Phase 20 added required `tasks.driver`; Phase 21 added the
		// retain-turn / continuation-hop validators; Phase 22 added
		// required distributed driver fields. All §17.6 cross-phase
		// additions stack here so the wave-2 helper keeps validating.
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
		// MemoryConfig populated by Phase 23 — the validator added a
		// required driver field. Wave 2's surfaces don't exercise
		// memory directly, but Validate runs the field through (same
		// §17.6 cross-phase pattern as later validator-tightening
		// phases).
		Memory: config.MemoryConfig{Driver: "inmem", Strategy: "none"},
	}
	return c
}

func identityCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "R-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func tripleN(seed int) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", seed),
			UserID:    fmt.Sprintf("u-%d", seed),
			SessionID: fmt.Sprintf("s-%d", seed),
		},
		RunID: fmt.Sprintf("r-%d", seed),
	}
}

func mustReceive(t *testing.T, sub events.Subscription, timeout time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for event after %v", timeout)
	}
	return events.Event{}
}

// boomRedactor errors on every Redact — used to force the
// audit.redaction_failed sibling-emit path.
type boomRedactor struct{}

func (boomRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, fmt.Errorf("%w: forced", audit.ErrRedactionFailed)
}

// notSafePayload deliberately does NOT implement events.SafePayload
// so the bus runs it through the redactor.
type notSafePayload struct {
	events.Sealed
	APIKey string
}
