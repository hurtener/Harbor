package s3_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	s3driver "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
)

// TestS3_Concurrent_PutGet_NoRace — supplemental S3-specific
// concurrent-reuse stress (D-025). The conformance suite already runs
// `Concurrent_PutGet_NoRace` at N=128 across a wide identity surface;
// this test stresses a tighter identity surface at N=32 (lower than
// the other drivers because S3 is rate-limited; the SDK default retry
// policy handles bursts but stress-testing harder serves no purpose
// per the Phase 19 plan).
//
// Asserts:
//   - No data races (the `-race` flag is the gate).
//   - No errored operations (per-goroutine atomic counter).
//   - No goroutine leak (baseline restored after all runs return).
//
// The SDK keeps a small pool of idle HTTP connections after the burst
// settles; we allow a slack window of 32 goroutines to absorb that.
func TestS3_Concurrent_PutGet_NoRace(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	baseline := runtime.NumGoroutine()

	const goroutines = 32
	const opsPerGo = 4

	var wg sync.WaitGroup
	var errs atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			scope := artifacts.ArtifactScope{
				TenantID:  fmt.Sprintf("t-%d", i%4),
				UserID:    fmt.Sprintf("u-%d", i%4),
				SessionID: fmt.Sprintf("s-%d", i%8),
				TaskID:    fmt.Sprintf("k-%d", i),
			}
			for j := 0; j < opsPerGo; j++ {
				data := []byte(fmt.Sprintf("payload-%d-%d", i, j))
				ref, err := s.PutBytes(ctx, scope, data, artifacts.PutOpts{
					Namespace: fmt.Sprintf("ns-%d", j%2),
				})
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
				if !found || string(got) != string(data) {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Get mismatch: found=%v got=%q want=%q",
						i, j, found, got, data)
					return
				}
				exists, err := s.Exists(ctx, scope, ref.ID)
				if err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Exists: %v", i, j, err)
					return
				}
				if !exists {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Exists=false after Put", i, j)
					return
				}
				if j%2 == 0 {
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

	// Goroutine-leak check: stage any background HTTP connection-pool
	// tear-down. The SDK keeps a small pool of idle connections; allow
	// a slack window so a few keep-alive workers don't trip the assert.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		const slack = 32
		if delta > slack {
			t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)",
				baseline, runtime.NumGoroutine(), delta)
		}
	}
}
