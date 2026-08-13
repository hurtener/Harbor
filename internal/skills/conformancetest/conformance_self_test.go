package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

var errAdversarialStore = errors.New("adversarial store failure")

type capturedConformanceFailure string

type captureFailureReporter struct{}

func (*captureFailureReporter) Helper() {}

func (*captureFailureReporter) Fatal(args ...any) {
	panic(capturedConformanceFailure(fmt.Sprint(args...)))
}

func (*captureFailureReporter) Fatalf(format string, args ...any) {
	panic(capturedConformanceFailure(fmt.Sprintf(format, args...)))
}

type adversarialSkillStore struct {
	upsert             func(context.Context, identity.Quadruple, skills.Skill) error
	get                func(context.Context, identity.Quadruple, string) (skills.Skill, error)
	getScope           func(context.Context, identity.Quadruple, string, skills.Scope) (skills.Skill, error)
	list               func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error)
	search             func(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error)
	searchSnapshot     func(context.Context, identity.Quadruple, string, []skills.Skill, int) ([]skills.RankedSkill, error)
	delete             func(context.Context, identity.Quadruple, string, skills.Scope) error
	deleteSessionScope func(context.Context, identity.Quadruple) error
	close              func(context.Context) error
}

func (s *adversarialSkillStore) GetScopeAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) (skills.Skill, error) {
	return s.GetScope(ctx, id, name, scope)
}

func (s *adversarialSkillStore) SearchAgent(ctx context.Context, id identity.Quadruple, agentID, query string, limit int) ([]skills.RankedSkill, error) {
	return s.Search(ctx, id, query, limit)
}

func (s *adversarialSkillStore) DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) error {
	return s.Delete(ctx, id, name, scope)
}

func (s *adversarialSkillStore) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	if s.upsert != nil {
		return s.upsert(ctx, id, skill)
	}
	return nil
}

func (s *adversarialSkillStore) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if s.get != nil {
		return s.get(ctx, id, name)
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (s *adversarialSkillStore) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if s.getScope != nil {
		return s.getScope(ctx, id, name, scope)
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (s *adversarialSkillStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if s.list != nil {
		return s.list(ctx, id, filter)
	}
	return nil, nil
}

func (s *adversarialSkillStore) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if s.search != nil {
		return s.search(ctx, id, query, limit)
	}
	return nil, nil
}

func (s *adversarialSkillStore) SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if s.searchSnapshot != nil {
		return s.searchSnapshot(ctx, id, query, candidates, limit)
	}
	return nil, nil
}

func (s *adversarialSkillStore) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	if s.delete != nil {
		return s.delete(ctx, id, name, scope)
	}
	return nil
}

func (s *adversarialSkillStore) DeleteSessionScope(ctx context.Context, id identity.Quadruple) error {
	if s.deleteSessionScope != nil {
		return s.deleteSessionScope(ctx, id)
	}
	return nil
}

func (s *adversarialSkillStore) Close(ctx context.Context) error {
	if s.close != nil {
		return s.close(ctx)
	}
	return nil
}

func expectConformanceFailure(t *testing.T, want string, check func(failureReporter)) {
	t.Helper()
	defer func() {
		recovered := recover()
		failure, ok := recovered.(capturedConformanceFailure)
		if !ok {
			t.Fatalf("conformance check panic = %#v, want captured failure containing %q", recovered, want)
		}
		if !strings.Contains(string(failure), want) {
			t.Fatalf("conformance failure = %q, want substring %q", failure, want)
		}
	}()
	check(&captureFailureReporter{})
	t.Fatalf("conformance check accepted adversarial driver; want failure containing %q", want)
}

func harnessWith(store skills.SkillStore) Harness {
	return Harness{Store: store, Cleanup: func() {}, SnapshotFullTextPath: skills.PathFTS5}
}

func TestConformanceHarness_RejectsAdversarialDrivers(t *testing.T) {
	validAlpha := newSkill("alpha")
	ordered := []skills.Skill{newSkill("echo"), newSkill("alpha"), newSkill("delta"), newSkill("bravo"), newSkill("charlie")}

	cases := []struct {
		name  string
		want  string
		check func(failureReporter)
	}{
		{name: "roundtrip upsert error", want: "Upsert", check: func(r failureReporter) {
			testUpsertGetRoundTrip(r, harnessWith(&adversarialSkillStore{upsert: func(context.Context, identity.Quadruple, skills.Skill) error { return errAdversarialStore }}))
		}},
		{name: "roundtrip get error", want: "Get", check: func(r failureReporter) {
			testUpsertGetRoundTrip(r, harnessWith(&adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) {
				return skills.Skill{}, errAdversarialStore
			}}))
		}},
		{name: "roundtrip body mismatch", want: "round-trip mismatch", check: func(r failureReporter) {
			testUpsertGetRoundTrip(r, harnessWith(&adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) {
				return skills.Skill{Name: "wrong"}, nil
			}}))
		}},
		{name: "roundtrip steps mismatch", want: "Steps not preserved", check: func(r failureReporter) {
			bad := validAlpha
			bad.Steps = []string{"wrong"}
			testUpsertGetRoundTrip(r, harnessWith(&adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) { return bad, nil }}))
		}},
		{name: "ordering upsert error", want: "Upsert", check: func(r failureReporter) {
			testOrdering(r, harnessWith(&adversarialSkillStore{upsert: func(context.Context, identity.Quadruple, skills.Skill) error { return errAdversarialStore }}))
		}},
		{name: "ordering list error", want: "List", check: func(r failureReporter) {
			testOrdering(r, harnessWith(&adversarialSkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
				return nil, errAdversarialStore
			}}))
		}},
		{name: "ordering wrong count", want: "want 5", check: func(r failureReporter) {
			testOrdering(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "ordering missing member", want: "omitted", check: func(r failureReporter) {
			rows := append([]skills.Skill(nil), ordered...)
			rows[4] = newSkill("echo")
			testOrdering(r, harnessWith(&adversarialSkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) { return rows, nil }}))
		}},
		{name: "identity accepted", want: "expected ErrIdentityRequired", check: func(r failureReporter) {
			testIdentityRejection(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "snapshot error", want: "SearchSnapshot", check: func(r failureReporter) {
			testSnapshotSearch(r, harnessWith(&adversarialSkillStore{searchSnapshot: func(context.Context, identity.Quadruple, string, []skills.Skill, int) ([]skills.RankedSkill, error) {
				return nil, errAdversarialStore
			}}))
		}},
		{name: "snapshot wrong result", want: "want only configured-driver", check: func(r failureReporter) {
			testSnapshotSearch(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "get missing row accepted", want: "Get: expected", check: func(r failureReporter) {
			testNotFound(r, harnessWith(&adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) { return skills.Skill{}, nil }}))
		}},
		{name: "delete missing row accepted", want: "Delete: expected", check: func(r failureReporter) {
			testNotFound(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "delete seed error", want: "Upsert", check: func(r failureReporter) {
			testDelete(r, harnessWith(&adversarialSkillStore{upsert: func(context.Context, identity.Quadruple, skills.Skill) error { return errAdversarialStore }}))
		}},
		{name: "delete operation error", want: "Delete", check: func(r failureReporter) {
			testDelete(r, harnessWith(&adversarialSkillStore{delete: func(context.Context, identity.Quadruple, string, skills.Scope) error { return errAdversarialStore }}))
		}},
		{name: "delete leaves row", want: "Get after Delete", check: func(r failureReporter) {
			testDelete(r, harnessWith(&adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) {
				return newSkill("doomed"), nil
			}}))
		}},
		{name: "restart seed error", want: "Upsert", check: func(r failureReporter) {
			testRestartSurvival(r, harnessWith(&adversarialSkillStore{upsert: func(context.Context, identity.Quadruple, skills.Skill) error { return errAdversarialStore }}))
		}},
		{name: "restart close error", want: "Close", check: func(r failureReporter) {
			h := harnessWith(&adversarialSkillStore{close: func(context.Context) error { return errAdversarialStore }})
			h.ReopenedStore = func() (skills.SkillStore, error) { return &adversarialSkillStore{}, nil }
			testRestartSurvival(r, h)
		}},
		{name: "restart reopen error", want: "ReopenedStore", check: func(r failureReporter) {
			h := harnessWith(&adversarialSkillStore{})
			h.ReopenedStore = func() (skills.SkillStore, error) { return nil, errAdversarialStore }
			testRestartSurvival(r, h)
		}},
		{name: "restart read error", want: "Get after reopen", check: func(r failureReporter) {
			h := harnessWith(&adversarialSkillStore{})
			h.ReopenedStore = func() (skills.SkillStore, error) {
				return &adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) {
					return skills.Skill{}, errAdversarialStore
				}}, nil
			}
			testRestartSurvival(r, h)
		}},
		{name: "restart body mismatch", want: "restart mismatch", check: func(r failureReporter) {
			h := harnessWith(&adversarialSkillStore{})
			h.ReopenedStore = func() (skills.SkillStore, error) {
				return &adversarialSkillStore{get: func(context.Context, identity.Quadruple, string) (skills.Skill, error) {
					return skills.Skill{Name: "wrong"}, nil
				}}, nil
			}
			testRestartSurvival(r, h)
		}},
		{name: "session sweep close error", want: "Close", check: func(r failureReporter) {
			testDeleteSessionScopeAfterClose(r, harnessWith(&adversarialSkillStore{close: func(context.Context) error { return errAdversarialStore }}))
		}},
		{name: "closed session sweep accepted", want: "ErrStoreClosed", check: func(r failureReporter) {
			testDeleteSessionScopeAfterClose(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "get scope close error", want: "Close", check: func(r failureReporter) {
			testGetScopeAfterClose(r, harnessWith(&adversarialSkillStore{close: func(context.Context) error { return errAdversarialStore }}))
		}},
		{name: "closed get scope accepted", want: "ErrStoreClosed", check: func(r failureReporter) {
			testGetScopeAfterClose(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "list error", want: "List", check: func(r failureReporter) {
			store := &adversarialSkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
				return nil, errAdversarialStore
			}}
			assertListContains(r, context.Background(), harnessWith(store), fixtureID, skills.ListFilter{}, "wanted", true, "adversarial")
		}},
		{name: "list membership mismatch", want: "List contains", check: func(r failureReporter) {
			store := &adversarialSkillStore{list: func(context.Context, identity.Quadruple, skills.ListFilter) ([]skills.Skill, error) {
				return []skills.Skill{newSkill("other")}, nil
			}}
			assertListContains(r, context.Background(), harnessWith(store), fixtureID, skills.ListFilter{}, "wanted", true, "adversarial")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectConformanceFailure(t, tc.want, tc.check)
		})
	}
}
