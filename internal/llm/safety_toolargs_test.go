package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// White-box tests for the tool-call ARGUMENTS arm of the heavy-content
// byte check.
//
// `ToolCalls[].Args` is the one field on an outbound request that
// reaches a provider carrying offloadable content without passing
// through `Content`: the React prompt builder replays a trajectory
// step's args into it on every turn it re-renders, and the driver
// translators map it straight onto the provider's tool_calls block. The
// check walks it for the same reason it walks `RoleTool` text — the
// content is machine-authored, tool-shaped, and has an ArtifactStub
// offload path — and for no wider reason: the threshold, the
// conversation-text exemption, the error type and the event are all
// unchanged.
//
// The alternative to walking it is the claim that nothing ever puts
// heavy content there. That claim restates the invariant the runtime's
// production-side bound asserts, and an invariant with no arrival-side
// check is asserted rather than enforced — the arrival being exactly
// where a violation becomes observable to a provider.

// argsOfSize builds a syntactically valid tool-call args document whose
// encoded length is EXACTLY n bytes (for n >= framing), so the threshold
// boundary can be asserted rather than approximated.
func argsOfSize(n int) json.RawMessage {
	// `{"doc":` + `"` + payload + `"` + `}` — ten framing bytes.
	const framing = 10
	pad := n - framing
	if pad < 0 {
		pad = 0
	}
	return json.RawMessage(fmt.Sprintf(`{"doc":%q}`, strings.Repeat("Z", pad)))
}

// asstWithToolCall builds the assistant/tool pair the prompt builder
// emits, so the request is structurally valid for the whole safety pass
// and not only for findContextLeak.
func asstWithToolCall(args json.RawMessage) []ChatMessage {
	toolText := "ok"
	return []ChatMessage{
		{
			Role:      RoleAssistant,
			ToolCalls: []ToolCallStructured{{ID: "c1", Name: "summarize", Args: args}},
		},
		{Role: RoleTool, ToolCallID: toolCallID("c1"), Content: Content{Text: &toolText}},
	}
}

// TestFindContextLeak_ToolCallArgs_OversizeArgsLeak — the widening
// itself. Before it, this request reached the provider carrying the
// payload verbatim.
func TestFindContextLeak_ToolCallArgs_OversizeArgsLeak(t *testing.T) {
	args := argsOfSize(rsThreshold + 1)
	req := CompleteRequest{Model: "m", Messages: asstWithToolCall(args)}

	site, size, ok := findContextLeak(req, rsThreshold)
	if !ok {
		t.Fatal("oversize ToolCalls[].Args was not flagged as a leak")
	}
	if size != len(args) {
		t.Errorf("size=%d, want %d", size, len(args))
	}
	if want := "Messages[0].ToolCalls[0].Args"; site != want {
		t.Errorf("site=%q, want %q", site, want)
	}
}

// TestFindContextLeak_ToolCallArgs_LegitimateArgsPass — the guard is a
// byte threshold, not a ban on tool-call arguments. An ordinary call
// (the overwhelming majority) is untouched, so the widening cannot be
// green by rejecting everything.
func TestFindContextLeak_ToolCallArgs_LegitimateArgsPass(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"doc":"tool_ab12cd34ef56","max_words":200}`),
		argsOfSize(rsThreshold - 1),
	}
	for i, args := range cases {
		req := CompleteRequest{Model: "m", Messages: asstWithToolCall(args)}
		if site, size, ok := findContextLeak(req, rsThreshold); ok {
			t.Fatalf("case %d (%d bytes) was flagged at %s size=%d; a legitimately-small args document must pass",
				i, len(args), site, size)
		}
	}
}

// TestFindContextLeak_ToolCallArgs_ThresholdBoundaryMatchesEveryOtherArm
// — the arm reuses the existing `>=` comparison rather than introducing
// its own boundary.
func TestFindContextLeak_ToolCallArgs_ThresholdBoundaryMatchesEveryOtherArm(t *testing.T) {
	atThreshold := argsOfSize(rsThreshold)
	if len(atThreshold) != rsThreshold {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atThreshold), rsThreshold)
	}
	req := CompleteRequest{Model: "m", Messages: asstWithToolCall(atThreshold)}
	if _, _, ok := findContextLeak(req, rsThreshold); !ok {
		t.Fatal("args exactly AT the threshold did not trip the check; every other arm uses >=")
	}

	below := argsOfSize(rsThreshold - 1)
	req = CompleteRequest{Model: "m", Messages: asstWithToolCall(below)}
	if _, _, ok := findContextLeak(req, rsThreshold); ok {
		t.Fatal("args one byte BELOW the threshold tripped the check")
	}
}

// TestFindContextLeak_ToolCallArgs_ParallelBranchIsNamedByIndex — the
// prompt builder emits one assistant message carrying every branch's
// call, so the site must identify WHICH branch to be actionable.
func TestFindContextLeak_ToolCallArgs_ParallelBranchIsNamedByIndex(t *testing.T) {
	small := json.RawMessage(`{"city":"Lisbon"}`)
	heavy := argsOfSize(rsThreshold + 1)
	okText := "ok"
	req := CompleteRequest{
		Model: "m",
		Messages: []ChatMessage{
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCallStructured{
					{ID: "c1", Name: "weather", Args: small},
					{ID: "c2", Name: "summarize", Args: heavy},
				},
			},
			{Role: RoleTool, ToolCallID: toolCallID("c1"), Content: Content{Text: &okText}},
			{Role: RoleTool, ToolCallID: toolCallID("c2"), Content: Content{Text: &okText}},
		},
	}
	site, _, ok := findContextLeak(req, rsThreshold)
	if !ok {
		t.Fatal("an oversize parallel branch's args was not flagged")
	}
	if want := "Messages[0].ToolCalls[1].Args"; site != want {
		t.Errorf("site=%q, want %q", site, want)
	}
}

// TestFindContextLeak_ToolCallArgs_ConversationTextStaysExempt — the
// widening adds ONE field. It does not reopen the conversation-text
// exemption, which stays governed by the token-window guard.
func TestFindContextLeak_ToolCallArgs_ConversationTextStaysExempt(t *testing.T) {
	heavy := rsHeavy()
	req := CompleteRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: RoleUser, Content: Content{Text: &heavy}},
			{
				Role:      RoleAssistant,
				ToolCalls: []ToolCallStructured{{ID: "c1", Name: "t", Args: json.RawMessage(`{"a":1}`)}},
			},
			{Role: RoleTool, ToolCallID: toolCallID("c1"), Content: Content{Text: &heavy}},
		},
	}
	site, _, ok := findContextLeak(req, rsThreshold)
	if !ok {
		t.Fatal("the RoleTool text in this request must still be flagged")
	}
	if want := "Messages[2].Content.Text"; site != want {
		t.Errorf("site=%q, want %q — the heavy USER turn must stay exempt and the tool text must be the hit", site, want)
	}
}
