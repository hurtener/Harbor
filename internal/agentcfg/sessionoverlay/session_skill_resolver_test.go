package sessionoverlay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/state"
)

type resolverModeReader struct{ mode sessionoverlay.CutoverMode }

func (r resolverModeReader) Mode(context.Context, string) (sessionoverlay.CutoverMode, error) {
	return r.mode, nil
}

type resolverReader struct {
	mu        sync.RWMutex
	rows      map[string]skills.Skill
	searchErr error
}

func resolverKey(id identity.Quadruple, name string, scope skills.Scope) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID + "\x00" + string(scope) + "\x00" + name
}

func (r *resolverReader) add(id identity.Quadruple, skill skills.Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = make(map[string]skills.Skill)
	}
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}
	r.rows[resolverKey(id, skill.Name, skill.Scope)] = skill
}

func (r *resolverReader) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	for _, scope := range []skills.Scope{skills.ScopeSession, skills.ScopeUser, skills.ScopeProject, skills.ScopeTenant, skills.ScopeGlobal} {
		if skill, err := r.GetScope(ctx, id, name, scope); err == nil {
			return skill, nil
		}
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (r *resolverReader) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return skills.Skill{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	skill, ok := r.rows[resolverKey(id, name, scope)]
	if !ok {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return skill, nil
}

func (r *resolverReader) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]skills.Skill, 0, len(r.rows))
	for _, skill := range r.rows {
		if skill.Scope == skills.ScopeUser {
			if skill.ScopeTenantID == id.TenantID && skill.ScopeProjectID == id.UserID {
				result = append(result, skill)
			}
			continue
		}
		// Test rows use ScopeTenantID/ScopeProjectID as the owning user/session
		// to keep this reader small while retaining exact identity behavior.
		if skill.ScopeTenantID == id.TenantID && skill.ScopeProjectID == id.SessionID {
			result = append(result, skill)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if filter.Offset >= len(result) {
		return []skills.Skill{}, nil
	}
	result = result[filter.Offset:]
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	return result, nil
}

func (r *resolverReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, errors.New("resolver must rank the composed view itself")
}

func (r *resolverReader) GetScopeAgent(ctx context.Context, id identity.Quadruple, _ string, name string, scope skills.Scope) (skills.Skill, error) {
	return r.GetScope(ctx, id, name, scope)
}

func (r *resolverReader) SearchAgent(ctx context.Context, id identity.Quadruple, _ string, query string, limit int) ([]skills.RankedSkill, error) {
	return r.Search(ctx, id, query, limit)
}

// SearchSnapshot is this test store's configured frozen-candidate policy.
func (r *resolverReader) SearchSnapshot(ctx context.Context, _ identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.searchErr != nil {
		return nil, r.searchErr
	}
	needle := strings.TrimSpace(query)
	if needle == "" {
		return nil, nil
	}
	result := make([]skills.RankedSkill, 0, len(candidates))
	for _, skill := range candidates {
		path := skills.PathRegex
		score := 0.75
		if strings.EqualFold(strings.TrimSpace(skill.Name), needle) || strings.EqualFold(strings.TrimSpace(skill.Title), needle) || strings.EqualFold(strings.TrimSpace(skill.Trigger), needle) {
			path, score = skills.PathExact, 1
		} else if !strings.Contains(strings.ToLower(skill.Name+" "+skill.Title+" "+skill.Trigger+" "+skill.Description+" "+strings.Join(skill.Tags, " ")), strings.ToLower(needle)) {
			continue
		}
		result = append(result, skills.RankedSkill{Skill: skill, Score: score, Path: path})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Skill.Name < result[j].Skill.Name })
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (*resolverReader) Upsert(context.Context, identity.Quadruple, skills.Skill) error {
	return errors.New("not used")
}
func (*resolverReader) Delete(context.Context, identity.Quadruple, string, skills.Scope) error {
	return errors.New("not used")
}
func (*resolverReader) DeleteAgent(context.Context, identity.Quadruple, string, string, skills.Scope) error {
	return errors.New("not used")
}
func (*resolverReader) DeleteSessionScope(context.Context, identity.Quadruple) error { return nil }
func (*resolverReader) Close(context.Context) error                                  { return nil }

func resolverSkill(id identity.Quadruple, name string, scope skills.Scope) skills.Skill {
	skill := durableSkill(name)
	skill.Scope = scope
	skill.ScopeTenantID = id.TenantID
	if scope == skills.ScopeUser {
		skill.ScopeProjectID = id.UserID
	} else {
		skill.ScopeProjectID = id.SessionID
	}
	return skill
}

func resolverConfig(id identity.Quadruple, personal *sessionoverlay.DurableStore, base skills.SkillStore, mode sessionoverlay.CutoverMode, membership sessionoverlay.SessionSkillMembership) sessionoverlay.SessionSkillResolverConfig {
	return sessionoverlay.SessionSkillResolverConfig{
		Run: id, AgentID: "agent-a", Base: base, Personal: personal, Cutover: resolverModeReader{mode: mode}, Membership: membership,
	}
}

func TestSessionSkillResolver_DualReadComposesOnlyExactLegacySessionTier(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-dual")
	id.RunID = "run-dual"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyID := id
	legacyID.RunID = ""
	if err := st.Save(context.Background(), legacyCandidate(t, legacyID, "agent-a", "legacy")); err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	for _, skill := range []skills.Skill{
		resolverSkill(id, "admin", skills.ScopeGlobal),
		resolverSkill(id, "user", skills.ScopeUser),
		resolverSkill(id, "legacy", skills.ScopeSession),
		resolverSkill(id, "unlisted-session", skills.ScopeSession),
	} {
		base.add(id, skill)
	}
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{
		AdminMembershipSet: true, AdminNames: []string{"admin"}, UserPersonalNames: []string{"user"},
	}))
	if err != nil {
		t.Fatalf("NewSessionSkillResolver: %v", err)
	}
	listed, err := resolver.List(context.Background(), id, skills.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := skillNames(listed); fmt.Sprint(got) != "[admin legacy user]" {
		t.Fatalf("composed names = %v", got)
	}
	session, err := resolver.SessionSkills(context.Background(), id)
	if err != nil || fmt.Sprint(skillNames(session)) != "[legacy]" {
		t.Fatalf("SessionSkills = (%v, %v)", skillNames(session), err)
	}
	if _, err := resolver.GetScope(context.Background(), id, "unlisted-session", skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("unreferenced base session row = %v, want ErrSkillNotFound", err)
	}
	if _, err := resolver.Get(context.Background(), id, "legacy"); err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	if _, err := resolver.Get(context.Background(), durableID("other"), "legacy"); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("cross-session Get = %v, want identity error", err)
	}
	result, err := resolver.Search(context.Background(), id, "legacy", 10)
	if err != nil || len(result) != 1 || result[0].Path != skills.PathExact {
		t.Fatalf("Search = (%+v, %v)", result, err)
	}
}

func TestSessionSkillResolver_SearchPolicyIsRequiredAndFailuresPassThrough(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-search-policy")
	id.RunID = "run-search-policy"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "searchable", skills.ScopeGlobal))

	missing := resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})
	missing.Base = nil
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), missing); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("missing configured search policy = %v, want ErrInvalidSessionSkillResolver", err)
	}

	semanticFailure := errors.New("embedder unavailable")
	configured := resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})
	base.searchErr = semanticFailure
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), configured)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Search(context.Background(), id, "searchable", 10); !errors.Is(err, semanticFailure) {
		t.Fatalf("configured semantic failure = %v, want preserved failure", err)
	}
}

func TestSessionSkillResolver_FailsLoudForMissingPinnedAndLegacyBodies(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-missing")
	id.RunID = "run-missing"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	// D-345: a durable-user membership name may legitimately outlive its
	// independently stored body. It is absent from the composed view, not a
	// resolver/bootstrap failure.
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{UserPersonalNames: []string{"missing-user"}}))
	if err != nil {
		t.Fatalf("missing ScopeUser membership body = %v, want harmless absence", err)
	}
	if listed, listErr := resolver.List(context.Background(), id, skills.ListFilter{Limit: 10}); listErr != nil || len(listed) != 0 {
		t.Fatalf("missing ScopeUser membership list = (%+v, %v), want empty", listed, listErr)
	}
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{AdminMembershipSet: true, AdminNames: []string{"missing"}})); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("missing admin pinned body = %v", err)
	}
	legacyID := id
	legacyID.RunID = ""
	if err := st.Save(context.Background(), legacyCandidate(t, legacyID, "agent-a", "missing-legacy")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrLegacySkillInvalid) {
		t.Fatalf("missing legacy body = %v, want ErrLegacySkillInvalid", err)
	}
}

func TestSessionSkillResolver_StateOnlyUsesOwnedExactPrefixAndTombstones(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-owned")
	id.RunID = "run-owned"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", durableSkill("owned"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.DeletePersonal(context.Background(), id, "agent-a", "gone"); err != nil {
		t.Fatal(err)
	}
	activateAgent(t, st, id, "agent-ab")
	if _, err := personal.SavePersonal(context.Background(), id, "agent-ab", durableSkill("other-agent"), "", ""); err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "legacy-session-must-not-leak", skills.ScopeSession))
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverStateOnly, sessionoverlay.SessionSkillMembership{}))
	if err != nil {
		t.Fatal(err)
	}
	session, err := resolver.SessionSkills(context.Background(), id)
	if err != nil || fmt.Sprint(skillNames(session)) != "[owned]" {
		t.Fatalf("state-only SessionSkills = (%v, %v)", skillNames(session), err)
	}
	for _, name := range []string{"gone", "legacy-session-must-not-leak", "other-agent"} {
		if _, err := resolver.Get(context.Background(), id, name); !errors.Is(err, skills.ErrSkillNotFound) {
			t.Fatalf("Get(%q) = %v, want ErrSkillNotFound", name, err)
		}
	}
}

func TestSessionSkillResolver_FailsLoudOnBaseOverflowAndCanonicalAliasConflict(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-bounds")
	id.RunID = "run-bounds"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("overflow", func(t *testing.T) {
		base := &resolverReader{}
		for i := range 1001 {
			base.add(id, resolverSkill(id, fmt.Sprintf("base-%04d", i), skills.ScopeGlobal))
		}
		if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
			t.Fatalf("base overflow = %v, want resolver error", err)
		}
	})
	t.Run("state-only owned overflow", func(t *testing.T) {
		for i := range skills.SnapshotSemanticCandidateCap + 1 {
			name := fmt.Sprintf("owned-%04d", i)
			if _, err := personal.SavePersonal(context.Background(), id, "agent-a", durableSkill(name), "", ""); err != nil {
				t.Fatalf("SavePersonal(%q): %v", name, err)
			}
		}
		if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, &resolverReader{}, sessionoverlay.CutoverStateOnly, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
			t.Fatalf("state-only owned overflow = %v, want ErrInvalidSessionSkillResolver", err)
		}
	})
	t.Run("same scope canonical aliases", func(t *testing.T) {
		base := &resolverReader{}
		first := resolverSkill(id, "Alpha", skills.ScopeGlobal)
		second := resolverSkill(id, " alpha ", skills.ScopeGlobal)
		second.Steps = []string{"different"}
		base.add(id, first)
		base.add(id, second)
		if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
			t.Fatalf("canonical alias conflict = %v, want resolver error", err)
		}
	})
}

func TestSessionSkillResolver_OwnedDecodeRejectsDuplicateFieldsAndExtraIsDeeplyImmutable(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-immutable")
	id.RunID = "run-immutable"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned := durableSkill("owned")
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", owned, "", ""); err != nil {
		t.Fatal(err)
	}
	kind, err := sessionoverlay.PersonalSkillKind("agent-a", "owned")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.Load(context.Background(), identity.Quadruple{Identity: id.Identity}, kind)
	if err != nil {
		t.Fatal(err)
	}
	stored.ID = state.NewEventID()
	stored.Bytes = bytes.Replace(stored.Bytes, []byte(`"canonical_name":"owned"`), []byte(`"canonical_name":"owned","canonical_name":"owned"`), 1)
	if err := st.Save(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, &resolverReader{}, sessionoverlay.CutoverStateOnly, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrPersonalRecordInvalid) {
		t.Fatalf("duplicate owned field = %v, want ErrPersonalRecordInvalid", err)
	}

	base := &resolverReader{}
	deep := resolverSkill(id, "deep", skills.ScopeGlobal)
	deep.Extra = map[string]any{"nested": map[string]any{"list": []any{map[string]any{"value": "original"}}}}
	base.add(id, deep)
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolver.Get(context.Background(), id, "deep")
	if err != nil {
		t.Fatal(err)
	}
	first.Extra["nested"].(map[string]any)["list"].([]any)[0].(map[string]any)["value"] = "mutated"
	second, err := resolver.Get(context.Background(), id, "deep")
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Extra["nested"].(map[string]any)["list"].([]any)[0].(map[string]any)["value"]; got != "original" {
		t.Fatalf("nested Extra alias leaked mutation: %v", got)
	}

	for name, extra := range map[string]map[string]any{
		"cycle": func() map[string]any {
			value := map[string]any{}
			value["self"] = value
			return value
		}(),
		"unsupported": {"func": func() {}},
	} {
		t.Run(name, func(t *testing.T) {
			bad := &resolverReader{}
			skill := resolverSkill(id, "bad-extra-"+name, skills.ScopeGlobal)
			skill.Extra = extra
			bad.add(id, skill)
			if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, bad, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
				t.Fatalf("unsupported Extra = %v, want ErrInvalidSessionSkillResolver", err)
			}
		})
	}
}

type flappingLifecycleStore struct {
	state.StateStore
	mu sync.Mutex
}

func (s *flappingLifecycleStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if err == nil && kind == "agentcfg.active" {
		s.mu.Lock()
		_ = s.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: record.Bytes})
		s.mu.Unlock()
	}
	return record, err
}

func TestSessionSkillResolver_FenceChurnExhausts(t *testing.T) {
	baseState := newDurableState(t)
	id := durableID("resolver-churn")
	id.RunID = "run-churn"
	activateAgent(t, baseState, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(&flappingLifecycleStore{StateStore: baseState}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})); !errors.Is(err, sessionoverlay.ErrSessionSkillReadUnstable) {
		t.Fatalf("churn build = %v, want ErrSessionSkillReadUnstable", err)
	}
}

func TestSessionSkillResolver_ConcurrentReuseCancellationAndIsolation(t *testing.T) {
	st := newDurableState(t)
	const n = 128
	id := durableID("shared")
	id.RunID = "run-shared"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "shared-skill", skills.ScopeGlobal))
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{}))
	if err != nil {
		t.Fatalf("NewSessionSkillResolver: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if i%2 == 0 {
				if _, err := resolver.Search(ctx, id, "shared-skill", 1); !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("%d canceled search = %w", i, err)
				}
				return
			}
			got, err := resolver.Get(ctx, id, "shared-skill")
			if err != nil || got.Name != "shared-skill" {
				errs <- fmt.Errorf("%d own skill = (%q, %w)", i, got.Name, err)
				return
			}
			if listed, err := resolver.List(ctx, id, skills.ListFilter{Limit: 1}); err != nil || len(listed) != 1 || listed[0].Name != "shared-skill" {
				errs <- fmt.Errorf("%d list = (%+v, %w)", i, listed, err)
				return
			}
			if ranked, err := resolver.Search(ctx, id, "shared-skill", 1); err != nil || len(ranked) != 1 || ranked[0].Path != skills.PathExact {
				errs <- fmt.Errorf("%d search = (%+v, %w)", i, ranked, err)
				return
			}
			other := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "other", SessionID: "other"}, RunID: "other"}
			if _, err := resolver.Get(ctx, other, got.Name); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
				errs <- fmt.Errorf("%d cross identity = %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func skillNames(in []skills.Skill) []string {
	result := make([]string, len(in))
	for i, skill := range in {
		result[i] = skill.Name
	}
	return result
}

func TestSessionSkillResolver_OperatorTierComposesLastWithProvenance(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-op-tier")
	id.RunID = "run-op-tier"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	// "shared" exists on EVERY caller rung (base global, durable user, owned
	// session) AND in both operator sources, so the strict merge must dedupe
	// the operator tier to one source=both item and it must win all three
	// caller rungs.
	if _, err := personal.SavePersonal(context.Background(), id, "agent-a", durableSkill("shared"), "", ""); err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "shared", skills.ScopeGlobal))
	base.add(id, resolverSkill(id, "shared", skills.ScopeUser))
	base.add(id, resolverSkill(id, "unrelated", skills.ScopeGlobal))
	boot := []bootpacks.Entry{testBootEntry("shared"), testBootEntry("boot-only")}
	packs := []skills.Skill{testRevisionPack("shared"), testRevisionPack("rev-only")}
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverStateOnly, sessionoverlay.SessionSkillMembership{
		UserPersonalNames: []string{"shared"},
		Boot:              boot,
		Packs:             packs,
	}))
	if err != nil {
		t.Fatalf("NewSessionSkillResolver: %v", err)
	}

	// The operator tier wins every caller rung for "shared": the composed Get
	// body is the operator body (Origin=pack, the retained boot body), not the
	// base / user / session row.
	got, err := resolver.Get(context.Background(), id, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != skills.OriginPack || got.Description != "operator body for shared" {
		t.Fatalf("operator tier did not win caller rungs: %+v", got)
	}
	// The session-only API keeps returning the session tier (unchanged
	// semantic: SessionSkills never widens into the operator tier).
	session, err := resolver.SessionSkills(context.Background(), id)
	if err != nil || fmt.Sprint(skillNames(session)) != "[shared]" {
		t.Fatalf("SessionSkills = (%v, %v), want the owned session row", skillNames(session), err)
	}

	// Deterministic composed view: the deduped shared (both), boot-only,
	// rev-only, and the unrelated base row.
	listed, err := resolver.List(context.Background(), id, skills.ListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(skillNames(listed)) != "[boot-only rev-only shared unrelated]" {
		t.Fatalf("composed list = %v", skillNames(listed))
	}

	// Provenance: the gated accessor reports source markers + set hashes.
	tier, err := resolver.OperatorTier(context.Background(), id)
	if err != nil {
		t.Fatalf("OperatorTier: %v", err)
	}
	if source, ok := tier.Source("shared"); !ok || source != skills.OperatorTierSourceBoth {
		t.Fatalf("Source(shared) = (%q, %v), want both", source, ok)
	}
	if source, ok := tier.Source("boot-only"); !ok || source != skills.OperatorTierSourceBoot {
		t.Fatalf("Source(boot-only) = (%q, %v), want boot", source, ok)
	}
	if source, ok := tier.Source("rev-only"); !ok || source != skills.OperatorTierSourceRevision {
		t.Fatalf("Source(rev-only) = (%q, %v), want revision", source, ok)
	}
	if tier.BootPackSetHash() != referenceBootPackSetHash(t, boot) {
		t.Fatalf("boot set hash = %q, want %q", tier.BootPackSetHash(), referenceBootPackSetHash(t, boot))
	}
	if tier.CombinedHash() == "" || tier.RevisionHash() == "" {
		t.Fatal("combined/revision hashes must be present")
	}
	// Identity-gated: a foreign quadruple cannot read the provenance.
	if _, err := resolver.OperatorTier(context.Background(), durableID("other")); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("cross-identity OperatorTier = %v, want identity error", err)
	}
}

func TestSessionSkillResolver_OperatorTierConflictAndBoundFailRunStart(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-op-conflict")
	id.RunID = "run-op-conflict"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("differing hash conflict", func(t *testing.T) {
		conflict := testRevisionPack("shared")
		conflict.Steps = []string{"different"}
		membership := sessionoverlay.SessionSkillMembership{
			Boot:  []bootpacks.Entry{testBootEntry("shared")},
			Packs: []skills.Skill{conflict},
		}
		if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, &resolverReader{}, sessionoverlay.CutoverDualRead, membership)); !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("conflict run start = %v, want ErrOperatorTierConflict", err)
		}
	})
	t.Run("257 items bound", func(t *testing.T) {
		membership := sessionoverlay.SessionSkillMembership{
			Boot:  testBootEntries("boot", 256),
			Packs: testRevisionPacks("extra", 1),
		}
		if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, &resolverReader{}, sessionoverlay.CutoverDualRead, membership)); !errors.Is(err, sessionoverlay.ErrOperatorTierBound) {
			t.Fatalf("bound run start = %v, want ErrOperatorTierBound", err)
		}
	})
}

func TestSessionSkillResolver_OperatorTierTwoAgentsUsersNoBleed(t *testing.T) {
	st := newDurableState(t)
	seed := durableID("seed")
	activateAgent(t, st, seed, "agent-a")
	activateAgent(t, st, seed, "agent-b")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	idA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user-a", SessionID: "session-a"}, RunID: "run-a"}
	idB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user-b", SessionID: "session-b"}, RunID: "run-b"}
	build := func(id identity.Quadruple, agentID, bootName, revName string) *sessionoverlay.SessionSkillResolver {
		t.Helper()
		resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), sessionoverlay.SessionSkillResolverConfig{
			Run: id, AgentID: agentID, Base: &resolverReader{}, Personal: personal,
			Cutover: resolverModeReader{mode: sessionoverlay.CutoverDualRead},
			Membership: sessionoverlay.SessionSkillMembership{
				Boot:  []bootpacks.Entry{testBootEntry(bootName)},
				Packs: []skills.Skill{testRevisionPack(revName)},
			},
		})
		if err != nil {
			t.Fatalf("resolver(%s/%s): %v", agentID, id.UserID, err)
		}
		return resolver
	}
	resolverA := build(idA, "agent-a", "a-boot", "a-rev")
	resolverB := build(idB, "agent-b", "b-boot", "b-rev")

	// Each agent's composed view contains ONLY its own operator tier.
	for _, name := range []string{"a-rev", "a-boot"} {
		if _, err := resolverA.Get(context.Background(), idA, name); err != nil {
			t.Fatalf("agent-a own %q: %v", name, err)
		}
		if _, err := resolverB.Get(context.Background(), idB, name); !errors.Is(err, skills.ErrSkillNotFound) {
			t.Fatalf("agent-b saw agent-a %q: %v", name, err)
		}
	}
	if _, err := resolverB.Get(context.Background(), idB, "b-boot"); err != nil {
		t.Fatalf("agent-b own boot entry: %v", err)
	}

	// Cross-identity calls (different user/session/run) fail closed on the
	// identity gate for both the composed view and the tier provenance.
	for _, call := range []struct {
		resolver *sessionoverlay.SessionSkillResolver
		id       identity.Quadruple
	}{
		{resolverA, idB},
		{resolverB, idA},
	} {
		if _, err := call.resolver.Get(context.Background(), call.id, "anything"); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
			t.Fatalf("cross identity Get = %v, want identity error", err)
		}
		if _, err := call.resolver.OperatorTier(context.Background(), call.id); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
			t.Fatalf("cross identity OperatorTier = %v, want identity error", err)
		}
	}

	// A foreign tenant never composes the boot baseline: the resolver was
	// built for the exact (tenant, user, session, run) quadruple.
	foreign := idA
	foreign.TenantID = "other-tenant"
	if _, err := resolverA.Get(context.Background(), foreign, "a-rev"); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("foreign tenant Get = %v, want identity error", err)
	}
}

func TestSessionSkillResolver_OperatorTierInFlightImmutability(t *testing.T) {
	st := newDurableState(t)
	id := durableID("resolver-op-immutable")
	id.RunID = "run-op-immutable"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "shared", skills.ScopeGlobal))
	first, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{
		Boot:  []bootpacks.Entry{testBootEntry("shared")},
		Packs: []skills.Skill{testRevisionPack("shared")},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// A LATER snapshot with a different membership (the boot body changed) is
	// an independent immutable value; concurrent changes affect only later
	// snapshots.
	changed := testBootEntry("shared")
	changed.Skill.Steps = []string{"new body"}
	changed.SemanticHash = skills.CanonicalContentHash(changed.Skill)
	second, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{
		Boot: []bootpacks.Entry{changed},
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the first snapshot's returned copy must never reach the first
	// snapshot again, nor the second snapshot, nor the shared tier.
	mutated, err := first.Get(context.Background(), id, "shared")
	if err != nil {
		t.Fatal(err)
	}
	mutated.Steps[0] = "mutated"

	again, err := first.Get(context.Background(), id, "shared")
	if err != nil || again.Steps[0] != "do it" {
		t.Fatalf("first snapshot drifted: (%q, %v)", again.Steps[0], err)
	}
	secondGot, err := second.Get(context.Background(), id, "shared")
	if err != nil || secondGot.Steps[0] != "new body" {
		t.Fatalf("second snapshot affected by first snapshot mutation: (%q, %v)", secondGot.Steps[0], err)
	}
	tier, err := first.OperatorTier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := tier.Get("shared"); got.Skill.Steps[0] != "do it" {
		t.Fatalf("shared tier drifted: %v", got.Skill.Steps)
	}
	// The tier accessor is stable across reads.
	if againTier, err := first.OperatorTier(context.Background(), id); err != nil || !reflect.DeepEqual(againTier.Items(), tier.Items()) {
		t.Fatalf("tier accessor not stable: (%v, %v)", err, againTier.Items())
	}
}

func TestSessionSkillResolver_OperatorTierConcurrentReuse(t *testing.T) {
	baseline := runtime.NumGoroutine()
	st := newDurableState(t)
	id := durableID("resolver-op-shared")
	id.RunID = "run-op-shared"
	activateAgent(t, st, id, "agent-a")
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := &resolverReader{}
	base.add(id, resolverSkill(id, "shared-skill", skills.ScopeGlobal))
	membership := sessionoverlay.SessionSkillMembership{
		Boot:  append(testBootEntries("shared", 32), testBootEntry("shared-skill")),
		Packs: append(testRevisionPacks("shared", 32), testRevisionPack("shared-skill")),
	}
	resolver, err := sessionoverlay.NewSessionSkillResolver(context.Background(), resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, membership))
	if err != nil {
		t.Fatalf("NewSessionSkillResolver: %v", err)
	}
	tier, err := resolver.OperatorTier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	wantItems := tier.Items()
	wantHash := tier.CombinedHash()
	wantSource, _ := tier.Source("shared-skill")

	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if i%2 == 0 {
				if _, err := resolver.Get(ctx, id, "shared-skill"); !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("%d canceled Get = %w", i, err)
				}
				return
			}
			got, err := resolver.Get(ctx, id, "shared-skill")
			if err != nil || got.Origin != skills.OriginPack {
				errs <- fmt.Errorf("%d operator Get = (%q, %w)", i, got.Name, err)
				return
			}
			view, err := resolver.OperatorTier(ctx, id)
			if err != nil {
				errs <- fmt.Errorf("%d OperatorTier: %w", i, err)
				return
			}
			if view.CombinedHash() != wantHash || !reflect.DeepEqual(view.Items(), wantItems) {
				errs <- fmt.Errorf("%d tier drifted", i)
				return
			}
			if source, ok := view.Source("shared-skill"); !ok || source != wantSource {
				errs <- fmt.Errorf("%d source marker drifted", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertGoroutinesRestored(t, baseline)
}
