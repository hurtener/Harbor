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

func TestValidateArchive_SkipsDirectoryEntries(t *testing.T) {
	z := buildZip(t, []zipEntry{
		{name: "dir/", data: ""},
		{name: "dir/file.txt", data: "hi"},
	})
	entries, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{})
	if err != nil {
		t.Fatalf("ValidateArchive: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "dir/file.txt" {
		t.Fatalf("entries = %+v", entries)
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
