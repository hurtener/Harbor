package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func newCache(t *testing.T) SearchCache {
	t.Helper()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

func names(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// TestSearch_VisibilityAppliedBeforeLimit is the security regression for a
// process-wide index: many foreign rows outranking or preceding the caller's
// own must neither be returned nor consume the caller's result slots.
func TestSearch_VisibilityAppliedBeforeLimit(t *testing.T) {
	t.Parallel()
	sc := newCache(t)
	ctx := context.Background()
	var all []tools.Tool
	for i := range 40 {
		// Foreign rows mention the term twice so bm25 ranks them first.
		all = append(all, tools.Tool{Name: fmt.Sprintf("foreign%02d_send_mail", i), Description: "mail mail send", Source: "foreign"})
	}
	all = append(all,
		tools.Tool{Name: "own_send_mail", Description: "send mail", Source: "own"},
		tools.Tool{Name: "own_read_mail", Description: "read mail", Source: "own"},
	)
	if err := sc.Sync(ctx, all); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	own := func(name string) bool { return strings.HasPrefix(name, "own_") }
	for _, q := range []string{"mail", "send mail", "mai"} { // FTS AND, FTS, substring fallback
		got, err := sc.Search(ctx, tools.ToolSearchRequest{Query: q, Limit: 10, Visible: own})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		for _, n := range names(got) {
			if !own(n) {
				t.Fatalf("Search(%q) returned a row the caller cannot see: %v", q, names(got))
			}
		}
		if q != "send mail" && len(got) != 2 {
			t.Fatalf("Search(%q) = %v, want both own rows despite 40 foreign matches", q, names(got))
		}
	}
	// A nil predicate admits nothing.
	if got, err := sc.Search(ctx, tools.ToolSearchRequest{Query: "mail", Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("nil Visible = %v, %v; want no rows", names(got), err)
	}
}

func TestSearch_RanksByRelevance(t *testing.T) {
	t.Parallel()
	sc := newCache(t)
	ctx := context.Background()
	if err := sc.Sync(ctx, []tools.Tool{
		{Name: "a_archive", Description: "Archive files; can also attach a calendar note"},
		{Name: "z_calendar_create", Description: "Create a calendar event"},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got, err := sc.Search(ctx, req("calendar", nil, 10))
	if err != nil || len(got) != 2 {
		t.Fatalf("Search = %v, %v", names(got), err)
	}
	if got[0].Name != "z_calendar_create" {
		t.Fatalf("ranking = %v, want the name match first", names(got))
	}
}

func TestSearch_MatchesServerTitleAndDescription(t *testing.T) {
	t.Parallel()
	sc := newCache(t)
	ctx := context.Background()
	if err := sc.Sync(ctx, []tools.Tool{
		{Name: "srv-1_list_items", Description: "List items", Source: "srv-1", SourceTitle: "Acme Mailbox", SourceDescription: "Electronic correspondence"},
		{Name: "srv-2_list_items", Description: "List items", Source: "srv-2", SourceTitle: "Weather"},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, q := range []string{"mailbox", "correspondence", "acme items", "Mailb"} {
		got, err := sc.Search(ctx, req(q, nil, 10))
		if err != nil || len(got) != 1 || got[0].Name != "srv-1_list_items" || got[0].SourceTitle != "Acme Mailbox" {
			t.Fatalf("Search(%q) = %+v, %v; want the Acme Mailbox tool", q, got, err)
		}
	}
}

func TestRemove_DeletesRowsAndFTSEntries(t *testing.T) {
	t.Parallel()
	sc := newCache(t)
	ctx := context.Background()
	if err := sc.Sync(ctx, []tools.Tool{{Name: "gone_tool", Description: "ephemeral"}, {Name: "kept_tool", Description: "ephemeral"}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := sc.Remove(ctx, []string{"gone_tool", "never_existed"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := sc.Search(ctx, req("ephemeral", nil, 10))
	if err != nil || len(got) != 1 || got[0].Name != "kept_tool" {
		t.Fatalf("after Remove: %v, %v", names(got), err)
	}
	got, err = sc.Search(ctx, req("gone", nil, 10))
	if err != nil || len(got) != 0 {
		t.Fatalf("removed row still searchable: %v, %v", names(got), err)
	}
}

func TestSync_BoundsServerMetadata(t *testing.T) {
	t.Parallel()
	sc := newCache(t)
	ctx := context.Background()
	long := strings.Repeat("é", 4000)
	if err := sc.Sync(ctx, []tools.Tool{{Name: "big", SourceTitle: long, SourceDescription: long}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got, err := sc.Search(ctx, req("", nil, 10))
	if err != nil || len(got) != 1 {
		t.Fatalf("Search: %v, %v", got, err)
	}
	if len(got[0].SourceTitle) > maxServerTitleBytes || len(got[0].SourceDescription) > maxServerDescriptionBytes {
		t.Fatalf("server metadata not bounded: %d/%d bytes", len(got[0].SourceTitle), len(got[0].SourceDescription))
	}
	if !strings.HasPrefix(long, got[0].SourceTitle) {
		t.Fatal("truncation split a rune")
	}
}

// TestNew_FileDSN_MigratesV1AndDropsPriorProcessRows proves an on-disk index
// written by the version-1 schema migrates forward and that rows a previous
// process persisted never survive into a new process's index.
func TestNew_FileDSN_MigratesV1AndDropsPriorProcessRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tools.db")
	dsn := "file:" + path
	// Seed a version-1 database exactly as the old binary left it.
	raw, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	v1, err := migrationsFS.ReadFile("migrations/0001_tools_search.sql")
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS tool_cache_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("v1 migrations table: %v", err)
	}
	if _, err := raw.Exec(string(v1)); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tool_cache(name, description) VALUES ('stale_tool', 'from a previous process')`); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	_ = raw.Close()

	sc, err := New(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New over v1 database: %v", err)
	}
	ctx := context.Background()
	if got, err := sc.Search(ctx, req("previous", nil, 10)); err != nil || len(got) != 0 {
		t.Fatalf("prior-process row survived: %v, %v", names(got), err)
	}
	if err := sc.Sync(ctx, []tools.Tool{{Name: "fresh_tool", Description: "current", SourceTitle: "Server", ArgsSchema: json.RawMessage(`{}`)}}); err != nil {
		t.Fatalf("Sync after migration: %v", err)
	}
	if got, err := sc.Search(ctx, req("server", nil, 10)); err != nil || len(got) != 1 {
		t.Fatalf("migrated schema does not index server title: %v, %v", names(got), err)
	}
	_ = sc.Close()
	// Re-opening is idempotent and again starts from an empty mirror.
	sc2, err := New(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() { _ = sc2.Close() }()
	if got, err := sc2.Search(ctx, req("current", nil, 10)); err != nil || len(got) != 0 {
		t.Fatalf("rows survived re-open: %v, %v", names(got), err)
	}
}
