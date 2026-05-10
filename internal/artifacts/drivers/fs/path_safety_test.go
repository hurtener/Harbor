package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	"github.com/hurtener/Harbor/internal/config"
)

// TestPathSafety_FilenameIsMetadataOnly verifies the contract: the
// `Filename` field is metadata only. A traversal-style filename does
// not influence the on-disk path; the `id` is what's used.
func TestPathSafety_FilenameIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	ref, err := s.PutBytes(context.Background(), scope, []byte("traversal-attempt"), artifacts.PutOpts{
		Namespace: "ns",
		Filename:  "../../../etc/passwd",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	// Filename is preserved as metadata.
	if ref.Filename != "../../../etc/passwd" {
		t.Errorf("Filename mutated: %q", ref.Filename)
	}

	// But the on-disk path is rooted under the FSRoot — no escape.
	canonRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	walkErr := filepath.Walk(canonRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		// Every file MUST be under canonRoot.
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(abs, canonRoot+string(filepath.Separator)) && abs != canonRoot {
			t.Errorf("file escaped FSRoot: %q (root=%q)", abs, canonRoot)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	// /etc/passwd was NOT touched (sanity — we obviously can't write
	// to it from a test, but check that no file landed there in the
	// tmp test root either).
	naive := filepath.Join(root, "etc", "passwd")
	if _, statErr := os.Stat(naive); statErr == nil {
		t.Errorf("filename traversal landed at %q (LEAK)", naive)
	}
}

// TestPathSafety_ScopeFieldsCannotEscape verifies that traversal
// components in scope identity are rejected by the path-safety guard.
// In practice, identity validators upstream prevent these from ever
// reaching the FS driver, but the driver is the last line of defense.
func TestPathSafety_ScopeFieldsCannotEscape(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	// A tenant id that, if naively joined, would climb out of root.
	scope := artifacts.ArtifactScope{
		TenantID:  "../../../escape-tenant",
		UserID:    "U",
		SessionID: "S",
		TaskID:    "K",
	}
	_, err = s.PutBytes(context.Background(), scope, []byte("escape"), artifacts.PutOpts{Namespace: "ns"})
	if err == nil {
		t.Fatal("PutBytes accepted scope with traversal in TenantID; expected rejection")
	}
}
