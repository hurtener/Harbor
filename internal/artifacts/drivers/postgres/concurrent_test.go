package postgres_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
)

// TestPostgres_Concurrent_SingleSchema — supplemental concurrent
// reuse test (D-025) that complements the conformance suite's
// `Concurrent_PutGet_NoRace` (N=128 across many identities). This
// test stresses the Postgres-specific contention path: N goroutines
// hammering Put/Get/Delete on a SINGLE shared driver against a
// SINGLE schema. The point is to flush out:
//
//   - Lock-ordering bugs in the INSERT path.
//   - Connection-pool exhaustion under hot contention.
//   - Goroutine leaks from prepared-statement caching, etc.
//
// The conformance suite's stress test exercises N=128 across a wide
// identity surface; here N=64 against a tighter identity surface so
// the per-row contention is higher.
func TestPostgres_Concurrent_SingleSchema(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	baseline := runtime.NumGoroutine()

	const goroutines = 64
	const opsPerGo = 8

	var wg sync.WaitGroup
	var errs atomic.Int64
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			scope := artifacts.ArtifactScope{
				// Tighter identity surface than the conformance test
				// — i%4 buckets so contention is higher per-slot.
				TenantID:  fmt.Sprintf("t-%d", i%4),
				UserID:    fmt.Sprintf("u-%d", i%4),
				SessionID: fmt.Sprintf("s-%d", i%8),
				TaskID:    fmt.Sprintf("k-%d", i%4),
			}
			for j := range opsPerGo {
				payload := []byte(fmt.Sprintf("payload-%d-%d", i, j))
				ref, err := s.PutBytes(ctx, scope, payload,
					artifacts.PutOpts{Namespace: fmt.Sprintf("ns-%d", j%3)})
				if err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Put: %v", i, j, err)
					return
				}
				got, found, err := s.Get(ctx, scope, ref.ID)
				if err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Get: %v", i, j, err)
					return
				}
				if !found || string(got) != string(payload) {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Get: got=%q found=%v want=%q",
						i, j, got, found, payload)
					return
				}
				if j%4 == 0 {
					if _, err := s.Delete(ctx, scope, ref.ID); err != nil {
						errs.Add(1)
						t.Errorf("goroutine %d op %d Delete: %v", i, j, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errs.Load(); n != 0 {
		t.Fatalf("%d concurrent operations errored", n)
	}

	// Goroutine-leak check: stage any background pool tear-down.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		// Postgres connection pool may keep a few idle connections
		// around — that's bounded by SetMaxIdleConns(5) and is not a
		// leak. Allow a small slack window.
		const slack = 12
		if delta > slack {
			t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
		}
	}
}

// TestPostgres_Concurrent_DedupUnderContention proves the dedup
// contract holds under contention: N goroutines Put-ing identical
// (scope, namespace, bytes) all return the SAME content-addressed id.
// ON CONFLICT DO NOTHING + post-INSERT SELECT must serialize correctly.
func TestPostgres_Concurrent_DedupUnderContention(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
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
