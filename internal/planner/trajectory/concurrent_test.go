package trajectory_test

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestTrajectory_Serialize_ConcurrentReuse_D025 is the D-025
// concurrent-reuse gate for the trajectory subsystem (CLAUDE.md
// §5 + §11 + RFC §3.5).
//
// The contract requires N≥100 concurrent invocations against a single
// shared reusable artifact under -race with:
//
//   - No data races (the race detector is the gate).
//   - No context bleed (each goroutine recovers its own data).
//   - No cancellation cross-talk (N/A here — Serialize takes no ctx,
//     but the test still asserts independence under -race).
//   - No goroutine leaks (baseline runtime.NumGoroutine restored).
//
// N=128 (above the D-025 floor of 100; rounded for scheduler ease).
//
// Each goroutine serialises a per-goroutine Trajectory that carries
// its own goroutine ID in LLMContext. The shared element is the
// Trajectory's *type* and the encoder path — the actual Trajectory
// values are per-goroutine to prove the Serialize implementation
// (the walker + json.Marshal) is concurrent-safe across distinct
// inputs.
func TestTrajectory_Serialize_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	var (
		wg       sync.WaitGroup
		failures int64
		bleeds   int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			tr := &trajectory.Trajectory{
				Query: fmt.Sprintf("goroutine-%d", i),
				LLMContext: map[string]any{
					"goroutine_id": float64(i),
				},
				ToolContext: trajectory.ToolContext{
					Serializable: map[string]any{"i": float64(i)},
					Handles:      []trajectory.HandleID{trajectory.HandleID(fmt.Sprintf("h-%d", i))},
				},
			}
			bytes, err := tr.Serialize()
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			back, err := trajectory.Deserialize(bytes)
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			gotI, _ := back.LLMContext["goroutine_id"].(float64)
			if int(gotI) != i {
				atomic.AddInt64(&bleeds, 1)
			}
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Errorf("D-025: %d concurrent Serialize/Deserialize cycles failed", failures)
	}
	if bleeds != 0 {
		t.Errorf("D-025 context bleed: %d goroutines did not recover their own goroutine_id", bleeds)
	}

	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}

// TestHandleRegistry_ConcurrentReuse_D025 — the second D-025 gate:
// the HandleRegistry is a reusable artifact shared across N
// goroutines. Each goroutine Sets, Gets, and Deletes its own
// handle ID; no cross-talk is allowed.
func TestHandleRegistry_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	r := trajectory.NewProcessLocalRegistry()

	var (
		wg     sync.WaitGroup
		bleeds int64
		lost   int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := trajectory.HandleID(fmt.Sprintf("h-%d", i))
			want := fmt.Sprintf("value-%d", i)

			r.Set(id, want)
			got, err := r.Get(id)
			if err != nil {
				atomic.AddInt64(&lost, 1)
				return
			}
			s, ok := got.(string)
			if !ok || s != want {
				atomic.AddInt64(&bleeds, 1)
				return
			}
			r.Delete(id)
			// After Delete, Get must return ErrToolContextLost.
			_, err = r.Get(id)
			var notLost trajectory.ErrToolContextLost
			if !errors.As(err, &notLost) {
				atomic.AddInt64(&lost, 1)
			}
		}()
	}
	wg.Wait()

	if lost != 0 {
		t.Errorf("D-025: %d goroutines saw Get/post-Delete error path failures", lost)
	}
	if bleeds != 0 {
		t.Errorf("D-025 context bleed: %d goroutines saw another goroutine's value", bleeds)
	}

	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d", baseline, final)
	}
}

// TestHandleRegistry_SharedReader_ConcurrentReuse — a single shared
// handle is read by N concurrent goroutines (the more common runtime
// shape: one tool registers one callback, N planner steps look it
// up). Asserts no races, no inconsistent reads.
func TestHandleRegistry_SharedReader_ConcurrentReuse(t *testing.T) {
	const N = 128

	r := trajectory.NewProcessLocalRegistry()
	r.Set("shared", "the-shared-value")

	var (
		wg       sync.WaitGroup
		failures int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got, err := r.Get("shared")
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			if got.(string) != "the-shared-value" {
				atomic.AddInt64(&failures, 1)
			}
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Errorf("D-025: %d concurrent reads of shared handle failed", failures)
	}
}

// TestSerialize_SharedTrajectory_ConcurrentReuse — when the
// Trajectory itself is shared (read-only) across N goroutines all
// invoking Serialize, the result is byte-identical for every caller
// and no races trip.
func TestSerialize_SharedTrajectory_ConcurrentReuse(t *testing.T) {
	const N = 128

	tr := &trajectory.Trajectory{
		Query: "shared-query",
		LLMContext: map[string]any{
			"shared_key": "shared-value",
		},
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"k": "v"},
		},
	}

	// Pre-compute the reference bytes once on the calling goroutine.
	reference, err := tr.Serialize()
	if err != nil {
		t.Fatalf("reference Serialize err = %v", err)
	}

	var (
		wg        sync.WaitGroup
		mismatch  int64
		errResult int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got, err := tr.Serialize()
			if err != nil {
				atomic.AddInt64(&errResult, 1)
				return
			}
			if string(got) != string(reference) {
				atomic.AddInt64(&mismatch, 1)
			}
		}()
	}
	wg.Wait()

	if errResult != 0 {
		t.Errorf("D-025: %d concurrent Serialize calls returned errors", errResult)
	}
	if mismatch != 0 {
		t.Errorf("D-025 byte-stable invariant violated under concurrency: %d goroutines saw differing bytes", mismatch)
	}
}
