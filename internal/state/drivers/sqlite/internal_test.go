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
	// D-207: the sentinel translates to a per-Open uniquely NAMED
	// memory URI (mode=memory + cache=shared) — no longer the
	// process-wide `file::memory:` database every subsystem shared.
	for _, want := range []string{"file:harbor_state_mem_", "mode=memory", "cache=shared", "_pragma=", "_txlock=immediate"} {
		if !strings.Contains(got, want) {
			t.Errorf("memory DSN missing %q: got %q", want, got)
		}
	}
	// Per-Open isolation: a second translation must mint a DIFFERENT
	// database name.
	again, err := augmentDSNForPragmas(":memory:")
	if err != nil {
		t.Fatalf("second augment err=%v", err)
	}
	if got == again {
		t.Errorf("two :memory: opens produced the same DSN (%q) — per-Open isolation lost", got)
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

func TestAugmentDSNForPragmas_RejectsUnsafeTxlock(t *testing.T) {
	for _, dsn := range []string{
		"file:/tmp/x.sqlite?_txlock=deferred",
		"/tmp/x.sqlite?_txlock=deferred",
		"file:/tmp/x.sqlite?_txlock=immediate&_txlock=exclusive",
	} {
		if _, err := augmentDSNForPragmas(dsn); err == nil {
			t.Errorf("augmentDSNForPragmas(%q) succeeded, want unsafe _txlock rejection", dsn)
		}
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

// (TestListMigrations_OrdersByVersion removed in the sqlmigrate
// extraction — the filename-parse + version-ordering logic now lives in
// internal/persistence/sqlmigrate and is covered by its own tests; this
// driver's end-to-end behaviour is covered by migration_test.go +
// the state conformance suite.)
