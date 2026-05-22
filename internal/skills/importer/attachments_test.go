package importer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

// TestAttachments_PathOutsideRoot_RejectsLoudly asserts the
// path-safety guard fires for an attachment referencing a sibling
// directory the operator did not declare safe.
func TestAttachments_PathOutsideRoot_RejectsLoudly(t *testing.T) {
	tmp := t.TempDir()
	allowedRoot := filepath.Join(tmp, "allowed")
	if err := os.MkdirAll(allowedRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Author a Skills.md that references a path outside the allowed root.
	src := []byte(`---
name: bad-attach
trigger: when the attachment escapes
---
![alt](../escape.txt)

## Steps

- step
`)
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "bad-attach.md",
		AllowedRoot: allowedRoot,
		Scope: artifacts.ArtifactScope{
			TenantID: "t", UserID: "u", SessionID: "s", TaskID: "import-bad",
		},
	})
	if !errors.Is(err, importer.ErrAttachmentOutsideRoot) {
		t.Errorf("err = %v, want ErrAttachmentOutsideRoot", err)
	}
}

// TestAttachments_EmptyAllowedRoot_RejectsAnyRef asserts that if the
// operator did not declare an AllowedRoot, any inline attachment
// reference is rejected (fail-closed default — CLAUDE.md §13).
func TestAttachments_EmptyAllowedRoot_RejectsAnyRef(t *testing.T) {
	src := []byte(`---
name: no-root
trigger: when no AllowedRoot is set
---
![alt](attachments/x.txt)

## Steps

- step
`)
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    src,
		PathHint: "no-root.md",
		// AllowedRoot left empty.
		Scope: artifacts.ArtifactScope{
			TenantID: "t", UserID: "u", SessionID: "s", TaskID: "import-no-root",
		},
	})
	if !errors.Is(err, importer.ErrAttachmentOutsideRoot) {
		t.Errorf("err = %v, want ErrAttachmentOutsideRoot", err)
	}
}

// TestAttachments_RoundTripAcrossClose ensures the path->ref mapping
// survives a Close + reopen cycle, because the mapping lives in the
// ImportArtifacts return value, not in the importer state.
func TestAttachments_RoundTripAcrossClose(t *testing.T) {
	src, _ := readFixture(t, "with-attachments")
	imp, _ := newImporter(t)
	skill, imports, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "with-attachments.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("with-attachments"),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// Close the first importer.
	if err = imp.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Build a new importer and Export against the SAME mapping.
	imp2, _ := newImporter(t)
	exported, err := imp2.Export(context.Background(), skill, imports)
	if err != nil {
		t.Fatalf("Export on new importer: %v", err)
	}
	if !bytes.Equal(src, exported) {
		t.Errorf("round-trip across close drifted")
	}
}

// TestAttachments_DuplicatePathRejected asserts a Skills.md that
// references the same path twice fails with ErrInvalidAttachmentRef
// at Import (preserves the Export injectivity invariant).
func TestAttachments_DuplicatePathRejected(t *testing.T) {
	src := []byte(`---
name: dup
trigger: when path duplicates
---
![one](attachments/example.txt) and ![two](attachments/example.txt)

## Steps

- step
`)
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "dup.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("dup"),
	})
	if !errors.Is(err, importer.ErrInvalidAttachmentRef) {
		t.Errorf("err = %v, want ErrInvalidAttachmentRef", err)
	}
}

// TestAttachments_URLAttachmentNotUploaded asserts http:// / https://
// / data: / artifact:// URI references are kept verbatim and NOT
// uploaded through the ArtifactStore.
func TestAttachments_URLAttachmentNotUploaded(t *testing.T) {
	src := []byte(`---
name: with-url
trigger: when external URL referenced
---
![logo](https://example.com/logo.png) and ![data](data:text/plain;base64,Zm9v)

## Steps

- step
`)
	imp, store := newImporter(t)
	_, imports, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "with-url.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("with-url"),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imports.PathToRef) != 0 {
		t.Errorf("URL refs should not be uploaded, got %d mappings", len(imports.PathToRef))
	}
	// Verify the store has zero artifacts under the scope.
	list, err := store.List(context.Background(), goldenScope("with-url"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("store has %d artifacts, want 0", len(list))
	}
}
