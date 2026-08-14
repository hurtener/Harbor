package skillpkg_test

import (
	"errors"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

const (
	validSkillMD = "---\nname: demo\ntrigger: when demo\n---\nDemo description.\n\n## Steps\n- step one\n- step two\n\n## Preconditions\n- pre\n\n## Failure modes\n- fail\n"
)

func rootEntry(path, data string) skillpkg.ArchiveEntry {
	return skillpkg.ArchiveEntry{Path: path, Data: []byte(data)}
}

func TestValidateRootSkillEntries_Valid(t *testing.T) {
	entries := []skillpkg.ArchiveEntry{
		rootEntry("SKILL.md", validSkillMD),
		rootEntry("examples/demo.json", `{"demo":true}`),
		rootEntry("LICENSE", "MIT"),
	}
	if err := skillpkg.ValidateRootSkillEntries(entries); err != nil {
		t.Fatalf("ValidateRootSkillEntries: %v", err)
	}
}

func TestValidateRootSkillEntries_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		entries []skillpkg.ArchiveEntry
		wantErr error
	}{
		{"missing", []skillpkg.ArchiveEntry{rootEntry("README.md", "hi")}, skillpkg.ErrSkillMDMissing},
		{"empty", nil, skillpkg.ErrSkillMDMissing},
		{"case mismatch root", []skillpkg.ArchiveEntry{rootEntry("skill.md", validSkillMD)}, skillpkg.ErrSkillMDCaseMismatch},
		{"case mismatch root all caps", []skillpkg.ArchiveEntry{rootEntry("SKILL.MD", validSkillMD)}, skillpkg.ErrSkillMDCaseMismatch},
		{"not root", []skillpkg.ArchiveEntry{rootEntry("docs/SKILL.md", validSkillMD)}, skillpkg.ErrSkillMDNotRoot},
		{"not root case variant", []skillpkg.ArchiveEntry{rootEntry("docs/skill.md", validSkillMD)}, skillpkg.ErrSkillMDNotRoot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := skillpkg.ValidateRootSkillEntries(c.entries)
			if err == nil {
				t.Fatalf("expected %v, got nil", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err=%v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestValidateSkillMarkdown_Valid(t *testing.T) {
	if err := skillpkg.ValidateSkillMarkdown([]byte(validSkillMD), skillpkg.MarkdownLimits{}); err != nil {
		t.Fatalf("ValidateSkillMarkdown: %v", err)
	}
	// "## steps:" and "## Steps" variants are accepted by the
	// canonical section classification.
	for _, body := range []string{
		"---\ntrigger: t\n---\n## steps:\n- do it\n",
		"---\ntrigger: t\n---\nintro\n## Steps\n- do it\n- and this\n",
	} {
		if err := skillpkg.ValidateSkillMarkdown([]byte(body), skillpkg.MarkdownLimits{}); err != nil {
			t.Fatalf("ValidateSkillMarkdown(%q): %v", body, err)
		}
	}
}

func TestValidateSkillMarkdown_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"empty", "", skillpkg.ErrSkillMDFrontmatterMissing},
		{"no frontmatter", "plain text", skillpkg.ErrSkillMDFrontmatterMissing},
		{"no closing fence", "---\ntrigger: t\n", skillpkg.ErrSkillMDMalformedYAML},
		{"malformed yaml", "---\n[unclosed\n---\n## Steps\n- s\n", skillpkg.ErrSkillMDMalformedYAML},
		{"missing trigger", "---\nname: x\n---\n## Steps\n- s\n", skillpkg.ErrSkillMDMissingTrigger},
		{"blank trigger", "---\ntrigger: \"\"\n---\n## Steps\n- s\n", skillpkg.ErrSkillMDMissingTrigger},
		{"no steps section", "---\ntrigger: t\n---\njust prose\n", skillpkg.ErrSkillMDEmptySteps},
		{"steps without items", "---\ntrigger: t\n---\n## Steps\nsome prose, not a list item\n", skillpkg.ErrSkillMDEmptySteps},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := skillpkg.ValidateSkillMarkdown([]byte(c.in), skillpkg.MarkdownLimits{})
			if err == nil {
				t.Fatalf("expected %v, got nil", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err=%v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestValidateSkillMarkdown_TooLarge(t *testing.T) {
	big := make([]byte, skillpkg.MaxPackageSkillMDBytes+1)
	copy(big, "---\ntrigger: t\n---\n## Steps\n- s\n")
	if err := skillpkg.ValidateSkillMarkdown(big, skillpkg.MarkdownLimits{}); !errors.Is(err, skillpkg.ErrSkillMDTooLarge) {
		t.Fatalf("err=%v, want ErrSkillMDTooLarge", err)
	}
}

// TestValidateSkillMarkdown_NotUTF8 pins the P6.1 closure: a SKILL.md
// document must be valid UTF-8. The check lives in the canonical
// document gate, so the ZIP ingest path (which runs this gate on the
// root SKILL.md) rejects non-UTF-8 documents, not just the
// single-document path.
func TestValidateSkillMarkdown_NotUTF8(t *testing.T) {
	bad := []byte("---\ntrigger: t\n---\n\xff\xfe not utf-8\n## Steps\n- s\n")
	if err := skillpkg.ValidateSkillMarkdown(bad, skillpkg.MarkdownLimits{}); !errors.Is(err, skillpkg.ErrSkillMDNotUTF8) {
		t.Fatalf("err=%v, want ErrSkillMDNotUTF8", err)
	}
	// Valid UTF-8 (including non-ASCII text) passes the gate.
	if err := skillpkg.ValidateSkillMarkdown([]byte("---\ntrigger: t\n---\n# Café guide\n\n## Steps\n- s\n"), skillpkg.MarkdownLimits{}); err != nil {
		t.Fatalf("valid UTF-8 rejected: %v", err)
	}
}
