package drafter

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// skillmd_test.go — the deterministic SKILL.md renderer and its
// round trip through the canonical complete-skill-package ingest (the
// same ingest the validate/commit workflow runs). The artifact this
// lane persists must be accepted by that ingest with an IDENTICAL
// package hash and an identical normalized representation — that is
// the reviewed-byte contract.

// newPackageImporter builds a fresh importer over an in-memory
// artifact store.
func newPackageImporter(t *testing.T) importer.Importer {
	t.Helper()
	store, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	imp, err := importer.New(importer.Deps{Store: store})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	t.Cleanup(func() { _ = imp.Close(context.Background()) })
	return imp
}

// assertRoundTrip renders skill, ingests the document through the
// canonical single-document ingest, and asserts the package hash and
// normalized canonical representation match exactly.
func assertRoundTrip(t *testing.T, imp importer.Importer, skill skillpkg.PackageSkill) {
	t.Helper()
	doc, err := RenderSkillMD(skill)
	if err != nil {
		t.Fatalf("RenderSkillMD: %v", err)
	}
	pkg := skillpkg.Package{Name: skillpkg.CanonicalName(skill.Name), Skill: skill}
	wantHash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	ingest, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{
		Markdown: doc,
		PathHint: "draft.md",
	})
	if err != nil {
		t.Fatalf("ImportPackageMarkdown of rendered draft: %v", err)
	}
	if ingest.Hash != wantHash {
		t.Fatalf("ingest hash %q != computed hash %q", ingest.Hash, wantHash)
	}
	if err := skillpkg.VerifyPackageHash(ingest.Package, wantHash); err != nil {
		t.Fatalf("VerifyPackageHash(ingest): %v", err)
	}
	wantCB, err := skillpkg.CanonicalBytes(pkg)
	if err != nil {
		t.Fatalf("CanonicalBytes(computed): %v", err)
	}
	gotCB, err := skillpkg.CanonicalBytes(ingest.Package)
	if err != nil {
		t.Fatalf("CanonicalBytes(ingest): %v", err)
	}
	if !bytes.Equal(wantCB, gotCB) {
		t.Fatalf("normalized representation mismatch:\nwant %s\ngot  %s", wantCB, gotCB)
	}
}

func TestRenderSkillMD_RoundTrip(t *testing.T) {
	imp := newPackageImporter(t)

	skill := skillpkg.PackageSkill{
		Name:        "triage-inbox",
		Title:       "Triage the shared inbox",
		Description: "Scan the shared inbox and triage every unread message.\n\nExternal [site](https://example.com) and an [anchor](#overview) stay verbatim.",
		Trigger:     "when the user asks to triage the inbox",
		TaskType:    "api",
		Tags:        []string{"triage", "inbox"},
		Steps: []string{
			"Fetch unread messages",
			"Classify each message by priority",
			"Assign each message to the right queue",
		},
		Preconditions: []string{"The inbox connection is active"},
		FailureModes:  []string{"A message cannot be classified"},
		RequiredTools: []string{"inbox.read"},
		RequiredNS:    []string{"mail"},
		RequiredTags:  []string{"personal"},
	}
	assertRoundTrip(t, imp, skill)
}

func TestRenderSkillMD_RoundTrip_TrickyValues(t *testing.T) {
	imp := newPackageImporter(t)

	// Values that stress YAML emission: colons, hashes, brackets,
	// quotes, a multi-line title containing a `---` line (must be
	// emitted as an indented block scalar, never a column-0 fence),
	// and list entries that would be ambiguous unquoted.
	skill := skillpkg.PackageSkill{
		Name:        "weird-values",
		Title:       "A title: with #hash, [brackets], \"quotes\"\n---\nand a second line",
		Description: "Prose with no section headings.",
		Trigger:     "when #something: happens",
		TaskType:    "code",
		Tags:        []string{"tag:one", "- dash", "# hash", "plain"},
		Steps:       []string{"Run the thing"},
	}
	assertRoundTrip(t, imp, skill)
}

func TestRenderSkillMD_RoundTrip_EmptyOptionalFields(t *testing.T) {
	imp := newPackageImporter(t)
	skill := skillpkg.PackageSkill{
		Name:    "minimal",
		Trigger: "when triggered",
		Steps:   []string{"One step"},
	}
	assertRoundTrip(t, imp, skill)
}

func TestRenderSkillMD_RejectsUnrenderable(t *testing.T) {
	descriptionHeading := skillpkg.PackageSkill{
		Name:        "x",
		Trigger:     "t",
		Description: "prose\n## Not a section\nmore prose",
		Steps:       []string{"s"},
	}
	if _, err := RenderSkillMD(descriptionHeading); !errors.Is(err, ErrUnrenderableSkill) {
		t.Fatalf("heading prose: err=%v, want ErrUnrenderableSkill", err)
	}

	multiLineStep := skillpkg.PackageSkill{
		Name:    "x",
		Trigger: "t",
		Steps:   []string{"first\nsecond"},
	}
	if _, err := RenderSkillMD(multiLineStep); !errors.Is(err, ErrUnrenderableSkill) {
		t.Fatalf("multi-line step: err=%v, want ErrUnrenderableSkill", err)
	}

	imageRef := skillpkg.PackageSkill{
		Name:        "x",
		Trigger:     "t",
		Description: "see ![diagram](assets/logo.png)",
		Steps:       []string{"s"},
	}
	if _, err := RenderSkillMD(imageRef); !errors.Is(err, ErrUnrenderableSkill) {
		t.Fatalf("image ref: err=%v, want ErrUnrenderableSkill", err)
	}

	relativeLink := skillpkg.PackageSkill{
		Name:        "x",
		Trigger:     "t",
		Description: "see [guide](docs/guide.md)",
		Steps:       []string{"s"},
	}
	if _, err := RenderSkillMD(relativeLink); !errors.Is(err, ErrUnrenderableSkill) {
		t.Fatalf("relative link: err=%v, want ErrUnrenderableSkill", err)
	}

	definition := skillpkg.PackageSkill{
		Name:        "x",
		Trigger:     "t",
		Description: "see [guide][g]\n\n[g]: docs/guide.md",
		Steps:       []string{"s"},
	}
	if _, err := RenderSkillMD(definition); !errors.Is(err, ErrUnrenderableSkill) {
		t.Fatalf("reference definition: err=%v, want ErrUnrenderableSkill", err)
	}
}

func TestRenderSkillMD_ValidatesFirst(t *testing.T) {
	if _, err := RenderSkillMD(skillpkg.PackageSkill{Name: "x", Trigger: "t"}); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
		t.Fatalf("empty steps: err=%v, want skillpkg.ErrInvalidSkillContent", err)
	}
}

func TestRenderSkillMD_Deterministic(t *testing.T) {
	skill := skillpkg.PackageSkill{
		Name:          "det",
		Trigger:       "t",
		Description:   "d",
		Steps:         []string{"a", "b"},
		Tags:          []string{"z", "a"},
		RequiredTools: []string{"y", "x"},
	}
	first, err := RenderSkillMD(skill)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := RenderSkillMD(skill)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("RenderSkillMD is not deterministic")
		}
	}
	if !strings.HasPrefix(string(first), "---\nname: det\n") {
		t.Fatalf("unexpected document head:\n%s", first)
	}
	if !strings.Contains(string(first), "## Steps\n\n- a\n- b\n") {
		t.Fatalf("missing canonical Steps section:\n%s", first)
	}
}
