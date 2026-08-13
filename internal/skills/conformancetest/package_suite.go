// package_suite.go — the canonical complete-skill-package semantic
// suite. It is the shared assertion set every consumer of the
// complete-skill-package core inherits: the versioned package hash,
// the deterministic serializer, the bounded immutable support URI, and
// the archive / SKILL.md validation primitives. Unlike the SkillStore
// driver suite, the core is concrete (there are no drivers to
// parameterise), so the suite takes no factory — it runs against the
// one canonical implementation and pins behaviors future resolvers
// and materializers must not drift from.
package conformancetest

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io/fs"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

// RunPackageSemanticsSuite executes the canonical complete-skill-
// package assertions against the shared `skills` facade (the one
// public surface of the semantic core).
func RunPackageSemanticsSuite(t *testing.T) {
	t.Helper()

	t.Run("package_hash_versioned_and_distinct", func(t *testing.T) {
		h, err := skills.PackageHash(packageFixture())
		if err != nil {
			t.Fatalf("PackageHash: %v", err)
		}
		if !strings.HasPrefix(h, "v1:") || len(h) != 3+64 {
			t.Fatalf("hash %q is not versioned v1:<64-hex>", h)
		}
		if err := skills.VerifyPackageHash(packageFixture(), h); err != nil {
			t.Fatalf("VerifyPackageHash: %v", err)
		}
		// The versioned package hash is distinct from the legacy
		// stored-row content hash (which has no version and covers no
		// support manifest).
		legacy := skills.CanonicalContentHash(skillAsStored(packageFixture()))
		if legacy == h || strings.HasPrefix(legacy, "v1:") {
			t.Fatalf("legacy content hash %q must differ from package hash %q", legacy, h)
		}
	})

	t.Run("canonical_serializer_round_trip", func(t *testing.T) {
		p := packageFixture()
		cb, err := skills.CanonicalPackageBytes(p)
		if err != nil {
			t.Fatalf("CanonicalPackageBytes: %v", err)
		}
		got, err := skills.PackageFromCanonicalBytes(cb)
		if err != nil {
			t.Fatalf("PackageFromCanonicalBytes: %v", err)
		}
		cb2, err := skills.CanonicalPackageBytes(got)
		if err != nil {
			t.Fatalf("CanonicalPackageBytes(round-tripped): %v", err)
		}
		if !bytes.Equal(cb, cb2) {
			t.Fatal("canonical serialization is not round-trip stable")
		}
	})

	t.Run("uri_resolver_neutral_round_trip", func(t *testing.T) {
		p := packageFixture()
		h, err := skills.PackageHash(p)
		if err != nil {
			t.Fatalf("PackageHash: %v", err)
		}
		path := p.Supports[0].Path
		u, err := skills.NewPackageURI(h, path)
		if err != nil {
			t.Fatalf("NewPackageURI: %v", err)
		}
		s := u.String()
		if !strings.HasPrefix(s, "skillpkg://"+h+"/"+path) {
			t.Fatalf("URI %q does not carry hash+support path verbatim", s)
		}
		// The `//` is the scheme's authority delimiter; the authority
		// position must never carry userinfo, host, query, or
		// fragment material.
		for _, forbidden := range []string{"@", "?", "#"} {
			if strings.Contains(s, forbidden) {
				t.Fatalf("URI %q carries %q material", s, forbidden)
			}
		}
		parsed, err := skills.ParsePackageURI(s)
		if err != nil {
			t.Fatalf("ParsePackageURI: %v", err)
		}
		if parsed.Hash != h || parsed.Path != path {
			t.Fatalf("URI round-trip mismatch: %+v", parsed)
		}
	})

	t.Run("archive_rejection_matrix", func(t *testing.T) {
		cases := []struct {
			name    string
			entries []zipEntry
			want    error
		}{
			{"traversal", []zipEntry{{name: "../x", data: "1"}}, skills.ErrArchiveTraversal},
			{"absolute", []zipEntry{{name: "/x", data: "1"}}, skills.ErrArchivePathInvalid},
			{"case collision", []zipEntry{{name: "SKILL.md", data: skillMD}, {name: "skill.md", data: skillMD}}, skills.ErrArchivePathCollision},
			{"non-regular symlink", []zipEntry{{name: "link", data: "t", mode: fs.ModeSymlink}}, skills.ErrArchiveNonRegular},
			{"unsupported mime", []zipEntry{{name: "a.exe", data: "MZ"}}, skills.ErrArchiveMimeUnsupported},
			{"nested archive", []zipEntry{{name: "payload.txt", data: "PK\x03\x04nested"}}, skills.ErrArchiveNested},
		}
		for _, c := range cases {
			z := zipBytes(t, c.entries)
			if _, err := skills.ValidatePackageArchive(z, skills.PackageArchiveLimits{}); !errors.Is(err, c.want) {
				t.Fatalf("%s: err=%v, want %v", c.name, err, c.want)
			}
		}
	})

	t.Run("skillmd_root_invariant", func(t *testing.T) {
		if err := skills.ValidateRootSkillEntries([]skills.ArchiveEntry{
			{Path: "SKILL.md"},
			{Path: "docs/skill.md"},
		}); !errors.Is(err, skills.ErrSkillMDNotRoot) {
			t.Fatalf("non-root case variant: err=%v", err)
		}
		if err := skills.ValidateRootSkillEntries([]skills.ArchiveEntry{
			{Path: "README.md"},
		}); !errors.Is(err, skills.ErrSkillMDMissing) {
			t.Fatalf("missing SKILL.md: err=%v", err)
		}
		if err := skills.ValidateSkillMarkdown([]byte(skillMD), skills.PackageMarkdownLimits{}); err != nil {
			t.Fatalf("ValidateSkillMarkdown: %v", err)
		}
	})
}

// packageFixture returns a canonical complete package with two
// support files. Digests are computed at runtime so the fixture can
// never drift from its bytes.
func packageFixture() skills.Package {
	demo := []byte(`{"demo": true}`)
	logo := pngBytes()
	return skills.Package{
		Name:    "suite-skill",
		Version: "1.0.0",
		Skill: skills.PackageSkill{
			Name:          "suite-skill",
			Title:         "Suite",
			Description:   "Conformance suite fixture.",
			Trigger:       "when the suite runs",
			TaskType:      "code",
			Tags:          []string{"beta", "alpha"},
			Steps:         []string{"run one", "run two"},
			Preconditions: []string{"precondition"},
			FailureModes:  []string{"failure"},
			RequiredTools: []string{"tool-a"},
			RequiredNS:    []string{"ns-a"},
			RequiredTags:  []string{"tag-a"},
		},
		Supports: []skills.SupportFile{
			{Path: "examples/demo.json", Mime: "application/json", Size: int64(len(demo)), Digest: hexDigest(demo), Data: demo},
			{Path: "assets/logo.png", Mime: "image/png", Size: int64(len(logo)), Digest: hexDigest(logo), Data: logo},
		},
	}
}

// pngBytes returns a structurally valid minimal PNG (signature +
// 13-byte IHDR + IEND). The content-truth MIME gate requires the real
// signature AND the IHDR chunk, so the fixture is a real container.
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

// skillAsStored projects the fixture's logical content onto the
// stored-skill envelope the legacy content hash covers (the seam that
// proves the two hash families differ).
func skillAsStored(p skills.Package) skills.Skill {
	return skills.Skill{
		Name:          p.Skill.Name,
		Title:         p.Skill.Title,
		Description:   p.Skill.Description,
		Trigger:       p.Skill.Trigger,
		TaskType:      p.Skill.TaskType,
		Tags:          p.Skill.Tags,
		Steps:         p.Skill.Steps,
		Preconditions: p.Skill.Preconditions,
		FailureModes:  p.Skill.FailureModes,
		RequiredTools: p.Skill.RequiredTools,
		RequiredNS:    p.Skill.RequiredNS,
		RequiredTags:  p.Skill.RequiredTags,
	}
}

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const skillMD = "---\nname: suite-skill\ntrigger: when the suite runs\n---\nSuite fixture.\n\n## Steps\n- run one\n- run two\n"

// zipEntry mirrors the archive-test builder so the suite is
// self-contained.
type zipEntry struct {
	name string
	data string
	mode fs.FileMode
}

func zipBytes(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name}
		fh.SetMode(e.mode)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatalf("Write(%q): %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
