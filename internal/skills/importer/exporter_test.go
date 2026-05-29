package importer

import (
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func TestDoExport_SynthesisedFrontmatter(t *testing.T) {
	skill := skills.Skill{
		Name:    "synth",
		Trigger: "when synthesised",
		Steps:   []string{"a", "b"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}
	out, err := doExport(skill, ImportArtifacts{})
	if err != nil {
		t.Fatalf("doExport: %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "---\n") {
		t.Errorf("output missing opening fence: %q", s)
	}
	if !strings.Contains(s, "name: synth\n") {
		t.Errorf("missing name: %q", s)
	}
	if !strings.Contains(s, "trigger: when synthesised\n") {
		t.Errorf("missing trigger: %q", s)
	}
	if !strings.Contains(s, "## Steps\n\n- a\n- b\n") {
		t.Errorf("steps not in canonical form: %q", s)
	}
}

func TestDoExport_OmitsEmptySections(t *testing.T) {
	skill := skills.Skill{
		Name:    "no-sections",
		Trigger: "when no sections",
		Steps:   []string{"only step"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}
	out, err := doExport(skill, ImportArtifacts{})
	if err != nil {
		t.Fatalf("doExport: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "## Preconditions") {
		t.Errorf("Preconditions section should be omitted: %q", s)
	}
	if strings.Contains(s, "## Failure modes") {
		t.Errorf("Failure modes section should be omitted: %q", s)
	}
}

func TestDoExport_CanonicalSectionOrder(t *testing.T) {
	skill := skills.Skill{
		Name:          "ordered",
		Trigger:       "when ordering matters",
		Steps:         []string{"s"},
		Preconditions: []string{"p"},
		FailureModes:  []string{"f"},
		Origin:        skills.OriginPack,
		Scope:         skills.ScopeProject,
	}
	out, err := doExport(skill, ImportArtifacts{})
	if err != nil {
		t.Fatalf("doExport: %v", err)
	}
	s := string(out)
	stepsIdx := strings.Index(s, "## Steps")
	preIdx := strings.Index(s, "## Preconditions")
	failIdx := strings.Index(s, "## Failure modes")
	if stepsIdx < 0 || preIdx < 0 || failIdx < 0 {
		t.Fatalf("missing one of the canonical sections in %q", s)
	}
	if stepsIdx >= preIdx || preIdx >= failIdx {
		t.Errorf("section order wrong: Steps@%d Pre@%d Fail@%d", stepsIdx, preIdx, failIdx)
	}
}

func TestDesubstituteArtifacts_HappyPath(t *testing.T) {
	in := "before ![alt](artifact://abc_123) after"
	idToPath := map[string]string{"abc_123": "attachments/foo.txt"}
	got, err := desubstituteArtifacts(in, idToPath)
	if err != nil {
		t.Fatalf("desubstituteArtifacts: %v", err)
	}
	want := "before ![alt](attachments/foo.txt) after"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDesubstituteArtifacts_DanglingRef(t *testing.T) {
	in := "![alt](artifact://dangling_xyz)"
	_, err := desubstituteArtifacts(in, map[string]string{})
	if !errors.Is(err, ErrInvalidAttachmentRef) {
		t.Errorf("err = %v, want ErrInvalidAttachmentRef", err)
	}
}

func TestDesubstituteArtifacts_NoRefsPassThrough(t *testing.T) {
	in := "plain text without refs"
	got, err := desubstituteArtifacts(in, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestSynthesiseFrontmatter_EmptyFieldsOmitted(t *testing.T) {
	out := synthesiseFrontmatter(skills.Skill{
		Name:    "min",
		Trigger: "t",
		Steps:   []string{"s"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	})
	s := string(out)
	if strings.Contains(s, "title:") {
		t.Errorf("title should be omitted")
	}
	if strings.Contains(s, "tags:") {
		t.Errorf("tags should be omitted")
	}
	if strings.Contains(s, "scope:") {
		t.Errorf("default scope=project should be omitted")
	}
}

func TestSynthesiseFrontmatter_NonDefaultScopeEmitted(t *testing.T) {
	out := synthesiseFrontmatter(skills.Skill{
		Name:    "tenant-scoped",
		Trigger: "t",
		Steps:   []string{"s"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeTenant,
	})
	if !strings.Contains(string(out), "scope: tenant\n") {
		t.Errorf("non-default scope should be emitted: %q", out)
	}
}
