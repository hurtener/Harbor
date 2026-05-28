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

// the nine XML section tags in their Phase 107c (D-167) fixed order.
// `<output_format>`, `<action_schema>`, `<finishing>`, and
// `<parallel_execution>` are deleted; `<tool_discovery>` replaces
// them. `<available_tools>` now renders name+description only —
// schemas live in req.Tools[]. Parallel emission is a native-side
// property: the runtime accepts multiple ToolCalls in one response
// and serialises them per the AC-19 fallback.
var section83aTags = []string{
	"identity",
	"tool_discovery",
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
// seven always-on sections render exactly once each, in the Phase 107c
// (D-167) fixed order, separated by a blank line. The remaining two
// (additional_guidance, planning_constraints) are conditional.
func TestBuildSystemContent_NineSectionsAlwaysPresentInOrder(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})

	// The seven always-on sections (8 + 9 are conditional).
	alwaysOn := section83aTags[:7]
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
// tool-calling contract: emit only tool calls during intermediate
// steps; reasoning is captured automatically via provider channels.
// The legacy "produce ONLY the JSON action object" + "thought/reasoning
// in the JSON" clamps were retired in Phase 107c (D-167 AC-20) — the
// new wire shape is native ToolCalls, not a JSON action envelope.
func TestBuildSystemContent_ToneIntermediateStepClamp(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	if !strings.Contains(body, "Emit only tool calls — keep any narration to the final answer turn.") {
		t.Errorf("<tone> missing intermediate-step clamp ('Emit only tool calls')")
	}
	if strings.Contains(body, "produce ONLY the JSON action object") {
		t.Errorf("<tone> still references the deleted JSON-action clamp — Phase 107c retired it")
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

// TestRenderNativeObservation_ArtifactStubInlinesPreview pins the
// Phase 107c follow-up (B). When the runtime tool-executor's
// heavy-content projection lands on a trajectory step (either as the
// `*llm.ArtifactStub` shape from the multimodal materialiser or as
// the `heavyTruncationSummary` map from the dev tool executor), the
// RoleTool message Content body is the inlined preview text + a
// positional `artifact_fetch` footer — NEVER the wrapper JSON the
// live YouTube test surfaced as the loop-trigger bug. Includes the
// negative-instruction-trap regression gate: no wrapper terminology
// ("ArtifactStub", "artifact_ref", "preview") appears in the
// LLM-facing body, mirroring the c12e309 fix for `_finish`.
func TestRenderNativeObservation_ArtifactStubInlinesPreview(t *testing.T) {
	t.Parallel()

	// Sub-test 1: the runtime tool-executor's map shape. This is what
	// `cmd/harbor/cmd_dev_executor.go::heavyTruncationSummary`
	// returns and what the live YouTube test exposed as the bug
	// trigger. The renderer MUST inline `preview` as the body.
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
		// REGRESSION GATE — no wrapper terminology. The negative-
		// instruction trap commit c12e309 closed for `_finish` applies
		// here too: naming the wrapper shape primes the model on the
		// wrong pattern. The tool's own description (in
		// <available_tools>) teaches what artifact_fetch does; the
		// footer signals when to call it.
		forbidden := []string{
			"ArtifactStub",
			"artifact_ref",
			`"preview"`, // quoted to avoid matching the operator-facing footer's literal
		}
		for _, term := range forbidden {
			if strings.Contains(body, term) {
				t.Errorf("body leaks wrapper terminology %q (negative-instruction trap regression). Body:\n%s", term, body)
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
