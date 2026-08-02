package sessionoverlay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

type sessionPersonalControllerContract interface {
	SessionSkills(context.Context, identity.Quadruple, string) ([]skills.Skill, error)
	UpsertSessionSkill(context.Context, identity.Quadruple, string, skills.Skill) error
	DeleteSessionSkill(context.Context, identity.Quadruple, string, string) error
}

var _ sessionPersonalControllerContract = (*sessionoverlay.SessionPersonalController)(nil)

type controllerModeReader struct {
	mode sessionoverlay.CutoverMode
	err  error
}

type controllerExtraLeaf struct {
	Value  string   `json:"value"`
	Labels []string `json:"labels"`
}

type controllerExtraEnvelope struct {
	Leaf *controllerExtraLeaf `json:"leaf"`
}

func (r controllerModeReader) Mode(ctx context.Context, _ string) (sessionoverlay.CutoverMode, error) {
	if err := ctx.Err(); err != nil {
		return sessionoverlay.CutoverDualRead, err
	}
	return r.mode, r.err
}

func newSessionPersonalController(t *testing.T, st state.StateStore, mode sessionoverlay.CutoverMode, reader skills.SkillReader) (*sessionoverlay.SessionPersonalController, *sessionoverlay.DurableStore) {
	t.Helper()
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := sessionoverlay.NewSessionPersonalController(personal, controllerModeReader{mode: mode}, reader)
	if err != nil {
		t.Fatal(err)
	}
	return controller, personal
}

func TestNewSessionPersonalController_RequiresAllDependencies(t *testing.T) {
	st := newDurableState(t)
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &exactLegacyReader{}
	mode := controllerModeReader{mode: sessionoverlay.CutoverDualRead}
	for _, tc := range []struct {
		name     string
		personal *sessionoverlay.DurableStore
		mode     sessionoverlay.CutoverModeReader
		reader   skills.SkillReader
	}{
		{name: "personal", mode: mode, reader: reader},
		{name: "cutover", personal: personal, reader: reader},
		{name: "legacy", personal: personal, mode: mode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sessionoverlay.NewSessionPersonalController(tc.personal, tc.mode, tc.reader); !errors.Is(err, sessionoverlay.ErrInvalidConfig) {
				t.Fatalf("constructor error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestSessionPersonalController_DualReadExactAgentTripleAndImmutableBodies(t *testing.T) {
	st := newDurableState(t)
	id := durableID("dual-controller")
	id.RunID = "run-writer"
	activateAgent(t, st, id, "agent-a")
	activateAgent(t, st, id, "agent-b")
	reader := &exactLegacyReader{}
	a := durableSkill("Alpha")
	a.Tags = []string{"original"}
	a.Extra = map[string]any{"nested": map[string]any{"value": "original"}}
	b := durableSkill("Beta")
	reader.add(id, "Alpha", a)
	reader.add(id, "Beta", b)
	reader.add(id, "user-only", func() skills.Skill {
		skill := durableSkill("user-only")
		skill.Scope = skills.ScopeUser
		skill.ContentHash = skills.CanonicalContentHash(skill)
		return skill
	}())
	legacyID := id
	legacyID.RunID = ""
	if err := st.Save(context.Background(), legacyCandidate(t, legacyID, "agent-a", "Alpha")); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), legacyCandidate(t, legacyID, "agent-b", "Beta")); err != nil {
		t.Fatal(err)
	}
	controller, _ := newSessionPersonalController(t, st, sessionoverlay.CutoverDualRead, reader)

	got, err := controller.SessionSkills(context.Background(), id, "agent-a")
	if err != nil || fmt.Sprint(skillNames(got)) != "[Alpha]" || got[0].Scope != skills.ScopeSession {
		t.Fatalf("agent-a SessionSkills = (%v, %v)", skillNames(got), err)
	}
	got[0].Tags[0] = "mutated"
	got[0].Extra["nested"].(map[string]any)["value"] = "mutated"
	reloaded, err := controller.SessionSkills(context.Background(), identity.Quadruple{Identity: id.Identity, RunID: "different-run"}, "agent-a")
	if err != nil || reloaded[0].Tags[0] != "original" || reloaded[0].Extra["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("immutable reload = (%+v, %v)", reloaded, err)
	}
	otherAgent, err := controller.SessionSkills(context.Background(), id, "agent-b")
	if err != nil || fmt.Sprint(skillNames(otherAgent)) != "[Beta]" {
		t.Fatalf("agent-b SessionSkills = (%v, %v)", skillNames(otherAgent), err)
	}
	otherTenant := id
	otherTenant.TenantID = "other-tenant"
	activateAgent(t, st, otherTenant, "agent-a")
	for _, otherTuple := range []identity.Quadruple{
		{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: "other-session"}},
		{Identity: identity.Identity{TenantID: id.TenantID, UserID: "other-user", SessionID: id.SessionID}},
		{Identity: identity.Identity{TenantID: "other-tenant", UserID: id.UserID, SessionID: id.SessionID}},
	} {
		other, err := controller.SessionSkills(context.Background(), otherTuple, "agent-a")
		if err != nil || len(other) != 0 {
			t.Fatalf("cross-tuple %+v SessionSkills = (%v, %v), want empty", otherTuple, skillNames(other), err)
		}
	}
}

func TestSessionPersonalController_NormalizesStructAndPointerExtraWithoutAliases(t *testing.T) {
	st := newDurableState(t)
	id := durableID("state-controller-extra")
	activateAgent(t, st, id, "agent-a")
	leaf := &controllerExtraLeaf{Value: "original", Labels: []string{"one"}}
	body := durableSkill("structured")
	body.Extra = map[string]any{
		"struct":  controllerExtraEnvelope{Leaf: leaf},
		"pointer": leaf,
	}
	controller, _ := newSessionPersonalController(t, st, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	if err := controller.UpsertSessionSkill(t.Context(), id, "agent-a", body); err != nil {
		t.Fatal(err)
	}
	leaf.Value = "source-mutated"
	leaf.Labels[0] = "source-mutated"

	got, err := controller.SessionSkills(t.Context(), id, "agent-a")
	if err != nil || len(got) != 1 {
		t.Fatalf("SessionSkills = (%+v, %v)", got, err)
	}
	structLeaf := got[0].Extra["struct"].(map[string]any)["leaf"].(map[string]any)
	pointerLeaf := got[0].Extra["pointer"].(map[string]any)
	if structLeaf["value"] != "original" || pointerLeaf["labels"].([]any)[0] != "one" {
		t.Fatalf("persisted Extra retained caller aliases: %+v", got[0].Extra)
	}
	structLeaf["value"] = "returned-struct-mutation"
	pointerLeaf["labels"].([]any)[0] = "returned-pointer-mutation"
	if leaf.Value != "source-mutated" || leaf.Labels[0] != "source-mutated" {
		t.Fatalf("returned normalized Extra mutated source pointer: %+v", leaf)
	}

	reloaded, err := controller.SessionSkills(t.Context(), id, "agent-a")
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("reloaded SessionSkills = (%+v, %v)", reloaded, err)
	}
	reloadedStruct := reloaded[0].Extra["struct"].(map[string]any)["leaf"].(map[string]any)
	reloadedPointer := reloaded[0].Extra["pointer"].(map[string]any)
	if reloadedStruct["value"] != "original" || reloadedPointer["labels"].([]any)[0] != "one" {
		t.Fatalf("normalized Extra leaked returned aliases: %+v", reloaded[0].Extra)
	}
}

func TestSessionPersonalController_RejectsCyclicAndUnsupportedExtra(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"cycle": func() map[string]any {
			value := map[string]any{}
			value["self"] = value
			return value
		}(),
		"unsupported": {"function": func() {}},
	} {
		t.Run(name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("state-controller-extra-" + name)
			activateAgent(t, st, id, "agent-a")
			body := durableSkill("bad-extra")
			body.Extra = extra
			controller, _ := newSessionPersonalController(t, st, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
			if err := controller.UpsertSessionSkill(t.Context(), id, "agent-a", body); err == nil {
				t.Fatal("UpsertSessionSkill accepted non-JSON Extra")
			}
			if got, err := controller.SessionSkills(t.Context(), id, "agent-a"); err != nil || len(got) != 0 {
				t.Fatalf("failed upsert changed session tier = (%+v, %v)", got, err)
			}
		})
	}
}

func TestSessionPersonalController_DualReadFailsLoudForMissingAndMalformedBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, state.StateStore, identity.Quadruple, *exactLegacyReader)
		want error
	}{
		{name: "missing body", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple, _ *exactLegacyReader) {
			if err := st.Save(context.Background(), legacyCandidate(t, id, "agent-a", "missing")); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrLegacySkillInvalid},
		{name: "bad content hash", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple, reader *exactLegacyReader) {
			body := durableSkill("bad-hash")
			body.ContentHash = strings.Repeat("0", 64)
			reader.add(id, "bad-hash", body)
			reader.rows[legacyReaderKey(id, "bad-hash", skills.ScopeSession)] = body
			if err := st.Save(context.Background(), legacyCandidate(t, id, "agent-a", "bad-hash")); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrLegacySkillInvalid},
		{name: "malformed overlay", seed: func(t *testing.T, st state.StateStore, id identity.Quadruple, _ *exactLegacyReader) {
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: sessionoverlay.LegacyOverlayKind("agent-a"), Bytes: []byte(`{"schema":1,"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrLegacyOverlayInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("dual-invalid")
			activateAgent(t, st, id, "agent-a")
			reader := &exactLegacyReader{}
			tc.seed(t, st, id, reader)
			controller, _ := newSessionPersonalController(t, st, sessionoverlay.CutoverDualRead, reader)
			if _, err := controller.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, tc.want) {
				t.Fatalf("SessionSkills = %v, want %v", err, tc.want)
			}
		})
	}
}

type cutoverBlockingLegacyReader struct {
	*exactLegacyReader
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (r *cutoverBlockingLegacyReader) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if r.blocked.CompareAndSwap(false, true) {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return skills.Skill{}, ctx.Err()
		}
	}
	return r.exactLegacyReader.GetScope(ctx, id, name, scope)
}

type controllerTransitionMigrator struct{}

func (controllerTransitionMigrator) CopyLegacyOverlay(context.Context, state.StateRecord, config.SessionPersonalCutoverTenant) (int, error) {
	return 0, nil
}

func (controllerTransitionMigrator) VerifyLegacyOverlay(context.Context, state.StateRecord, config.SessionPersonalCutoverTenant) (bool, error) {
	return true, nil
}

type alternatingControllerModeReader struct {
	calls atomic.Int32
}

func (r *alternatingControllerModeReader) Mode(ctx context.Context, _ string) (sessionoverlay.CutoverMode, error) {
	if err := ctx.Err(); err != nil {
		return sessionoverlay.CutoverDualRead, err
	}
	if r.calls.Add(1)%2 == 0 {
		return sessionoverlay.CutoverStateOnly, nil
	}
	return sessionoverlay.CutoverDualRead, nil
}

func TestSessionPersonalController_CutoverCASDuringEnumerationRetriesToStateOnly(t *testing.T) {
	st := newDurableState(t)
	id := durableID("controller-cutover-race")
	activateAgent(t, st, id, "agent-a")
	legacy := &cutoverBlockingLegacyReader{
		exactLegacyReader: &exactLegacyReader{},
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	legacy.add(id, "legacy", durableSkill("legacy"))
	if err := st.Save(t.Context(), legacyCandidate(t, id, "agent-a", "legacy")); err != nil {
		t.Fatal(err)
	}
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(t.Context(), id, "agent-a", durableSkill("owned"), "", ""); err != nil {
		t.Fatal(err)
	}
	declaration := config.SessionPersonalCutoverTenant{
		TenantID: id.TenantID, Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true,
	}
	cutover, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := cutover.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	controller, err := sessionoverlay.NewSessionPersonalController(personal, cutover, legacy)
	if err != nil {
		t.Fatal(err)
	}

	type readResult struct {
		skills []skills.Skill
		err    error
	}
	result := make(chan readResult, 1)
	go func() {
		rows, readErr := controller.SessionSkills(context.Background(), id, "agent-a")
		result <- readResult{skills: rows, err: readErr}
	}()
	select {
	case <-legacy.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dual-read enumeration did not reach the legacy body")
	}
	t.Cleanup(func() {
		select {
		case <-legacy.release:
		default:
			close(legacy.release)
		}
	})
	mode, err := cutover.Advance(t.Context(), id.TenantID, 8, controllerTransitionMigrator{})
	if err != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("Advance = (%q, %v), want state_only", mode, err)
	}
	close(legacy.release)
	select {
	case got := <-result:
		if got.err != nil || fmt.Sprint(skillNames(got.skills)) != "[owned]" {
			t.Fatalf("SessionSkills after cutover CAS = (%v, %v), want owned state-only view", skillNames(got.skills), got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SessionSkills did not complete after cutover CAS")
	}
}

func TestSessionPersonalController_PerpetualCutoverModeChurnIsBounded(t *testing.T) {
	st := newDurableState(t)
	id := durableID("controller-cutover-churn")
	activateAgent(t, st, id, "agent-a")
	legacy := &exactLegacyReader{}
	legacy.add(id, "legacy", durableSkill("legacy"))
	if err := st.Save(t.Context(), legacyCandidate(t, id, "agent-a", "legacy")); err != nil {
		t.Fatal(err)
	}
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	mode := &alternatingControllerModeReader{}
	controller, err := sessionoverlay.NewSessionPersonalController(personal, mode, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SessionSkills(t.Context(), id, "agent-a"); !errors.Is(err, sessionoverlay.ErrSessionSkillReadUnstable) {
		t.Fatalf("SessionSkills = %v, want ErrSessionSkillReadUnstable", err)
	}
	if got, want := mode.calls.Load(), int32(2*sessionoverlay.MaxSessionSkillReadAttempts); got != want {
		t.Fatalf("cutover mode reads = %d, want %d", got, want)
	}
}

func TestSessionPersonalController_StateOnlyOwnedMembershipTombstonesAndRunAgnosticStorage(t *testing.T) {
	st := newDurableState(t)
	id := durableID("owned-controller")
	id.RunID = "writer-run"
	activateAgent(t, st, id, "agent-a")
	activateAgent(t, st, id, "agent-ab")
	controller, personal := newSessionPersonalController(t, st, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", durableSkill("zeta")); err != nil {
		t.Fatal(err)
	}
	if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", durableSkill("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := controller.DeleteSessionSkill(context.Background(), id, "agent-a", "zeta"); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(context.Background(), id, "agent-ab", durableSkill("other-agent"), "", ""); err != nil {
		t.Fatal(err)
	}
	readID := identity.Quadruple{Identity: id.Identity, RunID: "reader-run"}
	got, err := controller.SessionSkills(context.Background(), readID, "agent-a")
	if err != nil || fmt.Sprint(skillNames(got)) != "[alpha]" {
		t.Fatalf("owned SessionSkills = (%v, %v)", skillNames(got), err)
	}
	for _, skill := range got {
		if skill.Scope != skills.ScopeSession {
			t.Fatalf("owned scope = %q, want session", skill.Scope)
		}
	}
	record, found, err := personal.LoadPersonal(context.Background(), readID, "agent-a", "alpha")
	if err != nil || !found || record.CopyEpoch != "" || record.LegacyContentHash != "" {
		t.Fatalf("owned record markers = (%+v, %v, %v)", record, found, err)
	}
}

func TestSessionPersonalController_StateOnlyRejectsMalformedOwnedRecord(t *testing.T) {
	st := newDurableState(t)
	id := durableID("owned-malformed")
	activateAgent(t, st, id, "agent-a")
	controller, personal := newSessionPersonalController(t, st, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", durableSkill("owned"), "", ""); err != nil {
		t.Fatal(err)
	}
	kind, err := sessionoverlay.PersonalSkillKind("agent-a", "owned")
	if err != nil {
		t.Fatal(err)
	}
	record, err := st.Load(context.Background(), durableNoRun(id), kind)
	if err != nil {
		t.Fatal(err)
	}
	record.ID = state.NewEventID()
	record.Bytes = bytes.Replace(record.Bytes, []byte(`"canonical_name":"owned"`), []byte(`"canonical_name":"owned","canonical_name":"owned"`), 1)
	if err := st.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, sessionoverlay.ErrPersonalRecordInvalid) {
		t.Fatalf("malformed owned SessionSkills = %v, want ErrPersonalRecordInvalid", err)
	}
}

type controllerSaveCounter struct {
	state.StateStore
	personalWrites atomic.Int32
}

func (s *controllerSaveCounter) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if strings.HasPrefix(next.Kind, "agentcfg.session_personal.v1.") {
		s.personalWrites.Add(1)
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestSessionPersonalController_MutationsRequireStateOnlyBeforeAnyWrite(t *testing.T) {
	base := newDurableState(t)
	id := durableID("pending-controller")
	activateAgent(t, base, id, "agent-a")
	counted := &controllerSaveCounter{StateStore: base}
	controller, _ := newSessionPersonalController(t, counted, sessionoverlay.CutoverDualRead, &exactLegacyReader{})
	if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", durableSkill("refused")); !errors.Is(err, sessionoverlay.ErrCutoverPending) {
		t.Fatalf("dual upsert = %v, want ErrCutoverPending", err)
	}
	if err := controller.DeleteSessionSkill(context.Background(), id, "agent-a", "refused"); !errors.Is(err, sessionoverlay.ErrCutoverPending) {
		t.Fatalf("dual delete = %v, want ErrCutoverPending", err)
	}
	if got := counted.personalWrites.Load(); got != 0 {
		t.Fatalf("dual mutations attempted %d personal writes", got)
	}
}

type controllerCommitLossStore struct {
	state.StateStore
	once sync.Once
}

func (s *controllerCommitLossStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	fired := false
	if strings.HasPrefix(next.Kind, "agentcfg.session_personal.v1.") {
		s.once.Do(func() { fired = true })
	}
	if fired {
		return errors.New("injected response loss after personal commit")
	}
	return nil
}

func TestSessionPersonalController_CommitLossConvergesAndRestartReads(t *testing.T) {
	base := newDurableState(t)
	id := durableID("controller-restart")
	activateAgent(t, base, id, "agent-a")
	first, _ := newSessionPersonalController(t, &controllerCommitLossStore{StateStore: base}, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	if err := first.UpsertSessionSkill(context.Background(), id, "agent-a", durableSkill("survives")); err != nil {
		t.Fatalf("commit-loss upsert: %v", err)
	}
	restarted, _ := newSessionPersonalController(t, base, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	got, err := restarted.SessionSkills(context.Background(), id, "agent-a")
	if err != nil || fmt.Sprint(skillNames(got)) != "[survives]" {
		t.Fatalf("restart SessionSkills = (%v, %v)", skillNames(got), err)
	}
	deleteController, _ := newSessionPersonalController(t, &controllerCommitLossStore{StateStore: base}, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	if err := deleteController.DeleteSessionSkill(context.Background(), id, "agent-a", "survives"); err != nil {
		t.Fatalf("commit-loss delete: %v", err)
	}
	got, err = restarted.SessionSkills(context.Background(), id, "agent-a")
	if err != nil || len(got) != 0 {
		t.Fatalf("post-delete restart SessionSkills = (%v, %v)", skillNames(got), err)
	}
}

type controllerCASBarrier struct {
	state.StateStore
	entered chan struct{}
	release chan struct{}
}

func (s *controllerCASBarrier) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if strings.HasPrefix(next.Kind, "agentcfg.session_personal.v1.") {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestSessionPersonalController_SameSlotCASHasOneWinner(t *testing.T) {
	base := newDurableState(t)
	id := durableID("controller-cas")
	activateAgent(t, base, id, "agent-a")
	const n = 128
	barrier := &controllerCASBarrier{StateStore: base, entered: make(chan struct{}, n), release: make(chan struct{})}
	controller, _ := newSessionPersonalController(t, barrier, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			skill := durableSkill("one-name")
			skill.Description = fmt.Sprintf("writer-%03d", i)
			errs <- controller.UpsertSessionSkill(context.Background(), id, "agent-a", skill)
		}(i)
	}
	for range n {
		<-barrier.entered
	}
	close(barrier.release)
	wg.Wait()
	close(errs)
	winners, losers := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, state.ErrConditionFailed):
			losers++
		default:
			t.Errorf("unexpected CAS result: %v", err)
		}
	}
	if winners != 1 || losers != n-1 {
		t.Fatalf("CAS winners/losers = %d/%d, want 1/%d", winners, losers, n-1)
	}
}

type controllerFenceChurnStore struct {
	state.StateStore
	id        identity.Quadruple
	agentID   string
	remaining atomic.Int32
	loads     atomic.Int32
	cancel    context.CancelFunc
}

func (s *controllerFenceChurnStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if err != nil || q != durableNoRun(s.id) || kind != sessionoverlay.LegacyOverlayKind(s.agentID) {
		return record, err
	}
	s.loads.Add(1)
	if s.cancel != nil {
		s.cancel()
	}
	if s.remaining.Add(-1) >= 0 {
		lifecycleQ, lifecycleKind, slotErr := agentcfg.LifecycleSlot(s.id.TenantID, s.agentID)
		if slotErr != nil {
			return state.StateRecord{}, slotErr
		}
		body := []byte(fmt.Sprintf(`{"schema":1,"revision_id":"change-%d","updated_at":"2026-08-02T00:00:00Z"}`, s.loads.Load()))
		if saveErr := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: lifecycleQ, Kind: lifecycleKind, Bytes: body}); saveErr != nil {
			return state.StateRecord{}, saveErr
		}
	}
	return record, nil
}

func durableNoRun(id identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: id.Identity}
}

func TestSessionPersonalController_ReadRetriesBoundsAndHonorsCancellation(t *testing.T) {
	seed := func(t *testing.T) (state.StateStore, identity.Quadruple, *exactLegacyReader) {
		t.Helper()
		base := newDurableState(t)
		id := durableID("controller-churn")
		activateAgent(t, base, id, "agent-a")
		reader := &exactLegacyReader{}
		reader.add(id, "stable", durableSkill("stable"))
		if err := base.Save(context.Background(), legacyCandidate(t, id, "agent-a", "stable")); err != nil {
			t.Fatal(err)
		}
		return base, id, reader
	}
	t.Run("one retry", func(t *testing.T) {
		base, id, reader := seed(t)
		wrapper := &controllerFenceChurnStore{StateStore: base, id: id, agentID: "agent-a"}
		wrapper.remaining.Store(1)
		controller, _ := newSessionPersonalController(t, wrapper, sessionoverlay.CutoverDualRead, reader)
		got, err := controller.SessionSkills(context.Background(), id, "agent-a")
		if err != nil || fmt.Sprint(skillNames(got)) != "[stable]" || wrapper.loads.Load() != 2 {
			t.Fatalf("retry read = (%v, %v), loads=%d", skillNames(got), err, wrapper.loads.Load())
		}
	})
	t.Run("perpetual", func(t *testing.T) {
		base, id, reader := seed(t)
		wrapper := &controllerFenceChurnStore{StateStore: base, id: id, agentID: "agent-a"}
		wrapper.remaining.Store(sessionoverlay.MaxSessionSkillReadAttempts)
		controller, _ := newSessionPersonalController(t, wrapper, sessionoverlay.CutoverDualRead, reader)
		if _, err := controller.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, sessionoverlay.ErrSessionSkillReadUnstable) {
			t.Fatalf("perpetual read = %v, want ErrSessionSkillReadUnstable", err)
		}
		if got := wrapper.loads.Load(); got != sessionoverlay.MaxSessionSkillReadAttempts {
			t.Fatalf("target loads = %d, want %d", got, sessionoverlay.MaxSessionSkillReadAttempts)
		}
	})
	t.Run("mid-read cancellation", func(t *testing.T) {
		base, id, reader := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		wrapper := &controllerFenceChurnStore{StateStore: base, id: id, agentID: "agent-a", cancel: cancel}
		wrapper.remaining.Store(-1)
		controller, _ := newSessionPersonalController(t, wrapper, sessionoverlay.CutoverDualRead, reader)
		if _, err := controller.SessionSkills(ctx, id, "agent-a"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read = %v, want context.Canceled", err)
		}
	})
}

func TestSessionPersonalController_FailsLoudForLifecycleErasureAndUnknownMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode sessionoverlay.CutoverMode
		seed func(*testing.T, state.StateStore, identity.Quadruple)
		want error
	}{
		{name: "missing lifecycle", mode: sessionoverlay.CutoverStateOnly, seed: func(*testing.T, state.StateStore, identity.Quadruple) {}, want: sessionoverlay.ErrAgentLifecycleInactive},
		{name: "retired lifecycle", mode: sessionoverlay.CutoverStateOnly, seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: agentcfg.ErrAgentRetired},
		{name: "corrupt lifecycle", mode: sessionoverlay.CutoverStateOnly, seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "pending erasure", mode: sessionoverlay.CutoverStateOnly, seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, "agent-a")
			q, kind, err := sessionfence.PendingSlot(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"pending":true}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrSessionErased},
		{name: "terminal erasure", mode: sessionoverlay.CutoverStateOnly, seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, "agent-a")
			q, kind, err := sessionfence.TombstoneSlot(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"erased":true}`)}); err != nil {
				t.Fatal(err)
			}
		}, want: sessionoverlay.ErrSessionErased},
		{name: "unknown mode", mode: sessionoverlay.CutoverMode("future"), seed: func(t *testing.T, st state.StateStore, id identity.Quadruple) {
			activateAgent(t, st, id, "agent-a")
		}, want: sessionoverlay.ErrCutoverRecordInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("controller-terminal")
			tc.seed(t, st, id)
			controller, _ := newSessionPersonalController(t, st, tc.mode, &exactLegacyReader{})
			if _, err := controller.SessionSkills(context.Background(), id, "agent-a"); !errors.Is(err, tc.want) {
				t.Fatalf("SessionSkills = %v, want %v", err, tc.want)
			}
			if err := controller.UpsertSessionSkill(context.Background(), id, "agent-a", durableSkill("refused")); !errors.Is(err, tc.want) {
				t.Fatalf("UpsertSessionSkill = %v, want %v", err, tc.want)
			}
			if err := controller.DeleteSessionSkill(context.Background(), id, "agent-a", "refused"); !errors.Is(err, tc.want) {
				t.Fatalf("DeleteSessionSkill = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSessionPersonalController_ConcurrentSharedInstanceIsolationAndCancellation(t *testing.T) {
	base := newDurableState(t)
	seed := durableID("controller-shared")
	activateAgent(t, base, seed, "agent-a")
	controller, _ := newSessionPersonalController(t, base, sessionoverlay.CutoverStateOnly, &exactLegacyReader{})
	const n = 128
	ids := make([]identity.Quadruple, n)
	for i := range n {
		ids[i] = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: fmt.Sprintf("user-%03d", i), SessionID: fmt.Sprintf("session-%03d", i)}, RunID: fmt.Sprintf("writer-%03d", i)}
		skill := durableSkill(fmt.Sprintf("skill-%03d", i))
		skill.Tags = []string{"original"}
		if err := controller.UpsertSessionSkill(context.Background(), ids[i], "agent-a", skill); err != nil {
			t.Fatal(err)
		}
	}
	baseline := runtime.NumGoroutine()
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			readID := identity.Quadruple{Identity: ids[i].Identity, RunID: fmt.Sprintf("reader-%03d", i)}
			got, err := controller.SessionSkills(ctx, readID, "agent-a")
			if i%2 == 0 {
				if !errors.Is(err, context.Canceled) {
					if err == nil {
						errs <- fmt.Errorf("run %d canceled read returned nil error", i)
					} else {
						errs <- fmt.Errorf("run %d canceled read: %w", i, err)
					}
				}
				return
			}
			want := fmt.Sprintf("skill-%03d", i)
			if err != nil {
				errs <- fmt.Errorf("run %d read: %w", i, err)
				return
			}
			if len(got) != 1 || got[0].Name != want {
				errs <- fmt.Errorf("run %d rows = %v, want [%s]", i, skillNames(got), want)
				return
			}
			got[0].Tags[0] = "mutated"
			reloaded, reloadErr := controller.SessionSkills(context.Background(), readID, "agent-a")
			if reloadErr != nil {
				errs <- fmt.Errorf("run %d immutable reload: %w", i, reloadErr)
			} else if len(reloaded) != 1 || reloaded[0].Tags[0] != "original" {
				errs <- fmt.Errorf("run %d immutable reload rows = %v", i, skillNames(reloaded))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutine baseline not restored: got=%d baseline=%d", got, baseline)
	}
}
