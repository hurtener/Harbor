package bootpacks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

// TestIndex_APISurface pins the full lookup contract: deep-copy
// immutable entries, OwnsName canonicalization, deterministic ordered
// keys, and the boot-pack set hash.
func TestIndex_APISurface(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "workbench-foundation", map[string]string{
		"SKILL.md": validSkillMD("workbench-foundation"),
	})
	writePackDir(t, root, "deploy-runbook", map[string]string{
		"SKILL.md": validSkillMD("deploy-runbook"),
	})
	ix := requireLoad(t, testDeps(t, nil),
		declaration(t, "acme", "harbor-dev-agent", root, "workbench-foundation", "deploy-runbook"),
	)

	// OwnsName: canonical and case-variant input both resolve; unknown
	// names and unknown keys do not.
	if !ix.OwnsName("acme", "harbor-dev-agent", "workbench-foundation") {
		t.Fatal("OwnsName(matching) = false")
	}
	if !ix.OwnsName("acme", "harbor-dev-agent", "Workbench-Foundation  ") {
		t.Fatal("OwnsName(case-variant) = false")
	}
	if ix.OwnsName("acme", "harbor-dev-agent", "not-declared") {
		t.Fatal("OwnsName(unknown name) = true")
	}
	if ix.OwnsName("acme", "other-agent", "workbench-foundation") {
		t.Fatal("OwnsName(other agent) = true")
	}
	if ix.OwnsName("other-tenant", "harbor-dev-agent", "workbench-foundation") {
		t.Fatal("OwnsName(other tenant) = true")
	}

	// BootPackSetHash: present for the loaded key, absent otherwise,
	// and stable across calls.
	hash, ok := ix.BootPackSetHash("acme", "harbor-dev-agent")
	if !ok || hash == "" {
		t.Fatalf("BootPackSetHash = %q, ok=%v", hash, ok)
	}
	if again, _ := ix.BootPackSetHash("acme", "harbor-dev-agent"); again != hash {
		t.Fatalf("BootPackSetHash not stable: %q vs %q", again, hash)
	}
	if _, ok := ix.BootPackSetHash("acme", "nobody"); ok {
		t.Fatal("BootPackSetHash for unknown key = present")
	}

	// Keys(): deterministic, sorted, fresh copy.
	keys := ix.Keys()
	if len(keys) != 1 || keys[0] != (Key{TenantID: "acme", AgentID: "harbor-dev-agent"}) {
		t.Fatalf("Keys() = %v", keys)
	}
	keys[0] = Key{TenantID: "mutated", AgentID: "x"}
	if ix.Keys()[0] != (Key{TenantID: "acme", AgentID: "harbor-dev-agent"}) {
		t.Fatal("mutating the returned Keys slice leaked into the index")
	}
}

// TestLookup_DeepCopyImmutability pins the deep-copy contract: a
// caller that mutates the returned entries — slices, maps, or the
// struct itself — never perturbs the frozen index.
func TestLookup_DeepCopyImmutability(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "pkg", map[string]string{
		"SKILL.md": validSkillMD("pkg"),
	})
	ix := requireLoad(t, testDeps(t, nil),
		declaration(t, "acme", "agent", root, "pkg"),
	)

	first, ok := ix.Lookup("acme", "agent")
	if !ok || len(first) != 1 {
		t.Fatalf("Lookup = %v, ok=%v", first, ok)
	}
	origHash := first[0].PackageHash

	// Mutate every mutable surface of the returned copy.
	first[0].Skill.Name = "mutated-name"
	first[0].Skill.Tags = append(first[0].Skill.Tags, "mutated")
	first[0].Skill.Steps[0] = "mutated-step"
	first[0].Skill.Extra = map[string]any{"k": "v"}
	first[0].PackageHash = "v1:mutated"
	first[0].SemanticHash = "mutated"
	first[0].Source = "/mutated"

	again, ok := ix.Lookup("acme", "agent")
	if !ok || len(again) != 1 {
		t.Fatalf("second Lookup = %v, ok=%v", again, ok)
	}
	if again[0].Skill.Name != "pkg" {
		t.Fatalf("name leaked: %q", again[0].Skill.Name)
	}
	if len(again[0].Skill.Tags) != 2 || again[0].Skill.Tags[0] != "ops" || again[0].Skill.Tags[1] != "boot" {
		t.Fatalf("tags leaked: %v", again[0].Skill.Tags)
	}
	if again[0].Skill.Steps[0] != "do the thing" {
		t.Fatalf("steps leaked: %v", again[0].Skill.Steps)
	}
	if again[0].Skill.Extra != nil {
		t.Fatalf("extra leaked: %v", again[0].Skill.Extra)
	}
	if again[0].PackageHash != origHash {
		t.Fatalf("package hash leaked: %q, want %q", again[0].PackageHash, origHash)
	}
	if again[0].SemanticHash == "mutated" || again[0].Source == "/mutated" {
		t.Fatalf("scalar fields leaked: %+v", again[0])
	}
	// Two lookups must never share a backing array either.
	a, _ := ix.Lookup("acme", "agent")
	b, _ := ix.Lookup("acme", "agent")
	if reflect.ValueOf(a[0].Skill.Steps).Pointer() == reflect.ValueOf(b[0].Skill.Steps).Pointer() {
		t.Fatal("two lookups share the same steps backing array")
	}
}

// TestIndex_NoFilesystemObservation pins the frozen contract: after
// New returns, deleting and rewriting the source files never changes
// what the index serves.
func TestIndex_NoFilesystemObservation(t *testing.T) {
	root := t.TempDir()
	dir := writePackDir(t, root, "pkg", map[string]string{
		"SKILL.md": validSkillMD("pkg"),
	})
	ix := requireLoad(t, testDeps(t, nil),
		declaration(t, "acme", "agent", root, "pkg"),
	)
	wantHash, _ := ix.BootPackSetHash("acme", "agent")
	wantEntries, _ := ix.Lookup("acme", "agent")

	// Destroy and rewrite the source tree with different content.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	writePackDir(t, root, "pkg", map[string]string{
		"SKILL.md": validSkillMD("completely-different"),
	})

	gotHash, ok := ix.BootPackSetHash("acme", "agent")
	if !ok || gotHash != wantHash {
		t.Fatalf("set hash changed after filesystem mutation: %q vs %q", gotHash, wantHash)
	}
	got, ok := ix.Lookup("acme", "agent")
	if !ok || len(got) != len(wantEntries) || got[0].Skill.Name != wantEntries[0].Skill.Name {
		t.Fatalf("lookup changed after filesystem mutation: %+v", got)
	}
	if got[0].PackageHash != wantEntries[0].PackageHash {
		t.Fatalf("package hash changed after filesystem mutation: %q vs %q", got[0].PackageHash, wantEntries[0].PackageHash)
	}
}

// TestLookup_SkillDeepCopy pins the Skill-level copy: every slice
// field is independent of the frozen entry.
func TestLookup_SkillDeepCopy(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "pkg", map[string]string{
		"SKILL.md": validSkillMD("pkg"),
	})
	ix := requireLoad(t, testDeps(t, nil),
		declaration(t, "acme", "agent", root, "pkg"),
	)
	got, ok := ix.Lookup("acme", "agent")
	if !ok || len(got) != 1 {
		t.Fatalf("Lookup = %v, ok=%v", got, ok)
	}
	e := got[0]
	want := []string{"ops", "boot"}
	if !reflect.DeepEqual(e.Skill.Tags, want) {
		t.Fatalf("tags = %v, want %v", e.Skill.Tags, want)
	}
	if !reflect.DeepEqual(e.Skill.RequiredTools, []string{"run_shell"}) {
		t.Fatalf("required tools = %v", e.Skill.RequiredTools)
	}
	if !reflect.DeepEqual(e.Skill.RequiredNS, []string{"shell"}) {
		t.Fatalf("required namespaces = %v", e.Skill.RequiredNS)
	}
	if e.Skill.Origin != skills.OriginPack || e.Skill.Scope != skills.ScopeProject {
		t.Fatalf("envelope = origin %q scope %q", e.Skill.Origin, e.Skill.Scope)
	}
	if e.Skill.ContentHash == "" {
		t.Fatal("ContentHash empty")
	}
	if e.PackageHash == "" || e.SemanticHash == "" || e.SemanticHash != e.Skill.ContentHash {
		t.Fatalf("hash fields inconsistent: %+v", e)
	}
	if e.Source != filepath.Join(root, "pkg") {
		t.Fatalf("source = %q, want %q", e.Source, filepath.Join(root, "pkg"))
	}
}
