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

// TestProjectResponse_SteerTask_Translates — AC-4: a single `_steer_task`
// call carries task_id + directive onto SteerTask.
func TestProjectResponse_SteerTask_Translates(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "s1", Name: SteerTaskToolName, Args: json.RawMessage(`{"task_id":"t-1","directive":"focus on auth"}`)},
		},
	}, &planner.RunContext{}, true)
	if err != nil {
		t.Fatalf("translate _steer_task: %v", err)
	}
	s, ok := dec.(planner.SteerTask)
	if !ok {
		t.Fatalf("expected SteerTask, got %T", dec)
	}
	if s.TaskID != tasks.TaskID("t-1") || s.Directive != "focus on auth" {
		t.Fatalf("SteerTask = %+v, want {t-1, focus on auth}", s)
	}
}

// TestProjectResponse_SteerTask_EmptyFieldsFailLoud — AC-4: an empty
// task_id OR an empty directive is rejected with ErrInvalidDecision.
func TestProjectResponse_SteerTask_EmptyFieldsFailLoud(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`{}`, `{"task_id":""}`, `{"directive":"d"}`, `{"task_id":"x"}`, `{"task_id":"x","directive":""}`} {
		_, err := projectResponse(llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{
				{ID: "s1", Name: SteerTaskToolName, Args: json.RawMessage(args)},
			},
		}, &planner.RunContext{}, true)
		if !errors.Is(err, planner.ErrInvalidDecision) {
			t.Fatalf("args %q: expected ErrInvalidDecision, got %v", args, err)
		}
	}
}

// TestProjectResponse_PauseTask_Translates — AC-4: a single `_pause_task`
// carries task_id (+ optional reason) onto PauseTask.
func TestProjectResponse_PauseTask_Translates(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "p1", Name: PauseTaskToolName, Args: json.RawMessage(`{"task_id":"t-2","reason":"hold"}`)},
		},
	}, &planner.RunContext{}, false)
	if err != nil {
		t.Fatalf("translate _pause_task: %v", err)
	}
	p, ok := dec.(planner.PauseTask)
	if !ok {
		t.Fatalf("expected PauseTask, got %T", dec)
	}
	if p.TaskID != tasks.TaskID("t-2") || p.Reason != "hold" {
		t.Fatalf("PauseTask = %+v, want {t-2, hold}", p)
	}
}

// TestProjectResponse_ResumeTask_Translates — AC-4: a single
// `_resume_task` carries task_id (+ optional directive) onto ResumeTask.
func TestProjectResponse_ResumeTask_Translates(t *testing.T) {
	t.Parallel()
	dec, err := projectResponse(llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{
			{ID: "r1", Name: ResumeTaskToolName, Args: json.RawMessage(`{"task_id":"t-3","directive":"go"}`)},
		},
	}, &planner.RunContext{}, false)
	if err != nil {
		t.Fatalf("translate _resume_task: %v", err)
	}
	r, ok := dec.(planner.ResumeTask)
	if !ok {
		t.Fatalf("expected ResumeTask, got %T", dec)
	}
	if r.TaskID != tasks.TaskID("t-3") || r.Directive != "go" {
		t.Fatalf("ResumeTask = %+v, want {t-3, go}", r)
	}
}

// TestProjectResponse_PauseResume_EmptyIDFailsLoud — AC-4: an empty
// task_id fails loud for both pause and resume; reason/directive optional.
func TestProjectResponse_PauseResume_EmptyIDFailsLoud(t *testing.T) {
	t.Parallel()
	for _, name := range []string{PauseTaskToolName, ResumeTaskToolName} {
		for _, args := range []string{`{}`, `{"task_id":""}`, `{"reason":"x"}`} {
			_, err := projectResponse(llm.CompleteResponse{
				ToolCalls: []llm.ToolCallStructured{{ID: "x", Name: name, Args: json.RawMessage(args)}},
			}, &planner.RunContext{}, false)
			if !errors.Is(err, planner.ErrInvalidDecision) {
				t.Fatalf("%s args %q: expected ErrInvalidDecision, got %v", name, args, err)
			}
		}
	}
}

// TestProjectResponse_SteerPauseResume_MalformedFailsLoud — AC-4:
// malformed JSON is rejected with ErrInvalidDecision, never silent.
func TestProjectResponse_SteerPauseResume_MalformedFailsLoud(t *testing.T) {
	t.Parallel()
	for _, name := range []string{SteerTaskToolName, PauseTaskToolName, ResumeTaskToolName} {
		_, err := projectResponse(llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{{ID: "x", Name: name, Args: json.RawMessage(`{"task_id":`)}},
		}, &planner.RunContext{}, false)
		if !errors.Is(err, planner.ErrInvalidDecision) {
			t.Fatalf("%s malformed: expected ErrInvalidDecision, got %v", name, err)
		}
	}
}

// TestProjectResponse_SteerPauseResume_StandaloneRejected — AC-5: each of
// the three controls rejects co-occurrence with ANY other tool-call (head
// or tail, spawn or catalog), on both parallel settings, naming the
// offending control.
func TestProjectResponse_SteerPauseResume_StandaloneRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		calls []llm.ToolCallStructured
		want  string
	}{
		{
			name: "steer_head_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "s1", Name: SteerTaskToolName, Args: json.RawMessage(`{"task_id":"x","directive":"d"}`)},
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
			},
			want: SteerTaskToolName,
		},
		{
			name: "pause_tail_with_tool",
			calls: []llm.ToolCallStructured{
				{ID: "t1", Name: "alpha", Args: json.RawMessage(`{}`)},
				{ID: "p1", Name: PauseTaskToolName, Args: json.RawMessage(`{"task_id":"x"}`)},
			},
			want: PauseTaskToolName,
		},
		{
			name: "resume_with_spawn",
			calls: []llm.ToolCallStructured{
				{ID: "r1", Name: ResumeTaskToolName, Args: json.RawMessage(`{"task_id":"x"}`)},
				{ID: "sp1", Name: SpawnTaskToolName, Args: json.RawMessage(`{"spec":{}}`)},
			},
			want: ResumeTaskToolName,
		},
		{
			name: "steer_with_pause",
			calls: []llm.ToolCallStructured{
				{ID: "s1", Name: SteerTaskToolName, Args: json.RawMessage(`{"task_id":"x","directive":"d"}`)},
				{ID: "p1", Name: PauseTaskToolName, Args: json.RawMessage(`{"task_id":"y"}`)},
			},
			want: SteerTaskToolName,
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
					t.Fatalf("error %q must name the offending control %q", err.Error(), tc.want)
				}
				if len(rc.PendingToolCalls) != 0 {
					t.Fatalf("PendingToolCalls must stay empty on reject, got %d", len(rc.PendingToolCalls))
				}
			})
		}
	}
}

// TestReservedDecl_SteerPauseResume_DeclaredWithSchemas — AC-3: the three
// new controls carry schemas pinning additionalProperties and the required
// fields, and descriptions that teach the descendant-only + operator-
// supremacy contract.
func TestReservedDecl_SteerPauseResume_DeclaredWithSchemas(t *testing.T) {
	t.Parallel()
	byName := map[string]llm.ToolDeclaration{}
	for _, d := range reservedPlannerControlDeclarations() {
		byName[d.Name] = d
	}

	steer := byName[SteerTaskToolName]
	if s := string(steer.Schema); !strings.Contains(s, `"required": ["task_id", "directive"]`) || !strings.Contains(s, `"additionalProperties": false`) {
		t.Fatalf("_steer_task schema must require task_id+directive with additionalProperties:false: %s", s)
	}
	pause := byName[PauseTaskToolName]
	if s := string(pause.Schema); !strings.Contains(s, `"required": ["task_id"]`) || !strings.Contains(s, `"additionalProperties": false`) {
		t.Fatalf("_pause_task schema must require task_id: %s", s)
	}
	resume := byName[ResumeTaskToolName]
	if s := string(resume.Schema); !strings.Contains(s, `"required": ["task_id"]`) || !strings.Contains(s, `"additionalProperties": false`) {
		t.Fatalf("_resume_task schema must require task_id: %s", s)
	}

	for name, d := range map[string]llm.ToolDeclaration{
		SteerTaskToolName:  steer,
		PauseTaskToolName:  pause,
		ResumeTaskToolName: resume,
	} {
		if !strings.Contains(d.Description, "OWN run") {
			t.Errorf("%s description must state own-run scope: %s", name, d.Description)
		}
		if !strings.Contains(strings.ToLower(d.Description), "operator") {
			t.Errorf("%s description must teach operator supremacy: %s", name, d.Description)
		}
		if !strings.Contains(strings.ToLower(d.Description), "alone") {
			t.Errorf("%s description must instruct it be sent alone: %s", name, d.Description)
		}
	}
}
