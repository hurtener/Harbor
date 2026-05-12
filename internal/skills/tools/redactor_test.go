package tools

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestRedact_DisallowedToolNameReplaced_WithSearch(t *testing.T) {
	t.Parallel()

	s := skills.Skill{
		Name:          "demo",
		Title:         "Use fs_write to persist results",
		Description:   "Call fs_write with the file path.",
		Trigger:       "save artifact via fs_write",
		Steps:         []string{"step1: fs_write to disk", "step2: emit"},
		RequiredTools: []string{"fs_write"},
	}
	cap := CapabilityContext{AllowedTools: []string{"tool_search"}}
	got := Redact(s, cap)

	for _, field := range []string{got.Title, got.Description, got.Trigger} {
		if strings.Contains(field, "fs_write") {
			t.Fatalf("disallowed tool name leaked: %q", field)
		}
		if !strings.Contains(field, replaceWithSearch) {
			t.Fatalf("expected %q replacement, got %q", replaceWithSearch, field)
		}
	}
	for i, step := range got.Steps {
		if strings.Contains(step, "fs_write") {
			t.Fatalf("steps[%d] leaked disallowed name: %q", i, step)
		}
	}
}

func TestRedact_DisallowedToolNameReplaced_WithoutSearch(t *testing.T) {
	t.Parallel()

	s := skills.Skill{
		Title:         "Use fs_write to persist results",
		RequiredTools: []string{"fs_write"},
	}
	cap := CapabilityContext{AllowedTools: []string{"http_fetch"}} // no tool_search
	got := Redact(s, cap)

	if !strings.Contains(got.Title, replaceWithoutSearch) {
		t.Fatalf("got Title=%q, want bare replacement %q", got.Title, replaceWithoutSearch)
	}
	if strings.Contains(got.Title, "(use tool_search)") {
		t.Fatalf("got Title=%q, must not include the search-aware variant when tool_search is not allowed", got.Title)
	}
}

func TestRedact_AllowedToolNameKept(t *testing.T) {
	t.Parallel()

	s := skills.Skill{
		Title:         "Use http_fetch to GET the page",
		RequiredTools: []string{"http_fetch"},
	}
	cap := CapabilityContext{AllowedTools: []string{"http_fetch", "tool_search"}}
	got := Redact(s, cap)

	if !strings.Contains(got.Title, "http_fetch") {
		t.Fatalf("got Title=%q, allowed tool name http_fetch was scrubbed", got.Title)
	}
}

func TestRedact_WordBoundaryAvoidsFalsePositive(t *testing.T) {
	t.Parallel()

	// A skill that uses 'email' as a tool name; the text contains
	// 'emails' (plural). Word-boundary regex must NOT redact the
	// plural noun.
	s := skills.Skill{
		Title:         "Send notification emails to users",
		RequiredTools: []string{"email"},
	}
	cap := CapabilityContext{AllowedTools: nil} // email is disallowed
	got := Redact(s, cap)
	if !strings.Contains(got.Title, "emails") {
		t.Fatalf("got Title=%q, word-boundary failed — plural noun 'emails' should survive", got.Title)
	}
}

func TestRedact_PII_Disabled_LeavesPIIAlone(t *testing.T) {
	t.Parallel()

	s := skills.Skill{
		Title:       "Contact: alice@example.com or +1 555-123-4567",
		Description: "Authorization: Bearer abc.def.ghi",
		Trigger:     "https://example.com/api?token=secret&key=v",
		Steps:       []string{"email: a@b.com"},
	}
	cap := CapabilityContext{RedactPII: false}
	got := Redact(s, cap)
	if !strings.Contains(got.Title, "alice@example.com") {
		t.Fatalf("got Title=%q, expected raw email when RedactPII=false", got.Title)
	}
	if !strings.Contains(got.Description, "Bearer abc.def.ghi") {
		t.Fatalf("got Description=%q, expected raw bearer when RedactPII=false", got.Description)
	}
}

func TestRedact_PII_Enabled_RedactsEmail(t *testing.T) {
	t.Parallel()

	s := skills.Skill{Title: "Contact: alice@example.com"}
	cap := CapabilityContext{RedactPII: true}
	got := Redact(s, cap)
	if strings.Contains(got.Title, "alice@example.com") {
		t.Fatalf("got Title=%q, email should be redacted", got.Title)
	}
	if !strings.Contains(got.Title, piiPlaceholder) {
		t.Fatalf("got Title=%q, expected %q marker", got.Title, piiPlaceholder)
	}
}

func TestRedact_PII_Enabled_RedactsBearer(t *testing.T) {
	t.Parallel()

	s := skills.Skill{Description: "Use Bearer abc.def.ghi for auth"}
	cap := CapabilityContext{RedactPII: true}
	got := Redact(s, cap)
	if strings.Contains(got.Description, "abc.def.ghi") {
		t.Fatalf("got Description=%q, bearer token leaked", got.Description)
	}
	if !strings.Contains(got.Description, piiPlaceholder) {
		t.Fatalf("got Description=%q, expected %q marker", got.Description, piiPlaceholder)
	}
}

func TestRedact_PII_Enabled_RedactsURLQuery(t *testing.T) {
	t.Parallel()

	s := skills.Skill{Trigger: "Hit https://example.com/api?token=secret&key=v for details"}
	cap := CapabilityContext{RedactPII: true}
	got := Redact(s, cap)
	if strings.Contains(got.Trigger, "token=secret") {
		t.Fatalf("got Trigger=%q, query string leaked", got.Trigger)
	}
	if !strings.Contains(got.Trigger, "https://example.com/api") {
		t.Fatalf("got Trigger=%q, URL path was over-redacted", got.Trigger)
	}
}

func TestRedact_PII_Enabled_AcrossEverySection(t *testing.T) {
	t.Parallel()

	s := skills.Skill{
		Title:         "alice@example.com",
		Description:   "alice@example.com",
		Trigger:       "alice@example.com",
		Steps:         []string{"alice@example.com"},
		Preconditions: []string{"alice@example.com"},
		FailureModes:  []string{"alice@example.com"},
	}
	cap := CapabilityContext{RedactPII: true}
	got := Redact(s, cap)
	for _, f := range []string{got.Title, got.Description, got.Trigger} {
		if strings.Contains(f, "alice@example.com") {
			t.Fatalf("scalar field leaked PII: %q", f)
		}
	}
	for i, e := range got.Steps {
		if strings.Contains(e, "alice@example.com") {
			t.Fatalf("steps[%d] leaked PII: %q", i, e)
		}
	}
	for i, e := range got.Preconditions {
		if strings.Contains(e, "alice@example.com") {
			t.Fatalf("preconditions[%d] leaked PII: %q", i, e)
		}
	}
	for i, e := range got.FailureModes {
		if strings.Contains(e, "alice@example.com") {
			t.Fatalf("failure_modes[%d] leaked PII: %q", i, e)
		}
	}
}

func TestRedact_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := skills.Skill{
		Title:         "Use fs_write",
		Steps:         []string{"call fs_write"},
		RequiredTools: []string{"fs_write"},
	}
	originalTitle := in.Title
	originalSteps := in.Steps[0]

	_ = Redact(in, CapabilityContext{})

	if in.Title != originalTitle {
		t.Fatalf("input Title mutated: %q vs %q", in.Title, originalTitle)
	}
	if in.Steps[0] != originalSteps {
		t.Fatalf("input Steps[0] mutated: %q vs %q", in.Steps[0], originalSteps)
	}
}

func TestRedact_NilSlicesStayNil(t *testing.T) {
	t.Parallel()

	s := skills.Skill{Title: "demo"}
	got := Redact(s, CapabilityContext{})
	if got.Steps != nil {
		t.Fatalf("nil Steps became %v", got.Steps)
	}
}
