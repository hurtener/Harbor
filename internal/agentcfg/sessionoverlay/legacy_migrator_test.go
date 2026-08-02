package sessionoverlay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

type exactLegacyReader struct {
	mu         sync.Mutex
	rows       map[string]skills.Skill
	getCalls   int
	scopeCalls int
	failScope  error
}

type expectationRecordingStore struct {
	state.StateStore
	mu           sync.Mutex
	expectations []state.SlotExpectation
}

func (s *expectationRecordingStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if strings.HasPrefix(next.Kind, "agentcfg.session_personal.v1.") {
		s.mu.Lock()
		s.expectations = append([]state.SlotExpectation(nil), expectations...)
		s.mu.Unlock()
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (r *exactLegacyReader) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	return skills.Skill{}, errors.New("precedence Get must not be used by legacy migration")
}

func (r *exactLegacyReader) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return skills.Skill{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopeCalls++
	if r.failScope != nil {
		err := r.failScope
		r.failScope = nil
		return skills.Skill{}, err
	}
	row, ok := r.rows[legacyReaderKey(id, name, scope)]
	if !ok {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return row, nil
}

func (*exactLegacyReader) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return nil, errors.New("List must not be used by legacy migration")
}

func (*exactLegacyReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, errors.New("Search must not be used by legacy migration")
}

func legacyReaderKey(id identity.Quadruple, name string, scope skills.Scope) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID + "\x00" + string(scope) + "\x00" + name
}

func (r *exactLegacyReader) add(id identity.Quadruple, lookupName string, skill skills.Skill) {
	if r.rows == nil {
		r.rows = make(map[string]skills.Skill)
	}
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}
	r.rows[legacyReaderKey(id, lookupName, skills.ScopeSession)] = skill
}

func legacyDeclaration() config.SessionPersonalCutoverTenant {
	return config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch-1", RosterDigest: "roster", LegacyWritersDrained: true}
}

func legacyCandidate(t *testing.T, id identity.Quadruple, agentID string, names ...string) state.StateRecord {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Schema    int                    `json:"schema"`
		Overlay   sessionoverlay.Overlay `json:"overlay"`
		UpdatedAt time.Time              `json:"updated_at"`
	}{Schema: 1, Overlay: sessionoverlay.Overlay{PersonalSkills: names}, UpdatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: sessionoverlay.LegacyOverlayKind(agentID), Bytes: encoded}
}

func activateAgent(t *testing.T, st state.StateStore, id identity.Quadruple, agentID string) {
	t.Helper()
	q, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: kind,
		Bytes: []byte(`{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z"}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func newLegacyMigrator(t *testing.T, st state.StateStore, reader skills.SkillReader) (*sessionoverlay.ExactLegacyMigrator, *sessionoverlay.DurableStore) {
	t.Helper()
	personal, err := sessionoverlay.NewDurableStore(st, func() time.Time {
		return time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := sessionoverlay.NewExactLegacyMigrator(reader, personal)
	if err != nil {
		t.Fatal(err)
	}
	return migrator, personal
}

func TestExactLegacyMigrator_CopiesExactScopeCanonicalDedupAndRetriesIdempotently(t *testing.T) {
	base := newDurableState(t)
	st := &expectationRecordingStore{StateStore: base}
	id := durableID("session-copy")
	activateAgent(t, base, id, "agent-a")
	reader := &exactLegacyReader{}
	body := durableSkill("Alpha")
	reader.add(id, " alpha ", body)
	reader.add(id, "Alpha", body)
	migrator, personal := newLegacyMigrator(t, st, reader)
	candidate := legacyCandidate(t, id, "agent-a", " alpha ", "Alpha")
	verified, err := migrator.VerifyLegacyOverlay(context.Background(), candidate, legacyDeclaration())
	if err != nil || verified {
		t.Fatalf("VerifyLegacyOverlay before copy = (%v, %v), want (false, nil)", verified, err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		copied, err := migrator.CopyLegacyOverlay(context.Background(), candidate, legacyDeclaration())
		if err != nil || copied != 1 {
			t.Fatalf("attempt %d CopyLegacyOverlay = (%d, %v), want (1, nil)", attempt, copied, err)
		}
	}
	if reader.getCalls != 0 || reader.scopeCalls != 6 {
		t.Fatalf("reader calls: Get=%d GetScope=%d, want 0/6", reader.getCalls, reader.scopeCalls)
	}
	st.mu.Lock()
	expectations := append([]state.SlotExpectation(nil), st.expectations...)
	st.mu.Unlock()
	if len(expectations) != 4 {
		t.Fatalf("personal SaveIf expectations=%d, want target+lifecycle+pending+tombstone", len(expectations))
	}
	wantKinds := make(map[string]bool, 4)
	personalKind, err := sessionoverlay.PersonalSkillKind("agent-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds[personalKind] = true
	_, lifecycleKind, _ := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
	_, pendingKind, _ := sessionfence.PendingSlot(id)
	_, tombstoneKind, _ := sessionfence.TombstoneSlot(id)
	wantKinds[lifecycleKind] = true
	wantKinds[pendingKind] = true
	wantKinds[tombstoneKind] = true
	for _, expectation := range expectations {
		if !wantKinds[expectation.Kind] {
			t.Fatalf("unexpected personal SaveIf expectation kind %q", expectation.Kind)
		}
		delete(wantKinds, expectation.Kind)
	}
	if len(wantKinds) != 0 {
		t.Fatalf("missing personal SaveIf expectation kinds: %v", wantKinds)
	}
	got, found, err := personal.LoadPersonal(context.Background(), id, "agent-a", "alpha")
	if err != nil || !found {
		t.Fatalf("LoadPersonal = (%+v, %v, %v)", got, found, err)
	}
	hash := skills.CanonicalContentHash(body)
	if got.CopyEpoch != "epoch-1" || got.LegacyContentHash != hash || got.ContentHash != hash || got.CanonicalName != "alpha" {
		t.Fatalf("copy marker/body mismatch: %+v", got)
	}
	verified, err = migrator.VerifyLegacyOverlay(context.Background(), candidate, legacyDeclaration())
	if err != nil || !verified {
		t.Fatalf("VerifyLegacyOverlay = (%v, %v)", verified, err)
	}
}

func TestExactLegacyMigrator_RefusesMismatchTombstoneAndAliasDrift(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *sessionoverlay.DurableStore, identity.Quadruple)
	}{
		{name: "different epoch", seed: func(t *testing.T, personal *sessionoverlay.DurableStore, id identity.Quadruple) {
			body := durableSkill("same")
			if _, err := personal.SavePersonal(context.Background(), id, "agent-a", body, "old-epoch", skills.CanonicalContentHash(body)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tombstone", seed: func(t *testing.T, personal *sessionoverlay.DurableStore, id identity.Quadruple) {
			if _, err := personal.DeletePersonal(context.Background(), id, "agent-a", "same"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("session-conflict")
			activateAgent(t, st, id, "agent-a")
			reader := &exactLegacyReader{}
			reader.add(id, "same", durableSkill("same"))
			migrator, personal := newLegacyMigrator(t, st, reader)
			tc.seed(t, personal, id)
			_, err := migrator.CopyLegacyOverlay(context.Background(), legacyCandidate(t, id, "agent-a", "same"), legacyDeclaration())
			if !errors.Is(err, sessionoverlay.ErrLegacyCopyConflict) {
				t.Fatalf("CopyLegacyOverlay = %v, want ErrLegacyCopyConflict", err)
			}
		})
	}

	t.Run("canonical aliases with different bodies", func(t *testing.T) {
		st := newDurableState(t)
		id := durableID("session-alias")
		activateAgent(t, st, id, "agent-a")
		reader := &exactLegacyReader{}
		first := durableSkill("Same")
		second := durableSkill("same")
		second.Description = "different"
		reader.add(id, "Same", first)
		reader.add(id, "same", second)
		migrator, _ := newLegacyMigrator(t, st, reader)
		_, err := migrator.CopyLegacyOverlay(context.Background(), legacyCandidate(t, id, "agent-a", "Same", "same"), legacyDeclaration())
		if !errors.Is(err, sessionoverlay.ErrLegacyCopyConflict) {
			t.Fatalf("CopyLegacyOverlay = %v, want ErrLegacyCopyConflict", err)
		}
	})
}

func TestExactLegacyMigrator_TerminalFencesResolveWithoutLegacyRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, state.StateStore, identity.Quadruple)
	}{
		{name: "retired lifecycle", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pending erasure", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, "agent-a")
			q, kind, err := sessionfence.PendingSlot(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"pending":true}`)}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("terminal")
			tc.seed(t, st, id)
			reader := &exactLegacyReader{}
			migrator, _ := newLegacyMigrator(t, st, reader)
			candidate := legacyCandidate(t, id, "agent-a", "one", "two")
			resolved, err := migrator.CopyLegacyOverlay(context.Background(), candidate, legacyDeclaration())
			if err != nil || resolved != 2 {
				t.Fatalf("CopyLegacyOverlay = (%d, %v), want (2, nil)", resolved, err)
			}
			verified, err := migrator.VerifyLegacyOverlay(context.Background(), candidate, legacyDeclaration())
			if err != nil || !verified {
				t.Fatalf("VerifyLegacyOverlay = (%v, %v)", verified, err)
			}
			if reader.getCalls != 0 || reader.scopeCalls != 0 {
				t.Fatalf("terminal overlay reached legacy reader: Get=%d GetScope=%d", reader.getCalls, reader.scopeCalls)
			}
		})
	}
}

type commitThenPersonalErrorStore struct{ state.StateStore }

func (s commitThenPersonalErrorStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	if len(next.Kind) >= len("agentcfg.session_personal.v1.") && next.Kind[:len("agentcfg.session_personal.v1.")] == "agentcfg.session_personal.v1." {
		return errors.New("injected response loss after personal commit")
	}
	return nil
}

func TestExactLegacyMigrator_CommitThenErrorConvergesAndRestartResumes(t *testing.T) {
	t.Run("personal commit then error", func(t *testing.T) {
		base := newDurableState(t)
		id := durableID("commit-error")
		activateAgent(t, base, id, "agent-a")
		reader := &exactLegacyReader{}
		reader.add(id, "one", durableSkill("one"))
		migrator, _ := newLegacyMigrator(t, commitThenPersonalErrorStore{StateStore: base}, reader)
		resolved, err := migrator.CopyLegacyOverlay(context.Background(), legacyCandidate(t, id, "agent-a", "one"), legacyDeclaration())
		if err != nil || resolved != 1 {
			t.Fatalf("CopyLegacyOverlay = (%d, %v)", resolved, err)
		}
	})

	t.Run("reader fault then new controller", func(t *testing.T) {
		st := newDurableState(t)
		id := durableID("restart")
		activateAgent(t, st, id, "agent-a")
		candidate := legacyCandidate(t, id, "agent-a", "one")
		if err := st.Save(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
		reader := &exactLegacyReader{failScope: errors.New("injected exact-read fault")}
		reader.add(id, "one", durableSkill("one"))
		declaration := legacyDeclaration()
		controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
		if err != nil {
			t.Fatal(err)
		}
		migrator, _ := newLegacyMigrator(t, st, reader)
		mode, err := controller.Advance(context.Background(), "tenant", 1, migrator)
		if mode != sessionoverlay.CutoverDualRead || err == nil {
			t.Fatalf("faulted Advance = (%q, %v), want dual_read error", mode, err)
		}

		controller, err = sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
		if err != nil {
			t.Fatal(err)
		}
		migrator, _ = newLegacyMigrator(t, st, reader)
		mode, err = controller.Advance(context.Background(), "tenant", 1, migrator)
		if err != nil || mode != sessionoverlay.CutoverStateOnly {
			t.Fatalf("restarted Advance = (%q, %v)", mode, err)
		}
	})
}

func TestExactLegacyMigrator_ExactRawKindsAndStrictEnvelope(t *testing.T) {
	st := newDurableState(t)
	idA := durableID("kind-a")
	idAB := durableID("kind-ab")
	activateAgent(t, st, idA, "a")
	activateAgent(t, st, idAB, "ab")
	reader := &exactLegacyReader{}
	reader.add(idA, "only-a", durableSkill("only-a"))
	reader.add(idAB, "only-ab", durableSkill("only-ab"))
	migrator, personal := newLegacyMigrator(t, st, reader)
	if _, err := migrator.CopyLegacyOverlay(context.Background(), legacyCandidate(t, idA, "a", "only-a"), legacyDeclaration()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := personal.LoadPersonal(context.Background(), idAB, "ab", "only-a"); err != nil || found {
		t.Fatalf("agent a overmatched adjacent ab kind: found=%v err=%v", found, err)
	}
	if _, err := migrator.CopyLegacyOverlay(context.Background(), legacyCandidate(t, idAB, "ab", "only-ab"), legacyDeclaration()); err != nil {
		t.Fatal(err)
	}

	malformed := legacyCandidate(t, idA, "a", "only-a")
	malformed.Bytes = []byte(`{"schema":1,"schema":1,"overlay":{"personal_skills":["only-a"]},"updated_at":"2026-08-02T00:00:00Z"}`)
	if _, err := migrator.CopyLegacyOverlay(context.Background(), malformed, legacyDeclaration()); !errors.Is(err, sessionoverlay.ErrLegacyOverlayInvalid) {
		t.Fatalf("duplicate envelope = %v, want ErrLegacyOverlayInvalid", err)
	}

	badBody := legacyCandidate(t, idA, "a", "only-a")
	reader.add(idA, "only-a", skills.Skill{Name: "other", Trigger: "x", Steps: []string{"x"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession})
	if _, err := migrator.CopyLegacyOverlay(context.Background(), badBody, legacyDeclaration()); !errors.Is(err, sessionoverlay.ErrLegacySkillInvalid) {
		t.Fatalf("mismatched body = %v, want ErrLegacySkillInvalid", err)
	}
}

func TestNewExactLegacyMigrator_RequiresDependencies(t *testing.T) {
	personal, err := sessionoverlay.NewDurableStore(newDurableState(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionoverlay.NewExactLegacyMigrator(nil, personal); !errors.Is(err, sessionoverlay.ErrInvalidConfig) {
		t.Fatalf("nil reader = %v", err)
	}
	reader := &exactLegacyReader{}
	if _, err := sessionoverlay.NewExactLegacyMigrator(reader, nil); !errors.Is(err, sessionoverlay.ErrInvalidConfig) {
		t.Fatalf("nil personal = %v", err)
	}
}

func TestExactLegacyMigrator_ConcurrentReuse_D025(t *testing.T) {
	st := newDurableState(t)
	const goroutines = 128
	reader := &exactLegacyReader{}
	ids := make([]identity.Quadruple, goroutines)
	for i := range goroutines {
		ids[i] = durableID(fmt.Sprintf("d025-%03d", i))
		reader.add(ids[i], fmt.Sprintf("skill-%03d", i), durableSkill(fmt.Sprintf("skill-%03d", i)))
	}
	activateAgent(t, st, ids[0], "agent-a")
	migrator, personal := newLegacyMigrator(t, st, reader)
	baseline := runtime.NumGoroutine()

	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%7 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			name := fmt.Sprintf("skill-%03d", i)
			resolved, err := migrator.CopyLegacyOverlay(ctx, legacyCandidate(t, ids[i], "agent-a", name), legacyDeclaration())
			if i%7 == 0 {
				if !errors.Is(err, context.Canceled) {
					if err == nil {
						errCh <- fmt.Errorf("run %d: cancelled call returned nil error", i)
					} else {
						errCh <- fmt.Errorf("run %d: cancelled error: %w", i, err)
					}
				}
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("run %d: copy: %w", i, err)
				return
			}
			if resolved != 1 {
				errCh <- fmt.Errorf("run %d: resolved=%d, want 1", i, resolved)
				return
			}
			record, found, err := personal.LoadPersonal(context.Background(), ids[i], "agent-a", name)
			if err != nil {
				errCh <- fmt.Errorf("run %d: load personal: %w", i, err)
				return
			}
			if !found || record.CanonicalName != name {
				errCh <- fmt.Errorf("run %d: personal record=%+v found=%v", i, record, found)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 4 {
		t.Fatalf("goroutine leak: baseline=%d final=%d delta=%d", baseline, runtime.NumGoroutine(), delta)
	}
}
