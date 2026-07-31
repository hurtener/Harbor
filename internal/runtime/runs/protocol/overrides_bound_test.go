package protocol_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
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

// tenantSlotID builds a distinct identity triple per (tenant, index) so a
// test can spread slots across tenants — the axis the per-tenant
// sub-bound is keyed on.
func tenantSlotID(tenant string, i int) identity.Identity {
	return identity.Identity{
		TenantID:  tenant,
		UserID:    "user-bound",
		SessionID: fmt.Sprintf("sess-%04d", i),
	}
}

// fillSpreadAcrossTenants writes n slots spread over `tenants` tenants,
// round-robin, so no single tenant reaches its sub-bound before the
// GLOBAL bound is exercised.
func fillSpreadAcrossTenants(s *runsprotocol.Store, tenants, n int, base time.Time) []identity.Identity {
	ids := make([]identity.Identity, 0, n)
	for i := range n {
		id := tenantSlotID(fmt.Sprintf("tenant-%02d", i%tenants), i)
		s.Set(id, pendingAt(base.Add(time.Duration(i)*time.Second)))
		ids = append(ids, id)
	}
	return ids
}

func residentOf(s *runsprotocol.Store, ids []identity.Identity) int {
	n := 0
	for _, id := range ids {
		if _, ok := s.Peek(id); ok {
			n++
		}
	}
	return n
}

// TestNewStore_DefaultBoundIsApplied proves the DEFAULT constructor — the
// one every production call site uses — is bounded on BOTH axes, not
// merely the option-configured one. Without this a fix that only bounded
// the explicitly-configured Store would leave production unbounded.
//
// The global arm spreads its inserts across enough tenants that no tenant
// reaches its own sub-bound; a single-tenant fill would stop at the
// sub-bound and say nothing about the global one.
//
// Mutations that turn this red: drop `max: DefaultMaxPendingOverrides`
// from NewStore (max becomes 0, and `len >= 0` then evicts on EVERY
// insert, so the resident count collapses to 1); or drop
// `maxPerTenant: DefaultMaxPendingOverridesPerTenant` (the sub-bound
// arm's resident count runs to the global bound instead).
func TestNewStore_DefaultBoundIsApplied(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()

	t.Run("global", func(t *testing.T) {
		s := runsprotocol.NewStore()
		// 64 tenants × 4096/64 = 64 slots each, comfortably under the
		// 256 sub-bound, so only the global bound can fire.
		tenants := 64
		n := runsprotocol.DefaultMaxPendingOverrides + 16
		ids := fillSpreadAcrossTenants(s, tenants, n, base)
		if resident := residentOf(s, ids); resident != runsprotocol.DefaultMaxPendingOverrides {
			t.Fatalf("default Store holds %d entries after %d inserts across %d tenants, want the global bound %d",
				resident, n, tenants, runsprotocol.DefaultMaxPendingOverrides)
		}
	})

	t.Run("per tenant", func(t *testing.T) {
		s := runsprotocol.NewStore()
		n := runsprotocol.DefaultMaxPendingOverrides + 16
		ids := make([]identity.Identity, 0, n)
		for i := range n {
			id := tenantSlotID("one-tenant", i)
			s.Set(id, pendingAt(base.Add(time.Duration(i)*time.Second)))
			ids = append(ids, id)
		}
		if resident := residentOf(s, ids); resident != runsprotocol.DefaultMaxPendingOverridesPerTenant {
			t.Fatalf("one tenant holds %d entries after %d inserts, want the per-tenant bound %d",
				resident, n, runsprotocol.DefaultMaxPendingOverridesPerTenant)
		}
	})
}

// TestStore_PerTenantBound_OneTenantCannotEvictAnother is the isolation
// property itself, and it is the reason the sub-bound exists.
//
// The recorded rationale for choosing eviction over refusal claimed that
// refusal "lets one caller filling the map deny the surface to every
// other tenant" while eviction "confines the damage to the evicted slot".
// With a process-global order list and no sub-bound that was FALSE:
// executing it showed one tenant writing MaxSlots fresh session ids
// evicted a victim tenant's slot, and kept doing so for as long as it
// wrote. The sub-bound is what makes the claim true.
//
// Mutation that turns this red: delete the per-tenant branch in Store.Set
// (or reorder it after the global check) — the victim's slot goes.
func TestStore_PerTenantBound_OneTenantCannotEvictAnother(t *testing.T) {
	const (
		max       = 64
		perTenant = 8
	)
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(max),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()

	victim := identity.Identity{TenantID: "victim", UserID: "u", SessionID: "s"}
	s.Set(victim, pendingAt(base))

	// The attacker writes far past BOTH bounds under fresh session ids —
	// the exact shape an authenticated caller uses.
	for i := range max * 4 {
		s.Set(tenantSlotID("attacker", i), pendingAt(base.Add(time.Duration(i+1)*time.Second)))
	}

	if _, ok := s.Peek(victim); !ok {
		t.Fatal("the victim tenant's slot was evicted by another tenant's churn — the eviction policy's isolation claim does not hold")
	}
	attackerIDs := make([]identity.Identity, 0, max*4)
	for i := range max * 4 {
		attackerIDs = append(attackerIDs, tenantSlotID("attacker", i))
	}
	if resident := residentOf(s, attackerIDs); resident != perTenant {
		t.Fatalf("the attacking tenant holds %d slots, want its sub-bound %d", resident, perTenant)
	}
}

// TestStore_PerTenantBound_GlobalBoundStillHolds proves the sub-bound did
// not replace the global one. Enough distinct tenants, each under its own
// sub-bound, must still be capped in aggregate — otherwise the growth
// defect the whole bound exists to close is back, keyed on tenant instead
// of session.
//
// Mutation that turns this red: delete the `len(s.slots) >= s.max` branch
// in Store.Set.
func TestStore_PerTenantBound_GlobalBoundStillHolds(t *testing.T) {
	const (
		max       = 32
		perTenant = 4
		tenants   = 40 // 40 × 4 = 160 slots wanted, 32 allowed
	)
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(max),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()
	ids := make([]identity.Identity, 0, tenants*perTenant)
	for tn := range tenants {
		for i := range perTenant {
			id := tenantSlotID(fmt.Sprintf("tenant-%02d", tn), i)
			s.Set(id, pendingAt(base.Add(time.Duration(tn*perTenant+i)*time.Second)))
			ids = append(ids, id)
		}
	}
	if resident := residentOf(s, ids); resident != max {
		t.Fatalf("store holds %d entries after %d tenants × %d slots, want the global bound %d",
			resident, tenants, perTenant, max)
	}
}

// TestStore_PerTenantBound_ConsumeReleasesTheTenantList is the sub-bound's
// tombstone guard, and it also pins that byTenant does not itself become
// the unbounded map one level up: a tenant whose slots are all consumed
// must be able to admit a full sub-bound's worth again, and its list must
// have been dropped rather than left behind holding stale elements.
//
// Mutation that turns this red: drop the tenant-list Remove (or the
// empty-list delete) in Store.removeLocked — the tenant's list keeps its
// consumed elements, so the tenant hits its sub-bound perTenant slots
// early on the ordinary, non-abusive path.
func TestStore_PerTenantBound_ConsumeReleasesTheTenantList(t *testing.T) {
	const perTenant = 4
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(64),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()

	for round := range 3 {
		ids := make([]identity.Identity, 0, perTenant)
		for i := range perTenant {
			id := tenantSlotID("recycler", round*perTenant+i)
			s.Set(id, pendingAt(base.Add(time.Duration(round*perTenant+i)*time.Second)))
			ids = append(ids, id)
		}
		if resident := residentOf(s, ids); resident != perTenant {
			t.Fatalf("round %d: tenant holds %d of %d slots it just wrote — a consumed slot left a tombstone in the tenant order",
				round, resident, perTenant)
		}
		for _, id := range ids {
			if _, ok := s.Consume(id); !ok {
				t.Fatalf("round %d: Consume did not find the slot it just recorded", round)
			}
		}
	}
}

// TestWithMaxSlots_NonPositiveKeepsTheDefault pins that neither bound can
// be configured AWAY: an unbounded slot map is the defect they close, so
// a zero / negative value is ignored rather than honoured.
func TestWithMaxSlots_NonPositiveKeepsTheDefault(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	for _, n := range []int{0, -1} {
		s := runsprotocol.NewStore(runsprotocol.WithMaxSlots(n))
		total := runsprotocol.DefaultMaxPendingOverrides + 4
		ids := fillSpreadAcrossTenants(s, 64, total, base)
		if resident := residentOf(s, ids); resident != runsprotocol.DefaultMaxPendingOverrides {
			t.Fatalf("WithMaxSlots(%d): store holds %d entries, want the default bound %d",
				n, resident, runsprotocol.DefaultMaxPendingOverrides)
		}

		sp := runsprotocol.NewStore(runsprotocol.WithMaxSlotsPerTenant(n))
		perIDs := make([]identity.Identity, 0, total)
		for i := range total {
			id := tenantSlotID("one-tenant", i)
			sp.Set(id, pendingAt(base.Add(time.Duration(i)*time.Second)))
			perIDs = append(perIDs, id)
		}
		if resident := residentOf(sp, perIDs); resident != runsprotocol.DefaultMaxPendingOverridesPerTenant {
			t.Fatalf("WithMaxSlotsPerTenant(%d): one tenant holds %d entries, want the default sub-bound %d",
				n, resident, runsprotocol.DefaultMaxPendingOverridesPerTenant)
		}
	}
}

// TestStore_AtGlobalCapacity_ATenantAtItsSubBoundEvictsItself is the
// ORDER of the two checks, and it needs its own row because the resident
// counts alone cannot see it.
//
// The two bounds only disagree in one situation: the map is globally FULL
// *and* the admitting tenant is already at its sub-bound. Checking the
// global bound first there takes the globally-oldest slot — which belongs
// to a different tenant — and hands the churning caller a sibling's slot
// as the price of its own overflow. That is precisely the cross-tenant
// eviction the sub-bound exists to prevent, reintroduced by an ordering.
//
// Mutation that turns this red: swap the two `case` arms in Store.Set so
// the global bound is consulted first.
func TestStore_AtGlobalCapacity_ATenantAtItsSubBoundEvictsItself(t *testing.T) {
	const (
		max       = 32
		perTenant = 4
		tenants   = 8 // 8 × 4 = 32 = exactly the global bound
	)
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(max),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()

	// tenant-00 is written FIRST, so it owns the globally-oldest slots —
	// the ones a global-first eviction would take.
	seq := 0
	for tn := range tenants {
		for i := range perTenant {
			s.Set(tenantSlotID(fmt.Sprintf("tenant-%02d", tn), i),
				pendingAt(base.Add(time.Duration(seq)*time.Second)))
			seq++
		}
	}
	// The map is now exactly full and every tenant is exactly at its
	// sub-bound. The LAST-written tenant admits one more.
	last := fmt.Sprintf("tenant-%02d", tenants-1)
	s.Set(tenantSlotID(last, perTenant), pendingAt(base.Add(time.Duration(seq)*time.Second)))

	victim := tenantSlotID("tenant-00", 0)
	if _, ok := s.Peek(victim); !ok {
		t.Fatal("the globally-oldest slot (another tenant's) was evicted to admit a tenant that was already at its OWN sub-bound — the global bound was consulted before the per-tenant one")
	}
	// And the overflowing tenant paid for itself.
	if _, ok := s.Peek(tenantSlotID(last, 0)); ok {
		t.Fatal("the overflowing tenant's own oldest slot survived — it did not pay for its own overflow")
	}
}

// TestStore_Consume_ReclaimsGlobalCapacityAcrossTenants is the
// tombstone guard for the GLOBAL order list, and it must span tenants to
// have teeth. The per-tenant branch tests its list's Len(), which counts
// a stale element and therefore absorbs the tombstone by evicting early;
// the global branch tests the MAP's len, so a stale global element is a
// tombstone that spends a later eviction on nothing and lets the map grow
// one past the bound per consumed slot — on the ordinary, non-abusive
// path. A single-tenant test cannot reach the global branch at all.
//
// Mutation that turns this red: have Consume delete from the map without
// unlinking the entry from the order lists.
func TestStore_Consume_ReclaimsGlobalCapacityAcrossTenants(t *testing.T) {
	const (
		max       = 4
		perTenant = 4
	)
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(max),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()

	ids := make([]identity.Identity, 0, max+2)
	for i := range max {
		id := tenantSlotID(fmt.Sprintf("tenant-%02d", i), 0)
		s.Set(id, pendingAt(base.Add(time.Duration(i)*time.Second)))
		ids = append(ids, id)
	}
	if _, ok := s.Consume(ids[0]); !ok {
		t.Fatal("Consume did not find the slot it just recorded")
	}
	// Capacity is free again: admitting one new tenant must evict nothing.
	newA := tenantSlotID("tenant-new-a", 0)
	s.Set(newA, pendingAt(base.Add(time.Hour)))
	ids = append(ids, newA)
	for _, id := range ids[1:] {
		if _, ok := s.Peek(id); !ok {
			t.Fatalf("slot %+v was evicted although a Consume had freed capacity", id)
		}
	}
	// One more FORCES an eviction. The bound must be exact: the consumed
	// slot must not have left a tombstone that absorbs it.
	newB := tenantSlotID("tenant-new-b", 0)
	s.Set(newB, pendingAt(base.Add(2*time.Hour)))
	ids = append(ids, newB)
	if resident := residentOf(s, ids); resident != max {
		t.Fatalf("store holds %d entries after a Consume then two Sets across tenants, want the global bound %d — a consumed slot left a tombstone in the GLOBAL recording order",
			resident, max)
	}
}

// captureLogger returns a slog.Logger writing to a synchronised buffer,
// plus a reader for its contents.
func captureLogger() (*slog.Logger, func() string) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf.String
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStore_EvictionLog_NamesWhichBoundFired pins the third policy
// bullet: an eviction is LOUD, and it says which bound forced it. A
// tenant evicting itself and a tenant being displaced by the global bound
// are different operator situations — one is self-inflicted and needs no
// action, the other means the deployment is at its aggregate ceiling —
// and a line that reads alike for both hides the distinction the
// sub-bound was added to create.
//
// This is also the row that makes the sub-bound CLAMP observable. A
// sub-bound above the global bound can never fire on resident counts
// (the global bound is always reached first), so the clamp is invisible
// to every count-based assertion; what it changes is which mechanism runs
// and therefore which line an operator reads.
//
// Mutations that turn this red: drop the clamp in NewStore (the
// oversized-sub-bound arm logs `bound=global`); or collapse the two log
// messages into one shared line.
func TestStore_EvictionLog_NamesWhichBoundFired(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()

	t.Run("per-tenant bound fires", func(t *testing.T) {
		logger, read := captureLogger()
		s := runsprotocol.NewStore(
			runsprotocol.WithMaxSlots(64),
			runsprotocol.WithMaxSlotsPerTenant(4),
			runsprotocol.WithStoreLogger(logger),
		)
		for i := range 6 {
			s.Set(tenantSlotID("solo", i), pendingAt(base.Add(time.Duration(i)*time.Second)))
		}
		out := read()
		if !strings.Contains(out, "bound=per_tenant") {
			t.Fatalf("eviction log does not name the per-tenant bound:\n%s", out)
		}
		if strings.Contains(out, "bound=global") {
			t.Fatalf("a per-tenant eviction was reported as a global one:\n%s", out)
		}
	})

	t.Run("global bound fires", func(t *testing.T) {
		logger, read := captureLogger()
		s := runsprotocol.NewStore(
			runsprotocol.WithMaxSlots(4),
			runsprotocol.WithMaxSlotsPerTenant(2),
			runsprotocol.WithStoreLogger(logger),
		)
		// Six DISTINCT tenants, one slot each: no tenant reaches its
		// sub-bound, so only the global bound can fire.
		for i := range 6 {
			s.Set(tenantSlotID(fmt.Sprintf("tenant-%02d", i), 0),
				pendingAt(base.Add(time.Duration(i)*time.Second)))
		}
		out := read()
		if !strings.Contains(out, "bound=global") {
			t.Fatalf("eviction log does not name the global bound:\n%s", out)
		}
		if strings.Contains(out, "bound=per_tenant") {
			t.Fatalf("a global eviction was reported as a per-tenant one:\n%s", out)
		}
	})

	t.Run("an oversized sub-bound is clamped, not honoured", func(t *testing.T) {
		logger, read := captureLogger()
		s := runsprotocol.NewStore(
			runsprotocol.WithMaxSlots(4),
			runsprotocol.WithMaxSlotsPerTenant(1024),
			runsprotocol.WithStoreLogger(logger),
		)
		for i := range 6 {
			s.Set(tenantSlotID("solo", i), pendingAt(base.Add(time.Duration(i)*time.Second)))
		}
		out := read()
		if !strings.Contains(out, "bound=per_tenant") {
			t.Fatalf("a sub-bound above the global bound was left un-clamped, so the eviction reported the wrong mechanism:\n%s", out)
		}
		if !strings.Contains(out, "max_slots_per_tenant=4") {
			t.Fatalf("the clamped sub-bound is not the value reported to the operator:\n%s", out)
		}
	})
}

// TestStore_PerTenantBound_HoldsUnderConcurrentTenants runs both bounds
// under the race detector with N concurrent writers spread over several
// tenants against ONE shared Store (the D-025 concurrent-reuse shape).
// The two invariants are that neither bound is exceeded regardless of
// interleaving, and that the map and the two order lists never diverge
// (a divergence shows up as a resident count above a bound).
func TestStore_PerTenantBound_HoldsUnderConcurrentTenants(t *testing.T) {
	const (
		max       = 64
		perTenant = 8
		tenants   = 16
		writers   = 128
		perWrite  = 16
	)
	s := runsprotocol.NewStore(
		runsprotocol.WithMaxSlots(max),
		runsprotocol.WithMaxSlotsPerTenant(perTenant),
	)
	base := time.Unix(1700000000, 0).UTC()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%02d", w%tenants)
			for j := range perWrite {
				idx := w*perWrite + j
				s.Set(tenantSlotID(tenant, idx), pendingAt(base.Add(time.Duration(idx)*time.Second)))
			}
		}(w)
	}
	wg.Wait()

	total := 0
	for tn := range tenants {
		tenant := fmt.Sprintf("tenant-%02d", tn)
		resident := 0
		for i := range writers * perWrite {
			if _, ok := s.Peek(tenantSlotID(tenant, i)); ok {
				resident++
			}
		}
		if resident > perTenant {
			t.Fatalf("tenant %s holds %d slots, want at most its sub-bound %d", tenant, resident, perTenant)
		}
		total += resident
	}
	if total > max {
		t.Fatalf("store holds %d slots across %d tenants, want at most the global bound %d", total, tenants, max)
	}
}
