package importer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// externalSkillMD is the common external SKILL.md shape: `name` +
// `description` frontmatter, no `trigger`, body prose before the
// sections.
const externalSkillMD = "---\n" +
	"name: summarize-notes\n" +
	"description: \"Summarizes meeting notes into decisions and action items. Use when: the user shares raw notes.\"\n" +
	"---\n" +
	"# Summarize notes\n" +
	"\n" +
	"Keep the summary short.\n" +
	"\n" +
	"## Steps\n" +
	"\n" +
	"- Read the notes.\n" +
	"- List the decisions.\n"

func TestImportPackageMarkdown_FrontmatterDescription(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()

	t.Run("description-only derives the trigger", func(t *testing.T) {
		ingest, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(externalSkillMD)})
		if err != nil {
			t.Fatalf("ImportPackageMarkdown: %v", err)
		}
		wantTrigger := "Summarizes meeting notes into decisions and action items. Use when: the user shares raw notes."
		if ingest.Skill.Trigger != wantTrigger || ingest.Package.Skill.Trigger != wantTrigger {
			t.Fatalf("trigger = %q / %q, want %q", ingest.Skill.Trigger, ingest.Package.Skill.Trigger, wantTrigger)
		}
		wantDesc := wantTrigger + "\n\n# Summarize notes\n\nKeep the summary short."
		if ingest.Skill.Description != wantDesc || ingest.Package.Skill.Description != wantDesc {
			t.Fatalf("description = %q, want %q", ingest.Skill.Description, wantDesc)
		}
		// Export -> reimport reproduces the same package hash: the
		// joined description re-parses to itself as body prose and the
		// derived trigger (with its `: `) survives the synthesized
		// frontmatter.
		body, err := imp.MaterializePackageBody(ctx, ingest)
		if err != nil {
			t.Fatalf("MaterializePackageBody: %v", err)
		}
		ex, err := imp.ExportPackage(ctx, ingest, body)
		if err != nil {
			t.Fatalf("ExportPackage: %v", err)
		}
		again, err := imp.ReimportPackage(ctx, ex)
		if err != nil {
			t.Fatalf("ReimportPackage: %v\n%s", err, ex.Document)
		}
		if again.Hash != ingest.Hash || again.Skill.ContentHash != ingest.Skill.ContentHash {
			t.Fatalf("reimport hash %q/%q, want %q/%q", again.Hash, again.Skill.ContentHash, ingest.Hash, ingest.Skill.ContentHash)
		}
	})

	t.Run("explicit trigger wins and description is accepted", func(t *testing.T) {
		md := "---\nname: x\ndescription: |\n  Explains things.\n  Twice.\ntrigger: when asked to explain\n---\n## Steps\n- explain\n"
		ingest, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)})
		if err != nil {
			t.Fatalf("ImportPackageMarkdown: %v", err)
		}
		if ingest.Skill.Trigger != "when asked to explain" {
			t.Fatalf("trigger = %q", ingest.Skill.Trigger)
		}
		if ingest.Skill.Description != "Explains things.\nTwice." {
			t.Fatalf("description = %q", ingest.Skill.Description)
		}
		// The ZIP path shares the same closed parse.
		if _, err := imp.ImportPackage(ctx, importer.PackageSource{
			Archive: packageZip(t, map[string]string{"SKILL.md": md}),
		}); err != nil {
			t.Fatalf("ImportPackage: %v", err)
		}
	})

	t.Run("a multi-line description derives a single-line trigger", func(t *testing.T) {
		md := "---\nname: x\ndescription: |\n  Line one.\n\n  Line   two.\n---\n## Steps\n- s\n"
		ingest, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)})
		if err != nil {
			t.Fatalf("ImportPackageMarkdown: %v", err)
		}
		if ingest.Skill.Trigger != "Line one. Line two." {
			t.Fatalf("trigger = %q", ingest.Skill.Trigger)
		}
	})

	t.Run("neither trigger nor description is refused", func(t *testing.T) {
		md := "---\nname: x\ndescription: \"  \"\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, skillpkg.ErrSkillMDMissingTrigger) {
			t.Fatalf("err = %v, want ErrSkillMDMissingTrigger", err)
		}
	})

	t.Run("an over-bound description is refused", func(t *testing.T) {
		long := strings.Repeat("é", skillpkg.MaxPackageTextRunes+1)
		md := "---\nname: x\ntrigger: t\ndescription: " + long + "\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
			t.Fatalf("err = %v, want ErrInvalidSkillContent", err)
		}
	})

	t.Run("a section heading inside the description is refused", func(t *testing.T) {
		md := "---\nname: x\ndescription: |\n  Intro.\n  ## Steps\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, importer.ErrUnknownSection) {
			t.Fatalf("err = %v, want ErrUnknownSection", err)
		}
	})
}

// TestImportPackage_HashStableWithoutDescription pins the package and
// stored-content hashes of description-free documents to the values the
// importer produced before frontmatter `description` was accepted: the
// change is additive and existing packages keep their identity.
func TestImportPackage_HashStableWithoutDescription(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()
	golden, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, goldenEntries())})
	if err != nil {
		t.Fatalf("ImportPackage(golden): %v", err)
	}
	plain, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{
		Markdown: []byte("---\nname: bound-skill\ntrigger: when asked for the bound skill\n---\nA bound body.\n\n## Steps\n- do the thing\n"),
	})
	if err != nil {
		t.Fatalf("ImportPackageMarkdown(plain): %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"golden package hash", golden.Hash, "v1:ec9ffcf586c15b02c775b33dcb721b848190f6420116ec224aa77b30448cda8e"},
		{"golden content hash", golden.Skill.ContentHash, "8bd606d7bfce5ffa785500b2130de56c0d0d4503a63421eada063282dd613f43"},
		{"plain package hash", plain.Hash, "v1:4c276a2e8e947ada811d461f8c71586f8dbc14c33901e80f51edf8a79cee49b8"},
		{"plain content hash", plain.Skill.ContentHash, "2607119231d55ad0acd94b0a51a01a6514af4f30990fca0a3c0bb614050574bf"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestImport_FrontmatterDescriptionRoundTrip(t *testing.T) {
	imp, _ := newImporter(t)
	src := "---\nname: notes\ndescription: Summarizes notes.\n---\nKeep it short.\n\n## Steps\n\n- Read the notes.\n"
	skill, attachments, err := imp.Import(context.Background(), importer.ImportSource{Bytes: []byte(src), Scope: goldenScope("description")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if skill.Trigger != "Summarizes notes." || skill.Description != "Summarizes notes.\n\nKeep it short." {
		t.Fatalf("trigger=%q description=%q", skill.Trigger, skill.Description)
	}
	if err := skill.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := imp.Export(context.Background(), skill, attachments)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if string(out) != src {
		t.Fatalf("round trip differs:\n got %q\nwant %q", out, src)
	}
}
