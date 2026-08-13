package skillpkg_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// zipEntry is one entry for the test zip builder.
type zipEntry struct {
	name   string
	data   string
	mode   fs.FileMode
	method uint16 // 0 → Store (default); zip.Deflate for compression tests
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name}
		fh.SetMode(e.mode)
		fh.Method = e.method
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

func TestValidateArchive_Valid(t *testing.T) {
	z := buildZip(t, []zipEntry{
		{name: "SKILL.md", data: "---\ntrigger: demo\n---\n## Steps\n- do it\n"},
		{name: "examples/demo.json", data: `{"demo": true}`},
		{name: "assets/logo.png", data: string(pngBytes())},
	})
	entries, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{})
	if err != nil {
		t.Fatalf("ValidateArchive: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	got := entries[1]
	if got.Path != "examples/demo.json" || got.Mime != "application/json" || got.Size != int64(len(`{"demo": true}`)) {
		t.Fatalf("entry 1 = %+v", got)
	}
	if got.Digest != sha256Hex([]byte(`{"demo": true}`)) {
		t.Fatalf("digest mismatch: %q", got.Digest)
	}
	// Archive order is preserved (SKILL.md first as authored).
	if entries[0].Path != skillpkg.RootSkillFileName {
		t.Fatalf("first entry = %q", entries[0].Path)
	}
}

// TestValidateArchive_RejectsDirectoryEntries pins the P6.2 closure:
// every directory-shaped archive entry — a trailing-slash name or a
// directory mode bit — FAILS the archive (ErrArchiveNonRegular), it is
// never skipped as "content-free". A hostile name cannot hide inside a
// directory-shaped entry, and a SKILL.md case-variant directory cannot
// slip past the root-entry invariant.
func TestValidateArchive_RejectsDirectoryEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
	}{
		{"trailing slash", []zipEntry{{name: "dir/", data: ""}, {name: "dir/file.txt", data: "hi"}}},
		{"directory mode", []zipEntry{{name: "dir", data: "", mode: fs.ModeDir}, {name: "dir/file.txt", data: "hi"}}},
		{"root skill directory", []zipEntry{{name: "SKILL.md/", data: ""}}},
		{"case-variant skill directory", []zipEntry{{name: "skill.md/", data: ""}}},
		{"traversal-shaped directory", []zipEntry{{name: "../escape/", data: ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z := buildZip(t, c.entries)
			_, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{})
			if err == nil {
				t.Fatalf("ValidateArchive(%s): expected rejection, got nil", c.name)
			}
			if !errors.Is(err, skillpkg.ErrArchiveNonRegular) {
				t.Fatalf("ValidateArchive(%s): err=%v, want ErrArchiveNonRegular", c.name, err)
			}
		})
	}
}

func TestValidateArchive_Rejects(t *testing.T) {
	validMD := "---\ntrigger: t\n---\n## Steps\n- s\n"
	cases := []struct {
		name    string
		entries []zipEntry
		limits  skillpkg.ArchiveLimits
		wantErr error
	}{
		{"not a zip", []zipEntry{}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveNotZip},
		{"traversal", []zipEntry{{name: "../escape", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveTraversal},
		{"traversal nested", []zipEntry{{name: "a/../../escape", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveTraversal},
		{"absolute", []zipEntry{{name: "/etc/passwd", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"backslash", []zipEntry{{name: `a\b.txt`, data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"empty segment", []zipEntry{{name: "a//b.txt", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"dot segment", []zipEntry{{name: "a/./b.txt", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"empty name", []zipEntry{{name: "", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"non-ascii", []zipEntry{{name: "assets/éclair.png", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"uri path over budget", []zipEntry{{name: strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 28) + ".txt", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathInvalid},
		{"case collision", []zipEntry{
			{name: "SKILL.md", data: validMD},
			{name: "skill.md", data: validMD},
		}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathCollision},
		{"exact collision", []zipEntry{
			{name: "a.txt", data: "one"},
			{name: "a.txt", data: "two"},
		}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchivePathCollision},
		{"symlink", []zipEntry{{name: "link", data: "target", mode: fs.ModeSymlink}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveNonRegular},
		{"device", []zipEntry{{name: "dev", data: "x", mode: fs.ModeDevice}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveNonRegular},
		{"nested zip", []zipEntry{{name: "payload.txt", data: string(buildZip(t, []zipEntry{{name: "inner.txt", data: "x"}}))}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveNested},
		{"nested gzip", []zipEntry{{name: "payload.txt", data: "\x1f\x8b\x08\x00fake"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveNested},
		{"unsupported mime exe", []zipEntry{{name: "tool.exe", data: "MZfake"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeUnsupported},
		{"unsupported mime extensionless", []zipEntry{{name: "LICENSE", data: "text"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeUnsupported},
		{"unsupported mime archive ext", []zipEntry{{name: "bundle.tar", data: "x"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeUnsupported},
		{"unsupported mime yaml", []zipEntry{{name: "config.yaml", data: "a: 1"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeUnsupported},
		{"mime content mismatch png", []zipEntry{{name: "assets/logo.png", data: `{"demo": true}`}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeContentMismatch},
		{"mime content mismatch json", []zipEntry{{name: "examples/demo.json", data: "\xff\xfe\x00binary"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeContentMismatch},
		{"mime content mismatch md", []zipEntry{{name: "docs/guide.md", data: "\x00\x01\x02"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeContentMismatch},
		{"mime content mismatch xml", []zipEntry{{name: "data.xml", data: "<open>never closed"}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeContentMismatch},
		{"mime content mismatch empty txt", []zipEntry{{name: "empty.txt", data: ""}}, skillpkg.ArchiveLimits{}, skillpkg.ErrArchiveMimeContentMismatch},
		{"too many entries", []zipEntry{
			{name: "a.txt", data: "1"}, {name: "b.txt", data: "2"}, {name: "c.txt", data: "3"},
		}, skillpkg.ArchiveLimits{MaxEntries: 2}, skillpkg.ErrArchiveTooManyEntries},
		{"entry too large", []zipEntry{{name: "big.txt", data: "hello"}}, skillpkg.ArchiveLimits{MaxEntryBytes: 4}, skillpkg.ErrArchiveEntryTooLarge},
		{"total too large", []zipEntry{
			{name: "a.txt", data: "aaa"}, {name: "b.txt", data: "bbb"},
		}, skillpkg.ArchiveLimits{MaxTotalBytes: 4}, skillpkg.ErrArchiveTotalTooLarge},
		{"ratio too high", []zipEntry{{name: "bomb.txt", data: strings.Repeat("a", 1<<20), method: zip.Deflate}}, skillpkg.ArchiveLimits{MaxCompressionRatio: 2}, skillpkg.ErrArchiveRatioTooHigh},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var z []byte
			if c.name == "not a zip" {
				z = []byte("this is definitely not a zip archive")
			} else {
				z = buildZip(t, c.entries)
			}
			_, err := skillpkg.ValidateArchive(z, c.limits)
			if err == nil {
				t.Fatalf("ValidateArchive: expected %v, got nil", c.wantErr)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ValidateArchive: err=%v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestValidateArchive_Corrupt(t *testing.T) {
	z := buildZip(t, []zipEntry{{name: "SKILL.md", data: "---\ntrigger: t\n---\n## Steps\n- s\n"}})
	// Truncate the trailing central directory so the reader fails.
	truncated := z[:len(z)-20]
	if _, err := skillpkg.ValidateArchive(truncated, skillpkg.ArchiveLimits{}); !errors.Is(err, skillpkg.ErrArchiveCorrupt) {
		t.Fatalf("ValidateArchive(truncated): err=%v, want ErrArchiveCorrupt", err)
	}
}

func TestValidateArchive_EmptyArchive(t *testing.T) {
	// A zip with no entries is structurally valid but has no
	// SKILL.md; the root-entry check (not ValidateArchive) rejects it.
	z := buildZip(t, nil)
	entries, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{})
	if err != nil {
		t.Fatalf("ValidateArchive(empty): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(entries))
	}
}

// TestValidateArchive_URIPathBoundary pins the archive-side URI
// representability closure: a support path at exactly
// MaxPackageURIPathRunes is accepted and URI-representable, while one
// rune beyond it is rejected at the archive boundary with
// ErrArchivePathInvalid (it would otherwise only fail later at
// PackageHash).
func TestValidateArchive_URIPathBoundary(t *testing.T) {
	at := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 27) + ".txt"
	if len(at) != skillpkg.MaxPackageURIPathRunes {
		t.Fatalf("fixture path is %d runes, want %d", len(at), skillpkg.MaxPackageURIPathRunes)
	}
	z := buildZip(t, []zipEntry{
		{name: "SKILL.md", data: "---\ntrigger: demo\n---\n## Steps\n- do it\n"},
		{name: at, data: "x"},
	})
	entries, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{})
	if err != nil {
		t.Fatalf("ValidateArchive at URI path bound: %v", err)
	}
	if len(entries) != 2 || entries[1].Path != at {
		t.Fatalf("entries = %+v", entries)
	}
	// The at-bound path is fully URI-representable: hash + URI succeed.
	h, err := skillpkg.PackageHash(skillpkg.Package{
		Name:  "x",
		Skill: skillpkg.PackageSkill{Name: "x", Trigger: "t", Steps: []string{"s"}},
		Supports: []skillpkg.SupportFile{
			{Path: at, Mime: "text/plain; charset=utf-8", Size: 1, Digest: sha256Hex([]byte("x")), Data: []byte("x")},
		},
	})
	if err != nil {
		t.Fatalf("PackageHash(at-bound path): %v", err)
	}
	if _, err := skillpkg.NewURI(h, at); err != nil {
		t.Fatalf("NewURI(at-bound path): %v", err)
	}
}
