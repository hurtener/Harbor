package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/postgres"
)

func openPostgresStore(t *testing.T) skills.SkillStore {
	t.Helper()
	store, err := postgres.New(
		skills.ConfigSnapshot{Driver: "postgres", DSN: freshSchema(t, requireDSN(t))},
		skills.Deps{Bus: buildBus(t)},
	)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func validCoverageSkill(name string) skills.Skill {
	return skills.Skill{
		Name: name, Title: name, Description: "coverage searchable marker",
		Trigger: "when coverage is requested", Steps: []string{"verify behavior"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
	}
}

// validCoverageInstalledUnit returns a valid atomic installed unit for
// the driver-level closed/canceled sentinel cases: canonical complete
// package with one bounded support file, its versioned PackageHash, and
// a self-consistent stored semantic skill on the ScopeUser rung.
func validCoverageInstalledUnit(t *testing.T, name, agentID string) skills.InstalledPackage {
	t.Helper()
	now := time.Now().UTC()
	data := []byte(`{"file": 0, "name": "` + name + `"}`)
	pkg := skills.Package{
		Name:    name,
		Version: "1.0.0",
		Skill: skills.PackageSkill{
			Name: name, Title: "Title " + name, Trigger: "trigger:" + name,
			TaskType: "code", Tags: []string{"alpha"}, Steps: []string{"step one"},
		},
		Supports: []skills.SupportFile{{
			Path: "examples/file-00.json", Mime: "application/json",
			Size: int64(len(data)), Digest: coverageHexDigest(data), Data: data,
		}},
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		t.Fatalf("PackageHash(%q): %v", name, err)
	}
	skill := skills.Skill{
		Name: name, AgentID: agentID, Title: pkg.Skill.Title,
		Trigger: pkg.Skill.Trigger, TaskType: pkg.Skill.TaskType,
		Tags:   append([]string(nil), pkg.Skill.Tags...),
		Steps:  append([]string(nil), pkg.Skill.Steps...),
		Origin: skills.OriginGenerated, OriginRef: "coverage:" + name,
		Scope: skills.ScopeUser, CreatedAt: now, UpdatedAt: now,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

// coverageHexDigest returns the lowercase hex sha256 of b — the digest
// form the support manifest records.
func coverageHexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestNew_RejectsUnknownRetrievalModeAndUnreachableDatabase(t *testing.T) {
	bus := buildBus(t)
	if _, err := postgres.New(skills.ConfigSnapshot{
		Driver: "postgres", DSN: "postgres://unused", Retrieval: skills.RetrievalMode("future"),
	}, skills.Deps{Bus: bus}); err == nil {
		t.Fatal("unknown retrieval mode returned nil error")
	}
	if _, err := postgres.New(skills.ConfigSnapshot{
		Driver: "postgres", DSN: "postgres://127.0.0.1:1/harbor?connect_timeout=1",
	}, skills.Deps{Bus: bus}); err == nil {
		t.Fatal("unreachable database returned nil error")
	}
}

func TestClosedStore_AllPublicOperationsFailClosed(t *testing.T) {
	store := openPostgresStore(t)
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	skill := validCoverageSkill("closed")
	unit := validCoverageInstalledUnit(t, "closed-pkg", "agent-closed")
	cases := []struct {
		name string
		call func() error
	}{
		{name: "upsert", call: func() error { return store.Upsert(context.Background(), fixtureID, skill) }},
		{name: "get", call: func() error { _, err := store.Get(context.Background(), fixtureID, skill.Name); return err }},
		{name: "list", call: func() error { _, err := store.List(context.Background(), fixtureID, skills.ListFilter{}); return err }},
		{name: "search", call: func() error { _, err := store.Search(context.Background(), fixtureID, skill.Name, 1); return err }},
		{name: "delete", call: func() error { return store.Delete(context.Background(), fixtureID, skill.Name, skill.Scope) }},
		{name: "snapshot", call: func() error {
			_, err := store.(skills.SnapshotCandidateSearcher).SearchSnapshot(context.Background(), fixtureID, skill.Name, []skills.Skill{skill}, 1)
			return err
		}},
		{name: "get_installed_package", call: func() error {
			_, err := store.GetInstalledPackage(context.Background(), fixtureID, "agent-closed", "closed-pkg")
			return err
		}},
		{name: "resolve_support", call: func() error {
			uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
			if err != nil {
				return err
			}
			_, err = store.ResolveSupport(context.Background(), fixtureID, "agent-closed", "closed-pkg", uri)
			return err
		}},
		{name: "put_installed_package", call: func() error {
			_, err := store.PutInstalledPackage(context.Background(), fixtureID, "agent-closed", unit,
				skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}},
		{name: "delete_installed_package", call: func() error {
			_, err := store.DeleteInstalledPackage(context.Background(), fixtureID, "agent-closed", "closed-pkg",
				skills.InstalledPackageReceipt{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: "agent-closed", Name: "closed-pkg", WrittenHash: unit.PackageHash})
			return err
		}},
		{name: "restore_installed_package", call: func() error {
			_, err := store.RestoreInstalledPackage(context.Background(), fixtureID, "agent-closed", "closed-pkg",
				skills.InstalledPackageReceipt{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: "agent-closed", Name: "closed-pkg", WrittenHash: unit.PackageHash, PriorHash: unit.PackageHash},
				unit)
			return err
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, skills.ErrStoreClosed) {
			t.Errorf("%s after Close = %v, want ErrStoreClosed", tc.name, err)
		}
	}
}

func TestCanceledContext_DatabaseOperationsFailLoudly(t *testing.T) {
	store := openPostgresStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	skill := validCoverageSkill("canceled")
	unit := validCoverageInstalledUnit(t, "canceled-pkg", "agent-canceled")
	cases := []struct {
		name string
		call func() error
	}{
		{name: "upsert", call: func() error { return store.Upsert(ctx, fixtureID, skill) }},
		{name: "get", call: func() error { _, err := store.Get(ctx, fixtureID, skill.Name); return err }},
		{name: "list", call: func() error { _, err := store.List(ctx, fixtureID, skills.ListFilter{}); return err }},
		{name: "search", call: func() error { _, err := store.Search(ctx, fixtureID, skill.Name, 1); return err }},
		{name: "delete", call: func() error { return store.Delete(ctx, fixtureID, skill.Name, skill.Scope) }},
		{name: "snapshot", call: func() error {
			_, err := store.(skills.SnapshotCandidateSearcher).SearchSnapshot(ctx, fixtureID, skill.Name, []skills.Skill{skill}, 1)
			return err
		}},
		{name: "get_installed_package", call: func() error {
			_, err := store.GetInstalledPackage(ctx, fixtureID, "agent-canceled", "canceled-pkg")
			return err
		}},
		{name: "resolve_support", call: func() error {
			uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
			if err != nil {
				return err
			}
			_, err = store.ResolveSupport(ctx, fixtureID, "agent-canceled", "canceled-pkg", uri)
			return err
		}},
		{name: "put_installed_package", call: func() error {
			_, err := store.PutInstalledPackage(ctx, fixtureID, "agent-canceled", unit,
				skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}},
		{name: "delete_installed_package", call: func() error {
			_, err := store.DeleteInstalledPackage(ctx, fixtureID, "agent-canceled", "canceled-pkg",
				skills.InstalledPackageReceipt{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: "agent-canceled", Name: "canceled-pkg", WrittenHash: unit.PackageHash})
			return err
		}},
		{name: "restore_installed_package", call: func() error {
			_, err := store.RestoreInstalledPackage(ctx, fixtureID, "agent-canceled", "canceled-pkg",
				skills.InstalledPackageReceipt{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, AgentID: "agent-canceled", Name: "canceled-pkg", WrittenHash: unit.PackageHash, PriorHash: unit.PackageHash},
				unit)
			return err
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s with canceled context = %v, want context.Canceled", tc.name, err)
		}
	}
}

func TestClosedEventBus_PersistingOperationsFailLoudly(t *testing.T) {
	ctx := context.Background()
	bus := buildBus(t)
	store, err := postgres.New(
		skills.ConfigSnapshot{Driver: "postgres", DSN: freshSchema(t, requireDSN(t))},
		skills.Deps{Bus: bus},
	)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	pack := validCoverageSkill("closed-bus-pack")
	pack.Origin = skills.OriginPack
	deletable := validCoverageSkill("closed-bus-delete")
	for _, skill := range []skills.Skill{pack, deletable} {
		if err := store.Upsert(ctx, fixtureID, skill); err != nil {
			t.Fatalf("seed %q: %v", skill.Name, err)
		}
	}
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("close EventBus: %v", err)
	}

	if err := store.Upsert(ctx, fixtureID, validCoverageSkill("closed-bus-upsert")); !errors.Is(err, events.ErrBusClosed) {
		t.Errorf("Upsert with closed bus = %v, want ErrBusClosed", err)
	}
	if _, err := store.Search(ctx, fixtureID, "searchable", 10); !errors.Is(err, events.ErrBusClosed) {
		t.Errorf("Search with closed bus = %v, want ErrBusClosed", err)
	}
	if err := store.Delete(ctx, fixtureID, deletable.Name, deletable.Scope); !errors.Is(err, events.ErrBusClosed) {
		t.Errorf("Delete with closed bus = %v, want ErrBusClosed", err)
	}
	overwrite := pack
	overwrite.Origin = skills.OriginGenerated
	overwrite.ContentHash = ""
	if err := store.Upsert(ctx, fixtureID, overwrite); !errors.Is(err, skills.ErrPackOverwriteRefused) || !errors.Is(err, events.ErrBusClosed) {
		t.Errorf("pack overwrite with closed bus = %v, want ErrPackOverwriteRefused and ErrBusClosed", err)
	}
}

func TestList_FiltersScopeTaskTypeTagsAndCapsLimit(t *testing.T) {
	store := openPostgresStore(t)
	ctx := context.Background()
	wanted := validCoverageSkill("wanted")
	wanted.TaskType = "review"
	wanted.Tags = []string{"security", "postgres"}
	wanted.Extra = map[string]any{"durable": true}
	decoy := validCoverageSkill("decoy")
	decoy.TaskType = "build"
	decoy.Tags = []string{"other"}
	for _, skill := range []skills.Skill{wanted, decoy} {
		if err := store.Upsert(ctx, fixtureID, skill); err != nil {
			t.Fatalf("Upsert(%q): %v", skill.Name, err)
		}
	}

	got, err := store.List(ctx, fixtureID, skills.ListFilter{
		Scope: skills.ScopeSession, TaskType: "review", Tags: []string{"security", "postgres"}, Limit: 50_000,
	})
	if err != nil {
		t.Fatalf("List(filtered): %v", err)
	}
	if len(got) != 1 || got[0].Name != wanted.Name || got[0].Extra["durable"] != true {
		t.Fatalf("List(filtered) = %+v, want wanted with Extra round-trip", got)
	}
}

func TestSearch_ExercisesORRegexExactAndEmptyFallbacks(t *testing.T) {
	store := openPostgresStore(t)
	ctx := context.Background()
	corpus := []skills.Skill{
		validCoverageSkill("harbor-only"),
		validCoverageSkill("="),
		validCoverageSkill("["),
	}
	corpus[0].Description = "harbor"
	for _, skill := range corpus {
		if err := store.Upsert(ctx, fixtureID, skill); err != nil {
			t.Fatalf("Upsert(%q): %v", skill.Name, err)
		}
	}

	orResult, err := store.Search(ctx, fixtureID, "harbor missing", 50_000)
	if err != nil {
		t.Fatalf("Search(OR fallback): %v", err)
	}
	if len(orResult) != 1 || orResult[0].Skill.Name != "harbor-only" || orResult[0].Path != skills.PathFullText {
		t.Fatalf("OR fallback = %+v", orResult)
	}
	regexResult, err := store.Search(ctx, fixtureID, "=", 1)
	if err != nil {
		t.Fatalf("Search(regex): %v", err)
	}
	if len(regexResult) != 1 || regexResult[0].Skill.Name != "=" || regexResult[0].Path != skills.PathRegex {
		t.Fatalf("regex fallback = %+v", regexResult)
	}
	exactResult, err := store.Search(ctx, fixtureID, "[", 1)
	if err != nil {
		t.Fatalf("Search(exact): %v", err)
	}
	if len(exactResult) != 1 || exactResult[0].Skill.Name != "[" || exactResult[0].Path != skills.PathExact {
		t.Fatalf("exact fallback = %+v", exactResult)
	}
	empty, err := store.Search(ctx, fixtureID, "   ", 0)
	if err != nil {
		t.Fatalf("Search(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("Search(empty) = %+v, want empty", empty)
	}
}

func TestSearchSnapshot_ORFallbackAndEmptyBoundaries(t *testing.T) {
	store := openPostgresStore(t)
	searcher := store.(skills.SnapshotCandidateSearcher)
	candidates := []skills.Skill{
		{Name: "alpha", Description: "harbor", UpdatedAt: fixtureIDTime(2)},
		{Name: "beta", Description: "dock", UpdatedAt: fixtureIDTime(1)},
	}
	got, err := searcher.SearchSnapshot(context.Background(), fixtureID, "harbor missing", candidates, 50_000)
	if err != nil {
		t.Fatalf("SearchSnapshot(OR fallback): %v", err)
	}
	if len(got) != 1 || got[0].Skill.Name != "alpha" || got[0].Path != skills.PathFullText {
		t.Fatalf("snapshot OR fallback = %+v", got)
	}
	for _, tc := range []struct {
		name       string
		query      string
		candidates []skills.Skill
	}{
		{name: "empty query", query: "   ", candidates: candidates},
		{name: "punctuation query", query: "---", candidates: candidates},
		{name: "empty candidates", query: "harbor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := searcher.SearchSnapshot(context.Background(), fixtureID, tc.query, tc.candidates, 0)
			if err != nil {
				t.Fatalf("SearchSnapshot: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("SearchSnapshot = %+v, want empty", got)
			}
		})
	}
}

func fixtureIDTime(seconds int64) time.Time {
	return time.Unix(seconds, 0).UTC()
}
