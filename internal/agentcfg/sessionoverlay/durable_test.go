package sessionoverlay_test

import (
	"context"
	"encoding/json"
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

type entryTokenContextKey struct{}

func withEntryToken(ctx context.Context, token int) context.Context {
	return context.WithValue(ctx, entryTokenContextKey{}, token)
}

// contextEntryBarrierStore stops each tagged invocation at its first actual
// StateStore read. Tests can therefore prove all calls entered the shared
// artifact before canceling selected runs and releasing the survivors.
type contextEntryBarrierStore struct {
	state.StateStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	seen    sync.Map
	current atomic.Int32
	maximum atomic.Int32
}

func newContextEntryBarrierStore(st state.StateStore, calls int) *contextEntryBarrierStore {
	return &contextEntryBarrierStore{StateStore: st, entered: make(chan struct{}, calls), release: make(chan struct{})}
}

func (s *contextEntryBarrierStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	token, tagged := ctx.Value(entryTokenContextKey{}).(int)
	if tagged {
		if _, loaded := s.seen.LoadOrStore(token, struct{}{}); !loaded {
			current := s.current.Add(1)
			for {
				maximum := s.maximum.Load()
				if current <= maximum || s.maximum.CompareAndSwap(maximum, current) {
					break
				}
			}
			s.entered <- struct{}{}
			select {
			case <-s.release:
			case <-ctx.Done():
				s.current.Add(-1)
				return state.StateRecord{}, ctx.Err()
			}
			s.current.Add(-1)
		}
	}
	return s.StateStore.Load(ctx, q, kind)
}

func (s *contextEntryBarrierStore) waitForAll(t *testing.T) {
	t.Helper()
	for range cap(s.entered) {
		select {
		case <-s.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for every concurrent invocation to enter StateStore")
		}
	}
}

func (s *contextEntryBarrierStore) releaseAll() { s.once.Do(func() { close(s.release) }) }

func (s *contextEntryBarrierStore) maximumOverlap() int { return int(s.maximum.Load()) }

func activate(t *testing.T, st state.StateStore, id identity.Quadruple) {
	t.Helper()
	q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(`{"schema":1,"revision_id":"active-revision","updated_at":"2026-08-02T00:00:00Z"}`)}); err != nil {
		t.Fatalf("seed active lifecycle: %v", err)
	}
}

func saveLifecycle(t *testing.T, st state.StateStore, id identity.Quadruple, bytes []byte) {
	t.Helper()
	q, kind, err := agentcfg.LifecycleSlot(id.TenantID, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: bytes}); err != nil {
		t.Fatalf("save lifecycle: %v", err)
	}
}

func savePersonalRecordBytes(t *testing.T, st state.StateStore, id identity.Quadruple, name string, bytes []byte) {
	t.Helper()
	kind, err := sessionoverlay.PersonalSkillKind("agent-a", name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kind, Bytes: bytes}); err != nil {
		t.Fatalf("save personal record: %v", err)
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
	skill := durableSkill("Alpha")
	first, err := store.SavePersonal(context.Background(), id, "agent-a", skill, "epoch-1", skills.CanonicalContentHash(skill))
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

func TestDurableStore_LifecycleEnvelopePreservesMissingTerminalAndCorruptStates(t *testing.T) {
	validTimestamp := `"updated_at":"2026-08-02T00:00:00Z"`
	for _, tc := range []struct {
		name    string
		present bool
		bytes   []byte
		want    error
	}{
		{name: "missing", want: sessionoverlay.ErrAgentLifecycleInactive},
		{name: "terminal empty pointer", present: true, bytes: []byte(`{"schema":1,"revision_id":"",` + validTimestamp + `}`), want: agentcfg.ErrAgentRetired},
		{name: "empty object", present: true, bytes: []byte(`{}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "sparse schema zero", present: true, bytes: []byte(`{"schema":0}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "missing schema", present: true, bytes: []byte(`{"revision_id":"active",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "missing revision", present: true, bytes: []byte(`{"schema":1,` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "missing timestamp", present: true, bytes: []byte(`{"schema":1,"revision_id":"active"}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "null schema", present: true, bytes: []byte(`{"schema":null,"revision_id":"active",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "null revision", present: true, bytes: []byte(`{"schema":1,"revision_id":null,` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "null timestamp", present: true, bytes: []byte(`{"schema":1,"revision_id":"active","updated_at":null}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "duplicate schema", present: true, bytes: []byte(`{"schema":1,"schema":0,"revision_id":"active",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "duplicate revision", present: true, bytes: []byte(`{"schema":1,"revision_id":"active","revision_id":"",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "duplicate timestamp", present: true, bytes: []byte(`{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z","updated_at":"2026-08-03T00:00:00Z"}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "unknown retired marker", present: true, bytes: []byte(`{"schema":1,"revision_id":"active","retired":true,` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "unknown field", present: true, bytes: []byte(`{"schema":1,"revision_id":"active","authority":true,` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "trailing document", present: true, bytes: []byte(`{"schema":1,"revision_id":"active",` + validTimestamp + `}{}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "future schema", present: true, bytes: []byte(`{"schema":2,"revision_id":"active",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "whitespace revision", present: true, bytes: []byte(`{"schema":1,"revision_id":" active ",` + validTimestamp + `}`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "malformed", present: true, bytes: []byte(`{"schema":1`), want: sessionoverlay.ErrAgentLifecycleCorrupt},
		{name: "oversized", present: true, bytes: []byte(strings.Repeat("x", sessionoverlay.MaxAgentLifecycleFenceBytes+1)), want: sessionoverlay.ErrAgentLifecycleCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			id := durableID("session-a")
			if tc.present {
				saveLifecycle(t, st, id, tc.bytes)
			}
			store, err := sessionoverlay.NewDurableStore(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			operations := []struct {
				name string
				run  func() error
			}{
				{name: "save", run: func() error {
					_, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", "")
					return err
				}},
				{name: "load", run: func() error { _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", "one"); return err }},
				{name: "delete", run: func() error { _, err := store.DeletePersonal(context.Background(), id, "agent-a", "one"); return err }},
			}
			for _, operation := range operations {
				if err := operation.run(); !errors.Is(err, tc.want) {
					t.Errorf("%s = %v, want %v", operation.name, err, tc.want)
				}
			}
		})
	}

	for _, bytes := range [][]byte{
		[]byte(`{"schema":0,"revision_id":"legacy-active","updated_at":"2026-08-02T00:00:00Z"}`),
		[]byte(`{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z"}`),
	} {
		st := newDurableState(t)
		id := durableID("session-active")
		saveLifecycle(t, st, id, bytes)
		store, err := sessionoverlay.NewDurableStore(st, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", ""); err != nil {
			t.Fatalf("known active compatibility shape refused: %v", err)
		}
	}
}

func TestDurableStore_ReservedAgentConfigUserRejectedAtEveryPersonalBoundary(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: agentcfg.ReservedAgentConfigUser, SessionID: "session-a"}}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error {
			_, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("one"), "", "")
			return err
		}},
		{name: "load", run: func() error { _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", "one"); return err }},
		{name: "delete", run: func() error { _, err := store.DeletePersonal(context.Background(), id, "agent-a", "one"); return err }},
	}
	for _, operation := range operations {
		if err := operation.run(); !errors.Is(err, agentcfg.ErrReservedUser) {
			t.Errorf("%s = %v, want ErrReservedUser", operation.name, err)
		}
	}
}

func TestDurableStore_PersonalRecordStrictLiveAndTombstoneInvariants(t *testing.T) {
	id := durableID("session-a")
	liveSkill := durableSkill("strict")
	valid := sessionoverlay.PersonalSkillRecord{
		Schema:        1,
		AgentID:       "agent-a",
		CanonicalName: "strict",
		ContentHash:   skills.CanonicalContentHash(liveSkill),
		Skill:         liveSkill,
		UpdatedAt:     time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	}
	encode := func(t *testing.T, record sessionoverlay.PersonalSkillRecord) []byte {
		t.Helper()
		bytes, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	withUnknown := func(t *testing.T) []byte {
		var object map[string]any
		if err := json.Unmarshal(encode(t, valid), &object); err != nil {
			t.Fatal(err)
		}
		object["authority"] = true
		bytes, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	for _, tc := range []struct {
		name  string
		bytes func(*testing.T) []byte
	}{
		{name: "unknown field", bytes: withUnknown},
		{name: "trailing document", bytes: func(t *testing.T) []byte { return append(encode(t, valid), []byte(`{}`)...) }},
		{name: "missing tombstone body", bytes: func(*testing.T) []byte {
			return []byte(`{"schema":1,"agent_id":"agent-a","canonical_name":"strict","content_hash":"","deleted":true,"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "null tombstone body", bytes: func(*testing.T) []byte {
			return []byte(`{"schema":1,"agent_id":"agent-a","canonical_name":"strict","content_hash":"","deleted":true,"skill":null,"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "live missing timestamp", bytes: func(t *testing.T) []byte {
			record := valid
			record.UpdatedAt = time.Time{}
			return encode(t, record)
		}},
		{name: "live non-session scope", bytes: func(t *testing.T) []byte {
			record := valid
			record.Skill.Scope = skills.ScopeUser
			record.ContentHash = skills.CanonicalContentHash(record.Skill)
			return encode(t, record)
		}},
		{name: "live explicit empty copy markers", bytes: func(t *testing.T) []byte {
			var object map[string]any
			if err := json.Unmarshal(encode(t, valid), &object); err != nil {
				t.Fatal(err)
			}
			object["copy_epoch"] = ""
			object["legacy_content_hash"] = ""
			bytes, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			return bytes
		}},
		{name: "tombstone with live body", bytes: func(t *testing.T) []byte {
			record := valid
			record.Deleted = true
			return encode(t, record)
		}},
		{name: "tombstone with copy markers", bytes: func(t *testing.T) []byte {
			record := sessionoverlay.PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "strict", Deleted: true, CopyEpoch: "epoch", LegacyContentHash: skills.CanonicalContentHash(liveSkill), UpdatedAt: valid.UpdatedAt}
			return encode(t, record)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			activate(t, st, id)
			savePersonalRecordBytes(t, st, id, "strict", tc.bytes(t))
			store, err := sessionoverlay.NewDurableStore(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", "strict"); !errors.Is(err, sessionoverlay.ErrPersonalRecordInvalid) {
				t.Fatalf("LoadPersonal = %v, want ErrPersonalRecordInvalid", err)
			}
		})
	}
}

func TestDurableStore_RejectsInvalidCopyMarkersAndRecordSize(t *testing.T) {
	st := newDurableState(t)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := durableID("session-a")
	activate(t, st, id)
	skill := durableSkill("bounded")
	validHash := skills.CanonicalContentHash(skill)
	for _, tc := range []struct {
		name  string
		epoch string
		hash  string
	}{
		{name: "epoch only", epoch: "epoch"},
		{name: "hash only", hash: validHash},
		{name: "whitespace epoch", epoch: " epoch", hash: validHash},
		{name: "oversized epoch", epoch: strings.Repeat("e", sessionoverlay.MaxSessionPersonalCopyEpochBytes+1), hash: validHash},
		{name: "short hash", epoch: "epoch", hash: "abcd"},
		{name: "uppercase hash", epoch: "epoch", hash: strings.ToUpper(validHash)},
		{name: "nonhex hash", epoch: "epoch", hash: strings.Repeat("z", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.SavePersonal(context.Background(), id, "agent-a", skill, tc.epoch, tc.hash); !errors.Is(err, sessionoverlay.ErrInvalidInput) {
				t.Fatalf("SavePersonal = %v, want ErrInvalidInput", err)
			}
		})
	}

	oversized := durableSkill("oversized")
	oversized.Description = strings.Repeat("x", sessionoverlay.MaxSessionPersonalRecordBytes)
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", oversized, "", ""); !errors.Is(err, sessionoverlay.ErrInvalidInput) {
		t.Fatalf("oversized SavePersonal = %v, want ErrInvalidInput", err)
	}

	canonicalName := "stored-oversized"
	kind, err := sessionoverlay.PersonalSkillKind("agent-a", canonicalName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kind, Bytes: []byte(strings.Repeat("x", sessionoverlay.MaxSessionPersonalRecordBytes+1))}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", canonicalName); !errors.Is(err, sessionoverlay.ErrPersonalRecordInvalid) {
		t.Fatalf("oversized LoadPersonal = %v, want ErrPersonalRecordInvalid", err)
	}

	badSkill := durableSkill("stored-bad-marker")
	badRecord := sessionoverlay.PersonalSkillRecord{
		Schema:            1,
		AgentID:           "agent-a",
		CanonicalName:     "stored-bad-marker",
		ContentHash:       skills.CanonicalContentHash(badSkill),
		Skill:             badSkill,
		CopyEpoch:         "epoch",
		LegacyContentHash: strings.ToUpper(skills.CanonicalContentHash(badSkill)),
	}
	badBytes, err := json.Marshal(badRecord)
	if err != nil {
		t.Fatal(err)
	}
	badKind, err := sessionoverlay.PersonalSkillKind("agent-a", badRecord.CanonicalName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: badKind, Bytes: badBytes}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", badRecord.CanonicalName); !errors.Is(err, sessionoverlay.ErrPersonalRecordInvalid) {
		t.Fatalf("invalid marker LoadPersonal = %v, want ErrPersonalRecordInvalid", err)
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

type fenceChangeDuringPointReadStore struct {
	state.StateStore
	id             identity.Quadruple
	personalKind   string
	maximumChanges int32
	pointReads     atomic.Int32
}

func (s *fenceChangeDuringPointReadStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if err != nil || q != s.id || kind != s.personalKind {
		return record, err
	}
	read := s.pointReads.Add(1)
	if s.maximumChanges >= 0 && read > s.maximumChanges {
		return record, nil
	}
	lifecycleQ, lifecycleKind, slotErr := agentcfg.LifecycleSlot(s.id.TenantID, "agent-a")
	if slotErr != nil {
		return state.StateRecord{}, slotErr
	}
	bytes := []byte(fmt.Sprintf(`{"schema":1,"revision_id":"revision-%d","updated_at":"2026-08-02T00:00:00Z"}`, read))
	if saveErr := s.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: lifecycleQ, Kind: lifecycleKind, Bytes: bytes}); saveErr != nil {
		return state.StateRecord{}, saveErr
	}
	return record, nil
}

func seedReadablePersonal(t *testing.T) (state.StateStore, identity.Quadruple, string) {
	t.Helper()
	st := newDurableState(t)
	id := durableID("session-readable")
	activate(t, st, id)
	store, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePersonal(context.Background(), id, "agent-a", durableSkill("readable"), "", ""); err != nil {
		t.Fatal(err)
	}
	kind, err := sessionoverlay.PersonalSkillKind("agent-a", "readable")
	if err != nil {
		t.Fatal(err)
	}
	return st, id, kind
}

func TestDurableStore_LoadPersonalRetriesFenceChangeDuringPointRead(t *testing.T) {
	base, id, kind := seedReadablePersonal(t)
	changing := &fenceChangeDuringPointReadStore{StateStore: base, id: id, personalKind: kind, maximumChanges: 1}
	store, err := sessionoverlay.NewDurableStore(changing, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := store.LoadPersonal(context.Background(), id, "agent-a", "readable")
	if err != nil || !found || record.CanonicalName != "readable" {
		t.Fatalf("LoadPersonal = (%+v, %v, %v)", record, found, err)
	}
	if got := changing.pointReads.Load(); got != 2 {
		t.Fatalf("point reads = %d, want one changed attempt plus one stable attempt", got)
	}
}

func TestDurableStore_LoadPersonalPerpetualFenceChurnStopsAfterThreeAttempts(t *testing.T) {
	base, id, kind := seedReadablePersonal(t)
	changing := &fenceChangeDuringPointReadStore{StateStore: base, id: id, personalKind: kind, maximumChanges: -1}
	store, err := sessionoverlay.NewDurableStore(changing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadPersonal(context.Background(), id, "agent-a", "readable"); !errors.Is(err, sessionoverlay.ErrSessionSkillReadUnstable) {
		t.Fatalf("LoadPersonal = %v, want ErrSessionSkillReadUnstable", err)
	}
	if got := changing.pointReads.Load(); got != sessionoverlay.MaxSessionSkillReadAttempts {
		t.Fatalf("point reads = %d, want exactly %d attempts", got, sessionoverlay.MaxSessionSkillReadAttempts)
	}
}

type cancelDuringPointReadStore struct {
	state.StateStore
	id           identity.Quadruple
	personalKind string
	cancel       context.CancelFunc
	pointReads   atomic.Int32
}

func (s *cancelDuringPointReadStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if err == nil && q == s.id && kind == s.personalKind {
		s.pointReads.Add(1)
		s.cancel()
	}
	return record, err
}

func TestDurableStore_LoadPersonalHonorsCancellationAndDeadlineOnAttempts(t *testing.T) {
	base, id, kind := seedReadablePersonal(t)
	ctx, cancel := context.WithCancel(context.Background())
	canceling := &cancelDuringPointReadStore{StateStore: base, id: id, personalKind: kind, cancel: cancel}
	store, err := sessionoverlay.NewDurableStore(canceling, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadPersonal(ctx, id, "agent-a", "readable"); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-attempt cancellation = %v, want context.Canceled", err)
	}
	if got := canceling.pointReads.Load(); got != 1 {
		t.Fatalf("point reads after cancellation = %d, want 1", got)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, _, err := store.LoadPersonal(expired, id, "agent-a", "readable"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline = %v, want context.DeadlineExceeded", err)
	}
	if got := canceling.pointReads.Load(); got != 1 {
		t.Fatalf("expired deadline began another point read: %d", got)
	}
}

func TestDurableStore_ConcurrentIsolation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	base := newDurableState(t)
	activate(t, base, durableID("seed"))
	const n = 128
	barrier := newContextEntryBarrierStore(base, n)
	store, err := sessionoverlay.NewDurableStore(barrier, nil)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, n)
	var wg sync.WaitGroup
	contexts := make([]context.Context, n)
	cancels := make([]context.CancelFunc, n)
	for i := range n {
		contexts[i], cancels[i] = context.WithCancel(withEntryToken(context.Background(), i))
	}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := durableID(fmt.Sprintf("session-%03d", i))
			name := fmt.Sprintf("skill-%03d", i)
			ctx := contexts[i]
			if _, err := store.SavePersonal(ctx, id, "agent-a", durableSkill(name), "", ""); err != nil {
				if i%2 == 0 && errors.Is(err, context.Canceled) {
					return
				}
				errs <- fmt.Errorf("session %d save: %w", i, err)
				return
			}
			if i%2 == 0 {
				errs <- fmt.Errorf("session %d canceled write completed", i)
				return
			}
			got, found, err := store.LoadPersonal(ctx, id, "agent-a", name)
			if err != nil || !found || got.Skill.Name != name {
				errs <- fmt.Errorf("session %d read = (%+v, %v, %w)", i, got, found, err)
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
		t.Errorf("actual durable-store overlap = %d, want %d", got, n)
	}
	for i := range n {
		id := durableID(fmt.Sprintf("session-%03d", i))
		name := fmt.Sprintf("skill-%03d", i)
		record, found, err := store.LoadPersonal(context.Background(), id, "agent-a", name)
		if err != nil {
			t.Fatalf("verify session %d: %v", i, err)
		}
		if i%2 == 0 {
			if found {
				t.Errorf("canceled session %d persisted %+v", i, record)
			}
			continue
		}
		if !found || record.CanonicalName != name || record.Skill.Name != name {
			t.Errorf("session %d identity bleed: found=%v record=%+v", i, found, record)
		}
	}
	assertGoroutinesRestored(t, baseline)
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
	legacy, err := sessionoverlay.NewStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.SetUserPrompt(context.Background(), durableID("session-a"), "agent-a", "legacy"); err != nil {
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
	legacy, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"a", "ab", "%", "_", `\\`} {
		id := durableID("session-" + agent)
		if _, err := legacy.SetUserPrompt(context.Background(), id, agent, "legacy "+agent); err != nil {
			t.Fatal(err)
		}
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

func TestCutoverController_MalformedLegacyRowsBlockStateOnly(t *testing.T) {
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	valid := []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-01T00:00:00Z"}`)
	for _, tc := range []struct {
		name  string
		id    identity.Quadruple
		bytes []byte
	}{
		{name: "malformed json", id: durableID("bad-json"), bytes: []byte(`{"schema":`)},
		{name: "future schema", id: durableID("future"), bytes: []byte(`{"schema":2,"overlay":{},"updated_at":"2026-08-01T00:00:00Z"}`)},
		{name: "missing envelope field", id: durableID("missing"), bytes: []byte(`{"schema":1,"updated_at":"2026-08-01T00:00:00Z"}`)},
		{name: "run scoped", id: identity.Quadruple{Identity: durableID("run").Identity, RunID: "run-a"}, bytes: []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-01T00:00:00Z"}`)},
		{name: "unknown envelope field", id: durableID("unknown-envelope"), bytes: []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-01T00:00:00Z","authority":true}`)},
		{name: "unknown overlay field", id: durableID("unknown-overlay"), bytes: []byte(`{"schema":1,"overlay":{"authority":true},"updated_at":"2026-08-01T00:00:00Z"}`)},
		{name: "trailing document", id: durableID("trailing"), bytes: append(append([]byte(nil), valid...), []byte(`{}`)...)},
		{name: "oversized", id: durableID("oversized"), bytes: []byte(strings.Repeat("x", sessionoverlay.MaxLegacySessionOverlayRecordBytes+1))},
		{name: "reserved control user", id: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "__agentcfg__", SessionID: "ordinary"}}, bytes: valid},
		{name: "exact reserved control identity", id: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "__agentcfg__", SessionID: "__session_personal_cutover__"}}, bytes: valid},
		{name: "noncanonical overlay set", id: durableID("duplicate"), bytes: []byte(`{"schema":1,"overlay":{"personal_skills":["same","same"]},"updated_at":"2026-08-01T00:00:00Z"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: tc.id, Kind: sessionoverlay.LegacyOverlayKind("agent-a"), Bytes: tc.bytes}); err != nil {
				t.Fatal(err)
			}
			migrator := &recordingMigrator{}
			mode, err := controller.Advance(context.Background(), "tenant", 8, migrator)
			if mode != sessionoverlay.CutoverDualRead || !errors.Is(err, sessionoverlay.ErrLegacyOverlayInvalid) {
				t.Fatalf("Advance = (%q, %v), want dual_read ErrLegacyOverlayInvalid", mode, err)
			}
			if len(migrator.copied) != 0 || len(migrator.verified) != 0 {
				t.Fatalf("invalid candidate reached migrator: copied=%v verified=%v", migrator.copied, migrator.verified)
			}
			mode, err = controller.Mode(context.Background(), "tenant")
			if err != nil || mode != sessionoverlay.CutoverDualRead {
				t.Fatalf("Mode after rejected row = (%q, %v), want dual_read", mode, err)
			}
		})
	}
}

func TestCutoverController_OrdinaryUserMayUseCutoverSessionSentinel(t *testing.T) {
	st := newDurableState(t)
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "ordinary-user", SessionID: "__session_personal_cutover__"}}
	if err := st.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: id,
		Kind:     sessionoverlay.LegacyOverlayKind("agent-a"),
		Bytes:    []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-01T00:00:00Z"}`),
	}); err != nil {
		t.Fatal(err)
	}
	migrator := &recordingMigrator{}
	mode, err := controller.Advance(context.Background(), "tenant", 8, migrator)
	if err != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("Advance ordinary sentinel session = (%q, %v)", mode, err)
	}
	if len(migrator.copied) != 1 || len(migrator.verified) != 1 {
		t.Fatalf("ordinary sentinel session was not migrated: copied=%v verified=%v", migrator.copied, migrator.verified)
	}
}

type conditionFailedWinnerStore struct {
	state.StateStore
	winnerBytes []byte
	once        sync.Once
}

func marshalCutoverRecord(t *testing.T, record sessionoverlay.CutoverRecord) []byte {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal cutover record: %v", err)
	}
	return encoded
}

func cutoverCursor(t *testing.T, tenantID, prefix string) string {
	t.Helper()
	cursor, err := state.EncodeStateScanContinuation(
		state.StateScanCursor{UserID: "user", SessionID: "session", Kind: prefix + "agent-a"},
		tenantID,
		prefix,
		state.ListScope{MaintenanceScoped: true},
	)
	if err != nil {
		t.Fatalf("encode cutover cursor: %v", err)
	}
	return cursor
}

func (s *conditionFailedWinnerStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	injected := false
	var seedErr error
	s.once.Do(func() {
		injected = true
		winner := next
		winner.ID = state.NewEventID()
		winner.Bytes = append([]byte(nil), s.winnerBytes...)
		if len(winner.Bytes) == 0 {
			winner.Bytes = append([]byte(nil), next.Bytes...)
		}
		seedErr = s.Save(ctx, winner)
	})
	if injected {
		if seedErr != nil {
			return seedErr
		}
		return state.ErrConditionFailed
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func TestCutoverController_EnsureValidatesConditionFailedWinner(t *testing.T) {
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	badCursorWinner := marshalCutoverRecord(t, sessionoverlay.CutoverRecord{Schema: 1, Mode: sessionoverlay.CutoverDualRead, Epoch: "epoch", RosterDigest: "digest", Continuation: "not-a-cursor", Generation: 1})
	for _, tc := range []struct {
		name        string
		winnerBytes []byte
		wantErr     bool
	}{
		{name: "exact winner"},
		{name: "malformed winner", winnerBytes: []byte(`{"schema":`), wantErr: true},
		{name: "mismatched winner", winnerBytes: []byte(`{"schema":1,"mode":"dual_read","epoch":"different","roster_digest":"digest","copied":0,"generation":0}`), wantErr: true},
		{name: "unknown field winner", winnerBytes: []byte(`{"schema":1,"mode":"state_only","epoch":"epoch","roster_digest":"digest","copied":0,"generation":2,"authority":true}`), wantErr: true},
		{name: "trailing document winner", winnerBytes: []byte(`{"schema":1,"mode":"state_only","epoch":"epoch","roster_digest":"digest","copied":0,"generation":2}{}`), wantErr: true},
		{name: "bad cursor winner", winnerBytes: badCursorWinner, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newDurableState(t)
			st := &conditionFailedWinnerStore{StateStore: base, winnerBytes: tc.winnerBytes}
			controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
			if err != nil {
				t.Fatal(err)
			}
			err = controller.Ensure(context.Background())
			if tc.wantErr && !errors.Is(err, sessionoverlay.ErrCutoverRecordInvalid) {
				t.Fatalf("Ensure = %v, want ErrCutoverRecordInvalid", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Ensure exact winner: %v", err)
			}
		})
	}
}

func TestCutoverController_StrictCheckpointValidationNeverAuthorizes(t *testing.T) {
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	valid := sessionoverlay.CutoverRecord{Schema: 1, Mode: sessionoverlay.CutoverDualRead, Epoch: "epoch", RosterDigest: "digest"}
	validStateOnly := valid
	validStateOnly.Mode = sessionoverlay.CutoverStateOnly
	validStateOnly.Generation = 2
	oversizedStateOnly := append(marshalCutoverRecord(t, validStateOnly), []byte(strings.Repeat(" ", sessionoverlay.MaxSessionPersonalCutoverRecordBytes))...)
	validCursor := cutoverCursor(t, declaration.TenantID, sessionoverlay.LegacyOverlayPrefix())
	mismatchedCursor := cutoverCursor(t, "other-tenant", sessionoverlay.LegacyOverlayPrefix())
	with := func(mutate func(*sessionoverlay.CutoverRecord)) []byte {
		record := valid
		mutate(&record)
		return marshalCutoverRecord(t, record)
	}
	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{name: "unknown field state only", bytes: []byte(`{"schema":1,"mode":"state_only","epoch":"epoch","roster_digest":"digest","copied":0,"generation":2,"authority":true}`)},
		{name: "trailing document state only", bytes: append(marshalCutoverRecord(t, validStateOnly), []byte(`{}`)...)},
		{name: "oversized state only", bytes: oversizedStateOnly},
		{name: "invalid cursor", bytes: with(func(record *sessionoverlay.CutoverRecord) {
			record.Continuation = "not-a-cursor"
			record.Generation = 1
		})},
		{name: "mismatched cursor", bytes: with(func(record *sessionoverlay.CutoverRecord) {
			record.Continuation = mismatchedCursor
			record.Generation = 1
		})},
		{name: "state only continuation", bytes: with(func(record *sessionoverlay.CutoverRecord) {
			record.Mode = sessionoverlay.CutoverStateOnly
			record.Continuation = validCursor
			record.Generation = 2
		})},
		{name: "negative copied", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.Copied = -1 })},
		{name: "negative generation", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.Generation = -1 })},
		{name: "overflow copied", bytes: []byte(fmt.Sprintf(`{"schema":1,"mode":"dual_read","epoch":"epoch","roster_digest":"digest","copied":%d,"generation":1}`, int64(sessionoverlay.MaxSessionPersonalCutoverCounter)+1))},
		{name: "overflow generation", bytes: []byte(fmt.Sprintf(`{"schema":1,"mode":"dual_read","epoch":"epoch","roster_digest":"digest","copied":0,"generation":%d}`, int64(sessionoverlay.MaxSessionPersonalCutoverCounter)+1))},
		{name: "initial checkpoint with progress", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.Copied = 1 })},
		{name: "state only before terminal generation", bytes: with(func(record *sessionoverlay.CutoverRecord) {
			record.Mode = sessionoverlay.CutoverStateOnly
			record.Generation = 1
		})},
		{name: "invalid mode", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.Mode = "authority" })},
		{name: "future schema", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.Schema = 2 })},
		{name: "declaration mismatch", bytes: with(func(record *sessionoverlay.CutoverRecord) { record.RosterDigest = "other" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDurableState(t)
			controller, err := sessionoverlay.NewCutoverController(st, []config.SessionPersonalCutoverTenant{declaration})
			if err != nil {
				t.Fatal(err)
			}
			q, err := sessionoverlay.CutoverScope(declaration.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			kind, err := sessionoverlay.CutoverKind(declaration.Epoch)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: tc.bytes}); err != nil {
				t.Fatal(err)
			}
			mode, err := controller.Mode(context.Background(), declaration.TenantID)
			if mode != sessionoverlay.CutoverDualRead || !errors.Is(err, sessionoverlay.ErrCutoverRecordInvalid) {
				t.Fatalf("Mode = (%q, %v), want dual_read ErrCutoverRecordInvalid", mode, err)
			}
			if err := controller.Ensure(context.Background()); !errors.Is(err, sessionoverlay.ErrCutoverRecordInvalid) {
				t.Fatalf("Ensure = %v, want ErrCutoverRecordInvalid", err)
			}
		})
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

func TestCutoverController_TenantIDsAreExactOpaqueKeys(t *testing.T) {
	st := newDurableState(t)
	declarations := []config.SessionPersonalCutoverTenant{
		{TenantID: "TenantA", Epoch: "upper", RosterDigest: "upper-digest", LegacyWritersDrained: true},
		{TenantID: "tenanta", Epoch: "lower", RosterDigest: "lower-digest", LegacyWritersDrained: false},
	}
	controller, err := sessionoverlay.NewCutoverController(st, declarations)
	if err != nil {
		t.Fatalf("case-distinct declarations: %v", err)
	}
	mode, err := controller.Advance(context.Background(), "TenantA", 8, &recordingMigrator{})
	if err != nil || mode != sessionoverlay.CutoverStateOnly {
		t.Fatalf("Advance(TenantA) = (%q, %v)", mode, err)
	}
	mode, err = controller.Mode(context.Background(), "tenanta")
	if err != nil || mode != sessionoverlay.CutoverDualRead {
		t.Fatalf("Mode(tenanta) aliased TenantA = (%q, %v)", mode, err)
	}
	if _, err := sessionoverlay.NewCutoverController(st, append(declarations, declarations[0])); !errors.Is(err, sessionoverlay.ErrInvalidInput) {
		t.Fatalf("exact duplicate declaration = %v, want ErrInvalidInput", err)
	}
}

func TestCutoverController_ConcurrentReuseIdentityCancellationAndLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	base := newDurableState(t)
	const n = 128
	declarations := make([]config.SessionPersonalCutoverTenant, n)
	for i := range n {
		declarations[i] = config.SessionPersonalCutoverTenant{
			TenantID:             fmt.Sprintf("tenant-%03d", i),
			Epoch:                fmt.Sprintf("epoch-%03d", i),
			RosterDigest:         fmt.Sprintf("digest-%03d", i),
			LegacyWritersDrained: true,
		}
	}
	seed, err := sessionoverlay.NewCutoverController(base, declarations)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Ensure(context.Background()); err != nil {
		t.Fatalf("seed cutover records: %v", err)
	}
	barrier := newContextEntryBarrierStore(base, n)
	controller, err := sessionoverlay.NewCutoverController(barrier, declarations)
	if err != nil {
		t.Fatal(err)
	}
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
			tenantID := declarations[i].TenantID
			mode, err := controller.Advance(contexts[i], tenantID, 8, &recordingMigrator{})
			if err != nil {
				if i%2 == 0 && errors.Is(err, context.Canceled) {
					return
				}
				errs <- fmt.Errorf("Advance(%q): %w", tenantID, err)
				return
			}
			if i%2 == 0 {
				errs <- fmt.Errorf("canceled Advance(%q) completed", tenantID)
			} else if mode != sessionoverlay.CutoverStateOnly {
				errs <- fmt.Errorf("Advance(%q) = %q, want state_only", tenantID, mode)
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
		t.Errorf("actual cutover-controller overlap = %d, want %d", got, n)
	}
	for i, declaration := range declarations {
		mode, err := controller.Mode(context.Background(), declaration.TenantID)
		if err != nil {
			t.Fatalf("verify Mode(%q): %v", declaration.TenantID, err)
		}
		want := sessionoverlay.CutoverStateOnly
		if i%2 == 0 {
			want = sessionoverlay.CutoverDualRead
		}
		if mode != want {
			t.Errorf("Mode(%q) = %q, want %q", declaration.TenantID, mode, want)
		}
	}
	assertGoroutinesRestored(t, baseline)
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
