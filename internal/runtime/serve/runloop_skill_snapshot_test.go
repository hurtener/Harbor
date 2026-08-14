package serve

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type runSnapshotModeReader struct{}

func (runSnapshotModeReader) Mode(context.Context, string) (sessionoverlay.CutoverMode, error) {
	return sessionoverlay.CutoverStateOnly, nil
}

// runSnapshotReader is a frozen skill surface: it serves the rows the
// run captured at start and never writes. A compile-time assertion
// pins it to the complete mandatory SkillStore interface below.
type runSnapshotReader struct {
	mu   sync.RWMutex
	rows map[string]skills.Skill
}

func runSnapshotKey(id identity.Quadruple, scope skills.Scope, name string) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID + "\x00" + string(scope) + "\x00" + name
}

func (r *runSnapshotReader) put(id identity.Quadruple, skill skills.Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = make(map[string]skills.Skill)
	}
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}
	r.rows[runSnapshotKey(id, skill.Scope, skill.Name)] = skill
}

func (r *runSnapshotReader) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	for _, scope := range []skills.Scope{skills.ScopeSession, skills.ScopeUser, skills.ScopeProject, skills.ScopeTenant, skills.ScopeGlobal} {
		if skill, err := r.GetScope(ctx, id, name, scope); err == nil {
			return skill, nil
		}
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (r *runSnapshotReader) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return skills.Skill{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	skill, ok := r.rows[runSnapshotKey(id, scope, name)]
	if !ok {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return skill, nil
}

func (r *runSnapshotReader) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]skills.Skill, 0)
	for key, skill := range r.rows {
		if key != runSnapshotKey(id, skill.Scope, skill.Name) {
			continue
		}
		if filter.Scope != "" && skill.Scope != filter.Scope {
			continue
		}
		result = append(result, skill)
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

func (*runSnapshotReader) Search(context.Context, identity.Quadruple, string, int) ([]skills.RankedSkill, error) {
	return nil, errors.New("run snapshot must use its frozen candidate searcher")
}

func (r *runSnapshotReader) GetScopeAgent(ctx context.Context, id identity.Quadruple, _ string, name string, scope skills.Scope) (skills.Skill, error) {
	return r.GetScope(ctx, id, name, scope)
}

func (r *runSnapshotReader) SearchAgent(ctx context.Context, id identity.Quadruple, _ string, query string, limit int) ([]skills.RankedSkill, error) {
	return r.Search(ctx, id, query, limit)
}

func (*runSnapshotReader) SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if err := skills.ValidateIdentity(id); err != nil {
		return nil, err
	}
	return skills.SearchSnapshotRegexExact(ctx, query, candidates, limit)
}

func (*runSnapshotReader) Upsert(context.Context, identity.Quadruple, skills.Skill) error {
	return errors.New("not used")
}
func (*runSnapshotReader) Delete(context.Context, identity.Quadruple, string, skills.Scope) error {
	return errors.New("not used")
}
func (*runSnapshotReader) DeleteAgent(context.Context, identity.Quadruple, string, string, skills.Scope) error {
	return errors.New("not used")
}
func (*runSnapshotReader) DeleteSessionScope(context.Context, identity.Quadruple) error { return nil }
func (*runSnapshotReader) Close(context.Context) error                                  { return nil }

// ---- Mandatory installed-package surface (interface parity) ----
//
// The frozen snapshot never captures installed-package state, so the
// reads deterministically report an empty surface with the canonical
// typed not-found errors a real driver returns for an absent key —
// never a generic lie about the surface. The mutations are never a
// write path on this reader and follow the fake's established "not
// used" convention for the legacy write surface. The run loop and
// every snapshot test resolve only the SkillReader rungs, so these
// five methods exist purely so runSnapshotReader remains a valid
// skills.SkillStore.
func (*runSnapshotReader) GetInstalledPackage(context.Context, identity.Quadruple, string, string) (skills.InstalledPackage, error) {
	return skills.InstalledPackage{}, skills.ErrInstalledPackageNotFound
}
func (*runSnapshotReader) ResolveSupport(context.Context, identity.Quadruple, string, string, skills.PackageURI) (skills.SupportFile, error) {
	return skills.SupportFile{}, skills.ErrSupportNotFound
}
func (*runSnapshotReader) PutInstalledPackage(context.Context, identity.Quadruple, string, skills.InstalledPackage, skills.InstalledPackageCondition, bool) (skills.InstalledPackageReceipt, error) {
	return skills.InstalledPackageReceipt{}, errors.New("not used")
}
func (*runSnapshotReader) DeleteInstalledPackage(context.Context, identity.Quadruple, string, string, skills.InstalledPackageReceipt) (bool, error) {
	return false, errors.New("not used")
}
func (*runSnapshotReader) RestoreInstalledPackage(context.Context, identity.Quadruple, string, string, skills.InstalledPackageReceipt, skills.InstalledPackage) (bool, error) {
	return false, errors.New("not used")
}

// ensure the frozen snapshot reader satisfies the complete mandatory
// SkillStore surface at compile time.
var _ skills.SkillStore = (*runSnapshotReader)(nil)

func runSnapshotSkill(q identity.Quadruple, name string, scope skills.Scope) skills.Skill {
	return skills.Skill{
		Name: name, Title: name, Trigger: name, Steps: []string{"use " + name}, Origin: skills.OriginGenerated,
		Scope: scope, ScopeTenantID: q.TenantID, ScopeProjectID: q.UserID,
	}
}

func runSnapshotState(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func activateRunSnapshotAgent(t *testing.T, st state.StateStore, q identity.Quadruple, agentID string) {
	t.Helper()
	lifecycleQ, kind, err := agentcfg.LifecycleSlot(q.TenantID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(t.Context(), state.StateRecord{
		ID: state.NewEventID(), Identity: lifecycleQ, Kind: kind,
		Bytes: []byte(`{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z"}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func newRunSnapshotDriver(t *testing.T, reg agentcfg.Registry, base skills.SkillStore, st state.StateStore, bootAgent string) (*RunLoopDriver, *sessionoverlay.DurableStore) {
	t.Helper()
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &RunLoopDriver{
		agentConfig: reg, agentConfigID: bootAgent, skillStore: base,
		sessionPersonalSkills: personal, sessionSkillCutover: runSnapshotModeReader{},
	}, personal
}

func withRunSnapshot(t *testing.T, q identity.Quadruple, snapshot skills.RunSkillReaderSnapshot) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return skills.WithRunSkillReaderSnapshot(ctx, snapshot)
}

func skillNames(rows []skills.Skill) []string {
	result := make([]string, len(rows))
	for i := range rows {
		result[i] = rows[i].Name
	}
	sort.Strings(result)
	return result
}

func viewNames(rows []skills.SkillView) []string {
	result := make([]string, len(rows))
	for i := range rows {
		result[i] = rows[i].Name
	}
	sort.Strings(result)
	return result
}

func TestRunLoopDriver_SkillSnapshot_UsesEffectiveAgentAndFreezesEveryConsumer(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "snapshot-t", UserID: "snapshot-u", SessionID: "snapshot-s"}, RunID: "run-1"}
	const bootAgent, selectedAgent = "boot-agent", "selected-agent"
	activateRunSnapshotAgent(t, st, q, selectedAgent)
	for _, skill := range []skills.Skill{
		runSnapshotSkill(q, "boot-skill", skills.ScopeGlobal),
		runSnapshotSkill(q, "selected-old", skills.ScopeGlobal),
		runSnapshotSkill(q, "selected-new", skills.ScopeGlobal),
		runSnapshotSkill(q, "user-skill", skills.ScopeUser),
	} {
		base.put(q, skill)
	}
	if _, err := reg.SetRevision(t.Context(), q, bootAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"boot-skill"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"selected-old"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"user-skill"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	driver, personal := newRunSnapshotDriver(t, reg, base, st, bootAgent)
	if _, err := personal.SavePersonal(t.Context(), q, selectedAgent, runSnapshotSkill(q, "session-old", skills.ScopeSession), "", ""); err != nil {
		t.Fatal(err)
	}

	first, ok, err := driver.captureRunSkillSnapshot(t.Context(), selectedAgent, q, nil)
	if err != nil || !ok {
		t.Fatalf("capture first snapshot: ok=%t err=%v", ok, err)
	}
	if first.EffectiveAgentID() != selectedAgent {
		t.Fatalf("effective agent = %q, want %q", first.EffectiveAgentID(), selectedAgent)
	}

	if _, err := reg.SetRevision(t.Context(), q, selectedAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"selected-new"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.DeletePersonal(t.Context(), q, selectedAgent, "session-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.SavePersonal(t.Context(), q, selectedAgent, runSnapshotSkill(q, "session-new", skills.ScopeSession), "", ""); err != nil {
		t.Fatal(err)
	}

	bus := mkDriverTestBus(t, auditpatterns.New())
	directory, err := skills.NewDirectory(base, skills.Deps{Bus: bus}, skills.DirectoryConfig{MaxEntries: 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withRunSnapshot(t, q, first)
	wantFirst := []string{"selected-old", "session-old", "user-skill"}
	views, err := directory.View(ctx, skills.DirectoryCapability{})
	if err != nil {
		t.Fatal(err)
	}
	if got := viewNames(views); !equalStrings(got, wantFirst) {
		t.Fatalf("Directory snapshot = %v, want %v", got, wantFirst)
	}
	listed, err := skilltools.ListHandler(ctx, base, bus, skilltools.ListArgs{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := skillNames(listed.Skills); !equalStrings(got, wantFirst) {
		t.Fatalf("skill_list snapshot = %v, want %v", got, wantFirst)
	}
	gotten, err := skilltools.GetHandler(ctx, base, bus, skilltools.GetArgs{Names: wantFirst, MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got := skillNames(gotten.Skills); !equalStrings(got, wantFirst) {
		t.Fatalf("skill_get snapshot = %v, want %v", got, wantFirst)
	}
	searched, err := skilltools.SearchHandler(ctx, base, bus, skilltools.SearchArgs{Query: "selected-old", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := rankedNames(searched.Skills); !equalStrings(got, []string{"selected-old"}) {
		t.Fatalf("skill_search snapshot = %v, want [selected-old]", got)
	}

	nextQ := q
	nextQ.RunID = "run-2"
	next, ok, err := driver.captureRunSkillSnapshot(t.Context(), selectedAgent, nextQ, nil)
	if err != nil || !ok {
		t.Fatalf("capture next snapshot: ok=%t err=%v", ok, err)
	}
	nextReader, err := skills.ResolveSkillReader(withRunSnapshot(t, nextQ, next), nextQ, base)
	if err != nil {
		t.Fatal(err)
	}
	nextRows, err := nextReader.List(t.Context(), nextQ, skills.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	wantNext := []string{"selected-new", "session-new", "user-skill"}
	if got := skillNames(nextRows); !equalStrings(got, wantNext) {
		t.Fatalf("next run snapshot = %v, want %v", got, wantNext)
	}
}

func TestRunLoopDriver_SkillSnapshot_ConcurrentTupleIsolation(t *testing.T) {
	const runs = 128
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	driver, personal := newRunSnapshotDriver(t, reg, base, st, "boot-agent")
	for i := range runs {
		q := concurrencySnapshotQ(i)
		agentID := "agent-" + q.SessionID
		activateRunSnapshotAgent(t, st, q, agentID)
		base.put(q, runSnapshotSkill(q, "admin-"+q.SessionID, skills.ScopeGlobal))
		if _, err := reg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"admin-" + q.SessionID}}}, agentcfg.SetOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := personal.SavePersonal(t.Context(), q, agentID, runSnapshotSkill(q, "personal-"+q.SessionID, skills.ScopeSession), "", ""); err != nil {
			t.Fatal(err)
		}
	}

	errCh := make(chan error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := concurrencySnapshotQ(i)
			agentID := "agent-" + q.SessionID
			snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
			if err != nil || !ok {
				errCh <- errors.New("capture failed")
				return
			}
			reader, err := skills.ResolveSkillReader(withRunSnapshot(t, q, snapshot), q, base)
			if err != nil {
				errCh <- err
				return
			}
			rows, err := reader.List(t.Context(), q, skills.ListFilter{})
			if err != nil {
				errCh <- err
				return
			}
			want := []string{"admin-" + q.SessionID, "personal-" + q.SessionID}
			if got := skillNames(rows); !equalStrings(got, want) {
				errCh <- errors.New("cross-session snapshot")
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func concurrencySnapshotQ(i int) identity.Quadruple {
	session := "session-" + string(rune('A'+i))
	return identity.Quadruple{Identity: identity.Identity{TenantID: "isolation-t", UserID: "isolation-u", SessionID: session}, RunID: "run-" + session}
}

func rankedNames(rows []skills.RankedSkill) []string {
	result := make([]string, len(rows))
	for i := range rows {
		result[i] = rows[i].Skill.Name
	}
	sort.Strings(result)
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
