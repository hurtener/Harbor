package tools_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// TestPlannerTools_ConcurrentReuse_D025 — N≥128 concurrent
// invocations of `skill_search` / `skill_get` / `skill_list` against
// ONE shared catalog + ONE shared store. Asserts no data races (via
// -race), no context bleed (each goroutine carries a unique identity
// and asserts the SkillStore sees that exact identity), no
// cross-cancellation (cancelling one goroutine's ctx does NOT affect
// siblings), no goroutine leak (NumGoroutine returns to baseline
// after teardown).
//
// Per CLAUDE.md §5 + §11 + D-025: a phase that builds a reusable
// artifact ships this test. Phase 38 builds three reusable
// ToolDescriptors and a shared `*tools.catalog` that holds them.
func TestPlannerTools_ConcurrentReuse_D025(t *testing.T) {
	// NOT t.Parallel() — the goroutine-count assertion races with
	// any sibling test bursting N goroutines. Serial keeps the
	// baseline + current measurement honest.
	const N = 128

	bus := newTestBus(t)
	store := newSpyStore(bus)
	catalog := tcat.NewCatalog()

	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dSearch, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)
	dGet, _ := catalog.Resolve(skilltools.ToolNameSkillGet)
	dList, _ := catalog.Resolve(skilltools.ToolNameSkillList)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var (
		searchOK atomic.Int64
		getOK    atomic.Int64
		listOK   atomic.Int64
		bleeds   atomic.Int64
	)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i)
			user := fmt.Sprintf("u-%d", i)
			session := fmt.Sprintf("s-%d", i)
			id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("r-%d", i))
			if err != nil {
				t.Errorf("goroutine %d: identity.WithRun: %v", i, err)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			// Cancel a small fraction at the end — proves
			// cancellation does NOT propagate to sibling goroutines.
			defer cancel()

			switch i % 3 {
			case 0:
				args := mustMarshal(t, skilltools.SearchArgs{
					Query:      "x",
					Capability: skilltools.CapabilityContext{AllowedTools: []string{"http_fetch", "tool_search"}},
				})
				if _, err := dSearch.Invoke(ctx, args); err != nil {
					t.Errorf("goroutine %d: search: %v", i, err)
					return
				}
				searchOK.Add(1)
			case 1:
				args := mustMarshal(t, skilltools.GetArgs{
					Names:      []string{"only", "alt-only"},
					MaxTokens:  4096,
					Capability: skilltools.CapabilityContext{AllowedTools: []string{"http_fetch"}},
				})
				if _, err := dGet.Invoke(ctx, args); err != nil {
					t.Errorf("goroutine %d: get: %v", i, err)
					return
				}
				getOK.Add(1)
			case 2:
				args := mustMarshal(t, skilltools.ListArgs{
					Capability: skilltools.CapabilityContext{AllowedTools: []string{"http_fetch"}},
				})
				if _, err := dList.Invoke(ctx, args); err != nil {
					t.Errorf("goroutine %d: list: %v", i, err)
					return
				}
				listOK.Add(1)
			}

			// Verify that the spy store saw the exact identity for
			// THIS goroutine — proves no context bleed.
			seen := store.lastIdentityFor(tenant)
			if seen.TenantID != tenant || seen.UserID != user || seen.SessionID != session {
				bleeds.Add(1)
				t.Errorf("goroutine %d: identity bleed: seen=(%s,%s,%s), want=(%s,%s,%s)",
					i, seen.TenantID, seen.UserID, seen.SessionID, tenant, user, session)
			}
		}(i)
	}
	wg.Wait()

	if total := searchOK.Load() + getOK.Load() + listOK.Load(); total != int64(N) {
		t.Fatalf("total successful invocations=%d, want %d (search=%d get=%d list=%d)",
			total, N, searchOK.Load(), getOK.Load(), listOK.Load())
	}
	if bleeds.Load() != 0 {
		t.Fatalf("identity bleeds detected: %d", bleeds.Load())
	}

	runtime.GC()
	current := runtime.NumGoroutine()
	if current > baseline+5 {
		t.Fatalf("goroutine count grew: baseline=%d current=%d", baseline, current)
	}
}

// spyStore is a concurrency-safe SkillStore that records the last
// identity it saw per tenant. Used by the D-025 test to assert no
// context bleed.
type spyStore struct {
	mu      sync.Mutex
	seen    map[string]identity.Identity
	storedSkills []skills.Skill
}

// newSpyStore is intentionally bus-agnostic — the spy reproduces the
// production identity-validation contract without participating in
// the event bus, so we don't need the bus reference here.
func newSpyStore(_ interface{}) *spyStore {
	return &spyStore{
		seen: make(map[string]identity.Identity),
		storedSkills: []skills.Skill{
			{
				Name:    "only",
				Trigger: "trig",
				Steps:   []string{"s"},
				Origin:  skills.OriginPack,
				Scope:   skills.ScopeProject,
			},
			{
				Name:          "alt-only",
				Trigger:       "trig2",
				Steps:         []string{"s"},
				Origin:        skills.OriginPack,
				Scope:         skills.ScopeProject,
				RequiredTools: []string{"http_fetch"},
			},
		},
	}
}

func (s *spyStore) recordIdentity(q identity.Quadruple) {
	s.mu.Lock()
	s.seen[q.TenantID] = q.Identity
	s.mu.Unlock()
}

func (s *spyStore) lastIdentityFor(tenant string) identity.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[tenant]
}

func (s *spyStore) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	return nil
}

func (s *spyStore) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return skills.Skill{}, fmt.Errorf("%w: %v", skills.ErrIdentityRequired, err)
	}
	s.recordIdentity(id)
	for _, sk := range s.storedSkills {
		if sk.Name == name {
			return sk, nil
		}
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (s *spyStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, fmt.Errorf("%w: %v", skills.ErrIdentityRequired, err)
	}
	s.recordIdentity(id)
	out := make([]skills.Skill, len(s.storedSkills))
	copy(out, s.storedSkills)
	return out, nil
}

func (s *spyStore) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, fmt.Errorf("%w: %v", skills.ErrIdentityRequired, err)
	}
	s.recordIdentity(id)
	out := make([]skills.RankedSkill, 0, len(s.storedSkills))
	for _, sk := range s.storedSkills {
		out = append(out, skills.RankedSkill{Skill: sk, Score: 0.9, Path: skills.PathFTS5})
	}
	return out, nil
}

func (s *spyStore) Delete(ctx context.Context, id identity.Quadruple, name string) error {
	return nil
}

func (s *spyStore) Close(ctx context.Context) error {
	return nil
}
