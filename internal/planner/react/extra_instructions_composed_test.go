// The composed two-producer additive-guidance value, rendered.
//
// `ExtraInstructions` now has two producers: the admin-set tenant-wide record
// and the per-run override. They are joined into ONE string before the bundle
// reaches the planner, so these tests exercise the JOINED value: both segments
// render in order, VERBATIM and unescaped (the property that distinguishes the
// operator-trusted `<additional_guidance>` position from the untrusted-framed
// `<user_instructions>`), and the composed value survives a
// SystemPromptOverride set in the same bundle.
package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// The joined shape ComposeLLMOverrides produces: tenant text, blank line,
// session text. The `<` in the session segment is the escaping probe.
const (
	composedTenantSeg  = "TENANT_ADDITIVE_SEGMENT"
	composedSessionSeg = "SESSION_ADDITIVE_SEGMENT <use the compare_a_b tool>"
	composedGuidance   = composedTenantSeg + "\n\n" + composedSessionSeg
)

// TestComposition_ExtraInstructionsRenderedVerbatim_TwoProducers proves the
// JOINED value lands in one `<additional_guidance>` block, both segments
// present and in order, and rendered VERBATIM — a `<` is NOT entity-escaped.
// Routing this position through escapeUntrustedSection turns the last
// assertion red, which is the point: the two positions must stay
// distinguishable.
func TestComposition_ExtraInstructionsRenderedVerbatim_TwoProducers(t *testing.T) {
	rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer:   sp("OPERATOR_BASE"),
		ExtraInstructions: sp(composedGuidance),
	}}
	body := systemBody(t, rc, "ignored")

	if !strings.Contains(body, "<additional_guidance>") {
		t.Fatalf("no <additional_guidance> section. Body: %s", body)
	}
	tenantIdx := strings.Index(body, composedTenantSeg)
	sessionIdx := strings.Index(body, composedSessionSeg)
	if tenantIdx < 0 {
		t.Fatalf("the tenant segment is missing. Body: %s", body)
	}
	if sessionIdx < 0 {
		t.Fatalf("the session segment is missing (or was escaped). Body: %s", body)
	}
	if tenantIdx >= sessionIdx {
		t.Fatalf("the tenant segment must render ABOVE the session segment (tenant@%d session@%d). Body: %s",
			tenantIdx, sessionIdx, body)
	}
	// ONE block, not two: exactly one opening and one closing tag.
	if got := strings.Count(body, "<additional_guidance>"); got != 1 {
		t.Fatalf("the two producers must share ONE guidance block (found %d). Body: %s", got, body)
	}
	// Both segments sit INSIDE that block.
	openIdx := strings.Index(body, "<additional_guidance>")
	closeIdx := strings.Index(body, "</additional_guidance>")
	if closeIdx < 0 || tenantIdx < openIdx || sessionIdx > closeIdx {
		t.Fatalf("a segment escaped the guidance block (open@%d close@%d tenant@%d session@%d). Body: %s",
			openIdx, closeIdx, tenantIdx, sessionIdx, body)
	}
	// VERBATIM: the position is operator-trusted, so its angle brackets are
	// NOT neutralised the way <user_instructions> neutralises them.
	if strings.Contains(body, "&lt;use the compare_a_b tool&gt;") {
		t.Fatalf("the additive guidance was ENTITY-ESCAPED; <additional_guidance> renders operator-trusted text verbatim. Body: %s", body)
	}
	if !strings.Contains(body, "<use the compare_a_b tool>") {
		t.Fatalf("the additive guidance did not render verbatim. Body: %s", body)
	}
}

// TestComposition_ComposedExtraInstructionsSurviveSystemPromptOverride
// extends the single-producer additive-survives-replace property to the
// COMPOSED two-producer value: a session that replaces the whole base+user
// spine in the same request still carries BOTH guidance segments.
func TestComposition_ComposedExtraInstructionsSurviveSystemPromptOverride(t *testing.T) {
	rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer:      sp("OPERATOR_BASE"),
		UserPromptLayer:      sp("USER_LAYER"),
		SystemPromptOverride: sp("ONE_SHOT_REPLACE"),
		ExtraInstructions:    sp(composedGuidance),
	}}
	body := systemBody(t, rc, "ignored")

	if !strings.Contains(body, "ONE_SHOT_REPLACE") {
		t.Fatalf("the session override should replace the spine. Body: %s", body)
	}
	if strings.Contains(body, "OPERATOR_BASE") || strings.Contains(body, "USER_LAYER") {
		t.Fatalf("the durable spine should be suppressed under a session override. Body: %s", body)
	}
	tenantIdx := strings.Index(body, composedTenantSeg)
	sessionIdx := strings.Index(body, composedSessionSeg)
	if tenantIdx < 0 || sessionIdx < 0 {
		t.Fatalf("the composed additive guidance was dropped under a system-prompt REPLACE (tenant@%d session@%d). Body: %s",
			tenantIdx, sessionIdx, body)
	}
	if tenantIdx >= sessionIdx {
		t.Fatalf("segment order lost under a replace (tenant@%d session@%d). Body: %s", tenantIdx, sessionIdx, body)
	}
}

// TestComposition_AbsentExtraInstructionsRendersNoGuidanceSection is the
// "absent ⇒ unchanged" pin: with no additive guidance in the bundle and no
// operator-baked guidance, no `<additional_guidance>` section is rendered at
// all, and the body is byte-identical to the body an LLMOverrides bundle
// without the dimension produces.
func TestComposition_AbsentExtraInstructionsRendersNoGuidanceSection(t *testing.T) {
	withField := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer:   sp("OPERATOR_BASE"),
		UserPromptLayer:   sp("USER_LAYER"),
		ExtraInstructions: nil, // explicitly absent
	}}
	withoutField := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer: sp("OPERATOR_BASE"),
		UserPromptLayer: sp("USER_LAYER"),
	}}
	a := systemBody(t, withField, "ignored")
	b := systemBody(t, withoutField, "ignored")
	if a != b {
		t.Fatalf("an absent extra_instructions changed the rendered system content.\n--- with ---\n%s\n--- without ---\n%s", a, b)
	}
	if strings.Contains(a, "<additional_guidance>") {
		t.Fatalf("no guidance contributed, yet an empty <additional_guidance> section rendered. Body: %s", a)
	}
}
