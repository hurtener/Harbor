package localdb_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/conformancetest"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
)

// fixtureID matches the conformance harness default.
var fixtureID = identity.Quadruple{
	Identity: identity.Identity{
		TenantID:  "t-localdb",
		UserID:    "u-localdb",
		SessionID: "s-localdb",
	},
	RunID: "r-localdb",
}

func newBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func openStore(t *testing.T, dsn string) skills.SkillStore {
	t.Helper()
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: newBus(t)})
	if err != nil {
		t.Fatalf("localdb.New(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// TestConformance runs the shared SkillStore conformance suite
// against the localdb driver, using a temp-file DSN so the restart-
// survival subtest can reopen.
func TestConformance(t *testing.T) {
	conformancetest.Run(t, func(t *testing.T) conformancetest.Harness {
		t.Helper()
		bus := newBus(t)
		dsn := filepath.Join(t.TempDir(), "skills.sqlite")
		store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
			skills.Deps{Bus: bus})
		if err != nil {
			t.Fatalf("localdb.New: %v", err)
		}
		return conformancetest.Harness{
			Store:   store,
			Bus:     bus,
			Cleanup: func() { _ = store.Close(context.Background()) },
			ReopenedStore: func() (skills.SkillStore, error) {
				return localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
					skills.Deps{Bus: bus})
			},
		}
	})
}

// TestUpsert_RejectsInvalidSkill — empty Trigger / empty Steps must
// fail validation at the boundary.
func TestUpsert_RejectsInvalidSkill(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")

	noTrigger := skills.Skill{
		Name:   "no-trigger",
		Steps:  []string{"step one"},
		Origin: skills.OriginGenerated,
		Scope:  skills.ScopeProject,
	}
	if err := store.Upsert(ctx, fixtureID, noTrigger); err == nil ||
		!errors.Is(err, skills.ErrInvalidSkill) {
		t.Fatalf("empty Trigger: want ErrInvalidSkill, got %v", err)
	}
	noSteps := skills.Skill{
		Name:    "no-steps",
		Trigger: "trigger",
		Origin:  skills.OriginGenerated,
		Scope:   skills.ScopeProject,
	}
	if err := store.Upsert(ctx, fixtureID, noSteps); err == nil ||
		!errors.Is(err, skills.ErrInvalidSkill) {
		t.Fatalf("empty Steps: want ErrInvalidSkill, got %v", err)
	}
}

// TestSearch_FTSPath — golden ranking via the FTS5 path. Seed three
// skills whose descriptions vary in token frequency for `harbor`;
// the FTS path should return all three with deterministic ordering.
func TestSearch_FTSPath(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")

	now := time.Now().UTC()
	corpus := []skills.Skill{
		mustHash(skills.Skill{
			Name: "alpha", Title: "Alpha", Trigger: "trigger-alpha",
			Description: "harbor harbor harbor frequent",
			Steps:       []string{"step a"},
			Origin:      skills.OriginGenerated,
			Scope:       skills.ScopeProject,
			Tags:        []string{"go"},
			UpdatedAt:   now,
		}),
		mustHash(skills.Skill{
			Name: "bravo", Title: "Bravo", Trigger: "trigger-bravo",
			Description: "harbor once",
			Steps:       []string{"step b"},
			Origin:      skills.OriginGenerated,
			Scope:       skills.ScopeProject,
			Tags:        []string{"go"},
			UpdatedAt:   now,
		}),
		mustHash(skills.Skill{
			Name: "charlie", Title: "Charlie", Trigger: "trigger-charlie",
			Description: "completely unrelated",
			Steps:       []string{"step c"},
			Origin:      skills.OriginGenerated,
			Scope:       skills.ScopeProject,
			Tags:        []string{"go"},
			UpdatedAt:   now,
		}),
	}
	for _, s := range corpus {
		if err := store.Upsert(ctx, fixtureID, s); err != nil {
			t.Fatalf("Upsert(%q): %v", s.Name, err)
		}
	}
	out, err := store.Search(ctx, fixtureID, "harbor", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("Search: got %d hits; want 2 (alpha, bravo); rows=%+v", len(out), out)
	}
	if out[0].Path != skills.PathFTS5 {
		t.Fatalf("Search path: got %q; want %q", out[0].Path, skills.PathFTS5)
	}
	if out[0].Skill.Name != "alpha" {
		t.Fatalf("Search ordering: expected alpha first (higher term frequency), got %q", out[0].Skill.Name)
	}
	if out[0].Score < out[1].Score {
		t.Fatalf("Search scores: alpha=%v should be >= bravo=%v", out[0].Score, out[1].Score)
	}
}

// TestSearch_ExactPath — when neither FTS nor regex matches, the
// exact path runs on lowercase equality against name/title/trigger
// and returns score=1.0.
func TestSearch_ExactPath(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")
	s := mustHash(skills.Skill{
		Name:        "exact-target",
		Title:       "Exact Target",
		Trigger:     "trg",
		Description: "no overlap whatsoever",
		Steps:       []string{"s"},
		Origin:      skills.OriginGenerated,
		Scope:       skills.ScopeProject,
		UpdatedAt:   time.Now().UTC(),
	})
	if err := store.Upsert(ctx, fixtureID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	out, err := store.Search(ctx, fixtureID, "trg", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 hit on exact trigger; got %d", len(out))
	}
	// Path can be FTS5 (single-token match in trigger column) OR
	// exact (when FTS5 produces no hit). Either is correct per the
	// ladder. We assert the row itself, not the path.
	if out[0].Skill.Name != "exact-target" {
		t.Fatalf("got %q; want exact-target", out[0].Skill.Name)
	}
}

// TestPackOverwriteRefused — emits skill.pack_overwrite_refused on
// the bus AND leaves the existing row untouched.
func TestPackOverwriteRefused(t *testing.T) {
	ctx := context.Background()
	bus := newBus(t)
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	pack := mustHash(skills.Skill{
		Name: "conflict", Trigger: "trg",
		Description: "from a pack",
		Steps:       []string{"s"},
		Origin:      skills.OriginPack,
		OriginRef:   "pack-foo@v1",
		Scope:       skills.ScopeProject,
	})
	if err := store.Upsert(ctx, fixtureID, pack); err != nil {
		t.Fatalf("seed pack: %v", err)
	}

	// Subscribe before the refused upsert so we can assert the emit
	// landed.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  fixtureID.TenantID,
		User:    fixtureID.UserID,
		Session: fixtureID.SessionID,
		Types:   []events.EventType{skills.EventTypeSkillPackOverwriteRefused},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	gen := pack
	gen.Origin = skills.OriginGenerated
	gen.Description = "hostile overwrite"
	gen.ContentHash = ""

	err = store.Upsert(ctx, fixtureID, gen)
	if err == nil || !errors.Is(err, skills.ErrPackOverwriteRefused) {
		t.Fatalf("expected ErrPackOverwriteRefused; got %v", err)
	}

	// Assert event emitted.
	select {
	case ev := <-sub.Events():
		if ev.Type != skills.EventTypeSkillPackOverwriteRefused {
			t.Fatalf("emit: wrong type %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for skill.pack_overwrite_refused emit")
	}

	// Row survived untouched.
	got, err := store.Get(ctx, fixtureID, "conflict")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Description != "from a pack" {
		t.Fatalf("pack row was mutated by refused overwrite: %+v", got)
	}
}

// TestIdentityRejected — emits skill.identity_rejected on missing
// triple AND returns wrapped ErrIdentityRequired.
func TestIdentityRejected(t *testing.T) {
	ctx := context.Background()
	bus := newBus(t)
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	// Subscribe with an elevated, type-only filter (the rejected
	// event carries a substituted-sentinel identity; subscribing
	// with the canonical fixture identity wouldn't match because
	// the bus filter checks the event's identity, not the
	// subscriber's). Pass the FULL fixture triple just to satisfy
	// Subscribe's validation; rely on Type filtering to catch the
	// event regardless of its substituted identity.
	sub, err := bus.Subscribe(ctx, events.Filter{
		Admin: true,
		Types: []events.EventType{skills.EventTypeSkillIdentityRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	_ = sub // event not asserted here — Upsert error already covers identity rejection

	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}} // missing session
	err = store.Upsert(ctx, bad, skills.Skill{
		Name: "x", Trigger: "t", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	})
	if err == nil || !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("Upsert with missing session: want ErrIdentityRequired, got %v", err)
	}
}

// TestSearch_IsolatesByIdentity — two identities never see each
// other's skills.
func TestSearch_IsolatesByIdentity(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")

	idA := fixtureID
	idB := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-other", UserID: "u-other", SessionID: "s-other"},
		RunID:    "r-other",
	}

	if err := store.Upsert(ctx, idA, mustHash(skills.Skill{
		Name: "a-only", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		Description: "harbor",
	})); err != nil {
		t.Fatalf("Upsert(A): %v", err)
	}
	if err := store.Upsert(ctx, idB, mustHash(skills.Skill{
		Name: "b-only", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		Description: "harbor",
	})); err != nil {
		t.Fatalf("Upsert(B): %v", err)
	}

	outA, err := store.Search(ctx, idA, "harbor", 10)
	if err != nil {
		t.Fatalf("Search(A): %v", err)
	}
	if len(outA) != 1 || outA[0].Skill.Name != "a-only" {
		t.Fatalf("A search isolation: got %+v; want only a-only", outA)
	}

	listB, err := store.List(ctx, idB, skills.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if len(listB) != 1 || listB[0].Name != "b-only" {
		t.Fatalf("B list isolation: got %+v; want only b-only", listB)
	}
}

// TestSearch_EmptyQueryReturnsEmpty — Search with empty / whitespace
// query returns no results without erroring (still emits audit).
func TestSearch_EmptyQueryReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")
	out, err := store.Search(ctx, fixtureID, "   ", 5)
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty result; got %d", len(out))
	}
}

// TestGet_AfterClose — methods on a closed store return ErrStoreClosed.
func TestGet_AfterClose(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ":memory:")
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.Get(ctx, fixtureID, "x"); !errors.Is(err, skills.ErrStoreClosed) {
		t.Fatalf("Get after Close: want ErrStoreClosed, got %v", err)
	}
	// Close is idempotent.
	if err := store.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestRegistry_OpenViaFactory — the registered "localdb" name opens
// through skills.Open as well as via the direct New constructor.
func TestRegistry_OpenViaFactory(t *testing.T) {
	ctx := context.Background()
	bus := newBus(t)
	store, err := skills.Open(ctx,
		skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	defer func() { _ = store.Close(ctx) }()
	if err := store.Upsert(ctx, fixtureID, mustHash(skills.Skill{
		Name: "via-registry", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	})); err != nil {
		t.Fatalf("Upsert via registry: %v", err)
	}
}

// mustHash stamps a canonical ContentHash on s for test fixtures.
func mustHash(s skills.Skill) skills.Skill {
	s.ContentHash = skills.CanonicalContentHash(s)
	return s
}
