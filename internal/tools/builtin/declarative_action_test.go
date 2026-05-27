package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// declarativeTestCtx is the identity-scoped context every meta-tool
// invocation requires (see requireIdentity in identity.go). Reused
// across the declarative_action tests so each test's body focuses on
// outcome assertions rather than identity boilerplate.
func declarativeTestCtx(t *testing.T) context.Context {
	t.Helper()
	q := identity.Identity{TenantID: "t-dec", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(t.Context(), q)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q, "r-dec")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// catalogWithEcho returns a catalog carrying ONLY text.echo. Used by
// the declarative_action dispatch tests so the meta-tool has a real
// inner tool to route to without entangling other built-ins.
func catalogWithEcho(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	if err := Register(cat, []string{"text.echo"}); err != nil {
		t.Fatalf("Register text.echo: %v", err)
	}
	if err := Register(cat, []string{"declarative_action"}); err != nil {
		t.Fatalf("Register declarative_action: %v", err)
	}
	return cat
}

// TestDeclarativeAction_MissingIdentityFailsLoud — §6 rule 9 + D-001:
// identity is mandatory.
func TestDeclarativeAction_MissingIdentityFailsLoud(t *testing.T) {
	t.Parallel()
	cat := catalogWithEcho(t)
	_, err := declarativeAction(context.Background(), cat, DeclarativeActionArgs{Tool: "text.echo"})
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("want ErrIdentityRequired, got %v", err)
	}
}

// TestDeclarativeAction_DispatchesTypedShape — the happy path: a
// well-formed `{Tool, Args}` invocation against an existing catalog
// tool dispatches and returns the inner tool's observation.
func TestDeclarativeAction_DispatchesTypedShape(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Tool: "text.echo",
		Args: json.RawMessage(`{"text":"hello","tag":"alpha"}`),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if !out.Dispatched {
		t.Fatalf("Dispatched = false, want true; Error=%q", out.Error)
	}
	if out.Tool != "text.echo" {
		t.Errorf("Tool = %q, want text.echo", out.Tool)
	}
	if out.RepairOutcome != nil {
		t.Errorf("RepairOutcome = %+v, want nil on clean dispatch", out.RepairOutcome)
	}
	// Verify the inner tool's typed result round-trips as a JSON
	// observation.
	var obs TextEchoOut
	if err := json.Unmarshal(out.Observation, &obs); err != nil {
		t.Fatalf("Observation unmarshal: %v (raw=%s)", err, string(out.Observation))
	}
	if obs.Echoed != "hello" || obs.Tag != "alpha" {
		t.Errorf("inner observation = %+v, want Echoed=hello Tag=alpha", obs)
	}
}

// TestDeclarativeAction_DispatchesSalvageShape — a `body` form with a
// fenced JSON envelope dispatches through the repair.ActionParser
// salvage ladder. Mirrors the brief-07 tolerance the parser ships.
func TestDeclarativeAction_DispatchesSalvageShape(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	body := "```json\n{\"tool\":\"text.echo\",\"args\":{\"text\":\"salvaged\",\"tag\":\"beta\"}}\n```"
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Body: json.RawMessage(body),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if !out.Dispatched {
		t.Fatalf("Dispatched = false on salvage, Error=%q", out.Error)
	}
	if out.Tool != "text.echo" {
		t.Errorf("Tool = %q, want text.echo (salvaged)", out.Tool)
	}
}

// TestDeclarativeAction_ArgsValidationFailureSignalsRepair — args
// schema rejection sets `RepairOutcome.ArgsRepaired=true`. Drives the
// planner's ArgsRepair counter escalation across steps.
func TestDeclarativeAction_ArgsValidationFailureSignalsRepair(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Tool: "text.echo",
		// text.echo's args schema requires `text` (string). Passing an
		// integer fails schema validation.
		Args: json.RawMessage(`{"text":42}`),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if out.Dispatched {
		t.Errorf("Dispatched = true, want false on args validation failure")
	}
	if out.RepairOutcome == nil || !out.RepairOutcome.ArgsRepaired {
		t.Fatalf("RepairOutcome = %+v, want ArgsRepaired=true", out.RepairOutcome)
	}
}

// TestDeclarativeAction_MultiActionBodySignalsMultiAction — a `body`
// that parses to N>1 envelopes returns a structured error with
// `RepairOutcome.MultiAction=true`.
func TestDeclarativeAction_MultiActionBodySignalsMultiAction(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	body := `[{"tool":"text.echo","args":{"text":"a"}},{"tool":"text.echo","args":{"text":"b"}}]`
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Body: json.RawMessage(body),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if out.Dispatched {
		t.Errorf("Dispatched = true, want false on multi-action body")
	}
	if out.RepairOutcome == nil || !out.RepairOutcome.MultiAction {
		t.Fatalf("RepairOutcome = %+v, want MultiAction=true", out.RepairOutcome)
	}
}

// TestDeclarativeAction_FinishReservedNameSignalsFinishRepair —
// reserved `_finish` via declarative_action returns a structured error
// with `RepairOutcome.FinishRepair=true`. The LLM is nudged toward
// native completion (Content-only response).
func TestDeclarativeAction_FinishReservedNameSignalsFinishRepair(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Tool: declarativeActionFinishToolName,
		Args: json.RawMessage(`{"answer":"done"}`),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if out.Dispatched {
		t.Errorf("Dispatched = true, want false on reserved _finish")
	}
	if out.RepairOutcome == nil || !out.RepairOutcome.FinishRepair {
		t.Fatalf("RepairOutcome = %+v, want FinishRepair=true", out.RepairOutcome)
	}
	if !strings.Contains(out.Error, "planner-reserved") {
		t.Errorf("Error = %q, want mention of planner-reserved", out.Error)
	}
}

// TestDeclarativeAction_SpawnAwaitReservedNamesReportWrongChannel —
// reserved `_spawn_task` / `_await_task` via declarative_action return
// a structured error nudging the LLM toward native invocation. No
// repair outcome (the LLM should re-emit through the native channel;
// classifying it as args-repair would conflate the signal).
func TestDeclarativeAction_SpawnAwaitReservedNamesReportWrongChannel(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	for _, name := range []string{declarativeActionSpawnToolName, declarativeActionAwaitToolName} {
		out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
			Tool: name,
			Args: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("declarativeAction(%q): %v", name, err)
		}
		if out.Dispatched {
			t.Errorf("Dispatched = true on reserved %q, want false", name)
		}
		if out.RepairOutcome != nil {
			t.Errorf("RepairOutcome = %+v on reserved %q, want nil", out.RepairOutcome, name)
		}
		if !strings.Contains(out.Error, "planner-reserved") {
			t.Errorf("Error on %q = %q, want mention of planner-reserved", name, out.Error)
		}
	}
}

// TestDeclarativeAction_UnknownToolReportsNotFound — a tool name miss
// returns a structured error WITHOUT a repair outcome (the LLM should
// consult `tool_search` / `tool_get`, not re-emit with adjusted args).
func TestDeclarativeAction_UnknownToolReportsNotFound(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{
		Tool: "does.not.exist",
		Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if out.Dispatched {
		t.Errorf("Dispatched = true, want false on unknown tool")
	}
	if out.RepairOutcome != nil {
		t.Errorf("RepairOutcome = %+v, want nil on unknown tool", out.RepairOutcome)
	}
	if !strings.Contains(out.Error, "not found") {
		t.Errorf("Error = %q, want mention of not found", out.Error)
	}
}

// TestDeclarativeAction_EmptyArgsRouteSignalsMissingTool — neither
// `Tool` nor `Body` supplied → ErrDeclarativeActionMissingTool with
// args-repair classification. The LLM is nudged to re-emit with the
// canonical envelope shape.
func TestDeclarativeAction_EmptyArgsRouteSignalsMissingTool(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := catalogWithEcho(t)
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if out.Dispatched {
		t.Errorf("Dispatched = true, want false on empty input")
	}
	if out.RepairOutcome == nil || !out.RepairOutcome.ArgsRepaired {
		t.Fatalf("RepairOutcome = %+v, want ArgsRepaired=true", out.RepairOutcome)
	}
}

// TestDeclarativeAction_DispatchPropagatesIdentity — the inner tool's
// invocation sees the same identity triple the meta-tool was called
// with. Pinned via a stub tool that fails on missing identity.
func TestDeclarativeAction_DispatchPropagatesIdentity(t *testing.T) {
	t.Parallel()
	ctx := declarativeTestCtx(t)
	cat := tools.NewCatalog()
	if err := Register(cat, []string{"declarative_action"}); err != nil {
		t.Fatalf("Register declarative_action: %v", err)
	}
	// A stub tool that asserts identity is present on ctx; fails the
	// inner Invoke when not.
	captured := identity.Identity{}
	descriptor := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        "id.probe",
			Description: "identity probe",
			ArgsSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		},
		Validate: func(_ json.RawMessage) error { return nil },
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			id, ok := identity.From(ctx)
			if !ok {
				return tools.ToolResult{}, errors.New("identity missing")
			}
			captured = id
			return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
		},
	}
	if err := cat.Register(descriptor); err != nil {
		t.Fatalf("Register id.probe: %v", err)
	}
	out, err := declarativeAction(ctx, cat, DeclarativeActionArgs{Tool: "id.probe", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("declarativeAction: %v", err)
	}
	if !out.Dispatched {
		t.Fatalf("Dispatched = false, Error=%q", out.Error)
	}
	if captured.TenantID != "t-dec" || captured.UserID != "u" || captured.SessionID != "s" {
		t.Errorf("inner-tool identity = %+v, want t-dec/u/s", captured)
	}
}

// TestDeclarativeAction_RegisteredAsDeferredMetaTool — the operator-
// facing shape: declarative_action carries the deferred loading mode +
// the canonical builtin tags, so it stays out of the always-loaded
// catalog unless an operator opts in (per AC-13 + AC-14).
func TestDeclarativeAction_RegisteredAsDeferredMetaTool(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := Register(cat, []string{"declarative_action"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, ok := cat.Resolve("declarative_action")
	if !ok {
		t.Fatal("declarative_action not registered")
	}
	if desc.Tool.Loading != tools.LoadingDeferred {
		t.Errorf("Loading = %q, want %q", desc.Tool.Loading, tools.LoadingDeferred)
	}
	hasTag := func(want string) bool {
		for _, tag := range desc.Tool.Tags {
			if tag == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"builtin", "meta", "escape_hatch"} {
		if !hasTag(want) {
			t.Errorf("Tags = %v, missing %q", desc.Tool.Tags, want)
		}
	}
}
