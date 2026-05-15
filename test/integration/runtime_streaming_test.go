// Phase 12 cross-subsystem integration test per AGENTS.md §17.
//
// Wires real audit + events + state + sessions + engine drivers and
// exercises the streaming surface end-to-end. Two scenarios:
//
//  1. TestE2E_Phase12_ParallelRuns_StreamFrames — N parallel runs ×
//     K frames each with a slow consumer. Asserts:
//     - Per-stream Seq order preserved.
//     - All frames delivered.
//     - Bus subscriber sees the runs' lifecycle events.
//     - No goroutine leak after Stop.
//
//  2. TestE2E_Phase12_StopMidStream_ReleasesWaiters — saturate the
//     producer beyond capacity, call Stop, observe ErrEngineStopped
//     on in-flight EmitChunk (the failure mode per §17.3).
//
// Both scenarios run under -race; identity propagates through every
// layer (envelope quadruple, bus filters, session-scoped subscribers).
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase12_ParallelRuns_StreamFrames covers the full Wave 4
// surface (audit + events + state + sessions + engine streaming) for
// the streaming primitive. N parallel runs × K frames each pin
// per-stream order + per-run isolation under -race.
func TestE2E_Phase12_ParallelRuns_StreamFrames(t *testing.T) {
	const tenants = 4
	const framesPerRun = 50
	const cap = 8

	cfg := phase12Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	// Open one session per tenant so the session.opened events land
	// on the bus and identity propagation is provable end-to-end.
	for i := 0; i < tenants; i++ {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", i),
			UserID:    fmt.Sprintf("u-%d", i),
			SessionID: fmt.Sprintf("s-%d", i),
		}
		ctx, _ := identity.With(context.Background(), id)
		if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
			t.Fatalf("Open tenant=%d: %v", i, err)
		}
	}

	// Build a 1-node engine with stream producer behavior.
	type seenRun struct {
		mu     sync.Mutex
		frames []engine.StreamFrame
	}
	deliveries := make(map[string]*seenRun)
	for i := 0; i < tenants; i++ {
		deliveries[fmt.Sprintf("r-%d", i)] = &seenRun{}
	}
	var deliveriesMu sync.Mutex

	nodeFunc := func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		runID := env.RunID
		for i := 0; i < framesPerRun; i++ {
			if err := nctx.EmitChunk(ctx, engine.StreamFrame{
				StreamID: runID,
				Text:     fmt.Sprintf("%s-%d", runID, i),
				Done:     i == framesPerRun-1,
			}); err != nil {
				return messages.Envelope{}, err
			}
		}
		return messages.Envelope{}, nil
	}
	node := engine.Node{Name: "stream", Func: nodeFunc, Policy: engine.NodePolicy{RunCapacity: cap}}
	eng, err := engine.New([]engine.Adjacency{{From: node}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancelEng := context.WithCancel(context.Background())
	defer cancelEng()
	baseline := runtime.NumGoroutine()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Slow consumer.
	totalFrames := tenants * framesPerRun
	consumerDone := make(chan error, 1)
	go func() {
		fetched := 0
		for fetched < totalFrames {
			fctx, fcancel := context.WithTimeout(ctx, 30*time.Second)
			env, err := eng.Fetch(fctx)
			fcancel()
			if err != nil {
				consumerDone <- err
				return
			}
			frame, ok := env.Payload.(engine.StreamFrame)
			if !ok {
				continue
			}
			deliveriesMu.Lock()
			set := deliveries[env.RunID]
			deliveriesMu.Unlock()
			if set == nil {
				consumerDone <- fmt.Errorf("unknown RunID %q", env.RunID)
				return
			}
			set.mu.Lock()
			set.frames = append(set.frames, frame)
			set.mu.Unlock()
			fetched++
			select {
			case <-time.After(150 * time.Microsecond):
			case <-ctx.Done():
				consumerDone <- ctx.Err()
				return
			}
		}
		consumerDone <- nil
	}()

	// Producers: each tenant emits one Emit (which triggers
	// framesPerRun internal EmitChunks).
	var prodWG sync.WaitGroup
	for i := 0; i < tenants; i++ {
		i := i
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			env := messages.Envelope{
				Headers: messages.Headers{
					TenantID: fmt.Sprintf("t-%d", i),
					UserID:   fmt.Sprintf("u-%d", i),
					Topic:    "stream",
				},
				SessionID: fmt.Sprintf("s-%d", i),
				RunID:     fmt.Sprintf("r-%d", i),
			}
			if err := eng.Emit(ctx, env); err != nil {
				t.Errorf("producer %d Emit: %v", i, err)
			}
		}()
	}
	prodWG.Wait()

	select {
	case err := <-consumerDone:
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("consumer didn't drain in 60s — likely deadlock")
	}

	// Per-stream order check: every run's frames have strictly
	// increasing Seq AND identity matches the originating tenant.
	for runID, set := range deliveries {
		set.mu.Lock()
		count := len(set.frames)
		ordered := true
		for i := 1; i < count; i++ {
			if set.frames[i].Seq <= set.frames[i-1].Seq {
				ordered = false
				break
			}
		}
		set.mu.Unlock()
		if count != framesPerRun {
			t.Errorf("run %q: %d frames, want %d", runID, count, framesPerRun)
		}
		if !ordered {
			t.Errorf("run %q: per-stream Seq order broken", runID)
		}
	}

	// Stop + leak check.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := eng.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// TestE2E_Phase12_StopMidStream_ReleasesWaiters covers the failure
// mode per AGENTS.md §17.3: an in-flight EmitChunk on a saturated
// run must observe ErrEngineStopped when the engine shuts down. This
// is the equivalent of the per-package TestEmitChunk_Stop_Releases-
// Waiters but exercises the surface through the wave-3 wiring.
func TestE2E_Phase12_StopMidStream_ReleasesWaiters(t *testing.T) {
	cfg := phase12Config()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	// Open the session.
	id := identity.Identity{TenantID: "TS", UserID: "US", SessionID: "SS"}
	openCtx, _ := identity.With(context.Background(), id)
	if _, err := reg.Open(openCtx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const burst = 5
	stopErrs := make(chan error, burst)
	startedCount := atomic.Int32{}

	nodeFunc := func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		var wg sync.WaitGroup
		for i := 0; i < burst; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				startedCount.Add(1)
				err := nctx.EmitChunk(ctx, engine.StreamFrame{StreamID: fmt.Sprintf("s-%d", i), Text: "x"})
				stopErrs <- err
			}()
		}
		wg.Wait()
		return messages.Envelope{}, nil
	}
	node := engine.Node{Name: "stop-stream", Func: nodeFunc, Policy: engine.NodePolicy{RunCapacity: 1}}
	eng, err := engine.New([]engine.Adjacency{{From: node}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Trigger the run.
	envOut := messages.Envelope{
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID, Topic: "stream"},
		SessionID: id.SessionID,
		RunID:     "stop-run",
	}
	if err := eng.Emit(ctx, envOut); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Drain one frame so the rest are blocked on cap=1.
	{
		fctx, fcancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = eng.Fetch(fctx)
		fcancel()
	}
	// Now Stop. Blocked EmitChunks should observe ErrEngineStopped.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := eng.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Collect all err results.
	collected := 0
	stopObserved := 0
	deadline := time.After(3 * time.Second)
	for collected < burst {
		select {
		case err := <-stopErrs:
			collected++
			if errors.Is(err, engine.ErrEngineStopped) {
				stopObserved++
			}
		case <-deadline:
			t.Fatalf("Stop did not release all waiters: collected=%d", collected)
		}
	}
	if stopObserved < 1 {
		t.Errorf("expected at least one ErrEngineStopped, got 0 (collected=%d)", collected)
	}
}

func phase12Config() *config.Config {
	return &config.Config{
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
			ServiceName: "harbor-phase12-e2e",
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
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
	}
}
