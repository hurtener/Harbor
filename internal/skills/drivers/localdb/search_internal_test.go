package localdb

import (
	"context"
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
