package sessionoverlay_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func newDurableState(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("new StateStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func durableID(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: session}}
}

func durableSkill(name string) skills.Skill {
	return skills.Skill{Name: name, Trigger: "when needed", Steps: []string{"do it"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
}

func activate(t *testing.T, st state.StateStore, id identity.Quadruple) {
	t.Helper()
	q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"active-revision"}`)}); err != nil {
		t.Fatalf("seed active lifecycle: %v", err)
	}
}

func TestDurableSlots_ExactControlIdentities(t *testing.T) {
	id := durableID("session-a")
	lifecycleQ, lifecycleKind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
	if err != nil {
		t.Fatalf("LifecycleSlot: %v", err)
	}
	if lifecycleQ.TenantID != "tenant" || lifecycleQ.UserID != "__agentcfg__" || lifecycleQ.SessionID != "agent-a" || lifecycleKind != "agentcfg.active" {
		t.Fatalf("lifecycle slot = (%+v, %q)", lifecycleQ, lifecycleKind)
	}
	pendingQ, pendingKind, err := sessionfence.PendingSlot(id)
	if err != nil {
		t.Fatalf("PendingSlot: %v", err)
	}
	tombstoneQ, tombstoneKind, err := sessionfence.TombstoneSlot(id)
	if err != nil {
		t.Fatalf("TombstoneSlot: %v", err)
	}
	for _, got := range []struct {
		q    identity.Quadruple
		kind string
	}{{pendingQ, pendingKind}, {tombstoneQ, tombstoneKind}} {
		if got.q.TenantID != "tenant" || got.q.UserID != "user" || got.q.SessionID != "<erasure-audit>" || got.q.RunID != "" {
			t.Fatalf("erasure scope = %+v", got.q)
		}
	}
	if pendingKind != "session.erasure.pending.session-a" || tombstoneKind != "session.erasure.tombstone.session-a" {
		t.Fatalf("erasure kinds = %q, %q", pendingKind, tombstoneKind)
	}
}

func TestDurableStore_PersonalRecordCASAndTombstone(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatalf("NewDurableStore: %v", err)
	}
	id := durableID("session-a")
	activate(t, st, id)
	first, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("Alpha"), "epoch-1", "legacy-hash")
	if err != nil {
		t.Fatalf("SavePersonal: %v", err)
	}
	if first.CanonicalName != "alpha" || first.ContentHash == "" || first.CopyEpoch != "epoch-1" {
		t.Fatalf("saved record = %+v", first)
	}
	loaded, found, err := store.LoadPersonal(context.Background(), id, "agent-a", "ALPHA")
	if err != nil || !found || loaded.Skill.Name != "Alpha" {
		t.Fatalf("LoadPersonal = (%+v, %v, %v)", loaded, found, err)
	}
	tombstone, err := store.DeletePersonal(context.Background(), id, "agent-a", "alpha")
	if err != nil || !tombstone.Deleted {
		t.Fatalf("DeletePersonal = (%+v, %v)", tombstone, err)
	}
	loaded, found, err = store.LoadPersonal(context.Background(), id, "agent-a", "alpha")
	if err != nil || !found || !loaded.Deleted {
		t.Fatalf("tombstone LoadPersonal = (%+v, %v, %v)", loaded, found, err)
	}
}

type commitThenErrorStore struct {
	state.StateStore
	once sync.Once
}

type expectationCaptureStore struct {
	state.StateStore
	expectations []state.SlotExpectation
}

func (s *expectationCaptureStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.expectations = append([]state.SlotExpectation(nil), expectations...)
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestDurableStore_UsesExactlyFourExpectedSlots(t *testing.T) {
	base := newDurableState(t)
	id := durableID("session-a")
	activate(t, base, id)
	captured := &expectationCaptureStore{StateStore: base}
	store, err := sessionoverlay.NewDurableStore(captured, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", ""); err != nil {
		t.Fatal(err)
	}
	if len(captured.expectations) != 4 {
		t.Fatalf("SaveIf expectation count = %d, want 4", len(captured.expectations))
	}
	want := map[string]bool{}
	q, kind, _ := agentcfg.LifecycleSlot("tenant", "agent-a")
	want[q.TenantID+"/"+q.UserID+"/"+q.SessionID+"/"+kind] = true
	q, kind, _ = sessionfence.PendingSlot(id)
	want[q.TenantID+"/"+q.UserID+"/"+q.SessionID+"/"+kind] = true
	q, kind, _ = sessionfence.TombstoneSlot(id)
	want[q.TenantID+"/"+q.UserID+"/"+q.SessionID+"/"+kind] = true
	for _, expectation := range captured.expectations {
		delete(want, expectation.Identity.TenantID+"/"+expectation.Identity.UserID+"/"+expectation.Identity.SessionID+"/"+expectation.Kind)
	}
	if len(want) != 0 {
		t.Fatalf("missing fence slots: %v", want)
	}
}

func (s *commitThenErrorStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	fired := false
	s.once.Do(func() { fired = true })
	if fired {
		return errors.New("injected response loss after commit")
	}
	return nil
}

func TestDurableStore_UncertainWriteExactRereadConverges(t *testing.T) {
	base := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(&commitThenErrorStore{StateStore: base}, nil)
	if err != nil {
		t.Fatalf("NewDurableStore: %v", err)
	}
	id := durableID("session-a")
	activate(t, base, id)
	got, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", "")
	if err != nil || got.CanonicalName != "one" {
		t.Fatalf("SavePersonal uncertain write = (%+v, %v)", got, err)
	}
}

type commitThenFenceChangeStore struct {
	state.StateStore
	identity identity.Quadruple
	agentID  string
}

func (s *commitThenFenceChangeStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	q, kind, err := agentcfg.LifecycleSlot(s.identity.TenantID, s.agentID)
	if err != nil {
		return err
	}
	if err := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"changed"}`)}); err != nil {
		return err
	}
	return errors.New("injected response loss after fence change")
}

func TestDurableStore_UncertainWriteDoesNotAcceptChangedFence(t *testing.T) {
	base := newDurableState(t)
	id := durableID("session-a")
	activate(t, base, id)
	store, err := sessionoverlay.NewDurableStore(&commitThenFenceChangeStore{StateStore: base, identity: id, agentID: "agent-a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", ""); err == nil {
		t.Fatal("SavePersonal accepted committed target after lifecycle fence changed")
	}
}

func TestDurableStore_FenceChangeRefusesWrite(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatalf("NewDurableStore: %v", err)
	}
	id := durableID("session-a")
	activate(t, st, id)
	q, kind, err := sessionfence.PendingSlot(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"pending":true}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", "")
	if !errors.Is(err, sessionoverlay.ErrSessionErased) {
		t.Fatalf("SavePersonal after pending erasure = %v, want ErrSessionErased", err)
	}
}

func TestDurableStore_ConcurrentIsolation(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	activate(t, st, durableID("seed"))
	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := durableID(fmt.Sprintf("session-%03d", i))
			name := fmt.Sprintf("skill-%03d", i)
			if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill(name), "", ""); err != nil {
				errs <- err
				return
			}
			got, found, err := store.LoadPersonal(context.Background(), id, "agent-a", name)
			if err != nil || !found || got.Skill.Name != name {
				errs <- fmt.Errorf("session %d read = (%+v, %v, %w)", i, got, found, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type saveIfBarrierStore struct {
	state.StateStore
	arrived chan<- struct{}
	release <-chan struct{}
}

func (s saveIfBarrierStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.arrived <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestDurableStore_TwoIndependentStoresOneCASWinner(t *testing.T) {
	base := newDurableState(t)
	id := durableID("session-a")
	activate(t, base, id)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	st := saveIfBarrierStore{StateStore: base, arrived: arrived, release: release}
	left, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, store := range []*sessionoverlay.DurableStore{left, right} {
		go func(store *sessionoverlay.DurableStore) {
			<-start
			_, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("same"), "", "")
			errs <- err
		}(store)
	}
	close(start)
	<-arrived
	<-arrived
	close(release)
	winners := 0
	for range 2 {
		err := <-errs
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			t.Fatalf("SavePersonal error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners = %d, want 1", winners)
	}
}

func TestDurableStore_MissingLifecycleAndNonSessionScopeRefuse(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := durableID("session-a")
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", ""); !errors.Is(err, sessionoverlay.ErrAgentLifecycleInactive) {
		t.Fatalf("missing lifecycle SavePersonal = %v", err)
	}
	activate(t, st, id)
	notSession := durableSkill("user-scope")
	notSession.Scope = skills.ScopeUser
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", notSession, "", ""); !errors.Is(err, sessionoverlay.ErrInvalidInput) {
		t.Fatalf("ScopeUser SavePersonal = %v", err)
	}
}

type recordingMigrator struct {
	copied   []string
	verified []string
}

type everyCommitThenErrorStore struct{ state.StateStore }

func (s everyCommitThenErrorStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	return errors.New("injected response loss after cutover checkpoint commit")
}

func TestCutoverController_CommitThenErrorConvergesInitProgressAndFinal(t *testing.T) {
	base := newDurableState(t)
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	controller, err := sessionoverlay.NewCutoverController(everyCommitThenErrorStore{StateStore: base}, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: durableID("session-a"), Kind: sessionoverlay.LegacyOverlayKind("agent-a"), Bytes: []byte(`{"agent_id":"agent-a"}`)}); err != nil {
		t.Fatal(err)
	}
	mode, err := controller.Advance(context.Background(), "tenant", 8, &recordingMigrator{})
	if err != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("Advance with post-commit errors = (%q, %v)", mode, err)
	}
}

func (m *recordingMigrator) CopyLegacyOverlay(_ context.Context, rec state.StateRecord, _ config.SessionPersonalCutoverTenant) (int, error) {
	m.copied = append(m.copied, rec.Kind)
	return 1, nil
}
func (m *recordingMigrator) VerifyLegacyOverlay(_ context.Context, rec state.StateRecord, _ config.SessionPersonalCutoverTenant) (bool, error) {
	m.verified = append(m.verified, rec.Kind)
	return true, nil
}

func TestCutoverController_ResumesLiteralTenantScanAndFreshVerifies(t *testing.T) {
	st := newDurableState(t)
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch/a", RosterDigest: "digest", LegacyWritersDrained: true}
	controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"a", "ab", "%", "_", `\\`} {
		id := durableID("session-" + agent)
		body := []byte(fmt.Sprintf(`{"agent_id":%q}`, agent))
		if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: sessionoverlay.LegacyOverlayKind(agent), Bytes: body}); err != nil {
			t.Fatal(err)
		}
	}
	// A kind/payload mismatch must never copy the a record as ab (or vice versa).
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: durableID("mismatch"), Kind: sessionoverlay.LegacyOverlayKind("a"), Bytes: []byte(`{"agent_id":"ab"}`)}); err != nil {
		t.Fatal(err)
	}
	migrator := &recordingMigrator{}
	mode, err := controller.Advance(context.Background(), "tenant", 1, migrator)
	if err != nil || mode != sessionoverlay.CutoverDualRead {
		t.Fatalf("first Advance = (%q, %v), copied=%v", mode, err, migrator.copied)
	}
	// A newly constructed controller proves the continuation is durable rather
	// than held in process memory.
	controller, err = sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	for mode != sessionoverlay.CutoverStateOnly {
		mode, err = controller.Advance(context.Background(), "tenant", 1, migrator)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(migrator.copied) != 5 || len(migrator.verified) != 5 {
		t.Fatalf("copied=%v verified=%v", migrator.copied, migrator.verified)
	}
}

func TestCutoverController_UnlistedAndUndrainedRemainDualRead(t *testing.T) {
	st := newDurableState(t)
	controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{{TenantID: "listed", Epoch: "e", RosterDigest: "d", LegacyWritersDrained: false}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"listed", "unlisted"} {
		mode, err := controller.Mode(context.Background(), tenant)
		if err != nil || mode != sessionoverlay.CutoverDualRead {
			t.Fatalf("Mode(%q) = (%q, %v)", tenant, mode, err)
		}
	}
}

func TestCutoverController_ExactScopeAndMismatchedRecordFailSafe(t *testing.T) {
	st := newDurableState(t)
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	q, err := sessionoverlay.CutoverScope("tenant")
	if err != nil || q.TenantID != "tenant" || q.UserID != "__agentcfg__" || q.SessionID != "__session_personal_cutover__" || q.RunID != "" {
		t.Fatalf("CutoverScope = (%+v, %v)", q, err)
	}
	kind, err := sessionoverlay.CutoverKind("epoch")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"mode":"state_only","epoch":"epoch","roster_digest":"different","generation":1}`)}); err != nil {
		t.Fatal(err)
	}
	mode, err := controller.Mode(context.Background(), "tenant")
	if mode != sessionoverlay.CutoverDualRead || !errors.Is(err, sessionoverlay.ErrCutoverRecordInvalid) {
		t.Fatalf("Mode mismatched record = (%q, %v)", mode, err)
	}
}
