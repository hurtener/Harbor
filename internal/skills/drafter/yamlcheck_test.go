package drafter

import (
	"bytes"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// frontmatter_test.go — the YAML-frontmatter safety contract the
// renderer depends on. The canonical ingest locates the closing
// `---` fence by scanning for a column-0 fence line, so the rendered
// frontmatter must never contain one inside its content. goccy quotes
// or indents every value that could produce one; this test pins that
// behavior against hostile values so a serializer change cannot
// silently break the round trip.

func TestRenderSkillMD_FrontmatterNeverEmitsColumnZeroFence(t *testing.T) {
	hostile := []skillpkg.PackageSkill{
		{Name: "x", Trigger: "t", Title: "---", Steps: []string{"s"}},
		{Name: "x", Trigger: "t", Title: "line\n---\nline", Steps: []string{"s"}},
		{Name: "x", Trigger: "t", Title: "plain", Tags: []string{"---"}, Steps: []string{"s"}},
		{Name: "x", Trigger: "t", Title: "plain", Tags: []string{"- dash"}, Steps: []string{"s"}},
		{Name: "x", Trigger: "# hashes # and : colons", Steps: []string{"s"}},
		{Name: "x", Trigger: "t", Title: "trailing spaces  ", Steps: []string{"s"}},
		{Name: "x", Trigger: "t", Title: "quote \" and backslash \\", Steps: []string{"s"}},
	}
	for _, skill := range hostile {
		doc, err := RenderSkillMD(skill)
		if err != nil {
			t.Fatalf("RenderSkillMD(%+v): %v", skill, err)
		}
		if !bytes.HasPrefix(doc, []byte("---\n")) {
			t.Fatalf("missing opening fence:\n%s", doc)
		}
		// Exactly two lines equal `---`: the opening fence and the
		// closing fence. Any third would be a column-0 fence smuggled
		// inside the frontmatter that the ingest's fence scan would
		// misread as the closing fence.
		lines := bytes.Split(doc, []byte("\n"))
		fenceLines := 0
		for _, line := range lines {
			if string(line) == "---" {
				fenceLines++
			}
		}
		if fenceLines != 2 {
			t.Fatalf("want exactly 2 fence lines (opening + closing), got %d:\n%s", fenceLines, doc)
		}
		if got := strings.Count(string(doc), "\n---\n"); got != 1 {
			t.Fatalf("want exactly one interior fence line, got %d:\n%s", got, doc)
		}
	}
}
