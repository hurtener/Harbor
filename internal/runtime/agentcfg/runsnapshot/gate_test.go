package runsnapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGate_SealDrainsOnlyMatchingTenantAgentAndPermanentlyRefuses(t *testing.T) {
	g := NewGate()
	lease, err := g.Acquire(t.Context(), "tenant-a", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := g.Acquire(t.Context(), "tenant-b", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := g.Seal("tenant-a", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire(t.Context(), "tenant-a", "agent-a"); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("post-seal admission = %v, want ErrAdmissionClosed", err)
	}
	if independent, err := g.Acquire(t.Context(), "tenant-a", "agent-b"); err != nil {
		t.Fatalf("other agent admission: %v", err)
	} else {
		independent.Release()
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := drain.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait with held matching lease = %v, want deadline", err)
	}
	other.Release()
	lease.Release()
	lease.Release()
	if err := drain.Wait(t.Context()); err != nil {
		t.Fatalf("retry wait after release: %v", err)
	}
	if _, err := g.Acquire(t.Context(), "tenant-a", "agent-a"); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("terminal gate reopened after drain: %v", err)
	}
}

func TestGate_ConcurrentSealAndReleaseDoesNotLoseLease(t *testing.T) {
	g := NewGate()
	const workers = 128
	leases := make([]*Lease, 0, workers)
	for range workers {
		lease, err := g.Acquire(t.Context(), "tenant", "agent")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	drain, err := g.Seal("tenant", "agent")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, lease := range leases {
		wg.Add(1)
		go func(l *Lease) {
			defer wg.Done()
			l.Release()
		}(lease)
	}
	wg.Wait()
	if err := drain.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}
