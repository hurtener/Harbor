package sqlite

import (
	"strings"
	"testing"
)

// White-box tests for the small unexported helpers that are
// otherwise only reachable through the public API path. These pin
// the contract the conformance / integration tests depend on.

func TestAugmentDSNForPragmas_BarePathCommonCase(t *testing.T) {
	got, err := augmentDSNForPragmas("/tmp/state.sqlite")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasPrefix(got, "/tmp/state.sqlite?") {
		t.Errorf("expected `?` separator on bare path, got %q", got)
	}
	for _, want := range []string{"_pragma=", "busy_timeout", "journal_mode", "_txlock=immediate"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q missing %q", got, want)
		}
	}
}

func TestAugmentDSNForPragmas_BarePathWithExistingQuery(t *testing.T) {
	got, err := augmentDSNForPragmas("/tmp/state.sqlite?cache=shared")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Must use `&` to extend existing query string, not `?`.
	idx := strings.Index(got, "?")
	if idx < 0 || strings.Contains(got[idx+1:], "?") {
		t.Errorf("multiple `?` in augmented DSN: %q", got)
	}
	if !strings.Contains(got, "cache=shared") {
		t.Errorf("dropped existing query param: %q", got)
	}
}

func TestAugmentDSNForPragmas_FileURI(t *testing.T) {
	got, err := augmentDSNForPragmas("file:/tmp/state.sqlite?cache=shared")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasPrefix(got, "file:") {
		t.Errorf("dropped file: prefix: %q", got)
	}
	if !strings.Contains(got, "cache=shared") {
		t.Errorf("dropped existing query param: %q", got)
	}
	if !strings.Contains(got, "_txlock=immediate") {
		t.Errorf("missing _txlock=immediate: %q", got)
	}
}

func TestAugmentDSNForPragmas_MemorySentinel(t *testing.T) {
	got, err := augmentDSNForPragmas(":memory:")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{"file:", ":memory:", "cache=shared", "_pragma=", "_txlock=immediate"} {
		if !strings.Contains(got, want) {
			t.Errorf("memory DSN missing %q: got %q", want, got)
		}
	}
}

func TestAugmentDSNForPragmas_RespectsExistingTxlock(t *testing.T) {
	got, err := augmentDSNForPragmas("file:/tmp/x.sqlite?_txlock=exclusive")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(got, "_txlock=exclusive") {
		t.Errorf("caller-supplied _txlock=exclusive was overridden: %q", got)
	}
	if strings.Contains(got, "_txlock=immediate") {
		t.Errorf("we wrote _txlock=immediate over the caller's value: %q", got)
	}
}

func TestIsMemoryDSN(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{":memory:", true},
		{"file::memory:?cache=shared", true},
		{"file:/var/lib/state.sqlite", false},
		{"/tmp/state.sqlite", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isMemoryDSN(c.dsn); got != c.want {
			t.Errorf("isMemoryDSN(%q)=%v, want %v", c.dsn, got, c.want)
		}
	}
}

func TestListMigrations_OrdersByVersion(t *testing.T) {
	files, err := listMigrations()
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("listMigrations returned 0 files; expected at least 0001_init.sql")
	}
	for i := 1; i < len(files); i++ {
		if files[i].version <= files[i-1].version {
			t.Errorf("not sorted ascending at %d: %v", i, files)
		}
	}
	if files[0].version != 1 {
		t.Errorf("first migration version=%d, want 1", files[0].version)
	}
}
