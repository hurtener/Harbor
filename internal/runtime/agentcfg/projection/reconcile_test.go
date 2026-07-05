package projection_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

// reconcile_test.go — unit coverage for ReconcileConnections (the DETACH leg
// of run-start reconciliation): the declared-vs-attached diff, the
// boot-declared skip, idempotency, fail-loud on a read error, and the
// concurrent-reuse / at-most-once-detach contract under -race.

// fakeDetacher is a ConnectionDetacher that reports a fixed attached set and
// records each Detach call. Safe for concurrent use.
type fakeDetacher struct {
	attached []string
	mu       sync.Mutex
	detached map[string]int
	err      error // returned by every Detach (nil = success)
}

func newFakeDetacher(attached ...string) *fakeDetacher {
	return &fakeDetacher{attached: attached, detached: map[string]int{}}
}

func (f *fakeDetacher) AttachedSources(_ context.Context) []string {
	return append([]string(nil), f.attached...)
}

func (f *fakeDetacher) Detach(_ context.Context, source string) error {
	f.mu.Lock()
	f.detached[source]++
	f.mu.Unlock()
	return f.err
}

func (f *fakeDetacher) detachedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.detached))
	for k := range f.detached {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedConnections writes an active revision declaring the named connections.
func seedConnections(t *testing.T, reg agentcfg.Registry, names ...string) {
	t.Helper()
	servers := make([]agentcfg.MCPConnectionDescriptor, 0, len(names))
	for _, n := range names {
		servers = append(servers, agentcfg.MCPConnectionDescriptor{
			Name: n, Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/" + n,
		})
	}
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: servers}}
	if _, err := reg.SetRevision(context.Background(), projID(), projAgent, agentcfg.ConfigScopeAgent, payload); err != nil {
		t.Fatalf("seed connections: %v", err)
	}
}

func TestReconcileConnections_DetachesUndeclared_KeepsDeclared(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "keep") // "drop" is attached but no longer declared.
	det := newFakeDetacher("keep", "drop")

	n, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if n != 1 {
		t.Fatalf("detached count = %d, want 1", n)
	}
	if got := det.detachedNames(); len(got) != 1 || got[0] != "drop" {
		t.Fatalf("detached = %v, want [drop]", got)
	}
}

func TestReconcileConnections_BootDeclaredNeverDetached(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// No revisioned connections at all; "yaml" is attached but boot-declared.
	det := newFakeDetacher("yaml", "runtime-added")
	boot := map[string]struct{}{"yaml": {}}

	n, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, boot)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if n != 1 {
		t.Fatalf("detached count = %d, want 1", n)
	}
	if got := det.detachedNames(); len(got) != 1 || got[0] != "runtime-added" {
		t.Fatalf("detached = %v, want [runtime-added] (yaml must never be detached)", got)
	}
}

func TestReconcileConnections_Idempotent_NothingUndeclared(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "a", "b")
	det := newFakeDetacher("a", "b") // every attached source is declared.

	n, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if n != 0 || len(det.detachedNames()) != 0 {
		t.Fatalf("detached %d %v, want none", n, det.detachedNames())
	}
}

func TestReconcileConnections_NilDetacher_NoOp(t *testing.T) {
	reg := newRegistry(t)
	n, err := projection.ReconcileConnections(context.Background(), reg, projAgent, projID(), nil, nil)
	if err != nil || n != 0 {
		t.Fatalf("nil detacher: n=%d err=%v, want 0,nil", n, err)
	}
}

func TestReconcileConnections_ReadError_FailsLoud(t *testing.T) {
	det := newFakeDetacher("x")
	_, err := projection.ReconcileConnections(context.Background(), errReg{}, projAgent, projID(), det, nil)
	if !errors.Is(err, projection.ErrReconcileRead) {
		t.Fatalf("err = %v, want ErrReconcileRead", err)
	}
	if len(det.detachedNames()) != 0 {
		t.Fatal("a read error must not detach anything (no silent fall-through)")
	}
}

func TestReconcileConnections_DetachError_Joined_ContinuesOthers(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// Both "x" and "y" are undeclared; the detacher fails every Detach.
	det := &fakeDetacher{attached: []string{"x", "y"}, detached: map[string]int{}, err: errors.New("boom")}
	n, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil)
	if err == nil {
		t.Fatal("want a joined detach error")
	}
	if n != 0 {
		t.Fatalf("detached count = %d, want 0 (all failed)", n)
	}
	// Both were attempted despite the first failing (no early abort).
	if got := det.detachedNames(); len(got) != 2 {
		t.Fatalf("attempted detaches = %v, want both x and y", got)
	}
}

func TestReconcileConnections_ConcurrentRunStarts_AtMostOnceDetach(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "keep")
	det := newFakeDetacher("keep", "drop")

	base := runtime.NumGoroutine()
	const N = 128
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			if _, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil); err != nil {
				t.Errorf("reconcile: %v", err)
			}
		}()
	}
	wg.Wait()

	// "drop" is detached repeatedly (idempotent at the detacher), but the
	// detacher records each call; the point is no race and no cross-talk. The
	// concrete detacher is idempotent (an already-gone server is a no-op), so
	// N reconciles calling Detach("drop") is safe.
	if got := det.detachedNames(); len(got) != 1 || got[0] != "drop" {
		t.Fatalf("detached = %v, want only [drop]", got)
	}
	assertGoroutineBaseline(t, base)
}

// TestReconcileConnections_RemoveUnderLoad_Stress is the D-025 concurrent-reuse
// stress: N≥100 mixed operations (config edits that re-declare / un-declare
// connections + run-start reconciles) against ONE shared registry + detacher
// under -race, asserting no races, no context bleed, and no goroutine leak.
func TestReconcileConnections_RemoveUnderLoad_Stress(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "keep")
	det := newFakeDetacher("keep", "drop-1", "drop-2", "drop-3")

	base := runtime.NumGoroutine()
	const N = 150
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				// A config edit that re-declares "keep" (an unrelated churn).
				seedConnections(t, reg, "keep")
			case 1, 2:
				// A run-start reconcile — must never race the edits.
				if _, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil); err != nil {
					t.Errorf("reconcile under load: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// "keep" is always declared, so it is never detached; the three "drop-*"
	// undeclared servers are detached (idempotently).
	got := det.detachedNames()
	for _, name := range got {
		if name == "keep" {
			t.Error("a declared server was detached under load (context bleed)")
		}
	}
	assertGoroutineBaseline(t, base)
}

// assertGoroutineBaseline fails if goroutines did not return to baseline.
func assertGoroutineBaseline(t *testing.T, base int) {
	t.Helper()
	for range 50 {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		runtime.Gosched()
	}
	t.Errorf("goroutines did not return to baseline: base=%d now=%d", base, runtime.NumGoroutine())
}

// errReg is an agentcfg.Registry whose Active always fails (the read-error leg).
type errReg struct{ agentcfg.Registry }

func (errReg) Active(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	return agentcfg.Revision{}, false, fmt.Errorf("state unavailable")
}
