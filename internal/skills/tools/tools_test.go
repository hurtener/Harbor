package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// fakeStore is a tiny in-memory `skills.SkillStore` for unit tests.
// Identity validation matches the production driver's contract.
type fakeStore struct {
	bus    events.EventBus
	skills map[string]skills.Skill
}

func newFakeStore(bus events.EventBus, items ...skills.Skill) *fakeStore {
	s := &fakeStore{bus: bus, skills: make(map[string]skills.Skill, len(items))}
	for _, it := range items {
		s.skills[it.Name] = it
	}
	return s
}

func (s *fakeStore) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	return errors.New("fakeStore: Upsert not used in Phase 38 tests")
}

func (s *fakeStore) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return skills.Skill{}, skills.EmitIdentityRejected(ctx, s.bus, id, "Get")
	}
	got, ok := s.skills[name]
	if !ok {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return got, nil
}

func (s *fakeStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, skills.EmitIdentityRejected(ctx, s.bus, id, "List")
	}
	out := make([]skills.Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk)
	}
	return out, nil
}

func (s *fakeStore) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, skills.EmitIdentityRejected(ctx, s.bus, id, "Search")
	}
	out := make([]skills.RankedSkill, 0, len(s.skills))
	for _, sk := range s.skills {
		if query == "" || strings.Contains(strings.ToLower(sk.Name+sk.Title+sk.Description+sk.Trigger), strings.ToLower(query)) {
			out = append(out, skills.RankedSkill{Skill: sk, Score: 0.9, Path: skills.PathFTS5})
		}
	}
	return out, nil
}

func (s *fakeStore) Delete(ctx context.Context, id identity.Quadruple, name string) error {
	return errors.New("fakeStore: Delete not used")
}

func (s *fakeStore) Close(ctx context.Context) error {
	return nil
}

func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func testIdentity() identity.Identity {
	return identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
}

func ctxWithIdentity(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), testIdentity(), "r1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// TestRegister_AllThreeToolsLanded — Register installs three tools
// with the expected names and the planner-visible LoadingAlways flag.
func TestRegister_AllThreeToolsLanded(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus)
	catalog := tcat.NewCatalog()

	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{
		skilltools.ToolNameSkillSearch,
		skilltools.ToolNameSkillGet,
		skilltools.ToolNameSkillList,
	} {
		d, ok := catalog.Resolve(name)
		if !ok {
			t.Fatalf("Resolve(%q) ok=false", name)
		}
		if d.Tool.Loading != tcat.LoadingAlways {
			t.Fatalf("%s: Loading=%q, want %q", name, d.Tool.Loading, tcat.LoadingAlways)
		}
		if d.Tool.Transport != tcat.TransportInProcess {
			t.Fatalf("%s: Transport=%q, want %q", name, d.Tool.Transport, tcat.TransportInProcess)
		}
		if d.Tool.SideEffects != tcat.SideEffectRead {
			t.Fatalf("%s: SideEffects=%q, want read", name, d.Tool.SideEffects)
		}
	}
}

func TestRegister_NilArgsRejected(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus)
	catalog := tcat.NewCatalog()

	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		name string
		fn   func() error
	}{
		{"nil catalog", func() error { return skilltools.Register(nil, store, skilltools.Deps{Bus: bus}) }},
		{"nil store", func() error { return skilltools.Register(catalog, nil, skilltools.Deps{Bus: bus}) }},
		{"nil bus", func() error { return skilltools.Register(catalog, store, skilltools.Deps{}) }},
	}
	for _, c := range cases {

		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := c.fn(); err == nil {
				t.Fatalf("err=nil, want non-nil for %s", c.name)
			}
		})
	}
}

// TestSearchHandler_HappyPath asserts identity propagation +
// capability filter via the catalog.Resolve → Invoke pipeline.
func TestSearchHandler_HappyPath(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus,
		skills.Skill{Name: "allowed", Trigger: "trig-allowed", RequiredTools: []string{"http_fetch"}, Origin: skills.OriginPack, Scope: skills.ScopeProject, Steps: []string{"s1"}},
		skills.Skill{Name: "blocked", Trigger: "trig-blocked", RequiredTools: []string{"fs_write"}, Origin: skills.OriginPack, Scope: skills.ScopeProject, Steps: []string{"s1"}},
	)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx := ctxWithIdentity(t)
	d, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)

	args := mustMarshal(t, skilltools.SearchArgs{
		Query:      "trig",
		Limit:      10,
		Capability: skilltools.CapabilityContext{AllowedTools: []string{"http_fetch"}},
	})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := unmarshalSearchResult(t, res)
	if len(out.Skills) != 1 {
		t.Fatalf("len(Skills)=%d, want 1 — capability filter must exclude fs_write skill", len(out.Skills))
	}
	if out.Skills[0].Skill.Name != "allowed" {
		t.Fatalf("got %q, want allowed", out.Skills[0].Skill.Name)
	}
}

// TestSearchHandler_MissingIdentity asserts the identity-mandatory
// contract — missing identity returns wrapped ErrIdentityRequired.
func TestSearchHandler_MissingIdentity(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus, skills.Skill{Name: "x", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginPack, Scope: skills.ScopeProject})
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d, _ := catalog.Resolve(skilltools.ToolNameSkillSearch)
	args := mustMarshal(t, skilltools.SearchArgs{Query: "x", Capability: skilltools.CapabilityContext{}})
	// NO identity in ctx — handler must reject.
	_, err := d.Invoke(context.Background(), args)
	if err == nil {
		t.Fatalf("err=nil, want wrapped ErrIdentityRequired")
	}
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("err=%v, want errors.Is(err, ErrIdentityRequired)", err)
	}
}

// TestGetHandler_MissingNameSkipped — Get returns a partial response
// when one of the requested names is missing, so a stale planner
// cache does not hard-fail.
func TestGetHandler_MissingNameSkipped(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus,
		skills.Skill{Name: "exists", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginPack, Scope: skills.ScopeProject},
	)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d, _ := catalog.Resolve(skilltools.ToolNameSkillGet)
	ctx := ctxWithIdentity(t)
	args := mustMarshal(t, skilltools.GetArgs{
		Names:      []string{"exists", "missing"},
		MaxTokens:  1024,
		Capability: skilltools.CapabilityContext{},
	})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := unmarshalGetResult(t, res)
	if len(out.Skills) != 1 || out.Skills[0].Name != "exists" {
		t.Fatalf("got %v, want [exists]", outNames(out.Skills))
	}
}

func TestGetHandler_BudgetOverflow(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus,
		skills.Skill{
			Name:    "huge",
			Trigger: strings.Repeat("y", 1000),
			Steps:   []string{strings.Repeat("a", 2000), strings.Repeat("b", 2000), strings.Repeat("c", 2000), strings.Repeat("d", 2000)},
			Origin:  skills.OriginPack,
			Scope:   skills.ScopeProject,
		},
	)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d, _ := catalog.Resolve(skilltools.ToolNameSkillGet)
	ctx := ctxWithIdentity(t)
	args := mustMarshal(t, skilltools.GetArgs{
		Names:      []string{"huge"},
		MaxTokens:  10,
		Capability: skilltools.CapabilityContext{},
	})
	_, err := d.Invoke(ctx, args)
	if err == nil {
		t.Fatalf("err=nil, want wrapped ErrSkillTooLarge")
	}
	if !errors.Is(err, skilltools.ErrSkillTooLarge) {
		t.Fatalf("err=%v, want errors.Is(err, ErrSkillTooLarge)", err)
	}
}

func TestListHandler_HappyPath(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newFakeStore(bus,
		skills.Skill{Name: "a", Trigger: "t1", Steps: []string{"s"}, Origin: skills.OriginPack, Scope: skills.ScopeProject},
		skills.Skill{Name: "b", Trigger: "t2", Steps: []string{"s"}, Origin: skills.OriginPack, Scope: skills.ScopeProject, RequiredTools: []string{"fs_write"}},
	)
	catalog := tcat.NewCatalog()
	if err := skilltools.Register(catalog, store, skilltools.Deps{Bus: bus}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d, _ := catalog.Resolve(skilltools.ToolNameSkillList)
	ctx := ctxWithIdentity(t)
	args := mustMarshal(t, skilltools.ListArgs{
		Capability: skilltools.CapabilityContext{AllowedTools: nil},
	})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := unmarshalListResult(t, res)
	if len(out.Skills) != 1 || out.Skills[0].Name != "a" {
		t.Fatalf("got %v, want [a] — b's RequiredTools=fs_write should be filtered out", outNames(out.Skills))
	}
}
