package skillpkg_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// testPackage returns a valid complete package with two support files.
func testPackage() skillpkg.Package {
	return skillpkg.Package{
		Name:    "demo-skill",
		Version: "1.0.0",
		Skill:   testSkill(),
		Supports: []skillpkg.SupportFile{
			supportFile("examples/demo.json", "application/json", `{"demo": true}`),
			supportFile("assets/logo.png", "image/png", "\x89PNG\r\n\x1a\nfakepng"),
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

func TestPackageSkillValidate_AnnotationBounds(t *testing.T) {
	s := testSkill()
	s.Tags = make([]string, skillpkg.MaxPackageTags+1)
	if err := s.Validate(); !errors.Is(err, skillpkg.ErrInvalidSkillContent) {
		t.Fatalf("expected invalid skill content for oversized tags, got %v", err)
	}
}
