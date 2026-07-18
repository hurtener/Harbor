package react

import (
	"encoding/json"
	"errors"
	"fmt"
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
// TestProjectResponse_ReservedNameCoOccurrenceRejected pins AC-21′: only
// `_finish` and `_await_task` reject co-occurrence with other tool-calls.
// The `_spawn_task` cases that were rejected pre-AC-21′ now project to a
// Batch (see TestProjectBatch_*); they are removed from this table.
func TestProjectResponse_ReservedNameCoOccurrenceRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		calls []llm.ToolCallStructured
		want  string // substring expected in the error
	}{
		{
			name: "finish_head_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "f1", Name: FinishToolName, Args: json.RawMessage(`{"answer":"x"}`)},
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
			},
			want: FinishToolName,
		},
		{
			name: "await_in_tail_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "a1", Name: AwaitTaskToolName, Args: json.RawMessage(`{"task_id":"q"}`)},
			},
			want: AwaitTaskToolName,
		},
		{
			name: "finish_with_spawn",
			calls: []llm.ToolCallStructured{
				{ID: "f1", Name: FinishToolName, Args: json.RawMessage(`{"answer":"x"}`)},
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
			},
			want: FinishToolName,
		},
		{
			name: "await_with_spawn",
			calls: []llm.ToolCallStructured{
				{ID: "a1", Name: AwaitTaskToolName, Args: json.RawMessage(`{"task_id":"q"}`)},
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{}`)},
			},
			want: AwaitTaskToolName,
		},
	}
	for _, tc := range cases {
		for _, parallelOn := range []bool{true, false} {
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

// TestDecisionKindAndTool_Batch — the observability label for a Batch
// decision is "Batch", not the "unknown" fall-through (the emit path
// fires when Next returns a Batch).
func TestDecisionKindAndTool_Batch(t *testing.T) {
	t.Parallel()
	kind, tool := decisionKindAndTool(planner.Batch{
		Tools:  []planner.CallTool{{Tool: "a"}},
		Spawns: []planner.SpawnTask{{}},
	})
	if kind != "Batch" {
		t.Errorf("decisionKindAndTool kind = %q, want Batch", kind)
	}
	if tool != "" {
		t.Errorf("decisionKindAndTool tool = %q, want empty", tool)
	}
}

// toolCall builds a catalog tool-call fixture.
func toolCall(id, name string) llm.ToolCallStructured {
	return llm.ToolCallStructured{ID: id, Name: name, Args: json.RawMessage(`{}`)}
}

// spawnCall builds a `_spawn_task` call fixture with the given query.
func spawnCall(id, query string) llm.ToolCallStructured {
	return llm.ToolCallStructured{
		ID:   id,
		Name: SpawnTaskToolName,
		Args: json.RawMessage(`{"spec":{"query":"` + query + `"}}`),
	}
}

// TestProjectResponse_PartitionTable is the AC-21′ partition table: every
// combination of {0,1,N} catalog-tool calls × {0,1,N} `_spawn_task`
// calls (no `_finish` / `_await_task` in the tail) maps to the correct
// Decision shape. The key invariant: a response is a Batch iff it has
// ≥1 spawn AND ≥2 combined branches; a lone spawn stays SpawnTask; a
// spawn-free multi-call stays CallParallel; a single tool stays CallTool.
func TestProjectResponse_PartitionTable(t *testing.T) {
	t.Parallel()
	type shape int
	const (
		wantCallTool shape = iota
		wantCallParallel
		wantSpawnTask
		wantBatch
	)
	cases := []struct {
		name      string
		tools     int
		spawns    int
		want      shape
		wantTools int // for Batch/CallParallel assertions
		wantSpn   int
	}{
		{name: "1tool_0spawn", tools: 1, spawns: 0, want: wantCallTool},
		{name: "2tool_0spawn", tools: 2, spawns: 0, want: wantCallParallel, wantTools: 2},
		{name: "0tool_1spawn", tools: 0, spawns: 1, want: wantSpawnTask},
		{name: "1tool_1spawn", tools: 1, spawns: 1, want: wantBatch, wantTools: 1, wantSpn: 1},
		{name: "0tool_2spawn", tools: 0, spawns: 2, want: wantBatch, wantTools: 0, wantSpn: 2},
		{name: "2tool_1spawn", tools: 2, spawns: 1, want: wantBatch, wantTools: 2, wantSpn: 1},
		{name: "1tool_2spawn", tools: 1, spawns: 2, want: wantBatch, wantTools: 1, wantSpn: 2},
		{name: "3tool_2spawn", tools: 3, spawns: 2, want: wantBatch, wantTools: 3, wantSpn: 2},
	}
	for _, tc := range cases {
		for _, parallelOn := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: "/on", false: "/off"}[parallelOn], func(t *testing.T) {
				t.Parallel()
				var calls []llm.ToolCallStructured
				for i := 0; i < tc.tools; i++ {
					calls = append(calls, toolCall(fmt.Sprintf("t%d", i), fmt.Sprintf("tool%d", i)))
				}
				for i := 0; i < tc.spawns; i++ {
					calls = append(calls, spawnCall(fmt.Sprintf("s%d", i), fmt.Sprintf("q%d", i)))
				}
				dec, err := projectResponse(llm.CompleteResponse{ToolCalls: calls}, &planner.RunContext{}, parallelOn)
				if err != nil {
					t.Fatalf("projectResponse err: %v", err)
				}
				switch tc.want {
				case wantCallTool:
					if _, ok := dec.(planner.CallTool); !ok {
						t.Fatalf("got %T, want CallTool", dec)
					}
				case wantCallParallel:
					// The serialization fallback (parallel off) yields a
					// head CallTool with the tail queued; only assert
					// CallParallel when native-parallel is on.
					if parallelOn {
						par, ok := dec.(planner.CallParallel)
						if !ok {
							t.Fatalf("got %T, want CallParallel", dec)
						}
						if len(par.Branches) != tc.wantTools {
							t.Errorf("CallParallel branches = %d, want %d", len(par.Branches), tc.wantTools)
						}
					} else if _, ok := dec.(planner.CallTool); !ok {
						t.Fatalf("got %T, want CallTool (serialization fallback head)", dec)
					}
				case wantSpawnTask:
					if _, ok := dec.(planner.SpawnTask); !ok {
						t.Fatalf("got %T, want SpawnTask", dec)
					}
				case wantBatch:
					b, ok := dec.(planner.Batch)
					if !ok {
						t.Fatalf("got %T, want Batch", dec)
					}
					if len(b.Tools) != tc.wantTools {
						t.Errorf("Batch.Tools = %d, want %d", len(b.Tools), tc.wantTools)
					}
					if len(b.Spawns) != tc.wantSpn {
						t.Errorf("Batch.Spawns = %d, want %d", len(b.Spawns), tc.wantSpn)
					}
					// Batch construction respects the sealed invariants.
					if len(b.Tools)+len(b.Spawns) < 2 {
						t.Errorf("Batch is degenerate: %d combined branches", len(b.Tools)+len(b.Spawns))
					}
				}
			})
		}
	}
}

// TestProjectBatch_StampsSpawnCallIDs — every spawn projected into a
// Batch carries the native call-id of its `_spawn_task` call, and every
// tool branch carries its own call-id (the batch executor keys
// observations by them).
func TestProjectBatch_StampsSpawnCallIDs(t *testing.T) {
	t.Parallel()
	calls := []llm.ToolCallStructured{
		toolCall("call_tool_1", "search"),
		spawnCall("call_spawn_1", "dig-a"),
		spawnCall("call_spawn_2", "dig-b"),
	}
	dec, err := projectResponse(llm.CompleteResponse{ToolCalls: calls}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("projectResponse err: %v", err)
	}
	b, ok := dec.(planner.Batch)
	if !ok {
		t.Fatalf("got %T, want Batch", dec)
	}
	if len(b.Tools) != 1 || b.Tools[0].CallID != "call_tool_1" {
		t.Errorf("tool branch CallID = %q, want call_tool_1", b.Tools[0].CallID)
	}
	if len(b.Spawns) != 2 {
		t.Fatalf("Batch.Spawns = %d, want 2", len(b.Spawns))
	}
	if b.Spawns[0].CallID != "call_spawn_1" || b.Spawns[1].CallID != "call_spawn_2" {
		t.Errorf("spawn CallIDs = [%q, %q], want [call_spawn_1, call_spawn_2]",
			b.Spawns[0].CallID, b.Spawns[1].CallID)
	}
}

// TestProjectBatch_MalformedSpawnArgsRejected — a `_spawn_task` with a
// malformed args envelope inside a batch surfaces ErrInvalidDecision
// (via the shared translateNativeSpawn path), so the batch and
// single-spawn projections agree on the schema.
func TestProjectBatch_MalformedSpawnArgsRejected(t *testing.T) {
	t.Parallel()
	calls := []llm.ToolCallStructured{
		toolCall("t1", "search"),
		{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"kind":"nonsense"}`)},
	}
	_, err := projectResponse(llm.CompleteResponse{ToolCalls: calls}, &planner.RunContext{}, true)
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("malformed spawn in batch err = %v, want ErrInvalidDecision", err)
	}
}

// TestProjectBatch_RetainTurnSpawnRejected — a batched `_spawn_task`
// carrying retain_turn:true is rejected loud (NewBatch's non-retain-turn
// invariant): a turn-retaining spawn cannot ride a non-blocking batch
// dispatch.
func TestProjectBatch_RetainTurnSpawnRejected(t *testing.T) {
	t.Parallel()
	calls := []llm.ToolCallStructured{
		toolCall("t1", "search"),
		{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"q","retain_turn":true}}`)},
	}
	_, err := projectResponse(llm.CompleteResponse{ToolCalls: calls}, &planner.RunContext{}, true)
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("retain-turn spawn in batch err = %v, want ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "RetainTurn") {
		t.Errorf("error %q does not name RetainTurn", err.Error())
	}
}

// TestProjectBatch_NeverDegenerate is the "one representation per
// semantic" invariant, asserted directly at the projector: no input the
// projector accepts ever yields a one-branch-total Batch. A lone spawn
// projects to SpawnTask; a spawn-free multi-call projects to
// CallParallel; only ≥2 combined branches with ≥1 spawn yield Batch.
func TestProjectBatch_NeverDegenerate(t *testing.T) {
	t.Parallel()
	// Lone spawn → SpawnTask, never a one-branch Batch.
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{spawnCall("s1", "solo")},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("lone spawn err: %v", err)
	}
	if _, isBatch := dec.(planner.Batch); isBatch {
		t.Fatalf("lone spawn projected to Batch — degenerate; want SpawnTask")
	}
	if _, ok := dec.(planner.SpawnTask); !ok {
		t.Fatalf("lone spawn = %T, want SpawnTask", dec)
	}
	// Spawn-free multi-call → CallParallel, never a Batch.
	dec, err = projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{toolCall("t1", "a"), toolCall("t2", "b")},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("spawn-free multi err: %v", err)
	}
	if _, isBatch := dec.(planner.Batch); isBatch {
		t.Fatalf("spawn-free multi-call projected to Batch; want CallParallel")
	}
}

// TestProjectBatch_FailFastDisagreementRejected — when ≥2 spawns share
// no explicit GroupID (auto-grouped) but disagree on fail_fast, the
// projector fails loud with ErrInvalidDecision naming both conflicting
// values. A matching fail_fast, or an explicit GroupID that opts a spawn
// out of auto-grouping, does NOT trip the guard.
func TestProjectBatch_FailFastDisagreementRejected(t *testing.T) {
	t.Parallel()
	mkSpawn := func(id string, failFast bool, group string) llm.ToolCallStructured {
		args := fmt.Sprintf(`{"group_id":%q,"spec":{"query":"q","fail_fast":%t}}`, group, failFast)
		return llm.ToolCallStructured{ID: id, Name: SpawnTaskToolName, Args: json.RawMessage(args)}
	}

	// Disagreement across auto-grouped (no group_id) spawns → reject.
	_, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		mkSpawn("s1", true, ""),
		mkSpawn("s2", false, ""),
	}}, &planner.RunContext{}, true)
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("disagreement err = %v, want ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "fail_fast") {
		t.Errorf("error %q does not name fail_fast", err.Error())
	}

	// Agreement across auto-grouped spawns → OK.
	dec, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		mkSpawn("s1", true, ""),
		mkSpawn("s2", true, ""),
	}}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("agreement err: %v", err)
	}
	if _, ok := dec.(planner.Batch); !ok {
		t.Fatalf("agreement got %T, want Batch", dec)
	}

	// Explicit group_ids opt the spawns out of auto-grouping — a
	// fail_fast difference across explicitly-grouped spawns is allowed.
	dec, err = projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		mkSpawn("s1", true, "g-a"),
		mkSpawn("s2", false, "g-b"),
	}}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("explicit-group err: %v", err)
	}
	if _, ok := dec.(planner.Batch); !ok {
		t.Fatalf("explicit-group got %T, want Batch", dec)
	}
}
