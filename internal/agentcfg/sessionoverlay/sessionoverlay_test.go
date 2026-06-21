package sessionoverlay_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

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
	s := newOverlay(t)
	const n = 120
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			id := quad("t", "u", fmt.Sprintf("sess-%d", i))
			want := fmt.Sprintf("prompt-%d", i)
			if _, err := s.SetUserPrompt(ctx, id, overlayAgentID, want); err != nil {
				errs <- err
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
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func mustGet(t *testing.T, s sessionoverlay.Store, id identity.Quadruple) (sessionoverlay.Overlay, bool, error) {
	t.Helper()
	ov, found, err := s.Get(context.Background(), id, overlayAgentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return ov, found, err
}
