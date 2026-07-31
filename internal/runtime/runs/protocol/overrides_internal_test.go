package protocol

import (
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// This is the one in-package test file for the override Store. It exists
// because the per-tenant sub-bound introduced a SECOND map — one
// recording-order list per tenant that currently owns a slot — and that
// map's own growth is not observable through the public surface. Peek and
// Consume answer about identities; nothing answers "how many tenant lists
// are you still holding?". Left unguarded, the fix for an unbounded map
// keyed on session would have reintroduced an unbounded map keyed on
// tenant, one level up, and every count-based test would have stayed
// green.

// TestStore_ByTenant_ReleasesATenantsListWhenItEmpties pins that the
// per-tenant index does not accumulate a key per tenant ever seen. A
// tenant whose last slot is consumed (or evicted) must leave no entry
// behind.
//
// Mutations that turn this red: drop the `delete(s.byTenant, …)` when a
// tenant's list empties; or drop the tenant-list Remove that lets it
// reach zero.
func TestStore_ByTenant_ReleasesATenantsListWhenItEmpties(t *testing.T) {
	s := NewStore(WithMaxSlots(1024), WithMaxSlotsPerTenant(4))
	base := time.Unix(1700000000, 0).UTC()
	effort := "high"

	const tenants = 200
	ids := make([]identity.Identity, 0, tenants)
	for i := range tenants {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%04d", i),
			UserID:    "u",
			SessionID: "s",
		}
		s.Set(id, PendingOverride{ReasoningEffort: &effort, RecordedAt: base})
		ids = append(ids, id)
	}

	s.mu.Lock()
	held := len(s.byTenant)
	s.mu.Unlock()
	if held != tenants {
		t.Fatalf("byTenant holds %d lists after %d tenants each wrote one slot, want %d", held, tenants, tenants)
	}

	for _, id := range ids {
		if _, ok := s.Consume(id); !ok {
			t.Fatalf("Consume did not find the slot it just recorded for %s", id.TenantID)
		}
	}

	s.mu.Lock()
	held = len(s.byTenant)
	orderLen := s.order.Len()
	slots := len(s.slots)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("byTenant still holds %d tenant list(s) after every slot was consumed — the per-tenant index accumulates a key per tenant ever seen, which is the unbounded-map defect one level up", held)
	}
	if orderLen != 0 || slots != 0 {
		t.Fatalf("after consuming every slot: order=%d slots=%d, want 0/0 — the map and the global order diverged", orderLen, slots)
	}
}

// TestStore_ByTenant_ReleasesOnEvictionToo covers the same invariant on
// the eviction path rather than the Consume path: a tenant displaced by
// the GLOBAL bound must not leave its (now empty) list behind either.
func TestStore_ByTenant_ReleasesOnEvictionToo(t *testing.T) {
	const max = 16
	s := NewStore(WithMaxSlots(max), WithMaxSlotsPerTenant(2))
	base := time.Unix(1700000000, 0).UTC()
	effort := "high"

	// Four times the global bound, one slot per distinct tenant, so every
	// admission past the bound evicts a whole tenant.
	for i := range max * 4 {
		s.Set(identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%04d", i),
			UserID:    "u",
			SessionID: "s",
		}, PendingOverride{ReasoningEffort: &effort, RecordedAt: base})
	}

	s.mu.Lock()
	held := len(s.byTenant)
	slots := len(s.slots)
	orderLen := s.order.Len()
	s.mu.Unlock()
	if held != max {
		t.Fatalf("byTenant holds %d tenant list(s) after %d single-slot tenants against a bound of %d — an evicted tenant left its empty list behind", held, max*4, max)
	}
	if slots != max || orderLen != max {
		t.Fatalf("slots=%d order=%d, want %d/%d — the map and the global order diverged under eviction", slots, orderLen, max, max)
	}
}
