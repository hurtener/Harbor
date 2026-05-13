package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

// goldenScope returns a deterministic ArtifactScope for the golden
// fixtures. The TaskID is derived from the fixture name so each
// fixture has its own scope partition.
func goldenScope(name string) artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  "tenant-golden",
		UserID:    "user-golden",
		SessionID: "session-golden",
		TaskID:    "import-" + name,
	}
}

// newImporter builds an Importer wired to an in-memory ArtifactStore
// for tests. The store is returned so the caller can inspect uploads.
func newImporter(t *testing.T) (importer.Importer, artifacts.ArtifactStore) {
	t.Helper()
	store, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	imp, err := importer.New(importer.Deps{Store: store})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	t.Cleanup(func() {
		_ = imp.Close(context.Background())
		_ = store.Close(context.Background())
	})
	return imp, store
}

// readFixture reads a golden Skills.md fixture plus its .want.json.
func readFixture(t *testing.T, name string) (src []byte, want skills.Skill) {
	t.Helper()
	srcPath := filepath.Join("testdata", "golden", name+".md")
	wantPath := filepath.Join("testdata", "golden", name+".want.json")
	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	wantBytes, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("unmarshal %s: %v", wantPath, err)
	}
	return srcBytes, want
}

// goldenFixtures enumerates the golden fixtures the round-trip
// invariant tests over. Adding to this list = adding a new fixture.
var goldenFixtures = []string{
	"minimal",
	"full",
	"preconditions-only",
	"failure-modes-only",
	"with-attachments",
}

// TestImport_GoldenCorpus_FieldsMatch asserts every fixture produces
// the Skill described in its `.want.json` mirror. Attachment fixtures
// substitute the `<REF:path>` placeholder with the actual upload ID.
func TestImport_GoldenCorpus_FieldsMatch(t *testing.T) {
	for _, name := range goldenFixtures {
		t.Run(name, func(t *testing.T) {
			src, want := readFixture(t, name)
			imp, _ := newImporter(t)
			skill, imports, err := imp.Import(context.Background(), importer.ImportSource{
				Bytes:       src,
				PathHint:    name + ".md",
				AllowedRoot: filepath.Join("testdata", "golden"),
				Scope:       goldenScope(name),
			})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			// Substitute <REF:path> placeholders in the want
			// description with the actual upload ID(s).
			wantDescription := substituteWantRefs(t, want.Description, imports)
			// ContentHash is recomputed at Import; want.json doesn't
			// carry it.
			if got, want := skill.Name, want.Name; got != want {
				t.Errorf("Name = %q, want %q", got, want)
			}
			if got, want := skill.Title, want.Title; got != want {
				t.Errorf("Title = %q, want %q", got, want)
			}
			if got := skill.Description; got != wantDescription {
				t.Errorf("Description mismatch\n got:  %q\n want: %q", got, wantDescription)
			}
			if got, want := skill.Trigger, want.Trigger; got != want {
				t.Errorf("Trigger = %q, want %q", got, want)
			}
			if got, want := skill.TaskType, want.TaskType; got != want {
				t.Errorf("TaskType = %q, want %q", got, want)
			}
			if !stringsEq(skill.Tags, want.Tags) {
				t.Errorf("Tags = %v, want %v", skill.Tags, want.Tags)
			}
			if !stringsEq(skill.Steps, want.Steps) {
				t.Errorf("Steps = %v, want %v", skill.Steps, want.Steps)
			}
			if !stringsEq(skill.Preconditions, want.Preconditions) {
				t.Errorf("Preconditions = %v, want %v", skill.Preconditions, want.Preconditions)
			}
			if !stringsEq(skill.FailureModes, want.FailureModes) {
				t.Errorf("FailureModes = %v, want %v", skill.FailureModes, want.FailureModes)
			}
			if !stringsEq(skill.RequiredTools, want.RequiredTools) {
				t.Errorf("RequiredTools = %v, want %v", skill.RequiredTools, want.RequiredTools)
			}
			if !stringsEq(skill.RequiredNS, want.RequiredNS) {
				t.Errorf("RequiredNS = %v, want %v", skill.RequiredNS, want.RequiredNS)
			}
			if !stringsEq(skill.RequiredTags, want.RequiredTags) {
				t.Errorf("RequiredTags = %v, want %v", skill.RequiredTags, want.RequiredTags)
			}
			if got, want := skill.Origin, want.Origin; got != want {
				t.Errorf("Origin = %q, want %q", got, want)
			}
			if got, want := skill.Scope, want.Scope; got != want {
				t.Errorf("Scope = %q, want %q", got, want)
			}
			if skill.ContentHash == "" {
				t.Errorf("ContentHash empty (importer must stamp at Import)")
			}
		})
	}
}

// TestRoundTrip_GoldenCorpus is the GATE — byte-stable round-trip
// over every golden fixture. Failure prints the diff.
func TestRoundTrip_GoldenCorpus(t *testing.T) {
	for _, name := range goldenFixtures {
		t.Run(name, func(t *testing.T) {
			src, _ := readFixture(t, name)
			imp, _ := newImporter(t)
			skill, imports, err := imp.Import(context.Background(), importer.ImportSource{
				Bytes:       src,
				PathHint:    name + ".md",
				AllowedRoot: filepath.Join("testdata", "golden"),
				Scope:       goldenScope(name),
			})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			exported, err := imp.Export(context.Background(), skill, imports)
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if !bytes.Equal(src, exported) {
				t.Errorf("round-trip drift on %s\n--- want (src) ---\n%s\n--- got (exported) ---\n%s",
					name, hexDiff(src, exported), exported)
			}
		})
	}
}

// TestImport_AttachmentsUploadedToStore asserts the with-attachments
// fixture uploads each inline reference through the ArtifactStore
// and surfaces the mapping in ImportArtifacts.
func TestImport_AttachmentsUploadedToStore(t *testing.T) {
	src, _ := readFixture(t, "with-attachments")
	imp, store := newImporter(t)
	skill, imports, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:       src,
		PathHint:    "with-attachments.md",
		AllowedRoot: filepath.Join("testdata", "golden"),
		Scope:       goldenScope("with-attachments"),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imports.PathToRef) != 1 {
		t.Fatalf("expected 1 attachment mapping, got %d", len(imports.PathToRef))
	}
	mapping := imports.PathToRef[0]
	if mapping.Path != "attachments/example.txt" {
		t.Errorf("Path = %q, want %q", mapping.Path, "attachments/example.txt")
	}
	// Verify the bytes landed in the store.
	got, ok, err := store.Get(context.Background(), goldenScope("with-attachments"), mapping.Ref.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok {
		t.Fatalf("attachment %q not found in store", mapping.Ref.ID)
	}
	wantBytes, err := os.ReadFile(filepath.Join("testdata", "golden", "attachments", "example.txt"))
	if err != nil {
		t.Fatalf("read fixture attachment: %v", err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Errorf("attachment bytes mismatch")
	}
	// Verify the body description carries the artifact:// URI.
	if !strings.Contains(skill.Description, "artifact://"+mapping.Ref.ID) {
		t.Errorf("Description missing artifact:// substitution\ngot: %q", skill.Description)
	}
}

// TestExport_NoExtraSkill asserts Export against a Skill that did
// NOT pass through Import (no _importer.frontmatter_raw) still
// emits a deterministic valid Skills.md.
func TestExport_NoExtraSkill(t *testing.T) {
	imp, _ := newImporter(t)
	skill := skills.Skill{
		Name:    "synth-skill",
		Trigger: "when the skill is synthesised",
		Steps:   []string{"step one"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}
	exported, err := imp.Export(context.Background(), skill, importer.ImportArtifacts{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Round-trip the synthesised form through Import to verify the
	// shape is self-consistent.
	skill2, _, err := imp.Import(context.Background(), importer.ImportSource{
		Bytes:    exported,
		PathHint: "synth.md",
	})
	if err != nil {
		t.Fatalf("Re-import: %v\n--- exported ---\n%s", err, exported)
	}
	if skill2.Name != skill.Name {
		t.Errorf("Re-imported Name = %q, want %q", skill2.Name, skill.Name)
	}
	if skill2.Trigger != skill.Trigger {
		t.Errorf("Re-imported Trigger = %q, want %q", skill2.Trigger, skill.Trigger)
	}
	if !stringsEq(skill2.Steps, skill.Steps) {
		t.Errorf("Re-imported Steps = %v, want %v", skill2.Steps, skill.Steps)
	}
}

// TestExport_ReturnsErrOnUnknownAttachmentRef asserts Export errors
// fail-loud when a body carries an artifact:// ID without a
// corresponding mapping.
func TestExport_ReturnsErrOnUnknownAttachmentRef(t *testing.T) {
	imp, _ := newImporter(t)
	skill := skills.Skill{
		Name:        "with-bad-ref",
		Trigger:     "when the body carries a dangling ref",
		Description: "![alt](artifact://nonexistent_abcdef012345)",
		Steps:       []string{"step"},
		Origin:      skills.OriginPack,
		Scope:       skills.ScopeProject,
		Extra: map[string]any{
			"_importer.frontmatter_raw": "name: with-bad-ref\ntrigger: when the body carries a dangling ref\n",
		},
	}
	_, err := imp.Export(context.Background(), skill, importer.ImportArtifacts{})
	if !errors.Is(err, importer.ErrInvalidAttachmentRef) {
		t.Errorf("Export err = %v, want ErrInvalidAttachmentRef", err)
	}
}

// TestImport_AfterClose returns ErrImporterClosed.
func TestImport_AfterClose(t *testing.T) {
	imp, _ := newImporter(t)
	if err := imp.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, err := imp.Import(context.Background(), importer.ImportSource{Bytes: []byte("---\nname: x\ntrigger: y\n---\n## Steps\n\n- a\n")})
	if !errors.Is(err, importer.ErrImporterClosed) {
		t.Errorf("Import after Close: err = %v, want ErrImporterClosed", err)
	}
}

// TestNew_NilStore_Fails ensures the constructor rejects a nil store.
func TestNew_NilStore_Fails(t *testing.T) {
	_, err := importer.New(importer.Deps{Store: nil})
	if err == nil {
		t.Errorf("New(nil store) = nil; want non-nil error")
	}
}

// substituteWantRefs walks the want.Description and replaces each
// `<REF:path>` placeholder with the actual ArtifactRef.ID from
// imports. Unknown placeholders fail the test.
func substituteWantRefs(t *testing.T, desc string, imports importer.ImportArtifacts) string {
	t.Helper()
	pathToID := make(map[string]string, len(imports.PathToRef))
	for _, m := range imports.PathToRef {
		pathToID[m.Path] = m.Ref.ID
	}
	out := desc
	for path, id := range pathToID {
		placeholder := fmt.Sprintf("<REF:%s>", path)
		out = strings.ReplaceAll(out, placeholder, id)
	}
	if strings.Contains(out, "<REF:") {
		t.Fatalf("unresolved <REF:...> placeholder remains in want description: %q", out)
	}
	return out
}

// stringsEq compares two []string for ordered equality. nil and
// empty are equivalent (the JSON decoder produces nil for absent
// arrays; the importer produces nil for absent sections).
func stringsEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hexDiff renders a side-by-side hex preview of two byte slices.
// Used in round-trip failure messages.
func hexDiff(a, b []byte) string {
	var sb strings.Builder
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var ac, bc string
		if i < len(a) {
			ac = fmt.Sprintf("%02x", a[i])
		} else {
			ac = "--"
		}
		if i < len(b) {
			bc = fmt.Sprintf("%02x", b[i])
		} else {
			bc = "--"
		}
		mark := " "
		if ac != bc {
			mark = "*"
		}
		fmt.Fprintf(&sb, "%04d %s %s %s\n", i, mark, ac, bc)
	}
	return sb.String()
}

// dedupSorted is used in tests that compare unordered string lists.
func dedupSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ensure dedupSorted compiles (referenced in concurrent_test).
var _ = dedupSorted
