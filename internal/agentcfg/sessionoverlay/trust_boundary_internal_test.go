package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

func trustBoundarySkill(name string) skills.Skill {
	skill := skills.Skill{
		Name: name, Trigger: "when requested", Steps: []string{"act"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skill
}

func trustBoundaryJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestLifecycleEnvelope_AdversarialClassification(t *testing.T) {
	now := "2026-08-02T00:00:00Z"
	tests := []struct {
		name string
		data []byte
		want lifecycleEnvelopeState
	}{
		{name: "empty", want: lifecycleEnvelopeInvalid},
		{name: "oversized", data: []byte(strings.Repeat("x", MaxAgentLifecycleFenceBytes+1)), want: lifecycleEnvelopeInvalid},
		{name: "not object", data: []byte(`[]`), want: lifecycleEnvelopeInvalid},
		{name: "duplicate", data: []byte(`{"schema":1,"schema":1,"revision_id":"r","updated_at":"` + now + `"}`), want: lifecycleEnvelopeInvalid},
		{name: "missing required", data: []byte(`{"schema":1}`), want: lifecycleEnvelopeInvalid},
		{name: "unknown field", data: []byte(`{"schema":1,"revision_id":"r","updated_at":"` + now + `","active":true}`), want: lifecycleEnvelopeInvalid},
		{name: "future schema", data: []byte(`{"schema":2,"revision_id":"r","updated_at":"` + now + `"}`), want: lifecycleEnvelopeInvalid},
		{name: "zero time", data: []byte(`{"schema":1,"revision_id":"r","updated_at":"0001-01-01T00:00:00Z"}`), want: lifecycleEnvelopeInvalid},
		{name: "terminal", data: []byte(`{"schema":1,"revision_id":"","updated_at":"` + now + `"}`), want: lifecycleEnvelopeTerminal},
		{name: "whitespace revision", data: []byte(`{"schema":1,"revision_id":" r ","updated_at":"` + now + `"}`), want: lifecycleEnvelopeInvalid},
		{name: "active schema zero", data: []byte(`{"schema":0,"revision_id":"r","updated_at":"` + now + `"}`), want: lifecycleEnvelopeActive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeLifecycleEnvelope(tc.data); got != tc.want {
				t.Fatalf("classification=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestStrictJSONObjectBoundary_RejectsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "array", data: []byte(`[]`)},
		{name: "duplicate", data: []byte(`{"x":1,"x":2}`)},
		{name: "truncated name", data: []byte(`{"x`)},
		{name: "truncated value", data: []byte(`{"x":`)},
		{name: "missing close", data: []byte(`{"x":1`)},
		{name: "trailing document", data: []byte(`{"x":1}{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rejectDuplicateJSONObjectFields(tc.data); err == nil {
				t.Fatal("ambiguous JSON accepted")
			}
		})
	}
	if err := rejectDuplicateJSONObjectFields([]byte(`{"outer":{"x":1},"array":[1,2]}`)); err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
}

func TestPersonalRecordDecoder_ExactKeyHashAndTombstoneBinding(t *testing.T) {
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	skill := trustBoundarySkill("alpha")
	valid := PersonalSkillRecord{
		Schema: 1, AgentID: "agent-a", CanonicalName: "alpha",
		ContentHash: skill.ContentHash, Skill: skill, UpdatedAt: now,
	}
	if got, found, err := decodePersonal(trustBoundaryJSON(t, valid), "agent-a", "alpha"); err != nil || !found || got.ContentHash != skill.ContentHash {
		t.Fatalf("valid record=(%+v,%v,%v)", got, found, err)
	}

	copyRecord := valid
	copyRecord.CopyEpoch = "epoch-1"
	copyRecord.LegacyContentHash = skill.ContentHash
	if _, found, err := decodePersonal(trustBoundaryJSON(t, copyRecord), "agent-a", "alpha"); err != nil || !found {
		t.Fatalf("valid copy record=(%v,%v)", found, err)
	}
	tombstone := PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "alpha", Deleted: true, UpdatedAt: now}
	if got, found, err := decodePersonal(trustBoundaryJSON(t, tombstone), "agent-a", "alpha"); err != nil || !found || !got.Deleted {
		t.Fatalf("valid tombstone=(%+v,%v,%v)", got, found, err)
	}

	base := map[string]any{
		"schema": 1, "agent_id": "agent-a", "canonical_name": "alpha",
		"content_hash": skill.ContentHash, "skill": skill, "updated_at": now,
	}
	cases := []struct {
		name             string
		data             []byte
		agent, canonical string
	}{
		{name: "empty", agent: "agent-a", canonical: "alpha"},
		{name: "oversized", data: []byte(strings.Repeat("x", MaxSessionPersonalRecordBytes+1)), agent: "agent-a", canonical: "alpha"},
		{name: "unknown field", data: []byte(`{"schema":1,"agent_id":"agent-a","canonical_name":"alpha","content_hash":"x","skill":{},"updated_at":"` + now.Format(time.RFC3339) + `","authority":true}`), agent: "agent-a", canonical: "alpha"},
		{name: "missing required", data: []byte(`{"schema":1}`), agent: "agent-a", canonical: "alpha"},
		{name: "malformed skill", data: []byte(`{"schema":1,"agent_id":"agent-a","canonical_name":"alpha","content_hash":"x","skill":{"unknown":true},"updated_at":"` + now.Format(time.RFC3339) + `"}`), agent: "agent-a", canonical: "alpha"},
		{name: "wrong agent", data: trustBoundaryJSON(t, valid), agent: "agent-b", canonical: "alpha"},
		{name: "wrong canonical name", data: trustBoundaryJSON(t, valid), agent: "agent-a", canonical: "beta"},
	}
	clone := func() map[string]any {
		out := make(map[string]any, len(base))
		for key, value := range base {
			out[key] = value
		}
		return out
	}
	oneMarker := clone()
	oneMarker["copy_epoch"] = "epoch"
	cases = append(cases, struct {
		name             string
		data             []byte
		agent, canonical string
	}{"one copy marker", trustBoundaryJSON(t, oneMarker), "agent-a", "alpha"})
	emptyMarkers := clone()
	emptyMarkers["copy_epoch"], emptyMarkers["legacy_content_hash"] = "", ""
	cases = append(cases, struct {
		name             string
		data             []byte
		agent, canonical string
	}{"empty copy markers", trustBoundaryJSON(t, emptyMarkers), "agent-a", "alpha"})
	badEpoch := clone()
	badEpoch["copy_epoch"], badEpoch["legacy_content_hash"] = " bad", skill.ContentHash
	cases = append(cases, struct {
		name             string
		data             []byte
		agent, canonical string
	}{"bad copy epoch", trustBoundaryJSON(t, badEpoch), "agent-a", "alpha"})
	badHash := clone()
	badHash["content_hash"] = strings.Repeat("A", 64)
	cases = append(cases, struct {
		name             string
		data             []byte
		agent, canonical string
	}{"noncanonical hash", trustBoundaryJSON(t, badHash), "agent-a", "alpha"})
	badTombstone := map[string]any{"schema": 1, "agent_id": "agent-a", "canonical_name": "alpha", "content_hash": skill.ContentHash, "deleted": false, "skill": skill, "updated_at": now}
	cases = append(cases, struct {
		name             string
		data             []byte
		agent, canonical string
	}{"false tombstone with live body", trustBoundaryJSON(t, badTombstone), "agent-a", "alpha"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, found, err := decodePersonal(tc.data, tc.agent, tc.canonical); err == nil || found || !errors.Is(err, ErrPersonalRecordInvalid) {
				t.Fatalf("decode=(found=%v,err=%v), want ErrPersonalRecordInvalid", found, err)
			}
		})
	}
}

func TestLegacyOverlayDecoder_CanonicalSetBoundary(t *testing.T) {
	now := "2026-08-02T00:00:00Z"
	valid := []byte(`{"schema":1,"overlay":{"disabled_servers":["a","b"],"disabled_tools":["x"],"personal_skills":["one"]},"updated_at":"` + now + `"}`)
	if got, err := decodeOverlayRecord(valid); err != nil || len(got.DisabledServers) != 2 {
		t.Fatalf("valid overlay=(%+v,%v)", got, err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "oversized", data: []byte(strings.Repeat("x", MaxLegacySessionOverlayRecordBytes+1))},
		{name: "duplicate envelope", data: []byte(`{"schema":1,"schema":1,"overlay":{},"updated_at":"` + now + `"}`)},
		{name: "unknown envelope field", data: []byte(`{"schema":1,"overlay":{},"updated_at":"` + now + `","authority":true}`)},
		{name: "missing fields", data: []byte(`{"schema":1}`)},
		{name: "duplicate overlay field", data: []byte(`{"schema":1,"overlay":{"user_prompt":"a","user_prompt":"b"},"updated_at":"` + now + `"}`)},
		{name: "unknown overlay field", data: []byte(`{"schema":1,"overlay":{"authority":true},"updated_at":"` + now + `"}`)},
		{name: "unsorted set", data: []byte(`{"schema":1,"overlay":{"disabled_servers":["b","a"]},"updated_at":"` + now + `"}`)},
		{name: "empty member", data: []byte(`{"schema":1,"overlay":{"disabled_servers":[""]},"updated_at":"` + now + `"}`)},
		{name: "duplicate member", data: []byte(`{"schema":1,"overlay":{"personal_skills":["a","a"]},"updated_at":"` + now + `"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeOverlayRecord(tc.data); !errors.Is(err, ErrLegacyOverlayInvalid) {
				t.Fatalf("error=%v, want ErrLegacyOverlayInvalid", err)
			}
		})
	}
}

func TestResolverTrustBoundary_AliasesFiltersAndMaliciousSearchResults(t *testing.T) {
	if _, err := canonicalMembership([]string{"ok", "  "}); err == nil {
		t.Fatal("empty canonical membership accepted")
	}
	if err := validateCopyMarkers("epoch", ""); err == nil {
		t.Fatal("one copy marker accepted")
	}
	if err := validateCopyMarkers("\n", strings.Repeat("a", 64)); err == nil {
		t.Fatal("noncanonical epoch accepted")
	}
	if err := validateCopyMarkers("epoch", "BAD"); err == nil {
		t.Fatal("noncanonical hash accepted")
	}
	if _, err := PersonalSkillKind("", "name"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PersonalSkillKind=%v", err)
	}
	if _, err := PersonalSkillPrefix(" "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PersonalSkillPrefix=%v", err)
	}
	if _, err := CutoverScope(" "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CutoverScope=%v", err)
	}
	if _, err := CutoverKind(""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CutoverKind=%v", err)
	}

	skill := trustBoundarySkill("alpha")
	skill.Tags = []string{"secure"}
	skill.TaskType = "audit"
	resolver := &SessionSkillResolver{
		run:      identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"},
		searcher: &scriptedSnapshotSearcher{}, all: map[string]skills.Skill{"alpha": skill},
		byScope: map[skills.Scope]map[string]skills.Skill{skills.ScopeSession: {"alpha": skill}},
		session: map[string]skills.Skill{"alpha": skill},
	}
	ctx := context.Background()
	if got, err := resolver.List(ctx, resolver.run, skills.ListFilter{Scope: skills.ScopeSession, TaskType: "audit", Tags: []string{"secure"}, Limit: 1}); err != nil || len(got) != 1 {
		t.Fatalf("filtered list=(%+v,%v)", got, err)
	}
	if got, err := resolver.List(ctx, resolver.run, skills.ListFilter{TaskType: "other", Tags: []string{"missing"}, Offset: 10}); err != nil || len(got) != 0 {
		t.Fatalf("filtered empty list=(%+v,%v)", got, err)
	}
	for _, filter := range []skills.ListFilter{{Limit: -1}, {Offset: -1}, {Limit: 1001}} {
		if _, err := resolver.List(ctx, resolver.run, filter); !errors.Is(err, ErrInvalidSessionSkillResolver) {
			t.Fatalf("bad filter=%+v err=%v", filter, err)
		}
	}
	if _, err := resolver.Search(ctx, resolver.run, "x", -1); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("negative search=%v", err)
	}
	if _, err := resolver.GetScope(ctx, resolver.run, "missing", skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("missing scope=%v", err)
	}
	var nilResolver *SessionSkillResolver
	if _, err := nilResolver.SessionSkills(ctx, resolver.run); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("nil resolver=%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := resolver.SessionSkills(cancelled, resolver.run); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolver=%v", err)
	}

	attacks := [][]skills.RankedSkill{
		{{Skill: skill, Score: math.NaN(), Path: skills.PathExact}},
		{{Skill: skill, Score: 1, Path: "portable_contains"}},
		{{Skill: trustBoundarySkill("injected"), Score: 1, Path: skills.PathExact}},
		{{Skill: skill, Score: 1, Path: skills.PathExact}, {Skill: skill, Score: 1, Path: skills.PathExact}},
		{{Skill: skill, Score: 1, Path: skills.PathExact}, {Skill: skill, Score: 1, Path: skills.PathExact}},
	}
	for i, result := range attacks {
		resolver.searcher = &scriptedSnapshotSearcher{result: result}
		limit := 20
		if i == len(attacks)-1 {
			limit = 1
		}
		if _, err := resolver.Search(ctx, resolver.run, "x", limit); !errors.Is(err, ErrInvalidSessionSkillResolver) {
			t.Fatalf("attack %d accepted: %v", i, err)
		}
	}
	for _, path := range []string{skills.PathFTS5, skills.PathFullText, skills.PathRegex, skills.PathExact, skills.PathSemantic} {
		resolver.searcher = &scriptedSnapshotSearcher{result: []skills.RankedSkill{{Skill: skill, Score: 1, Path: path}}}
		if got, err := resolver.Search(ctx, resolver.run, "x", 0); err != nil || len(got) != 1 {
			t.Fatalf("path %q=(%+v,%v)", path, got, err)
		}
	}
}

type scriptedSnapshotSearcher struct {
	result []skills.RankedSkill
	err    error
}

func (s *scriptedSnapshotSearcher) SearchSnapshot(context.Context, identity.Quadruple, string, []skills.Skill, int) ([]skills.RankedSkill, error) {
	return append([]skills.RankedSkill(nil), s.result...), s.err
}

func TestLegacyCandidateDecoder_ExactDeclarationKindAndIdentity(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	valid := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: LegacyOverlayKind("agent-a"), Bytes: []byte(`{"schema":1,"overlay":{"personal_skills":[]},"updated_at":"2026-08-02T00:00:00Z"}`), UpdatedAt: time.Now().UTC()}
	if got, err := decodeLegacyCandidate(valid, declaration); err != nil || got.agentID != "agent-a" {
		t.Fatalf("valid candidate=(%+v,%v)", got, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*state.StateRecord, *config.SessionPersonalCutoverTenant)
	}{
		{name: "writers active", mutate: func(_ *state.StateRecord, d *config.SessionPersonalCutoverTenant) { d.LegacyWritersDrained = false }},
		{name: "tenant mismatch", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) { r.Identity.TenantID = "other" }},
		{name: "run scoped", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) { r.Identity.RunID = "run" }},
		{name: "wrong kind", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) { r.Kind = LegacyOverlayPrefix() }},
		{name: "duplicate envelope", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) {
			r.Bytes = []byte(`{"schema":1,"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "unknown envelope", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) {
			r.Bytes = []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z","authority":true}`)
		}},
		{name: "duplicate overlay", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) {
			r.Bytes = []byte(`{"schema":1,"overlay":{"user_prompt":"a","user_prompt":"b"},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "unknown overlay", mutate: func(r *state.StateRecord, _ *config.SessionPersonalCutoverTenant) {
			r.Bytes = []byte(`{"schema":1,"overlay":{"authority":true},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, declared := valid, declaration
			tc.mutate(&candidate, &declared)
			if _, err := decodeLegacyCandidate(candidate, declared); !errors.Is(err, ErrLegacyOverlayInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type embeddedSkillStore struct{ skills.SkillStore }

type resolverBoundarySkillStore struct {
	skills.SkillStore
	list     func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error)
	getScope func(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error)
}

func (s resolverBoundarySkillStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if s.list == nil {
		return nil, nil
	}
	return s.list(ctx, id, filter)
}

func (s resolverBoundarySkillStore) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if s.getScope == nil {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return s.getScope(ctx, id, name, scope)
}

func (s resolverBoundarySkillStore) GetScopeAgent(ctx context.Context, id identity.Quadruple, _ string, name string, scope skills.Scope) (skills.Skill, error) {
	return s.GetScope(ctx, id, name, scope)
}

type resolverBoundaryStateStore struct {
	state.StateStore
	load        func(context.Context, identity.Quadruple, string) (state.StateRecord, error)
	listBounded func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error)
}

func (s resolverBoundaryStateStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.load == nil {
		return state.StateRecord{}, state.ErrNotFound
	}
	return s.load(ctx, id, kind)
}

func (s resolverBoundaryStateStore) ListKindForIdentityBounded(ctx context.Context, id identity.Quadruple, prefix string, limit int) ([]state.StateRecord, error) {
	if s.listBounded == nil {
		return nil, nil
	}
	return s.listBounded(ctx, id, prefix, limit)
}

type fixedCutoverReader struct {
	mode CutoverMode
	err  error
}

func (r fixedCutoverReader) Mode(context.Context, string) (CutoverMode, error) { return r.mode, r.err }

type loadErrorStateStore struct {
	state.StateStore
	err error
}

type boundaryLoadResult struct {
	record state.StateRecord
	err    error
}

type scriptedBoundaryStateStore struct {
	state.StateStore
	mu        sync.Mutex
	loads     []boundaryLoadResult
	saveIfErr error
}

func (s *scriptedBoundaryStateStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.loads) == 0 {
		return state.StateRecord{}, state.ErrNotFound
	}
	result := s.loads[0]
	s.loads = s.loads[1:]
	return result.record, result.err
}

func (s *scriptedBoundaryStateStore) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return s.saveIfErr
}

func (s loadErrorStateStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, s.err
}

func TestDurableAndResolverConfiguration_FailClosedAtEveryRequiredAuthority(t *testing.T) {
	if _, err := NewDurableStore(nil, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil DurableStore state=%v", err)
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "r"}
	personal := &DurableStore{state: loadErrorStateStore{err: state.ErrNotFound}, clock: time.Now}
	valid := SessionSkillResolverConfig{Run: id, AgentID: "agent-a", Base: embeddedSkillStore{}, Personal: personal, Cutover: fixedCutoverReader{mode: CutoverDualRead}}
	for _, tc := range []struct {
		name   string
		mutate func(*SessionSkillResolverConfig)
	}{
		{name: "missing identity", mutate: func(c *SessionSkillResolverConfig) { c.Run.UserID = "" }},
		{name: "missing run", mutate: func(c *SessionSkillResolverConfig) { c.Run.RunID = " " }},
		{name: "missing agent", mutate: func(c *SessionSkillResolverConfig) { c.AgentID = " " }},
		{name: "missing base", mutate: func(c *SessionSkillResolverConfig) { c.Base = nil }},
		{name: "missing personal", mutate: func(c *SessionSkillResolverConfig) { c.Personal = nil }},
		{name: "missing cutover", mutate: func(c *SessionSkillResolverConfig) { c.Cutover = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if err := validateResolverConfig(cfg); !errors.Is(err, ErrInvalidSessionSkillResolver) {
				t.Fatalf("validation=%v", err)
			}
		})
	}

	if _, _, err := personal.LoadPersonal(context.Background(), id, "", "name"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty LoadPersonal agent=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := personal.LoadPersonal(cancelled, id, "agent-a", "name"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled LoadPersonal=%v", err)
	}
	if _, err := personal.DeletePersonal(context.Background(), id, "agent-a", " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty DeletePersonal name=%v", err)
	}
	badSkill := trustBoundarySkill("bad")
	badSkill.Trigger = ""
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", badSkill, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid SavePersonal body=%v", err)
	}
	badScope := trustBoundarySkill("bad-scope")
	badScope.Scope = skills.ScopeUser
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", badScope, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid SavePersonal scope=%v", err)
	}
	cyclic := trustBoundarySkill("cyclic")
	cycle := map[string]any{}
	cycle["self"] = cycle
	cyclic.Extra = cycle
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", cyclic, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cyclic SavePersonal Extra=%v", err)
	}
	personal.state = loadErrorStateStore{err: errors.New("state offline")}
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", trustBoundarySkill("state-error"), "", ""); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("SavePersonal state error=%v", err)
	}
	if _, err := personal.DeletePersonal(context.Background(), id, "agent-a", "state-error"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("DeletePersonal state error=%v", err)
	}
	if _, _, err := personal.LoadPersonal(context.Background(), id, "agent-a", "state-error"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("LoadPersonal state error=%v", err)
	}
}

func TestFenceAndCutoverControls_CancellationUnknownStatesAndDeclarations(t *testing.T) {
	if err := (fences{state: lifecycleEnvelopeState(99)}).lifecycleError(); !errors.Is(err, ErrAgentLifecycleCorrupt) {
		t.Fatalf("unknown lifecycle=%v", err)
	}
	for _, tc := range []struct {
		state        lifecycleEnvelopeState
		wantTerminal bool
		wantErr      error
	}{
		{state: lifecycleEnvelopeActive},
		{state: lifecycleEnvelopeTerminal, wantTerminal: true},
		{state: lifecycleEnvelopeMissing, wantErr: ErrAgentLifecycleInactive},
		{state: lifecycleEnvelopeInvalid, wantErr: ErrAgentLifecycleCorrupt},
		{state: lifecycleEnvelopeState(99), wantErr: ErrAgentLifecycleCorrupt},
	} {
		terminal, err := terminalLegacyFences(fences{state: tc.state})
		if terminal != tc.wantTerminal || !errors.Is(err, tc.wantErr) {
			t.Fatalf("terminalLegacyFences(%d)=(%v,%v)", tc.state, terminal, err)
		}
	}
	if terminal, err := terminalLegacyFences(fences{state: lifecycleEnvelopeActive, pending: state.SlotExpectation{ExpectedEventID: "pending"}}); err != nil || !terminal {
		t.Fatalf("erased fence=(%v,%v)", terminal, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadFences(cancelled, loadErrorStateStore{}, identity.Quadruple{}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled loadFences=%v", err)
	}
	if _, _, err := slotExpectationWithBytes(cancelled, loadErrorStateStore{}, identity.Quadruple{}, "kind"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled slot expectation=%v", err)
	}
	if stable, err := (fences{lifecycle: state.SlotExpectation{Identity: identity.Quadruple{}, Kind: "kind"}}).stable(context.Background(), loadErrorStateStore{err: errors.New("offline")}); err == nil || stable {
		t.Fatalf("stable load error=(%v,%v)", stable, err)
	}

	if _, err := NewCutoverController(nil, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil controller state=%v", err)
	}
	if _, err := NewCutoverController(loadErrorStateStore{}, make([]config.SessionPersonalCutoverTenant, 257)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("declaration cap=%v", err)
	}
	for _, declaration := range []config.SessionPersonalCutoverTenant{
		{TenantID: "", Epoch: "e", RosterDigest: "d"},
		{TenantID: "t", Epoch: "\n", RosterDigest: "d"},
		{TenantID: "t", Epoch: "e", RosterDigest: "\x00"},
	} {
		if _, err := NewCutoverController(loadErrorStateStore{}, []config.SessionPersonalCutoverTenant{declaration}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid declaration=%+v err=%v", declaration, err)
		}
	}
	duplicate := config.SessionPersonalCutoverTenant{TenantID: "t", Epoch: "e", RosterDigest: "d"}
	if _, err := NewCutoverController(loadErrorStateStore{}, []config.SessionPersonalCutoverTenant{duplicate, duplicate}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate declaration=%v", err)
	}
	controller, err := NewCutoverController(loadErrorStateStore{err: state.ErrNotFound}, []config.SessionPersonalCutoverTenant{{TenantID: "t", Epoch: "e", RosterDigest: "d", LegacyWritersDrained: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Mode(cancelled, "t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Mode=%v", err)
	}
	if err := controller.Ensure(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Ensure=%v", err)
	}
	if _, err := controller.Advance(cancelled, "t", 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Advance=%v", err)
	}
	if _, err := controller.Advance(context.Background(), "t", 1, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil migrator=%v", err)
	}
}

type boundarySkillReader struct {
	byName map[string]skills.Skill
	err    error
}

func (r boundarySkillReader) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	return skills.Skill{}, skills.ErrSkillNotFound
}
func (r boundarySkillReader) GetScope(_ context.Context, _ identity.Quadruple, name string, _ skills.Scope) (skills.Skill, error) {
	if r.err != nil {
		return skills.Skill{}, r.err
	}
	skill, ok := r.byName[name]
	if !ok {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return skill, nil
}
func (r boundarySkillReader) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return nil, nil
}
func (r boundarySkillReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, nil
}

func TestLegacyMigrationBoundary_EmptyCancellationReaderAndAliasFailures(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	candidate := func(names string) state.StateRecord {
		return state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: LegacyOverlayKind("agent-a"), Bytes: []byte(`{"schema":1,"overlay":{"personal_skills":` + names + `},"updated_at":"2026-08-02T00:00:00Z"}`), UpdatedAt: time.Now().UTC()}
	}
	personal := &DurableStore{state: loadErrorStateStore{err: state.ErrNotFound}, clock: time.Now}
	migrator := &ExactLegacyMigrator{reader: boundarySkillReader{}, personal: personal}
	if copied, err := migrator.CopyLegacyOverlay(context.Background(), candidate(`[]`), declaration); err != nil || copied != 0 {
		t.Fatalf("empty copy=(%d,%v)", copied, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := migrator.CopyLegacyOverlay(cancelled, candidate(`["alpha"]`), declaration); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy=%v", err)
	}
	if _, err := migrator.VerifyLegacyOverlay(cancelled, candidate(`["alpha"]`), declaration); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verify=%v", err)
	}
	if _, err := migrator.CopyLegacyOverlay(context.Background(), candidate(`[" "]`), declaration); !errors.Is(err, ErrLegacyOverlayInvalid) {
		t.Fatalf("empty canonical name=%v", err)
	}

	missing := &ExactLegacyMigrator{reader: boundarySkillReader{}, personal: personal}
	if _, err := missing.loadLegacyBodies(context.Background(), id, []string{"missing"}); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("missing body=%v", err)
	}
	readErr := errors.New("reader offline")
	if _, err := (&ExactLegacyMigrator{reader: boundarySkillReader{err: readErr}}).loadLegacyBodies(context.Background(), id, []string{"alpha"}); !errors.Is(err, readErr) {
		t.Fatalf("reader error=%v", err)
	}
	wrongScope := trustBoundarySkill("alpha")
	wrongScope.Scope = skills.ScopeUser
	if _, err := (&ExactLegacyMigrator{reader: boundarySkillReader{byName: map[string]skills.Skill{"alpha": wrongScope}}}).loadLegacyBodies(context.Background(), id, []string{"alpha"}); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("wrong scope=%v", err)
	}
	badHash := trustBoundarySkill("alpha")
	badHash.ContentHash = strings.Repeat("0", 64)
	if _, err := (&ExactLegacyMigrator{reader: boundarySkillReader{byName: map[string]skills.Skill{"alpha": badHash}}}).loadLegacyBodies(context.Background(), id, []string{"alpha"}); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("bad hash=%v", err)
	}
}

func TestResolverRecordDecoders_ExactKindPayloadAndCanonicalName(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	legacy := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: LegacyOverlayKind("agent-a"), Bytes: []byte(`{"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`), UpdatedAt: time.Now().UTC()}
	if _, err := decodeResolverLegacyOverlay(legacy, "tenant", "agent-a"); err != nil {
		t.Fatalf("valid resolver legacy=%v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*state.StateRecord)
	}{
		{name: "wrong selected agent", mutate: func(r *state.StateRecord) { r.Kind = LegacyOverlayKind("agent-b") }},
		{name: "duplicate envelope", mutate: func(r *state.StateRecord) {
			r.Bytes = []byte(`{"schema":1,"schema":1,"overlay":{},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "malformed envelope", mutate: func(r *state.StateRecord) { r.Bytes = []byte(`{`) }},
		{name: "duplicate overlay", mutate: func(r *state.StateRecord) {
			r.Bytes = []byte(`{"schema":1,"overlay":{"user_prompt":"a","user_prompt":"b"},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
		{name: "unknown overlay", mutate: func(r *state.StateRecord) {
			r.Bytes = []byte(`{"schema":1,"overlay":{"authority":true},"updated_at":"2026-08-02T00:00:00Z"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := legacy
			tc.mutate(&record)
			if _, err := decodeResolverLegacyOverlay(record, "tenant", "agent-a"); !errors.Is(err, ErrLegacyOverlayInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	skill := trustBoundarySkill("alpha")
	personal := PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "alpha", ContentHash: skill.ContentHash, Skill: skill, UpdatedAt: time.Now().UTC()}
	kind, err := PersonalSkillKind("agent-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	record := state.StateRecord{Kind: kind, Bytes: trustBoundaryJSON(t, personal)}
	if _, err := decodeResolverPersonal(record, "agent-a"); err != nil {
		t.Fatalf("valid resolver personal=%v", err)
	}
	for _, tc := range []struct {
		name   string
		record state.StateRecord
	}{
		{name: "invalid prefix", record: state.StateRecord{Kind: "other", Bytes: record.Bytes}},
		{name: "duplicate", record: state.StateRecord{Kind: kind, Bytes: []byte(`{"canonical_name":"alpha","canonical_name":"alpha"}`)}},
		{name: "malformed", record: state.StateRecord{Kind: kind, Bytes: []byte(`{`)}},
		{name: "canonical absent", record: state.StateRecord{Kind: kind, Bytes: []byte(`{}`)}},
		{name: "payload invalid", record: state.StateRecord{Kind: kind, Bytes: []byte(`{"canonical_name":"alpha"}`)}},
		{name: "wrong key", record: state.StateRecord{Kind: kind + "x", Bytes: record.Bytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeResolverPersonal(tc.record, "agent-a"); !errors.Is(err, ErrPersonalRecordInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	invalid := trustBoundarySkill("bad")
	invalid.Trigger = ""
	if err := validateResolverSkill(invalid); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("invalid resolver skill=%v", err)
	}
	if err := validateExactResolverSkill(skill, "beta", skills.ScopeSession); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("mismatched exact body=%v", err)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	skill.Extra = cycle
	if _, err := normalizeResolverSkill(skill); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("cyclic resolver Extra=%v", err)
	}
	if got := cloneExtraValue(make(chan int)); got != nil {
		t.Fatalf("unsupported clone retained: %T", got)
	}
	if validCutoverToken("x\n", 10) {
		t.Fatal("control character accepted as cutover token")
	}
}

func TestResolverConstruction_PropagatesDependencyBoundsAndExactBodyFailures(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	personal := &DurableStore{state: resolverBoundaryStateStore{}, clock: time.Now}
	baseConfig := func(store skills.SkillStore, cutover CutoverModeReader) SessionSkillResolverConfig {
		return SessionSkillResolverConfig{Run: id, AgentID: "agent-a", Base: store, Personal: personal, Cutover: cutover}
	}
	dependencyErr := errors.New("dependency offline")
	for _, tc := range []struct {
		name string
		cfg  SessionSkillResolverConfig
		want error
	}{
		{name: "base list error", cfg: baseConfig(resolverBoundarySkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
			return nil, dependencyErr
		}}, fixedCutoverReader{mode: CutoverDualRead}), want: dependencyErr},
		{name: "cutover error", cfg: baseConfig(resolverBoundarySkillStore{}, fixedCutoverReader{err: dependencyErr}), want: dependencyErr},
		{name: "unknown cutover mode", cfg: baseConfig(resolverBoundarySkillStore{}, fixedCutoverReader{mode: "authority"}), want: ErrInvalidSessionSkillResolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildResolver(context.Background(), tc.cfg, nil, nil); !errors.Is(err, tc.want) {
				t.Fatalf("buildResolver=%v want=%v", err, tc.want)
			}
		})
	}

	overflow := make([]skills.Skill, maxSessionSkillResolverBaseRows+1)
	if _, err := buildResolver(context.Background(), baseConfig(resolverBoundarySkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
		return overflow, nil
	}}, fixedCutoverReader{}), nil, nil); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("base materialization overflow=%v", err)
	}
	full := make([]skills.Skill, maxSessionSkillResolverBaseRows)
	probeErrorStore := resolverBoundarySkillStore{list: func(_ context.Context, _ identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
		if filter.Offset > 0 {
			return nil, dependencyErr
		}
		return full, nil
	}}
	if _, err := buildResolver(context.Background(), baseConfig(probeErrorStore, fixedCutoverReader{}), nil, nil); !errors.Is(err, dependencyErr) {
		t.Fatalf("base bound probe error=%v", err)
	}
	probeOverflowStore := resolverBoundarySkillStore{list: func(_ context.Context, _ identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
		if filter.Offset > 0 {
			return []skills.Skill{trustBoundarySkill("overflow")}, nil
		}
		return full, nil
	}}
	if _, err := buildResolver(context.Background(), baseConfig(probeOverflowStore, fixedCutoverReader{}), nil, nil); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("base bound probe overflow=%v", err)
	}

	invalid := trustBoundarySkill("invalid")
	invalid.Trigger = ""
	invalidStore := resolverBoundarySkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
		return []skills.Skill{invalid}, nil
	}}
	if _, err := buildResolver(context.Background(), baseConfig(invalidStore, fixedCutoverReader{}), nil, nil); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("invalid base body=%v", err)
	}
	alpha := trustBoundarySkill("Alpha")
	alpha.Scope = skills.ScopeGlobal
	alias := alpha
	alias.Name = " alpha "
	alias.Description = "different"
	alias.ContentHash = skills.CanonicalContentHash(alias)
	aliasStore := resolverBoundarySkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
		return []skills.Skill{alpha, alias}, nil
	}}
	if _, err := buildResolver(context.Background(), baseConfig(aliasStore, fixedCutoverReader{}), nil, nil); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("same-scope alias collision=%v", err)
	}

	userErrorStore := resolverBoundarySkillStore{getScope: func(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error) {
		return skills.Skill{}, dependencyErr
	}}
	if _, err := buildResolver(context.Background(), baseConfig(userErrorStore, fixedCutoverReader{}), nil, map[string]struct{}{"user": {}}); !errors.Is(err, dependencyErr) {
		t.Fatalf("user body read error=%v", err)
	}
	wrongUser := trustBoundarySkill("user")
	wrongUser.Scope = skills.ScopeSession
	wrongUserStore := resolverBoundarySkillStore{getScope: func(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error) {
		return wrongUser, nil
	}}
	if _, err := buildResolver(context.Background(), baseConfig(wrongUserStore, fixedCutoverReader{}), nil, map[string]struct{}{"user": {}}); !errors.Is(err, ErrLegacySkillInvalid) {
		t.Fatalf("wrong exact user body=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelStore := resolverBoundarySkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
		return []skills.Skill{alpha}, nil
	}}
	if _, err := buildResolver(cancelled, baseConfig(cancelStore, fixedCutoverReader{}), nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during base enumeration=%v", err)
	}
}

func TestResolverTierLoading_BoundedStorageAndAliasFailures(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	dependencyErr := errors.New("state offline")
	cfg := SessionSkillResolverConfig{Run: id, AgentID: "agent-a", Base: resolverBoundarySkillStore{}, Personal: &DurableStore{state: resolverBoundaryStateStore{load: func(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
		return state.StateRecord{}, dependencyErr
	}}}}
	if _, err := loadLegacySessionTier(context.Background(), cfg); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("legacy load error=%v", err)
	}

	ownedErrCfg := cfg
	ownedErrCfg.Personal = &DurableStore{state: resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return nil, dependencyErr
	}}}
	if _, err := loadOwnedSessionTier(context.Background(), ownedErrCfg); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("owned list error=%v", err)
	}
	overflowCfg := cfg
	overflowCfg.Personal = &DurableStore{state: resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return make([]state.StateRecord, maxSessionSkillResolverOwnedRows+1), nil
	}}}
	if _, err := loadOwnedSessionTier(context.Background(), overflowCfg); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("owned overflow=%v", err)
	}

	skill := trustBoundarySkill("alpha")
	personal := PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "alpha", ContentHash: skill.ContentHash, Skill: skill, UpdatedAt: time.Now().UTC()}
	kind, err := PersonalSkillKind("agent-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	record := state.StateRecord{Kind: kind, Bytes: trustBoundaryJSON(t, personal)}
	duplicateCfg := cfg
	duplicateCfg.Personal = &DurableStore{state: resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return []state.StateRecord{record, record}, nil
	}}}
	if _, err := loadOwnedSessionTier(context.Background(), duplicateCfg); !errors.Is(err, ErrPersonalRecordInvalid) {
		t.Fatalf("duplicate owned canonical name=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelCfg := cfg
	cancelCfg.Personal = &DurableStore{state: resolverBoundaryStateStore{listBounded: func(context.Context, identity.Quadruple, string, int) ([]state.StateRecord, error) {
		return []state.StateRecord{record}, nil
	}}}
	if _, err := loadOwnedSessionTier(cancelled, cancelCfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled owned enumeration=%v", err)
	}
}

func activeLifecycleBoundaryRecord() state.StateRecord {
	return state.StateRecord{ID: state.NewEventID(), Bytes: []byte(`{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z"}`)}
}

func terminalLifecycleBoundaryRecord() state.StateRecord {
	return state.StateRecord{ID: state.NewEventID(), Bytes: []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)}
}

func noRecordBoundaryLoad() boundaryLoadResult { return boundaryLoadResult{err: state.ErrNotFound} }

func TestDurableExactRereads_RejectMismatchesFenceChangesAndUnknownOutcomes(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	q := durableSessionQuad(id)
	skill := trustBoundarySkill("alpha")
	record := PersonalSkillRecord{Schema: 1, AgentID: "agent-a", CanonicalName: "alpha", ContentHash: skill.ContentHash, Skill: skill, UpdatedAt: time.Now().UTC()}
	kind, err := PersonalSkillKind("agent-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	expected := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: trustBoundaryJSON(t, record)}
	before := fences{state: lifecycleEnvelopeActive}
	targetLoadErr := errors.New("target offline")
	fenceLoadErr := errors.New("fence offline")

	for _, tc := range []struct {
		name    string
		loads   []boundaryLoadResult
		wantOK  bool
		wantErr error
	}{
		{name: "target load error", loads: []boundaryLoadResult{{err: targetLoadErr}}, wantErr: targetLoadErr},
		{name: "target generation mismatch", loads: []boundaryLoadResult{{record: state.StateRecord{ID: state.NewEventID()}}}},
		{name: "fence load error", loads: []boundaryLoadResult{{record: expected}, {err: fenceLoadErr}}, wantErr: fenceLoadErr},
		{name: "retired after commit", loads: []boundaryLoadResult{{record: expected}, {record: terminalLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}, wantErr: agentcfg.ErrAgentRetired},
		{name: "erased after commit", loads: []boundaryLoadResult{{record: expected}, {record: activeLifecycleBoundaryRecord()}, {record: state.StateRecord{ID: state.NewEventID()}}, noRecordBoundaryLoad()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &scriptedBoundaryStateStore{loads: append([]boundaryLoadResult(nil), tc.loads...)}
			durable := &DurableStore{state: store, clock: time.Now}
			_, ok, gotErr := durable.exactPersonal(context.Background(), id, kind, expected, "agent-a", "alpha", before)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v err=%v", ok, tc.wantOK, gotErr)
			}
			if !errors.Is(gotErr, tc.wantErr) {
				t.Fatalf("error=%v want errors.Is(%v)", gotErr, tc.wantErr)
			}
		})
	}

	operationErr := errors.New("save response lost")
	activeLoads := func(target boundaryLoadResult) []boundaryLoadResult {
		return []boundaryLoadResult{{record: activeLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad(), noRecordBoundaryLoad(), target}
	}
	for _, tc := range []struct {
		name   string
		target boundaryLoadResult
		delete bool
	}{
		{name: "save exact reread errors", target: boundaryLoadResult{err: errors.New("target offline")}},
		{name: "save uncertain mismatch", target: boundaryLoadResult{record: state.StateRecord{ID: state.NewEventID()}}},
		{name: "delete exact reread errors", target: boundaryLoadResult{err: errors.New("target offline")}, delete: true},
		{name: "delete uncertain mismatch", target: boundaryLoadResult{record: state.StateRecord{ID: state.NewEventID()}}, delete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &scriptedBoundaryStateStore{loads: activeLoads(tc.target), saveIfErr: operationErr}
			durable := &DurableStore{state: store, clock: time.Now}
			var gotErr error
			if tc.delete {
				_, gotErr = durable.DeletePersonal(context.Background(), id, "agent-a", "alpha")
			} else {
				_, gotErr = durable.SavePersonal(context.Background(), id, "agent-a", skill, "", "")
			}
			if !errors.Is(gotErr, ErrStateUnavailable) {
				t.Fatalf("error=%v want ErrStateUnavailable", gotErr)
			}
		})
	}
}

func TestOverlayAndCutoverExactRereads_RejectCASUncertainty(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	q := durableSessionQuad(id)
	overlayBytes := []byte(`{"schema":1,"overlay":{"user_prompt":"safe"},"updated_at":"2026-08-02T00:00:00Z"}`)
	expected := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: LegacyOverlayKind("agent-a"), Bytes: overlayBytes}
	activeLifecycle := activeLifecycleBoundaryRecord()
	before := fences{state: lifecycleEnvelopeActive, lifecycle: state.SlotExpectation{ExpectedEventID: activeLifecycle.ID}}
	overlayLoadErr := errors.New("overlay target offline")
	overlayFenceErr := errors.New("overlay fence offline")
	corruptExpected := expected
	corruptExpected.ID = state.NewEventID()
	corruptExpected.Bytes = []byte(`{`)
	for _, tc := range []struct {
		name       string
		expected   state.StateRecord
		loads      []boundaryLoadResult
		wantFound  bool
		wantPrompt string
		wantErr    error
	}{
		{name: "target storage error", expected: expected, loads: []boundaryLoadResult{{err: overlayLoadErr}}, wantErr: overlayLoadErr},
		{name: "target generation mismatch", expected: expected, loads: []boundaryLoadResult{{record: state.StateRecord{ID: state.NewEventID()}}}},
		{name: "fence storage error", expected: expected, loads: []boundaryLoadResult{{record: expected}, {err: overlayFenceErr}}, wantErr: overlayFenceErr},
		{name: "fence generation changed", expected: expected, loads: []boundaryLoadResult{{record: expected}, {record: activeLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}},
		{name: "erasure appeared", expected: expected, loads: []boundaryLoadResult{{record: expected}, {record: activeLifecycle}, {record: state.StateRecord{ID: state.NewEventID()}}, noRecordBoundaryLoad()}},
		{name: "corrupt committed bytes", expected: corruptExpected, loads: []boundaryLoadResult{{record: corruptExpected}, {record: activeLifecycle}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}, wantErr: ErrLegacyOverlayInvalid},
		{name: "exact committed overlay", expected: expected, loads: []boundaryLoadResult{{record: expected}, {record: activeLifecycle}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}, wantFound: true, wantPrompt: "safe"},
	} {
		t.Run("overlay "+tc.name, func(t *testing.T) {
			scripted := &scriptedBoundaryStateStore{loads: append([]boundaryLoadResult(nil), tc.loads...)}
			overlayStore := &store{state: scripted, clock: time.Now}
			got, found, gotErr := overlayStore.exactOverlay(context.Background(), id, "agent-a", tc.expected, before)
			if found != tc.wantFound || got.UserPrompt != tc.wantPrompt {
				t.Fatalf("result=(%+v,%v) want prompt=%q found=%v", got, found, tc.wantPrompt, tc.wantFound)
			}
			if !errors.Is(gotErr, tc.wantErr) {
				t.Fatalf("error=%v want errors.Is(%v)", gotErr, tc.wantErr)
			}
		})
	}
	if _, err := encodeOverlayRecord(Overlay{UserPrompt: strings.Repeat("x", MaxLegacySessionOverlayRecordBytes)}, time.Now()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized overlay=%v", err)
	}

	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	controller := &CutoverController{state: &scriptedBoundaryStateStore{}, declarations: map[string]config.SessionPersonalCutoverTenant{"tenant": declaration}}
	cutoverQ, err := CutoverScope("tenant")
	if err != nil {
		t.Fatal(err)
	}
	cutoverKind, err := CutoverKind("epoch")
	if err != nil {
		t.Fatal(err)
	}
	slot := state.SlotExpectation{Identity: cutoverQ, Kind: cutoverKind}
	valid := CutoverRecord{Schema: 1, Mode: CutoverDualRead, Epoch: "epoch", RosterDigest: "digest"}
	if err := (&CutoverController{declarations: map[string]config.SessionPersonalCutoverTenant{}}).saveRecord(context.Background(), cutoverQ, slot, valid); !errors.Is(err, ErrCutoverRecordInvalid) {
		t.Fatalf("undeclared save=%v", err)
	}
	invalid := valid
	invalid.Epoch = "other"
	if err := controller.saveRecord(context.Background(), cutoverQ, slot, invalid); !errors.Is(err, ErrCutoverRecordInvalid) {
		t.Fatalf("invalid checkpoint=%v", err)
	}
	conditionController := &CutoverController{state: &scriptedBoundaryStateStore{saveIfErr: state.ErrConditionFailed}, declarations: controller.declarations}
	if err := conditionController.saveRecord(context.Background(), cutoverQ, slot, valid); !errors.Is(err, state.ErrConditionFailed) {
		t.Fatalf("checkpoint CAS=%v", err)
	}
	for _, loads := range [][]boundaryLoadResult{{{err: errors.New("offline")}}, {{record: state.StateRecord{ID: state.NewEventID()}}}} {
		uncertain := &CutoverController{state: &scriptedBoundaryStateStore{loads: loads, saveIfErr: errors.New("response lost")}, declarations: controller.declarations}
		if err := uncertain.saveRecord(context.Background(), cutoverQ, slot, valid); !errors.Is(err, ErrStateUnavailable) {
			t.Fatalf("uncertain checkpoint=%v", err)
		}
	}
	expectedCutover := state.StateRecord{ID: state.NewEventID(), Identity: cutoverQ, Kind: cutoverKind, Bytes: trustBoundaryJSON(t, valid)}
	cutoverLoadErr := errors.New("cutover target offline")
	corruptCutover := expectedCutover
	corruptCutover.ID = state.NewEventID()
	corruptCutover.Bytes = []byte(`{`)
	for _, tc := range []struct {
		name     string
		expected state.StateRecord
		actual   boundaryLoadResult
		wantOK   bool
		wantErr  error
	}{
		{name: "storage error", expected: expectedCutover, actual: boundaryLoadResult{err: cutoverLoadErr}, wantErr: cutoverLoadErr},
		{name: "generation mismatch", expected: expectedCutover, actual: boundaryLoadResult{record: state.StateRecord{ID: state.NewEventID()}}},
		{name: "corrupt exact bytes", expected: corruptCutover, actual: boundaryLoadResult{record: corruptCutover}, wantErr: ErrCutoverRecordInvalid},
		{name: "exact checkpoint", expected: expectedCutover, actual: boundaryLoadResult{record: expectedCutover}, wantOK: true},
	} {
		t.Run("cutover "+tc.name, func(t *testing.T) {
			exact := &CutoverController{state: &scriptedBoundaryStateStore{loads: []boundaryLoadResult{tc.actual}}, declarations: controller.declarations}
			ok, gotErr := exact.exactCutover(context.Background(), tc.expected, declaration)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v err=%v", ok, tc.wantOK, gotErr)
			}
			if !errors.Is(gotErr, tc.wantErr) {
				t.Fatalf("error=%v want errors.Is(%v)", gotErr, tc.wantErr)
			}
		})
	}
}

func TestSessionSkillResolverConstruction_FenceAndMembershipAuthorityFailures(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	base := resolverBoundarySkillStore{}
	newConfig := func(loads []boundaryLoadResult) SessionSkillResolverConfig {
		return SessionSkillResolverConfig{
			Run: id, AgentID: "agent-a", Base: base,
			Personal: &DurableStore{state: &scriptedBoundaryStateStore{loads: append([]boundaryLoadResult(nil), loads...)}, clock: time.Now},
			Cutover:  fixedCutoverReader{mode: CutoverDualRead},
		}
	}
	activeFence := []boundaryLoadResult{{record: activeLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}
	lifecycleLoadErr := errors.New("lifecycle offline")
	afterFenceLoadErr := errors.New("after offline")
	for _, tc := range []struct {
		name  string
		loads []boundaryLoadResult
		want  error
	}{
		{name: "before lifecycle storage error", loads: []boundaryLoadResult{{err: lifecycleLoadErr}}, want: ErrStateUnavailable},
		{name: "before erasure", loads: []boundaryLoadResult{{record: activeLifecycleBoundaryRecord()}, {record: state.StateRecord{ID: state.NewEventID()}}, noRecordBoundaryLoad()}, want: ErrSessionErased},
		{name: "before retirement", loads: []boundaryLoadResult{{record: terminalLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()}, want: agentcfg.ErrAgentRetired},
		{name: "after lifecycle storage error", loads: append(append([]boundaryLoadResult{}, activeFence...), noRecordBoundaryLoad(), boundaryLoadResult{err: afterFenceLoadErr}), want: ErrStateUnavailable},
		{name: "after erasure", loads: append(append([]boundaryLoadResult{}, activeFence...), noRecordBoundaryLoad(), boundaryLoadResult{record: activeLifecycleBoundaryRecord()}, boundaryLoadResult{record: state.StateRecord{ID: state.NewEventID()}}, noRecordBoundaryLoad()), want: ErrSessionErased},
		{name: "after retirement", loads: append(append([]boundaryLoadResult{}, activeFence...), noRecordBoundaryLoad(), boundaryLoadResult{record: terminalLifecycleBoundaryRecord()}, noRecordBoundaryLoad(), noRecordBoundaryLoad()), want: agentcfg.ErrAgentRetired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSessionSkillResolver(context.Background(), newConfig(tc.loads))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want errors.Is(%v)", err, tc.want)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSessionSkillResolver(cancelled, newConfig(nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled construction=%v", err)
	}
	for _, membership := range []SessionSkillMembership{{AdminNames: []string{" "}}, {UserPersonalNames: []string{" "}}} {
		cfg := newConfig(nil)
		cfg.Membership = membership
		if _, err := NewSessionSkillResolver(context.Background(), cfg); !errors.Is(err, ErrInvalidSessionSkillResolver) {
			t.Fatalf("invalid membership=%+v err=%v", membership, err)
		}
	}
}

func TestResolverAndOverlay_PublicBoundsCancellationAndExactScope(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	alpha := trustBoundarySkill("alpha")
	beta := trustBoundarySkill("beta")
	resolver := &SessionSkillResolver{
		run: id, searcher: &scriptedSnapshotSearcher{},
		all:     map[string]skills.Skill{"alpha": alpha, "beta": beta},
		byScope: map[skills.Scope]map[string]skills.Skill{skills.ScopeSession: {"alpha": alpha, "beta": beta}},
		session: map[string]skills.Skill{"alpha": alpha, "beta": beta},
	}
	if got, err := resolver.GetScope(context.Background(), id, "alpha", skills.ScopeSession); err != nil || got.Name != "alpha" {
		t.Fatalf("exact scope=(%+v,%v)", got, err)
	}
	if got, err := resolver.List(context.Background(), id, skills.ListFilter{Limit: 1}); err != nil || len(got) != 1 {
		t.Fatalf("bounded list=(%+v,%v)", got, err)
	}
	if got, err := resolver.List(context.Background(), id, skills.ListFilter{Tags: []string{"missing"}}); err != nil || len(got) != 0 {
		t.Fatalf("tag miss=(%+v,%v)", got, err)
	}
	if _, err := resolver.List(context.Background(), identity.Quadruple{}, skills.ListFilter{}); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("cross-identity List=%v", err)
	}
	invalid := trustBoundarySkill("invalid")
	invalid.Trigger = ""
	if _, err := normalizeResolverSkill(invalid); !errors.Is(err, ErrInvalidSessionSkillResolver) {
		t.Fatalf("normalize invalid skill=%v", err)
	}

	overlayState := &scriptedBoundaryStateStore{}
	overlay := &store{state: overlayState, clock: time.Now}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := overlay.Get(cancelled, id, "agent-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled overlay Get=%v", err)
	}
	if _, err := overlay.AddPersonalSkill(cancelled, id, "agent-a", "alpha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled AddPersonalSkill=%v", err)
	}
	if _, err := overlay.RemovePersonalSkill(cancelled, id, "agent-a", "alpha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled RemovePersonalSkill=%v", err)
	}
	if _, err := overlay.RemovePersonalSkill(context.Background(), id, "agent-a", " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty RemovePersonalSkill=%v", err)
	}
	if _, err := overlay.SetUserPrompt(context.Background(), id, "agent-a", strings.Repeat("x", MaxLegacySessionOverlayRecordBytes)); !errors.Is(err, ErrAgentLifecycleInactive) {
		// Fence authority is evaluated before payload size, preventing an absent
		// agent from using validation timing as a write oracle.
		t.Fatalf("missing lifecycle oversized mutation=%v", err)
	}
}

func TestLegacyMigrator_FenceLoadFailuresPropagateWithoutReadingBodies(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	declaration := config.SessionPersonalCutoverTenant{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: true}
	candidate := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: LegacyOverlayKind("agent-a"), Bytes: []byte(`{"schema":1,"overlay":{"personal_skills":["alpha"]},"updated_at":"2026-08-02T00:00:00Z"}`), UpdatedAt: time.Now().UTC()}
	dependencyErr := errors.New("fence storage offline")
	for _, verify := range []bool{false, true} {
		migrator := &ExactLegacyMigrator{reader: boundarySkillReader{}, personal: &DurableStore{state: &scriptedBoundaryStateStore{loads: []boundaryLoadResult{{err: dependencyErr}}}}}
		if verify {
			if _, err := migrator.VerifyLegacyOverlay(context.Background(), candidate, declaration); !errors.Is(err, ErrStateUnavailable) {
				t.Fatalf("verify fence error=%v", err)
			}
		} else if _, err := migrator.CopyLegacyOverlay(context.Background(), candidate, declaration); !errors.Is(err, ErrStateUnavailable) {
			t.Fatalf("copy fence error=%v", err)
		}
	}
}
