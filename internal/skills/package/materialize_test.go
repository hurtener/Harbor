package skillpkg_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// packageWithRefs returns a package whose support manifest matches the
// body refs the materialize tests use.
func packageWithRefs(t *testing.T) skillpkg.Package {
	t.Helper()
	return skillpkg.Package{
		Name:    "refs-skill",
		Version: "1.0.0",
		Skill: skillpkg.PackageSkill{
			Name:    "refs-skill",
			Trigger: "when refs are tested",
			Steps:   []string{"step one"},
		},
		Supports: []skillpkg.SupportFile{
			supportFile("assets/logo.png", "image/png", string(pngBytes())),
			supportFile("docs/guide.md", "text/markdown", "# Guide\n"),
			supportFile("examples/demo.json", "application/json", `{"demo": true}`),
		},
	}
}

func packageHashOf(t *testing.T, p skillpkg.Package) string {
	t.Helper()
	h, err := skillpkg.PackageHash(p)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	return h
}

// TestMaterializeSupportRefs_RewritesRelativeRefs pins the exact
// materialized form: every validated relative support reference
// (inline and reference-definition) becomes the exact
// `skillpkg://<hash>/<encoded path>` URI; remote links and document
// anchors stay verbatim.
func TestMaterializeSupportRefs_RewritesRelativeRefs(t *testing.T) {
	p := packageWithRefs(t)
	h := packageHashOf(t, p)
	body := "See [the guide](docs/guide.md) and ![diagram](assets/logo.png).\n\n" +
		"[logo]: assets/logo.png\n[guide]: docs/guide.md\n\n" +
		"Ref usage ![logo][logo] and [guide][guide].\n\n" +
		"Anchored [section](docs/guide.md#sec) keeps its anchor.\n\n" +
		"External [site](https://example.com) and an [anchor](#overview) stay verbatim.\n"
	got, err := skillpkg.MaterializeSupportRefs(body, p)
	if err != nil {
		t.Fatalf("MaterializeSupportRefs: %v", err)
	}
	for _, path := range []string{"assets/logo.png", "docs/guide.md"} {
		uri := "skillpkg://" + h + "/" + path
		if !strings.Contains(got, uri) {
			t.Fatalf("materialized body missing %q:\n%s", uri, got)
		}
	}
	if !strings.Contains(got, "[logo]: skillpkg://"+h+"/assets/logo.png") {
		t.Fatalf("definition dest not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "skillpkg://"+h+"/docs/guide.md#sec") {
		t.Fatalf("anchored ref must keep its fragment:\n%s", got)
	}
	if !strings.Contains(got, "https://example.com") || !strings.Contains(got, "#overview") {
		t.Fatalf("remote link / anchor must stay verbatim:\n%s", got)
	}
	// The identity encoding: canonical paths encode to themselves.
	if strings.Contains(got, "%2F") || strings.Contains(got, "%2e") {
		t.Fatalf("unexpected percent-encoding in materialized body:\n%s", got)
	}
}

// TestMaterializeSupportRefs_DanglingRefFails pins the defensive
// strictness: a relative destination that is not a manifest entry (or
// cannot canonicalize) refuses loudly rather than producing a dangling
// URI.
func TestMaterializeSupportRefs_DanglingRefFails(t *testing.T) {
	p := packageWithRefs(t)
	body := "Broken ![x](assets/nope.png)"
	_, err := skillpkg.MaterializeSupportRefs(body, p)
	if !errors.Is(err, skillpkg.ErrSupportRefDangling) {
		t.Fatalf("err=%v, want ErrSupportRefDangling", err)
	}
	// A percent-encoded ref decodes outside the canonical charset.
	body2 := "Broken ![x](assets%2Flogo.png)"
	if _, err := skillpkg.MaterializeSupportRefs(body2, p); !errors.Is(err, skillpkg.ErrSupportRefDangling) {
		t.Fatalf("percent-encoded ref: err=%v, want ErrSupportRefDangling", err)
	}
}

// TestDematerializeSupportRefs_ExactInverse pins the round-trip: the
// materialized body dematerializes back to the exact logical body,
// byte for byte.
func TestDematerializeSupportRefs_ExactInverse(t *testing.T) {
	p := packageWithRefs(t)
	body := "See [the guide](docs/guide.md) and ![diagram](assets/logo.png).\n\n" +
		"[logo]: assets/logo.png\n[guide]: docs/guide.md\n\n" +
		"Ref usage ![logo][logo] and [guide][guide].\n\n" +
		"Anchored [section](docs/guide.md#sec) keeps its anchor.\n\n" +
		"External [site](https://example.com) and an [anchor](#overview).\n"
	mat, err := skillpkg.MaterializeSupportRefs(body, p)
	if err != nil {
		t.Fatalf("MaterializeSupportRefs: %v", err)
	}
	got, err := skillpkg.DematerializeSupportRefs(mat, p)
	if err != nil {
		t.Fatalf("DematerializeSupportRefs: %v", err)
	}
	if got != body {
		t.Fatalf("round-trip drifted:\n--- got ---\n%s\n--- want ---\n%s", got, body)
	}
}

// TestDematerializeSupportRefs_RefusesForeignMalformedDangling pins
// the refusal contract: only URIs of the exact package/hash whose
// paths are manifest entries dematerialize; everything else refuses
// loudly.
func TestDematerializeSupportRefs_RefusesForeignMalformedDangling(t *testing.T) {
	p := packageWithRefs(t)
	h := packageHashOf(t, p)
	foreignHash := "v1:" + strings.Repeat("0", 64)

	foreign := "see skillpkg://" + foreignHash + "/assets/logo.png"
	if _, err := skillpkg.DematerializeSupportRefs(foreign, p); !errors.Is(err, skillpkg.ErrSupportRefForeignURI) {
		t.Fatalf("foreign: err=%v, want ErrSupportRefForeignURI", err)
	}
	malformed := "see skillpkg://not-a-uri/at-all"
	if _, err := skillpkg.DematerializeSupportRefs(malformed, p); !errors.Is(err, skillpkg.ErrSupportRefMalformedURI) {
		t.Fatalf("malformed: err=%v, want ErrSupportRefMalformedURI", err)
	}
	dangling := "see skillpkg://" + h + "/assets/nope.png"
	if _, err := skillpkg.DematerializeSupportRefs(dangling, p); !errors.Is(err, skillpkg.ErrSupportRefDangling) {
		t.Fatalf("dangling: err=%v, want ErrSupportRefDangling", err)
	}
	// A correct URI in prose dematerializes back to its path.
	ok := "see skillpkg://" + h + "/assets/logo.png"
	got, err := skillpkg.DematerializeSupportRefs(ok, p)
	if err != nil {
		t.Fatalf("DematerializeSupportRefs(valid): %v", err)
	}
	if got != "see assets/logo.png" {
		t.Fatalf("got %q, want %q", got, "see assets/logo.png")
	}
}

// TestResolveSupportURI pins the bounded resolver: it returns the
// exact manifest entry (bytes, MIME, size, digest) for a URI of the
// exact package and refuses foreign hashes and dangling paths.
func TestResolveSupportURI(t *testing.T) {
	p := packageWithRefs(t)
	h := packageHashOf(t, p)
	u, err := skillpkg.NewURI(h, "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	got, err := skillpkg.ResolveSupportURI(u, p)
	if err != nil {
		t.Fatalf("ResolveSupportURI: %v", err)
	}
	if got.Path != "assets/logo.png" || got.Mime != "image/png" || got.Size != int64(len(pngBytes())) {
		t.Fatalf("resolved = %+v", got)
	}
	sum := sha256.Sum256(got.Data)
	if hex.EncodeToString(sum[:]) != got.Digest || got.Digest != p.Supports[0].Digest {
		t.Fatalf("resolved digest mismatch")
	}

	foreign, err := skillpkg.NewURI("v1:"+strings.Repeat("0", 64), "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI(foreign): %v", err)
	}
	if _, err := skillpkg.ResolveSupportURI(foreign, p); !errors.Is(err, skillpkg.ErrSupportRefForeignURI) {
		t.Fatalf("foreign: err=%v, want ErrSupportRefForeignURI", err)
	}
	dangling, err := skillpkg.NewURI(h, "assets/nope.png")
	if err != nil {
		t.Fatalf("NewURI(dangling): %v", err)
	}
	if _, err := skillpkg.ResolveSupportURI(dangling, p); !errors.Is(err, skillpkg.ErrSupportRefDangling) {
		t.Fatalf("dangling: err=%v, want ErrSupportRefDangling", err)
	}
}

// TestMaterializeDematerialize_NoRefsPassThrough verifies the helpers
// are identity transforms for bodies without support references.
func TestMaterializeDematerialize_NoRefsPassThrough(t *testing.T) {
	p := packageWithRefs(t)
	body := "Plain prose, no refs."
	got, err := skillpkg.MaterializeSupportRefs(body, p)
	if err != nil {
		t.Fatalf("MaterializeSupportRefs: %v", err)
	}
	if got != body {
		t.Fatalf("materialize changed a ref-free body")
	}
	back, err := skillpkg.DematerializeSupportRefs(got, p)
	if err != nil {
		t.Fatalf("DematerializeSupportRefs: %v", err)
	}
	if back != body {
		t.Fatalf("dematerialize changed a ref-free body")
	}
}
