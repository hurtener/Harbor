package localdb

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
)

// TestSearchLadder_FTSOff_FallsBackToRegex — drives the ladder with
// FTS5 forced OFF via direct ftsAvailable mutation. The regex path
// MUST return rows and the audit emit reports `Path == "regex"`.
//
// This is the brief 04 §4.4 "FTS-off fallback test" gate.
func TestSearchLadder_FTSOff_FallsBackToRegex(t *testing.T) {
	ctx := context.Background()
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(ctx) }()

	dsn := filepath.Join(t.TempDir(), "fts-off.sqlite")
	store, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = store.Close(ctx) }()

	// Force FTS off — package-internal test reaches the field.
	d := store.(*driver)
	d.ftsAvailable = false

	id := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-fts-off",
			UserID:    "u-fts-off",
			SessionID: "s-fts-off",
		},
		RunID: "r-fts-off",
	}

	s := skills.Skill{
		Name:        "regex-target",
		Title:       "Regex Target",
		Description: "harbor planner reference",
		Trigger:     "trg",
		Steps:       []string{"s"},
		Origin:      skills.OriginGenerated,
		Scope:       skills.ScopeProject,
		UpdatedAt:   time.Now().UTC(),
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, id, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// "planner" — appears in description; regex body-search ranks it
	// at 0.75 (brief 04 §4.4 constant).
	out, err := d.Search(ctx, id, "planner", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("regex fallback: got %d hits; want 1", len(out))
	}
	if out[0].Path != skills.PathRegex {
		t.Fatalf("path: got %q; want %q", out[0].Path, skills.PathRegex)
	}
	if out[0].Score != 0.75 {
		t.Fatalf("score: got %v; want 0.75 (regex body search constant)", out[0].Score)
	}

	// "regex-target" — full-name match → 0.95 score.
	out2, err := d.Search(ctx, id, "regex-target", 5)
	if err != nil {
		t.Fatalf("Search 2: %v", err)
	}
	if len(out2) != 1 || out2[0].Path != skills.PathRegex || out2[0].Score != 0.95 {
		t.Fatalf("regex name-fullmatch: got %+v; want path=regex score=0.95", out2)
	}
}

// TestRegexScore_Constants — the brief 04 §4.4 scoring constants on
// regexScore, exercised directly on the helper.
func TestRegexScore_Constants(t *testing.T) {
	s := skills.Skill{
		Name:        "alpha",
		Title:       "Title",
		Description: "Body",
		Trigger:     "trg",
	}

	tests := []struct {
		name  string
		query string
		want  float64
	}{
		{"name_fullmatch", "alpha", 0.95},
		{"name_prefix", "alph", 0.90},
		{"name_search", "lph", 0.85}, // matches inside "alpha", not at prefix
		{"body_search", "ody", 0.75},
		{"miss", "zzz", 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			re, err := buildRegex(tc.query)
			if err != nil {
				t.Fatalf("buildRegex: %v", err)
			}
			got := regexScore(re, s)
			if got != tc.want {
				t.Fatalf("regexScore(%q): got %v; want %v", tc.query, got, tc.want)
			}
		})
	}
}

func newSnapshotSearchDriver(t *testing.T) *driver {
	t.Helper()
	ctx := context.Background()
	bus, err := events.Open(ctx, config.EventsConfig{
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
	store, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: filepath.Join(t.TempDir(), "snapshot.sqlite")}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store.(*driver)
}

func snapshotSearchID() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-snapshot", UserID: "u-snapshot", SessionID: "s-snapshot"},
		RunID:    "r-snapshot",
	}
}

func TestSearchSnapshot_FTSUnavailable_UsesRegexAndExactFallbacks(t *testing.T) {
	d := newSnapshotSearchDriver(t)
	d.ftsAvailable = false
	candidates := []skills.Skill{
		{Name: "regex-target", Description: "harbor planner reference", UpdatedAt: time.Unix(2, 0)},
		{Name: "[", Description: "punctuation exact target", UpdatedAt: time.Unix(1, 0)},
	}

	regex, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "planner", candidates, 5)
	if err != nil {
		t.Fatalf("SearchSnapshot(regex): %v", err)
	}
	if len(regex) != 1 || regex[0].Skill.Name != "regex-target" || regex[0].Path != skills.PathRegex {
		t.Fatalf("regex fallback = %+v, want regex-target via %q", regex, skills.PathRegex)
	}

	exact, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "[", candidates, 5)
	if err != nil {
		t.Fatalf("SearchSnapshot(exact): %v", err)
	}
	if len(exact) != 1 || exact[0].Skill.Name != "[" || exact[0].Path != skills.PathExact {
		t.Fatalf("exact fallback = %+v, want punctuation target via %q", exact, skills.PathExact)
	}
}

func TestSearchSnapshot_FTS5EqualRank_AppliesTieBreakBeforeLimit(t *testing.T) {
	d := newSnapshotSearchDriver(t)
	if !d.ftsAvailable {
		t.Skip("reason: linked SQLite build does not provide FTS5")
	}
	newest := time.Unix(30, 0).UTC()
	candidates := []skills.Skill{
		{Name: "charl-old", Title: "same", Trigger: "same", Description: "harborneedle", UpdatedAt: time.Unix(10, 0).UTC()},
		{Name: "bravo-new", Title: "same", Trigger: "same", Description: "harborneedle", UpdatedAt: newest},
		{Name: "alpha-new", Title: "same", Trigger: "same", Description: "harborneedle", UpdatedAt: newest},
	}

	got, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "harborneedle", candidates, 2)
	if err != nil {
		t.Fatalf("SearchSnapshot: %v", err)
	}
	if len(got) != 2 || got[0].Skill.Name != "alpha-new" || got[1].Skill.Name != "bravo-new" {
		t.Fatalf("equal-rank top two = %+v, want alpha-new then bravo-new", got)
	}
	for _, result := range got {
		if result.Path != skills.PathFTS5 {
			t.Fatalf("path = %q, want %q", result.Path, skills.PathFTS5)
		}
	}
}

func TestSearchSnapshot_FailClosedBoundariesAndSemanticDispatch(t *testing.T) {
	t.Run("identity required", func(t *testing.T) {
		d := newSnapshotSearchDriver(t)
		if _, err := d.SearchSnapshot(context.Background(), identity.Quadruple{}, "harbor", nil, 1); err == nil {
			t.Fatal("SearchSnapshot accepted an empty identity")
		}
	})

	t.Run("closed store", func(t *testing.T) {
		d := newSnapshotSearchDriver(t)
		if err := d.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "harbor", nil, 1); !errors.Is(err, skills.ErrStoreClosed) {
			t.Fatalf("SearchSnapshot after Close = %v, want ErrStoreClosed", err)
		}
	})

	t.Run("semantic mode fails loud without embedder", func(t *testing.T) {
		d := newSnapshotSearchDriver(t)
		d.retrieval = skills.RetrievalSemantic
		candidate := []skills.Skill{{Name: "semantic", Description: "harbor", UpdatedAt: time.Unix(1, 0)}}
		if _, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "harbor", candidate, 0); err == nil {
			t.Fatal("semantic SearchSnapshot without Embedder returned nil error")
		}
	})
}

func TestSearchSnapshot_FTS5FallsBackFromANDToOR(t *testing.T) {
	d := newSnapshotSearchDriver(t)
	if !d.ftsAvailable {
		t.Skip("reason: linked SQLite build does not provide FTS5")
	}
	candidates := []skills.Skill{
		{Name: "alpha", Description: "harbor only", UpdatedAt: time.Unix(2, 0).UTC()},
		{Name: "beta", Description: "dock only", UpdatedAt: time.Unix(1, 0).UTC()},
	}

	got, err := d.SearchSnapshot(context.Background(), snapshotSearchID(), "harbor missing", candidates, 50_000)
	if err != nil {
		t.Fatalf("SearchSnapshot: %v", err)
	}
	if len(got) != 1 || got[0].Skill.Name != "alpha" || got[0].Path != skills.PathFTS5 {
		t.Fatalf("OR fallback = %+v, want alpha via FTS5", got)
	}

	empty, err := d.searchSnapshotFTS5(context.Background(), "   ", candidates, 5)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty FTS query = (%+v, %v), want empty success", empty, err)
	}
}

func TestSnapshotFTS5Results_RejectsDriverCorruptionAndNormalizesScores(t *testing.T) {
	candidates := []skills.Skill{
		{Name: "newer", UpdatedAt: time.Unix(2, 0).UTC()},
		{Name: "older", UpdatedAt: time.Unix(1, 0).UTC()},
	}
	if empty, err := snapshotFTS5Results(candidates, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty hits = (%+v, %v)", empty, err)
	}
	if _, err := snapshotFTS5Results(candidates, []snapshotFTS5Hit{{ordinal: 0}}); err == nil {
		t.Fatal("out-of-range ordinal was accepted")
	}
	got, err := snapshotFTS5Results(candidates, []snapshotFTS5Hit{{ordinal: 2, raw: 3}, {ordinal: 1, raw: 1}})
	if err != nil {
		t.Fatalf("snapshotFTS5Results: %v", err)
	}
	if len(got) != 2 || got[0].Skill.Name != "newer" || got[0].Score != 1 || got[1].Skill.Name != "older" || got[1].Score != 0 {
		t.Fatalf("normalized results = %+v", got)
	}
}
