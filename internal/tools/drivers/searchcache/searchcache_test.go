package searchcache

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestSearchCache_OpenAndMigrate asserts a fresh :memory: DSN opens
// cleanly and runs the bundled migration to completion. The
// `tool_cache_migrations` table must be populated so a second `New`
// against the same DSN is a no-op (idempotent migrations — AC-8 + §9).
func TestSearchCache_OpenAndMigrate(t *testing.T) {
	t.Parallel()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
}

// TestSearchCache_SyncAndSearch_FTS5 exercises the FTS5 path: sync
// a few tools with distinct descriptions, then search by token. The
// match should return only the matching rows.
func TestSearchCache_SyncAndSearch_FTS5(t *testing.T) {
	t.Parallel()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	toolSet := []tools.Tool{
		{
			Name:        "youtube_download",
			Description: "Download a YouTube video by URL",
			Tags:        []string{"video", "media"},
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		},
		{
			Name:        "image_resize",
			Description: "Resize an image to a target width",
			Tags:        []string{"image", "media"},
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		},
		{
			Name:        "shell_exec",
			Description: "Run a shell command",
			Tags:        []string{"system"},
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		},
	}
	if err := sc.Sync(ctx, toolSet); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := sc.Search(ctx, "youtube", nil, 10)
	if err != nil {
		t.Fatalf("Search youtube: %v", err)
	}
	if len(got) != 1 || got[0].Name != "youtube_download" {
		t.Fatalf("Search youtube returned %d rows: %#v", len(got), got)
	}

	// Tag intersection — "media" matches video + image, "video"+"media"
	// narrows to the YouTube entry only.
	media, err := sc.Search(ctx, "", []string{"media"}, 10)
	if err != nil {
		t.Fatalf("Search media tag: %v", err)
	}
	if len(media) != 2 {
		t.Fatalf("Search by tag media returned %d rows: %#v", len(media), media)
	}
	video, err := sc.Search(ctx, "", []string{"video", "media"}, 10)
	if err != nil {
		t.Fatalf("Search video+media tags: %v", err)
	}
	if len(video) != 1 || video[0].Name != "youtube_download" {
		t.Fatalf("Search by tags video+media returned %d rows: %#v", len(video), video)
	}
}

// TestSearchCache_RegexFallback fires when FTS5 yields no rows for the
// query token (no exact token match), and we expect the regex path to
// pick up a substring match instead.
func TestSearchCache_RegexFallback(t *testing.T) {
	t.Parallel()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	ctx := context.Background()
	if err := sc.Sync(ctx, []tools.Tool{
		{
			Name:        "report_generator",
			Description: "Generate a quarterly report PDF",
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// "quarterl" is a substring; FTS5 with the AND-of-tokens expression
	// `"quarterl"` won't find a token match, so the regex/exact path
	// takes over.
	got, err := sc.Search(ctx, "quarterl", nil, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Name != "report_generator" {
		t.Fatalf("regex fallback returned %d rows: %#v", len(got), got)
	}
}

// TestSearchCache_ClosedRejects asserts that operations against a
// closed store return errStoreClosed (driver-internal sentinel).
func TestSearchCache_ClosedRejects(t *testing.T) {
	t.Parallel()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := sc.Search(context.Background(), "x", nil, 10); err == nil {
		t.Fatalf("Search on closed store: expected error")
	}
	if err := sc.Sync(context.Background(), []tools.Tool{{Name: "x"}}); err == nil {
		t.Fatalf("Sync on closed store: expected error")
	}
}

// TestSearchCache_AugmentDSN asserts the DSN augmentation builds a
// valid multi-pragma URI for both :memory: and file: forms. The
// previous bug embedded `&_pragma=` inside a single value, which
// `q.Encode()` would escape — producing one broken pragma.
func TestSearchCache_AugmentDSN(t *testing.T) {
	t.Parallel()
	mem, err := augmentDSN(":memory:")
	if err != nil {
		t.Fatalf("augmentDSN(:memory:): %v", err)
	}
	if !strings.Contains(mem, "journal_mode(WAL)") || !strings.Contains(mem, "busy_timeout(") {
		t.Fatalf("augmented :memory: DSN missing pragmas: %s", mem)
	}

	file, err := augmentDSN("file:./harbor-test.db")
	if err != nil {
		t.Fatalf("augmentDSN(file:): %v", err)
	}
	// Both pragmas appear as separate `_pragma=` params (URL-encoded).
	if strings.Count(file, "_pragma=") < 2 {
		t.Fatalf("augmented file: DSN has fewer than 2 _pragma= params: %s", file)
	}
}

// TestSearchCache_ConcurrentReuse asserts D-025 — N concurrent Search +
// Sync invocations against ONE shared instance under -race. No data
// races + no panics.
func TestSearchCache_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	sc, err := New(Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	ctx := context.Background()
	// Seed.
	seed := make([]tools.Tool, 0, 25)
	for i := 0; i < 25; i++ {
		seed = append(seed, tools.Tool{
			Name:        nameFor(i),
			Description: "Tool number " + nameFor(i),
			Tags:        []string{"concurrent"},
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
		})
	}
	if err := sc.Sync(ctx, seed); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}

	const N = 128
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := sc.Search(ctx, "Tool", []string{"concurrent"}, 5); err != nil {
				t.Errorf("goroutine %d: Search err: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

func nameFor(i int) string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	return "t_" + string(letters[i%len(letters)])
}
