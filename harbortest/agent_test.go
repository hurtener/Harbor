package harbortest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/harbortest"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// helloIn is the args type for the kit's canonical demo tool. The
// schema is derived by the inproc driver; the test author only
// declares the Go types.
type helloIn struct {
	Name string `json:"name"`
}

type helloOut struct {
	Message string `json:"message"`
}

// registerHelloTool registers a thin in-process tool with the kit's
// wrapped catalog so RunOnce-based tests can exercise the
// tool.invoked / tool.completed event flow end-to-end.
func registerHelloTool(t *testing.T, cat tools.ToolCatalog, bus events.EventBus) {
	t.Helper()
	if err := inproc.RegisterFunc[helloIn, helloOut](
		cat,
		"hello",
		func(_ context.Context, in helloIn) (helloOut, error) {
			return helloOut{Message: "hi " + in.Name}, nil
		},
		tools.WithDescription("Say hi to a name"),
		tools.WithBus(bus),
	); err != nil {
		t.Fatalf("RegisterFunc: %v", err)
	}
}

// TestRunOnce_RoundTrip_CapturesEvents — the "flow-level test in ten
// lines" acceptance criterion: an Agent invokes a registered tool;
// the kit captures the tool.invoked + tool.completed events
// alongside the audit.admin_scope_used emit the subscription
// triggers.
func TestRunOnce_RoundTrip_CapturesEvents(t *testing.T) {
	red := stubRedactor{}
	bus := openInmemBus(t, red)
	cat := tools.NewCatalog()
	registerHelloTool(t, cat, bus)

	agent := harbortest.AgentFunc(func(ctx context.Context, in any) (any, error) {
		d, _ := cat.Resolve("hello")
		args, _ := json.Marshal(helloIn{Name: in.(string)})
		return d.Invoke(ctx, args)
	})

	out, log, err := harbortest.RunOnce(t.Context(), agent, "harbor", harbortest.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if log.Len() == 0 {
		t.Fatal("RunOnce: captured EventLog is empty")
	}
	got := capturedTypeSet(log.All())
	for _, want := range []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted} {
		if !got[want] {
			t.Errorf("RunOnce: expected %q in captured log, got %v", want, capturedTypeList(log.All()))
		}
	}
	// Output threading: the tool produces a helloOut wrapped in a
	// tools.ToolResult; we don't decode it here, only assert it's
	// non-nil so the test confirms the Agent's return surface flows.
	if _, ok := out.(tools.ToolResult); !ok {
		t.Errorf("RunOnce: output type = %T, want tools.ToolResult", out)
	}
}

// TestRunOnce_DefaultIdentity_IsCanonical — caller passes nothing;
// the synthesised identity uses the canonical harbortest triple. The
// test also confirms RunIDs are distinct across two zero-deps calls.
func TestRunOnce_DefaultIdentity_IsCanonical(t *testing.T) {
	red := stubRedactor{}
	bus := openInmemBus(t, red)

	var seenA, seenB identity.Quadruple
	agentA := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		seenA = identity.MustQuadrupleFrom(ctx)
		return nil, nil
	})
	agentB := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		seenB = identity.MustQuadrupleFrom(ctx)
		return nil, nil
	})
	if _, _, err := harbortest.RunOnce(t.Context(), agentA, nil, harbortest.Deps{Bus: bus}); err != nil {
		t.Fatalf("RunOnce A: %v", err)
	}
	if _, _, err := harbortest.RunOnce(t.Context(), agentB, nil, harbortest.Deps{Bus: bus}); err != nil {
		t.Fatalf("RunOnce B: %v", err)
	}
	if seenA.TenantID != "harbortest" || seenA.UserID != "harbortest" || seenA.SessionID != "harbortest" {
		t.Errorf("RunOnce A canonical triple = %+v", seenA.Identity)
	}
	if seenA.RunID == "" || seenB.RunID == "" {
		t.Errorf("RunOnce default RunIDs empty: A=%q B=%q", seenA.RunID, seenB.RunID)
	}
	if seenA.RunID == seenB.RunID {
		t.Errorf("RunOnce default RunIDs collide: %q", seenA.RunID)
	}
}

// TestRunOnce_FailsLoudly_OnNilAgent — CLAUDE.md §5: errors are
// explicit. A nil Agent is a wrapped ErrNilAgent.
func TestRunOnce_FailsLoudly_OnNilAgent(t *testing.T) {
	_, _, err := harbortest.RunOnce(t.Context(), nil, nil)
	if !errors.Is(err, harbortest.ErrNilAgent) {
		t.Errorf("RunOnce(nil agent) err = %v, want errors.Is ErrNilAgent", err)
	}
}

// TestRunOnce_FailsLoudly_OnInvalidIdentity — empty-triple Identity
// override yields ErrStackConstruction wrapping
// identity.ErrIdentityIncomplete.
func TestRunOnce_FailsLoudly_OnInvalidIdentity(t *testing.T) {
	agent := harbortest.AgentFunc(func(_ context.Context, _ any) (any, error) { return nil, nil })
	bad := identity.Identity{TenantID: "", UserID: "u", SessionID: "s"}
	_, _, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{Identity: &bad})
	if !errors.Is(err, harbortest.ErrStackConstruction) {
		t.Errorf("RunOnce(invalid identity) err = %v, want errors.Is ErrStackConstruction", err)
	}
	if !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Errorf("RunOnce(invalid identity) err = %v, want errors.Is identity.ErrIdentityIncomplete", err)
	}
}

// TestRunOnce_CustomIdentity_FlowsThrough — caller-supplied
// Identity + RunID land in the Agent's ctx exactly.
func TestRunOnce_CustomIdentity_FlowsThrough(t *testing.T) {
	id := identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "sess-1"}
	want := identity.Quadruple{Identity: id, RunID: "fixed-run"}
	var got identity.Quadruple
	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		got = identity.MustQuadrupleFrom(ctx)
		return nil, nil
	})
	if _, _, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Identity: &id,
		RunID:    "fixed-run",
	}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != want {
		t.Errorf("RunOnce custom identity: got %+v want %+v", got, want)
	}
}

// TestRunOnce_OwnsBusLifecycle_WhenDepsBusOmitted — RunOnce opens
// its own bus when Deps.Bus is nil, and closes it on return. The
// test exercises the path and just asserts no error.
func TestRunOnce_OwnsBusLifecycle_WhenDepsBusOmitted(t *testing.T) {
	called := false
	agent := harbortest.AgentFunc(func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !called {
		t.Error("RunOnce did not invoke agent")
	}
	if log == nil {
		t.Error("RunOnce returned nil log")
	}
}

// TestRunOnce_AgentError_ReturnsLog — even when the Agent errors,
// the captured EventLog is returned alongside the error so the
// caller can inspect what was emitted before the failure.
func TestRunOnce_AgentError_ReturnsLog(t *testing.T) {
	wantErr := errors.New("planned failure")
	agent := harbortest.AgentFunc(func(_ context.Context, _ any) (any, error) {
		return nil, wantErr
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("RunOnce err = %v, want %v", err, wantErr)
	}
	if log == nil {
		t.Error("RunOnce returned nil log on agent error")
	}
}

// TestEventLog_RecordedEvents_FiltersByRun — two RunOnce calls
// share a Deps.Bus; the union log filters cleanly by RunID.
func TestEventLog_RecordedEvents_FiltersByRun(t *testing.T) {
	red := stubRedactor{}
	bus := openInmemBus(t, red)
	cat := tools.NewCatalog()
	registerHelloTool(t, cat, bus)

	agent := harbortest.AgentFunc(func(ctx context.Context, in any) (any, error) {
		d, _ := cat.Resolve("hello")
		args, _ := json.Marshal(helloIn{Name: "x"})
		return d.Invoke(ctx, args)
	})
	_, logA, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{Bus: bus, RunID: "run-a"})
	if err != nil {
		t.Fatalf("RunOnce A: %v", err)
	}
	_, _, err = harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{Bus: bus, RunID: "run-b"})
	if err != nil {
		t.Fatalf("RunOnce B: %v", err)
	}

	// logA is local to the first invocation — it captured every
	// event from the first subscription before the second RunOnce
	// began. So logA should contain only run-a events.
	for _, ev := range logA.RecordedEvents("run-a") {
		if ev.Identity.RunID != "run-a" {
			t.Errorf("RecordedEvents(run-a) returned event with RunID=%q", ev.Identity.RunID)
		}
	}
	if got := logA.RecordedEvents("run-b"); len(got) != 0 {
		t.Errorf("RecordedEvents(run-b) on logA = %d events, want 0 (logA subscribed before run-b started)", len(got))
	}
}

// capturedTypeSet returns the set of EventTypes in evs as a map.
func capturedTypeSet(evs []events.Event) map[events.EventType]bool {
	out := make(map[events.EventType]bool, len(evs))
	for _, ev := range evs {
		out[ev.Type] = true
	}
	return out
}

// capturedTypeList returns the comma-joined types for error messages.
func capturedTypeList(evs []events.Event) string {
	names := make([]string, len(evs))
	for i, ev := range evs {
		names[i] = string(ev.Type)
	}
	return "[" + strings.Join(names, ", ") + "]"
}
