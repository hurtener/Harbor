package sessionoverlay_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

const overlayAgentID = "agent-x"

func newState(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func newOverlay(t *testing.T) sessionoverlay.Store {
	t.Helper()
	s, err := sessionoverlay.NewStore(newState(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func quad(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
}

func TestNewStore_NilStateFailsLoud(t *testing.T) {
	if _, err := sessionoverlay.NewStore(nil, nil); !errors.Is(err, sessionoverlay.ErrInvalidConfig) {
		t.Fatalf("nil state should fail with ErrInvalidConfig, got %v", err)
	}
}

func TestStore_IdentityRequired(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	_, _, err := s.Get(ctx, quad("t", "", "s"), overlayAgentID)
	if !errors.Is(err, sessionoverlay.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should fail closed, got %v", err)
	}
	if _, err := s.SetUserPrompt(ctx, quad("t", "u", "s"), "", "x"); !errors.Is(err, sessionoverlay.ErrIdentityRequired) {
		t.Fatalf("empty agent id should fail closed, got %v", err)
	}
}

func TestStore_ReservedAgentConfigUserRejectedAtEveryControlBoundary(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", agentcfg.ReservedAgentConfigUser, "ordinary-session")
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "get", run: func() error { _, _, err := s.Get(ctx, id, overlayAgentID); return err }},
		{name: "set prompt", run: func() error { _, err := s.SetUserPrompt(ctx, id, overlayAgentID, "prompt"); return err }},
		{name: "set disables", run: func() error {
			_, err := s.SetSourceDisables(ctx, id, overlayAgentID, []string{"server"}, nil)
			return err
		}},
		{name: "add personal", run: func() error { _, err := s.AddPersonalSkill(ctx, id, overlayAgentID, "skill"); return err }},
		{name: "remove personal", run: func() error { _, err := s.RemovePersonalSkill(ctx, id, overlayAgentID, "skill"); return err }},
	}
	for _, operation := range operations {
		if err := operation.run(); !errors.Is(err, agentcfg.ErrReservedUser) {
			t.Errorf("%s = %v, want ErrReservedUser", operation.name, err)
		}
	}
}

func TestStore_UserPromptRoundTrip(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", "u", "s")
	if _, found, _ := mustGet(t, s, id); found {
		t.Fatal("fresh overlay should not exist")
	}
	if _, err := s.SetUserPrompt(ctx, id, overlayAgentID, "guidance"); err != nil {
		t.Fatalf("set: %v", err)
	}
	ov, found, _ := mustGet(t, s, id)
	if !found || ov.UserPrompt != "guidance" {
		t.Fatalf("user prompt round-trip = %+v found=%v", ov, found)
	}
}

func TestStore_SetSourceDisablesPreservesOtherFields(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", "u", "s")
	if _, err := s.SetUserPrompt(ctx, id, overlayAgentID, "keep-me"); err != nil {
		t.Fatalf("set prompt: %v", err)
	}
	ov, err := s.SetSourceDisables(ctx, id, overlayAgentID, []string{"srvB", "srvA"}, []string{"toolX"})
	if err != nil {
		t.Fatalf("set disables: %v", err)
	}
	// Sorted + preserved user prompt.
	if ov.UserPrompt != "keep-me" {
		t.Fatalf("user prompt clobbered by disable edit: %+v", ov)
	}
	if len(ov.DisabledServers) != 2 || ov.DisabledServers[0] != "srvA" || ov.DisabledServers[1] != "srvB" {
		t.Fatalf("disabled servers not sorted/recorded: %+v", ov.DisabledServers)
	}
}

func TestStore_PersonalSkillAddRemoveIdempotent(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", "u", "s")
	if _, err := s.AddPersonalSkill(ctx, id, overlayAgentID, "sk1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	ov, err := s.AddPersonalSkill(ctx, id, overlayAgentID, "sk1") // idempotent
	if err != nil {
		t.Fatalf("add again: %v", err)
	}
	if len(ov.PersonalSkills) != 1 {
		t.Fatalf("idempotent add produced dup: %+v", ov.PersonalSkills)
	}
	ov, err = s.RemovePersonalSkill(ctx, id, overlayAgentID, "sk1")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(ov.PersonalSkills) != 0 {
		t.Fatalf("remove left residue: %+v", ov.PersonalSkills)
	}
	// Empty name fails loud.
	if _, err := s.AddPersonalSkill(ctx, id, overlayAgentID, ""); !errors.Is(err, sessionoverlay.ErrInvalidInput) {
		t.Fatalf("empty name should fail with ErrInvalidInput, got %v", err)
	}
}

// TestStore_CrossSessionIsolation proves one session's overlay is invisible
// to another session (the overlay is keyed by the real triple).
func TestStore_CrossSessionIsolation(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	a := quad("t", "u", "sessA")
	b := quad("t", "u", "sessB")
	if _, err := s.SetUserPrompt(ctx, a, overlayAgentID, "A"); err != nil {
		t.Fatalf("set A: %v", err)
	}
	if _, found, _ := mustGet(t, s, b); found {
		t.Fatal("session B must not see session A's overlay")
	}
	// Different tenant, same user/session string — also isolated.
	c := quad("other", "u", "sessA")
	if _, found, _ := mustGet(t, s, c); found {
		t.Fatal("tenant 'other' must not see tenant 't' overlay")
	}
}

// TestStore_ConcurrentReuse runs N concurrent sessions against ONE shared
// store and asserts no cross-session bleed under -race (the concurrent-reuse
// contract + §6 cross-session isolation).
func TestStore_ConcurrentReuse(t *testing.T) {
	baseline := runtime.NumGoroutine()
	base := newState(t)
	barrier := newContextEntryBarrierStore(base, 128)
	s, err := sessionoverlay.NewStore(barrier, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	contexts := make([]context.Context, n)
	cancels := make([]context.CancelFunc, n)
	for i := range n {
		contexts[i], cancels[i] = context.WithCancel(withEntryToken(context.Background(), i))
	}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := contexts[i]
			id := quad("t", "u", fmt.Sprintf("sess-%d", i))
			want := fmt.Sprintf("prompt-%d", i)
			if _, err := s.SetUserPrompt(ctx, id, overlayAgentID, want); err != nil {
				if i%2 == 0 && errors.Is(err, context.Canceled) {
					return
				}
				errs <- fmt.Errorf("session %d set: %w", i, err)
				return
			}
			if i%2 == 0 {
				errs <- fmt.Errorf("session %d canceled write completed", i)
				return
			}
			if _, err := s.AddPersonalSkill(ctx, id, overlayAgentID, fmt.Sprintf("sk-%d", i)); err != nil {
				errs <- err
				return
			}
			ov, found, err := s.Get(ctx, id, overlayAgentID)
			if err != nil {
				errs <- err
				return
			}
			if !found || ov.UserPrompt != want {
				errs <- fmt.Errorf("session %d saw prompt %q want %q", i, ov.UserPrompt, want)
				return
			}
			if len(ov.PersonalSkills) != 1 || ov.PersonalSkills[0] != fmt.Sprintf("sk-%d", i) {
				errs <- fmt.Errorf("session %d personal skills bled: %+v", i, ov.PersonalSkills)
			}
		}(i)
	}
	barrier.waitForAll(t)
	for i := 0; i < n; i += 2 {
		cancels[i]()
	}
	barrier.releaseAll()
	wg.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := barrier.maximumOverlap(); got != n {
		t.Errorf("actual store overlap = %d, want %d", got, n)
	}
	for i := range n {
		id := quad("t", "u", fmt.Sprintf("sess-%d", i))
		ov, found, err := s.Get(context.Background(), id, overlayAgentID)
		if err != nil {
			t.Fatalf("verify session %d: %v", i, err)
		}
		if i%2 == 0 {
			if found {
				t.Errorf("canceled session %d persisted %+v", i, ov)
			}
			continue
		}
		if !found || ov.UserPrompt != fmt.Sprintf("prompt-%d", i) || len(ov.PersonalSkills) != 1 || ov.PersonalSkills[0] != fmt.Sprintf("sk-%d", i) {
			t.Errorf("session %d identity bleed: found=%v overlay=%+v", i, found, ov)
		}
	}
	assertGoroutinesRestored(t, baseline)
}

func mustGet(t *testing.T, s sessionoverlay.Store, id identity.Quadruple) (sessionoverlay.Overlay, bool, error) {
	t.Helper()
	ov, found, err := s.Get(context.Background(), id, overlayAgentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return ov, found, err
}

// TestStore_ConcurrentSameSessionMutations_NoLostUpdate is the wave-end
// regression for the same-session lost-update race (audit W1): mutate() is a
// load → apply → save read-modify-write over a last-write-wins overlay row, so
// two concurrent SAME-session edits via different verbs would each rebuild
// from the other's pre-write snapshot, clobbering a sibling field. N≥100
// concurrent AddPersonalSkill / SetSourceDisables / SetUserPrompt against ONE
// shared store + ONE (triple, agent) slot must leave ALL THREE fields present.
// Run under -race.
func TestStore_ConcurrentSameSessionMutations_NoLostUpdate(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", "u", "s")
	const n = 128

	var wg sync.WaitGroup
	errs := make(chan error, n)
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entered <- struct{}{}
			<-release
			var err error
			switch i % 3 {
			case 0:
				_, err = s.AddPersonalSkill(ctx, id, overlayAgentID, fmt.Sprintf("skill-%d", i))
			case 1:
				_, err = s.SetSourceDisables(ctx, id, overlayAgentID, []string{fmt.Sprintf("srv-%d", i)}, nil)
			case 2:
				_, err = s.SetUserPrompt(ctx, id, overlayAgentID, fmt.Sprintf("prompt-%d", i))
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	for range n {
		<-entered
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent same-session mutate failed: %v", err)
	}

	// All three fields must COEXIST: each was set by ≥1 goroutine, and the
	// per-slot lock guarantees no verb rebuilt from a snapshot missing a
	// concurrently-committed sibling field (the lost-update bug).
	o, ok, err := s.Get(ctx, id, overlayAgentID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if o.UserPrompt == "" {
		t.Error("user prompt lost under concurrent same-session mutation (lost-update race)")
	}
	wantPersonal := 0
	for i := range n {
		if i%3 == 0 {
			wantPersonal++
		}
	}
	if len(o.PersonalSkills) != wantPersonal {
		t.Errorf("personal skills lost under concurrent same-session mutation: got=%d want=%d", len(o.PersonalSkills), wantPersonal)
	}
	if len(o.DisabledServers) == 0 {
		t.Error("disabled servers lost under concurrent same-session mutation (lost-update race)")
	}
}

func assertGoroutinesRestored(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Errorf("goroutine baseline not restored: got=%d baseline=%d", got, baseline)
	}
}
