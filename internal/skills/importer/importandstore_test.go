package importer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

const importStoreFixture = `---
name: triage-incident
title: Triage an incident
trigger: when a support ticket arrives
scope: project
---
Classify a ticket and recommend the next action.

## Steps

- Read the user's report.
- Match against known categories.
`

func importStoreTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func importStoreTestStore(t *testing.T, bus events.EventBus) skills.SkillStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "skills.sqlite")
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func importStoreTestDeps(t *testing.T) importer.Deps {
	t.Helper()
	artStore, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })
	return importer.Deps{Store: artStore}
}

func writeFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "triage-incident.skill.md")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

var importStoreIdentity = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// TestImportAndStore_HappyPath_StoresPackSkill — the one-call ingest
// path lands an Origin=pack skill in the store and reports honestly.
func TestImportAndStore_HappyPath_StoresPackSkill(t *testing.T) {
	t.Parallel()
	bus := importStoreTestBus(t)
	store := importStoreTestStore(t, bus)
	path := writeFixture(t, importStoreFixture)

	rep, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, importStoreTestDeps(t), path)
	if err != nil {
		t.Fatalf("ImportAndStore: %v", err)
	}
	if rep.Name != "triage-incident" || rep.Steps != 2 || rep.Overwrote {
		t.Errorf("report = %+v, want Name=triage-incident Steps=2 Overwrote=false", rep)
	}
	if rep.Scope != skills.ScopeProject {
		t.Errorf("Scope = %q, want %q", rep.Scope, skills.ScopeProject)
	}
	if rep.ContentHash == "" {
		t.Error("ContentHash empty — the canonical hash must be reported")
	}

	got, err := store.Get(context.Background(), identity.Quadruple{Identity: importStoreIdentity}, "triage-incident")
	if err != nil {
		t.Fatalf("store.Get after import: %v", err)
	}
	if got.Origin != skills.OriginPack {
		t.Errorf("stored Origin = %q, want %q", got.Origin, skills.OriginPack)
	}
}

// TestImportAndStore_DuplicateName_RejectsLoud pins the default
// conflict stance: a second import of the same name fails with
// ErrDuplicateSkillName; the stored row is untouched.
func TestImportAndStore_DuplicateName_RejectsLoud(t *testing.T) {
	t.Parallel()
	bus := importStoreTestBus(t)
	store := importStoreTestStore(t, bus)
	deps := importStoreTestDeps(t)
	path := writeFixture(t, importStoreFixture)

	if _, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, deps, path); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, deps, path)
	if !errors.Is(err, importer.ErrDuplicateSkillName) {
		t.Fatalf("second import err = %v, want ErrDuplicateSkillName", err)
	}
}

// TestImportAndStore_Overwrite_ConflictMatrix pins WithOverwrite
// against the two existing-origin shapes the plan's matrix names:
// pack→pack replaces; generated→pack replaces (the store's policy
// only protects PACK rows from NON-pack input — an explicit operator
// re-import with overwrite wins over a generated row).
func TestImportAndStore_Overwrite_ConflictMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := identity.Quadruple{Identity: importStoreIdentity}

	t.Run("pack_over_pack", func(t *testing.T) {
		t.Parallel()
		bus := importStoreTestBus(t)
		store := importStoreTestStore(t, bus)
		deps := importStoreTestDeps(t)
		path := writeFixture(t, importStoreFixture)
		if _, err := importer.ImportAndStore(ctx, importStoreIdentity, store, deps, path); err != nil {
			t.Fatalf("first import: %v", err)
		}
		rep, err := importer.ImportAndStore(ctx, importStoreIdentity, store, deps, path, importer.WithOverwrite())
		if err != nil {
			t.Fatalf("overwrite import: %v", err)
		}
		if !rep.Overwrote {
			t.Error("report.Overwrote = false, want true")
		}
	})

	t.Run("pack_over_generated", func(t *testing.T) {
		t.Parallel()
		bus := importStoreTestBus(t)
		store := importStoreTestStore(t, bus)
		deps := importStoreTestDeps(t)
		// Seed a generated row under the same name.
		if err := store.Upsert(ctx, q, skills.Skill{
			Name:    "triage-incident",
			Trigger: "generated trigger",
			Steps:   []string{"generated step"},
			Origin:  skills.OriginGenerated,
			Scope:   skills.ScopeProject,
		}); err != nil {
			t.Fatalf("seed generated row: %v", err)
		}
		path := writeFixture(t, importStoreFixture)

		// Without overwrite: duplicate-name rejection (loud).
		if _, err := importer.ImportAndStore(ctx, importStoreIdentity, store, deps, path); !errors.Is(err, importer.ErrDuplicateSkillName) {
			t.Fatalf("err = %v, want ErrDuplicateSkillName", err)
		}
		// With overwrite: the pack import replaces the generated row.
		rep, err := importer.ImportAndStore(ctx, importStoreIdentity, store, deps, path, importer.WithOverwrite())
		if err != nil {
			t.Fatalf("overwrite import: %v", err)
		}
		if !rep.Overwrote {
			t.Error("report.Overwrote = false, want true")
		}
		got, err := store.Get(ctx, q, "triage-incident")
		if err != nil {
			t.Fatalf("store.Get: %v", err)
		}
		if got.Origin != skills.OriginPack {
			t.Errorf("post-overwrite Origin = %q, want pack", got.Origin)
		}
	})
}

// TestImportAndStore_InvalidFrontmatter_FailsLoud — the §17.3 failure
// mode: a frontmatter-less file surfaces the parser's own sentinel.
func TestImportAndStore_InvalidFrontmatter_FailsLoud(t *testing.T) {
	t.Parallel()
	bus := importStoreTestBus(t)
	store := importStoreTestStore(t, bus)
	path := writeFixture(t, "no frontmatter here\n\n## Steps\n\n- one\n")

	_, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, importStoreTestDeps(t), path)
	if !errors.Is(err, importer.ErrMissingFrontmatter) {
		t.Fatalf("err = %v, want ErrMissingFrontmatter", err)
	}
}

// TestImportAndStore_MissingFile_FailsLoud — a nonexistent path is a
// wrapped read error, never a silent no-op.
func TestImportAndStore_MissingFile_FailsLoud(t *testing.T) {
	t.Parallel()
	bus := importStoreTestBus(t)
	store := importStoreTestStore(t, bus)
	_, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, importStoreTestDeps(t),
		filepath.Join(t.TempDir(), "missing.skill.md"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// TestImportAndStore_IdentityScoped — a skill imported under tenant A
// is invisible to tenant B (§6 rule 10 read at the ingest seam).
func TestImportAndStore_IdentityScoped(t *testing.T) {
	t.Parallel()
	bus := importStoreTestBus(t)
	store := importStoreTestStore(t, bus)
	path := writeFixture(t, importStoreFixture)

	if _, err := importer.ImportAndStore(context.Background(), importStoreIdentity, store, importStoreTestDeps(t), path); err != nil {
		t.Fatalf("import: %v", err)
	}
	other := identity.Quadruple{Identity: identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"}}
	if _, err := store.Get(context.Background(), other, "triage-incident"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("cross-tenant Get err = %v, want ErrSkillNotFound", err)
	}
}
