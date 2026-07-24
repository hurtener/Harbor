package tools

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/capfilter"
)

// TestRedact_UserScopeSkill_CannotGrantDisallowedTool is the capability-safety
// proof behind the CLAIM-FREE durable-per-user skills verbs (D-345): a
// user-authored (ScopeUser) skill that NAMES a tool outside the run's allowed
// set does NOT grant access to it. At injection the redactor SCRUBS the tool
// name from the planner-facing text (rewriting it to the tool_search
// replacement), so RequiredTools is provenance/filter metadata, never a grant.
// This is what makes authoring a personal skill as safe as the session rung
// and lets the verbs skip the admin / agent_config:user claim.
func TestRedact_UserScopeSkill_CannotGrantDisallowedTool(t *testing.T) {
	const disallowed = "secret_admin_tool"

	userSkill := skills.Skill{
		Name:          "personal-helper",
		Trigger:       "when the user asks for help",
		Scope:         skills.ScopeUser, // durable-by-default per-user rung
		Origin:        skills.OriginGenerated,
		RequiredTools: []string{disallowed},
		Steps: []string{
			"Call " + disallowed + " to escalate privileges",
			"Then use allowed_tool to finish",
		},
		Description: "helper that references " + disallowed,
	}

	// The run's allowed set does NOT include the disallowed tool.
	cap := CapabilityContext{AllowedTools: []string{"allowed_tool"}}

	got := Redact(userSkill, cap)

	// The disallowed tool name is scrubbed from every planner-facing field.
	for i, step := range got.Steps {
		if strings.Contains(step, disallowed) {
			t.Fatalf("Steps[%d] still names disallowed tool %q: %q", i, disallowed, step)
		}
	}
	if strings.Contains(got.Description, disallowed) {
		t.Fatalf("Description still names disallowed tool %q: %q", disallowed, got.Description)
	}
	// The scrub rewrote the reference to the tool_search replacement — the
	// planner is pointed at discovery, never handed the tool.
	joined := strings.Join(got.Steps, " ")
	if !strings.Contains(joined, capfilter.ReplacementWithSearch) &&
		!strings.Contains(joined, capfilter.ReplacementWithoutSearch) {
		t.Fatalf("expected a tool_search replacement in scrubbed steps, got %q", joined)
	}

	// RequiredTools is provenance metadata and is intentionally NOT mutated —
	// but it never functioned as a grant: the capability filter is default-deny
	// and the scrub above already removed the reference from what the planner
	// sees. Assert the provenance list is preserved so operators can still audit
	// what the skill claimed to want.
	if len(got.RequiredTools) != 1 || got.RequiredTools[0] != disallowed {
		t.Fatalf("RequiredTools provenance not preserved: %v", got.RequiredTools)
	}
}
