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

	t.Run("over-bound description and derived trigger are refused", func(t *testing.T) {
		long := strings.Repeat("é", skillpkg.MaxPackageDescriptionRunes+1)
		md := "---\nname: x\ntrigger: t\ndescription: " + long + "\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
			t.Fatalf("description: err = %v, want ErrInvalidSkillContent", err)
		}
		// The derived trigger keeps the ordinary text bound.
		derived := "---\nname: x\ndescription: " + strings.Repeat("é", skillpkg.MaxPackageTextRunes+1) + "\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(derived)}); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
			t.Fatalf("derived trigger: err = %v, want ErrInvalidSkillContent", err)
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

// agentSkillMD is a realistic skill in the open Agent Skills format:
// `name` + `description` (plus the advisory license / allowed-tools /
// metadata keys), a free-form body with arbitrary headings, numbered
// lists, a fenced example, and a link to a bundled reference file.
const agentSkillMD = "---\n" +
	"name: release-notes\n" +
	"description: Drafts release notes from a list of merged changes. Use when the user asks for release notes or a changelog entry.\n" +
	"license: Apache-2.0\n" +
	"allowed-tools: Read, Grep\n" +
	"metadata:\n" +
	"  version: \"1.2\"\n" +
	"  author: example\n" +
	"---\n" +
	"# Release notes\n" +
	"\n" +
	"## Overview\n" +
	"\n" +
	"Turn merged changes into reader-facing notes. Follow the [style guide](references/style.md).\n" +
	"\n" +
	"## Instructions\n" +
	"\n" +
	"1. Group the changes into Added, Changed and Fixed.\n" +
	"2. Write one line per change, in the past tense.\n" +
	"3. Drop internal-only changes.\n" +
	"\n" +
	"## Examples\n" +
	"\n" +
	"```markdown\n" +
	"### Added\n" +
	"- Export to CSV.\n" +
	"```\n"

func agentSkillEntries() map[string]string {
	return map[string]string{
		"SKILL.md":            agentSkillMD,
		"references/style.md": "# Style\n\nUse plain words.\n",
	}
}

// TestImportPackage_AgentSkillsFormat imports a standard Agent Skills
// package: the body is kept verbatim as the instructions, the trigger
// is derived from the description, the advisory keys grant nothing,
// the bundled reference stays a support file, and export -> reimport
// reproduces the package hash.
func TestImportPackage_AgentSkillsFormat(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()
	ingest, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, agentSkillEntries())})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	sk := ingest.Skill
	const wantTrigger = "Drafts release notes from a list of merged changes. Use when the user asks for release notes or a changelog entry."
	if sk.Name != "release-notes" || sk.Trigger != wantTrigger {
		t.Fatalf("name=%q trigger=%q", sk.Name, sk.Trigger)
	}
	_, body, ok := strings.Cut(agentSkillMD, "---\n# Release notes")
	if !ok {
		t.Fatal("fixture lost its body")
	}
	body = "# Release notes" + body
	if want := wantTrigger + "\n\n" + strings.TrimSuffix(body, "\n"); sk.Description != want {
		t.Fatalf("description =\n%q\nwant\n%q", sk.Description, want)
	}
	if len(sk.Steps) != 1 || sk.Steps[0] != importer.FreeFormStep {
		t.Fatalf("steps = %q", sk.Steps)
	}
	if len(sk.RequiredTools) != 0 || len(sk.Extra) != 0 {
		t.Fatalf("advisory keys leaked into the skill: tools=%v extra=%v", sk.RequiredTools, sk.Extra)
	}
	if len(ingest.Package.Supports) != 1 || ingest.Package.Supports[0].Path != "references/style.md" {
		t.Fatalf("supports = %+v", ingest.Package.Supports)
	}
	uri, err := ingest.SupportURI("references/style.md")
	if err != nil {
		t.Fatalf("SupportURI: %v", err)
	}
	materialized, err := imp.MaterializePackageBody(ctx, ingest)
	if err != nil || !strings.Contains(materialized, uri.String()) {
		t.Fatalf("materialized body lacks %s (err=%v)", uri, err)
	}
	ex, err := imp.ExportPackage(ctx, ingest, materialized)
	if err != nil {
		t.Fatalf("ExportPackage: %v", err)
	}
	again, err := imp.ReimportPackage(ctx, ex)
	if err != nil {
		t.Fatalf("ReimportPackage: %v\n%s", err, ex.Document)
	}
	if again.Hash != ingest.Hash || again.Skill.Description != sk.Description || again.Skill.Steps[0] != importer.FreeFormStep {
		t.Fatalf("reimport drifted: %q vs %q", again.Hash, ingest.Hash)
	}
	// The advisory keys are not authority: changing them does not change
	// the package identity.
	other := agentSkillEntries()
	other["SKILL.md"] = strings.Replace(agentSkillMD, "allowed-tools: Read, Grep", "allowed-tools: [Bash]", 1)
	alt, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, other)})
	if err != nil || alt.Hash != ingest.Hash {
		t.Fatalf("advisory key changed identity: %v %q vs %q", err, alt.Hash, ingest.Hash)
	}
}

// TestImport_AgentSkillsFormatFilePath: the file importer (CLI, boot
// file path) accepts the same document.
func TestImport_AgentSkillsFormatFilePath(t *testing.T) {
	imp, _ := newImporter(t)
	md := strings.Replace(agentSkillMD, "Follow the [style guide](references/style.md).", "Keep it short.", 1)
	skill, _, err := imp.Import(context.Background(), importer.ImportSource{Bytes: []byte(md), Scope: goldenScope("agent-skill")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.Contains(skill.Description, "## Instructions\n\n1. Group the changes") || skill.Steps[0] != importer.FreeFormStep {
		t.Fatalf("skill = %+v", skill)
	}
}
