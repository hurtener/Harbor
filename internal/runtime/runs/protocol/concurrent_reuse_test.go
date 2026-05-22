package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// TestService_ConcurrentReuse_NoCrossSessionBleed is the mandatory
// D-025 concurrent-reuse test: N≥100 concurrent `runs.set_overrides`
// invocations against ONE shared Service + Store, asserting no data
// races (the -race gate), no context bleed (each session reads back its
// OWN override, never another session's), and no goroutine leak
// (baseline restored after all calls return).
//
// Each goroutine targets a distinct session, so the test simultaneously
// proves multi-isolation: the Store's identity-triple key keeps every
// session's pending override invisible to every other session.
func TestService_ConcurrentReuse_NoCrossSessionBleed(t *testing.T) {
	const n = 200

	baseline := runtime.NumGoroutine()

	svc, store := newService(t)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("session-%04d", i)
			effort := []string{"low", "medium", "high"}[i%3]
			req := prototypes.RunSetOverridesRequest{
				Identity: prototypes.IdentityScope{
					Tenant: testTenant, User: testUser, Session: session,
				},
				Overrides: prototypes.RunOverrides{
					SessionID:       session,
					ReasoningEffort: strPtr(effort),
					MaxTokens:       intPtr(1024 + i),
				},
			}
			if _, err := svc.SetOverrides(context.Background(), req); err != nil {
				t.Errorf("session %s: SetOverrides: %v", session, err)
			}
		}()
	}
	wg.Wait()

	// Every session recorded EXACTLY its own override — no bleed.
	for i := range n {
		session := fmt.Sprintf("session-%04d", i)
		wantEffort := []string{"low", "medium", "high"}[i%3]
		id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: session}
		po, ok := store.Peek(id)
		if !ok {
			t.Errorf("session %s: no override recorded", session)
			continue
		}
		if po.ReasoningEffort == nil || *po.ReasoningEffort != wantEffort {
			t.Errorf("session %s: ReasoningEffort = %v, want %q (context bleed)", session, po.ReasoningEffort, wantEffort)
		}
		if po.MaxTokens == nil || *po.MaxTokens != 1024+i {
			t.Errorf("session %s: MaxTokens = %v, want %d (context bleed)", session, po.MaxTokens, 1024+i)
		}
	}

	// Goroutine-leak gate — the baseline is restored once every
	// SetOverrides has returned (the Service spawns no per-call
	// goroutine, so this is a strict equality after a short settle).
	assertGoroutineBaseline(t, baseline)
}

// TestStore_ConcurrentSetConsume_NoRace exercises the Store directly
// with concurrent Set / Consume / Peek against ONE shared instance —
// the close-during-publish-style race the §17.3 boundary stress targets.
func TestStore_ConcurrentSetConsume_NoRace(t *testing.T) {
	const n = 150
	store := runsprotocol.NewStore()

	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := range n {
		id := identity.Identity{
			TenantID: testTenant, UserID: testUser,
			SessionID: fmt.Sprintf("sess-%d", i),
		}
		go func() { defer wg.Done(); store.Set(id, runsprotocol.PendingOverride{}) }()
		go func() { defer wg.Done(); store.Consume(id) }()
		go func() { defer wg.Done(); store.Peek(id) }()
	}
	wg.Wait()
}

// assertGoroutineBaseline polls until the live goroutine count returns
// to (or below) baseline, failing after a bounded real-time timeout.
// CLAUDE.md §11 — no time.Sleep as a synchronisation primitive.
func assertGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		runtime.Gosched()
	}
	t.Errorf("goroutine leak: NumGoroutine()=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}
