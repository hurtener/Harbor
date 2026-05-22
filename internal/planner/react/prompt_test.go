package react

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

// TestDefaultBuilder_FallsBackToDefaultSystemPrompt asserts that an
// empty system prompt argument substitutes the canonical default —
// i.e. the rendered twelve-section structured prompt (Phase 83a).
func TestDefaultBuilder_FallsBackToDefaultSystemPrompt(t *testing.T) {
	t.Parallel()
	rc := planner.RunContext{Goal: "g"}
	req := defaultBuilder{}.Build(rc, "")
	if len(req.Messages) == 0 {
		t.Fatal("Build returned zero messages")
	}
	body := *req.Messages[0].Content.Text
	// The structured default opens with the <identity> section.
	if !strings.Contains(body, "<identity>") || !strings.Contains(body, "<action_schema>") {
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

// ----------------------------------------------------------------------------
// Phase 83a — twelve-section structured prompt (brief 13 §2.1).
// ----------------------------------------------------------------------------

// the twelve XML section tags in their fixed brief-13 §2.1 order.
var section83aTags = []string{
	"identity",
	"output_format",
	"action_schema",
	"finishing",
	"tool_usage",
	"parallel_execution",
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

// TestBuildSystemContent_TenSectionsAlwaysPresentInOrder asserts the
// ten always-on sections render exactly once each, in the brief 13
// §2.1 fixed order, separated by a blank line.
func TestBuildSystemContent_TenSectionsAlwaysPresentInOrder(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})

	// The ten always-on sections (11 + 12 are conditional).
	alwaysOn := section83aTags[:10]
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
	if !strings.Contains(body, "</identity>\n\n<output_format>") {
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

// TestBuildSystemContent_ToneCarriesCriticalClamp asserts the <tone>
// section ports the predecessor's CRITICAL clamp verbatim (brief 13
// §2.6 — both lines, case-sensitive).
func TestBuildSystemContent_ToneCarriesCriticalClamp(t *testing.T) {
	t.Parallel()
	body := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{Goal: "g"})
	clampA := "During intermediate steps, produce ONLY the JSON action object. Do not add commentary."
	clampB := "Do not include a 'thought' or 'reasoning' field in the JSON."
	if !strings.Contains(body, clampA) {
		t.Errorf("<tone> missing CRITICAL clamp line A: %q", clampA)
	}
	if !strings.Contains(body, clampB) {
		t.Errorf("<tone> missing CRITICAL clamp line B: %q", clampB)
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

// TestBuildSystemContent_ErrorHandlingNoRequiresFollowup asserts the
// <error_handling> block guides clarification via args.answer, not a
// `requires_followup` flag (acceptance criterion).
func TestBuildSystemContent_ErrorHandlingNoRequiresFollowup(t *testing.T) {
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
		t.Errorf("<error_handling> references requires_followup — must guide via args.answer instead")
	}
	if !strings.Contains(section, "args.answer") {
		t.Errorf("<error_handling> should guide clarification via args.answer")
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
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	got := renderDefaultSystem(t, defaultBuilder{}, planner.RunContext{})
	// Normalise the volatile date line to the fixture's sentinel.
	dateRE := regexp.MustCompile(`Current date: \d{4}-\d{2}-\d{2}`)
	gotNorm := dateRE.ReplaceAllString(got, "Current date: 2025-01-01")
	if gotNorm != string(want) {
		t.Errorf("rendered default prompt diverged from %s.\n"+
			"If this change is intentional, regenerate the fixture.\n"+
			"--- got ---\n%s\n--- want ---\n%s", goldenPath, gotNorm, string(want))
	}
}

// --- Phase 83b — tool schema injection (D-144) ----------------------

// fixtureCatalog returns the canonical two-tool catalog the Phase 83b
// golden fixture documents: one tool with a non-trivial args_schema
// plus curated examples, one bare tool with neither. The bare tool
// proves the no-examples / no-schema path renders without an
// `examples:` or `args_schema:` line — backward-compatible with
// pre-83b registrations.
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
			// No ArgsSchema, no Examples — the pre-83b bare shape.
		},
	}}
}

// TestRenderTool_TagRankingOrdersExamples asserts examples render in
// `minimal > common > edge-case` order regardless of registration
// order (acceptance criterion).
func TestRenderTool_TagRankingOrdersExamples(t *testing.T) {
	t.Parallel()
	tool := tools.Tool{
		Name:        "t",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		SideEffects: tools.SideEffectPure,
		Examples: []tools.ToolExample{
			{Description: "edge", Args: map[string]any{"x": "e"}, Tags: []string{"edge-case"}},
			{Description: "common", Args: map[string]any{"x": "c"}, Tags: []string{"common"}},
			{Description: "minimal", Args: map[string]any{"x": "m"}, Tags: []string{"minimal"}},
		},
	}
	out := renderTool(tool, toolRenderConfig{maxExamples: 3})
	mi := strings.Index(out, "minimal")
	ci := strings.Index(out, "common")
	ei := strings.Index(out, "edge")
	if !(mi < ci && ci < ei) || mi < 0 {
		t.Errorf("examples not in minimal<common<edge order. Output:\n%s", out)
	}
}

// TestRenderTool_LimitKeepsTopRanked asserts that with maxExamples=1
// only the highest-priority (minimal) example renders.
func TestRenderTool_LimitKeepsTopRanked(t *testing.T) {
	t.Parallel()
	tool := tools.Tool{
		Name:        "t",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		SideEffects: tools.SideEffectPure,
		Examples: []tools.ToolExample{
			{Description: "common", Args: map[string]any{"x": "c"}, Tags: []string{"common"}},
			{Description: "minimal", Args: map[string]any{"x": "m"}, Tags: []string{"minimal"}},
		},
	}
	out := renderTool(tool, toolRenderConfig{maxExamples: 1})
	if !strings.Contains(out, "minimal") || strings.Contains(out, "common") {
		t.Errorf("maxExamples=1 should keep only the minimal example. Output:\n%s", out)
	}
	if strings.Count(out, "    - ") != 1 {
		t.Errorf("expected exactly 1 rendered example line. Output:\n%s", out)
	}
}

// TestRenderTool_NoExamplesOmitsExamplesLine asserts a tool without
// examples renders no `examples:` line at all — not `examples: []`
// (acceptance criterion).
func TestRenderTool_NoExamplesOmitsExamplesLine(t *testing.T) {
	t.Parallel()
	out := renderTool(tools.Tool{Name: "ping", Description: "health check"},
		toolRenderConfig{maxExamples: 3})
	if strings.Contains(out, "examples:") {
		t.Errorf("no-examples tool rendered an examples: line. Output:\n%s", out)
	}
	if strings.Contains(out, "args_schema:") {
		t.Errorf("no-schema tool rendered an args_schema: line. Output:\n%s", out)
	}
	// side_effects always renders, defaulting to pure.
	if !strings.Contains(out, "side_effects: pure") {
		t.Errorf("no-side-effects tool should default to pure. Output:\n%s", out)
	}
}

// TestRenderTool_SchemaRendersCompactSingleLine asserts the args_schema
// renders as compact (whitespace-free) JSON on one line.
func TestRenderTool_SchemaRendersCompactSingleLine(t *testing.T) {
	t.Parallel()
	tool := tools.Tool{
		Name: "t",
		ArgsSchema: json.RawMessage(
			"{\n  \"type\": \"object\",\n  \"properties\": { \"a\": { \"type\": \"string\" } }\n}"),
		SideEffects: tools.SideEffectPure,
	}
	out := renderTool(tool, toolRenderConfig{maxExamples: 3})
	var schemaLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "args_schema:") {
			schemaLine = ln
		}
	}
	if schemaLine == "" {
		t.Fatalf("no args_schema line rendered. Output:\n%s", out)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(schemaLine), "args_schema:"))
	if strings.ContainsAny(payload, "\n") {
		t.Errorf("args_schema JSON spans multiple lines: %q", payload)
	}
	if strings.Contains(payload, ": ") || strings.Contains(payload, ", ") {
		t.Errorf("args_schema JSON is not compact (has insignificant whitespace): %q", payload)
	}
}

// TestRenderAvailableTools_DefaultExampleCap asserts a zero maxExamples
// resolves to defaultMaxToolExamples rather than rendering no examples.
func TestRenderAvailableTools_DefaultExampleCap(t *testing.T) {
	t.Parallel()
	body := renderAvailableToolsSection(planner.RunContext{Catalog: fixtureCatalog()}, 0)
	if !strings.Contains(body, "examples:") {
		t.Errorf("zero cap should resolve to default (3), not suppress examples. Body:\n%s", body)
	}
}

// TestDefaultBuilder_GoldenToolsPrompt is the fixture-driven golden for
// the enriched <available_tools> rendering (acceptance criterion). The
// fixture is the normative spec for the per-tool block shape.
func TestDefaultBuilder_GoldenToolsPrompt(t *testing.T) {
	t.Parallel()
	const goldenPath = "testdata/golden_tools_prompt.txt"
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	got := renderAvailableToolsSection(planner.RunContext{Catalog: fixtureCatalog()}, 3)
	if got != string(want) {
		t.Errorf("rendered <available_tools> diverged from %s.\n"+
			"If this change is intentional, regenerate the fixture.\n"+
			"--- got ---\n%s\n--- want ---\n%s", goldenPath, got, string(want))
	}
}

// TestRenderTool_ConcurrentReuse_D025 is the Phase 83b (D-144)
// concurrent-reuse gate for the tool-catalog renderer. renderTool and
// renderAvailableToolsSection are pure functions — no shared state, no
// receiver — so N≥100 concurrent invocations against the SAME fixture
// catalog produce byte-identical output under -race: no data races,
// no cross-call bleed. N=128 (above the D-025 floor of 100).
func TestRenderTool_ConcurrentReuse_D025(t *testing.T) {
	t.Parallel()
	const N = 128
	cat := fixtureCatalog()
	want := renderAvailableToolsSection(planner.RunContext{Catalog: cat}, 3)

	var (
		wg       sync.WaitGroup
		mismatch int64
	)
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			got := renderAvailableToolsSection(planner.RunContext{Catalog: cat}, 3)
			if got != want {
				atomic.AddInt64(&mismatch, 1)
			}
		}()
	}
	wg.Wait()
	if mismatch != 0 {
		t.Errorf("D-025: %d concurrent catalog renders diverged from the reference", mismatch)
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
