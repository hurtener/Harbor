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
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
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
	st := newState(t)
	activateAgent(t, st, quad("t", "u", "s"), overlayAgentID)
	activateAgent(t, st, quad("other", "u", "s"), overlayAgentID)
	s, err := sessionoverlay.NewStore(st, nil)
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

func TestStore_LegacyPersonalSkillMutationsFailWithoutWrite(t *testing.T) {
	s := newOverlay(t)
	ctx := context.Background()
	id := quad("t", "u", "s")
	if _, err := s.AddPersonalSkill(ctx, id, overlayAgentID, "sk1"); !errors.Is(err, sessionoverlay.ErrCutoverPending) {
		t.Fatalf("add = %v, want ErrCutoverPending", err)
	}
	if _, err := s.RemovePersonalSkill(ctx, id, overlayAgentID, "sk1"); !errors.Is(err, sessionoverlay.ErrCutoverPending) {
		t.Fatalf("remove = %v, want ErrCutoverPending", err)
	}
	if _, found, err := s.Get(ctx, id, overlayAgentID); err != nil || found {
		t.Fatalf("legacy personal refusal wrote overlay: found=%v err=%v", found, err)
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
	activateAgent(t, base, quad("t", "u", "s"), overlayAgentID)
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
			if _, err := s.SetSourceDisables(ctx, id, overlayAgentID, []string{fmt.Sprintf("srv-%d", i)}, nil); err != nil {
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
			if len(ov.DisabledServers) != 1 || ov.DisabledServers[0] != fmt.Sprintf("srv-%d", i) {
				errs <- fmt.Errorf("session %d disables bled: %+v", i, ov.DisabledServers)
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
		if !found || ov.UserPrompt != fmt.Sprintf("prompt-%d", i) || len(ov.DisabledServers) != 1 || ov.DisabledServers[0] != fmt.Sprintf("srv-%d", i) {
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

type overlaySaveIfBarrier struct {
	state.StateStore
	entered chan struct{}
	release chan struct{}
}

func (s *overlaySaveIfBarrier) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

// TestStore_ConcurrentSameSlotCAS_OneWinner proves correctness is the durable
// four-slot CAS, not a process-local lock: every writer reaches SaveIf with the
// same absent-target expectation, exactly one wins, and all losers fail loud.
func TestStore_ConcurrentSameSlotCAS_OneWinner(t *testing.T) {
	base := newState(t)
	id := quad("t", "u", "same-slot")
	activateAgent(t, base, id, overlayAgentID)
	const n = 128
	barrier := &overlaySaveIfBarrier{StateStore: base, entered: make(chan struct{}, n), release: make(chan struct{})}
	s, err := sessionoverlay.NewStore(barrier, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		prompt string
		err    error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prompt := fmt.Sprintf("prompt-%03d", i)
			_, err := s.SetUserPrompt(context.Background(), id, overlayAgentID, prompt)
			results <- result{prompt: prompt, err: err}
		}(i)
	}
	for range n {
		<-barrier.entered
	}
	close(barrier.release)
	wg.Wait()
	close(results)
	winners := make(map[string]struct{}, 1)
	losers := 0
	for result := range results {
		switch {
		case result.err == nil:
			winners[result.prompt] = struct{}{}
		case errors.Is(result.err, state.ErrConditionFailed):
			losers++
		default:
			t.Errorf("unexpected CAS result for %q: %v", result.prompt, result.err)
		}
	}
	if len(winners) != 1 || losers != n-1 {
		t.Fatalf("CAS winners=%v losers=%d, want one/%d", winners, losers, n-1)
	}
	got, found, err := s.Get(context.Background(), id, overlayAgentID)
	if err != nil || !found {
		t.Fatalf("Get winner = (%+v, %v, %v)", got, found, err)
	}
	if _, ok := winners[got.UserPrompt]; !ok {
		t.Fatalf("durable prompt %q is not the CAS winner %v", got.UserPrompt, winners)
	}
}

type overlayCommitThenErrorStore struct {
	state.StateStore
	mu           sync.Mutex
	expectations []state.SlotExpectation
}

func (s *overlayCommitThenErrorStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	if next.Kind == sessionoverlay.LegacyOverlayKind(overlayAgentID) {
		s.mu.Lock()
		s.expectations = append([]state.SlotExpectation(nil), expectations...)
		s.mu.Unlock()
		return errors.New("injected response loss after overlay commit")
	}
	return nil
}

func TestStore_FourSlotCommitThenErrorRestartAndClose(t *testing.T) {
	base := newState(t)
	id := quad("t", "u", "restart")
	activateAgent(t, base, id, overlayAgentID)
	fault := &overlayCommitThenErrorStore{StateStore: base}
	first, err := sessionoverlay.NewStore(fault, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := first.SetUserPrompt(context.Background(), id, overlayAgentID, "committed")
	if err != nil || got.UserPrompt != "committed" {
		t.Fatalf("commit-then-error SetUserPrompt = (%+v, %v)", got, err)
	}
	fault.mu.Lock()
	expectations := append([]state.SlotExpectation(nil), fault.expectations...)
	fault.mu.Unlock()
	if len(expectations) != 4 {
		t.Fatalf("SaveIf expectations=%d, want target+lifecycle+pending+tombstone", len(expectations))
	}

	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Get(context.Background(), id, overlayAgentID); !errors.Is(err, sessionoverlay.ErrClosed) {
		t.Fatalf("closed first store Get = %v, want ErrClosed", err)
	}
	// Close is wrapper-local and must not close the shared StateStore. A fresh
	// process-equivalent wrapper reads and updates the committed record.
	restarted, err := sessionoverlay.NewStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, found, err := restarted.Get(context.Background(), id, overlayAgentID)
	if err != nil || !found || loaded.UserPrompt != "committed" {
		t.Fatalf("restart Get = (%+v, %v, %v)", loaded, found, err)
	}
	updated, err := restarted.SetSourceDisables(context.Background(), id, overlayAgentID, []string{"b", "a"}, []string{"tool"})
	if err != nil || updated.UserPrompt != "committed" || len(updated.DisabledServers) != 2 || updated.DisabledServers[0] != "a" {
		t.Fatalf("restart SetSourceDisables = (%+v, %v)", updated, err)
	}
}

func TestStore_PreservesStrictLegacyPersonalNamesWithoutMutatingThem(t *testing.T) {
	st := newState(t)
	id := quad("t", "u", "legacy-personal")
	activateAgent(t, st, id, overlayAgentID)
	if err := st.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: id, Kind: sessionoverlay.LegacyOverlayKind(overlayAgentID),
		Bytes: []byte(`{"schema":1,"overlay":{"personal_skills":["legacy-a","legacy-b"]},"updated_at":"2026-08-02T00:00:00Z"}`),
	}); err != nil {
		t.Fatal(err)
	}
	s, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.SetUserPrompt(context.Background(), id, overlayAgentID, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.PersonalSkills) != 2 || updated.PersonalSkills[0] != "legacy-a" || updated.PersonalSkills[1] != "legacy-b" {
		t.Fatalf("prompt write mutated legacy personal names: %+v", updated.PersonalSkills)
	}

	kind := sessionoverlay.LegacyOverlayKind(overlayAgentID)
	rec, err := st.Load(context.Background(), id, kind)
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = state.NewEventID()
	rec.Bytes = []byte(`{"schema":1,"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`)
	if err := st.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(context.Background(), id, overlayAgentID); !errors.Is(err, sessionoverlay.ErrLegacyOverlayInvalid) {
		t.Fatalf("duplicate schema Get = %v, want ErrLegacyOverlayInvalid", err)
	}
}

func TestStore_LifecycleAndErasureErrorsRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, state.StateStore, identity.Quadruple)
		want error
	}{
		{name: "missing lifecycle", seed: func(*testing.T, state.StateStore, identity.Quadruple) {}, want: sessionoverlay.ErrAgentLifecycleInactive},
		{name: "retired lifecycle", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			q, kind, err := agentcfg.LifecycleSlot(id.TenantID, overlayAgentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: agentcfg.ErrAgentRetired},
		{name: "corrupt lifecycle", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			q, kind, err := agentcfg.LifecycleSlot(id.TenantID, overlayAgentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "pending erasure", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, overlayAgentID)
			q, kind, err := sessionfence.PendingSlot(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"pending":true}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrSessionErased},
		{name: "terminal erasure", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, overlayAgentID)
			q, kind, err := sessionfence.TombstoneSlot(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"erased":true}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrSessionErased},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newState(t)
			id := quad("t", "u", "terminal")
			tc.seed(t, st, id)
			s, err := sessionoverlay.NewStore(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Get(context.Background(), id, overlayAgentID); !errors.Is(err, tc.want) {
				t.Fatalf("Get = %v, want %v", err, tc.want)
			}
			if _, err := s.SetUserPrompt(context.Background(), id, overlayAgentID, "refused"); !errors.Is(err, tc.want) {
				t.Fatalf("SetUserPrompt = %v, want %v", err, tc.want)
			}
			if _, loadErr := st.Load(context.Background(), id, sessionoverlay.LegacyOverlayKind(overlayAgentID)); !errors.Is(loadErr, state.ErrNotFound) {
				t.Fatalf("refused mutation wrote overlay: %v", loadErr)
			}
		})
	}
}

type overlayFenceChurnStore struct {
	state.StateStore
	id        identity.Quadruple
	agentID   string
	mu        sync.Mutex
	remaining int
	loads     int
}

func (s *overlayFenceChurnStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	rec, err := s.StateStore.Load(ctx, q, kind)
	if err != nil || q != s.id || kind != sessionoverlay.LegacyOverlayKind(s.agentID) {
		return rec, err
	}
	s.mu.Lock()
	s.loads++
	churn := s.remaining > 0
	if churn {
		s.remaining--
	}
	s.mu.Unlock()
	if churn {
		lifecycleQ, lifecycleKind, slotErr := agentcfg.LifecycleSlot(s.id.TenantID, s.agentID)
		if slotErr != nil {
			return state.StateRecord{}, slotErr
		}
		body := fmt.Sprintf(`{"schema":1,"revision_id":"active-%d","updated_at":"2026-08-02T00:00:00Z"}`, s.loads)
		if saveErr := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: lifecycleQ, Kind: lifecycleKind, Bytes: []byte(body)}); saveErr != nil {
			return state.StateRecord{}, saveErr
		}
	}
	return rec, nil
}

func TestStore_GetRetriesFenceChangeAndBoundsPerpetualChurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		churn     int
		wantError error
		wantLoads int
	}{
		{name: "one change", churn: 1, wantLoads: 2},
		{name: "perpetual", churn: sessionoverlay.MaxSessionSkillReadAttempts, wantError: sessionoverlay.ErrSessionSkillReadUnstable, wantLoads: sessionoverlay.MaxSessionSkillReadAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newState(t)
			id := quad("t", "u", "fence-churn")
			activateAgent(t, base, id, overlayAgentID)
			seed, err := sessionoverlay.NewStore(base, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.SetUserPrompt(context.Background(), id, overlayAgentID, "stable"); err != nil {
				t.Fatal(err)
			}
			wrapper := &overlayFenceChurnStore{StateStore: base, id: id, agentID: overlayAgentID, remaining: tc.churn}
			s, err := sessionoverlay.NewStore(wrapper, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, found, err := s.Get(context.Background(), id, overlayAgentID)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("Get error=%v, want %v", err, tc.wantError)
			}
			if tc.wantError == nil && (!found || got.UserPrompt != "stable") {
				t.Fatalf("stable Get = (%+v, %v)", got, found)
			}
			if wrapper.loads != tc.wantLoads {
				t.Fatalf("target loads=%d, want %d", wrapper.loads, tc.wantLoads)
			}
		})
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
