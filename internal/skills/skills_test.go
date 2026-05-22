package skills_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// driverSeq makes every test-registered driver name unique across the
// whole test binary — including under `go test -count=N`, which
// re-runs test functions in the SAME process. skills.Register is
// write-once-and-panics-on-duplicate, so a name derived from t.Name()
// would panic on the second -count iteration. An atomic counter never
// repeats. (This is the flake class feedback_harbor_agent_dispatch.md
// + AGENTS.md §17.6 call out — process-wide registration without a
// cleanup path.)
var driverSeq atomic.Int64

func uniqueDriverName(t *testing.T) string {
	t.Helper()
	// itoa is shared with directory_test.go (same _test package).
	return "test-driver-" + strings.ReplaceAll(t.Name(), "/", "_") + "-" +
		itoa(int(driverSeq.Add(1)))
}

// stubBus is a no-op events.EventBus — skills.Open only checks the bus
// is non-nil (validateDeps); the registry tests never publish.
type stubBus struct{}

func (stubBus) Publish(context.Context, events.Event) error { return nil }
func (stubBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}
func (stubBus) Close(context.Context) error { return nil }

// stubStore is a minimal SkillStore a test factory can hand back.
type stubStore struct{}

func (stubStore) Upsert(context.Context, identity.Quadruple, skills.Skill) error { return nil }
func (stubStore) Get(context.Context, identity.Quadruple, string) (skills.Skill, error) {
	return skills.Skill{}, nil
}
func (stubStore) List(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
	return nil, nil
}
func (stubStore) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, nil
}
func (stubStore) Delete(context.Context, identity.Quadruple, string) error { return nil }
func (stubStore) Close(context.Context) error                              { return nil }

func validSkill() skills.Skill {
	return skills.Skill{
		Name:    "demo",
		Trigger: "when the user asks for a demo",
		Steps:   []string{"step one"},
		Origin:  skills.OriginGenerated,
		Scope:   skills.ScopeSession,
	}
}

// --- Skill.Validate ---------------------------------------------------------

func TestSkill_Validate_AcceptsWellFormed(t *testing.T) {
	t.Parallel()
	if err := validSkill().Validate(); err != nil {
		t.Fatalf("Validate(valid skill) = %v, want nil", err)
	}
}

func TestSkill_Validate_RejectsEachMissingMandatoryField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*skills.Skill)
		want   string
	}{
		{"empty name", func(s *skills.Skill) { s.Name = "  " }, "Name empty"},
		{"empty trigger", func(s *skills.Skill) { s.Trigger = "" }, "Trigger empty"},
		{"no steps", func(s *skills.Skill) { s.Steps = nil }, "Steps empty"},
		{"bad origin", func(s *skills.Skill) { s.Origin = "made-up" }, "Origin="},
		{"bad scope", func(s *skills.Skill) { s.Scope = "made-up" }, "Scope="},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := validSkill()
			tc.mutate(&s)
			err := s.Validate()
			if !errors.Is(err, skills.ErrInvalidSkill) {
				t.Fatalf("Validate() = %v, want errors.Is ErrInvalidSkill", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSkill_Validate_AcceptsEveryOriginAndScope(t *testing.T) {
	t.Parallel()
	for _, o := range []skills.Origin{skills.OriginPack, skills.OriginGenerated} {
		for _, sc := range []skills.Scope{
			skills.ScopeSession, skills.ScopeProject, skills.ScopeTenant, skills.ScopeGlobal,
		} {
			s := validSkill()
			s.Origin, s.Scope = o, sc
			if err := s.Validate(); err != nil {
				t.Errorf("Validate(Origin=%q,Scope=%q) = %v, want nil", o, sc, err)
			}
		}
	}
}

// --- ValidateIdentity -------------------------------------------------------

func TestValidateIdentity(t *testing.T) {
	t.Parallel()
	full := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    "r",
	}
	if err := skills.ValidateIdentity(full); err != nil {
		t.Errorf("ValidateIdentity(full) = %v, want nil", err)
	}
	// Empty RunID is allowed — skills are session-scoped at storage.
	noRun := full
	noRun.RunID = ""
	if err := skills.ValidateIdentity(noRun); err != nil {
		t.Errorf("ValidateIdentity(no run id) = %v, want nil (RunID is optional)", err)
	}
	for _, tc := range []struct {
		name string
		q    identity.Quadruple
	}{
		{"missing tenant", identity.Quadruple{Identity: identity.Identity{UserID: "u", SessionID: "s"}}},
		{"missing user", identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}}},
		{"missing session", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}},
		{"all empty", identity.Quadruple{}},
	} {
		if err := skills.ValidateIdentity(tc.q); !errors.Is(err, skills.ErrIdentityRequired) {
			t.Errorf("ValidateIdentity(%s) = %v, want errors.Is ErrIdentityRequired", tc.name, err)
		}
	}
}

// --- Register / Open / OpenDriver -------------------------------------------

func TestRegister_PanicsOnEmptyName(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Register(\"\", ...) did not panic")
		}
	}()
	skills.Register("", func(skills.ConfigSnapshot, skills.Deps) (skills.SkillStore, error) {
		return stubStore{}, nil
	})
}

func TestRegister_PanicsOnNilFactory(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Register(name, nil) did not panic")
		}
	}()
	skills.Register(uniqueDriverName(t), nil)
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	t.Parallel()
	name := uniqueDriverName(t)
	f := func(skills.ConfigSnapshot, skills.Deps) (skills.SkillStore, error) {
		return stubStore{}, nil
	}
	skills.Register(name, f)
	defer func() {
		if recover() == nil {
			t.Fatalf("Register(%q) twice did not panic", name)
		}
	}()
	skills.Register(name, f)
}

func TestOpen_DispatchesToRegisteredDriver(t *testing.T) {
	t.Parallel()
	name := uniqueDriverName(t)
	var gotCfg skills.ConfigSnapshot
	skills.Register(name, func(cfg skills.ConfigSnapshot, _ skills.Deps) (skills.SkillStore, error) {
		gotCfg = cfg
		return stubStore{}, nil
	})

	store, err := skills.Open(context.Background(),
		skills.ConfigSnapshot{Driver: name, DSN: ":memory:"},
		skills.Deps{Bus: stubBus{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if store == nil {
		t.Fatal("Open returned nil store")
	}
	if gotCfg.DSN != ":memory:" {
		t.Errorf("factory saw cfg.DSN = %q, want :memory:", gotCfg.DSN)
	}
}

func TestOpenDriver_DispatchesByExplicitName(t *testing.T) {
	t.Parallel()
	name := uniqueDriverName(t)
	skills.Register(name, func(skills.ConfigSnapshot, skills.Deps) (skills.SkillStore, error) {
		return stubStore{}, nil
	})
	store, err := skills.OpenDriver(name, skills.ConfigSnapshot{}, skills.Deps{Bus: stubBus{}})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	if store == nil {
		t.Fatal("OpenDriver returned nil store")
	}
}

func TestOpen_MissingBusFailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := skills.Open(context.Background(), skills.ConfigSnapshot{}, skills.Deps{Bus: nil})
	if err == nil {
		t.Fatal("Open with nil Bus returned nil err, want a wrapped failure")
	}
	if !strings.Contains(err.Error(), "Bus is required") {
		t.Errorf("Open error = %q, want it to name the missing Bus", err.Error())
	}
	// OpenDriver enforces the same contract.
	if _, err := skills.OpenDriver("anything", skills.ConfigSnapshot{}, skills.Deps{}); err == nil {
		t.Fatal("OpenDriver with nil Bus returned nil err")
	}
}

func TestOpen_UnknownDriverNamesRegisteredSet(t *testing.T) {
	t.Parallel()
	known := uniqueDriverName(t)
	skills.Register(known, func(skills.ConfigSnapshot, skills.Deps) (skills.SkillStore, error) {
		return stubStore{}, nil
	})

	_, err := skills.Open(context.Background(),
		skills.ConfigSnapshot{Driver: "definitely-not-registered"},
		skills.Deps{Bus: stubBus{}})
	if !errors.Is(err, skills.ErrUnknownDriver) {
		t.Fatalf("Open(unknown) = %v, want errors.Is ErrUnknownDriver", err)
	}
	// The error must list the registered drivers so a misconfig is
	// obvious (AGENTS.md §4.4).
	if !strings.Contains(err.Error(), known) {
		t.Errorf("ErrUnknownDriver message = %q, want it to list registered driver %q", err.Error(), known)
	}
}

func TestRegisteredDrivers_SortedAndContainsRegistered(t *testing.T) {
	t.Parallel()
	a := uniqueDriverName(t)
	b := uniqueDriverName(t)
	f := func(skills.ConfigSnapshot, skills.Deps) (skills.SkillStore, error) {
		return stubStore{}, nil
	}
	skills.Register(a, f)
	skills.Register(b, f)

	got := skills.RegisteredDrivers()
	if !sortedAscending(got) {
		t.Errorf("RegisteredDrivers() = %v, want ascending sort", got)
	}
	if !contains(got, a) || !contains(got, b) {
		t.Errorf("RegisteredDrivers() = %v, want it to contain %q and %q", got, a, b)
	}
}

func sortedAscending(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
