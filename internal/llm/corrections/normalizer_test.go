package corrections

import (
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func msg(role llm.Role, text string) llm.ChatMessage {
	t := text
	return llm.ChatMessage{Role: role, Content: llm.Content{Text: &t}}
}

func TestNormalizer_SystemFirstStrict_ReordersInterleavedSystem(t *testing.T) {
	in := []llm.ChatMessage{
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleSystem, "s1"),
		msg(llm.RoleAssistant, "a1"),
		msg(llm.RoleSystem, "s2"),
		msg(llm.RoleUser, "u2"),
	}
	out, err := normalizeMessages(in, llm.OrderingSystemFirstStrict)
	if err != nil {
		t.Fatalf("normalizeMessages: %v", err)
	}
	want := []llm.Role{
		llm.RoleSystem, llm.RoleSystem,
		llm.RoleUser, llm.RoleAssistant, llm.RoleUser,
	}
	if len(out) != len(want) {
		t.Fatalf("len: got %d want %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i].Role != w {
			t.Errorf("out[%d].Role: got %q want %q", i, out[i].Role, w)
		}
	}
	// Stability — first system message before reorder is s1; should
	// be first in output.
	if t1 := out[0].Content.Text; t1 == nil || *t1 != "s1" {
		t.Errorf("out[0].Text: got %v want %q", t1, "s1")
	}
	if t2 := out[1].Content.Text; t2 == nil || *t2 != "s2" {
		t.Errorf("out[1].Text: got %v want %q", t2, "s2")
	}
}

func TestNormalizer_SystemFirstStrict_NoSystemMessages_IsPassthrough(t *testing.T) {
	in := []llm.ChatMessage{
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleAssistant, "a1"),
		msg(llm.RoleUser, "u2"),
	}
	out, err := normalizeMessages(in, llm.OrderingSystemFirstStrict)
	if err != nil {
		t.Fatalf("normalizeMessages: %v", err)
	}
	for i := range in {
		if out[i].Role != in[i].Role {
			t.Errorf("out[%d].Role: got %q want %q", i, out[i].Role, in[i].Role)
		}
	}
}

func TestNormalizer_Default_CopiesSliceUnchanged(t *testing.T) {
	in := []llm.ChatMessage{
		msg(llm.RoleSystem, "s"),
		msg(llm.RoleUser, "u"),
	}
	out, err := normalizeMessages(in, llm.OrderingDefault)
	if err != nil {
		t.Fatalf("normalizeMessages: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Role != in[i].Role {
			t.Errorf("out[%d].Role: got %q want %q", i, out[i].Role, in[i].Role)
		}
	}
	// Verify the output is NOT the same backing array — concurrent
	// callers should never share mutable state.
	if len(out) > 0 && len(in) > 0 {
		// Mutate output and verify input untouched.
		out[0].Role = "marker"
		if in[0].Role == "marker" {
			t.Errorf("normalizeMessages returned input slice (no copy)")
		}
	}
}

func TestNormalizer_UnknownPolicy_FailsLoudly(t *testing.T) {
	in := []llm.ChatMessage{msg(llm.RoleUser, "u")}
	_, err := normalizeMessages(in, "not_a_real_policy")
	if err == nil {
		t.Fatalf("expected error for unknown policy, got nil")
	}
}
