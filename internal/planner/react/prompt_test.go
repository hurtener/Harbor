package react

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// stubCatalog satisfies planner.ToolCatalogView for prompt-builder
// tests. The view exposes only the planner-relevant Tool surface
// (schemas + descriptions); no descriptors leak through.
type stubCatalog struct {
	tools []tools.Tool
}

func (s *stubCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range s.tools {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (s *stubCatalog) List() []tools.Tool { return s.tools }

// TestDefaultBuilder_EmitsSystemPromptAndCatalog asserts the system
// message contains both the supplied system prompt and the rendered
// tool catalog block (name + description per tool).
func TestDefaultBuilder_EmitsSystemPromptAndCatalog(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "test goal",
		Catalog: &stubCatalog{tools: []tools.Tool{
			{Name: "search", Description: "find things"},
			{Name: "answer", Description: "respond to user"},
		}},
	}
	req := defaultBuilder{}.Build(rc, "SYS_PROMPT")
	if len(req.Messages) == 0 {
		t.Fatal("Build returned zero messages")
	}
	sys := req.Messages[0]
	if sys.Role != llm.RoleSystem {
		t.Errorf("first message role = %q, want system", sys.Role)
	}
	if sys.Content.Text == nil {
		t.Fatal("system content text is nil")
	}
	body := *sys.Content.Text
	// An operator-supplied non-default prompt is honoured verbatim;
	// the <available_tools> section still appends so tool rendering
	// survives a custom base prompt (Phase 83a buildSystemContent
	// contract).
	for _, want := range []string{"SYS_PROMPT", "search", "find things", "answer", "respond to user", "<available_tools>"} {
		if !strings.Contains(body, want) {
			t.Errorf("system content missing %q. Body: %s", want, body)
		}
	}
}

// TestBuildSystemContent_FallsBackToDefaultSystemPrompt asserts that an
// empty system prompt argument substitutes the canonical default —
// i.e. the rendered ten-section structured prompt (Phase 107c — D-167
// deletes <output_format>, <action_schema>, <finishing>; adds
// <tool_discovery>).
func TestDefaultBuilder_FallsBackToDefaultSystemPrompt(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{Goal: "g"}
	req := defaultBuilder{}.Build(rc, "")
	if len(req.Messages) == 0 {
		t.Fatal("Build returned zero messages")
	}
	body := *req.Messages[0].Content.Text
	// The structured default opens with the <identity> section.
	if !strings.Contains(body, "<identity>") || !strings.Contains(body, "<tool_discovery>") {
		t.Errorf("structured default system prompt not used. Body: %s", body)
	}
}

// TestDefaultBuilder_UserMessagePrefersGoalOverQuery asserts the user
// block reads Goal first; falls back to Query when Goal is empty;
// falls back to a marker when both are empty.
func TestDefaultBuilder_UserMessagePrefersGoalOverQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rc      planner.RunContext
		wantSub string
	}{
		{"goal", planner.RunContext{Goal: "find foo", Query: "find bar"}, "find foo"},
		{"query fallback", planner.RunContext{Query: "only query"}, "only query"},
		{"both empty", planner.RunContext{}, "no goal supplied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := defaultBuilder{}.Build(c.rc, "sys")
			if len(req.Messages) < 2 {
				t.Fatalf("Build returned %d messages, want ≥ 2", len(req.Messages))
			}
			user := req.Messages[1]
			if user.Role != llm.RoleUser {
				t.Errorf("second message role = %q, want user", user.Role)
			}
			if !strings.Contains(*user.Content.Text, c.wantSub) {
				t.Errorf("user content missing %q. Body: %s", c.wantSub, *user.Content.Text)
			}
		})
	}
}

// TestDefaultBuilder_RendersTrajectoryStepsAsAssistantUserPairs
// asserts the Phase 107c (D-167) native tool-calling replay: each
// completed CallTool Step renders as an assistant message carrying a
// `ToolCalls` block + a RoleTool message carrying the matching
// `ToolCallID` and the observation as Content. The brief-07 user-role
// observation string convention is REPLACED on the native path.
func TestDefaultBuilder_RendersTrajectoryStepsAsAssistantToolPairs(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "find stuff",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action: planner.CallTool{
						Tool:   "search",
						Args:   json.RawMessage(`{"q":"foo"}`),
						CallID: "call_aaa",
					},
					LLMObservation: "found 3 hits",
				},
				{
					Action:         planner.CallTool{Tool: "summarize"},
					LLMObservation: "summary text",
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	// Expect: system, user (goal), assistant (step1 ToolCalls),
	// tool (step1 ToolCallID + observation), assistant (step2 ToolCalls),
	// tool (step2 ToolCallID + observation) → 6 messages.
	if len(req.Messages) != 6 {
		t.Fatalf("len(Messages) = %d, want 6 — messages: %+v", len(req.Messages), summariseRoles(req.Messages))
	}
	wantRoles := []llm.Role{
		llm.RoleSystem,
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleTool,
		llm.RoleAssistant,
		llm.RoleTool,
	}
	for i, w := range wantRoles {
		if req.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, w)
		}
	}
	// Step 1 assistant carries the provider-supplied call id verbatim.
	asst1 := req.Messages[2]
	if len(asst1.ToolCalls) != 1 || asst1.ToolCalls[0].ID != "call_aaa" || asst1.ToolCalls[0].Name != "search" {
		t.Errorf("step-1 assistant ToolCalls = %+v, want [{ID: call_aaa, Name: search}]", asst1.ToolCalls)
	}
	tool1 := req.Messages[3]
	if tool1.ToolCallID == nil || *tool1.ToolCallID != "call_aaa" {
		t.Errorf("step-1 tool ToolCallID = %v, want call_aaa", tool1.ToolCallID)
	}
	if !strings.Contains(*tool1.Content.Text, "found 3 hits") {
		t.Errorf("step-1 tool observation missing: %s", *tool1.Content.Text)
	}
	// Step 2 has no provider CallID — the renderer synthesises
	// `react.callid.<idx>` and stamps it on both messages.
	asst2 := req.Messages[4]
	if len(asst2.ToolCalls) != 1 || asst2.ToolCalls[0].ID != "react.callid.1" || asst2.ToolCalls[0].Name != "summarize" {
		t.Errorf("step-2 assistant ToolCalls = %+v, want synthetic id react.callid.1", asst2.ToolCalls)
	}
	tool2 := req.Messages[5]
	if tool2.ToolCallID == nil || *tool2.ToolCallID != "react.callid.1" {
		t.Errorf("step-2 tool ToolCallID = %v, want react.callid.1", tool2.ToolCallID)
	}
}

// TestDefaultBuilder_RendersSpawnAwaitStepsAsNativeToolPairs — Phase
// 107e (D-170): a SpawnTask / AwaitTask trajectory step replays as a
// native tool_call (`_spawn_task` / `_await_task`) + a matching RoleTool
// message, consistent with the CallTool path — NOT the legacy
// `{"action":"planner.SpawnTask"}` text marker that dropped the args.
// The spawn's args (the sub-goal query) survive the round-trip so the
// model can correlate a returned task_id with what it spawned.
func TestDefaultBuilder_RendersSpawnAwaitStepsAsNativeToolPairs(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "coordinate sub-goals",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action: planner.SpawnTask{
						Kind: tasks.KindBackground,
						Spec: planner.SpawnSpec{Query: "research sub-question A"},
					},
					LLMObservation: map[string]any{"task_id": "task-A", "kind": "background", "status": "spawned"},
				},
				{
					Action:         planner.AwaitTask{TaskID: tasks.TaskID("task-A")},
					LLMObservation: map[string]any{"task_id": "task-A", "status": "complete", "result": map[string]any{"answer": "A done"}},
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	// system, user (goal), assistant (spawn tool_call), tool (spawn obs),
	// assistant (await tool_call), tool (await obs) → 6 messages.
	wantRoles := []llm.Role{
		llm.RoleSystem, llm.RoleUser,
		llm.RoleAssistant, llm.RoleTool,
		llm.RoleAssistant, llm.RoleTool,
	}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("len(Messages) = %d, want %d — roles: %+v", len(req.Messages), len(wantRoles), summariseRoles(req.Messages))
	}
	for i, w := range wantRoles {
		if req.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, w)
		}
	}

	// Spawn step: assistant emits a native _spawn_task tool_call whose
	// args preserve the sub-goal query; the RoleTool carries the task_id.
	spawnAsst := req.Messages[2]
	if len(spawnAsst.ToolCalls) != 1 || spawnAsst.ToolCalls[0].Name != SpawnTaskToolName {
		t.Fatalf("spawn assistant ToolCalls = %+v, want one %q call", spawnAsst.ToolCalls, SpawnTaskToolName)
	}
	if !strings.Contains(string(spawnAsst.ToolCalls[0].Args), "research sub-question A") {
		t.Errorf("spawn replay dropped the sub-goal query: args = %s", spawnAsst.ToolCalls[0].Args)
	}
	spawnTool := req.Messages[3]
	if spawnTool.ToolCallID == nil || *spawnTool.ToolCallID != spawnAsst.ToolCalls[0].ID {
		t.Errorf("spawn tool ToolCallID = %v, want match to assistant id %q", spawnTool.ToolCallID, spawnAsst.ToolCalls[0].ID)
	}
	if spawnTool.Content.Text == nil || !strings.Contains(*spawnTool.Content.Text, "task-A") {
		t.Errorf("spawn observation missing task_id: %v", spawnTool.Content.Text)
	}

	// Await step: native _await_task tool_call + matching RoleTool.
	awaitAsst := req.Messages[4]
	if len(awaitAsst.ToolCalls) != 1 || awaitAsst.ToolCalls[0].Name != AwaitTaskToolName {
		t.Fatalf("await assistant ToolCalls = %+v, want one %q call", awaitAsst.ToolCalls, AwaitTaskToolName)
	}
	if !strings.Contains(string(awaitAsst.ToolCalls[0].Args), "task-A") {
		t.Errorf("await replay dropped the task_id: args = %s", awaitAsst.ToolCalls[0].Args)
	}
	awaitTool := req.Messages[5]
	if awaitTool.ToolCallID == nil || *awaitTool.ToolCallID != awaitAsst.ToolCalls[0].ID {
		t.Errorf("await tool ToolCallID = %v, want match to assistant id %q", awaitTool.ToolCallID, awaitAsst.ToolCalls[0].ID)
	}
}

// TestDefaultBuilder_RendersTaskStatusCancelStepsAsNativeToolPairs —
// AC-12: a trajectory step whose Action is TaskStatusQuery / CancelTask
// replays as a native tool_call + RoleTool pair, mirroring the
// _spawn_task / _await_task replay.
func TestDefaultBuilder_RendersTaskStatusCancelStepsAsNativeToolPairs(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "manage fan-out",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action:         planner.TaskStatusQuery{TaskIDs: []tasks.TaskID{"task-A", "task-B"}},
					LLMObservation: map[string]any{"tasks": []any{map[string]any{"task_id": "task-A", "status": "running"}}},
				},
				{
					Action:         planner.CancelTask{TaskID: tasks.TaskID("task-B"), Reason: "lost the race"},
					LLMObservation: map[string]any{"task_id": "task-B", "cancelled": true},
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	wantRoles := []llm.Role{
		llm.RoleSystem, llm.RoleUser,
		llm.RoleAssistant, llm.RoleTool,
		llm.RoleAssistant, llm.RoleTool,
	}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("len(Messages) = %d, want %d — roles: %+v", len(req.Messages), len(wantRoles), summariseRoles(req.Messages))
	}
	for i, w := range wantRoles {
		if req.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, w)
		}
	}

	// Status step: assistant emits a native _task_status tool_call whose
	// args preserve the queried ids; the RoleTool carries the observation.
	statusAsst := req.Messages[2]
	if len(statusAsst.ToolCalls) != 1 || statusAsst.ToolCalls[0].Name != TaskStatusToolName {
		t.Fatalf("status assistant ToolCalls = %+v, want one %q call", statusAsst.ToolCalls, TaskStatusToolName)
	}
	if !strings.Contains(string(statusAsst.ToolCalls[0].Args), "task-A") {
		t.Errorf("status replay dropped the queried ids: args = %s", statusAsst.ToolCalls[0].Args)
	}
	statusTool := req.Messages[3]
	if statusTool.ToolCallID == nil || *statusTool.ToolCallID != statusAsst.ToolCalls[0].ID {
		t.Errorf("status tool ToolCallID = %v, want match to assistant id %q", statusTool.ToolCallID, statusAsst.ToolCalls[0].ID)
	}

	// Cancel step: native _cancel_task tool_call + matching RoleTool.
	cancelAsst := req.Messages[4]
	if len(cancelAsst.ToolCalls) != 1 || cancelAsst.ToolCalls[0].Name != CancelTaskToolName {
		t.Fatalf("cancel assistant ToolCalls = %+v, want one %q call", cancelAsst.ToolCalls, CancelTaskToolName)
	}
	if !strings.Contains(string(cancelAsst.ToolCalls[0].Args), "task-B") || !strings.Contains(string(cancelAsst.ToolCalls[0].Args), "lost the race") {
		t.Errorf("cancel replay dropped task_id/reason: args = %s", cancelAsst.ToolCalls[0].Args)
	}
	cancelTool := req.Messages[5]
	if cancelTool.ToolCallID == nil || *cancelTool.ToolCallID != cancelAsst.ToolCalls[0].ID {
		t.Errorf("cancel tool ToolCallID = %v, want match to assistant id %q", cancelTool.ToolCallID, cancelAsst.ToolCalls[0].ID)
	}
}

// TestDefaultBuilder_RendersSteerPauseResumeStepsAsNativeToolPairs —
// AC-11: a trajectory step whose Action is SteerTask / PauseTask /
// ResumeTask replays as a native tool_call + RoleTool pair, mirroring the
// _task_status / _cancel_task replay, preserving the task ids and
// directives/reasons the model emitted.
func TestDefaultBuilder_RendersSteerPauseResumeStepsAsNativeToolPairs(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "manage fan-out",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action:         planner.SteerTask{TaskID: tasks.TaskID("task-A"), Directive: "focus on auth"},
					LLMObservation: map[string]any{"task_id": "task-A", "steered": true},
				},
				{
					Action:         planner.PauseTask{TaskID: tasks.TaskID("task-B"), Reason: "hold"},
					LLMObservation: map[string]any{"task_id": "task-B", "paused": true},
				},
				{
					Action:         planner.ResumeTask{TaskID: tasks.TaskID("task-B"), Directive: "carry on"},
					LLMObservation: map[string]any{"task_id": "task-B", "resumed": true},
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	wantRoles := []llm.Role{
		llm.RoleSystem, llm.RoleUser,
		llm.RoleAssistant, llm.RoleTool,
		llm.RoleAssistant, llm.RoleTool,
		llm.RoleAssistant, llm.RoleTool,
	}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("len(Messages) = %d, want %d — roles: %+v", len(req.Messages), len(wantRoles), summariseRoles(req.Messages))
	}
	for i, w := range wantRoles {
		if req.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, w)
		}
	}

	steerAsst := req.Messages[2]
	if len(steerAsst.ToolCalls) != 1 || steerAsst.ToolCalls[0].Name != SteerTaskToolName {
		t.Fatalf("steer assistant ToolCalls = %+v, want one %q call", steerAsst.ToolCalls, SteerTaskToolName)
	}
	if !strings.Contains(string(steerAsst.ToolCalls[0].Args), "task-A") || !strings.Contains(string(steerAsst.ToolCalls[0].Args), "focus on auth") {
		t.Errorf("steer replay dropped task_id/directive: args = %s", steerAsst.ToolCalls[0].Args)
	}
	if m := req.Messages[3]; m.ToolCallID == nil || *m.ToolCallID != steerAsst.ToolCalls[0].ID {
		t.Errorf("steer tool ToolCallID = %v, want match to assistant id %q", m.ToolCallID, steerAsst.ToolCalls[0].ID)
	}

	pauseAsst := req.Messages[4]
	if len(pauseAsst.ToolCalls) != 1 || pauseAsst.ToolCalls[0].Name != PauseTaskToolName {
		t.Fatalf("pause assistant ToolCalls = %+v, want one %q call", pauseAsst.ToolCalls, PauseTaskToolName)
	}
	if !strings.Contains(string(pauseAsst.ToolCalls[0].Args), "task-B") || !strings.Contains(string(pauseAsst.ToolCalls[0].Args), "hold") {
		t.Errorf("pause replay dropped task_id/reason: args = %s", pauseAsst.ToolCalls[0].Args)
	}

	resumeAsst := req.Messages[6]
	if len(resumeAsst.ToolCalls) != 1 || resumeAsst.ToolCalls[0].Name != ResumeTaskToolName {
		t.Fatalf("resume assistant ToolCalls = %+v, want one %q call", resumeAsst.ToolCalls, ResumeTaskToolName)
	}
	if !strings.Contains(string(resumeAsst.ToolCalls[0].Args), "task-B") || !strings.Contains(string(resumeAsst.ToolCalls[0].Args), "carry on") {
		t.Errorf("resume replay dropped task_id/directive: args = %s", resumeAsst.ToolCalls[0].Args)
	}
	if m := req.Messages[7]; m.ToolCallID == nil || *m.ToolCallID != resumeAsst.ToolCalls[0].ID {
		t.Errorf("resume tool ToolCallID = %v, want match to assistant id %q", m.ToolCallID, resumeAsst.ToolCalls[0].ID)
	}
}

// TestDefaultBuilder_PrefersLLMObservationOverRawObservation asserts
// D-026 heavy-content discipline: the prompt uses LLMObservation
// (compressed/redacted projection) over raw Observation when both
// are set.
func TestDefaultBuilder_PrefersLLMObservationOverRawObservation(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "g",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action:         planner.CallTool{Tool: "t"},
					Observation:    map[string]any{"raw": "heavy_blob"},
					LLMObservation: "compressed text",
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	// Last message is the RoleTool tool-result message (Phase 107c).
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleTool {
		t.Fatalf("last role = %q, want tool", last.Role)
	}
	body := *last.Content.Text
	if !strings.Contains(body, "compressed text") {
		t.Errorf("body missing LLMObservation: %s", body)
	}
	if strings.Contains(body, "heavy_blob") {
		t.Errorf("body leaked raw Observation: %s", body)
	}
}

// TestDefaultBuilder_RendersErrorAndFailureFirst asserts errors /
// failures take precedence over observations in the prompt (the
// planner needs to see failures to course-correct).
func TestDefaultBuilder_RendersErrorAndFailureFirst(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "g",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action: planner.CallTool{Tool: "t"},
					Error:  "boom",
				},
				{
					Action: planner.CallTool{Tool: "t2"},
					Failure: &planner.FailureRecord{
						Code:    "schema_repair_exhausted",
						Message: "bad shape",
					},
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	// Step1 obs at index 3, step2 obs at index 5 — RoleTool messages
	// under the Phase 107c native renderer.
	if !strings.Contains(*req.Messages[3].Content.Text, "Tool error: boom") {
		t.Errorf("step1 obs missing error: %s", *req.Messages[3].Content.Text)
	}
	if !strings.Contains(*req.Messages[5].Content.Text, "Tool failure: schema_repair_exhausted") {
		t.Errorf("step2 obs missing failure: %s", *req.Messages[5].Content.Text)
	}
}

// TestDefaultBuilder_RendersSummary asserts that a non-nil
// Trajectory.Summary appears in the user prompt (Phase 46 populates;
// Phase 45 consumes the read path).
func TestDefaultBuilder_RendersSummary(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "g",
		Trajectory: &planner.Trajectory{
			Summary: &planner.Summary{
				Goals:            []string{"goal1"},
				Facts:            []string{"fact1", "fact2"},
				Pending:          []string{"task1"},
				LastOutputDigest: "digest text",
				Note:             "compressed at step 4",
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	user := *req.Messages[1].Content.Text
	for _, want := range []string{"goal1", "fact1", "fact2", "task1", "digest text", "compressed at step 4"} {
		if !strings.Contains(user, want) {
			t.Errorf("user content missing summary segment %q. Body: %s", want, user)
		}
	}
}

// TestDefaultBuilder_RendersBackgroundResults asserts D-032 push-wake
// read path: resolved background outcomes appear as a trailing user
// message.
func TestDefaultBuilder_RendersBackgroundResults(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "g",
		Trajectory: &planner.Trajectory{
			Background: map[string]planner.BackgroundResult{
				"grp-1": {
					GroupID:    "grp-1",
					Status:     "completed",
					ResolvedAt: time.Now(),
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("last role = %q, want user", last.Role)
	}
	if !strings.Contains(*last.Content.Text, "background_resolved") {
		t.Errorf("background block missing: %s", *last.Content.Text)
	}
}

// TestDefaultBuilder_WithSummary_SkipsStepHistory is the Phase 46
// contract assertion (D-055): when rc.Trajectory.Summary is non-nil,
// the builder MUST NOT render per-step assistant/user pairs. The
// summary block in the user content IS the trajectory representation;
// rendering both would double-count tokens and defeat the
// compression. Brief 02 §4: "The compressed digest replaces the raw
// step history in subsequent prompt builds."
//
// Test shape: a trajectory carries 3 Steps AND a non-nil Summary.
// Expected messages: [system, user (goal + summary)]. No assistant
// messages from the step loop.
func TestDefaultBuilder_WithSummary_SkipsStepHistory(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "test",
		Trajectory: &planner.Trajectory{
			Summary: &planner.Summary{
				Goals: []string{"reach goal"},
				Note:  "compacted by Phase 46 runner",
			},
			Steps: []planner.Step{
				{
					Action:         planner.CallTool{Tool: "search", Args: json.RawMessage(`{"q":"foo"}`)},
					LLMObservation: "found 3 hits — heavy raw text that would normally render",
				},
				{
					Action:         planner.CallTool{Tool: "summarize"},
					LLMObservation: "summary text",
				},
				{
					Action:         planner.CallTool{Tool: "verify"},
					LLMObservation: "verified",
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")

	// Count assistant messages — Phase 46 contract: ZERO when Summary
	// is non-nil.
	asstCount := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			asstCount++
		}
	}
	if asstCount != 0 {
		t.Errorf("Phase 46 contract: assistant message count = %d, want 0 (Summary present must skip step history)", asstCount)
	}

	// Confirm the summary IS in the user content — the planner still
	// observes the compacted view.
	userMsg := req.Messages[1]
	if userMsg.Role != llm.RoleUser {
		t.Fatalf("Messages[1].Role = %q, want user", userMsg.Role)
	}
	body := *userMsg.Content.Text
	for _, want := range []string{"reach goal", "compacted by Phase 46 runner"} {
		if !strings.Contains(body, want) {
			t.Errorf("user content missing summary segment %q. Body: %s", want, body)
		}
	}

	// Confirm raw step history did NOT leak through.
	for _, leak := range []string{"found 3 hits", `"tool":"search"`, `"tool":"summarize"`, `"tool":"verify"`} {
		for _, m := range req.Messages {
			if m.Content.Text != nil && strings.Contains(*m.Content.Text, leak) {
				t.Errorf("Phase 46 contract: step-history fragment %q leaked into prompt despite non-nil Summary", leak)
				break
			}
		}
	}
}

// TestDefaultBuilder_NoSummary_RendersStepHistory is the regression
// guard: when Summary == nil, the builder must STILL render the
// per-step assistant/user pairs (the Phase 45 V1 minimum-viable
// shape). The Phase 46 swap is conditional on Summary != nil; the
// nil branch must not regress.
func TestDefaultBuilder_NoSummary_RendersStepHistory(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "test",
		Trajectory: &planner.Trajectory{
			Summary: nil, // explicit
			Steps: []planner.Step{
				{
					Action:         planner.CallTool{Tool: "search", Args: json.RawMessage(`{"q":"foo"}`)},
					LLMObservation: "found 3 hits",
				},
			},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")

	asstCount := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			asstCount++
		}
	}
	if asstCount != 1 {
		t.Errorf("Summary==nil regression: assistant message count = %d, want 1 (step history must render)", asstCount)
	}
}

// TestDefaultBuilder_NilCatalogProducesNoToolsBlock asserts the
// prompt builder handles nil catalog defensively.
func TestDefaultBuilder_NilCatalogProducesNoToolsBlock(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{Goal: "g"} // Catalog == nil
	req := defaultBuilder{}.Build(rc, "sys")
	body := *req.Messages[0].Content.Text
	if !strings.Contains(body, "no tools registered") {
		t.Errorf("expected 'no tools registered' marker. Body: %s", body)
	}
}

// summariseRoles returns the role sequence in a slice. Used by test
// failure messages for clarity.
func summariseRoles(msgs []llm.ChatMessage) []llm.Role {
	out := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// TestOneLine_CollapsesWhitespace asserts the small helper handles
// newlines + tabs deterministically.
func TestOneLine_CollapsesWhitespace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"a b", "a b"},
		{"a\nb", "a b"},
		{"a\r\nb", "a  b"},
		{"  trimmed  ", "trimmed"},
	}
	for _, c := range cases {
		if got := oneLine(c.in); got != c.want {
			t.Errorf("oneLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderAny_HandlesShapesSafely asserts the renderer doesn't
// leak raw bytes and falls through cleanly on unmarshalable types.
func TestRenderAny_HandlesShapesSafely(t *testing.T) {
	t.Parallel()
	if got := renderAny(nil); got != "(nil)" {
		t.Errorf("nil = %q, want (nil)", got)
	}
	if got := renderAny([]byte("secret bytes")); !strings.Contains(got, "raw bytes") {
		t.Errorf("[]byte path: %s", got)
	}
	if got := renderAny("hello\nworld"); got != "hello world" {
		t.Errorf("string: %s", got)
	}
	if got := renderAny(json.RawMessage(`{"a":1}`)); got != `{"a":1}` {
		t.Errorf("RawMessage: %s", got)
	}
	if got := renderAny(map[string]any{"k": "v"}); got != `{"k":"v"}` {
		t.Errorf("map: %s", got)
	}
}

// ----------------------------------------------------------------------------
// Phase 83a — twelve-section structured prompt (brief 13 §2.1).
// ----------------------------------------------------------------------------

// The ten XML section tags in their Phase 107c (D-167) fixed order.
// `<output_format>`, `<action_schema>`, `<finishing>`, and
// `<parallel_execution>` are deleted; `<tool_discovery>` +
// `<heavy_results>` replace them. `<available_tools>` renders
// name+description only — schemas live in req.Tools[].
var section83aTags = []string{
	"identity",
	"tool_discovery",
	"heavy_results",
	"tool_usage",
	"reasoning",
	"tone",
	"error_handling",
	"available_tools",
	"additional_guidance",
	"planning_constraints",
}

// renderDefaultSystem renders the default (structured) system prompt
// for a given builder + RunContext. Helper for the section tests.
func renderDefaultSystem(t *testing.T, b defaultBuilder, rc planner.RunContext) string {
	t.Helper()
	req := b.Build(rc, "")
	if len(req.Messages) == 0 || req.Messages[0].Content.Text == nil {
		t.Fatal("Build produced no system message")
	}
	return *req.Messages[0].Content.Text
}

// TestBuildSystemContent_NineSectionsAlwaysPresentInOrder asserts the
// eight always-on sections render exactly once each, in the Phase
// 107c (D-167) fixed order, separated by a blank line. The remaining
// two (additional_guidance, planning_constraints) are conditional.
func TestBuildSystemContent_NineSectionsAlwaysPresentInOrder(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})

	// The eight always-on sections (9 + 10 are conditional).
	alwaysOn := section83aTags[:8]
	lastIdx := -1
	for _, tag := range alwaysOn {
		opener := "<" + tag + ">"
		if got := strings.Count(body, opener); got != 1 {
			t.Errorf("section %s opener count = %d, want 1", opener, got)
		}
		idx := strings.Index(body, opener)
		if idx <= lastIdx {
			t.Errorf("section %s out of order (idx=%d, prev=%d)", opener, idx, lastIdx)
		}
		lastIdx = idx
		if !strings.Contains(body, "</"+tag+">") {
			t.Errorf("section %s missing closer", tag)
		}
	}
}

// TestBuildSystemContent_SectionsSeparatedByBlankLine asserts the
// sections are joined by `\n\n` (acceptance criterion).
func TestBuildSystemContent_SectionsSeparatedByBlankLine(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	if !strings.Contains(body, "</identity>\n\n<tool_discovery>") {
		t.Errorf("sections not separated by a blank line. Body:\n%s", body)
	}
}

// TestBuildSystemContent_OmitsEmptyOptionalSections asserts that with
// no extra_guidance and no planning hints, the <additional_guidance>
// and <planning_constraints> sections are omitted ENTIRELY — not
// emitted as empty tag pairs.
func TestBuildSystemContent_OmitsEmptyOptionalSections(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	for _, tag := range []string{"additional_guidance", "planning_constraints"} {
		if strings.Contains(body, "<"+tag+">") {
			t.Errorf("empty optional section <%s> should be omitted, but it is present", tag)
		}
	}
}

// TestBuildSystemContent_HeavyResultsTeachesArtifactFetch asserts the
// always-on <heavy_results> section names `artifact_fetch` + the
// reference-handle shape + the re-call anti-pattern pre-emption, and
// does NOT leak wrapper-shape terminology (which would prime the
// model on the internal projection).
func TestBuildSystemContent_HeavyResultsTeachesArtifactFetch(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})

	start := strings.Index(body, "<heavy_results>")
	end := strings.Index(body, "</heavy_results>")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("<heavy_results> section missing or malformed. Body:\n%s", body)
	}
	section := body[start:end]

	// The section MUST name the meta-tool, the reference-handle shape,
	// and the re-call anti-pattern pre-emption.
	for _, want := range []string{
		"artifact_fetch",
		`ref="`,
		"Re-calling the upstream tool",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("<heavy_results> missing %q. Section:\n%s", want, section)
		}
	}

	// No wrapper-shape terminology leaks into the section copy.
	for _, forbidden := range []string{
		`"artifact_ref"`,
		`"preview"`,
		"ArtifactStub",
	} {
		if strings.Contains(section, forbidden) {
			t.Errorf("<heavy_results> leaks wrapper terminology %q. Section:\n%s", forbidden, section)
		}
	}
}

// TestBuildSystemContent_HeavyResultsCarriesThePagingRuleAndTheBinaryCaveat
// pins the three facts the read path's contract obliges this block to
// state. The block is one of TWO model-facing descriptions of one tool
// (the other is the built-in's own WithDescription, rendered into
// <available_tools>), and the two must not disagree.
//
// The `offset` parameter had been shipped and undocumented here; the
// text-only admissibility rule and the `mime` discriminator land with it
// because a model that follows an auto-materialised attachment's fetch
// hint arrives at this tool holding binary.
func TestBuildSystemContent_HeavyResultsCarriesThePagingRuleAndTheBinaryCaveat(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})

	start := strings.Index(body, "<heavy_results>")
	end := strings.Index(body, "</heavy_results>")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("<heavy_results> section missing or malformed. Body:\n%s", body)
	}
	section := body[start:end]

	// The block states exactly the three facts its contract obliges and
	// no more. It is rendered on EVERY turn of EVERY run, so a fact that
	// belongs somewhere more just-in-time is paid for here forever: the
	// by-reference route is deliberately NOT restated here, because the
	// refusal message itself names it at the moment a model needs it,
	// and `<available_tools>` carries the tool's full description.
	for _, want := range []struct{ fact, substr string }{
		{"the offset parameter", "offset"},
		{"the concrete paging shape", "offset + returned_bytes"},
		{"the truncated re-call condition", "truncated is true"},
		{"the mime discriminator to read BEFORE calling", "Check the ref's mime first"},
		{"that a non-text artifact refuses", "refused with an error naming its MIME"},
		{"that the refusal is actionable", "how to reach those bytes instead"},
	} {
		if !strings.Contains(section, want.substr) {
			t.Errorf("<heavy_results> does not state %s (missing %q). Section:\n%s",
				want.fact, want.substr, section)
		}
	}

	// The stale phrasing is gone: it promised the whole payload and was
	// silent about paging, which stopped being true when `offset`
	// shipped.
	if strings.Contains(section, "retrieve the full payload") {
		t.Errorf("<heavy_results> still promises the full payload with no paging rule. Section:\n%s", section)
	}
}

// alwaysOnPromptBudgetBytes is a byte budget on the system prompt every
// run pays on every turn. It is not a style rule — it is a measured cost.
//
// The default prompt is assembled from scratch per planner step, and the
// step's allocation profile scales with its size at roughly 2x (the text
// is built and then copied into the request), so a byte added here is
// ~2 bytes of allocation on every step of every run, plus a token on
// every turn for every operator.
//
// This budget exists because that cost was learned the expensive way: a
// 765-byte prose addition to `<heavy_results>` moved
// BenchmarkReActPlanner_NextStep_DeclaredTool by +19% sec/op and +19% B/op with
// allocs/op UNCHANGED at 16 — the same allocations, each bigger — and
// tripped the CI perf gate. A package-level test is the cheaper place to
// find that than a benchmark gate on a pull request.
//
// Raising this number is allowed; doing it silently is not. If a section
// genuinely needs more room, raise the budget in the same commit and say
// what the prompt bought for the bytes.
const alwaysOnPromptBudgetBytes = 7000

// TestBuildSystemContent_AlwaysOnPromptStaysWithinItsByteBudget pins the
// cost of the prompt surface every turn carries.
func TestBuildSystemContent_AlwaysOnPromptStaysWithinItsByteBudget(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{})
	if got := len(body); got > alwaysOnPromptBudgetBytes {
		t.Errorf("the always-on system prompt is %d bytes, over its %d-byte budget by %d.\n"+
			"Every byte here is ~2 bytes of allocation on EVERY planner step and a token on "+
			"EVERY turn. Either tighten the wording, move the fact to a just-in-time surface "+
			"(a tool description, or the error message that needs it), or raise "+
			"alwaysOnPromptBudgetBytes in this commit and justify it.",
			got, alwaysOnPromptBudgetBytes, got-alwaysOnPromptBudgetBytes)
	}
}

// TestRenderObservationForLLM_CarriesTheErrorClass closes the loop the
// observation class exists for: the planner reads it as TEXT on its next
// turn. Asserted against the RENDERED string, not the struct, because
// the rendering is where a class that is merely persisted would be lost.
func TestRenderObservationForLLM_CarriesTheErrorClass(t *testing.T) {
	t.Parallel()

	classified := renderObservationForLLM(planner.Step{
		LLMObservation: map[string]any{
			"error":                     `tool "artifact_stats" invoke: artifact reference does not resolve under this run's scope: "abc"`,
			planner.ObservationClassKey: string(planner.ObservationClassArtifactRefNotFound),
		},
	})
	if !strings.Contains(classified, planner.ObservationClassKey) {
		t.Errorf("the rendered observation drops the class key:\n%s", classified)
	}
	if !strings.Contains(classified, string(planner.ObservationClassArtifactRefNotFound)) {
		t.Errorf("the rendered observation drops the class value:\n%s", classified)
	}

	// The unclassified leg is unchanged — no stray key on the hottest
	// path in the runtime.
	plain := renderObservationForLLM(planner.Step{
		LLMObservation: map[string]any{"error": "upstream 503"},
	})
	if strings.Contains(plain, planner.ObservationClassKey) {
		t.Errorf("a tool's own error rendered a class key:\n%s", plain)
	}
	if want := `Observation: {"error":"upstream 503"}`; plain != want {
		t.Errorf("plain render = %q, want the pre-phase %q", plain, want)
	}
}

// classifiedMCPFailureStep builds the trajectory step the run loop
// records for a classified MCP `IsError` failure (HA-54 amendment): a
// structured classified LLMObservation carrying the typed MCP class, the
// terminal policy projection, and the retained bounded result — and, for
// the masking-leg assertions, a GENERIC Step.Error stamp on top of it
// (the pre-amendment runloop shape, or a legacy persisted trajectory).
func classifiedMCPFailureStep() planner.Step {
	return planner.Step{
		Action: planner.CallTool{Tool: "mcp_conflict", CallID: "c1", Args: json.RawMessage(`{"document_id":"doc-1"}`)},
		Error:  "tool execution failed",
		LLMObservation: map[string]any{
			"error":                              `tool "mcp_conflict" invoke: mcp: server returned tool error (conflict): document changed; current revision is rev-42`,
			planner.ObservationMCPClassKey:       string(tools.MCPToolErrorConflict),
			planner.ObservationPolicyClassKey:    string(tools.ErrClassPermanent),
			planner.ObservationPolicyAttemptsKey: 1,
			planner.ObservationPolicyBudgetKey:   1,
			"result":                             map[string]any{"current_revision": "rev-42"},
		},
	}
}

// TestRenderNativeObservation_ClassifiedObservationWinsOverGenericError
// is the HA-54 masking gate on the NATIVE render path: a structured
// classified LLMObservation (MCP class / bounded message / retry
// outcome / bounded result) renders into the RoleTool body even when a
// generic Step.Error is stamped on the step — the generic string never
// masks the richer payload. Also asserts the raw args never leak into
// the observation body.
func TestRenderNativeObservation_ClassifiedObservationWinsOverGenericError(t *testing.T) {
	t.Parallel()
	body := renderNativeObservation(classifiedMCPFailureStep())

	for _, want := range []string{
		string(tools.MCPToolErrorConflict), // the typed provider class
		planner.ObservationMCPClassKey,     // the class key
		"document changed",                 // the bounded actionable message
		string(tools.ErrClassPermanent),    // the retryability projection
		"rev-42",                           // the retained bounded result
	} {
		if !strings.Contains(body, want) {
			t.Errorf("native observation drops %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "tool execution failed") {
		t.Errorf("the generic Step.Error masked the classified observation:\n%s", body)
	}
	if strings.Contains(body, "document_id") || strings.Contains(body, "doc-1") {
		t.Errorf("raw tool args leaked into the observation body:\n%s", body)
	}
}

// TestRenderObservationForLLM_ClassifiedObservationWinsOverGenericError
// is the same masking gate on the LEGACY render path (the non-native
// assistant-text fallback).
func TestRenderObservationForLLM_ClassifiedObservationWinsOverGenericError(t *testing.T) {
	t.Parallel()
	body := renderObservationForLLM(classifiedMCPFailureStep())

	if !strings.Contains(body, string(tools.MCPToolErrorConflict)) || !strings.Contains(body, "document changed") {
		t.Errorf("legacy observation drops the classified class/message:\n%s", body)
	}
	if !strings.Contains(body, "rev-42") {
		t.Errorf("legacy observation drops the retained bounded result:\n%s", body)
	}
	if strings.Contains(body, "tool execution failed") {
		t.Errorf("the generic Step.Error masked the classified observation:\n%s", body)
	}
}

// TestRenderParallelFallbackBody_ClassifiedObservationWinsOverGenericError
// is the same masking gate on the PARALLEL fallback path (a CallParallel
// step whose whole-call abort the runloop wrapped as a flat error map).
func TestRenderParallelFallbackBody_ClassifiedObservationWinsOverGenericError(t *testing.T) {
	t.Parallel()
	body := renderParallelFallbackBody(classifiedMCPFailureStep())

	if !strings.Contains(body, string(tools.MCPToolErrorConflict)) || !strings.Contains(body, "document changed") {
		t.Errorf("parallel fallback drops the classified class/message:\n%s", body)
	}
	if !strings.Contains(body, "rev-42") {
		t.Errorf("parallel fallback drops the retained bounded result:\n%s", body)
	}
	if strings.Contains(body, "tool execution failed") {
		t.Errorf("the generic Step.Error masked the classified observation:\n%s", body)
	}
}

// TestRenderNativeBatchStep_WholeCallAbortSurfacesClassifiedObservation
// is the BATCH fallback gate: a Batch step whose dispatch aborted
// whole-call carries no BatchObservation aggregate, so the renderer must
// fall back to the centralized failure-first precedence — the classified
// MCP observation reaches every RoleTool answer instead of being
// flattened to "(tool returned no observation)" — while every call_id is
// still answered exactly once (the provider wire invariant).
func TestRenderNativeBatchStep_WholeCallAbortSurfacesClassifiedObservation(t *testing.T) {
	t.Parallel()
	call := planner.Batch{
		Tools: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"a":1}`), CallID: "t_a"},
			{Tool: "beta", Args: json.RawMessage(`{"b":2}`), CallID: "t_b"},
		},
	}
	step := classifiedMCPFailureStep()
	step.Action = call
	// Deliberately NO BatchObservation: the whole-call abort shape.

	asst, toolMsgs := renderNativeBatchStep(step, call, planner.ReasoningReplayNever, 0)
	if len(asst.ToolCalls) != 2 || len(toolMsgs) != 2 {
		t.Fatalf("got %d tool_calls / %d tool msgs, want 2/2", len(asst.ToolCalls), len(toolMsgs))
	}
	for i, tm := range toolMsgs {
		if tm.Role != llm.RoleTool || tm.ToolCallID == nil || *tm.ToolCallID != asst.ToolCalls[i].ID {
			t.Errorf("tool msg[%d] role/ID = %q/%v, want RoleTool matched to %q", i, tm.Role, tm.ToolCallID, asst.ToolCalls[i].ID)
		}
		body := ""
		if tm.Content.Text != nil {
			body = *tm.Content.Text
		}
		if !strings.Contains(body, string(tools.MCPToolErrorConflict)) || !strings.Contains(body, "document changed") {
			t.Errorf("batch body[%d] drops the classified class/message:\n%s", i, body)
		}
		if !strings.Contains(body, "rev-42") {
			t.Errorf("batch body[%d] drops the retained bounded result:\n%s", i, body)
		}
		if strings.Contains(body, "tool execution failed") {
			t.Errorf("batch body[%d] rendered the generic error instead of the classified observation:\n%s", i, body)
		}
	}
}

// TestRenderNativeObservation_UnclassifiedErrorKeepsGenericFallback pins
// the acceptance-6 leg on the renderer: a legacy unstructured failure —
// a generic Step.Error with NO classified LLMObservation — still renders
// the generic safe fallback, byte-identical to the pre-amendment shape.
func TestRenderNativeObservation_UnclassifiedErrorKeepsGenericFallback(t *testing.T) {
	t.Parallel()
	step := planner.Step{
		Action:         planner.CallTool{Tool: "plain", CallID: "c1"},
		Error:          "tool execution failed",
		LLMObservation: map[string]any{"error": "tool blew up"},
	}
	if body := renderNativeObservation(step); body != "Tool error: tool execution failed" {
		t.Errorf("native render = %q, want the generic fallback %q", body, "Tool error: tool execution failed")
	}
	if body := renderObservationForLLM(step); body != "Observation (error): tool execution failed" {
		t.Errorf("legacy render = %q, want the generic fallback %q", body, "Observation (error): tool execution failed")
	}
}

// TestBuildSystemContent_RendersAdditionalGuidanceWhenSet asserts the
// <additional_guidance> section appears, wrapping the operator string
// verbatim, when extraGuidance is non-empty.
func TestBuildSystemContent_RendersAdditionalGuidanceWhenSet(t *testing.T) {
	t.Parallel()
	b := defaultBuilder{extraGuidance: "Speak like a pirate."}
	body := renderDefaultSystem(t, b, planner.RunContext{Goal: "g"})
	want := "<additional_guidance>\nSpeak like a pirate.\n</additional_guidance>"
	if !strings.Contains(body, want) {
		t.Errorf("expected verbatim additional_guidance block. Body:\n%s", body)
	}
}

// TestBuildSystemContent_WhitespaceOnlyExtraGuidanceOmitsSection
// asserts a whitespace-only extra guidance string omits the section
// (it is treated as empty).
func TestBuildSystemContent_WhitespaceOnlyExtraGuidanceOmitsSection(t *testing.T) {
	t.Parallel()
	b := defaultBuilder{extraGuidance: "   \n\t  "}
	body := renderDefaultSystem(t, b, planner.RunContext{Goal: "g"})
	if strings.Contains(body, "<additional_guidance>") {
		t.Errorf("whitespace-only extra guidance should omit the section")
	}
}

// TestBuildSystemContent_CurrentDateIsDateOnly asserts the <identity>
// section's `Current date:` line is YYYY-MM-DD with no time-of-day
// component (brief 13 §4 — date-only for KV-cache stability).
func TestBuildSystemContent_CurrentDateIsDateOnly(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	re := regexp.MustCompile(`Current date: (\S+)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no `Current date:` line found. Body:\n%s", body)
	}
	date := m[1]
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(date) {
		t.Errorf("current date %q is not YYYY-MM-DD", date)
	}
	for _, bad := range []string{"T", ":", " "} {
		if strings.Contains(date, bad) {
			t.Errorf("current date %q contains time-of-day marker %q", date, bad)
		}
	}
	// It must equal today's UTC date.
	if want := time.Now().UTC().Format("2006-01-02"); date != want {
		t.Errorf("current date = %q, want %q (UTC today)", date, want)
	}
}

// TestBuildSystemContent_NoReasoningFieldInActionSchema asserts the
// rendered prompt contains NO `"reasoning":` substring (acceptance
// criterion: the action JSON drops the reasoning field; brief 13
// §2.6).
func TestBuildSystemContent_NoReasoningFieldInActionSchema(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	if strings.Contains(body, `"reasoning":`) {
		t.Errorf("rendered prompt contains a `\"reasoning\":` JSON field — must be dropped (brief 13 §2.6)")
	}
	if strings.Contains(body, `"thought":`) {
		t.Errorf("rendered prompt contains a `\"thought\":` JSON field — must be dropped")
	}
}

// TestBuildSystemContent_ToneIntermediateStepClamp asserts the <tone>
// section's intermediate-step clamp matches the Phase 107c native
// tool-calling contract. The legacy "produce ONLY the JSON action
// object" + "thought/reasoning in the JSON" clamps were retired in
// Phase 107c (D-167 AC-20) — the new wire shape is native ToolCalls,
// not a JSON action envelope. The "Emit only tool calls" sibling
// bullet was also dropped: in native tool-calling, tool_calls and
// content live in separate channels and don't need a prompt clamp
// to keep them separate. The reasoning-channel guidance stays
// because Anthropic's `thinking` channel exists separately and the
// model shouldn't echo it.
func TestBuildSystemContent_ToneIntermediateStepClamp(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	if strings.Contains(body, "Emit only tool calls — keep any narration to the final answer turn.") {
		t.Errorf("<tone> regressed: re-introduced the 'Emit only tool calls' bullet")
	}
	if strings.Contains(body, "produce ONLY the JSON action object") {
		t.Errorf("<tone> still references the deleted JSON-action clamp — Phase 107c retired it")
	}
	if !strings.Contains(body, "Internal reasoning is captured automatically") {
		t.Errorf("<tone> missing intermediate-step reasoning guidance")
	}
}

// TestBuildSystemContent_FinishingCarriesOnlyAnswer asserts the
// <finishing> block reserves no rich-output JSON fields — no
// `"confidence"`, `"route"`, `"requires_followup"`, `"warnings"` keys
// (brief 13 §5 — rich output dropped from Harbor entirely).
func TestBuildSystemContent_FinishingCarriesOnlyAnswer(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	for _, field := range []string{`"confidence"`, `"route"`, `"requires_followup"`, `"warnings"`} {
		if strings.Contains(body, field) {
			t.Errorf("rendered prompt contains rich-output finish field %s — dropped per brief 13 §5", field)
		}
	}
	if strings.Contains(body, "optional fields you may include") {
		t.Errorf("rendered prompt describes optional finish fields — dropped per brief 13 §5")
	}
}

// TestBuildSystemContent_ErrorHandlingNoLegacyShape asserts the
// <error_handling> block (a) avoids the legacy `requires_followup`
// schema field that brief 13 §5 deleted, AND (b) avoids the deleted
// `_finish` / `args.answer` discriminator pair that Phase 107c (D-167
// AC-20) retired. Operators clarify via the terminal answer message
// under the native tool-calling contract.
func TestBuildSystemContent_ErrorHandlingNoLegacyShape(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	// Isolate the <error_handling> section body.
	start := strings.Index(body, "<error_handling>")
	end := strings.Index(body, "</error_handling>")
	if start < 0 || end < 0 {
		t.Fatal("no <error_handling> section")
	}
	section := body[start:end]
	if strings.Contains(section, "requires_followup") {
		t.Errorf("<error_handling> references requires_followup — brief 13 §5 deleted the field")
	}
	// The deleted Phase 107c shape — operator should guide via the
	// terminal answer message, NOT a JSON args.answer field.
	if strings.Contains(section, "args.answer") {
		t.Errorf("<error_handling> still references args.answer — Phase 107c AC-20 retired the field; use 'final answer' wording")
	}
	if strings.Contains(section, "_finish") {
		t.Errorf("<error_handling> still references _finish — Phase 107c AC-20 retired the discriminator")
	}
	if !strings.Contains(section, "final answer") {
		t.Errorf("<error_handling> should guide clarification via the user-visible final answer")
	}
}

// TestBuildSystemContent_AvailableToolsRendersCatalog asserts the
// <available_tools> section renders the catalog (name + description).
func TestBuildSystemContent_AvailableToolsRendersCatalog(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "g",
		Catalog: &stubCatalog{tools: []tools.Tool{
			{Name: "search", Description: "find things"},
		}},
	}
	body := renderDefaultSystem(t, defaultBuilder{}, rc)
	start := strings.Index(body, "<available_tools>")
	end := strings.Index(body, "</available_tools>")
	section := body[start:end]
	for _, want := range []string{"search", "find things"} {
		if !strings.Contains(section, want) {
			t.Errorf("<available_tools> missing %q. Section:\n%s", want, section)
		}
	}
}

// TestBuildSystemContent_OverrideHonouredVerbatim asserts a non-default
// WithSystemPrompt override REPLACES the structured sections, but the
// <available_tools> + <additional_guidance> injection sections still
// append.
func TestBuildSystemContent_OverrideHonouredVerbatim(t *testing.T) {
	t.Parallel()
	b := defaultBuilder{extraGuidance: "extra rules"}
	req := b.Build(planner.RunContext{Goal: "g"}, "MY CUSTOM PROMPT")
	body := *req.Messages[0].Content.Text
	if !strings.Contains(body, "MY CUSTOM PROMPT") {
		t.Errorf("override not honoured. Body:\n%s", body)
	}
	if strings.Contains(body, "<identity>") {
		t.Errorf("structured <identity> section leaked into an overridden prompt")
	}
	if !strings.Contains(body, "<available_tools>") {
		t.Errorf("<available_tools> should still append under an override")
	}
	if !strings.Contains(body, "<additional_guidance>\nextra rules\n</additional_guidance>") {
		t.Errorf("<additional_guidance> should still append under an override")
	}
}

// TestBuildSystemContent_NoUnresolvedTemplateMarkers asserts no `{{`
// template markers survive into the rendered prompt (catches an
// un-rendered placeholder regression).
func TestBuildSystemContent_NoUnresolvedTemplateMarkers(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{extraGuidance: "x"}, planner.RunContext{
		Goal: "g",
		Catalog: &stubCatalog{tools: []tools.Tool{
			{Name: "t", Description: "d"},
		}},
	})
	if strings.Contains(body, "{{") {
		t.Errorf("rendered prompt contains unresolved `{{` template marker. Body:\n%s", body)
	}
}

// TestDefaultBuilder_GoldenDefaultPrompt is the fixture-driven golden
// test (acceptance criterion): the rendered default prompt with no
// tools and no extra_guidance must match the checked-in fixture. The
// fixture *is* the normative spec. The volatile `Current date:` line
// is normalised to a sentinel before the compare so the test is
// date-independent.
func TestDefaultBuilder_GoldenDefaultPrompt(t *testing.T) {
	t.Parallel()
	const goldenPath = "testdata/golden_default_prompt.txt"
	got := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{})
	// Normalise the volatile date line to the fixture's sentinel.
	dateRE := regexp.MustCompile(`Current date: \d{4}-\d{2}-\d{2}`)
	gotNorm := dateRE.ReplaceAllString(got, "Current date: 2025-01-01")
	if os.Getenv("HARBOR_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(gotNorm), 0o644); err != nil {
			t.Fatalf("update golden fixture: %v", err)
		}
		t.Logf("regenerated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	if gotNorm != string(want) {
		t.Errorf("rendered default prompt diverged from %s.\n"+
			"If this change is intentional, regenerate the fixture with "+
			"HARBOR_UPDATE_GOLDEN=1 go test ./internal/planner/react/...\n"+
			"--- got ---\n%s\n--- want ---\n%s", goldenPath, gotNorm, string(want))
	}
}

// --- Phase 83b — tool schema injection (D-144) — NARROWED by
// Phase 107c (D-167) to name+description only; schemas live in the
// provider's native Tools[] declaration.
// -------------------------------------------------------------------

// fixtureCatalog returns the canonical two-tool catalog the Phase 83b
// golden fixture documents. Phase 107c (D-167) narrows the prompt-side
// rendering to name+description only — args_schema, side_effects, and
// examples are suppressed (they live in req.Tools[]). The fixture still
// carries the full Tool data so tests can assert the narrow rendering.
func fixtureCatalog() *stubCatalog {
	return &stubCatalog{tools: []tools.Tool{
		{
			Name:        "search",
			Description: "Search the knowledge base for documents matching a query.",
			ArgsSchema: json.RawMessage(
				`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
			SideEffects: tools.SideEffectRead,
			Examples: []tools.ToolExample{
				{
					Description: "broadest search",
					Args:        map[string]any{"query": "quarterly revenue"},
					Tags:        []string{"minimal"},
				},
				{
					Description: "bounded result set",
					Args:        map[string]any{"query": "revenue", "limit": 5},
					Tags:        []string{"common"},
				},
			},
		},
		{
			Name:        "ping",
			Description: "Health-check the upstream service.",
		},
	}}
}

// TestRenderToolNameDesc_RendersNameAndDescriptionOnly asserts
// Phase 107c (D-167) narrows the prompt-side tool block to name +
// description only — no args_schema, side_effects, or examples.
func TestRenderToolNameDesc_RendersNameAndDescriptionOnly(t *testing.T) {
	t.Parallel()
	out := renderToolNameDesc(tools.Tool{
		Name:        "search",
		Description: "find things",
		ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		SideEffects: tools.SideEffectRead,
		Examples:    []tools.ToolExample{{Description: "ex", Args: map[string]any{"q": "x"}}},
	})
	if !strings.Contains(out, "search") || !strings.Contains(out, "find things") {
		t.Errorf("tool block missing name/description: %s", out)
	}
	if strings.Contains(out, "args_schema") || strings.Contains(out, "side_effects") || strings.Contains(out, "examples:") {
		t.Errorf("Phase 107c: tool block leaks schema/side_effects/examples. Output:\n%s", out)
	}
}

// TestRenderToolNameDesc_NoDescriptionRendersNameOnly asserts a tool
// without description renders name only.
func TestRenderToolNameDesc_NoDescriptionRendersNameOnly(t *testing.T) {
	t.Parallel()
	out := renderToolNameDesc(tools.Tool{Name: "ping"})
	want := "- ping\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestRenderAvailableToolsSection_NameDescOnly asserts Phase 107c
// (D-167) — the section renders name+description per tool, no
// schemas/side_effects/examples.
func TestRenderAvailableToolsSection_NameDescOnly(t *testing.T) {
	t.Parallel()
	body := renderAvailableToolsSection(planner.RunContext{Catalog: fixtureCatalog()}, 0)
	for _, want := range []string{"search", "Search the knowledge base", "ping", "Health-check"} {
		if !strings.Contains(body, want) {
			t.Errorf("<available_tools> missing %q. Body:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"args_schema", "side_effects", "examples:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Phase 107c: <available_tools> leaks %q (should render name+desc only). Body:\n%s", forbidden, body)
		}
	}
}

// TestWithSystemPromptExtra_FlowsToAdditionalGuidance asserts the
// react.New + WithSystemPromptExtra Option injects content into the
// <additional_guidance> section of the rendered prompt.
func TestWithSystemPromptExtra_FlowsToAdditionalGuidance(t *testing.T) {
	t.Parallel()
	// The Option finalises the in-package builder; render through it.
	b := defaultBuilder{extraGuidance: "domain rule: always cite sources"}
	body := renderDefaultSystem(t, b, planner.RunContext{Goal: "g"})
	if !strings.Contains(body, "<additional_guidance>\ndomain rule: always cite sources\n</additional_guidance>") {
		t.Errorf("WithSystemPromptExtra content not in <additional_guidance>. Body:\n%s", body)
	}
}

// TestRenderNativeStepPair_AssistantPreambleReplayed asserts that
// the assistant message on trajectory replay carries the model's
// prior `AssistantPreamble` prose as its content, preserving the
// narrative thread across steps. The wiring: planner stamps
// `Step.AssistantPreamble` from `llm.CompleteResponse.Content`,
// runloop appends it on the Step, renderer reads it here.
func TestRenderNativeStepPair_AssistantPreambleReplayed(t *testing.T) {
	t.Parallel()

	step := planner.Step{
		Action: planner.CallTool{
			Tool:   "youtube_get_metadata",
			Args:   json.RawMessage(`{"url":"https://example.com"}`),
			CallID: "call_abc",
		},
		AssistantPreamble: "I'll fetch the metadata for that YouTube video to get its duration.",
	}
	asst, _, native := renderNativeStepPair(step, planner.ReasoningReplayNever, 0)
	if !native {
		t.Fatal("renderNativeStepPair returned native=false for a CallTool step")
	}
	if asst.Content.Text == nil {
		t.Fatal("assistant message has nil Content.Text")
	}
	got := *asst.Content.Text
	if got != "I'll fetch the metadata for that YouTube video to get its duration." {
		t.Errorf("assistant content lost preamble. got=%q", got)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Name != "youtube_get_metadata" {
		t.Errorf("tool_call lost or mangled: %+v", asst.ToolCalls)
	}
}

// TestRenderNativeStepPair_EmptyPreambleEmitsNoContent asserts that
// when the model emitted a tool_call with no preamble, the renderer
// produces an assistant message with zero-value Content (both Text
// and Parts nil) — NOT a pointer-to-empty-string. The bifrost
// translator emits `content: null` on the wire from this shape, per
// OpenAI's spec (which rejects `content: ""` with tool_calls
// present).
func TestRenderNativeStepPair_EmptyPreambleEmitsNoContent(t *testing.T) {
	t.Parallel()

	step := planner.Step{
		Action: planner.CallTool{
			Tool:   "youtube_get_metadata",
			Args:   json.RawMessage(`{"url":"https://example.com"}`),
			CallID: "call_xyz",
		},
		// AssistantPreamble intentionally empty.
	}
	asst, _, native := renderNativeStepPair(step, planner.ReasoningReplayNever, 0)
	if !native {
		t.Fatal("renderNativeStepPair returned native=false")
	}
	if asst.Content.Text != nil {
		t.Errorf("empty preamble should yield zero-value Content (Text=nil), got Text=%q", *asst.Content.Text)
	}
	if asst.Content.Parts != nil {
		t.Errorf("empty preamble should yield zero-value Content (Parts=nil), got %d parts", len(asst.Content.Parts))
	}
	if len(asst.ToolCalls) != 1 {
		t.Errorf("tool_call lost on empty-preamble path: %+v", asst.ToolCalls)
	}
}

// TestRenderNativeStepPair_ReasoningReplayLayersBelowPreamble asserts
// the reasoning-replay text mode (D-148) layers BELOW the preamble
// when both are present, so the natural-language intent reads first
// and the structured provider trace below.
func TestRenderNativeStepPair_ReasoningReplayLayersBelowPreamble(t *testing.T) {
	t.Parallel()

	step := planner.Step{
		Action: planner.CallTool{
			Tool:   "youtube_get_metadata",
			Args:   json.RawMessage(`{}`),
			CallID: "call_xyz",
		},
		AssistantPreamble: "I'll fetch the metadata.",
		ReasoningTrace:    "Step 1: identify the right tool. Step 2: dispatch.",
	}
	asst, _, _ := renderNativeStepPair(step, planner.ReasoningReplayText, 0)
	if asst.Content.Text == nil {
		t.Fatal("nil Content.Text")
	}
	got := *asst.Content.Text
	if !strings.Contains(got, "I'll fetch the metadata.") {
		t.Errorf("preamble missing: %q", got)
	}
	if !strings.Contains(got, "Reasoning:\nStep 1") {
		t.Errorf("reasoning trace missing: %q", got)
	}
	// Order: preamble FIRST, reasoning AFTER.
	if strings.Index(got, "I'll fetch") > strings.Index(got, "Reasoning:") {
		t.Errorf("order wrong — preamble should precede reasoning: %q", got)
	}
}

// TestRenderNativeObservation_ArtifactStubInlinesPreview asserts the
// heavy-content projection: when the trajectory step's observation
// is either an `*llm.ArtifactStub` or the runtime tool-executor's
// `HeavyTruncationSummary` map, the RoleTool message body is the
// inlined preview text + a positional `artifact_fetch` footer —
// NEVER the wrapper JSON. No wrapper terminology leaks into the
// LLM-facing body.
func TestRenderNativeObservation_ArtifactStubInlinesPreview(t *testing.T) {
	t.Parallel()

	// Sub-test 1: the runtime tool-executor's map shape. This is what
	// `internal/runtime/dispatch.HeavyTruncationSummary` returns
	// (Phase 110a). The renderer MUST inline `preview` as the body.
	t.Run("executor_map_shape", func(t *testing.T) {
		t.Parallel()
		rc := planner.RunContext{
			Goal: "fetch metadata",
			Trajectory: &planner.Trajectory{
				Steps: []planner.Step{{
					Action: planner.CallTool{
						Tool:   "youtube_get_metadata",
						Args:   json.RawMessage(`{"url":"https://example.com/x"}`),
						CallID: "call_y1",
					},
					LLMObservation: map[string]any{
						"tool":         "youtube_get_metadata",
						"size_bytes":   8192,
						"truncated":    true,
						"preview":      `{"title":"Example","duration":"3:42"`,
						"artifact_ref": "youtube_meta_abc123",
					},
				}},
			},
		}
		req := defaultBuilder{}.Build(rc, "sys")
		last := req.Messages[len(req.Messages)-1]
		if last.Role != llm.RoleTool {
			t.Fatalf("last role = %q, want tool", last.Role)
		}
		body := *last.Content.Text
		// The preview text MUST be the body.
		if !strings.Contains(body, `{"title":"Example"`) {
			t.Errorf("body missing preview text: %s", body)
		}
		// The fetch hint MUST name artifact_fetch + the ref + the size.
		if !strings.Contains(body, "artifact_fetch") {
			t.Errorf("body missing artifact_fetch footer: %s", body)
		}
		if !strings.Contains(body, "youtube_meta_abc123") {
			t.Errorf("body missing ref in footer: %s", body)
		}
		if !strings.Contains(body, "8192") {
			t.Errorf("body missing size in footer: %s", body)
		}
		// No wrapper terminology — naming the internal shape primes
		// the model on the wrong pattern.
		forbidden := []string{
			"ArtifactStub",
			"artifact_ref",
			`"preview"`, // quoted to avoid matching the operator-facing footer's literal
		}
		for _, term := range forbidden {
			if strings.Contains(body, term) {
				t.Errorf("body leaks wrapper terminology %q. Body:\n%s", term, body)
			}
		}
	})

	// Sub-test 2: the *llm.ArtifactStub shape from the multimodal
	// materialiser. Same projection rules apply.
	t.Run("artifact_stub_shape", func(t *testing.T) {
		t.Parallel()
		stub := &llm.ArtifactStub{
			Ref:       "img_xyz789",
			MIME:      "image/png",
			SizeBytes: 65536,
			Summary:   "User-uploaded screenshot at turn 3",
		}
		rc := planner.RunContext{
			Goal: "describe image",
			Trajectory: &planner.Trajectory{
				Steps: []planner.Step{{
					Action: planner.CallTool{
						Tool:   "vision.describe",
						Args:   json.RawMessage(`{}`),
						CallID: "call_v1",
					},
					LLMObservation: stub,
				}},
			},
		}
		req := defaultBuilder{}.Build(rc, "sys")
		last := req.Messages[len(req.Messages)-1]
		body := *last.Content.Text
		if !strings.Contains(body, "User-uploaded screenshot at turn 3") {
			t.Errorf("body missing Summary as preview: %s", body)
		}
		if !strings.Contains(body, "artifact_fetch") {
			t.Errorf("body missing artifact_fetch footer: %s", body)
		}
		if !strings.Contains(body, "img_xyz789") {
			t.Errorf("body missing ref in footer: %s", body)
		}
		if !strings.Contains(body, "image/png") {
			t.Errorf("body missing mime in footer: %s", body)
		}
		// No wrapper JSON in the body — Summary inline, not the
		// MarshalJSON output.
		if strings.Contains(body, `"artifact_ref"`) {
			t.Errorf("body contains wrapper JSON: %s", body)
		}
	})

	// Sub-test 3: non-wrapper observation preserved verbatim. The
	// renderer's fast-path is unchanged for the common case.
	t.Run("non_wrapper_observation_unchanged", func(t *testing.T) {
		t.Parallel()
		rc := planner.RunContext{
			Goal: "g",
			Trajectory: &planner.Trajectory{
				Steps: []planner.Step{{
					Action:         planner.CallTool{Tool: "echo"},
					LLMObservation: "small result",
				}},
			},
		}
		req := defaultBuilder{}.Build(rc, "sys")
		last := req.Messages[len(req.Messages)-1]
		body := *last.Content.Text
		if body != "small result" {
			t.Errorf("non-wrapper observation body = %q, want %q", body, "small result")
		}
		if strings.Contains(body, "artifact_fetch") {
			t.Errorf("footer leaked into non-wrapper observation: %s", body)
		}
	})

	// Sub-test 4: map shape WITHOUT artifact_ref still inlines preview
	// — defensive behaviour for a partial wrapper (preview-only) that
	// could arise from a degraded artifact-store path. No footer is
	// emitted in that case (no ref to fetch).
	t.Run("preview_only_map_inlines_without_footer", func(t *testing.T) {
		t.Parallel()
		rc := planner.RunContext{
			Goal: "g",
			Trajectory: &planner.Trajectory{
				Steps: []planner.Step{{
					Action: planner.CallTool{Tool: "t"},
					LLMObservation: map[string]any{
						"tool":      "t",
						"preview":   "first 100 chars of payload",
						"truncated": true,
					},
				}},
			},
		}
		req := defaultBuilder{}.Build(rc, "sys")
		body := *req.Messages[len(req.Messages)-1].Content.Text
		if !strings.Contains(body, "first 100 chars of payload") {
			t.Errorf("preview not inlined: %s", body)
		}
		if strings.Contains(body, "artifact_fetch") {
			t.Errorf("footer leaked for ref-less projection: %s", body)
		}
	})

	// Sub-test 5: Error / Failure precedence is preserved — the
	// renderer's existing fast-path for error states stays in front
	// of the heavy-content detection.
	t.Run("error_precedence_preserved", func(t *testing.T) {
		t.Parallel()
		rc := planner.RunContext{
			Goal: "g",
			Trajectory: &planner.Trajectory{
				Steps: []planner.Step{{
					Action: planner.CallTool{Tool: "t"},
					Error:  "tool dispatch failed",
					// LLMObservation set to a wrapper, but Error wins.
					LLMObservation: map[string]any{
						"preview":      "should not be rendered",
						"artifact_ref": "nope",
					},
				}},
			},
		}
		req := defaultBuilder{}.Build(rc, "sys")
		body := *req.Messages[len(req.Messages)-1].Content.Text
		if !strings.Contains(body, "tool dispatch failed") {
			t.Errorf("Error precedence broken: %s", body)
		}
		if strings.Contains(body, "should not be rendered") {
			t.Errorf("wrapper leaked through error precedence: %s", body)
		}
	})
}

// TestRenderNativeStepPair_EmptyObservationEmitsPlaceholderToolMsg
// asserts the contract-preservation behaviour: when a CallTool step
// lands with Observation=nil, LLMObservation=nil, and no Failure /
// Error, the renderer MUST still emit a `RoleTool` sibling message
// with the matching `ToolCallID` — synthesised with a placeholder
// body — rather than returning nil and orphaning the assistant
// tool_call on the wire. OpenAI's spec requires the pairing.
func TestRenderNativeStepPair_EmptyObservationEmitsPlaceholderToolMsg(t *testing.T) {
	t.Parallel()

	step := planner.Step{
		Action: planner.CallTool{
			Tool:   "youtube_get_metadata",
			Args:   json.RawMessage(`{"url":"https://example.com"}`),
			CallID: "call_orphan_repro",
		},
		// Observation, LLMObservation, Failure, Error all intentionally zero.
	}
	asst, tool, native := renderNativeStepPair(step, planner.ReasoningReplayNever, 0)
	if !native {
		t.Fatal("renderNativeStepPair returned native=false for a CallTool step")
	}
	if tool == nil {
		t.Fatal("orphan regression: tool message is nil — assistant tool_call would be unpaired on the wire")
	}
	if tool.Role != llm.RoleTool {
		t.Errorf("tool.Role = %q, want %q", tool.Role, llm.RoleTool)
	}
	if tool.ToolCallID == nil || *tool.ToolCallID != "call_orphan_repro" {
		t.Errorf("tool.ToolCallID must match assistant tool_call id (asst=%q, tool=%v)",
			"call_orphan_repro", tool.ToolCallID)
	}
	if tool.Content.Text == nil || *tool.Content.Text == "" {
		t.Error("placeholder tool message has empty Content — must carry a sentinel body the model can read")
	}
	// Assistant half retains the tool_call (orphan-fix must not lose the call).
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_orphan_repro" {
		t.Errorf("assistant tool_call lost: %+v", asst.ToolCalls)
	}
}

// TestRenderHeavyContentMap_FieldAwarePreviewSurfacesRef asserts
// that when the executor's HeavyTruncationSummary lands with a
// field-aware preview (well-formed JSON containing `[omitted: N
// bytes]` sentinels) AND an artifact_ref, the renderer surfaces
// BOTH the preview body AND the artifact_fetch ref. Without the
// ref the model has no callable path to retrieve the omitted
// fields.
func TestRenderHeavyContentMap_FieldAwarePreviewSurfacesRef(t *testing.T) {
	t.Parallel()

	// The renderer must:
	//   1. Inline the preview body verbatim
	//   2. Append the field-aware footer naming the ref
	//   3. Use language that names what artifact_fetch retrieves
	//      (the OMITTED fields) and tells the model not to re-call
	//      the upstream tool
	fieldAwareJSON := `{"title":"Example Video","duration":213,"description":"[omitted: 2492 bytes]","subtitles":"[omitted: 17697 bytes]"}`
	wrapper := map[string]any{
		"tool":         "youtube_get_metadata",
		"size_bytes":   720000,
		"truncated":    true,
		"preview":      fieldAwareJSON,
		"artifact_ref": "yt_meta_ref_xyz",
	}
	body, matched := renderHeavyContentMap(wrapper)
	if !matched {
		t.Fatal("renderHeavyContentMap did not match a wrapper with field-aware preview + ref")
	}

	// The preview body must be inlined.
	if !strings.Contains(body, `"title":"Example Video"`) {
		t.Errorf("body missing preview content: %s", body)
	}
	if !strings.Contains(body, `"[omitted: 2492 bytes]"`) {
		t.Errorf("body missing in-band omission sentinel: %s", body)
	}

	// The artifact_fetch ref MUST be surfaced (the bug-#2 regression).
	if !strings.Contains(body, "artifact_fetch") {
		t.Fatalf("FIELD-AWARE PREVIEW BUG REGRESSED: artifact_fetch ref dropped — model has no callable path to retrieve omitted fields, will loop on upstream tool. Body:\n%s", body)
	}
	if !strings.Contains(body, "yt_meta_ref_xyz") {
		t.Errorf("body missing ref value: %s", body)
	}

	// The footer wording must name artifact_fetch as the retrieval
	// path for the OMITTED fields and tell the model not to re-call
	// the upstream tool.
	if !strings.Contains(body, "do not re-call the upstream tool") {
		t.Errorf("footer missing upstream-retry pre-emption: %s", body)
	}
}

// TestDefaultBuilder_EmptyObservationProducesPairedWireSlice asserts
// the builder-level invariant: when a trajectory step has
// Observation=nil and LLMObservation=nil, the assembled
// `req.Messages` slice still contains the matching RoleTool message
// after the assistant message. The translator preserves the
// pairing; if it's unpaired here, OpenAI rejects.
func TestDefaultBuilder_EmptyObservationProducesPairedWireSlice(t *testing.T) {
	t.Parallel()

	rc := planner.RunContext{
		Goal: "fetch metadata",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{{
				Action: planner.CallTool{
					Tool:   "youtube_get_metadata",
					Args:   json.RawMessage(`{"url":"https://x"}`),
					CallID: "call_pair_repro",
				},
				// All observation fields zero — the orphan trigger.
			}},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")

	// Walk the message slice and assert every assistant message with
	// ToolCalls is followed by a RoleTool message with matching
	// ToolCallID — the same invariant validateToolCallPairing
	// enforces, checked from the producer side.
	var pending []string
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			if len(pending) != 0 {
				t.Fatalf("messages[%d]: new assistant ToolCalls turn arrived with %d unanswered prior IDs %v",
					i, len(pending), pending)
			}
			for _, tc := range m.ToolCalls {
				pending = append(pending, tc.ID)
			}
		}
		if m.Role == llm.RoleTool {
			if m.ToolCallID == nil {
				t.Fatalf("messages[%d]: RoleTool message missing ToolCallID", i)
			}
			found := false
			for j, pid := range pending {
				if pid == *m.ToolCallID {
					pending = append(pending[:j], pending[j+1:]...)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("messages[%d]: RoleTool ToolCallID=%q matched no pending assistant ToolCalls (pending=%v)",
					i, *m.ToolCallID, pending)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("builder produced orphan assistant tool_calls: %v", pending)
	}
}

// TestRenderNativeParallelStep_RoundTrip — AC-15: a trajectory step
// whose Action is a CallParallel with N branches renders ONE assistant
// message carrying N tool_calls + N RoleTool messages, one per branch,
// each ToolCallID matched to its branch CallID, in branch-index order.
// One branch carries an error → its RoleTool body surfaces the error.
func TestRenderNativeParallelStep_RoundTrip(t *testing.T) {
	t.Parallel()

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"a":1}`), CallID: "c_a"},
			{Tool: "beta", Args: json.RawMessage(`{"b":2}`), CallID: "c_b"},
			{Tool: "gamma", Args: json.RawMessage(`{"c":3}`), CallID: "c_c"},
		},
	}
	step := planner.Step{
		Action: call,
		// LLMObservation carries the AC-4 aggregate (out of branch order
		// on purpose — the renderer must key by Index, not slice order).
		LLMObservation: planner.ParallelObservation{
			Branches: []planner.ParallelBranchObservation{
				{CallID: "c_b", Tool: "beta", Index: 1, Value: map[string]any{"ok": "beta-result"}},
				{CallID: "c_a", Tool: "alpha", Index: 0, Value: "alpha-result"},
				{CallID: "c_c", Tool: "gamma", Index: 2, Error: "boom"},
			},
		},
	}

	asst, toolMsgs := renderNativeParallelStep(step, call, planner.ReasoningReplayNever, 0)

	// ONE assistant message with N tool_calls, branch-index order.
	if len(asst.ToolCalls) != 3 {
		t.Fatalf("assistant ToolCalls = %d, want 3", len(asst.ToolCalls))
	}
	wantIDs := []string{"c_a", "c_b", "c_c"}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, tc := range asst.ToolCalls {
		if tc.ID != wantIDs[i] || tc.Name != wantNames[i] {
			t.Errorf("assistant tool_call[%d] = {%q,%q}, want {%q,%q}", i, tc.ID, tc.Name, wantIDs[i], wantNames[i])
		}
	}

	// N RoleTool messages, each ToolCallID matched in branch-index order.
	if len(toolMsgs) != 3 {
		t.Fatalf("tool messages = %d, want 3", len(toolMsgs))
	}
	for i, tm := range toolMsgs {
		if tm.Role != llm.RoleTool {
			t.Errorf("toolMsgs[%d].Role = %q, want RoleTool", i, tm.Role)
		}
		if tm.ToolCallID == nil || *tm.ToolCallID != wantIDs[i] {
			t.Errorf("toolMsgs[%d].ToolCallID = %v, want %q", i, tm.ToolCallID, wantIDs[i])
		}
		if tm.Content.Text == nil || *tm.Content.Text == "" {
			t.Errorf("toolMsgs[%d] empty content", i)
		}
	}
	// branch 0 (alpha) success body.
	if body := *toolMsgs[0].Content.Text; !strings.Contains(body, "alpha-result") {
		t.Errorf("alpha body = %q, want it to carry alpha-result", body)
	}
	// branch 2 (gamma) error body.
	if body := *toolMsgs[2].Content.Text; !strings.Contains(body, "boom") {
		t.Errorf("gamma body = %q, want it to surface the error", body)
	}

	// Every tool_call_id has exactly one matching RoleTool (wire invariant).
	pending := map[string]int{}
	for _, tc := range asst.ToolCalls {
		pending[tc.ID]++
	}
	for _, tm := range toolMsgs {
		pending[*tm.ToolCallID]--
	}
	for id, n := range pending {
		if n != 0 {
			t.Errorf("tool_call_id %q unbalanced: %d", id, n)
		}
	}
}

// TestRenderNativeParallelStep_EmptyCallIDsSynthesised — AC-9: branches
// with empty CallIDs get a deterministic synthetic ID stamped on BOTH
// the assistant tool_call AND its RoleTool answer so the pairing stays
// well-formed; lookup is by Index, robust to the empty IDs.
func TestRenderNativeParallelStep_EmptyCallIDsSynthesised(t *testing.T) {
	t.Parallel()
	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{}`)}, // no CallID
			{Tool: "beta", Args: json.RawMessage(`{}`)},  // no CallID
		},
	}
	step := planner.Step{
		Action: call,
		LLMObservation: planner.ParallelObservation{
			Branches: []planner.ParallelBranchObservation{
				{Tool: "alpha", Index: 0, Value: "a"},
				{Tool: "beta", Index: 1, Value: "b"},
			},
		},
	}
	asst, toolMsgs := renderNativeParallelStep(step, call, planner.ReasoningReplayNever, 4)
	if len(asst.ToolCalls) != 2 || len(toolMsgs) != 2 {
		t.Fatalf("got %d tool_calls / %d tool msgs, want 2/2", len(asst.ToolCalls), len(toolMsgs))
	}
	for i := range asst.ToolCalls {
		id := asst.ToolCalls[i].ID
		if id == "" {
			t.Fatalf("tool_call[%d] has empty synthetic ID", i)
		}
		if toolMsgs[i].ToolCallID == nil || *toolMsgs[i].ToolCallID != id {
			t.Errorf("tool msg[%d] ID %v does not match synthetic assistant ID %q", i, toolMsgs[i].ToolCallID, id)
		}
	}
	// Deterministic shape: react.callid.<step>.<branch>.
	if asst.ToolCalls[0].ID != "react.callid.4.0" || asst.ToolCalls[1].ID != "react.callid.4.1" {
		t.Errorf("synthetic IDs = %q,%q, want react.callid.4.0/.1", asst.ToolCalls[0].ID, asst.ToolCalls[1].ID)
	}
}

// TestRenderNativeBatchStep_RoundTrip — a Batch step reconstructs as ONE
// assistant message carrying native tool_calls for every Tools branch AND
// every Spawns call (in declaration order: tools then spawns), answered
// by one RoleTool message per call_id in the SAME order. Reconstruction
// keys the call-id/index-keyed BatchObservation (deliberately shuffled
// here), never Go map order.
func TestRenderNativeBatchStep_RoundTrip(t *testing.T) {
	t.Parallel()

	call := planner.Batch{
		Tools: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"a":1}`), CallID: "t_a"},
			{Tool: "beta", Args: json.RawMessage(`{"b":2}`), CallID: "t_b"},
		},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "sub-a"}, CallID: "s_a"},
			{Spec: planner.SpawnSpec{Query: "sub-b"}, CallID: "s_b"},
		},
	}
	step := planner.Step{
		Action: call,
		// Both halves are shuffled on purpose — the renderer must key by
		// Index, not slice order.
		LLMObservation: planner.BatchObservation{
			Tools: []planner.ParallelBranchObservation{
				{CallID: "t_b", Tool: "beta", Index: 1, Error: "beta-boom"},
				{CallID: "t_a", Tool: "alpha", Index: 0, Value: "alpha-result"},
			},
			Spawns: []planner.BatchSpawnObservation{
				{CallID: "s_b", Index: 1, Error: "sealed group"},
				{CallID: "s_a", Index: 0, TaskID: "task-1", GroupID: "grp-1"},
			},
		},
	}

	asst, toolMsgs := renderNativeBatchStep(step, call, planner.ReasoningReplayNever, 0)

	// ONE assistant message: 4 tool_calls in declaration order (tools then spawns).
	wantIDs := []string{"t_a", "t_b", "s_a", "s_b"}
	wantNames := []string{"alpha", "beta", SpawnTaskToolName, SpawnTaskToolName}
	if len(asst.ToolCalls) != 4 {
		t.Fatalf("assistant ToolCalls = %d, want 4", len(asst.ToolCalls))
	}
	for i, tc := range asst.ToolCalls {
		if tc.ID != wantIDs[i] || tc.Name != wantNames[i] {
			t.Errorf("assistant tool_call[%d] = {%q,%q}, want {%q,%q}", i, tc.ID, tc.Name, wantIDs[i], wantNames[i])
		}
	}

	// 4 RoleTool answers, matched by ID in the same order.
	if len(toolMsgs) != 4 {
		t.Fatalf("tool messages = %d, want 4", len(toolMsgs))
	}
	for i, tm := range toolMsgs {
		if tm.Role != llm.RoleTool {
			t.Errorf("toolMsgs[%d].Role = %q, want RoleTool", i, tm.Role)
		}
		if tm.ToolCallID == nil || *tm.ToolCallID != wantIDs[i] {
			t.Errorf("toolMsgs[%d].ToolCallID = %v, want %q", i, tm.ToolCallID, wantIDs[i])
		}
	}
	// tool 0 (alpha) success, tool 1 (beta) error.
	if body := *toolMsgs[0].Content.Text; !strings.Contains(body, "alpha-result") {
		t.Errorf("alpha body = %q, want alpha-result", body)
	}
	if body := *toolMsgs[1].Content.Text; !strings.Contains(body, "beta-boom") {
		t.Errorf("beta body = %q, want the tool error", body)
	}
	// spawn 0 registration outcome, spawn 1 reject error.
	if body := *toolMsgs[2].Content.Text; !strings.Contains(body, "task-1") || !strings.Contains(body, "grp-1") {
		t.Errorf("spawn-a body = %q, want task_id + group_id", body)
	}
	if body := *toolMsgs[3].Content.Text; !strings.Contains(body, "sealed group") {
		t.Errorf("spawn-b body = %q, want the registry reject error", body)
	}

	// Every tool_call_id has exactly one matching RoleTool (wire invariant).
	pending := map[string]int{}
	for _, tc := range asst.ToolCalls {
		pending[tc.ID]++
	}
	for _, tm := range toolMsgs {
		pending[*tm.ToolCallID]--
	}
	for id, n := range pending {
		if n != 0 {
			t.Errorf("tool_call_id %q unbalanced: %d", id, n)
		}
	}
}

// TestBatchObservationFrom_SlotsAndPointer — the extraction prefers
// LLMObservation, falls back to the raw Observation slot, accepts the
// pointer form, and reports (‑, false) when neither slot carries a
// BatchObservation (the renderer then emits placeholders).
func TestBatchObservationFrom_SlotsAndPointer(t *testing.T) {
	t.Parallel()
	// Pointer form in LLMObservation.
	ptr := &planner.BatchObservation{Spawns: []planner.BatchSpawnObservation{{Index: 0, TaskID: "p"}}}
	if got, ok := batchObservationFrom(planner.Step{LLMObservation: ptr}); !ok || len(got.Spawns) != 1 {
		t.Errorf("pointer LLMObservation: got %+v ok=%v", got, ok)
	}
	// Raw Observation slot fallback (LLMObservation absent).
	val := planner.BatchObservation{Tools: []planner.ParallelBranchObservation{{Index: 0, Value: "x"}}}
	if got, ok := batchObservationFrom(planner.Step{Observation: val}); !ok || len(got.Tools) != 1 {
		t.Errorf("Observation-slot fallback: got %+v ok=%v", got, ok)
	}
	// A nil pointer in LLMObservation is skipped; the value slot wins.
	var nilPtr *planner.BatchObservation
	if got, ok := batchObservationFrom(planner.Step{LLMObservation: nilPtr, Observation: val}); !ok || len(got.Tools) != 1 {
		t.Errorf("nil pointer skip → value slot: got %+v ok=%v", got, ok)
	}
	// Neither slot carries a BatchObservation.
	if _, ok := batchObservationFrom(planner.Step{Observation: "not-a-batch-obs"}); ok {
		t.Error("non-BatchObservation slots should report ok=false")
	}
}

// TestRenderNativeBatchStep_EmptyCallIDsSynthesised — a Batch whose
// branches carry no CallID gets deterministic synthetic IDs (tools
// `react.callid.<step>.t<i>`, spawns `.s<i>`) stamped on BOTH the
// assistant tool_call AND its RoleTool answer, so the pairing stays
// well-formed.
func TestRenderNativeBatchStep_EmptyCallIDsSynthesised(t *testing.T) {
	t.Parallel()
	call := planner.Batch{
		Tools:  []planner.CallTool{{Tool: "alpha", Args: json.RawMessage(`{}`)}},
		Spawns: []planner.SpawnTask{{Spec: planner.SpawnSpec{Query: "x"}}},
	}
	step := planner.Step{
		Action: call,
		LLMObservation: planner.BatchObservation{
			Tools:  []planner.ParallelBranchObservation{{Tool: "alpha", Index: 0, Value: "a"}},
			Spawns: []planner.BatchSpawnObservation{{Index: 0, TaskID: "t9"}},
		},
	}
	asst, toolMsgs := renderNativeBatchStep(step, call, planner.ReasoningReplayNever, 3)
	if len(asst.ToolCalls) != 2 || len(toolMsgs) != 2 {
		t.Fatalf("got %d tool_calls / %d tool msgs, want 2/2", len(asst.ToolCalls), len(toolMsgs))
	}
	if asst.ToolCalls[0].ID != "react.callid.3.t0" || asst.ToolCalls[1].ID != "react.callid.3.s0" {
		t.Errorf("synthetic IDs = %q,%q, want react.callid.3.t0/.s0", asst.ToolCalls[0].ID, asst.ToolCalls[1].ID)
	}
	for i := range asst.ToolCalls {
		if toolMsgs[i].ToolCallID == nil || *toolMsgs[i].ToolCallID != asst.ToolCalls[i].ID {
			t.Errorf("tool msg[%d] ID %v does not match assistant ID %q", i, toolMsgs[i].ToolCallID, asst.ToolCalls[i].ID)
		}
	}
}

// TestDefaultBuilder_BatchStepRoundTripsThroughMessages — the builder
// level: a Batch step assembled through Build produces a paired wire
// slice (one assistant tool_calls turn spanning tools + spawns, one
// matching RoleTool answer per call_id, no orphans) — so merging the
// Batch executor no longer leaves a degraded replay for a multi-step run.
func TestDefaultBuilder_BatchStepRoundTripsThroughMessages(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "batch",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{{
				Action: planner.Batch{
					Tools:  []planner.CallTool{{Tool: "alpha", Args: json.RawMessage(`{}`), CallID: "b1"}},
					Spawns: []planner.SpawnTask{{Spec: planner.SpawnSpec{Query: "q"}, CallID: "b2"}},
				},
				LLMObservation: planner.BatchObservation{
					Tools:  []planner.ParallelBranchObservation{{CallID: "b1", Tool: "alpha", Index: 0, Value: "ra"}},
					Spawns: []planner.BatchSpawnObservation{{CallID: "b2", Index: 0, TaskID: "task-9"}},
				},
			}},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	var pending []string
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			if len(pending) != 0 {
				t.Fatalf("messages[%d]: new assistant ToolCalls turn with %d unanswered IDs %v", i, len(pending), pending)
			}
			for _, tc := range m.ToolCalls {
				pending = append(pending, tc.ID)
			}
		}
		if m.Role == llm.RoleTool {
			if m.ToolCallID == nil {
				t.Fatalf("messages[%d]: RoleTool missing ToolCallID", i)
			}
			found := false
			for j, pid := range pending {
				if pid == *m.ToolCallID {
					pending = append(pending[:j], pending[j+1:]...)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("messages[%d]: RoleTool ToolCallID=%q matched no pending (pending=%v)", i, *m.ToolCallID, pending)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("Batch step produced orphan assistant tool_calls: %v", pending)
	}
}

// TestDefaultBuilder_CallParallelStepRoundTripsThroughMessages — AC-15
// at the builder level: a CallParallel step assembled through Build
// produces a paired wire slice (one assistant tool_calls turn, N
// matching RoleTool answers, no orphans).
func TestDefaultBuilder_CallParallelStepRoundTripsThroughMessages(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "fan out",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{{
				Action: planner.CallParallel{
					Branches: []planner.CallTool{
						{Tool: "alpha", Args: json.RawMessage(`{}`), CallID: "p1"},
						{Tool: "beta", Args: json.RawMessage(`{}`), CallID: "p2"},
					},
				},
				LLMObservation: planner.ParallelObservation{
					Branches: []planner.ParallelBranchObservation{
						{CallID: "p1", Tool: "alpha", Index: 0, Value: "ra"},
						{CallID: "p2", Tool: "beta", Index: 1, Value: "rb"},
					},
				},
			}},
		},
	}
	req := defaultBuilder{}.Build(rc, "sys")
	var pending []string
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			if len(pending) != 0 {
				t.Fatalf("messages[%d]: new assistant ToolCalls turn with %d unanswered IDs %v", i, len(pending), pending)
			}
			for _, tc := range m.ToolCalls {
				pending = append(pending, tc.ID)
			}
		}
		if m.Role == llm.RoleTool {
			if m.ToolCallID == nil {
				t.Fatalf("messages[%d]: RoleTool missing ToolCallID", i)
			}
			found := false
			for j, pid := range pending {
				if pid == *m.ToolCallID {
					pending = append(pending[:j], pending[j+1:]...)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("messages[%d]: RoleTool ToolCallID=%q matched no pending (pending=%v)", i, *m.ToolCallID, pending)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("CallParallel step produced orphan assistant tool_calls: %v", pending)
	}
}
