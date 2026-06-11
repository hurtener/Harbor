package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// TestRun_WritesFourDeterministicPages runs the whole command twice
// against temp dirs and asserts (a) all four pages land with the
// generated header, (b) the two runs are byte-identical — the property
// the `make protocol-docs-gen-check` diff gate depends on.
func TestRun_WritesFourDeterministicPages(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := run(dirA); err != nil {
		t.Fatalf("run(A): %v", err)
	}
	if err := run(dirB); err != nil {
		t.Fatalf("run(B): %v", err)
	}
	for _, name := range []string{"methods.md", "events.md", "errors.md", "types.md"} {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatalf("read %s (A): %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatalf("read %s (B): %v", name, err)
		}
		if !strings.HasPrefix(string(a), generatedHeader) {
			t.Errorf("%s: missing generated-file header", name)
		}
		if string(a) != string(b) {
			t.Errorf("%s: two runs produced different bytes — generation is not deterministic", name)
		}
	}
}

// TestRenderMethodsPage_CoversEveryCanonicalMethod asserts the rendered
// page names every methods.Methods() entry as a table row.
func TestRenderMethodsPage_CoversEveryCanonicalMethod(t *testing.T) {
	page, err := renderMethodsPage()
	if err != nil {
		t.Fatalf("renderMethodsPage: %v", err)
	}
	for _, m := range methods.Methods() {
		if !strings.Contains(page, "| `"+string(m)+"` |") {
			t.Errorf("methods.md is missing a row for %q", m)
		}
	}
}

// TestRenderEventsPage_CoversEveryRegisteredType asserts the rendered
// page carries one section per registered event type.
func TestRenderEventsPage_CoversEveryRegisteredType(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	for _, et := range events.EventTypes() {
		if !strings.Contains(page, "## `"+string(et)+"`") {
			t.Errorf("events.md is missing a section for %q", et)
		}
	}
}

// TestRenderErrorsPage_CoversEveryCode asserts the rendered page names
// every canonical error code as a table row.
func TestRenderErrorsPage_CoversEveryCode(t *testing.T) {
	page, err := renderErrorsPage()
	if err != nil {
		t.Fatalf("renderErrorsPage: %v", err)
	}
	for _, code := range protoerrors.Codes() {
		if !strings.Contains(page, "| `"+string(code)+"` |") {
			t.Errorf("errors.md is missing a row for %q", code)
		}
	}
}

// TestRenderTypesPage_CoversEveryCanonicalType asserts the rendered
// page carries one section per CanonicalWireTypes entry, and that the
// snake_case wire tags survive (spot-check on StartRequest).
func TestRenderTypesPage_CoversEveryCanonicalType(t *testing.T) {
	page, err := renderTypesPage()
	if err != nil {
		t.Fatalf("renderTypesPage: %v", err)
	}
	for name := range singlesource.CanonicalWireTypes {
		if !strings.Contains(page, "\n## "+name+"\n") {
			t.Errorf("types.md is missing a section for %q", name)
		}
	}
	for _, key := range []string{"`idempotency_key`", "`input_artifact_ids`", "`protocol_version`"} {
		if !strings.Contains(page, key) {
			t.Errorf("types.md: expected StartRequest/StartResponse wire key %s", key)
		}
	}
}

// TestRenderEventsPage_SafeVsRedactedClassification spot-checks the
// mechanical SafePayload classification: a SafeSealed payload renders
// as typed-verbatim, a Sealed-only payload renders as redacted.
func TestRenderEventsPage_SafeVsRedactedClassification(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	// task.spawned carries a SafeSealed payload.
	idx := strings.Index(page, "## `task.spawned`")
	if idx < 0 {
		t.Fatal("events.md missing task.spawned")
	}
	section := page[idx:min(idx+400, len(page))]
	if !strings.Contains(section, "safe payload") {
		t.Errorf("task.spawned should classify as a safe payload; section:\n%s", section)
	}
	// runtime.error carries a Sealed (redacted) payload.
	idx = strings.Index(page, "## `runtime.error`")
	if idx < 0 {
		t.Fatal("events.md missing runtime.error")
	}
	section = page[idx:min(idx+400, len(page))]
	if !strings.Contains(section, "redacted") {
		t.Errorf("runtime.error should classify as redacted; section:\n%s", section)
	}
}
