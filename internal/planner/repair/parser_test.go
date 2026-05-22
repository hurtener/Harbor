package repair_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/repair"
)

// TestParser_GreedyDecode_SingleObject verifies the happy path: a
// clean JSON envelope decodes to one CallTool.
func TestParser_GreedyDecode_SingleObject(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := `{"tool":"search","args":{"q":"hello"},"reasoning":"user wants info"}`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if actions[0].Tool != "search" {
		t.Errorf("Tool = %q, want %q", actions[0].Tool, "search")
	}
	if string(actions[0].Args) != `{"q":"hello"}` {
		t.Errorf("Args = %q, want %q", string(actions[0].Args), `{"q":"hello"}`)
	}
	// Phase 83e (D-147): the action schema is narrowed to {tool, args}.
	// The incoming `reasoning` field is stripped silently; the parser
	// reports it via DroppedExtraFields so the loop can emit telemetry.
	dropped := repair.DroppedExtraFields(text)
	if len(dropped) != 1 || dropped[0] != "reasoning" {
		t.Errorf("DroppedExtraFields = %v, want [reasoning]", dropped)
	}
}

// TestParser_GreedyDecode_Array verifies the multi-action happy path:
// a JSON array of envelopes decodes to N CallTools in source order.
func TestParser_GreedyDecode_Array(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := `[
  {"tool":"a","args":{"x":1}},
  {"tool":"b","args":{"y":2}}
]`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if actions[0].Tool != "a" || actions[1].Tool != "b" {
		t.Errorf("order not preserved: %q, %q", actions[0].Tool, actions[1].Tool)
	}
}

// TestParser_FencedJSON verifies fenced JSON blocks are extracted.
func TestParser_FencedJSON(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := "Here is my action:\n```json\n{\"tool\":\"weather\",\"args\":{\"city\":\"sf\"}}\n```\nThat should do it."

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 || actions[0].Tool != "weather" {
		t.Fatalf("got %v; want one action tool=weather", actions)
	}
}

// TestParser_FencedJSON_Bare verifies bare-fence blocks (no language
// label) are also extracted.
func TestParser_FencedJSON_Bare(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := "Sure!\n```\n{\"tool\":\"search\",\"args\":{}}\n```"

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 || actions[0].Tool != "search" {
		t.Fatalf("got %v; want one action tool=search", actions)
	}
}

// TestParser_MultipleFences verifies multiple fenced blocks produce
// multiple actions in source order.
func TestParser_MultipleFences(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := "Plan:\n```json\n{\"tool\":\"first\",\"args\":{}}\n```\nThen:\n```json\n{\"tool\":\"second\",\"args\":{}}\n```"

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if actions[0].Tool != "first" || actions[1].Tool != "second" {
		t.Errorf("order not preserved: %q %q", actions[0].Tool, actions[1].Tool)
	}
}

// TestParser_DecoderScan_ProseWrapped verifies the scan path picks
// up JSON embedded in prose (no fences).
func TestParser_DecoderScan_ProseWrapped(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	// No fences and the prose makes greedy decode fail. The scan
	// path should find the object.
	text := `Sure, here's the action: {"tool":"calc","args":{"expr":"1+2"}} hope that helps!`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 || actions[0].Tool != "calc" {
		t.Fatalf("got %v; want one action tool=calc", actions)
	}
}

// TestParser_DecoderScan_TwoObjects verifies the scan picks up two
// adjacent JSON objects.
func TestParser_DecoderScan_TwoObjects(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := `First: {"tool":"a","args":{"i":1}} and then: {"tool":"b","args":{"i":2}}`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if actions[0].Tool != "a" || actions[1].Tool != "b" {
		t.Errorf("order not preserved: %q %q", actions[0].Tool, actions[1].Tool)
	}
}

// TestParser_EmptyString verifies an empty string returns
// ErrNoActionsFound.
func TestParser_EmptyString(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	_, err := p.Parse("")
	if !errors.Is(err, repair.ErrNoActionsFound) {
		t.Fatalf("want ErrNoActionsFound, got %v", err)
	}
}

// TestParser_WhitespaceOnly verifies whitespace-only text returns
// ErrNoActionsFound.
func TestParser_WhitespaceOnly(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	_, err := p.Parse("   \n\t  ")
	if !errors.Is(err, repair.ErrNoActionsFound) {
		t.Fatalf("want ErrNoActionsFound, got %v", err)
	}
}

// TestParser_MalformedJSON_NoCloseBrace verifies a truncated JSON
// returns ErrNoActionsFound (parser refuses to guess).
func TestParser_MalformedJSON_NoCloseBrace(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	_, err := p.Parse(`{"tool":"foo","args":{`)
	if !errors.Is(err, repair.ErrNoActionsFound) {
		t.Fatalf("want ErrNoActionsFound, got %v", err)
	}
}

// TestParser_MissingTool verifies an envelope missing the `tool`
// field returns ErrNoActionsFound (a JSON object without `tool` is
// not a valid action).
func TestParser_MissingTool(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	_, err := p.Parse(`{"args":{"x":1}}`)
	if !errors.Is(err, repair.ErrNoActionsFound) {
		t.Fatalf("want ErrNoActionsFound, got %v", err)
	}
}

// TestParser_DefaultArgsToEmptyObject verifies an envelope with a
// `tool` field but no `args` gets `args` defaulted to `{}` so the
// downstream validator sees a well-shaped value.
func TestParser_DefaultArgsToEmptyObject(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	actions, err := p.Parse(`{"tool":"ping"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if string(actions[0].Args) != "{}" {
		t.Errorf("Args = %q, want %q", string(actions[0].Args), "{}")
	}
}

// TestParser_ArrayWithBadEntry verifies that a JSON array with a
// malformed entry returns the well-shaped entries only.
func TestParser_ArrayWithBadEntry(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	// First entry is malformed (missing tool); second is valid.
	text := `[{"args":{}}, {"tool":"b","args":{}}]`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(actions) != 1 || actions[0].Tool != "b" {
		t.Fatalf("got %v; want one action tool=b", actions)
	}
}

// TestParser_NestedFences verifies a python fence followed by a json
// fence picks up the json fence only (brief 07 §10 sharp edge).
func TestParser_NestedFences(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	text := "Example code:\n```python\nprint('hi')\n```\nNow the action:\n```json\n{\"tool\":\"run\",\"args\":{\"code\":\"x\"}}\n```"

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// We expect ONLY the json fence to produce an action — the
	// python fence's body is not valid JSON, so the fenced-block
	// pass returns nothing for it.
	if len(actions) != 1 || actions[0].Tool != "run" {
		t.Fatalf("got %v; want one action tool=run", actions)
	}
}

// TestParser_PreservesArgsBytes verifies the parser preserves the
// LLM's exact arg bytes (no re-marshalling). The downstream
// schema validator depends on byte-exact preservation for
// deterministic error messages.
func TestParser_PreservesArgsBytes(t *testing.T) {
	t.Parallel()
	p := repair.NewParser()
	// Use a specific key ordering + whitespace that re-marshalling
	// would normalise away.
	text := `{"tool":"x","args":{"b":2,"a":1}}`

	actions, err := p.Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(string(actions[0].Args), `"b":2`) {
		t.Errorf("args were re-marshalled — original byte order lost: %s", string(actions[0].Args))
	}
}

// TestDroppedExtraFields_Phase83e covers the D-147 extra-field scanner
// across the parser's salvage paths: single object, fenced block,
// prose-wrapped object, multi-action array, and the clean (no-extra)
// case.
func TestDroppedExtraFields_Phase83e(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"clean_object", `{"tool":"x","args":{}}`, nil},
		{"reasoning_field", `{"tool":"x","args":{},"reasoning":"r"}`, []string{"reasoning"}},
		{"thought_field", `{"tool":"x","args":{},"thought":"t"}`, []string{"thought"}},
		{"both_fields", `{"tool":"x","args":{},"reasoning":"r","thought":"t"}`, []string{"reasoning", "thought"}},
		{"fenced", "```json\n{\"tool\":\"x\",\"args\":{},\"reasoning\":\"r\"}\n```", []string{"reasoning"}},
		{"prose_wrapped", `Here is my action: {"tool":"x","args":{},"thought":"t"}`, []string{"thought"}},
		{"array_two", `[{"tool":"a","args":{},"reasoning":"r1"},{"tool":"b","args":{},"reasoning":"r2"}]`, []string{"reasoning", "reasoning"}},
		{"empty", ``, nil},
		{"junk", `not json at all`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := repair.DroppedExtraFields(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("DroppedExtraFields(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("DroppedExtraFields[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
