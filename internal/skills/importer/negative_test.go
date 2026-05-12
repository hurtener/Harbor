package importer_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

func TestNegative_EmptySource(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes: []byte{},
	})
	if !errors.Is(err, importer.ErrMissingFrontmatter) {
		t.Errorf("err = %v, want ErrMissingFrontmatter", err)
	}
}

func TestNegative_NoOpeningFence(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes: []byte("no frontmatter\n\n## Steps\n\n- step\n"),
	})
	if !errors.Is(err, importer.ErrMissingFrontmatter) {
		t.Errorf("err = %v, want ErrMissingFrontmatter", err)
	}
}

func TestNegative_NoClosingFence(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes: []byte("---\nname: x\ntrigger: y\n\n## Steps\n\n- step\n"),
	})
	if !errors.Is(err, importer.ErrMalformedYAML) {
		t.Errorf("err = %v, want ErrMalformedYAML", err)
	}
}

func TestNegative_MissingTrigger(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    []byte("---\nname: no-trigger\n---\n\n## Steps\n\n- step\n"),
		PathHint: "no-trigger.md",
	})
	if !errors.Is(err, importer.ErrMissingTrigger) {
		t.Errorf("err = %v, want ErrMissingTrigger", err)
	}
	if !errors.Is(err, skills.ErrInvalidSkill) {
		t.Errorf("err should wrap skills.ErrInvalidSkill, got %v", err)
	}
}

func TestNegative_EmptySteps(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    []byte("---\nname: no-steps\ntrigger: yes\n---\n\nbody without steps\n"),
		PathHint: "no-steps.md",
	})
	if !errors.Is(err, importer.ErrEmptySteps) {
		t.Errorf("err = %v, want ErrEmptySteps", err)
	}
	if !errors.Is(err, skills.ErrInvalidSkill) {
		t.Errorf("err should wrap skills.ErrInvalidSkill, got %v", err)
	}
}

func TestNegative_StepsHeaderWithoutItems(t *testing.T) {
	imp, _ := newImporter(t)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    []byte("---\nname: bare-steps\ntrigger: yes\n---\n\n## Steps\n\n"),
		PathHint: "bare.md",
	})
	if !errors.Is(err, importer.ErrEmptySteps) {
		t.Errorf("err = %v, want ErrEmptySteps", err)
	}
}

func TestNegative_MalformedYAML(t *testing.T) {
	imp, _ := newImporter(t)
	src := []byte("---\nname: : :\ntrigger: ?\n---\n\n## Steps\n\n- s\n")
	_, _, err := imp.Import(context.Background(), importer.ImportSource{Bytes: src})
	if !errors.Is(err, importer.ErrMalformedYAML) {
		t.Errorf("err = %v, want ErrMalformedYAML", err)
	}
}

func TestNegative_UnknownSection(t *testing.T) {
	imp, _ := newImporter(t)
	src := []byte(`---
name: unknown-section
trigger: yes
---

desc

## Steps

- ok

## Examples

- not allowed
`)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes: src, PathHint: "unknown.md",
	})
	if !errors.Is(err, importer.ErrUnknownSection) {
		t.Errorf("err = %v, want ErrUnknownSection", err)
	}
}

func TestNegative_AttachmentPathOutsideAllowedRoot(t *testing.T) {
	imp, _ := newImporter(t)
	src := []byte(`---
name: escape
trigger: yes
---

![alt](../escape.txt)

## Steps

- s
`)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "escape.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("escape"),
	})
	if !errors.Is(err, importer.ErrAttachmentOutsideRoot) {
		t.Errorf("err = %v, want ErrAttachmentOutsideRoot", err)
	}
}

func TestNegative_AttachmentMissingFile(t *testing.T) {
	imp, _ := newImporter(t)
	src := []byte(`---
name: missing
trigger: yes
---

![alt](attachments/nonexistent.txt)

## Steps

- s
`)
	_, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "missing.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("missing"),
	})
	if err == nil {
		t.Errorf("expected error for missing attachment file")
	}
}

func TestNegative_NameFallback_FromPathHint(t *testing.T) {
	// Skill without `name` in frontmatter; the importer derives it
	// from PathHint via slugify.
	imp, _ := newImporter(t)
	src := []byte("---\ntrigger: yes\n---\n\n## Steps\n\n- s\n")
	skill, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    src,
		PathHint: "skills/My Cool Skill.md",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if skill.Name != "my-cool-skill" {
		t.Errorf("Name = %q, want %q", skill.Name, "my-cool-skill")
	}
}
