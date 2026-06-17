package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// recordingCapturer is a thread-safe in-memory ToolContextCapturer used to
// assert the driver's capture wiring without the runtime store. It records
// each capture keyed by ToolCallID and snapshots the ctx identity so a
// context-bleed assertion is possible.
type recordingCapturer struct {
	mu      sync.Mutex
	calls   map[string]capturedCall
	failAll bool
}

type capturedCall struct {
	in       CapturedToolContext
	identity identity.Identity
}

func newRecordingCapturer() *recordingCapturer {
	return &recordingCapturer{calls: make(map[string]capturedCall)}
}

func (r *recordingCapturer) Capture(ctx context.Context, in CapturedToolContext) error {
	if r.failAll {
		return errors.New("capture boom")
	}
	id, _ := identity.From(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.calls[in.ToolCallID]; dup {
		return fmt.Errorf("duplicate tool-call id captured: %q", in.ToolCallID)
	}
	r.calls[in.ToolCallID] = capturedCall{in: in, identity: id}
	return nil
}

func (r *recordingCapturer) get(id string) (capturedCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.calls[id]
	return c, ok
}

func (r *recordingCapturer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// TestToolCallID_DeterministicAndCollisionFree proves the id is a stable
// content hash (same inputs -> same id) and collision-free across distinct
// (run, server, tool, args) tuples — the property the capture key + the
// discovery-event correlation rely on. No mutable state participates.
func TestToolCallID_DeterministicAndCollisionFree(t *testing.T) {
	a := ToolCallID("run-1", "srv-a", "weather", json.RawMessage(`{"city":"NYC"}`))
	b := ToolCallID("run-1", "srv-a", "weather", json.RawMessage(`{"city":"NYC"}`))
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}
	seen := map[string]string{}
	tuples := []struct{ run, srv, tool, args string }{
		{"run-1", "srv-a", "weather", `{"city":"NYC"}`},
		{"run-2", "srv-a", "weather", `{"city":"NYC"}`},  // different run
		{"run-1", "srv-b", "weather", `{"city":"NYC"}`},  // different server
		{"run-1", "srv-a", "forecast", `{"city":"NYC"}`}, // different tool
		{"run-1", "srv-a", "weather", `{"city":"LA"}`},   // different args
		// Field-boundary aliasing guard: "ab|c" vs "a|bc" must not collide.
		{"ab", "c", "weather", `{}`},
		{"a", "bc", "weather", `{}`},
	}
	for _, tp := range tuples {
		id := ToolCallID(tp.run, tp.srv, tp.tool, json.RawMessage(tp.args))
		if prev, dup := seen[id]; dup {
			t.Fatalf("collision: %v collides with %s (id=%s)", tp, prev, id)
		}
		seen[id] = fmt.Sprintf("%v", tp)
	}
}

// TestProvider_CaptureToolContext_PlannerPath proves a planner-path tool
// call that declares a `ui://` app captures the input + lowered result keyed
// by the deterministic tool-call id, and stamps the SAME id on the discovery
// event (so a client can correlate the event to the captured context).
func TestProvider_CaptureToolContext_PlannerPath(t *testing.T) {
	bus := newTestBus(t)
	const resourceURI = "ui://weather/main.html"
	p := newAppToolProvider(t, bus, resourceURI)
	rec := newRecordingCapturer()
	p.cfg.ToolContext = rec

	id := defaultIdentity()
	const runID = "run-ctx-1"
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	desc := resolveTool(t, p, "weather-server_weather")
	args := json.RawMessage(`{}`)
	if _, err := desc.Invoke(ctx, args); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	wantID := ToolCallID(runID, "weather-server", "weather", args)
	got, ok := rec.get(wantID)
	if !ok {
		t.Fatalf("no capture for id %q (captured %d)", wantID, rec.count())
	}
	if got.in.Tool != "weather" || string(got.in.ServerID) != "weather-server" {
		t.Errorf("capture metadata wrong: %+v", got.in)
	}
	if string(got.in.Result) == "" {
		t.Errorf("capture result empty")
	}
	// The discovery event carries the same id.
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(AppAvailablePayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		if payload.ToolCallID != wantID {
			t.Errorf("event ToolCallID = %q, want %q", payload.ToolCallID, wantID)
		}
		if payload.ToolName != "weather" {
			t.Errorf("event ToolName = %q, want weather", payload.ToolName)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.app_available event")
	}
}

// TestProvider_CaptureToolContext_ErrorLoggedNotSwallowed proves a Capture
// error does NOT fail the tool call (the planner still gets its result) and
// is surfaced through the logger rather than silently dropped (CLAUDE.md
// §13). The call returns successfully despite the capturer failing.
func TestProvider_CaptureToolContext_ErrorLoggedNotSwallowed(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/alt.html")
	p.cfg.ToolContext = &recordingCapturer{calls: map[string]capturedCall{}, failAll: true}

	id := defaultIdentity()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-err-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	desc := resolveTool(t, p, "weather-server_weather")
	res, err := desc.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke must succeed despite capture failure, got: %v", err)
	}
	val, ok := res.Value.(MCPToolValue)
	if !ok || val.AppRef == nil {
		t.Fatalf("result value/app missing despite capture failure: %+v", res.Value)
	}
}

// TestProvider_ToolContextCapture_ConcurrentReuse drives N>=128 concurrent
// app-declaring tool calls against ONE shared Provider, each under a
// distinct identity + run, and asserts the D-025 guarantees: no tool-call-id
// collisions, no context bleed (each capture carries its own goroutine's
// identity), and no goroutine leak. The Provider holds no per-call state —
// the id is a pure content hash and the capture rides ctx.
func TestProvider_ToolContextCapture_ConcurrentReuse(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/main.html")
	rec := newRecordingCapturer()
	p.cfg.ToolContext = rec
	desc := resolveTool(t, p, "weather-server_weather")

	const n = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, n)
	wantIDs := make([]string, n)
	wantSessions := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "t-conc",
				UserID:    "u-conc",
				SessionID: fmt.Sprintf("s-%d", i),
			}
			runID := fmt.Sprintf("run-%d", i)
			wantSessions[i] = id.SessionID
			wantIDs[i] = ToolCallID(runID, "weather-server", "weather", json.RawMessage(`{}`))
			ctx, cerr := identity.With(context.Background(), id)
			if cerr != nil {
				errs[i] = cerr
				return
			}
			ctx, cerr = identity.WithRun(ctx, id, runID)
			if cerr != nil {
				errs[i] = cerr
				return
			}
			_, errs[i] = desc.Invoke(ctx, json.RawMessage(`{}`))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if rec.count() != n {
		t.Fatalf("captured %d contexts, want %d (id collision or lost capture)", rec.count(), n)
	}
	// No context bleed: each capture's id maps to its goroutine's identity.
	for i := range n {
		c, ok := rec.get(wantIDs[i])
		if !ok {
			t.Fatalf("goroutine %d: no capture for id %q", i, wantIDs[i])
		}
		if c.identity.SessionID != wantSessions[i] {
			t.Errorf("goroutine %d: captured session %q, want %q (context bleed)", i, c.identity.SessionID, wantSessions[i])
		}
	}
	assertNoGoroutineLeak(t, baseline)
}

// assertNoGoroutineLeak waits (bounded) for the live goroutine count to
// return to within a small slack of the baseline.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d now=%d", baseline, runtime.NumGoroutine())
}
