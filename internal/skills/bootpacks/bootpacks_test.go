package bootpacks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

// newTestParser builds the REAL pure single-document package ingest —
// the importer wired to an in-memory artifact store — so the loader
// tests exercise the actual HA-61 parser, never a fake.
func newTestParser(t *testing.T) Parser {
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
	return imp
}

// staticCatalog is a fake ToolCatalog: a fixed name -> compatible map.
type staticCatalog map[string]bool

func (c staticCatalog) Compatible(tool string) bool { return c[tool] }

// allToolsCatalog answers true for every tool (the "everything is in
// the static catalog" fixture).
var allToolsCatalog = staticCatalog{"run_shell": true, "git": true}

// testDeps builds a Deps with the real parser and the given catalog.
func testDeps(t *testing.T, catalog ToolCatalog) Deps {
	t.Helper()
	if catalog == nil {
		catalog = allToolsCatalog
	}
	return Deps{Parser: newTestParser(t), Catalog: catalog}
}

// validSkillMD returns a valid resource-free SKILL.md document.
func validSkillMD(name string) string {
	return fmt.Sprintf(`---
name: %s
title: %s title
trigger: when asked about %s
task_type: domain
tags: [ops, boot]
required_tools: [run_shell]
required_namespaces: [shell]
---
Boot skill for %s.

## Steps
- do the thing
- verify the thing

## Preconditions
- the system is booted

## Failure modes
- the thing fails
`, name, name, name, name)
}

// writePackDir writes one include package directory under root and
// returns its path.
func writePackDir(t *testing.T, root, include string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, include)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	return dir
}

// declaration builds one config.BootAgentPackConfig over a temp root.
func declaration(t *testing.T, tenant, agent, directory string, include ...string) config.BootAgentPackConfig {
	t.Helper()
	return config.BootAgentPackConfig{
		TenantID:  tenant,
		AgentID:   agent,
		Directory: directory,
		Include:   include,
	}
}

// requireLoad loads a declaration list and fails the test on error.
func requireLoad(t *testing.T, deps Deps, packs ...config.BootAgentPackConfig) *Index {
	t.Helper()
	ix, err := New(context.Background(), packs, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ix
}

// TestNew_ValidMultiKeyIndex pins the happy path: two tenants, one
// with two includes, load eagerly into an index keyed by the exact
// (tenant, agent) pair.
func TestNew_ValidMultiKeyIndex(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "workbench-foundation", map[string]string{
		"SKILL.md": validSkillMD("workbench-foundation"),
	})
	writePackDir(t, root, "deploy-runbook", map[string]string{
		"SKILL.md": validSkillMD("deploy-runbook"),
	})
	writePackDir(t, root, "other-foundation", map[string]string{
		"SKILL.md": validSkillMD("other-foundation"),
	})

	deps := testDeps(t, nil)
	ix := requireLoad(t, deps,
		declaration(t, "acme", "harbor-dev-agent", root, "workbench-foundation", "deploy-runbook"),
		declaration(t, "globex", "harbor-dev-agent", root, "other-foundation"),
	)

	wantKeys := []Key{{TenantID: "acme", AgentID: "harbor-dev-agent"}, {TenantID: "globex", AgentID: "harbor-dev-agent"}}
	gotKeys := ix.Keys()
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("Keys() = %v, want %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("Keys()[%d] = %v, want %v", i, gotKeys[i], wantKeys[i])
		}
	}

	acme, ok := ix.Lookup("acme", "harbor-dev-agent")
	if !ok || len(acme) != 2 {
		t.Fatalf("Lookup(acme) = %d entries, ok=%v; want 2 entries", len(acme), ok)
	}
	// Deterministic canonical-name order.
	if acme[0].Skill.Name != "deploy-runbook" || acme[1].Skill.Name != "workbench-foundation" {
		t.Fatalf("acme entries not in canonical-name order: %+v", acme)
	}
	for _, e := range acme {
		if e.PackageHash == "" || e.SemanticHash == "" || e.Source != filepath.Join(root, e.Skill.Name) {
			t.Fatalf("entry incomplete: %+v", e)
		}
		if e.Skill.Origin != skills.OriginPack || e.Skill.Scope != skills.ScopeProject {
			t.Fatalf("entry envelope not the parser-fixed pack/project form: %+v", e.Skill)
		}
	}

	globex, ok := ix.Lookup("globex", "harbor-dev-agent")
	if !ok || len(globex) != 1 || globex[0].Skill.Name != "other-foundation" {
		t.Fatalf("Lookup(globex) = %v, ok=%v", globex, ok)
	}

	// Exact-key binding: a different tenant or agent never composes.
	if _, ok := ix.Lookup("acme", "some-other-agent"); ok {
		t.Fatal("Lookup with a different agent composed the boot pack")
	}
	if _, ok := ix.Lookup("other-tenant", "harbor-dev-agent"); ok {
		t.Fatal("Lookup with a different tenant composed the boot pack")
	}
}

// TestNew_EmptyConfig pins the config-removal representation: an
// absent boot_agent_packs block yields an empty index — no keys, no
// lookups, no set hashes, and no tombstone.
func TestNew_EmptyConfig(t *testing.T) {
	deps := testDeps(t, nil)
	ix := requireLoad(t, deps)
	if got := ix.Keys(); len(got) != 0 {
		t.Fatalf("Keys() = %v, want none", got)
	}
	if _, ok := ix.Lookup("acme", "agent"); ok {
		t.Fatal("Lookup on an empty index must be absent")
	}
	if ix.OwnsName("acme", "agent", "anything") {
		t.Fatal("OwnsName on an empty index must be false")
	}
	if _, ok := ix.BootPackSetHash("acme", "agent"); ok {
		t.Fatal("BootPackSetHash on an empty index must be absent")
	}
}

// TestNew_RestartDeterministic pins the restart contract: loading the
// same directory tree through two independent New calls yields
// byte-identical entries and identical hashes.
func TestNew_RestartDeterministic(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "workbench-foundation", map[string]string{
		"SKILL.md": validSkillMD("workbench-foundation"),
	})
	writePackDir(t, root, "deploy-runbook", map[string]string{
		"SKILL.md": validSkillMD("deploy-runbook"),
	})
	packs := []config.BootAgentPackConfig{
		declaration(t, "acme", "harbor-dev-agent", root, "workbench-foundation", "deploy-runbook"),
	}

	a := requireLoad(t, testDeps(t, nil), packs...)
	b := requireLoad(t, testDeps(t, nil), packs...)

	hashA, _ := a.BootPackSetHash("acme", "harbor-dev-agent")
	hashB, _ := b.BootPackSetHash("acme", "harbor-dev-agent")
	if hashA != hashB {
		t.Fatalf("set hash not stable across restarts: %q vs %q", hashA, hashB)
	}
	entriesA, _ := a.Lookup("acme", "harbor-dev-agent")
	entriesB, _ := b.Lookup("acme", "harbor-dev-agent")
	if len(entriesA) != len(entriesB) {
		t.Fatalf("entry count differs across restarts: %d vs %d", len(entriesA), len(entriesB))
	}
	for i := range entriesA {
		if entriesA[i].PackageHash != entriesB[i].PackageHash ||
			entriesA[i].SemanticHash != entriesB[i].SemanticHash ||
			entriesA[i].Skill.Name != entriesB[i].Skill.Name {
			t.Fatalf("entry %d differs across restarts: %+v vs %+v", i, entriesA[i], entriesB[i])
		}
	}
}
