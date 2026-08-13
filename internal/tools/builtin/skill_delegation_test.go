// skill_delegation_test.go — Phase 111d (D-201): the skill_* built-ins
// are thin delegations to the Phase-38 handlers / Phase-41 generator.
// These tests pin (a) the registration-time wiring-dep posture (Bus
// mandatory; Redactor mandatory for skill_propose), (b) delegation
// parity (the registered tool's output IS the rich handler's output —
// capability filter + redaction + normalisation included), (c) the
// server-computed capability envelope (default-deny against the run's
// visible-tool set; never LLM-supplied), (d) the `skill_get` budgeter
// running on the production registration path, and (e) D-025
// concurrent reuse through the new registration path.

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// fakeSkillStore is a minimal identity-checking SkillStore for the
// unit-level delegation tests (the integration E2E uses the real
// localdb driver). ScopeUser rows are session-zeroed and resolve across
// sessions of the same tenant/user; every other row stays session-pinned.
// Concurrent-safe.
type fakeSkillStore struct {
	mu    sync.RWMutex
	rows  map[string]skills.Skill // key: tenant + "/" + user + "/" + storage-session + "/" + name
	bus   events.EventBus
	limit int
}

func (f *fakeSkillStore) GetScopeAgent(ctx context.Context, q identity.Quadruple, agentID, name string, scope skills.Scope) (skills.Skill, error) {
	return f.GetScope(ctx, q, name, scope)
}
func (f *fakeSkillStore) SearchAgent(ctx context.Context, q identity.Quadruple, agentID, query string, limit int) ([]skills.RankedSkill, error) {
	return f.Search(ctx, q, query, limit)
}
func (f *fakeSkillStore) DeleteAgent(ctx context.Context, q identity.Quadruple, agentID, name string, scope skills.Scope) error {
	return f.Delete(ctx, q, name, scope)
}

func newFakeSkillStore(bus events.EventBus) *fakeSkillStore {
	return &fakeSkillStore{rows: map[string]skills.Skill{}, bus: bus, limit: 20}
}

func (f *fakeSkillStore) key(q identity.Quadruple, scope skills.Scope, name string) string {
	session := q.SessionID
	if scope == skills.ScopeUser {
		session = ""
	}
	return q.TenantID + "/" + q.UserID + "/" + session + "/" + name
}

func (f *fakeSkillStore) sessionPrefix(q identity.Quadruple) string {
	return q.TenantID + "/" + q.UserID + "/" + q.SessionID + "/"
}

func (f *fakeSkillStore) userPrefix(q identity.Quadruple) string {
	return q.TenantID + "/" + q.UserID + "//"
}

func (f *fakeSkillStore) visible(q identity.Quadruple, key string, skill skills.Skill) bool {
	return strings.HasPrefix(key, f.sessionPrefix(q)) ||
		(skill.Scope == skills.ScopeUser && strings.HasPrefix(key, f.userPrefix(q)))
}

func (f *fakeSkillStore) Upsert(ctx context.Context, q identity.Quadruple, s skills.Skill) error {
	if err := skills.ValidateIdentity(q); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[f.key(q, s.Scope, s.Name)] = s
	return nil
}

func (f *fakeSkillStore) Get(ctx context.Context, q identity.Quadruple, name string) (skills.Skill, error) {
	if err := skills.ValidateIdentity(q); err != nil {
		return skills.Skill{}, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if s, ok := f.rows[f.key(q, skills.ScopeSession, name)]; ok {
		return s, nil
	}
	if s, ok := f.rows[f.key(q, skills.ScopeUser, name)]; ok && s.Scope == skills.ScopeUser {
		return s, nil
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (f *fakeSkillStore) GetScope(ctx context.Context, q identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := skills.ValidateIdentity(q); err != nil {
		return skills.Skill{}, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if s, ok := f.rows[f.key(q, scope, name)]; ok && s.Scope == scope {
		return s, nil
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (f *fakeSkillStore) List(ctx context.Context, q identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := skills.ValidateIdentity(q); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]skills.Skill, 0, len(f.rows))
	for k, s := range f.rows {
		if f.visible(q, k, s) && (filter.Scope == "" || s.Scope == filter.Scope) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeSkillStore) Search(ctx context.Context, q identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if err := skills.ValidateIdentity(q); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = f.limit
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]skills.RankedSkill, 0, len(f.rows))
	for k, s := range f.rows {
		if !f.visible(q, k, s) {
			continue
		}
		if query == "" || strings.Contains(strings.ToLower(s.Title+s.Trigger+s.Name), strings.ToLower(query)) {
			out = append(out, skills.RankedSkill{Skill: s, Score: 0.9, Path: skills.PathExact})
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSkillStore) SearchSnapshot(ctx context.Context, q identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if err := skills.ValidateIdentity(q); err != nil {
		return nil, err
	}
	return skills.SearchSnapshotRegexExact(ctx, query, candidates, limit)
}

func (f *fakeSkillStore) Delete(ctx context.Context, q identity.Quadruple, name string, scope skills.Scope) error {
	if err := skills.ValidateIdentity(q); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(q, scope, name)
	if _, ok := f.rows[k]; !ok {
		return skills.ErrSkillNotFound
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeSkillStore) DeleteSessionScope(ctx context.Context, q identity.Quadruple) error {
	if err := skills.ValidateIdentity(q); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, skill := range f.rows {
		if skill.Scope == skills.ScopeSession && strings.HasPrefix(key, f.sessionPrefix(q)) {
			delete(f.rows, key)
		}
	}
	return nil
}

func (f *fakeSkillStore) Close(context.Context) error { return nil }

func TestFakeSkillStore_UserScopeCrossSessionAndSessionSweep(t *testing.T) {
	t.Parallel()
	store := newFakeSkillStore(skillTestBus(t))
	sessionA := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "a"}}
	sessionB := sessionA
	sessionB.SessionID = "b"
	userSkill := skills.Skill{Name: "durable", Scope: skills.ScopeUser}
	sessionSkill := skills.Skill{Name: "ephemeral", Scope: skills.ScopeSession}
	if err := store.Upsert(context.Background(), sessionA, userSkill); err != nil {
		t.Fatalf("Upsert user: %v", err)
	}
	if err := store.Upsert(context.Background(), sessionA, sessionSkill); err != nil {
		t.Fatalf("Upsert session: %v", err)
	}
	if got, err := store.Get(context.Background(), sessionB, userSkill.Name); err != nil || got.Scope != skills.ScopeUser {
		t.Fatalf("Get user from sibling session: got=%+v err=%v", got, err)
	}
	if got, err := store.List(context.Background(), sessionB, skills.ListFilter{Scope: skills.ScopeUser}); err != nil || len(got) != 1 || got[0].Name != userSkill.Name {
		t.Fatalf("List user from sibling session: got=%+v err=%v", got, err)
	}
	if got, err := store.Search(context.Background(), sessionB, userSkill.Name, 5); err != nil || len(got) != 1 || got[0].Skill.Name != userSkill.Name {
		t.Fatalf("Search user from sibling session: got=%+v err=%v", got, err)
	}
	if _, err := store.Get(context.Background(), sessionB, sessionSkill.Name); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("Get session row from sibling: err=%v, want ErrSkillNotFound", err)
	}
	if err := store.DeleteSessionScope(context.Background(), sessionA); err != nil {
		t.Fatalf("DeleteSessionScope: %v", err)
	}
	if _, err := store.Get(context.Background(), sessionA, sessionSkill.Name); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("session row survived sweep: err=%v", err)
	}
	if got, err := store.Get(context.Background(), sessionB, userSkill.Name); err != nil || got.Scope != skills.ScopeUser {
		t.Fatalf("user row did not survive sweep: got=%+v err=%v", got, err)
	}
}

// skillTestBus opens an inmem events bus for the delegation tests.
func skillTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// skillTestRegistryContext returns a full RegistryContext (minus
// Catalog, which the caller sets) suitable for registering every
// known built-in.
func skillTestRegistryContext(t *testing.T) RegistryContext {
	t.Helper()
	bus := skillTestBus(t)
	return RegistryContext{
		SkillStore: newFakeSkillStore(bus),
		Bus:        bus,
		Redactor:   auditpatterns.New(),
	}
}

var skillTestID = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

func skillTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), skillTestID, "r1")
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// seedDelegationCatalog registers the skill built-ins plus two
// catalog tools: `kb_search` (no auth scopes — always visible) and
// `scoped_tool` (AuthScopes the run is NOT granted — invisible). The
// pair drives the default-deny capability assertions.
func seedDelegationCatalog(t *testing.T) (tools.ToolCatalog, RegistryContext) {
	t.Helper()
	cat := tools.NewCatalog()
	rc := skillTestRegistryContext(t)
	rc.Catalog = cat

	type emptyArgs struct{}
	type emptyOut struct{}
	if err := inproc.RegisterFunc[emptyArgs, emptyOut](cat, "kb_search",
		func(context.Context, emptyArgs) (emptyOut, error) { return emptyOut{}, nil },
		tools.WithDescription("test fixture tool")); err != nil {
		t.Fatalf("register kb_search: %v", err)
	}
	if err := inproc.RegisterFunc[emptyArgs, emptyOut](cat, "scoped_tool",
		func(context.Context, emptyArgs) (emptyOut, error) { return emptyOut{}, nil },
		tools.WithDescription("scope-gated fixture tool"),
		tools.WithAuthScopes("admin:everything")); err != nil {
		t.Fatalf("register scoped_tool: %v", err)
	}
	if err := RegisterWith(rc, []string{"skill_search", "skill_get", "skill_list", "skill_propose"}); err != nil {
		t.Fatalf("RegisterWith(skill_*): %v", err)
	}
	return cat, rc
}

// invoke runs a registered descriptor with JSON-marshalled args and
// unmarshals the result into out.
func invoke[T any](t *testing.T, cat tools.ToolCatalog, ctx context.Context, name string, args any) T {
	t.Helper()
	desc, ok := cat.Resolve(name)
	if !ok {
		t.Fatalf("Resolve(%q): not found", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := desc.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("Invoke(%q): %v", name, err)
	}
	body, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

// TestSkillBuiltins_RequireWiringDepsAtRegistration pins the fail-loud
// posture: nil Bus rejects every skill_* registration; nil Redactor
// additionally rejects skill_propose.
func TestSkillBuiltins_RequireWiringDepsAtRegistration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"skill_search", "skill_get", "skill_list", "skill_propose"} {
		rc := RegistryContext{Catalog: tools.NewCatalog()}
		err := RegisterWith(rc, []string{name})
		if !errors.Is(err, ErrRegisterFailed) || !strings.Contains(err.Error(), "Bus is required") {
			t.Errorf("%s without Bus: err = %v, want ErrRegisterFailed naming Bus", name, err)
		}
	}
	// skill_propose additionally demands the Redactor.
	rc := skillTestRegistryContext(t)
	rc.Catalog = tools.NewCatalog()
	rc.Redactor = nil
	err := RegisterWith(rc, []string{"skill_propose"})
	if !errors.Is(err, ErrRegisterFailed) || !strings.Contains(err.Error(), "Redactor is required") {
		t.Errorf("skill_propose without Redactor: err = %v, want ErrRegisterFailed naming Redactor", err)
	}
}

// TestSkillReadBuiltins_AcceptReadOnlyDependency pins the compatibility seam:
// read builtins prefer SkillReader and do not require a mutation-capable
// SkillStore, while skill_propose retains its writer dependency.
func TestSkillReadBuiltins_AcceptReadOnlyDependency(t *testing.T) {
	t.Parallel()
	bus := skillTestBus(t)
	backing := newFakeSkillStore(bus)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}
	if err := backing.Upsert(ctx, q, skills.Skill{
		Name: "read-only-skill", Title: "Read only", Trigger: "read trigger",
		Steps: []string{"read"}, Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	cat := tools.NewCatalog()
	rc := RegistryContext{
		Catalog:     cat,
		SkillReader: skills.SkillReader(backing),
		Bus:         bus,
		Redactor:    auditpatterns.New(),
	}
	if err := RegisterWith(rc, []string{"skill_search", "skill_get", "skill_list", "skill_propose"}); err != nil {
		t.Fatalf("RegisterWith: %v", err)
	}
	if got := invoke[skilltools.GetResult](t, cat, ctx, "skill_get", SkillGetArgs{
		Names: []string{"read-only-skill"}, MaxTokens: 1024,
	}); len(got.Skills) != 1 || got.Skills[0].Name != "read-only-skill" {
		t.Fatalf("skill_get = %+v, want read-only-skill", got)
	}

	desc, ok := cat.Resolve("skill_propose")
	if !ok {
		t.Fatal("Resolve(skill_propose): not found")
	}
	raw, err := json.Marshal(map[string]any{"skill": map[string]any{
		"name": "proposal", "trigger": "trigger", "steps": []string{"step"},
	}})
	if err != nil {
		t.Fatalf("marshal skill_propose args: %v", err)
	}
	if _, err := desc.Invoke(ctx, raw); err == nil || !strings.Contains(err.Error(), "SkillStore is nil") {
		t.Fatalf("skill_propose with read-only dep error = %v, want SkillStore is nil", err)
	}
}

// TestSkillSearch_Delegation_AppliesCapabilityFilter proves the §13
// closure: the PRODUCTION-registered skill_search runs the Phase-38
// capability filter — a skill requiring a tool the run cannot see
// (scope-gated `scoped_tool`) is excluded; a skill requiring a
// visible tool survives. The pre-111d thin path returned both.
func TestSkillSearch_Delegation_AppliesCapabilityFilter(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}

	store := rc.SkillStore
	mustUpsert := func(s skills.Skill) {
		t.Helper()
		if err := store.Upsert(ctx, q, s); err != nil {
			t.Fatalf("Upsert(%s): %v", s.Name, err)
		}
	}
	mustUpsert(skills.Skill{
		Name: "visible-skill", Title: "Visible", Trigger: "trigger one",
		Steps: []string{"use kb_search"}, RequiredTools: []string{"kb_search"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	})
	mustUpsert(skills.Skill{
		Name: "gated-skill", Title: "Gated", Trigger: "trigger two",
		Steps: []string{"use scoped_tool"}, RequiredTools: []string{"scoped_tool"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	})

	out := invoke[skilltools.SearchResult](t, cat, ctx, "skill_search", SkillSearchArgs{Query: "trigger"})
	names := make([]string, 0, len(out.Skills))
	for _, r := range out.Skills {
		names = append(names, r.Skill.Name)
	}
	if !reflect.DeepEqual(names, []string{"visible-skill"}) {
		t.Errorf("skill_search returned %v, want [visible-skill] (default-deny filter against the run's visible-tool set)", names)
	}
}

// TestSkillSearch_Delegation_ParityWithRichHandler — the registered
// builtin's output is byte-identical to calling the Phase-38 handler
// directly with the same server-computed capability: ONE
// implementation, two carriers collapsed (filter + redaction +
// normalisation included transitively).
func TestSkillSearch_Delegation_ParityWithRichHandler(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}

	if err := rc.SkillStore.Upsert(ctx, q, skills.Skill{
		Name: "parity-skill", Title: "Parity", Trigger: "parity trigger",
		Steps:  []string{"step"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}

	got := invoke[skilltools.SearchResult](t, cat, ctx, "skill_search", SkillSearchArgs{Query: "parity"})
	want, err := skilltools.SearchHandler(ctx, rc.SkillStore, rc.Bus, skilltools.SearchArgs{
		Query:      "parity",
		Capability: runCapability(ctx, rc),
	})
	if err != nil {
		t.Fatalf("direct SearchHandler: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("delegation parity broken:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestSkillSearch_Delegation_RedactionContract pins the redaction
// semantics on the production registration path (Wave C checkpoint
// audit). The 111d plan sketched a golden "disallowed tool name in a
// skill body" fixture through the builtin skill_search — that fixture
// is STRUCTURALLY unreachable: Redact's scrub set derives from the
// skill's RequiredTools (capfilter.DisallowedNames(s.RequiredTools,
// allowed)), and Filter drops any skill whose RequiredTools are not a
// subset of the allowed set BEFORE Redact runs — so a survivor never
// carries a disallowed required tool to scrub. This test pins both
// halves of that contract through the production carrier:
//
//  1. A skill with a PARTIALLY disallowed RequiredTools list is
//     filtered out entirely (the disallowed name never reaches the
//     planner at all — stronger than redaction).
//  2. A surviving skill whose PROSE mentions a gated tool it does not
//     require is returned verbatim — prose-mention scrubbing keys off
//     RequiredTools, not arbitrary text (brief 04 §4.5; the redactor
//     unit suite in internal/skills/tools/redactor_test.go remains
//     the scrub-mechanics gate).
func TestSkillSearch_Delegation_RedactionContract(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}

	mustUpsert := func(s skills.Skill) {
		t.Helper()
		if err := rc.SkillStore.Upsert(ctx, q, s); err != nil {
			t.Fatalf("Upsert(%s): %v", s.Name, err)
		}
	}
	// Half 1: requires BOTH a visible and a gated tool → filtered out.
	mustUpsert(skills.Skill{
		Name: "partially-gated", Title: "Partially gated", Trigger: "redaction contract",
		Steps:         []string{"kb_search then scoped_tool"},
		RequiredTools: []string{"kb_search", "scoped_tool"},
		Origin:        skills.OriginGenerated, Scope: skills.ScopeProject,
	})
	// Half 2: satisfiable RequiredTools, prose mentions the gated tool.
	mustUpsert(skills.Skill{
		Name: "prose-mention", Title: "Mentions scoped_tool in prose", Trigger: "redaction contract",
		Steps:         []string{"try scoped_tool if you somehow have it, else kb_search"},
		RequiredTools: []string{"kb_search"},
		Origin:        skills.OriginGenerated, Scope: skills.ScopeProject,
	})

	out := invoke[skilltools.SearchResult](t, cat, ctx, "skill_search", SkillSearchArgs{Query: "redaction contract"})
	names := make([]string, 0, len(out.Skills))
	for _, r := range out.Skills {
		names = append(names, r.Skill.Name)
	}
	if !reflect.DeepEqual(names, []string{"prose-mention"}) {
		t.Fatalf("skill_search returned %v, want [prose-mention] (partially-gated must be filtered, not redacted)", names)
	}
	got := out.Skills[0].Skill
	if !strings.Contains(got.Title, "scoped_tool") || !strings.Contains(got.Steps[0], "scoped_tool") {
		t.Errorf("prose mention was scrubbed (%q / %q) — the scrub contract keys off RequiredTools; if this changed deliberately, update this pin AND the redactor godoc", got.Title, got.Steps[0])
	}
}

// TestSkillGet_Delegation_RunsBudgeter proves the tiered token
// budgeter runs on the production registration: a small max_tokens
// forces the ladder to drop optional fields (Summarized=true), and an
// impossible budget surfaces ErrSkillTooLarge LOUD.
func TestSkillGet_Delegation_RunsBudgeter(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}

	long := strings.Repeat("a very long precondition sentence ", 40)
	if err := rc.SkillStore.Upsert(ctx, q, skills.Skill{
		Name: "budget-skill", Title: "Budget", Trigger: "budget trigger",
		Steps:         []string{"step one", "step two"},
		Preconditions: []string{long, long},
		FailureModes:  []string{long},
		Origin:        skills.OriginGenerated, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}

	out := invoke[skilltools.GetResult](t, cat, ctx, "skill_get", SkillGetArgs{
		Names: []string{"budget-skill"}, MaxTokens: 120,
	})
	if !out.Summarized {
		t.Error("Summarized=false — the budgeter ladder did not run on the production path")
	}
	if len(out.Skills) != 1 || len(out.Skills[0].Preconditions) != 0 {
		t.Errorf("budgeter did not drop optional fields: %+v", out.Skills)
	}

	// Impossible budget → loud ErrSkillTooLarge (no silent trim).
	desc, _ := cat.Resolve("skill_get")
	raw, _ := json.Marshal(SkillGetArgs{Names: []string{"budget-skill"}, MaxTokens: 1})
	if _, err := desc.Invoke(ctx, raw); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("impossible budget err = %v, want the ladder's fail-loud error", err)
	}
}

// TestSkillBuiltins_IdentityMandatory — a missing triple rejects via
// the delegated handlers' own gate (wrapped skills.ErrIdentityRequired).
func TestSkillBuiltins_IdentityMandatory(t *testing.T) {
	t.Parallel()
	cat, _ := seedDelegationCatalog(t)
	desc, _ := cat.Resolve("skill_search")
	raw, _ := json.Marshal(SkillSearchArgs{Query: "anything"})
	_, err := desc.Invoke(context.Background(), raw)
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("identity-less invoke err = %v, want skills.ErrIdentityRequired", err)
	}
}

// TestSkillList_Delegation_Registered — the Phase-38 third tool's
// first production registration: paged enumeration through the
// carrier, capability-filtered.
func TestSkillList_Delegation_Registered(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}
	if err := rc.SkillStore.Upsert(ctx, q, skills.Skill{
		Name: "list-skill", Title: "List", Trigger: "list trigger",
		Steps:  []string{"step"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	out := invoke[skilltools.ListResult](t, cat, ctx, "skill_list", SkillListArgs{})
	if len(out.Skills) != 1 || out.Skills[0].Name != "list-skill" {
		t.Errorf("skill_list = %+v, want the seeded skill", out.Skills)
	}
}

// TestSkillPropose_Delegation_D054Semantics — the Phase-41 generator's
// first production registration: persist=true persists with
// Origin=Generated and the receipt's result branch; the D-054
// audit-mandatory `skill.proposed` emit is observed DIRECTLY on the
// bus (Wave C checkpoint audit — the plan's "audit emit asserted
// through the production registration path" criterion, previously
// pinned only via the persist-implies-emitted rollback indirection);
// the conflict policy (pack-protected) rejects LOUD through the same
// carrier.
func TestSkillPropose_Delegation_D054Semantics(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	ctx := skillTestCtx(t)
	q := identity.Quadruple{Identity: skillTestID}

	// Subscribe BEFORE invoking so the emit cannot be missed.
	sub, err := rc.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  skillTestID.TenantID,
		User:    skillTestID.UserID,
		Session: skillTestID.SessionID,
		Types:   []events.EventType{skills.EventTypeSkillProposed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	receipt := invoke[generator.SkillReceipt](t, cat, ctx, "skill_propose", generator.ProposeArgs{
		Skill: generator.SkillDraft{
			Name: "proposed-skill", Trigger: "proposed trigger", Steps: []string{"do it"},
		},
		Persist: true,
	})
	if !receipt.Persisted || receipt.Result != generator.ResultPersisted || receipt.Origin != skills.OriginGenerated {
		t.Fatalf("receipt = %+v, want persisted Origin=generated", receipt)
	}

	// Exactly one `skill.proposed`, carrying the run identity and the
	// redacted payload shape.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before skill.proposed arrived")
		}
		if ev.Type != skills.EventTypeSkillProposed {
			t.Fatalf("event type = %s, want %s", ev.Type, skills.EventTypeSkillProposed)
		}
		if ev.Identity.Identity != skillTestID || ev.Identity.RunID != "r1" {
			t.Errorf("skill.proposed identity = %+v, want %+v run r1 (the rc.Bus → generator.Deps.Bus wiring carries the run quadruple)", ev.Identity, skillTestID)
		}
		payload, ok := ev.Payload.(generator.SkillProposedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want generator.SkillProposedPayload", ev.Payload)
		}
		if payload.Name != "proposed-skill" || payload.Result != string(generator.ResultPersisted) {
			t.Errorf("payload = %+v, want name proposed-skill result persisted", payload)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no skill.proposed event within bound — the D-054 audit emit did not reach the bus")
	}

	// Conflict policy: a pack row with the same name blocks the
	// generator (D-054 pack-protected) — loud error, not silent skip.
	if err := rc.SkillStore.Upsert(ctx, q, skills.Skill{
		Name: "pack-skill", Trigger: "pack trigger", Steps: []string{"s"},
		Origin: skills.OriginPack, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	desc, _ := cat.Resolve("skill_propose")
	raw, _ := json.Marshal(generator.ProposeArgs{
		Skill:   generator.SkillDraft{Name: "pack-skill", Trigger: "overwrite attempt", Steps: []string{"s"}},
		Persist: true,
	})
	if _, err := desc.Invoke(ctx, raw); err == nil || !errors.Is(err, generator.ErrSkillConflictSentinel) {
		t.Fatalf("pack-overwrite err = %v, want generator conflict", err)
	}
}

// TestSkillBuiltins_ConcurrentReuse — D-025 through the new
// registration path: N≥100 concurrent skill_search + skill_get
// invocations against ONE registered catalog under -race; no
// cross-talk (every response carries only the caller's tenant rows).
func TestSkillBuiltins_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	cat, rc := seedDelegationCatalog(t)
	q := identity.Quadruple{Identity: skillTestID}
	seedCtx := skillTestCtx(t)
	if err := rc.SkillStore.Upsert(seedCtx, q, skills.Skill{
		Name: "shared-skill", Title: "Shared", Trigger: "shared trigger",
		Steps:  []string{"step"},
		Origin: skills.OriginGenerated, Scope: skills.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	otherID := identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"}

	const n = 120
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	searchDesc, _ := cat.Resolve("skill_search")
	getDesc, _ := cat.Resolve("skill_get")
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := skillTestID
			wantRows := true
			if i%2 == 1 {
				id = otherID // the other tenant must see NOTHING
				wantRows = false
			}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("r%d", i))
			if err != nil {
				errCh <- err
				return
			}
			rawS, _ := json.Marshal(SkillSearchArgs{Query: "shared"})
			res, err := searchDesc.Invoke(ctx, rawS)
			if err != nil {
				errCh <- fmt.Errorf("search[%d]: %w", i, err)
				return
			}
			body, _ := json.Marshal(res.Value)
			var sr skilltools.SearchResult
			_ = json.Unmarshal(body, &sr)
			if wantRows != (len(sr.Skills) > 0) {
				errCh <- fmt.Errorf("search[%d]: cross-tenant bleed — tenant %s got %d rows", i, id.TenantID, len(sr.Skills))
				return
			}
			rawG, _ := json.Marshal(SkillGetArgs{Names: []string{"shared-skill"}})
			if _, err := getDesc.Invoke(ctx, rawG); err != nil {
				errCh <- fmt.Errorf("get[%d]: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
