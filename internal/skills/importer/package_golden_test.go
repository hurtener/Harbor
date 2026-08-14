package importer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// goldenSkillBody is the canonical logical BODY of the golden package
// (the materialize helpers operate on the body, not the frontmatter).
// It exercises inline and reference-style links AND images, a remote
// navigation link and a fragment-only document anchor (both retained),
// and every support file the manifest carries.
const goldenSkillBody = "A golden multi-file package.\n" +
	"\n" +
	"![diagram](assets/logo.png)\n" +
	"\n" +
	"Inline link to [the guide](docs/guide.md) and [the demo](examples/demo.json).\n" +
	"\n" +
	"[logo]: assets/logo.png\n" +
	"[guide]: docs/guide.md\n" +
	"[usage]: docs/usage.txt\n" +
	"\n" +
	"Reference image ![logo][logo], reference link [guide][guide], and collapsed [usage][].\n" +
	"\n" +
	"External [site](https://example.com) and an [anchor](#overview) stay verbatim.\n" +
	"\n" +
	"## Steps\n" +
	"\n" +
	"- run the golden\n"

// goldenSkillMD is the full canonical logical SKILL.md document.
const goldenSkillMD = "---\n" +
	"name: golden-skill\n" +
	"trigger: when the golden runs\n" +
	"---\n" +
	goldenSkillBody

// goldenEntries is the logical multi-file package behind goldenSkillMD.
func goldenEntries() map[string]string {
	return map[string]string{
		"SKILL.md":           goldenSkillMD,
		"assets/logo.png":    string(pngBytes()),
		"docs/guide.md":      "# Guide\n",
		"docs/usage.txt":     "usage notes\n",
		"examples/demo.json": `{"demo": true}`,
	}
}

// TestPackageGolden_FullRoundTrip is the pinned golden: logical
// multi-file package -> canonical semantic hash -> per-support URI ->
// bounded resolver (bytes/MIME/hash/size) -> materialized body ->
// dematerialized/exported logical body + manifest -> reimport. The
// final logical body, ordered manifest, package hash, and support
// hashes match exactly.
func TestPackageGolden_FullRoundTrip(t *testing.T) {
	imp, _ := newImporter(t)

	// 1. Ingest the logical multi-file package.
	ingest, err := imp.ImportPackage(context.Background(), importer.PackageSource{
		Archive:  packageZip(t, goldenEntries()),
		PathHint: "golden-skill.zip",
	})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}
	hash := ingest.Hash
	if !strings.HasPrefix(hash, "v1:") || len(hash) != 3+64 {
		t.Fatalf("hash %q is not versioned v1:<64-hex>", hash)
	}
	if err := skillpkg.VerifyPackageHash(ingest.Package, hash); err != nil {
		t.Fatalf("VerifyPackageHash: %v", err)
	}
	if len(ingest.Package.Supports) != 4 {
		t.Fatalf("supports = %d, want 4", len(ingest.Package.Supports))
	}

	// 2. Per-support URI: one exact immutable URI per manifest entry.
	uris := map[string]skillpkg.URI{}
	for _, f := range ingest.Package.Supports {
		u, err := ingest.SupportURI(f.Path)
		if err != nil {
			t.Fatalf("SupportURI(%q): %v", f.Path, err)
		}
		// The canonical path encodes to itself: the identity encoding.
		wantStr := "skillpkg://" + hash + "/" + f.Path
		if u.String() != wantStr {
			t.Fatalf("URI(%q) = %q, want %q", f.Path, u.String(), wantStr)
		}
		uris[f.Path] = u
		// The identity encoding round-trips through the strict parser.
		parsed, err := skills.ParsePackageURI(u.String())
		if err != nil || parsed.Hash != hash || parsed.Path != f.Path {
			t.Fatalf("ParsePackageURI(%q): %+v err=%v", u.String(), parsed, err)
		}
	}

	// 3. Bounded resolver: bytes / MIME / hash / size per support URI.
	origBytes := goldenEntries()
	for _, f := range ingest.Package.Supports {
		resolved, err := skillpkg.ResolveSupportURI(uris[f.Path], ingest.Package)
		if err != nil {
			t.Fatalf("ResolveSupportURI(%q): %v", f.Path, err)
		}
		wantData := []byte(origBytes[f.Path])
		if resolved.Path != f.Path || resolved.Mime != f.Mime ||
			resolved.Size != int64(len(wantData)) || resolved.Digest != f.Digest {
			t.Fatalf("resolved %q = %+v", f.Path, resolved)
		}
		if !bytes.Equal(resolved.Data, wantData) {
			t.Fatalf("resolved bytes for %q do not match the archive", f.Path)
		}
		sum := sha256.Sum256(wantData)
		if f.Digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("support digest for %q does not match its bytes", f.Path)
		}
	}
	// The resolver refuses foreign and dangling URIs.
	foreign, err := skillpkg.NewURI("v1:"+strings.Repeat("0", 64), "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI(foreign): %v", err)
	}
	if _, err := skillpkg.ResolveSupportURI(foreign, ingest.Package); !errors.Is(err, skillpkg.ErrSupportRefForeignURI) {
		t.Fatalf("foreign resolve: err=%v, want ErrSupportRefForeignURI", err)
	}
	dangling, err := skillpkg.NewURI(hash, "assets/nope.png")
	if err != nil {
		t.Fatalf("NewURI(dangling): %v", err)
	}
	if _, err := skillpkg.ResolveSupportURI(dangling, ingest.Package); !errors.Is(err, skillpkg.ErrSupportRefDangling) {
		t.Fatalf("dangling resolve: err=%v, want ErrSupportRefDangling", err)
	}

	// 4. Materialized body: validated relative support refs become the
	// exact skillpkg URIs; remote links and anchors stay verbatim.
	matBody, err := imp.MaterializePackageBody(context.Background(), ingest)
	if err != nil {
		t.Fatalf("MaterializePackageBody: %v", err)
	}
	logoURI := "skillpkg://" + hash + "/assets/logo.png"
	guideURI := "skillpkg://" + hash + "/docs/guide.md"
	demoURI := "skillpkg://" + hash + "/examples/demo.json"
	usageURI := "skillpkg://" + hash + "/docs/usage.txt"
	wantMat := strings.ReplaceAll(goldenSkillBody, "assets/logo.png", logoURI)
	wantMat = strings.ReplaceAll(wantMat, "docs/guide.md", guideURI)
	wantMat = strings.ReplaceAll(wantMat, "examples/demo.json", demoURI)
	wantMat = strings.ReplaceAll(wantMat, "docs/usage.txt", usageURI)
	if matBody != wantMat {
		t.Fatalf("materialized body drifted:\n--- got ---\n%s\n--- want ---\n%s", matBody, wantMat)
	}
	if strings.Contains(matBody, "https://example.com") == false || strings.Contains(matBody, "#overview") == false {
		t.Fatalf("remote link / anchor must stay verbatim in the materialized body")
	}

	// 5. Export / dematerialization: URIs of the exact package/hash
	// reverse to the relative logical body; the logical document and
	// ordered manifest are exact.
	ex, err := imp.ExportPackage(context.Background(), ingest, matBody)
	if err != nil {
		t.Fatalf("ExportPackage: %v", err)
	}
	if !bytes.Equal(ex.Document, []byte(goldenSkillMD)) {
		t.Fatalf("exported logical document drifted:\n--- got ---\n%s\n--- want ---\n%s", ex.Document, goldenSkillMD)
	}
	if ex.Hash != hash {
		t.Fatalf("export hash = %q, want %q", ex.Hash, hash)
	}
	if len(ex.Manifest) != 4 {
		t.Fatalf("export manifest = %d entries, want 4", len(ex.Manifest))
	}
	wantPaths := []string{"assets/logo.png", "docs/guide.md", "docs/usage.txt", "examples/demo.json"}
	for i, f := range ex.Manifest {
		if f.Path != wantPaths[i] || f.Size != ingest.Package.Supports[i].Size ||
			f.Digest != ingest.Package.Supports[i].Digest || f.Mime != ingest.Package.Supports[i].Mime {
			t.Fatalf("export manifest[%d] = %+v, want match with %+v", i, f, ingest.Package.Supports[i])
		}
		if len(f.Data) != 0 {
			t.Fatalf("export manifest carries Data for %q", f.Path)
		}
	}

	// 6. Reimport: the exported logical body + manifest reproduce the
	// exact package — logical body, ordered manifest, package hash,
	// and support hashes all match.
	re, err := imp.ReimportPackage(context.Background(), ex)
	if err != nil {
		t.Fatalf("ReimportPackage: %v", err)
	}
	if re.Hash != hash {
		t.Fatalf("reimported hash %q != original %q", re.Hash, hash)
	}
	if re.Skill.Description != ingest.Skill.Description {
		t.Fatalf("reimported description drifted:\n%q\nvs\n%q", re.Skill.Description, ingest.Skill.Description)
	}
	if strings.Join(re.Skill.Steps, "\n") != strings.Join(ingest.Skill.Steps, "\n") {
		t.Fatalf("reimported steps drifted: %v vs %v", re.Skill.Steps, ingest.Skill.Steps)
	}
	if re.Skill.ContentHash != ingest.Skill.ContentHash {
		t.Fatalf("reimported ContentHash drifted")
	}
	origCB, err := skillpkg.CanonicalBytes(ingest.Package)
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	reCB, err := skillpkg.CanonicalBytes(re.Package)
	if err != nil {
		t.Fatalf("CanonicalBytes(re): %v", err)
	}
	if !bytes.Equal(origCB, reCB) {
		t.Fatalf("reimported canonical package drifted:\n%s\nvs\n%s", reCB, origCB)
	}
	for i, f := range re.Package.Supports {
		if f.Path != ingest.Package.Supports[i].Path ||
			f.Digest != ingest.Package.Supports[i].Digest ||
			f.Size != ingest.Package.Supports[i].Size ||
			f.Mime != ingest.Package.Supports[i].Mime {
			t.Fatalf("reimported support %d = %+v, want %+v", i, f, ingest.Package.Supports[i])
		}
	}
	if err := skillpkg.VerifyPackageHash(re.Package, hash); err != nil {
		t.Fatalf("VerifyPackageHash(re): %v", err)
	}
}

// TestPackageGolden_PercentEncoding pins the percent-encoding arms of
// the golden: the materialized URI is the identity encoding of the
// canonical path (encoded form round-trips through the strict
// parser), while encoded variants — in a URI or a body reference —
// are rejected as ambiguous/non-canonical.
func TestPackageGolden_PercentEncoding(t *testing.T) {
	imp, _ := newImporter(t)
	ingest, err := imp.ImportPackage(context.Background(), importer.PackageSource{
		Archive:  packageZip(t, goldenEntries()),
		PathHint: "golden-skill.zip",
	})
	if err != nil {
		t.Fatalf("ImportPackage: %v", err)
	}

	// Identity encoding: the canonical path needs no percent-escapes.
	u, err := ingest.SupportURI("assets/logo.png")
	if err != nil {
		t.Fatalf("SupportURI: %v", err)
	}
	if strings.Contains(u.String(), "%") {
		t.Fatalf("identity encoding violated: %q", u.String())
	}

	// An encoded-variant URI is not the canonical form and is refused.
	for _, bad := range []string{
		"skillpkg://" + ingest.Hash + "/assets%2Flogo.png",
		"skillpkg://" + ingest.Hash + "/%61ssets/logo.png",
		"skillpkg://" + ingest.Hash + "/assets%2elogo.png",
	} {
		if _, err := skills.ParsePackageURI(bad); !errors.Is(err, skillpkg.ErrMalformedURI) {
			t.Fatalf("ParsePackageURI(%q): err=%v, want ErrMalformedURI", bad, err)
		}
	}

	// A percent-encoded body reference decodes outside the canonical
	// path charset and cannot name a manifest entry.
	badMD := strings.Replace(goldenSkillMD, "assets/logo.png", "assets%2Flogo.png", 1)
	_, err = imp.ImportPackage(context.Background(), importer.PackageSource{
		Archive: packageZip(t, map[string]string{
			"SKILL.md":        badMD,
			"assets/logo.png": string(pngBytes()),
		}),
	})
	if !errors.Is(err, importer.ErrPackageSupportRefMissing) {
		t.Fatalf("percent-encoded ref: err=%v, want ErrPackageSupportRefMissing", err)
	}
}

// TestPackageGolden_MimeContentMismatch pins the extension / content
// mismatch arms of the golden: a suffix alone never claims a MIME —
// an image extension carrying text, and a JSON extension carrying
// binary, are rejected at ingest with the content-mismatch sentinel.
func TestPackageGolden_MimeContentMismatch(t *testing.T) {
	imp, _ := newImporter(t)

	pngAsJSON := goldenEntries()
	pngAsJSON["assets/logo.png"] = `{"not": "a png"}`
	if _, err := imp.ImportPackage(context.Background(), importer.PackageSource{
		Archive: packageZip(t, pngAsJSON),
	}); !errors.Is(err, skillpkg.ErrArchiveMimeContentMismatch) {
		t.Fatalf("png-as-json: err=%v, want ErrArchiveMimeContentMismatch", err)
	}

	jsonAsBinary := goldenEntries()
	jsonAsBinary["examples/demo.json"] = "\x00\x01\x02\xffbinary"
	if _, err := imp.ImportPackage(context.Background(), importer.PackageSource{
		Archive: packageZip(t, jsonAsBinary),
	}); !errors.Is(err, skillpkg.ErrArchiveMimeContentMismatch) {
		t.Fatalf("json-as-binary: err=%v, want ErrArchiveMimeContentMismatch", err)
	}
}
