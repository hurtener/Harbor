package skillpkg_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// testSkill returns a minimal valid logical skill body.
func testSkill() skillpkg.PackageSkill {
	return skillpkg.PackageSkill{
		Name:          "demo-skill",
		Title:         "Demo",
		Description:   "A demo skill.",
		Trigger:       "when the user asks for a demo",
		TaskType:      "code",
		Tags:          []string{"alpha", "beta"},
		Steps:         []string{"step one", "step two"},
		Preconditions: []string{"precondition one"},
		FailureModes:  []string{"failure one"},
		RequiredTools: []string{"tool-a"},
		RequiredNS:    []string{"ns-a"},
		RequiredTags:  []string{"tag-a"},
	}
}

func supportFile(path, mime, content string) skillpkg.SupportFile {
	return skillpkg.SupportFile{
		Path:   path,
		Mime:   mime,
		Size:   int64(len(content)),
		Digest: sha256Hex([]byte(content)),
		Data:   []byte(content),
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pngBytes returns a structurally valid minimal PNG (signature +
// 13-byte IHDR + IEND) — the content check requires the signature AND
// the IHDR chunk, so fixtures must be real PNG containers, not magic
// fragments.
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

// testPackage returns a valid complete package with two support files.
func testPackage() skillpkg.Package {
	return skillpkg.Package{
		Name:    "demo-skill",
		Version: "1.0.0",
		Skill:   testSkill(),
		Supports: []skillpkg.SupportFile{
			supportFile("examples/demo.json", "application/json", `{"demo": true}`),
			supportFile("assets/logo.png", "image/png", string(pngBytes())),
		},
	}
}

func TestPackageValidate_Valid(t *testing.T) {
	if err := testPackage().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestPackageValidate_Rejects(t *testing.T) {
	valid := testPackage()
	cases := []struct {
		name   string
		mutate func(*skillpkg.Package)
	}{
		{"empty name", func(p *skillpkg.Package) { p.Name = " " }},
		{"non-canonical name", func(p *skillpkg.Package) { p.Name = "Demo Skill" }},
		{"name too long", func(p *skillpkg.Package) { p.Name = strings.Repeat("n", skillpkg.MaxPackageNameRunes+1) }},
		{"version too long", func(p *skillpkg.Package) { p.Version = strings.Repeat("v", skillpkg.MaxPackageVersionRunes+1) }},
		{"missing trigger", func(p *skillpkg.Package) { p.Skill.Trigger = "" }},
		{"empty steps", func(p *skillpkg.Package) { p.Skill.Steps = nil }},
		{"empty step item", func(p *skillpkg.Package) { p.Skill.Steps = []string{""} }},
		{"whitespace step item", func(p *skillpkg.Package) { p.Skill.Steps = []string{"   "} }},
		{"empty precondition item", func(p *skillpkg.Package) { p.Skill.Preconditions = []string{""} }},
		{"empty failure mode item", func(p *skillpkg.Package) { p.Skill.FailureModes = []string{""} }},
		{"steps too many", func(p *skillpkg.Package) {
			p.Skill.Steps = make([]string, skillpkg.MaxPackageSteps+1)
			for i := range p.Skill.Steps {
				p.Skill.Steps[i] = "s"
			}
		}},
		{"traversal support path", func(p *skillpkg.Package) {
			p.Supports[0].Path = "../escape.txt"
		}},
		{"absolute support path", func(p *skillpkg.Package) {
			p.Supports[0].Path = "/etc/passwd"
		}},
		{"backslash support path", func(p *skillpkg.Package) {
			p.Supports[0].Path = `assets\logo.png`
		}},
		{"non-ascii support path", func(p *skillpkg.Package) {
			p.Supports[0].Path = "assets/éclair.png"
		}},
		{"root SKILL.md as support", func(p *skillpkg.Package) {
			p.Supports[0].Path = skillpkg.RootSkillFileName
		}},
		{"unsupported mime", func(p *skillpkg.Package) {
			p.Supports[0].Mime = "application/zip"
		}},
		{"empty mime", func(p *skillpkg.Package) { p.Supports[0].Mime = "" }},
		{"bad digest", func(p *skillpkg.Package) {
			p.Supports[0].Digest = "not-a-digest"
		}},
		{"digest mismatch", func(p *skillpkg.Package) {
			p.Supports[0].Digest = strings.Repeat("0", 64)
		}},
		{"size mismatch", func(p *skillpkg.Package) {
			p.Supports[0].Size = p.Supports[0].Size + 1
		}},
		{"negative size", func(p *skillpkg.Package) { p.Supports[0].Size = -1 }},
		{"duplicate support path", func(p *skillpkg.Package) {
			p.Supports[1].Path = p.Supports[0].Path
		}},
		{"case-colliding support path", func(p *skillpkg.Package) {
			p.Supports[1].Path = strings.ToUpper(p.Supports[0].Path)
		}},
		{"support too large", func(p *skillpkg.Package) {
			p.Supports[0].Size = skillpkg.MaxPackageSupportBytes + 1
			p.Supports[0].Data = nil
		}},
		{"total too large", func(p *skillpkg.Package) {
			big := int64(skillpkg.MaxPackageTotalBytes) + 1
			p.Supports = []skillpkg.SupportFile{
				{Path: "big.txt", Mime: "text/plain; charset=utf-8", Size: big, Digest: strings.Repeat("0", 64)},
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid
			// deep-ish copy of supports so mutation is per-case.
			p.Supports = append([]skillpkg.SupportFile(nil), valid.Supports...)
			c.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate: expected error for %s", c.name)
			} else if !errors.Is(err, skillpkg.ErrInvalidPackage) && !errors.Is(err, skillpkg.ErrInvalidSkillContent) && !errors.Is(err, skillpkg.ErrInvalidSupport) && !errors.Is(err, skillpkg.ErrUnsupportedMime) && !errors.Is(err, skillpkg.ErrSupportDigestMismatch) && !errors.Is(err, skillpkg.ErrSupportSizeMismatch) {
				t.Fatalf("Validate(%s): error %v does not wrap a package sentinel", c.name, err)
			}
		})
	}
}

func TestSupportFileValidate_DataChecks(t *testing.T) {
	f := supportFile("a.json", "application/json", `{"a":1}`)
	if err := f.Validate(); err != nil {
		t.Fatalf("valid support file rejected: %v", err)
	}
	// No Data: digest + size accepted as manifest-only.
	manifestOnly := f
	manifestOnly.Data = nil
	if err := manifestOnly.Validate(); err != nil {
		t.Fatalf("manifest-only support file rejected: %v", err)
	}
}

// TestSupportFileValidate_MaterializedEmptyBytes pins the P3 closure:
// a materialized empty `[]byte{}` is NOT the manifest-only view. A
// nil `Data` is manifest-only (digest + size accepted unverified);
// a non-nil empty slice MUST be verified — size, digest, and MIME
// content — so a false digest on empty bytes can no longer bypass
// verification (the previous `len(f.Data) > 0` guard treated
// `[]byte{}` as manifest-only).
func TestSupportFileValidate_MaterializedEmptyBytes(t *testing.T) {
	emptyDigest := sha256Hex(nil) // sha256 of zero bytes: e3b0c442...
	materialized := skillpkg.SupportFile{
		Path:   "empty.txt",
		Mime:   "text/plain; charset=utf-8",
		Size:   0,
		Digest: emptyDigest,
		Data:   []byte{},
	}
	// The size and digest of the materialized empty bytes verify, then
	// the MIME content gate rejects empty text (ambiguous data) — the
	// verification chain RUNS, it is not skipped.
	if err := materialized.Validate(); !errors.Is(err, skillpkg.ErrMimeContentMismatch) {
		t.Fatalf("materialized empty text: err=%v, want ErrMimeContentMismatch (verification must run)", err)
	}

	// A false digest on materialized empty bytes is now caught (the
	// bypass: `[]byte{}` + Size 0 + false digest used to pass because
	// len(Data) == 0 skipped every check).
	falseDigest := materialized
	falseDigest.Digest = strings.Repeat("0", 64)
	if err := falseDigest.Validate(); !errors.Is(err, skillpkg.ErrSupportDigestMismatch) {
		t.Fatalf("materialized empty with false digest: err=%v, want ErrSupportDigestMismatch", err)
	}

	// A size lie on materialized empty bytes is caught too.
	liesSize := materialized
	liesSize.Size = 1
	if err := liesSize.Validate(); !errors.Is(err, skillpkg.ErrSupportSizeMismatch) {
		t.Fatalf("materialized empty with lying size: err=%v, want ErrSupportSizeMismatch", err)
	}

	// Nil Data is still the manifest-only view: the same digest + size
	// pass without byte verification.
	manifestOnly := materialized
	manifestOnly.Data = nil
	if err := manifestOnly.Validate(); err != nil {
		t.Fatalf("manifest-only empty support file rejected: %v", err)
	}
}

// TestPackageValidate_NameSkillAgreement pins the dual-name closure:
// the package envelope name must equal the canonical form of the
// carried skill name. Two disagreeing names would let the same
// reviewed hash present different names to different consumers.
func TestPackageValidate_NameSkillAgreement(t *testing.T) {
	p := testPackage()
	if err := p.Validate(); err != nil {
		t.Fatalf("valid package rejected: %v", err)
	}
	// Case / whitespace variants of the skill name still agree with
	// the canonical package name.
	variants := []struct {
		name      string
		skillName string
	}{
		{"case variant", "Demo-Skill"},
		{"whitespace variant", "  demo-skill  "},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			q := p
			q.Skill.Name = v.skillName
			if err := q.Validate(); err != nil {
				t.Fatalf("package with skill name %q rejected: %v", v.skillName, err)
			}
		})
	}
	// A genuinely different skill name disagrees with the package name.
	mismatch := p
	mismatch.Skill.Name = "other-skill"
	if err := mismatch.Validate(); !errors.Is(err, skillpkg.ErrInvalidPackage) {
		t.Fatalf("mismatched skill name: err=%v, want ErrInvalidPackage", err)
	}
}

// TestPackageSkillValidate_SectionCardinality pins the P5 bound:
// Preconditions and FailureModes are ordered text lists like Steps and
// share the same cardinality limit (MaxPackageSections).
func TestPackageSkillValidate_SectionCardinality(t *testing.T) {
	over := make([]string, skillpkg.MaxPackageSections+1)
	for i := range over {
		over[i] = "entry"
	}
	s := testSkill()
	s.Preconditions = over
	if err := s.Validate(); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
		t.Fatalf("oversized Preconditions: err=%v, want ErrInvalidSkillContent", err)
	}
	s2 := testSkill()
	s2.FailureModes = over
	if err := s2.Validate(); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
		t.Fatalf("oversized FailureModes: err=%v, want ErrInvalidSkillContent", err)
	}
	// At the bound, both validate.
	at := make([]string, skillpkg.MaxPackageSections)
	for i := range at {
		at[i] = "entry"
	}
	s3 := testSkill()
	s3.Preconditions = at
	s3.FailureModes = at
	if err := s3.Validate(); err != nil {
		t.Fatalf("at-bound sections rejected: %v", err)
	}
}

// TestSupportFileValidate_URIRepresentability pins the P6.3 closure:
// every support path Package.Validate accepts must be representable by
// the exact bounded skillpkg URI constructor. A path within the raw
// path bound but beyond the URI path budget is rejected at the DTO
// boundary (it would fail NewURI / MaterializeSupportRefs later).
func TestSupportFileValidate_URIRepresentability(t *testing.T) {
	// A path at exactly MaxPackageURIPathRunes (433 runes across
	// three bounded segments) validates and its URI fits MaxURIRunes.
	pathAt := func(total int) string {
		return strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", total-402)
	}
	at := pathAt(skillpkg.MaxPackageURIPathRunes) // 200+1+200+1+31 = 433
	f := skillpkg.SupportFile{
		Path:   at,
		Mime:   "text/plain; charset=utf-8",
		Size:   1,
		Digest: sha256Hex([]byte("x")),
		Data:   []byte("x"),
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("at-bound URI path rejected: %v", err)
	}
	hash, err := skillpkg.PackageHash(skillpkg.Package{
		Name:     "x",
		Skill:    skillpkg.PackageSkill{Name: "x", Trigger: "t", Steps: []string{"s"}},
		Supports: []skillpkg.SupportFile{f},
	})
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	if _, err := skillpkg.NewURI(hash, at); err != nil {
		t.Fatalf("NewURI(at-bound path): %v", err)
	}
	// One rune beyond the URI path budget is rejected by
	// SupportFile.Validate — and would exceed MaxURIRunes in the URI
	// constructor.
	over := pathAt(skillpkg.MaxPackageURIPathRunes + 1)
	fo := f
	fo.Path = over
	if err := fo.Validate(); !errors.Is(err, skillpkg.ErrInvalidSupport) {
		t.Fatalf("over-URI-bound path: err=%v, want ErrInvalidSupport", err)
	}
}

func TestPackageSkillValidate_AnnotationBounds(t *testing.T) {
	s := testSkill()
	s.Tags = make([]string, skillpkg.MaxPackageTags+1)
	if err := s.Validate(); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
		t.Fatalf("expected invalid skill content for oversized tags, got %v", err)
	}
}
