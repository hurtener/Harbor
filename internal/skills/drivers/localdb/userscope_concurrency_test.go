package localdb_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// TestUserScope_ConcurrentReuse_NoCrossUserBleed is the D-025 concurrent-reuse
// gate for the durable ScopeUser rung (D-345): a single shared SkillStore is
// hammered by N≥100 concurrent goroutines, each authoring and reading a
// user-scope skill under its OWN (tenant, user) from a DIFFERENT session than
// it wrote from. It asserts (a) no data race (run under -race), (b) no
// cross-user / cross-tenant bleed (each reader sees exactly its own durable
// skill), (c) within-user cross-session visibility (write session != read
// session), and (d) no goroutine leak after the fan-out joins.
func TestUserScope_ConcurrentReuse_NoCrossUserBleed(t *testing.T) {
	ctx := context.Background()
	store := openStore(t) // one shared instance

	const n = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once to maximise contention

			tenant := fmt.Sprintf("t-%03d", i%7) // several tenants share goroutines
			user := fmt.Sprintf("u-%03d", i)     // every goroutine a distinct user
			name := fmt.Sprintf("durable-%03d", i)

			writeID := identity.Quadruple{Identity: identity.Identity{
				TenantID: tenant, UserID: user, SessionID: fmt.Sprintf("s-write-%03d", i),
			}}
			readID := identity.Quadruple{Identity: identity.Identity{
				TenantID: tenant, UserID: user, SessionID: fmt.Sprintf("s-read-%03d", i),
			}}

			sk := skills.Skill{
				Name:    name,
				Trigger: "trigger:" + name,
				Steps:   []string{"only step"},
				Origin:  skills.OriginGenerated,
				Scope:   skills.ScopeUser,
			}
			sk.ContentHash = skills.CanonicalContentHash(sk)
			if err := store.Upsert(ctx, writeID, sk); err != nil {
				errCh <- fmt.Errorf("goroutine %d: Upsert: %w", i, err)
				return
			}

			// Cross-session read of the caller's OWN durable skill.
			got, err := store.Get(ctx, readID, name)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: cross-session Get: %w", i, err)
				return
			}
			if got.Name != name || got.Scope != skills.ScopeUser {
				errCh <- fmt.Errorf("goroutine %d: read wrong skill: %+v", i, got)
				return
			}

			// List (Scope=user) must return exactly this user's one durable skill
			// — never another user's (no cross-user bleed).
			list, err := store.List(ctx, readID, skills.ListFilter{Scope: skills.ScopeUser})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: List: %w", i, err)
				return
			}
			if len(list) != 1 || list[0].Name != name {
				errCh <- fmt.Errorf("goroutine %d: cross-user bleed, list=%v", i, names(list))
				return
			}
		}(i)
	}

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak check: the fan-out has joined; allow the runtime a brief
	// settle for the store's internal bookkeeping, then assert baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: baseline=%d now=%d (leaked %d)", baseline, runtime.NumGoroutine(), leaked)
	}
}

func names(list []skills.Skill) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}
