package react

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestProjectResponse_SingleToolCallMapsToCallTool — AC-19 first
// branch: `len(resp.ToolCalls) == 1` produces a `CallTool` carrying
// the native ID + Name + Args verbatim. `PendingToolCalls` stays empty.
func TestProjectResponse_SingleToolCallMapsToCallTool(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{
		Content: "preamble that should not become Finish",
		ToolCalls: []llm.ToolCallStructured{
			{ID: "call_123", Name: "foo", Args: json.RawMessage(`{"x":1}`)},
		},
	}, rc, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("expected CallTool, got %T (%#v)", dec, dec)
	}
	if call.Tool != "foo" || call.CallID != "call_123" || string(call.Args) != `{"x":1}` {
		t.Fatalf("CallTool mismatch: %#v", call)
	}
	if len(rc.PendingToolCalls) != 0 {
		t.Fatalf("PendingToolCalls should be empty, got %d", len(rc.PendingToolCalls))
	}
}

// TestProjectResponse_MultiToolCallSerializes — AC-19 serialization
// fallback: N>1 ToolCalls emit the FIRST as CallTool, the remainder
// accumulate on `rc.PendingToolCalls` for subsequent steps to drain.
func TestProjectResponse_MultiToolCallSerializes(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "a", Name: "first", Args: json.RawMessage(`{"a":1}`)},
			{ID: "b", Name: "second", Args: json.RawMessage(`{"b":2}`)},
			{ID: "c", Name: "third", Args: json.RawMessage(`{"c":3}`)},
		},
	}, rc, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok || call.Tool != "first" || call.CallID != "a" {
		t.Fatalf("first decision: expected CallTool first/a, got %T %#v", dec, dec)
	}
	if len(rc.PendingToolCalls) != 2 {
		t.Fatalf("PendingToolCalls len = %d, want 2", len(rc.PendingToolCalls))
	}
	if rc.PendingToolCalls[0].Name != "second" || rc.PendingToolCalls[0].CallID != "b" {
		t.Fatalf("pending[0] mismatch: %#v", rc.PendingToolCalls[0])
	}
	if rc.PendingToolCalls[1].Name != "third" || rc.PendingToolCalls[1].CallID != "c" {
		t.Fatalf("pending[1] mismatch: %#v", rc.PendingToolCalls[1])
	}
}

// TestProjectResponse_NoToolsWithContentFinishesGoal — AC-19 third
// branch: zero ToolCalls + non-empty Content maps to a goal-finish
// carrying the model's natural-language reply as Payload.
func TestProjectResponse_NoToolsWithContentFinishesGoal(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{Content: "All done."}, rc, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("expected Finish, got %T (%#v)", dec, dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Reason = %q, want FinishGoal", fin.Reason)
	}
	if s, _ := fin.Payload.(string); s != "All done." {
		t.Fatalf("Payload mismatch: %#v", fin.Payload)
	}
}

// TestProjectResponse_EmptyEverythingMapsToNoPath — AC-19 fallback:
// empty Content + empty ToolCalls → Finish{NoPath} with a followup
// marker so the runtime can graceful-fail.
func TestProjectResponse_EmptyEverythingMapsToNoPath(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{}, rc, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("expected Finish, got %T (%#v)", dec, dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Fatalf("Reason = %q, want FinishNoPath", fin.Reason)
	}
}

// TestDrainPending_PullsFromPendingAndShrinks — the helper the
// React planner's Next() will call before consulting the LLM again.
func TestDrainPending_PullsFromPendingAndShrinks(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{
		PendingToolCalls: []planner.ToolCallDeferred{
			{Name: "a", CallID: "x", Args: json.RawMessage(`{}`)},
			{Name: "b", CallID: "y", Args: json.RawMessage(`{"y":true}`)},
		},
	}
	first := drainPending(rc)
	if first == nil || first.Tool != "a" || first.CallID != "x" {
		t.Fatalf("first drain mismatch: %#v", first)
	}
	if len(rc.PendingToolCalls) != 1 {
		t.Fatalf("Pending length after first drain = %d, want 1", len(rc.PendingToolCalls))
	}
	second := drainPending(rc)
	if second == nil || second.Tool != "b" {
		t.Fatalf("second drain mismatch: %#v", second)
	}
	if len(rc.PendingToolCalls) != 0 {
		t.Fatalf("Pending should be empty after final drain, got %d", len(rc.PendingToolCalls))
	}
	if drainPending(rc) != nil {
		t.Fatalf("empty drain should return nil")
	}
}

// TestProjectResponse_ReservedFinishToolNameProducesFinish — the
// projector recognises the reserved `_finish` tool-name (which the
// React planner declares as a meta-tool) and produces Finish{Goal}
// with the args.answer string as the payload.
func TestProjectResponse_ReservedFinishToolNameProducesFinish(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{
				ID:   "f1",
				Name: FinishToolName,
				Args: json.RawMessage(`{"answer":"ok"}`),
			},
		},
	}, rc, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("expected Finish, got %T (%#v)", dec, dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Reason = %q, want FinishGoal", fin.Reason)
	}
	if s, _ := fin.Payload.(string); s != "ok" {
		t.Fatalf("Payload = %#v, want \"ok\"", fin.Payload)
	}
}

// TestProjectResponse_MultiToolCallNativeParallel — AC-12: with native
// parallel ON, N>1 ToolCalls project to a CallParallel carrying one
// branch per call (each with its CallID) + a nil Join. PendingToolCalls
// stays empty (no serialization).
func TestProjectResponse_MultiToolCallNativeParallel(t *testing.T) {
	t.Parallel()
	rc := &planner.RunContext{}
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "a", Name: "first", Args: json.RawMessage(`{"a":1}`)},
			{ID: "b", Name: "second", Args: json.RawMessage(`{"b":2}`)},
			{ID: "c", Name: "third", Args: json.RawMessage(`{"c":3}`)},
		},
	}, rc, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	par, ok := dec.(planner.CallParallel)
	if !ok {
		t.Fatalf("expected CallParallel, got %T (%#v)", dec, dec)
	}
	if par.Join != nil {
		t.Fatalf("Join should be nil (normalises to JoinAll), got %#v", par.Join)
	}
	if len(par.Branches) != 3 {
		t.Fatalf("Branches len = %d, want 3", len(par.Branches))
	}
	want := []struct{ tool, id, args string }{
		{"first", "a", `{"a":1}`},
		{"second", "b", `{"b":2}`},
		{"third", "c", `{"c":3}`},
	}
	for i, w := range want {
		b := par.Branches[i]
		if b.Tool != w.tool || b.CallID != w.id || string(b.Args) != w.args {
			t.Fatalf("branch[%d] mismatch: %#v", i, b)
		}
	}
	if len(rc.PendingToolCalls) != 0 {
		t.Fatalf("PendingToolCalls should be empty on the native path, got %d", len(rc.PendingToolCalls))
	}
}

// TestProjectResponse_ReservedNameCoOccurrenceRejected — AC-22: a
// reserved planner-control name co-occurring with another tool-call is
// rejected with ErrInvalidDecision, in head AND tail position, with the
// knob ON and OFF, and PendingToolCalls is never populated.
func TestProjectResponse_ReservedNameCoOccurrenceRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		calls []llm.ToolCallStructured
		want  string // substring expected in the error
	}{
		{
			name: "spawn_head_plus_two",
			calls: []llm.ToolCallStructured{
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "t2", Name: "beta", Args: json.RawMessage(`{}`)},
			},
			want: SpawnTaskToolName,
		},
		{
			name: "regular_head_spawn_in_tail",
			calls: []llm.ToolCallStructured{
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
			},
			want: SpawnTaskToolName,
		},
		{
			name: "two_spawns",
			calls: []llm.ToolCallStructured{
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
				{ID: "s2", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
			},
			want: SpawnTaskToolName,
		},
		{
			name: "finish_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "f1", Name: FinishToolName, Args: json.RawMessage(`{"answer":"x"}`)},
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
			},
			want: FinishToolName,
		},
		{
			name: "await_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "a1", Name: AwaitTaskToolName, Args: json.RawMessage(`{"task_id":"q"}`)},
			},
			want: AwaitTaskToolName,
		},
	}
	for _, tc := range cases {
		for _, parallelOn := range []bool{true, false} {
			tc, parallelOn := tc, parallelOn
			t.Run(tc.name+map[bool]string{true: "/on", false: "/off"}[parallelOn], func(t *testing.T) {
				t.Parallel()
				rc := &planner.RunContext{}
				_, err := projectResponse(llm.CompleteResponse{ToolCalls: tc.calls}, rc, parallelOn)
				if err == nil {
					t.Fatalf("expected ErrInvalidDecision, got nil")
				}
				if !errors.Is(err, planner.ErrInvalidDecision) {
					t.Fatalf("expected ErrInvalidDecision, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %q does not name the offending control tool %q", err.Error(), tc.want)
				}
				if len(rc.PendingToolCalls) != 0 {
					t.Fatalf("PendingToolCalls must stay empty on reject, got %d", len(rc.PendingToolCalls))
				}
			})
		}
	}
}

// TestProjectResponse_SingleReservedCallsStillTranslate — AC-22(d)
// regression: a single reserved call still translates normally with the
// guard in place (head switch unchanged).
func TestProjectResponse_SingleReservedCallsStillTranslate(t *testing.T) {
	t.Parallel()
	// single _spawn_task → SpawnTask
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"go"}}`)},
		},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("single spawn err: %v", err)
	}
	if _, ok := dec.(planner.SpawnTask); !ok {
		t.Fatalf("expected SpawnTask, got %T", dec)
	}
	// single _await_task → AwaitTask
	dec, err = projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "a1", Name: AwaitTaskToolName, Args: json.RawMessage(`{"task_id":"q"}`)},
		},
	}, &planner.RunContext{}, false)
	if err != nil {
		t.Fatalf("single await err: %v", err)
	}
	if _, ok := dec.(planner.AwaitTask); !ok {
		t.Fatalf("expected AwaitTask, got %T", dec)
	}
}
