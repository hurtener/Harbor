package react

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
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
	for _, want := range []string{"SYS_PROMPT", "search", "find things", "answer", "respond to user", "_finish"} {
		if !strings.Contains(body, want) {
			t.Errorf("system content missing %q. Body: %s", want, body)
		}
	}
}

// TestDefaultBuilder_FallsBackToDefaultSystemPrompt asserts that an
// empty system prompt argument substitutes the canonical default.
func TestDefaultBuilder_FallsBackToDefaultSystemPrompt(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{Goal: "g"}
	req := defaultBuilder{}.Build(rc, "")
	if len(req.Messages) == 0 {
		t.Fatal("Build returned zero messages")
	}
	body := *req.Messages[0].Content.Text
	if !strings.Contains(body, "ReAct planner") {
		t.Errorf("default system prompt not used. Body: %s", body)
	}
}

// TestDefaultBuilder_UserMessagePrefersGoalOverQuery asserts the user
// block reads Goal first; falls back to Query when Goal is empty;
// falls back to a marker when both are empty.
func TestDefaultBuilder_UserMessagePrefersGoalOverQuery(t *testing.T) {
	t.Parallel()
	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
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
// asserts the brief 07 §5 shape: each completed Step renders as
// [assistant: action JSON] + [user: observation].
func TestDefaultBuilder_RendersTrajectoryStepsAsAssistantUserPairs(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{
		Goal: "find stuff",
		Trajectory: &planner.Trajectory{
			Steps: []planner.Step{
				{
					Action: planner.CallTool{
						Tool: "search",
						Args: json.RawMessage(`{"q":"foo"}`),
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
	// Expect: system, user (goal), assistant (step1), user (obs1),
	// assistant (step2), user (obs2) → 6 messages.
	if len(req.Messages) != 6 {
		t.Fatalf("len(Messages) = %d, want 6 — messages: %+v", len(req.Messages), summariseRoles(req.Messages))
	}
	wantRoles := []llm.Role{
		llm.RoleSystem,
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleUser,
	}
	for i, w := range wantRoles {
		if req.Messages[i].Role != w {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, w)
		}
	}
	if !strings.Contains(*req.Messages[2].Content.Text, `"tool":"search"`) {
		t.Errorf("step-1 assistant message missing tool envelope: %s", *req.Messages[2].Content.Text)
	}
	if !strings.Contains(*req.Messages[3].Content.Text, "found 3 hits") {
		t.Errorf("step-1 user observation missing: %s", *req.Messages[3].Content.Text)
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
	// Last message is the observation user-message.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("last role = %q, want user", last.Role)
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
	// Step1 obs at index 3, step2 obs at index 5.
	if !strings.Contains(*req.Messages[3].Content.Text, "Observation (error): boom") {
		t.Errorf("step1 obs missing error: %s", *req.Messages[3].Content.Text)
	}
	if !strings.Contains(*req.Messages[5].Content.Text, "Observation (failure): schema_repair_exhausted") {
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
