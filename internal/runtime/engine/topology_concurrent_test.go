package engine

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// TestTopology_ConcurrentReuse pins the D-025 concurrent-reuse contract
// for the Phase 74 Topology accessor: N=128 goroutines call
// engine.Topology(ctx) against ONE shared engine under -race. The test
// asserts:
//
//   - no data races (the -race detector is the gate);
//   - no field tearing — every call returns byte-identical projection
//     JSON (the engine is idle, so the projection is stable);
//   - no context bleed — each goroutine drives its own identity triple
//     and gets a valid projection back;
//   - no goroutine leak — baseline runtime.NumGoroutine restored after
//     all calls return.
//
// The engine is a compiled artifact: engineID + nodes + adjs + channels
// are all set once at New and never mutated. Topology reads only those
// + ctx; it holds no per-call state on the engine.
func TestTopology_ConcurrentReuse(t *testing.T) {
	const n = 128 // ≥100 per the D-025 contract

	noop := func(_ context.Context, env messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		return env, nil
	}
	in := Node{Name: "in", Func: noop}
	mid := Node{Name: "mid", Func: noop}
	out := Node{Name: "out", Func: noop}
	eng, err := New([]Adjacency{
		{From: in, To: []Node{mid}},
		{From: mid, To: []Node{out}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Reference projection — every concurrent call must match its JSON
	// (modulo OccurredAt, which we zero before comparing).
	refCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "ref", UserID: "ref", SessionID: "ref",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ref, err := eng.Topology(refCtx)
	if err != nil {
		t.Fatalf("reference Topology: %v", err)
	}
	ref.OccurredAt = time.Time{}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal reference: %v", err)
	}

	// Settle before snapshotting the goroutine baseline.
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine drives a distinct identity triple — a
			// context bleed would surface as a wrong projection or a
			// validation failure.
			ctx, cerr := identity.With(context.Background(), identity.Identity{
				TenantID:  "tenant-" + itoa(i),
				UserID:    "user-" + itoa(i),
				SessionID: "session-" + itoa(i),
			})
			if cerr != nil {
				errs <- "identity.With: " + cerr.Error()
				return
			}
			proj, perr := eng.Topology(ctx)
			if perr != nil {
				errs <- "Topology: " + perr.Error()
				return
			}
			proj.OccurredAt = time.Time{}
			gotJSON, merr := json.Marshal(proj)
			if merr != nil {
				errs <- "marshal: " + merr.Error()
				return
			}
			if string(gotJSON) != string(refJSON) {
				errs <- "projection drift / field tearing across concurrent calls"
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// No goroutine leak: baseline restored once every call returned.
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+5 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// itoa is a tiny dependency-free int→string for the test's identity
// strings (strconv would do; this keeps the test self-contained).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
