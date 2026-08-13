package react

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestProjectResponse_TaskStatus_ExplicitIDs — a single `_task_status`
// call with an explicit task_ids array translates to a TaskStatusQuery
// carrying those ids in order.
func TestProjectResponse_TaskStatus_ExplicitIDs(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: TaskStatusToolName, Args: json.RawMessage(`{"task_ids":["t-1","t-2"]}`)},
		},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("translate _task_status: %v", err)
	}
	q, ok := dec.(planner.TaskStatusQuery)
	if !ok {
		t.Fatalf("expected TaskStatusQuery, got %T", dec)
	}
	if len(q.TaskIDs) != 2 || q.TaskIDs[0] != tasks.TaskID("t-1") || q.TaskIDs[1] != tasks.TaskID("t-2") {
		t.Fatalf("TaskIDs = %v, want [t-1 t-2]", q.TaskIDs)
	}
}

// TestProjectResponse_TaskStatus_EmptyMeansListAll — missing / null / []
// task_ids all map to nil TaskIDs (the list-everything sentinel), never
// an error.
func TestProjectResponse_TaskStatus_EmptyMeansListAll(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`{}`, `{"task_ids":null}`, `{"task_ids":[]}`, ``} {
		dec, err := projectResponse(llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{
				{ID: "s1", Name: TaskStatusToolName, Args: json.RawMessage(args)},
			},
		}, &planner.RunContext{}, true)
		if err != nil {
			t.Fatalf("args %q: unexpected error %v", args, err)
		}
		q, ok := dec.(planner.TaskStatusQuery)
		if !ok {
			t.Fatalf("args %q: expected TaskStatusQuery, got %T", args, dec)
		}
		if q.TaskIDs != nil {
			t.Fatalf("args %q: TaskIDs = %v, want nil (list-everything)", args, q.TaskIDs)
		}
	}
}

// TestProjectResponse_TaskStatus_MalformedFailsLoud — malformed JSON is
// rejected with ErrInvalidDecision, never a silent empty query.
func TestProjectResponse_TaskStatus_MalformedFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: TaskStatusToolName, Args: json.RawMessage(`{"task_ids":`)},
		},
	}, &planner.RunContext{}, true)
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("expected ErrInvalidDecision, got %v", err)
	}
}

// TestProjectResponse_CancelTask_Translates — a single `_cancel_task`
// call carries task_id + reason onto CancelTask.
func TestProjectResponse_CancelTask_Translates(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "c1", Name: CancelTaskToolName, Args: json.RawMessage(`{"task_id":"t-9","reason":"lost the race"}`)},
		},
	}, &planner.RunContext{}, false)
	if err != nil {
		t.Fatalf("translate _cancel_task: %v", err)
	}
	c, ok := dec.(planner.CancelTask)
	if !ok {
		t.Fatalf("expected CancelTask, got %T", dec)
	}
	if c.TaskID != tasks.TaskID("t-9") || c.Reason != "lost the race" {
		t.Fatalf("CancelTask = %+v, want {t-9, lost the race}", c)
	}
}

// TestProjectResponse_CancelTask_EmptyIDFailsLoud — an empty task_id is
// rejected with ErrInvalidDecision (mirrors the _await_task empty-id
// guard).
func TestProjectResponse_CancelTask_EmptyIDFailsLoud(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`{}`, `{"task_id":""}`, `{"reason":"no id"}`} {
		_, err := projectResponse(llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{
				{ID: "c1", Name: CancelTaskToolName, Args: json.RawMessage(args)},
			},
		}, &planner.RunContext{}, false)
		if !errors.Is(err, planner.ErrInvalidDecision) {
			t.Fatalf("args %q: expected ErrInvalidDecision, got %v", args, err)
		}
	}
}

// TestProjectResponse_Spawn_PropagateOnCancel — spec.propagate_on_cancel
// is threaded onto SpawnSpec for the accepted enum values.
func TestProjectResponse_Spawn_PropagateOnCancel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"isolate": tasks.PropagateIsolate,
		"cascade": tasks.PropagateCascade,
	}
	for arg, want := range cases {
		dec, err := projectResponse(llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"go","propagate_on_cancel":"` + arg + `"}}`)},
			},
		}, &planner.RunContext{}, true)
		if err != nil {
			t.Fatalf("arg %q: %v", arg, err)
		}
		sp, ok := dec.(planner.SpawnTask)
		if !ok {
			t.Fatalf("arg %q: expected SpawnTask, got %T", arg, dec)
		}
		if sp.Spec.PropagateOnCancel != want {
			t.Fatalf("arg %q: PropagateOnCancel = %q, want %q", arg, sp.Spec.PropagateOnCancel, want)
		}
	}
}

// TestProjectResponse_Spawn_PropagateOnCancel_Default — an omitted
// propagate_on_cancel leaves the field empty (dispatch maps empty →
// cascade).
func TestProjectResponse_Spawn_PropagateOnCancel_Default(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"go"}}`)},
		},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if sp := dec.(planner.SpawnTask); sp.Spec.PropagateOnCancel != "" {
		t.Fatalf("PropagateOnCancel = %q, want empty", sp.Spec.PropagateOnCancel)
	}
}

// TestProjectResponse_Spawn_PropagateOnCancel_OutOfEnumFailsLoud — an
// out-of-enum propagate_on_cancel is rejected with ErrInvalidDecision
// naming the offending value, never silently clamped to cascade.
func TestProjectResponse_Spawn_PropagateOnCancel_OutOfEnumFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"go","propagate_on_cancel":"detach"}}`)},
		},
	}, &planner.RunContext{}, true)
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("expected ErrInvalidDecision, got %v", err)
	}
	if !strings.Contains(err.Error(), "detach") {
		t.Fatalf("error %q must name the offending value 'detach'", err.Error())
	}
}

func TestProjectResponse_Spawn_VirtualAgentAndArtifactIDs(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"query":"go","virtual_agent":"reviewer","input_artifact_ids":["art-1","art-2"]}}`)},
	}}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sp, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("expected SpawnTask, got %T", dec)
	}
	if sp.Spec.VirtualAgent != "reviewer" || len(sp.Spec.InputArtifactIDs) != 2 || sp.Spec.InputArtifactIDs[1] != "art-2" {
		t.Fatalf("projection = %+v, want virtual profile and two artifact IDs", sp.Spec)
	}
	if sp.Spec.InputArtifactDispositions != nil {
		t.Fatal("model disposition hints must not be projected")
	}
}

func TestProjectResponse_Spawn_RejectsContentAndDispositionFields(t *testing.T) {
	t.Parallel()
	for _, args := range []string{
		`{"spec":{"virtual_agent":"reviewer","input_artifact_ids":["art-1"],"input_artifact_bytes":"raw"}}`,
		`{"spec":{"virtual_agent":"reviewer","input_artifact_ids":["art-1"],"input_artifact_dispositions":{"art-1":"inline"}}}`,
	} {
		_, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(args)},
		}}, &planner.RunContext{}, true)
		if !errors.Is(err, planner.ErrInvalidDecision) {
			t.Fatalf("args %q: expected ErrInvalidDecision, got %v", args, err)
		}
	}
}

func TestProjectResponse_Spawn_BatchProjectsVirtualInputs(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
		{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"virtual_agent":"one","input_artifact_ids":["a"]}}`)},
		{ID: "s2", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{"virtual_agent":"two","input_artifact_ids":["b","c"]}}`)},
	}}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("batch spawn: %v", err)
	}
	b, ok := dec.(planner.Batch)
	if !ok || len(b.Spawns) != 2 {
		t.Fatalf("expected two-spawn Batch, got %T %+v", dec, dec)
	}
	if b.Spawns[0].Spec.VirtualAgent != "one" || b.Spawns[0].CallID != "s1" || len(b.Spawns[0].Spec.InputArtifactIDs) != 1 ||
		b.Spawns[1].Spec.VirtualAgent != "two" || b.Spawns[1].CallID != "s2" || len(b.Spawns[1].Spec.InputArtifactIDs) != 2 {
		t.Fatalf("batch spawns = %+v", b.Spawns)
	}
}

func TestSpawnTaskReplayArgs_VirtualInputs(t *testing.T) {
	t.Parallel()
	args := string(spawnTaskReplayArgs(planner.SpawnTask{Spec: planner.SpawnSpec{
		VirtualAgent:     "reviewer",
		InputArtifactIDs: []string{"art-1", "art-2"},
	}}))
	for _, want := range []string{`"virtual_agent":"reviewer"`, `"input_artifact_ids":["art-1","art-2"]`} {
		if !strings.Contains(args, want) {
			t.Fatalf("replay args %s missing %s", args, want)
		}
	}
}

// TestProjectResponse_TaskMgmt_StandaloneRejected — _task_status and
// _cancel_task reject co-occurrence with ANY other tool-call (head or
// tail, spawn or catalog), on both parallel settings.
func TestProjectResponse_TaskMgmt_StandaloneRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		calls []llm.ToolCallStructured
		want  string
	}{
		{
			name: "status_head_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "q1", Name: TaskStatusToolName, Args: json.RawMessage(`{}`)},
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
			},
			want: TaskStatusToolName,
		},
		{
			name: "cancel_tail_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "c1", Name: CancelTaskToolName, Args: json.RawMessage(`{"task_id":"x"}`)},
			},
			want: CancelTaskToolName,
		},
		{
			name: "status_with_spawn",
			calls: []llm.ToolCallStructured{
				{ID: "q1", Name: TaskStatusToolName, Args: json.RawMessage(`{}`)},
				{ID: "s1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{}}`)},
			},
			want: TaskStatusToolName,
		},
		{
			name: "cancel_with_status",
			calls: []llm.ToolCallStructured{
				{ID: "c1", Name: CancelTaskToolName, Args: json.RawMessage(`{"task_id":"x"}`)},
				{ID: "q1", Name: TaskStatusToolName, Args: json.RawMessage(`{}`)},
			},
			want: CancelTaskToolName,
		},
	}
	for _, tc := range cases {
		for _, parallelOn := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: "/on", false: "/off"}[parallelOn], func(t *testing.T) {
				t.Parallel()
				rc := &planner.RunContext{}
				_, err := projectResponse(llm.CompleteResponse{ToolCalls: tc.calls}, rc, parallelOn)
				if !errors.Is(err, planner.ErrInvalidDecision) {
					t.Fatalf("expected ErrInvalidDecision, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %q must name the offending control tool %q", err.Error(), tc.want)
				}
				if len(rc.PendingToolCalls) != 0 {
					t.Fatalf("PendingToolCalls must stay empty on reject, got %d", len(rc.PendingToolCalls))
				}
			})
		}
	}
}

// TestProjectResponse_TaskMgmt_SingleStillTranslates — a single
// _task_status / _cancel_task translates normally with the guard in
// place.
func TestProjectResponse_TaskMgmt_SingleStillTranslates(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "q1", Name: TaskStatusToolName, Args: json.RawMessage(`{}`)},
		},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("single status: %v", err)
	}
	if _, ok := dec.(planner.TaskStatusQuery); !ok {
		t.Fatalf("expected TaskStatusQuery, got %T", dec)
	}
	dec, err = projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "c1", Name: CancelTaskToolName, Args: json.RawMessage(`{"task_id":"x"}`)},
		},
	}, &planner.RunContext{}, false)
	if err != nil {
		t.Fatalf("single cancel: %v", err)
	}
	if _, ok := dec.(planner.CancelTask); !ok {
		t.Fatalf("expected CancelTask, got %T", dec)
	}
}

// TestReservedDecl_TaskMgmt_DeclaredWithSchemas — AC-3: the reserved
// declaration set now carries seven entries; the task-management
// controls are present with their schemas, and the _spawn_task schema
// carries propagate_on_cancel.
func TestReservedDecl_TaskMgmt_DeclaredWithSchemas(t *testing.T) {
	t.Parallel()
	decls := reservedPlannerControlDeclarations()
	if len(decls) != 7 {
		t.Fatalf("reserved declarations = %d, want 7", len(decls))
	}
	byName := map[string]llm.ToolDeclaration{}
	for _, d := range decls {
		byName[d.Name] = d
	}
	for _, name := range []string{
		SpawnTaskToolName, AwaitTaskToolName, TaskStatusToolName, CancelTaskToolName,
		SteerTaskToolName, PauseTaskToolName, ResumeTaskToolName,
	} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("reserved declaration %q missing", name)
		}
	}
	// task_status schema names task_ids and pins additionalProperties.
	if s := string(byName[TaskStatusToolName].Schema); !strings.Contains(s, "task_ids") || !strings.Contains(s, `"additionalProperties": false`) {
		t.Fatalf("_task_status schema missing task_ids / additionalProperties: %s", s)
	}
	// cancel_task schema requires task_id.
	if s := string(byName[CancelTaskToolName].Schema); !strings.Contains(s, `"required": ["task_id"]`) {
		t.Fatalf("_cancel_task schema must require task_id: %s", s)
	}
	// spawn_task schema carries the strict virtual-agent and artifact-ID
	// projection surface, but no content or disposition carrier.
	if s := string(byName[SpawnTaskToolName].Schema); !strings.Contains(s, "propagate_on_cancel") || !strings.Contains(s, `"isolate"`) ||
		!strings.Contains(s, "virtual_agent") || !strings.Contains(s, "input_artifact_ids") || strings.Contains(s, "input_artifact_bytes") || strings.Contains(s, "input_artifact_dispositions") {
		t.Fatalf("_spawn_task schema has incorrect strict input surface: %s", s)
	}
	// The task-management descriptions state the descendant-only scope and
	// the isolate-detaches-from-YOU (not the operator) semantics honestly.
	if d := byName[TaskStatusToolName].Description; !strings.Contains(d, "OWN run") {
		t.Fatalf("_task_status description must state own-run scope: %s", d)
	}
	if d := byName[SpawnTaskToolName].Description; !strings.Contains(d, "isolate") || !strings.Contains(d, "operator") {
		t.Fatalf("_spawn_task description must teach isolate + operator semantics: %s", d)
	}
}
