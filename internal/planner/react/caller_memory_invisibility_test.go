// caller_memory_invisibility_test.go — the regression guard that makes
// "this phase is invisible to the planner" a fact rather than an
// intention (Phase 219 / D-364).
//
// Caller-supplied memory reaches the prompt as ONE MORE KEY inside the
// External tier's map. No wrapper changed, no rule line was added, no
// tag was renamed and the four-section injection order is untouched.
// That claim is only worth anything if a test would notice it breaking.
package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// composedExternalTier is what the runtime hands the planner once a
// caller has contributed: the runtime's own recall key alongside the
// fixed caller key, in ONE map, in ONE tier.
func composedExternalTier() map[string]any {
	return map[string]any{
		"recalled_turns": []map[string]any{{"user": "how do refunds work?", "assistant": "within 30 days"}},
		"caller_supplied": map[string]any{
			"note": "the caller's own recalled content",
		},
	}
}

// TestRenderMemoryBlock_ComposedExternalTier_WrapperUnchanged asserts a
// COMPOSED tier renders inside the same wrapper, behind the same
// five-line UNTRUSTED rules block, with the same tags — byte-for-byte
// against the golden's framing.
func TestRenderMemoryBlock_ComposedExternalTier_WrapperUnchanged(t *testing.T) {
	m, err := renderMemoryBlock(
		"read_only_external_memory", "external memory",
		memoryRulesExternal, composedExternalTier())
	if err != nil {
		t.Fatalf("renderMemoryBlock: %v", err)
	}
	if m.Role != llm.RoleSystem {
		t.Fatalf("Role = %q, want system — caller content must stay in the untrusted-framed system tier", m.Role)
	}
	body := msgText(t, m)

	// The golden fixture's framing is everything OUTSIDE the JSON body.
	// Split the golden on its own payload line and require both halves
	// verbatim, so a copy edit to the preamble or the rules block fails
	// here as loudly as it fails the golden test itself.
	golden := readGolden(t, "external_memory_wrapper.txt")
	const openTag = "<read_only_external_memory_json>\n"
	const closeTag = "\n</read_only_external_memory_json>"
	gi := strings.Index(golden, openTag)
	if gi < 0 {
		t.Fatalf("golden fixture has no %q — the wrapper shape changed", openTag)
	}
	prefix := golden[:gi+len(openTag)]
	if !strings.HasPrefix(body, prefix) {
		t.Fatalf("composed tier's framing diverged from the golden:\n got=%q\nwant prefix=%q", body, prefix)
	}
	if !strings.HasSuffix(body, closeTag+"\n</read_only_external_memory>") {
		t.Fatalf("composed tier's closing framing diverged from the golden: %q", body)
	}

	// Both producers' keys survive into the rendered payload, and they
	// are distinguishable — the whole point of composing at map-key
	// granularity rather than adding a third tier.
	for _, key := range []string{`"recalled_turns"`, `"caller_supplied"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("rendered tier does not carry %s — provenance must be legible in the prompt itself", key)
		}
	}
}

// TestRenderInjectionMessages_ComposedExternalTier_FourSectionOrder
// asserts a run carrying caller-supplied memory still produces the
// documented four-section order, with the caller's content in the
// EXTERNAL message and nowhere else.
func TestRenderInjectionMessages_ComposedExternalTier_FourSectionOrder(t *testing.T) {
	const marker = "phase219-invisibility-marker"
	external := composedExternalTier()
	external["caller_supplied"] = map[string]any{"note": marker}

	msgs, err := renderInjectionMessages(planner.RunContext{
		MemoryBlocks: &planner.MemoryBlocks{
			External:     external,
			Conversation: map[string]any{"strategy": "recent_turns"},
		},
		SkillsContext: []any{map[string]any{"id": "s1"}},
	})
	if err != nil {
		t.Fatalf("renderInjectionMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (external, conversation, skills)", len(msgs))
	}
	wantOrder := []string{
		"<read_only_external_memory>",
		"<read_only_conversation_memory>",
		"<skills_context>",
	}
	for i, want := range wantOrder {
		if !strings.Contains(msgText(t, msgs[i]), want) {
			t.Fatalf("message[%d] does not carry %q — the injection order changed", i, want)
		}
	}
	if !strings.Contains(msgText(t, msgs[0]), marker) {
		t.Fatal("the caller's marker is not in the external-memory message")
	}
	for i := 1; i < len(msgs); i++ {
		if strings.Contains(msgText(t, msgs[i]), marker) {
			t.Fatalf("the caller's marker leaked into message[%d] (%s) — caller content reaches ONE position", i, wantOrder[i])
		}
	}
}
