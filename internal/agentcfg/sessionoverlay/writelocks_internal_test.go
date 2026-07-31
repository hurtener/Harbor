package sessionoverlay

import (
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// quadFor builds a distinct session triple per index — the shape an
// authenticated caller uses to reach a fresh write-lock slot on every call.
func quadFor(i int) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  "tenant-locks",
		UserID:    "user-locks",
		SessionID: fmt.Sprintf("sess-%04d", i),
	}}
}

func (s *store) lockCount() int {
	s.writeLocksMu.Lock()
	defer s.writeLocksMu.Unlock()
	return len(s.writeLocks)
}

// TestLockSlot_ReleasesTheSlotEntry is the bound: N sequential write-lock
// acquisitions under N DISTINCT slots leave ZERO entries behind, where an
// append-only map would hold all N for the lifetime of the process.
//
// Mutation that turns this red: drop the `delete(s.writeLocks, key)` from
// the release func.
func TestLockSlot_ReleasesTheSlotEntry(t *testing.T) {
	const n = 512
	s := &store{writeLocks: make(map[string]*slotLock)}
	for i := range n {
		release := s.lockSlot(quadFor(i), "agent-x")
		release()
	}
	if got := s.lockCount(); got != 0 {
		t.Fatalf("write-lock map holds %d entries after %d released acquisitions, want 0", got, n)
	}
}

// TestLockSlot_KeepsTheEntryWhileHeld proves the entry is NOT dropped while
// a holder still references it — the refcount is what makes the delete safe,
// so a delete that ignored it would be the bug this test names.
func TestLockSlot_KeepsTheEntryWhileHeld(t *testing.T) {
	s := &store{writeLocks: make(map[string]*slotLock)}
	release := s.lockSlot(quadFor(0), "agent-x")
	if got := s.lockCount(); got != 1 {
		t.Fatalf("write-lock map holds %d entries while a lock is held, want 1", got)
	}
	release()
	if got := s.lockCount(); got != 0 {
		t.Fatalf("write-lock map holds %d entries after release, want 0", got)
	}
}

// TestLockSlot_SerialisesConcurrentHoldersOfOneSlot proves the refcounted
// map still provides MUTUAL EXCLUSION per slot: N concurrent acquirers of
// the SAME slot never overlap, and the entry is gone once they all release.
//
// This is the guard against a "fix" that bounded the map by deleting the
// entry on every release regardless of waiters: a waiter would then be
// handed a different mutex than the holder released, the critical sections
// would overlap, and the counter below would exceed 1 under -race.
func TestLockSlot_SerialisesConcurrentHoldersOfOneSlot(t *testing.T) {
	const holders = 128
	s := &store{writeLocks: make(map[string]*slotLock)}
	id := quadFor(0)

	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
		wg      sync.WaitGroup
	)
	for range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := s.lockSlot(id, "agent-x")
			mu.Lock()
			inside++
			if inside > maxSeen {
				maxSeen = inside
			}
			mu.Unlock()

			mu.Lock()
			inside--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("%d goroutines were inside one slot's critical section at once, want 1 — the per-slot lock stopped excluding", maxSeen)
	}
	if got := s.lockCount(); got != 0 {
		t.Fatalf("write-lock map holds %d entries after every holder released, want 0", got)
	}
}

// TestLockSlot_DistinctSlotsDoNotContend proves the bound did not
// accidentally collapse the per-slot keying into one global lock: two
// distinct slots are held simultaneously, which deadlocks if they share one.
func TestLockSlot_DistinctSlotsDoNotContend(t *testing.T) {
	s := &store{writeLocks: make(map[string]*slotLock)}
	releaseA := s.lockSlot(quadFor(0), "agent-x")
	releaseB := s.lockSlot(quadFor(1), "agent-x")
	if got := s.lockCount(); got != 2 {
		t.Fatalf("write-lock map holds %d entries with two distinct slots held, want 2", got)
	}
	releaseB()
	releaseA()
	if got := s.lockCount(); got != 0 {
		t.Fatalf("write-lock map holds %d entries after both released, want 0", got)
	}
}
