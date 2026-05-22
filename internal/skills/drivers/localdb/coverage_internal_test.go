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

func covBus(t *testing.T) events.EventBus {
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

func covDriver(t *testing.T) *driver {
	t.Helper()
	store, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"},
		skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store.(*driver)
}

var covID = identity.Quadruple{
	Identity: identity.Identity{TenantID: "t-cov", UserID: "u-cov", SessionID: "s-cov"},
	RunID:    "r-cov",
}

// --- New error paths --------------------------------------------------------

func TestNew_RejectsMissingBus(t *testing.T) {
	t.Parallel()
	if _, err := New(skills.ConfigSnapshot{DSN: ":memory:"}, skills.Deps{Bus: nil}); err == nil {
		t.Fatal("New with nil Bus returned nil err")
	}
}

func TestNew_RejectsEmptyDSN(t *testing.T) {
	t.Parallel()
	if _, err := New(skills.ConfigSnapshot{DSN: ""}, skills.Deps{Bus: covBus(t)}); err == nil {
		t.Fatal("New with empty DSN returned nil err")
	}
}

func TestNew_RejectsUnparseableFileURI(t *testing.T) {
	t.Parallel()
	// A `file:` DSN containing a raw control character fails url.Parse
	// inside augmentDSNForPragmas — New must surface it, not panic.
	if _, err := New(skills.ConfigSnapshot{DSN: "file:\x7f.db"}, skills.Deps{Bus: covBus(t)}); err == nil {
		t.Fatal("New with unparseable file: URI returned nil err")
	}
}

func TestNew_FileDSNRoundTrips(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "cov.sqlite")
	store, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("New(file dsn): %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- Extra round-trip (marshalExtra / unmarshalExtra) -----------------------

func TestExtra_RoundTripsThroughUpsertGet(t *testing.T) {
	// No t.Parallel(): augmentDSNForPragmas maps ":memory:" to
	// `file::memory:?cache=shared`, which is process-global — parallel
	// :memory: stores collide ("database table is locked").
	d := covDriver(t)
	ctx := context.Background()

	s := skills.Skill{
		Name:    "extra-skill",
		Trigger: "trg",
		Steps:   []string{"s"},
		Origin:  skills.OriginGenerated,
		Scope:   skills.ScopeProject,
		Extra: map[string]any{
			"model":  "claude",
			"pinned": true,
		},
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := d.Get(ctx, covID, "extra-skill")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Extra["model"] != "claude" {
		t.Errorf("Extra[model] = %v, want claude", got.Extra["model"])
	}
	if got.Extra["pinned"] != true {
		t.Errorf("Extra[pinned] = %v, want true", got.Extra["pinned"])
	}

	// A skill with nil Extra round-trips as nil (the "{}" sentinel
	// unmarshals back to nil — marshalExtra/unmarshalExtra empty path).
	bare := skills.Skill{
		Name: "no-extra", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}
	bare.ContentHash = skills.CanonicalContentHash(bare)
	if err = d.Upsert(ctx, covID, bare); err != nil {
		t.Fatalf("Upsert(bare): %v", err)
	}
	gotBare, err := d.Get(ctx, covID, "no-extra")
	if err != nil {
		t.Fatalf("Get(bare): %v", err)
	}
	if len(gotBare.Extra) != 0 {
		t.Errorf("bare Extra = %v, want empty", gotBare.Extra)
	}
}

// --- List filter branches ---------------------------------------------------

func TestList_FilterBranches(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store — see
	// TestExtra_RoundTripsThroughUpsertGet.
	d := covDriver(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := []skills.Skill{
		{Name: "ops-a", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject, TaskType: "ops", Tags: []string{"infra"}, UpdatedAt: now},
		{Name: "ops-b", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeProject, TaskType: "ops", Tags: []string{"db"}, UpdatedAt: now},
		{Name: "doc-a", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated,
			Scope: skills.ScopeTenant, TaskType: "docs", Tags: []string{"infra"}, UpdatedAt: now},
	}
	for _, s := range seed {
		s.ContentHash = skills.CanonicalContentHash(s)
		if err := d.Upsert(ctx, covID, s); err != nil {
			t.Fatalf("Upsert(%q): %v", s.Name, err)
		}
	}

	// Scope filter.
	byScope, err := d.List(ctx, covID, skills.ListFilter{Scope: skills.ScopeTenant, Limit: 10})
	if err != nil {
		t.Fatalf("List(scope): %v", err)
	}
	if len(byScope) != 1 || byScope[0].Name != "doc-a" {
		t.Errorf("List(Scope=tenant) = %v, want [doc-a]", names(byScope))
	}

	// TaskType filter.
	byType, err := d.List(ctx, covID, skills.ListFilter{TaskType: "ops", Limit: 10})
	if err != nil {
		t.Fatalf("List(taskType): %v", err)
	}
	if len(byType) != 2 {
		t.Errorf("List(TaskType=ops) = %v, want 2 rows", names(byType))
	}

	// Tag any-of filter.
	byTag, err := d.List(ctx, covID, skills.ListFilter{Tags: []string{"infra"}, Limit: 10})
	if err != nil {
		t.Fatalf("List(tags): %v", err)
	}
	if len(byTag) != 2 {
		t.Errorf("List(Tags=infra) = %v, want 2 rows", names(byTag))
	}

	// Offset paging: Limit=1 with Offset 0 vs 1 must return distinct
	// single rows (Upsert stamps its own updated_at, so the absolute
	// order is upsert-time-dependent — the test only pins that Offset
	// actually advances the window).
	page0, err := d.List(ctx, covID, skills.ListFilter{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("List(page0): %v", err)
	}
	page1, err := d.List(ctx, covID, skills.ListFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("List(page1): %v", err)
	}
	if len(page0) != 1 || len(page1) != 1 {
		t.Fatalf("paged List sizes = %d/%d, want 1/1", len(page0), len(page1))
	}
	if page0[0].Name == page1[0].Name {
		t.Errorf("List Offset did not advance: page0=%q page1=%q", page0[0].Name, page1[0].Name)
	}
}

func names(in []skills.Skill) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Name
	}
	return out
}

// --- buildRegex branches ----------------------------------------------------

func TestBuildRegex_Branches(t *testing.T) {
	t.Parallel()

	// Single valid token → compiles as-is.
	if re, err := buildRegex("alpha"); err != nil || re == nil {
		t.Errorf("buildRegex(alpha) = (%v, %v), want a compiled regex", re, err)
	}

	// Single token that is an invalid regex AND has no alphanumeric
	// tokens → falls through to the token path, which finds nothing →
	// error.
	if _, err := buildRegex("((("); err == nil {
		t.Error("buildRegex(\"(((\") returned nil err, want a no-tokens failure")
	}

	// Single token that is an invalid regex but DOES carry an
	// alphanumeric token → token-OR fallback compiles successfully.
	if re, err := buildRegex("foo("); err != nil || re == nil {
		t.Errorf("buildRegex(\"foo(\") = (%v, %v), want token-OR fallback to compile", re, err)
	}

	// Multi-token NL-style query → OR-of-tokens regex.
	re, err := buildRegex("deploy the service")
	if err != nil || re == nil {
		t.Fatalf("buildRegex(multi-token) = (%v, %v), want compiled OR regex", re, err)
	}
	if !re.MatchString("service") || !re.MatchString("deploy") {
		t.Error("multi-token regex did not match its own tokens")
	}

	// Empty / whitespace-only query → error.
	if _, err := buildRegex("   "); err == nil {
		t.Error("buildRegex(whitespace) returned nil err, want empty-query failure")
	}
}

// --- searchExact frozen 1.0 score ------------------------------------------

func TestSearchExact_ScoreIsExactlyOne(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store — see
	// TestExtra_RoundTripsThroughUpsertGet.
	d := covDriver(t)
	ctx := context.Background()

	s := skills.Skill{
		Name: "exact-row", Title: "Exact Row", Trigger: "the-exact-trigger",
		Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		UpdatedAt: time.Now().UTC(),
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Call the exact path directly — it is the ladder's terminal tier
	// and always scores a matched row at exactly 1.0 (brief 04 §4.4).
	out, err := d.searchExact(ctx, covID, "the-exact-trigger", 5)
	if err != nil {
		t.Fatalf("searchExact: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("searchExact hits = %d, want 1", len(out))
	}
	if out[0].Score != 1.0 {
		t.Errorf("searchExact score = %v, want exactly 1.0", out[0].Score)
	}
	if out[0].Path != skills.PathExact {
		t.Errorf("searchExact path = %q, want %q", out[0].Path, skills.PathExact)
	}

	// A query that matches nothing returns no rows, no error.
	none, err := d.searchExact(ctx, covID, "nothing-matches-this", 5)
	if err != nil {
		t.Fatalf("searchExact(miss): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("searchExact(miss) = %v, want empty", none)
	}
}

// --- searchRegex name_search path via the public Search ladder --------------

func TestSearchRegex_NameSearchScore(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store — see
	// TestExtra_RoundTripsThroughUpsertGet.
	d := covDriver(t)
	d.ftsAvailable = false // force the ladder onto the regex tier
	ctx := context.Background()

	s := skills.Skill{
		Name: "deployment", Title: "T", Trigger: "trg", Description: "body",
		Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		UpdatedAt: time.Now().UTC(),
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// "ploy" matches inside "deployment" but not at the prefix →
	// regexScore returns the name-search constant 0.85.
	out, err := d.Search(ctx, covID, "ploy", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Search(ploy) hits = %d, want 1", len(out))
	}
	if out[0].Path != skills.PathRegex {
		t.Fatalf("path = %q, want %q", out[0].Path, skills.PathRegex)
	}
	if out[0].Score != 0.85 {
		t.Errorf("score = %v, want 0.85 (regex name-search constant)", out[0].Score)
	}
}

// --- Delete success + not-found ---------------------------------------------

func TestDelete_SuccessAndNotFound(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	d := covDriver(t)
	ctx := context.Background()

	// Delete of a row that was never inserted → ErrSkillNotFound.
	if err := d.Delete(ctx, covID, "ghost"); err == nil ||
		!errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("Delete(ghost) = %v, want ErrSkillNotFound", err)
	}

	s := skills.Skill{
		Name: "deletable", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := d.Delete(ctx, covID, "deletable"); err != nil {
		t.Fatalf("Delete(deletable): %v", err)
	}
	// Gone now.
	if _, err := d.Get(ctx, covID, "deletable"); err == nil ||
		!errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrSkillNotFound", err)
	}
}

// --- Idempotent Upsert (emitUpserted idempotent=true branch) ----------------

func TestUpsert_IdempotentOnSameContentHash(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	d := covDriver(t)
	ctx := context.Background()

	s := skills.Skill{
		Name: "idem", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}
	// Second upsert with the identical content hash hits the
	// idempotent short-circuit (emitUpserted idempotent=true).
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert #2 (idempotent): %v", err)
	}
	// Last-write-wins on a genuine content change.
	changed := s
	changed.Description = "now different"
	changed.ContentHash = skills.CanonicalContentHash(changed)
	if err := d.Upsert(ctx, covID, changed); err != nil {
		t.Fatalf("Upsert #3 (LWW): %v", err)
	}
	got, err := d.Get(ctx, covID, "idem")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Description != "now different" {
		t.Errorf("Description = %q, want the last-write value", got.Description)
	}
}

// --- searchRegex limit truncation -------------------------------------------

func TestSearchRegex_TruncatesToLimit(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	d := covDriver(t)
	d.ftsAvailable = false // force the regex tier
	ctx := context.Background()

	// Five skills that all carry the token "widget" in their name.
	for _, n := range []string{"widget-a", "widget-b", "widget-c", "widget-d", "widget-e"} {
		s := skills.Skill{
			Name: n, Trigger: "trg", Steps: []string{"s"},
			Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
			UpdatedAt: time.Now().UTC(),
		}
		s.ContentHash = skills.CanonicalContentHash(s)
		if err := d.Upsert(ctx, covID, s); err != nil {
			t.Fatalf("Upsert(%q): %v", n, err)
		}
	}
	out, err := d.Search(ctx, covID, "widget", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("Search(limit=2) returned %d rows, want 2 (regex tier must truncate)", len(out))
	}
	if out[0].Path != skills.PathRegex {
		t.Errorf("path = %q, want %q", out[0].Path, skills.PathRegex)
	}
}

// --- Search limit clamp branches --------------------------------------------

func TestSearch_LimitClampBranches(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	d := covDriver(t)
	ctx := context.Background()

	s := skills.Skill{
		Name: "clamp-target", Trigger: "trg", Description: "harbor planner",
		Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		UpdatedAt: time.Now().UTC(),
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// limit <= 0 → falls back to the driver default.
	if _, err := d.Search(ctx, covID, "harbor", 0); err != nil {
		t.Fatalf("Search(limit=0): %v", err)
	}
	// limit > maxSearchN → capped to the driver maximum.
	if _, err := d.Search(ctx, covID, "harbor", 1_000_000); err != nil {
		t.Fatalf("Search(limit=huge): %v", err)
	}
}

// --- FTS5 OR-of-tokens fallback ---------------------------------------------

func TestSearchFTS5_ORFallbackWhenStrictANDMisses(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	d := covDriver(t)
	if !d.ftsAvailable {
		t.Skip("FTS5 not available in this build — OR-fallback path is FTS-only")
	}
	ctx := context.Background()

	s := skills.Skill{
		Name: "fts-or", Trigger: "trg", Description: "alpha planner reference",
		Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
		UpdatedAt: time.Now().UTC(),
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	if err := d.Upsert(ctx, covID, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// "alpha zzzznomatch": strict-AND finds nothing (no doc has both
	// tokens), so the ladder falls to the OR-of-tokens FTS expression,
	// which matches on "alpha". Multi-token is required to reach the
	// OR branch (len(tokens) > 1).
	out, err := d.Search(ctx, covID, "alpha zzzznomatch", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Search(OR-fallback) = %d hits, want 1", len(out))
	}
	if out[0].Path != skills.PathFTS5 {
		t.Errorf("path = %q, want %q (OR fallback stays on the FTS tier)", out[0].Path, skills.PathFTS5)
	}
}

// --- Guard branches: closed store + missing identity, every method ----------

func TestGuardBranches_ClosedStoreAndBadIdentity(t *testing.T) {
	// No t.Parallel(): shared-cache :memory: store.
	ctx := context.Background()
	badID := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u"}, // missing session
	}
	skill := skills.Skill{
		Name: "x", Trigger: "trg", Steps: []string{"s"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}

	// Missing-identity branch on every method.
	d := covDriver(t)
	if err := d.Upsert(ctx, badID, skill); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Errorf("Upsert(bad id) = %v, want ErrIdentityRequired", err)
	}
	if _, err := d.Get(ctx, badID, "x"); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Errorf("Get(bad id) = %v, want ErrIdentityRequired", err)
	}
	if _, err := d.List(ctx, badID, skills.ListFilter{}); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Errorf("List(bad id) = %v, want ErrIdentityRequired", err)
	}
	if _, err := d.Search(ctx, badID, "q", 5); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Errorf("Search(bad id) = %v, want ErrIdentityRequired", err)
	}
	if err := d.Delete(ctx, badID, "x"); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Errorf("Delete(bad id) = %v, want ErrIdentityRequired", err)
	}

	// Closed-store branch on every method.
	d2 := covDriver(t)
	if err := d2.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d2.Upsert(ctx, covID, skill); !errors.Is(err, skills.ErrStoreClosed) {
		t.Errorf("Upsert(closed) = %v, want ErrStoreClosed", err)
	}
	if _, err := d2.Get(ctx, covID, "x"); !errors.Is(err, skills.ErrStoreClosed) {
		t.Errorf("Get(closed) = %v, want ErrStoreClosed", err)
	}
	if _, err := d2.List(ctx, covID, skills.ListFilter{}); !errors.Is(err, skills.ErrStoreClosed) {
		t.Errorf("List(closed) = %v, want ErrStoreClosed", err)
	}
	if _, err := d2.Search(ctx, covID, "q", 5); !errors.Is(err, skills.ErrStoreClosed) {
		t.Errorf("Search(closed) = %v, want ErrStoreClosed", err)
	}
	if err := d2.Delete(ctx, covID, "x"); !errors.Is(err, skills.ErrStoreClosed) {
		t.Errorf("Delete(closed) = %v, want ErrStoreClosed", err)
	}
}
