package importer_test

import (
	"archive/zip"
	"bytes"
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
		if got := skillpkg.DescriptionSummary(ingest.Skill.Description); got != wantTrigger {
			t.Fatalf("summary = %q, want frontmatter description %q", got, wantTrigger)
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
		if got := skillpkg.DescriptionSummary(ingest.Skill.Description); got != "Explains things.\nTwice." {
			t.Fatalf("multiline summary = %q", got)
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

	t.Run("over-bound description is refused and a long derived trigger is capped", func(t *testing.T) {
		long := strings.Repeat("é", skillpkg.MaxPackageDescriptionBytes/2+1) // 2 bytes per rune
		md := "---\nname: x\ntrigger: t\ndescription: " + long + "\n---\n## Steps\n- s\n"
		if _, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
			t.Fatalf("description: err = %v, want ErrInvalidSkillContent", err)
		}
		// A derived trigger is capped on a rune boundary, never refused.
		derived := "---\nname: x\ndescription: " + strings.Repeat("é", skillpkg.MaxPackageTextRunes+50) + "\n---\n## Steps\n- s\n"
		ingest, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(derived)})
		if err != nil {
			t.Fatalf("long description: %v", err)
		}
		if r := []rune(ingest.Skill.Trigger); len(r) != skillpkg.MaxPackageTextRunes || r[len(r)-1] != '…' {
			t.Fatalf("derived trigger has %d runes", len(r))
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
	if got := skillpkg.DescriptionSummary(plain.Skill.Description); got != "A bound body." {
		t.Fatalf("canonical summary = %q", got)
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
	if got := skillpkg.DescriptionSummary(skill.Description); got != "Summarizes notes." {
		t.Fatalf("file summary = %q", got)
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
	if got := skillpkg.DescriptionSummary(sk.Description); got != wantTrigger {
		t.Fatalf("Agent Skills summary = %q", got)
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

// TestImportPackage_AgentSkillsBundledScript: a standard Agent Skills
// zip with scripts/helper.py and a references/ file imports; the script
// is recorded as plain-text data (never a script MIME). A bundled
// binary executable, or binary bytes behind a script extension, is
// refused.
func TestImportPackage_AgentSkillsBundledScript(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()
	entries := agentSkillEntries()
	entries["scripts/helper.py"] = "import sys\n\nprint(sys.argv[1:])\n"
	ingest, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, entries)})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	mimes := map[string]string{}
	for _, f := range ingest.Package.Supports {
		mimes[f.Path] = f.Mime
	}
	if mimes["scripts/helper.py"] != "text/plain; charset=utf-8" || mimes["references/style.md"] != "text/markdown" {
		t.Fatalf("support MIMEs = %v", mimes)
	}
	if _, err := ingest.SupportURI("scripts/helper.py"); err != nil {
		t.Fatalf("SupportURI(script): %v", err)
	}

	withBinary := agentSkillEntries()
	withBinary["scripts/tool"] = "\x7fELF\x02\x01\x01\x00"
	if _, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, withBinary)}); !errors.Is(err, skillpkg.ErrArchiveMimeUnsupported) {
		t.Fatalf("binary executable: err = %v, want ErrArchiveMimeUnsupported", err)
	}
	disguised := agentSkillEntries()
	disguised["scripts/helper.py"] = "\x7fELF\x02\x01\x01\x00"
	if _, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, disguised)}); !errors.Is(err, skillpkg.ErrArchiveMimeContentMismatch) {
		t.Fatalf("binary behind .py: err = %v, want ErrArchiveMimeContentMismatch", err)
	}
}

// roundTripMarkdown imports one SKILL.md, exports it, reimports the
// export, and requires the same package hash and logical content.
func roundTripMarkdown(t *testing.T, imp importer.Importer, md string) importer.PackageIngest {
	t.Helper()
	ctx := context.Background()
	ingest, err := imp.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{Markdown: []byte(md)})
	if err != nil {
		t.Fatalf("ImportPackageMarkdown: %v", err)
	}
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
	if again.Hash != ingest.Hash || again.Skill.Description != ingest.Skill.Description {
		t.Fatalf("round trip drifted:\n got %+v\nwant %+v", again.Skill, ingest.Skill)
	}
	return ingest
}

// TestFreeForm_EmptyHeadingRoundTrip pins the review regressions: a
// canonical heading without items inside a free-form body stays in the
// description, so export -> reimport is exact.
func TestFreeForm_EmptyHeadingRoundTrip(t *testing.T) {
	imp, _ := newImporter(t)
	for _, body := range []string{
		"Intro\n## Preconditions\n- p\n## Preconditions\n",
		"Intro\n## Failure modes\n- f\n## Failure modes\n",
		"## Preconditions\n- earlier\n## Preconditions\n\n## Steps\n- x\n",
		"Intro\n## Steps\n\n## Preconditions\n- p\n",
		"Intro\n## Steps\n- a\n## Usage\ntext\n",
	} {
		roundTripMarkdown(t, imp, "---\nname: x\ntrigger: t\n---\n"+body)
	}
}

// TestFreeForm_BareHeadingsRefused: a body of headings only carries no
// instructions and is refused like a blank body.
func TestFreeForm_BareHeadingsRefused(t *testing.T) {
	imp, _ := newImporter(t)
	md := "---\nname: x\ntrigger: t\n---\n# Title\n\n## Overview\n\n## Usage\n"
	if _, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{Markdown: []byte(md)}); !errors.Is(err, importer.ErrEmptySteps) {
		t.Fatalf("err = %v, want ErrEmptySteps", err)
	}
}

// TestExport_StaleFrontmatterDescriptionSynthesizes: when a skill's
// description no longer starts with its stashed frontmatter description,
// the export synthesizes frontmatter instead of duplicating text.
func TestExport_StaleFrontmatterDescriptionSynthesizes(t *testing.T) {
	imp, _ := newImporter(t)
	src := "---\nname: notes\ndescription: Summarizes notes.\n---\nKeep it short.\n\n## Steps\n\n- Read the notes.\n"
	skill, attachments, err := imp.Import(context.Background(), importer.ImportSource{Bytes: []byte(src), Scope: goldenScope("stale")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	skill.Description = "Rewritten description."
	out, err := imp.Export(context.Background(), skill, attachments)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// The derived trigger still reads "Summarizes notes."; the stale
	// frontmatter description is gone and the new text appears once.
	if strings.Contains(string(out), "description:") || strings.Count(string(out), "Rewritten description.") != 1 {
		t.Fatalf("stale export:\n%s", out)
	}
	again, _, err := imp.Import(context.Background(), importer.ImportSource{Bytes: out, Scope: goldenScope("stale2")})
	if err != nil || again.Description != "Rewritten description." {
		t.Fatalf("reimport description=%q err=%v", again.Description, err)
	}
}

// TestExport_QuotesListEntries: synthesized list entries are YAML-quoted
// when needed, so tags with `: ` survive the round trip.
func TestExport_QuotesListEntries(t *testing.T) {
	imp, _ := newImporter(t)
	md := "---\nname: x\ntrigger: t\ntags: [\"kind: report\", \"#hash\"]\n---\n## Steps\n- s\n"
	ingest := roundTripMarkdown(t, imp, md)
	if len(ingest.Skill.Tags) != 2 {
		t.Fatalf("tags = %q", ingest.Skill.Tags)
	}
}

// TestImportPackage_AgentSkillsRealWorldZip exercises the shapes real
// Agent Skills zips carry together: a CRLF SKILL.md, an unquoted `: ` in
// the description, an empty __init__.py, an .xsd schema, an
// extension-less LICENSE, and macOS __MACOSX / .DS_Store junk.
func TestImportPackage_AgentSkillsRealWorldZip(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()
	lf := "---\n" +
		"name: office-docs\n" +
		"description: Creates and edits office documents. Use for: (1) new reports, (2) edits to existing files.\n" +
		"license: Proprietary. LICENSE has complete terms\n" +
		"---\n" +
		"# Office documents\n\n## Overview\n\nValidate against the bundled schema before saving.\n\n## Workflow\n\n1. Unpack the file.\n2. Edit the XML.\n3. Pack it again.\n"
	entries := map[string]string{
		"SKILL.md":                   strings.ReplaceAll(lf, "\n", "\r\n"),
		"LICENSE":                    "Terms of use.\n",
		"scripts/__init__.py":        "",
		"scripts/pack.py":            "print('pack')\n",
		"ooxml/schemas/main.xsd":     "<xs:schema xmlns:xs=\"http://www.w3.org/2001/XMLSchema\"/>\n",
		"__MACOSX/._SKILL.md":        "\x00\x05\x16\x07binary resource fork",
		"__MACOSX/scripts/._pack.py": "\x00\x05\x16\x07",
		".DS_Store":                  "\x00\x00\x00\x01Bud1",
		"scripts/.DS_Store":          "\x00\x00\x00\x01Bud1",
	}
	ingest, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, entries)})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	if want := "Creates and edits office documents. Use for: (1) new reports, (2) edits to existing files."; ingest.Skill.Trigger != want {
		t.Fatalf("trigger = %q", ingest.Skill.Trigger)
	}
	if strings.Contains(ingest.Skill.Description, "\r") || !strings.Contains(ingest.Skill.Description, "## Workflow\n\n1. Unpack the file.") {
		t.Fatalf("description = %q", ingest.Skill.Description)
	}
	mimes := map[string]string{}
	for _, f := range ingest.Package.Supports {
		mimes[f.Path] = f.Mime
	}
	want := map[string]string{
		"LICENSE":                "text/plain; charset=utf-8",
		"scripts/__init__.py":    "text/plain; charset=utf-8",
		"scripts/pack.py":        "text/plain; charset=utf-8",
		"ooxml/schemas/main.xsd": "application/xml",
	}
	if len(mimes) != len(want) {
		t.Fatalf("supports = %v, want %v (junk must be skipped)", mimes, want)
	}
	for p, m := range want {
		if mimes[p] != m {
			t.Fatalf("support %q MIME = %q, want %q", p, mimes[p], m)
		}
	}
	// The CRLF document hashes like its LF form.
	lfEntries := map[string]string{"SKILL.md": lf}
	for p := range want {
		lfEntries[p] = entries[p]
	}
	lfIngest, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, lfEntries)})
	if err != nil || lfIngest.Hash != ingest.Hash {
		t.Fatalf("LF hash %q vs CRLF %q (err=%v)", lfIngest.Hash, ingest.Hash, err)
	}
}

// orderedZip builds a zip from entries in the given order; a name
// ending in "/" becomes a directory entry, as zip tools write them.
func orderedZip(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatalf("Create(%q): %v", e[0], err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatalf("Write(%q): %v", e[0], err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestImportPackage_ZipToolLayouts: a Finder-shaped zip (wrapper folder,
// directory entries, __MACOSX, .DS_Store) and a `zip -r` zip (root
// layout with directory entries) import with the flat zip's hash.
func TestImportPackage_ZipToolLayouts(t *testing.T) {
	imp, _ := newImporter(t)
	ctx := context.Background()
	flat, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: packageZip(t, agentSkillEntries())})
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	skill, guide := agentSkillEntries()["SKILL.md"], agentSkillEntries()["references/style.md"]
	layouts := map[string][][2]string{
		"finder": {
			{"release-notes/", ""},
			{"release-notes/SKILL.md", skill},
			{"release-notes/references/", ""},
			{"release-notes/references/style.md", guide},
			{"release-notes/.DS_Store", "\x00\x00\x00\x01Bud1"},
			{"__MACOSX/", ""},
			{"__MACOSX/release-notes/", ""},
			{"__MACOSX/release-notes/._SKILL.md", "\x00\x05\x16\x07"},
		},
		"zip -r": {
			{"SKILL.md", skill},
			{"references/", ""},
			{"references/style.md", guide},
		},
	}
	for name, entries := range layouts {
		got, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: orderedZip(t, entries)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Hash != flat.Hash {
			t.Fatalf("%s hash %q, want flat %q", name, got.Hash, flat.Hash)
		}
	}
	for name, entries := range map[string][][2]string{
		"two skill folders": {{"a/SKILL.md", skill}, {"b/SKILL.md", skill}},
		"root and wrapper":  {{"SKILL.md", skill}, {"a/SKILL.md", skill}},
	} {
		if _, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: orderedZip(t, entries)}); !errors.Is(err, skillpkg.ErrSkillMDNotRoot) {
			t.Fatalf("%s: err = %v, want ErrSkillMDNotRoot", name, err)
		}
	}
	traversal := [][2]string{{"../escape/", ""}, {"SKILL.md", skill}}
	if _, err := imp.ImportPackage(ctx, importer.PackageSource{Archive: orderedZip(t, traversal)}); !errors.Is(err, skillpkg.ErrArchiveTraversal) {
		t.Fatalf("traversal dir entry: err = %v, want ErrArchiveTraversal", err)
	}
}
