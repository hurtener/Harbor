package importer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

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
		"assets/logo.png":    "PNGDATA!",
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

	// Hash + URI: versioned, distinct from the stored ContentHash,
	// and the URI round-trips.
	if !strings.HasPrefix(ingest.Hash, "v1:") || ingest.Hash == ingest.Skill.ContentHash {
		t.Fatalf("hash %q (content %q)", ingest.Hash, ingest.Skill.ContentHash)
	}
	if ingest.URI.String() != "skillpkg:"+ingest.Hash+"/packaged-demo" {
		t.Fatalf("URI = %q", ingest.URI.String())
	}
	parsed, err := skills.ParsePackageURI(ingest.URI.String())
	if err != nil || parsed.Hash != ingest.Hash {
		t.Fatalf("URI round-trip: %+v err=%v", parsed, err)
	}
	if err := skills.VerifyPackageHash(ingest.Package, ingest.Hash); err != nil {
		t.Fatalf("VerifyPackageHash: %v", err)
	}
}

func TestImportPackage_DeterministicHash(t *testing.T) {
	imp, _ := newImporter(t)
	entries := map[string]string{
		"SKILL.md":           packageSkillMD,
		"assets/logo.png":    "PNGDATA!",
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
	if a.Hash != b.Hash || a.URI.String() != b.URI.String() {
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
		{"traversal support ref", packageZip(t, map[string]string{
			"SKILL.md": "---\nname: x\ntrigger: t\n---\n![bad](../escape.png)\n\n## Steps\n- s\n",
		}), importer.ErrPackageSupportRefMissing},
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
}
