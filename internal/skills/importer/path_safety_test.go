package importer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveSafePath_HappyPath(t *testing.T) {
	root := t.TempDir()
	rel := "foo/bar.txt"
	got, err := resolveSafePath(root, rel)
	if err != nil {
		t.Fatalf("resolveSafePath: %v", err)
	}
	wantAbs := filepath.Join(root, rel)
	wantClean, _ := filepath.Abs(filepath.Clean(wantAbs))
	if got != wantClean {
		t.Errorf("got %q, want %q", got, wantClean)
	}
}

func TestResolveSafePath_RejectionTable(t *testing.T) {
	root := t.TempDir()
	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		name    string
		root    string
		rel     string
		wantErr error
	}{
		{"empty-root", "", "foo", ErrAttachmentOutsideRoot},
		{"empty-rel", root, "", ErrAttachmentOutsideRoot},
		{"absolute-rel", root, "/etc/passwd", ErrAttachmentOutsideRoot},
		{"dotdot-escape", root, "../escape", ErrAttachmentOutsideRoot},
		{"deep-dotdot-escape", root, "../../etc/passwd", ErrAttachmentOutsideRoot},
		{"join-with-dotdot-escape", root, "allowed/../../escape", ErrAttachmentOutsideRoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSafePath(tc.root, tc.rel)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestResolveSafePath_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test skipped on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	linkPath := filepath.Join(root, "link")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, err := resolveSafePath(root, "link")
	if !errors.Is(err, ErrAttachmentOutsideRoot) {
		t.Errorf("symlink escape: err = %v, want ErrAttachmentOutsideRoot", err)
	}
}

func TestResolveSafePath_NonexistentPathLexicalOnly(t *testing.T) {
	root := t.TempDir()
	// The path doesn't exist on disk; the lexical check still passes.
	got, err := resolveSafePath(root, "nonexistent/subdir/file.txt")
	if err != nil {
		t.Fatalf("resolveSafePath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

func TestPathHasPrefix(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/abc", "/a", false}, // false-positive defense
		{"/a/b" + sep + "c", "/a/b", true},
	}
	for _, tc := range cases {
		got := pathHasPrefix(tc.p, tc.root)
		if got != tc.want {
			t.Errorf("pathHasPrefix(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
}
