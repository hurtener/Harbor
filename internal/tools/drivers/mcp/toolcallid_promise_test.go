package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// The tool-call id is a PROMISE, not a label.
//
// A host that receives a non-empty ToolCallID fetches the tool context through
// mcp.apps.tool_context and, per the reader contract, treats a miss as "the
// record is gone" — a rendered app then reports its view as unavailable rather
// than mounting an empty shell. So an id minted where NO record could ever
// exist (no capturer wired, or the capture failed) costs the reader its whole
// render for a context that never existed. These tests pin the invariant at
// the emitting side: the id is stamped on the app reference AND the discovery
// event only when a record actually landed.

// invokeAppTool drives the app-declaring fixture tool under a run-scoped
// identity and returns the reconciled app reference from the result value.
func invokeAppTool(t *testing.T, p *Provider, runID string) *AppRef {
	t.Helper()
	id := defaultIdentity()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	desc := resolveTool(t, p, "weather-server_weather")
	res, err := desc.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	val, ok := res.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("result Value type = %T, want MCPToolValue", res.Value)
	}
	if val.AppRef == nil {
		t.Fatalf("result AppRef is nil — the fixture tool declares a ui:// app")
	}
	return val.AppRef
}

// awaitAppAvailable waits for the single discovery event on the subscription.
func awaitAppAvailable(t *testing.T, sub events.Subscription) AppAvailablePayload {
	t.Helper()
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(AppAvailablePayload)
		if !ok {
			t.Fatalf("payload type = %T, want AppAvailablePayload", ev.Payload)
		}
		return payload
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.app_available event")
	}
	return AppAvailablePayload{}
}

// subscribeAppAvailable subscribes to the discovery event for the default
// identity, cancelling on cleanup.
func subscribeAppAvailable(t *testing.T, bus events.EventBus) events.Subscription {
	t.Helper()
	id := defaultIdentity()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

// TestProvider_ToolCallID_EmptyWhenNoCapturerWired proves the honest shape for
// a Provider with NO tool-context capturer: the app is still discovered and
// still published (the app renders — the discovery is independent of the
// capture), but the reference and the event carry NO tool-call id, so a host
// mounts with no data delivery instead of reporting the view as lost.
//
// The regression this closes: a runtime-ATTACHED MCP server (the
// add_mcp_connection path) reached the driver with no capturer wired while the
// id was stamped unconditionally, so every app it declared advertised a
// context that had never been written.
func TestProvider_ToolCallID_EmptyWhenNoCapturerWired(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/main.html")
	// No p.cfg.ToolContext — the unwired shape.
	sub := subscribeAppAvailable(t, bus)

	ref := invokeAppTool(t, p, "run-nocap-1")
	if ref.ToolCallID != "" {
		t.Errorf("AppRef.ToolCallID = %q, want empty (no capturer wired — nothing to fetch)", ref.ToolCallID)
	}
	if ref.ResourceURI == "" {
		t.Error("AppRef.ResourceURI empty — the app must still be discovered without a capturer")
	}

	payload := awaitAppAvailable(t, sub)
	if payload.ToolCallID != "" {
		t.Errorf("event ToolCallID = %q, want empty (no capturer wired)", payload.ToolCallID)
	}
	if payload.ResourceURI == "" {
		t.Error("event ResourceURI empty — discovery must still publish without a capturer")
	}
}

// TestProvider_ToolCallID_EmptyWhenCaptureFails proves the same honesty for a
// transient capture failure: the call still succeeds (the tool result is the
// planner's source of truth, D-225 posture) and the app is still published,
// but no id is advertised for a record that was never persisted — a state-store
// hiccup costs the delivered data, never the whole render.
func TestProvider_ToolCallID_EmptyWhenCaptureFails(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/alt.html")
	p.cfg.ToolContext = &recordingCapturer{calls: map[string]capturedCall{}, failAll: true}
	sub := subscribeAppAvailable(t, bus)

	ref := invokeAppTool(t, p, "run-capfail-1")
	if ref.ToolCallID != "" {
		t.Errorf("AppRef.ToolCallID = %q, want empty (capture failed — nothing to fetch)", ref.ToolCallID)
	}

	payload := awaitAppAvailable(t, sub)
	if payload.ToolCallID != "" {
		t.Errorf("event ToolCallID = %q, want empty (capture failed)", payload.ToolCallID)
	}
}

// TestProvider_ToolCallID_StampedWhenCaptured is the positive half of the
// invariant: with a capturer wired and the capture succeeding, the id IS
// stamped — on the reference (which the proxy path projects onto the wire) and
// on the discovery event — and it resolves to the recorded context.
func TestProvider_ToolCallID_StampedWhenCaptured(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/main.html")
	rec := newRecordingCapturer()
	p.cfg.ToolContext = rec
	sub := subscribeAppAvailable(t, bus)

	const runID = "run-cap-ok-1"
	ref := invokeAppTool(t, p, runID)
	want := ToolCallID(runID, "weather-server", "weather", json.RawMessage(`{}`))
	if ref.ToolCallID != want {
		t.Errorf("AppRef.ToolCallID = %q, want %q", ref.ToolCallID, want)
	}
	if _, ok := rec.get(want); !ok {
		t.Errorf("no captured record for the advertised id %q — the id would be a false promise", want)
	}

	payload := awaitAppAvailable(t, sub)
	if payload.ToolCallID != want {
		t.Errorf("event ToolCallID = %q, want %q", payload.ToolCallID, want)
	}
}
