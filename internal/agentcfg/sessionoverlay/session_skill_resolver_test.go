package sessionoverlay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

type resolverModeReader struct{ mode sessionoverlay.CutoverMode }

func (r resolverModeReader) Mode(context.Context, string) (sessionoverlay.CutoverMode, error) {
	return r.mode, nil
}

type resolverReader struct {
	mu   sync.RWMutex
	rows map[string]skills.Skill
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

// resolverSearcher is the test stand-in for the boot-configured snapshot
// search policy. Production supplies a policy that retains its configured
// full-text or semantic retrieval strategy; the resolver only owns the frozen
// candidate set and validates the returned rows.
type resolverSearcher struct {
	err error
}

func (s resolverSearcher) SearchSnapshot(ctx context.Context, _ identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
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

func resolverConfig(id identity.Quadruple, personal *sessionoverlay.DurableStore, base skills.SkillReader, mode sessionoverlay.CutoverMode, membership sessionoverlay.SessionSkillMembership) sessionoverlay.SessionSkillResolverConfig {
	return sessionoverlay.SessionSkillResolverConfig{
		Run: id, AgentID: "agent-a", Base: base, Searcher: resolverSearcher{}, Personal: personal, Cutover: resolverModeReader{mode: mode}, Membership: membership,
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
	missing.Searcher = nil
	if _, err := sessionoverlay.NewSessionSkillResolver(context.Background(), missing); !errors.Is(err, sessionoverlay.ErrInvalidSessionSkillResolver) {
		t.Fatalf("missing configured search policy = %v, want ErrInvalidSessionSkillResolver", err)
	}

	semanticFailure := errors.New("embedder unavailable")
	configured := resolverConfig(id, personal, base, sessionoverlay.CutoverDualRead, sessionoverlay.SessionSkillMembership{})
	configured.Searcher = resolverSearcher{err: semanticFailure}
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
