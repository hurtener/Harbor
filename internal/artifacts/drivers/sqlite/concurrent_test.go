package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/config"
)

// TestSQLite_Concurrent_BusyTimeoutAbsorbsContention is the
// supplemental driver-local concurrency test mandated by the Phase 18
// plan ("concurrent-reuse contract — mandatory" — D-025 + AGENTS.md
// §5). The conformance suite's `Concurrent_PutGet_NoRace` already
// runs N=128 goroutines against the driver; this test runs an
// additional N≥64 cohort that hammers the file-backed write path
// specifically to assert that `SQLITE_BUSY` retries are absorbed by
// the `busy_timeout=5000` PRAGMA and do NOT escape as caller-visible
// errors.
//
// What we prove:
//
//   - No data races (run under -race).
//   - No caller-visible errors despite contended writes.
//   - No goroutine leak after the cohort returns.
//   - Reads of recently-written ids observe the correct payload.
func TestSQLite_Concurrent_BusyTimeoutAbsorbsContention(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "concurrent.sqlite")

	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	const goroutines = 64
	const opsPerGo = 12

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var putErrs atomic.Int64
	var getErrs atomic.Int64
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			scope := artifacts.ArtifactScope{
				TenantID:  fmt.Sprintf("t-%d", i%7),
				UserID:    fmt.Sprintf("u-%d", i%11),
				SessionID: fmt.Sprintf("s-%d", i),
				TaskID:    fmt.Sprintf("k-%d", i%5),
			}
			for j := range opsPerGo {
				payload := []byte(fmt.Sprintf("payload-%d-%d", i, j))
				ref, err := s.PutBytes(ctx, scope, payload,
					artifacts.PutOpts{Namespace: fmt.Sprintf("ns-%d", j%3)})
				if err != nil {
					putErrs.Add(1)
					t.Errorf("Put (g=%d j=%d): %v", i, j, err)
					return
				}
				got, found, err := s.Get(ctx, scope, ref.ID)
				if err != nil {
					getErrs.Add(1)
					t.Errorf("Get (g=%d j=%d): %v", i, j, err)
					return
				}
				if !found || string(got) != string(payload) {
					getErrs.Add(1)
					t.Errorf("Get (g=%d j=%d): got=%q found=%v want=%q",
						i, j, got, found, payload)
				}
			}
		}()
	}
	wg.Wait()

	if n := putErrs.Load(); n != 0 {
		t.Fatalf("%d Put errors leaked despite busy_timeout", n)
	}
	if n := getErrs.Load(); n != 0 {
		t.Fatalf("%d Get errors", n)
	}

	// Goroutine-leak check (D-025).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestSQLite_Concurrent_DedupUnderContention proves the dedup
// contract holds under contention: N goroutines Put-ing identical
// (scope, namespace, bytes) all return the SAME content-addressed id,
// and the resulting List shows exactly one row. ON CONFLICT DO
// NOTHING + post-INSERT SELECT must serialize correctly.
func TestSQLite_Concurrent_DedupUnderContention(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "dedup.sqlite")

	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	const goroutines = 64
	scope := artifacts.ArtifactScope{
		TenantID: "shared", UserID: "shared", SessionID: "shared", TaskID: "shared",
	}
	payload := []byte("identical-bytes-under-contention")
	opts := artifacts.PutOpts{Namespace: "dedup"}

	var wg sync.WaitGroup
	ids := make([]string, goroutines)
	var errs atomic.Int64
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			ref, err := s.PutBytes(context.Background(), scope, payload, opts)
			if err != nil {
				errs.Add(1)
				t.Errorf("Put: %v", err)
				return
			}
			ids[i] = ref.ID
		}()
	}
	wg.Wait()

	if n := errs.Load(); n != 0 {
		t.Fatalf("%d Put errors under dedup contention", n)
	}

	expected := ids[0]
	for i, id := range ids {
		if id != expected {
			t.Errorf("dedup race: ids[%d]=%q, ids[0]=%q", i, id, expected)
		}
	}

	got, err := s.List(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("List len=%d after dedup contention; want 1", len(got))
	}
}
