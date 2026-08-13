package importer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sort"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// pngBytes returns a structurally valid minimal PNG (signature +
// 13-byte IHDR + IEND). The content-truth MIME gate requires the real
// signature AND the IHDR chunk, so fixtures must be real containers.
func pngBytes() []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	writeChunk := func(typ string, data []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(data)))
		b.Write(l[:])
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], crc.Sum32())
		b.Write(c[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1) // width
	binary.BigEndian.PutUint32(ihdr[4:8], 1) // height
	ihdr[8] = 8                              // bit depth
	ihdr[9] = 6                              // color type: RGBA
	writeChunk("IHDR", ihdr)
	writeChunk("IEND", nil)
	return b.Bytes()
}

// packageZip builds a complete-skill-package archive in memory.
func packageZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Deterministic order.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

const packageSkillMD = "---\nname: packaged-demo\ntrigger: when asked for a package demo\n---\nA packaged demo skill.\n\n![diagram](assets/logo.png)\n\n## Steps\n- do the thing\n\n## Preconditions\n- have the thing\n"

func TestImportPackage_Valid(t *testing.T) {
	imp, _ := newImporter(t)
	z := packageZip(t, map[string]string{
		"SKILL.md":           packageSkillMD,
		"assets/logo.png":    string(pngBytes()),
		"examples/demo.json": `{"demo": true}`,
		"docs/usage.txt":     "usage notes",
	})

	ingest, err := imp.ImportPackage(context.Background(), importer.PackageSource{Archive: z, PathHint: "packaged-demo.zip"})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}

	// Stored-skill form.
	if ingest.Skill.Name != "packaged-demo" || ingest.Skill.Origin != skills.OriginPack || ingest.Skill.Scope != skills.ScopeProject {
		t.Fatalf("Skill = %+v", ingest.Skill)
	}
	if ingest.Skill.Trigger != "when asked for a package demo" || len(ingest.Skill.Steps) != 1 {
		t.Fatalf("Skill content = %+v", ingest.Skill)
	}
	// The body keeps its relative path (no artifact:// substitution
	// in the pure semantic ingest).
	if !strings.Contains(ingest.Skill.Description, "assets/logo.png") {
		t.Fatalf("description lost the relative ref: %q", ingest.Skill.Description)
	}

	// Canonical DTO: logical content + ordered normalized manifest
	// (SKILL.md excluded).
	if ingest.Package.Name != "packaged-demo" {
		t.Fatalf("package name = %q", ingest.Package.Name)
	}
	if len(ingest.Package.Supports) != 3 {
		t.Fatalf("supports = %+v", ingest.Package.Supports)
	}
	for _, f := range ingest.Package.Supports {
		if f.Path == skills.RootSkillFileName {
			t.Fatalf("SKILL.md leaked into the support manifest: %+v", f)
		}
	}
	if err := ingest.Package.Validate(); err != nil {
		t.Fatalf("Package.Validate: %v", err)
	}

	// Hash + support URI: versioned, distinct from the stored
	// ContentHash, and the URI round-trips. The URI references ONE
	// support file — never the package name.
	if !strings.HasPrefix(ingest.Hash, "v1:") || ingest.Hash == ingest.Skill.ContentHash {
		t.Fatalf("hash %q (content %q)", ingest.Hash, ingest.Skill.ContentHash)
	}
	u, err := ingest.SupportURI("assets/logo.png")
	if err != nil {
		t.Fatalf("SupportURI: %v", err)
	}
	if u.String() != "skillpkg://"+ingest.Hash+"/assets/logo.png" {
		t.Fatalf("URI = %q", u.String())
	}
	parsed, err := skills.ParsePackageURI(u.String())
	if err != nil || parsed.Hash != ingest.Hash || parsed.Path != "assets/logo.png" {
		t.Fatalf("URI round-trip: %+v err=%v", parsed, err)
	}
	// Every manifest entry yields its own immutable support URI.
	seen := map[string]bool{}
	for _, f := range ingest.Package.Supports {
		su, err := ingest.SupportURI(f.Path)
		if err != nil {
			t.Fatalf("SupportURI(%q): %v", f.Path, err)
		}
		if seen[su.String()] {
			t.Fatalf("duplicate support URI %q", su.String())
		}
		seen[su.String()] = true
	}
	// A path outside the manifest fails loudly.
	if _, err := ingest.SupportURI("assets/nope.png"); !errors.Is(err, importer.ErrPackageSupportRefMissing) {
		t.Fatalf("SupportURI(missing): err=%v, want ErrPackageSupportRefMissing", err)
	}
	if err := skills.VerifyPackageHash(ingest.Package, ingest.Hash); err != nil {
		t.Fatalf("VerifyPackageHash: %v", err)
	}
}

func TestImportPackage_DeterministicHash(t *testing.T) {
	imp, _ := newImporter(t)
	entries := map[string]string{
		"SKILL.md":           packageSkillMD,
		"assets/logo.png":    string(pngBytes()),
		"examples/demo.json": `{"demo": true}`,
	}
	z1 := packageZip(t, entries)
	z2 := packageZip(t, entries)
	a, err := imp.ImportPackage(context.Background(), importer.PackageSource{Archive: z1})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	b, err := imp.ImportPackage(context.Background(), importer.PackageSource{Archive: z2})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	ua, err := a.SupportURI("assets/logo.png")
	if err != nil {
		t.Fatalf("SupportURI(a): %v", err)
	}
	ub, err := b.SupportURI("assets/logo.png")
	if err != nil {
		t.Fatalf("SupportURI(b): %v", err)
	}
	if a.Hash != b.Hash || ua.String() != ub.String() {
		t.Fatalf("hash not deterministic: %q vs %q", a.Hash, b.Hash)
	}
}

func TestImportPackage_Rejects(t *testing.T) {
	imp, _ := newImporter(t)
	validMD := "---\ntrigger: t\n---\n## Steps\n- s\n"
	cases := []struct {
		name    string
		archive []byte
		wantErr error
	}{
		{"not a zip", []byte("not a zip"), skills.ErrArchiveNotZip},
		{"traversal", packageZip(t, map[string]string{"../escape": "x"}), skills.ErrArchiveTraversal},
		{"case collision", packageZip(t, map[string]string{"SKILL.md": validMD, "skill.md": validMD}), skills.ErrArchivePathCollision},
		{"nested archive", packageZip(t, map[string]string{"SKILL.md": validMD, "payload.txt": "PK\x03\x04nested"}), skills.ErrArchiveNested},
		{"unsupported mime", packageZip(t, map[string]string{"SKILL.md": validMD, "tool.exe": "MZ"}), skills.ErrArchiveMimeUnsupported},
		{"missing skillmd", packageZip(t, map[string]string{"README.md": "hi"}), skills.ErrSkillMDMissing},
		{"non-root skillmd", packageZip(t, map[string]string{"docs/SKILL.md": validMD}), skills.ErrSkillMDNotRoot},
		{"case-mismatch skillmd", packageZip(t, map[string]string{"skill.md": validMD}), skills.ErrSkillMDCaseMismatch},
		{"skillmd no frontmatter", packageZip(t, map[string]string{"SKILL.md": "plain"}), skills.ErrSkillMDFrontmatterMissing},
		{"skillmd missing trigger", packageZip(t, map[string]string{"SKILL.md": "---\nname: x\n---\n## Steps\n- s\n"}), skills.ErrSkillMDMissingTrigger},
		{"skillmd empty steps", packageZip(t, map[string]string{"SKILL.md": "---\ntrigger: t\n---\nno steps\n"}), skills.ErrSkillMDEmptySteps},
		{"unknown section", packageZip(t, map[string]string{"SKILL.md": "---\ntrigger: t\n---\n## Steps\n- s\n## Bizarre\n- x\n"}), importer.ErrUnknownSection},
		{"missing support ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![missing](assets/nope.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"missing link ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\nSee [the guide](docs/guide.md).\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"traversal support ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![bad](../escape.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"remote image ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![x](https://example.com/x.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefRemote},
		{"absolute image ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![x](/etc/x.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefRemote},
		{"absolute link ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\nSee [x](/etc/passwd).\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefRemote},
		{"fragment-only image ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![x](#fig)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"query link ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\nSee [x](docs/guide.md?v=1).\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"percent-encoded ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![x](assets%2Flogo.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
		{"mime content mismatch", packageZip(t, map[string]string{
			"SKILL.md":        "---\nname: x\ntrigger: t\n---\n![x](assets/logo.png)\n\n## Steps\n- s\n",
			"assets/logo.png": `{"not": "a png"}`,
		}), skillpkg.ErrArchiveMimeContentMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := imp.ImportPackage(context.Background(), importer.PackageSource{Archive: c.archive})
			if err == nil {
				t.Fatalf("ImportPackage: expected %v, got nil", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ImportPackage: err=%v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestImportPackage_AfterClose(t *testing.T) {
	imp, _ := newImporter(t)
	_ = imp.Close(context.Background())
	if _, err := imp.ImportPackage(context.Background(), importer.PackageSource{}); !errors.Is(err, importer.ErrImporterClosed) {
		t.Fatalf("ImportPackage after Close: err=%v, want ErrImporterClosed", err)
	}
	if _, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{}); !errors.Is(err, importer.ErrImporterClosed) {
		t.Fatalf("ImportPackageMarkdown after Close: err=%v, want ErrImporterClosed", err)
	}
}

const packageMarkdownMD = "---\nname: markdown-skill\ntrigger: when asked for a markdown skill\n---\nA pure single-document skill.\n\n## Steps\n- do the thing\n\n## Preconditions\n- have the thing\n"

func TestImportPackageMarkdown_Valid(t *testing.T) {
	imp, _ := newImporter(t)
	ingest, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{
		Markdown: []byte(packageMarkdownMD),
		PathHint: "markdown-skill.md",
	})
	if err != nil {
		t.Fatalf("ImportPackageMarkdown: %v", err)
	}

	// Stored-skill form, identical shape to the ZIP path.
	if ingest.Skill.Name != "markdown-skill" || ingest.Skill.Origin != skills.OriginPack || ingest.Skill.Scope != skills.ScopeProject {
		t.Fatalf("Skill = %+v", ingest.Skill)
	}
	if ingest.Skill.Trigger != "when asked for a markdown skill" || len(ingest.Skill.Steps) != 1 {
		t.Fatalf("Skill content = %+v", ingest.Skill)
	}

	// Resource-free package: same parser / canonical DTO / hash, but
	// NO support manifest.
	if ingest.Package.Name != "markdown-skill" {
		t.Fatalf("package name = %q", ingest.Package.Name)
	}
	if len(ingest.Package.Supports) != 0 {
		t.Fatalf("resource-free package carried supports: %+v", ingest.Package.Supports)
	}
	if err := ingest.Package.Validate(); err != nil {
		t.Fatalf("Package.Validate: %v", err)
	}

	// Versioned hash, distinct from the stored ContentHash, and
	// verifiable against the DTO.
	if !strings.HasPrefix(ingest.Hash, "v1:") || ingest.Hash == ingest.Skill.ContentHash {
		t.Fatalf("hash %q (content %q)", ingest.Hash, ingest.Skill.ContentHash)
	}
	if err := skills.VerifyPackageHash(ingest.Package, ingest.Hash); err != nil {
		t.Fatalf("VerifyPackageHash: %v", err)
	}

	// A resource-free package has no support files, so it has no
	// support URI — SupportURI always fails loudly.
	if _, err := ingest.SupportURI("assets/logo.png"); !errors.Is(err, importer.ErrPackageSupportRefMissing) {
		t.Fatalf("SupportURI on resource-free package: err=%v, want ErrPackageSupportRefMissing", err)
	}
}

func TestImportPackageMarkdown_DeterministicHash(t *testing.T) {
	imp, _ := newImporter(t)
	src := importer.PackageMarkdownSource{Markdown: []byte(packageMarkdownMD), PathHint: "markdown-skill.md"}
	a, err := imp.ImportPackageMarkdown(context.Background(), src)
	if err != nil {
		t.Fatalf("ImportPackageMarkdown: %v", err)
	}
	b, err := imp.ImportPackageMarkdown(context.Background(), src)
	if err != nil {
		t.Fatalf("ImportPackageMarkdown: %v", err)
	}
	if a.Hash != b.Hash || a.Package.Name != b.Package.Name {
		t.Fatalf("markdown ingest not deterministic: %q vs %q", a.Hash, b.Hash)
	}
}

func TestImportPackageMarkdown_Rejects(t *testing.T) {
	imp, _ := newImporter(t)
	cases := []struct {
		name    string
		md      []byte
		wantErr error
	}{
		{"invalid utf8", []byte("---\ntrigger: t\n---\n\xff\xfe\n## Steps\n- s\n"), importer.ErrPackageMarkdownNotUTF8},
		{"over bound", bytes.Repeat([]byte("a"), skillpkg.MaxPackageSkillMDBytes+1), skills.ErrSkillMDTooLarge},
		{"no frontmatter", []byte("plain text"), skills.ErrSkillMDFrontmatterMissing},
		{"missing trigger", []byte("---\nname: x\n---\n## Steps\n- s\n"), skills.ErrSkillMDMissingTrigger},
		{"empty steps", []byte("---\ntrigger: t\n---\nno steps\n"), skills.ErrSkillMDEmptySteps},
		{"unknown section", []byte("---\ntrigger: t\n---\n## Steps\n- s\n## Bizarre\n- x\n"), importer.ErrUnknownSection},
		{"support ref in description", []byte("---\nname: x\ntrigger: t\n---\n![diagram](assets/logo.png)\n\n## Steps\n- s\n"), importer.ErrPackageSupportRefMissing},
		{"support ref in step", []byte("---\nname: x\ntrigger: t\n---\n## Steps\n- see ![diagram](assets/logo.png)\n"), importer.ErrPackageSupportRefMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{Markdown: c.md})
			if err == nil {
				t.Fatalf("ImportPackageMarkdown: expected %v, got nil", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ImportPackageMarkdown: err=%v, want %v", err, c.wantErr)
			}
		})
	}
	// A remote IMAGE reference is a resource the resource-free
	// package does not carry — the self-contained-package contract
	// rejects it loudly.
	imp2, _ := newImporter(t)
	withRemoteImage := []byte("---\nname: ext\ntrigger: t\n---\n![remote](https://example.com/x.png)\n\n## Steps\n- s\n")
	if _, err := imp2.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{Markdown: withRemoteImage}); !errors.Is(err, importer.ErrPackageSupportRefRemote) {
		t.Fatalf("remote image: err=%v, want ErrPackageSupportRefRemote", err)
	}
	// Ordinary remote navigation links and `#fragment` document
	// anchors are NOT resource references — they stay verbatim.
	withRemoteLink := []byte("---\nname: ext-link\ntrigger: t\n---\nSee [the docs](https://example.com) and [top](#overview).\n\n## Steps\n- s\n")
	if _, err := imp2.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{Markdown: withRemoteLink}); err != nil {
		t.Fatalf("remote link / anchor rejected: %v", err)
	}
	// An absolute resource reference is rejected for links too.
	withAbsolute := []byte("---\nname: abs\ntrigger: t\n---\nSee [x](/etc/passwd).\n\n## Steps\n- s\n")
	if _, err := imp2.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{Markdown: withAbsolute}); !errors.Is(err, importer.ErrPackageSupportRefRemote) {
		t.Fatalf("absolute link: err=%v, want ErrPackageSupportRefRemote", err)
	}
	// Explicitly blank (no name) with no hint also fails validation.
	if _, err := imp2.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{
		Markdown: []byte("---\ntrigger: t\n---\n## Steps\n- s\n"),
	}); !errors.Is(err, skills.ErrInvalidSkill) {
		t.Fatalf("nameless markdown: err=%v, want ErrInvalidSkill", err)
	}
}
