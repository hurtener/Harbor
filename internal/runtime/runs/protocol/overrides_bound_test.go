package protocol_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// slotID builds a distinct identity triple per index — a fresh session id
// under one tenant/user, which is exactly the shape an authenticated
// caller uses to grow the slot map without bound.
func slotID(i int) identity.Identity {
	return identity.Identity{
		TenantID:  "tenant-bound",
		UserID:    "user-bound",
		SessionID: fmt.Sprintf("sess-%04d", i),
	}
}

func pendingAt(at time.Time) runsprotocol.PendingOverride {
	effort := "high"
	return runsprotocol.PendingOverride{ReasoningEffort: &effort, RecordedAt: at}
}

// TestStore_Set_BoundsSlotMapUnderNInserts is the bound itself: N inserts
// under N DISTINCT identity triples leave at most MaxSlots slots resident,
// where an unbounded map would hold all N for the life of the process.
//
// Mutation that turns this red: delete the `if len(s.slots) >= s.max`
// eviction call in Store.Set.
func TestStore_Set_BoundsSlotMapUnderNInserts(t *testing.T) {
	const (
		max = 64
		n   = 512
	)
	s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(max))
	base := time.Unix(1700000000, 0).UTC()
	for i := range n {
		s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
	}
	resident := 0
	for i := range n {
		if _, ok := s.Peek(slotID(i)); ok {
			resident++
		}
	}
	if resident != max {
		t.Fatalf("slot map holds %d entries after %d inserts, want the bound %d", resident, n, max)
	}
}

// TestStore_Set_EvictsOldestRecordedFirst pins the DIRECTION of the drop
// policy: the oldest-recorded slot goes and the newest survive. A
// drop-newest implementation holds the same bound and is wrong — it
// discards the write of the caller who just asked for it.
//
// Mutation that turns this red: evict s.order.Back() instead of Front().
func TestStore_Set_EvictsOldestRecordedFirst(t *testing.T) {
	const max = 8
	s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(max))
	base := time.Unix(1700000000, 0).UTC()
	// Fill, then push exactly max more so every original slot is displaced.
	for i := range 2 * max {
		s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
	}
	for i := range max {
		if _, ok := s.Peek(slotID(i)); ok {
			t.Fatalf("slot %d (oldest half) survived eviction — the policy dropped the wrong end", i)
		}
	}
	for i := max; i < 2*max; i++ {
		if _, ok := s.Peek(slotID(i)); !ok {
			t.Fatalf("slot %d (newest half) was evicted — the policy dropped the wrong end", i)
		}
	}
}

// TestStore_Set_ReSetRefreshesRecencyAndDoesNotGrow proves a re-Set for an
// identity ALREADY holding a slot neither grows the map nor leaves the
// identity at its original eviction position: the most recently expressed
// intent is the last to go.
//
// Mutation that turns this red: drop the s.order.MoveToBack(el) in the
// re-Set branch of Store.Set.
func TestStore_Set_ReSetRefreshesRecencyAndDoesNotGrow(t *testing.T) {
	const max = 4
	s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(max))
	base := time.Unix(1700000000, 0).UTC()
	for i := range max {
		s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
	}
	// Refresh the OLDEST slot, then admit one new identity. The refreshed
	// slot must survive and slot 1 (now the oldest) must go.
	s.Set(slotID(0), pendingAt(base.Add(time.Hour)))
	s.Set(slotID(max), pendingAt(base.Add(2*time.Hour)))

	if _, ok := s.Peek(slotID(0)); !ok {
		t.Fatal("the re-Set slot was evicted — a re-Set did not refresh its recency")
	}
	if _, ok := s.Peek(slotID(1)); ok {
		t.Fatal("slot 1 survived — the eviction did not follow the refreshed order")
	}
	resident := 0
	for i := 0; i <= max; i++ {
		if _, ok := s.Peek(slotID(i)); ok {
			resident++
		}
	}
	if resident != max {
		t.Fatalf("slot map holds %d entries, want the bound %d — a re-Set grew the map", resident, max)
	}
}

// TestStore_Consume_ReclaimsCapacity proves Consume frees a slot for the
// bound's purposes: a runtime whose overrides ARE consumed does not evict,
// AND the bound still holds afterwards because the recording-order list is
// popped alongside the map.
//
// The second half is the load-bearing one. Dropping the s.order.Remove(el)
// in Consume leaves a STALE element behind: the next eviction spends
// itself removing that tombstone (its map delete is a no-op), so the map
// grows one past the bound for every consumed slot — the bound leaks back
// open on the ordinary, non-abusive path. A test that only asserted "the
// remaining slots survived" stays green through exactly that mutation,
// which is why the resident count is asserted EXACTLY after the eviction
// is forced.
func TestStore_Consume_ReclaimsCapacity(t *testing.T) {
	const max = 4
	s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(max))
	base := time.Unix(1700000000, 0).UTC()
	for i := range max {
		s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
	}
	if _, ok := s.Consume(slotID(0)); !ok {
		t.Fatal("Consume did not find the slot it just recorded")
	}
	// Capacity is free again, so admitting a new identity must evict
	// NOTHING: every remaining original slot survives.
	s.Set(slotID(max), pendingAt(base.Add(time.Hour)))
	for i := 1; i <= max; i++ {
		if _, ok := s.Peek(slotID(i)); !ok {
			t.Fatalf("slot %d was evicted although a Consume had freed capacity", i)
		}
	}
	// One more identity now FORCES an eviction. The bound must be exact:
	// a consumed slot must not have left a tombstone that absorbs it.
	s.Set(slotID(max+1), pendingAt(base.Add(2*time.Hour)))
	resident := 0
	for i := 0; i <= max+1; i++ {
		if _, ok := s.Peek(slotID(i)); ok {
			resident++
		}
	}
	if resident != max {
		t.Fatalf("slot map holds %d entries after a Consume then two Sets, want the bound %d — a consumed slot left a tombstone in the recording order", resident, max)
	}
}

// TestStore_Set_BoundHoldsUnderConcurrentWriters runs the bound under the
// race detector with N concurrent writers against ONE shared Store (the
// D-025 concurrent-reuse shape): the invariant is that the map never
// exceeds MaxSlots regardless of interleaving.
func TestStore_Set_BoundHoldsUnderConcurrentWriters(t *testing.T) {
	const (
		max      = 32
		writers  = 128
		perWrite = 8
	)
	s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(max))
	base := time.Unix(1700000000, 0).UTC()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := range perWrite {
				idx := w*perWrite + j
				s.Set(slotID(idx), pendingAt(base.Add(time.Duration(idx)*time.Second)))
			}
		}(w)
	}
	wg.Wait()
	resident := 0
	for i := range writers * perWrite {
		if _, ok := s.Peek(slotID(i)); ok {
			resident++
		}
	}
	if resident > max {
		t.Fatalf("slot map holds %d entries after %d concurrent inserts, want at most %d",
			resident, writers*perWrite, max)
	}
}

// TestNewStore_DefaultBoundIsApplied proves the DEFAULT constructor — the
// one every production call site uses — is bounded, not merely the
// option-configured one. Without this a fix that only bounded the
// explicitly-configured Store would leave production unbounded.
//
// Mutation that turns this red: drop `max: DefaultMaxPendingOverrides`
// from NewStore (max becomes 0, and `len >= 0` then evicts on EVERY
// insert, so the resident count collapses to 1).
func TestNewStore_DefaultBoundIsApplied(t *testing.T) {
	s := runsprotocol.NewStore()
	base := time.Unix(1700000000, 0).UTC()
	n := runsprotocol.DefaultMaxPendingOverrides + 16
	for i := range n {
		s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
	}
	resident := 0
	for i := range n {
		if _, ok := s.Peek(slotID(i)); ok {
			resident++
		}
	}
	if resident != runsprotocol.DefaultMaxPendingOverrides {
		t.Fatalf("default Store holds %d entries after %d inserts, want %d",
			resident, n, runsprotocol.DefaultMaxPendingOverrides)
	}
}

// TestWithMaxSlots_NonPositiveKeepsTheDefault pins that the bound cannot
// be configured AWAY: an unbounded slot map is the defect the bound
// closes, so a zero / negative value is ignored rather than honoured.
func TestWithMaxSlots_NonPositiveKeepsTheDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(n))
		base := time.Unix(1700000000, 0).UTC()
		total := runsprotocol.DefaultMaxPendingOverrides + 4
		for i := range total {
			s.Set(slotID(i), pendingAt(base.Add(time.Duration(i)*time.Second)))
		}
		resident := 0
		for i := range total {
			if _, ok := s.Peek(slotID(i)); ok {
				resident++
			}
		}
		if resident != runsprotocol.DefaultMaxPendingOverrides {
			t.Fatalf("WithMaxSlots(%d): store holds %d entries, want the default bound %d",
				n, resident, runsprotocol.DefaultMaxPendingOverrides)
		}
	}
}
