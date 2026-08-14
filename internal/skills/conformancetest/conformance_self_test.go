package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

	// Complete installed-package contract. A nil field is a benign lie:
	// the method returns a zero value with a nil error, which the suite
	// MUST catch (missing/lying method). A case sets the field to make
	// the specific lie it proves.
	getInstalledPackage func(context.Context, identity.Quadruple, string, string) (skills.InstalledPackage, error)
	resolveSupport      func(context.Context, identity.Quadruple, string, string, skills.PackageURI) (skills.SupportFile, error)
	putInstalledPackage func(context.Context, identity.Quadruple, string, skills.InstalledPackage, skills.InstalledPackageCondition, bool) (skills.InstalledPackageReceipt, error)
	deleteInstalledPkg  func(context.Context, identity.Quadruple, string, string, skills.InstalledPackageReceipt) (bool, error)
	restoreInstalledPkg func(context.Context, identity.Quadruple, string, string, skills.InstalledPackageReceipt, skills.InstalledPackage) (bool, error)
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

func (s *adversarialSkillStore) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	if s.getInstalledPackage != nil {
		return s.getInstalledPackage(ctx, id, agentID, name)
	}
	return skills.InstalledPackage{}, nil // benign lie: absent rows read as zero units
}

func (s *adversarialSkillStore) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	if s.resolveSupport != nil {
		return s.resolveSupport(ctx, id, agentID, name, uri)
	}
	return skills.SupportFile{}, nil // benign lie: every URI resolves to a zero file
}

func (s *adversarialSkillStore) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	if s.putInstalledPackage != nil {
		return s.putInstalledPackage(ctx, id, agentID, pkg, cond, replace)
	}
	return skills.InstalledPackageReceipt{}, nil // benign lie: every put succeeds with a zero receipt
}

func (s *adversarialSkillStore) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	if s.deleteInstalledPkg != nil {
		return s.deleteInstalledPkg(ctx, id, agentID, name, receipt)
	}
	return false, nil // benign lie: nothing is ever deleted
}

func (s *adversarialSkillStore) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	if s.restoreInstalledPkg != nil {
		return s.restoreInstalledPkg(ctx, id, agentID, name, receipt, prior)
	}
	return false, nil // benign lie: nothing is ever restored
}

// lyingPut returns a PutInstalledPackage closure that looks successful —
// it answers a correctly-shaped receipt from the incoming unit without
// persisting anything. It is the "write accepted, nothing stored" lie
// the suite must catch on the next read.
func lyingPut() func(context.Context, identity.Quadruple, string, skills.InstalledPackage, skills.InstalledPackageCondition, bool) (skills.InstalledPackageReceipt, error) {
	return func(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
		return skills.InstalledPackageReceipt{
			TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
			WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		}, nil
	}
}

// lyingGet returns a GetInstalledPackage closure that always returns
// the supplied unit with a nil error.
func lyingGet(unit skills.InstalledPackage) func(context.Context, identity.Quadruple, string, string) (skills.InstalledPackage, error) {
	return func(context.Context, identity.Quadruple, string, string) (skills.InstalledPackage, error) {
		return unit, nil
	}
}

// resolveTruthfulFor returns a ResolveSupport closure that resolves
// exactly the supplied unit's manifest entries (and refuses everything
// else) — the minimal truthful read the resolve scenario needs to reach
// its later assertions.
func resolveTruthfulFor(unit skills.InstalledPackage) func(context.Context, identity.Quadruple, string, string, skills.PackageURI) (skills.SupportFile, error) {
	return func(_ context.Context, _ identity.Quadruple, _ string, _ string, uri skills.PackageURI) (skills.SupportFile, error) {
		if uri.Hash != unit.PackageHash {
			return skills.SupportFile{}, skills.ErrSupportNotFound
		}
		for _, f := range unit.Package.Supports {
			if f.Path == uri.Path {
				return f, nil
			}
		}
		return skills.SupportFile{}, skills.ErrSupportNotFound
	}
}

// lyingPackageStore is a stateful adversarial SkillStore used to drive
// the multi-step replace/compensation/erasure scenarios to a SPECIFIC
// lie: it records package puts (a correct receipt, a correct read-back)
// but each contract check is a truthfulness switch that defaults to
// OFF (the lie). The legacy surface delegates to an embedded reference
// store so the scenario's legacy assertions stay truthful.
//
//   - enforceReplace  — refuse implicit replacement (ErrInstalledPackageReplaceRequired)
//   - enforceOrigin   — refuse generated-over-pack (ErrPackOverwriteRefused)
//   - enforceCondition— refuse absent/hash/version condition mismatches
//   - exactReplay     — treat an exact same-hash put as a no-op success
//   - deleteLies      — a stale receipt deletes the CURRENT winner anyway
//   - restoreLies     — a stale receipt replaces the CURRENT winner anyway
//   - deleteNoop      — deletion always reports (false, nil)
type lyingPackageStore struct {
	*referenceStore
	mu               sync.Mutex
	pkgs             map[string]skills.InstalledPackage
	enforceReplace   bool
	enforceOrigin    bool
	enforceCondition bool
	exactReplay      bool
	deleteLies       bool
	restoreLies      bool
	deleteNoop       bool
}

func newLyingPackageStore() *lyingPackageStore {
	return &lyingPackageStore{referenceStore: newReferenceStore(), pkgs: map[string]skills.InstalledPackage{}}
}

func (s *lyingPackageStore) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.pkgs[refPkgKey(id, agentID, name)]
	if !ok {
		return skills.InstalledPackage{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageNotFound, name)
	}
	return refDeepCopyUnit(u), nil
}

func (s *lyingPackageStore) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.pkgs[refPkgKey(id, agentID, name)]
	if !ok {
		return skills.SupportFile{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageNotFound, name)
	}
	if uri.Hash != u.PackageHash {
		return skills.SupportFile{}, skills.ErrSupportNotFound
	}
	for _, f := range u.Package.Supports {
		if f.Path == uri.Path {
			return f, nil
		}
	}
	return skills.SupportFile{}, skills.ErrSupportNotFound
}

func (s *lyingPackageStore) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := refPkgKey(id, agentID, pkg.Package.Name)
	prior, present := s.pkgs[key]
	priorHash, priorVersion := "", ""
	if present {
		priorHash, priorVersion = prior.PackageHash, prior.Package.Version
	}
	if present && s.exactReplay && prior.PackageHash == pkg.PackageHash {
		return skills.InstalledPackageReceipt{
			TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
			WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		}, nil
	}
	switch {
	case present && s.enforceReplace && !replace:
		return skills.InstalledPackageReceipt{}, skills.ErrInstalledPackageReplaceRequired
	case present && s.enforceOrigin && prior.Skill.Origin == skills.OriginPack && pkg.Skill.Origin != skills.OriginPack:
		return skills.InstalledPackageReceipt{}, skills.ErrPackOverwriteRefused
	case s.enforceCondition && cond.ExpectedAbsent && present:
		return skills.InstalledPackageReceipt{}, skills.ErrInstalledPackageExists
	case s.enforceCondition && !cond.ExpectedAbsent && (!present || prior.PackageHash != cond.ExpectedHash):
		return skills.InstalledPackageReceipt{}, skills.ErrInstalledPackageConditionFailed
	case s.enforceCondition && !cond.ExpectedAbsent && cond.ExpectedVersion != "" && present && prior.Package.Version != cond.ExpectedVersion:
		return skills.InstalledPackageReceipt{}, skills.ErrInstalledPackageConditionFailed
	}
	s.pkgs[key] = refDeepCopyUnit(pkg)
	return skills.InstalledPackageReceipt{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
		WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		PriorHash: priorHash, PriorVersion: priorVersion,
	}, nil
}

func (s *lyingPackageStore) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteNoop {
		return false, nil
	}
	key := refPkgKey(id, agentID, name)
	winner, present := s.pkgs[key]
	if !present {
		return false, nil
	}
	if winner.PackageHash == receipt.WrittenHash {
		delete(s.pkgs, key)
		return true, nil
	}
	if s.deleteLies {
		delete(s.pkgs, key) // deletes ANOTHER proposal's winner — the lie
		return true, nil
	}
	return false, nil
}

func (s *lyingPackageStore) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := refPkgKey(id, agentID, name)
	winner, present := s.pkgs[key]
	if present && winner.PackageHash == receipt.WrittenHash {
		s.pkgs[key] = refDeepCopyUnit(prior)
		return true, nil
	}
	if s.restoreLies {
		s.pkgs[key] = refDeepCopyUnit(prior) // replaces ANOTHER proposal's winner — the lie
		return true, nil
	}
	return false, nil
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

		// ---- Complete installed-package contract: missing/lying
		// methods must fail the suite (HA-61 / D-422). ----

		{name: "installed roundtrip put error", want: "PutInstalledPackage(create", check: func(r failureReporter) {
			testInstalledMinimalRoundtrip(r, harnessWith(&adversarialSkillStore{putInstalledPackage: func(context.Context, identity.Quadruple, string, skills.InstalledPackage, skills.InstalledPackageCondition, bool) (skills.InstalledPackageReceipt, error) {
				return skills.InstalledPackageReceipt{}, errAdversarialStore
			}}))
		}},
		{name: "installed roundtrip get error", want: "GetInstalledPackage(", check: func(r failureReporter) {
			testInstalledMinimalRoundtrip(r, harnessWith(&adversarialSkillStore{
				putInstalledPackage: lyingPut(),
				getInstalledPackage: func(context.Context, identity.Quadruple, string, string) (skills.InstalledPackage, error) {
					return skills.InstalledPackage{}, errAdversarialStore
				},
			}))
		}},
		{name: "installed roundtrip missing row accepted", want: "PackageHash got", check: func(r failureReporter) {
			testInstalledMinimalRoundtrip(r, harnessWith(&adversarialSkillStore{putInstalledPackage: lyingPut()}))
		}},
		{name: "installed roundtrip body mismatch", want: "PackageHash got", check: func(r failureReporter) {
			other := installedFixtureUnit(r, "minimal", "agent-minimal", skills.OriginGenerated, "1.0.0", 1)
			other.PackageHash = "v1:1111111111111111111111111111111111111111111111111111111111111111"
			testInstalledMinimalRoundtrip(r, harnessWith(&adversarialSkillStore{
				putInstalledPackage: lyingPut(),
				getInstalledPackage: lyingGet(other),
			}))
		}},
		{name: "installed resolve foreign hash accepted", want: "metadata = ", check: func(r failureReporter) {
			testInstalledResolveChecks(r, harnessWith(&adversarialSkillStore{
				putInstalledPackage: lyingPut(),
				resolveSupport: func(context.Context, identity.Quadruple, string, string, skills.PackageURI) (skills.SupportFile, error) {
					return skills.SupportFile{}, nil
				},
			}))
		}},
		{name: "installed resolve wrong bytes", want: "bytes differ", check: func(r failureReporter) {
			unit := installedFixtureUnit(r, "resolve", "agent-resolve", skills.OriginGenerated, "1.0.0", 3)
			bad := unit.Package.Supports[0]
			bad.Data = []byte(`{"file": 99, "name": "wrong"}`)
			testInstalledResolveChecks(r, harnessWith(&adversarialSkillStore{
				putInstalledPackage: lyingPut(),
				resolveSupport: func(context.Context, identity.Quadruple, string, string, skills.PackageURI) (skills.SupportFile, error) {
					return bad, nil
				},
			}))
		}},
		{name: "installed resolve missing package accepted", want: "want ErrInstalledPackageNotFound", check: func(r failureReporter) {
			unit := installedFixtureUnit(r, "resolve", "agent-resolve", skills.OriginGenerated, "1.0.0", 3)
			testInstalledResolveChecks(r, harnessWith(&adversarialSkillStore{
				putInstalledPackage: lyingPut(),
				resolveSupport:      resolveTruthfulFor(unit),
			}))
		}},
		{name: "installed implicit replace accepted", want: "want ErrInstalledPackageReplaceRequired", check: func(r failureReporter) {
			testInstalledReplaceOriginMatrix(r, harnessWith(newLyingPackageStore()))
		}},
		{name: "installed generated over pack accepted", want: "want ErrPackOverwriteRefused", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.enforceReplace = true
			testInstalledReplaceOriginMatrix(r, harnessWith(store))
		}},
		{name: "installed condition ignored", want: "want ErrInstalledPackageConditionFailed", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.enforceReplace = true
			store.enforceOrigin = true
			testInstalledReplaceOriginMatrix(r, harnessWith(store))
		}},
		{name: "installed nil-data manifest accepted", want: "want ErrInstalledPackageInvalid", check: func(r failureReporter) {
			testInstalledDanglingImpossible(r, harnessWith(newLyingPackageStore()))
		}},
		{name: "installed stale delete deletes another winner", want: "stale receipt deleted", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.enforceReplace = true
			store.enforceOrigin = true
			store.enforceCondition = true
			store.exactReplay = true
			store.deleteLies = true
			testInstalledConditionalCompensation(r, harnessWith(store))
		}},
		{name: "installed stale restore replaces another winner", want: "stale restore", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.enforceReplace = true
			store.enforceOrigin = true
			store.enforceCondition = true
			store.exactReplay = true
			store.restoreLies = true
			testInstalledConditionalCompensation(r, harnessWith(store))
		}},
		{name: "installed wrong prior accepted", want: "want ErrInstalledPackageConditionFailed", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.enforceReplace = true
			store.enforceOrigin = true
			store.enforceCondition = true
			store.exactReplay = true
			testInstalledConditionalCompensation(r, harnessWith(store))
		}},
		{name: "installed replay not idempotent", want: "exact create replay must succeed", check: func(r failureReporter) {
			putCalls := 0
			testInstalledResponseLossReplay(r, harnessWith(&adversarialSkillStore{putInstalledPackage: func(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
				putCalls++
				if putCalls == 2 {
					return skills.InstalledPackageReceipt{}, errAdversarialStore
				}
				return lyingPut()(ctx, id, agentID, pkg, cond, replace)
			}}))
		}},
		{name: "installed identity accepted", want: "expected ErrIdentityRequired", check: func(r failureReporter) {
			testInstalledIdentityAgentIsolation(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "installed closed sentinel ignored", want: "want ErrStoreClosed", check: func(r failureReporter) {
			testInstalledClosedSentinels(r, harnessWith(&adversarialSkillStore{}))
		}},
		{name: "installed delete leaves package", want: "DeleteInstalledPackage: deleted=", check: func(r failureReporter) {
			store := newLyingPackageStore()
			store.deleteNoop = true
			testInstalledErasure(r, harnessWith(store))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectConformanceFailure(t, tc.want, tc.check)
		})
	}
}

// TestReferenceStore_PassesInstalledPackageSuite proves the installed-
// package suite itself is sound: a store that honors the contract (the
// test-only reference implementation) passes every scenario, including
// the N>=100 mixed concurrent-reuse run under `-race`.
func TestReferenceStore_PassesInstalledPackageSuite(t *testing.T) {
	RunInstalledPackageSuite(t, func(t *testing.T) Harness {
		return Harness{Store: newReferenceStore(), Cleanup: func() {}, SnapshotFullTextPath: skills.PathFTS5}
	})
}

// TestReferenceStore_PassesFullSuite runs the ENTIRE shared suite
// (legacy + installed-package) against the reference implementation,
// proving the reference store is a faithful model of the contract the
// real drivers must implement.
func TestReferenceStore_PassesFullSuite(t *testing.T) {
	Run(t, func(t *testing.T) Harness {
		return Harness{Store: newReferenceStore(), Cleanup: func() {}, SnapshotFullTextPath: skills.PathFTS5}
	})
}
