// newruncontext_test.go — parity coverage for the shared RunContext
// factory (D-265). The binding assertion is that NewRunContext COMPOSES
// the established projection helpers (FetchMemoryBlocks,
// ProjectSkillsDirectory, ResolveInputArtifacts, the bus chunk
// publisher) — the SAME helpers the cmd/harbor + devstack run-loop
// drivers call — rather than reimplementing them. Each leg builds the
// projected field through NewRunContext and through the underlying
// helper directly, then asserts the two are identical: a future refactor
// that forks NewRunContext off the shared helpers breaks these tests.

package runctx_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/skills"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// snapshotSkillReader intentionally implements only skills.SkillReader. Its
// atomic counters prove the exact same read projection serves the Directory
// and every read-only skill tool without exposing mutation capabilities.
type snapshotSkillReader struct {
	skill       skills.Skill
	getCalls    atomic.Int64
	listCalls   atomic.Int64
	searchCalls atomic.Int64
}

func (r *snapshotSkillReader) Get(_ context.Context, q identity.Quadruple, name string) (skills.Skill, error) {
	if err := identity.Validate(q.Identity); err != nil {
		return skills.Skill{}, err
	}
	r.getCalls.Add(1)
	if name != r.skill.Name {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return r.skill, nil
}

func (r *snapshotSkillReader) GetScope(ctx context.Context, q identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	got, err := r.Get(ctx, q, name)
	if err != nil {
		return skills.Skill{}, err
	}
	if got.Scope != scope {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return got, nil
}

func (r *snapshotSkillReader) List(_ context.Context, q identity.Quadruple, _ skills.ListFilter) ([]skills.Skill, error) {
	if err := identity.Validate(q.Identity); err != nil {
		return nil, err
	}
	r.listCalls.Add(1)
	return []skills.Skill{r.skill}, nil
}

func (r *snapshotSkillReader) Search(_ context.Context, q identity.Quadruple, _ string, _ int) ([]skills.RankedSkill, error) {
	if err := identity.Validate(q.Identity); err != nil {
		return nil, err
	}
	r.searchCalls.Add(1)
	return []skills.RankedSkill{{Skill: r.skill, Score: 1, Path: skills.PathExact}}, nil
}

// parityQuad is the run quadruple every parity leg uses (a complete
// identity triple + a run ID).
func parityQuad() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "acme", UserID: "alice", SessionID: "s-parity"},
		RunID:    "run-parity-1",
	}
}

// parityEchoIn / parityEchoOut are the schema for the one tool the
// parity catalog registers.
type parityEchoIn struct {
	S string `json:"s"`
}
type parityEchoOut struct {
	S string `json:"s"`
}

// parityCatalog builds a one-tool catalog (the catalog-view + skills
// capability filter need a real, non-empty visible-tool set).
func parityCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	if err := inproc.RegisterFunc(cat, "parity.echo",
		func(_ context.Context, in parityEchoIn) (parityEchoOut, error) {
			return parityEchoOut(in), nil
		}); err != nil {
		t.Fatalf("inproc.RegisterFunc: %v", err)
	}
	return cat
}

// paritySkillStore is a minimal in-memory SkillStore — the directory
// unit tests use the same shape; the localdb driver would be excessive
// for a projection-parity check. Identity is mandatory (production
// contract).
type paritySkillStore struct {
	id     identity.Identity
	skills []skills.Skill
}

func (s *paritySkillStore) Upsert(_ context.Context, id identity.Quadruple, sk skills.Skill) error {
	if err := identity.Validate(id.Identity); err != nil {
		return err
	}
	s.id = id.Identity
	s.skills = append(s.skills, sk)
	return nil
}

func (s *paritySkillStore) Get(_ context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return skills.Skill{}, err
	}
	for _, sk := range s.skills {
		if sk.Name == name {
			return sk, nil
		}
	}
	return skills.Skill{}, skills.ErrSkillNotFound
}

func (s *paritySkillStore) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	got, err := s.Get(ctx, id, name)
	if err != nil {
		return skills.Skill{}, err
	}
	if got.Scope != scope {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return got, nil
}

func (s *paritySkillStore) List(_ context.Context, id identity.Quadruple, _ skills.ListFilter) ([]skills.Skill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, err
	}
	out := make([]skills.Skill, len(s.skills))
	copy(out, s.skills)
	return out, nil
}

func (s *paritySkillStore) Search(_ context.Context, id identity.Quadruple, _ string, _ int) ([]skills.RankedSkill, error) {
	if err := identity.Validate(id.Identity); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *paritySkillStore) Delete(_ context.Context, id identity.Quadruple, _ string, _ skills.Scope) error {
	if err := identity.Validate(id.Identity); err != nil {
		return err
	}
	return nil
}

func (s *paritySkillStore) DeleteSessionScope(_ context.Context, id identity.Quadruple) error {
	if err := identity.Validate(id.Identity); err != nil {
		return err
	}
	return nil
}

func (s *paritySkillStore) Close(_ context.Context) error { return nil }

// parityDirectory builds a skills Directory over a store seeded with one
// no-required-tools skill (always capability-visible).
func parityDirectory(t *testing.T, bus events.EventBus, q identity.Quadruple) *skills.Directory {
	t.Helper()
	store := &paritySkillStore{}
	if err := store.Upsert(context.Background(), q, skills.Skill{
		Name:    "greet",
		Title:   "Greeter",
		Trigger: "say hello",
		Steps:   []string{"greet the user"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	return dir
}

// TestNewRunContext_IdentityMandatory_FailsLoud — an incomplete triple
// returns a wrapped identity error, never a half-scoped RunContext
// (CLAUDE.md §6 identity-mandatory; §13 fail-loud).
func TestNewRunContext_IdentityMandatory_FailsLoud(t *testing.T) {
	_, err := runctx.NewRunContext(context.Background(), runctx.Sources{},
		identity.Quadruple{Identity: identity.Identity{TenantID: "acme"}}, "goal")
	if err == nil {
		t.Fatal("NewRunContext with an incomplete identity must fail loud")
	}
}

func TestNewRunContext_EmptyRunID_RemainsSupportedWithoutReaderSnapshot(t *testing.T) {
	t.Parallel()
	q := identity.Quadruple{Identity: parityQuad().Identity}
	if _, err := runctx.NewRunContext(context.Background(), runctx.Sources{}, q, "goal"); err != nil {
		t.Fatalf("NewRunContext with empty RunID and no reader snapshot: %v", err)
	}
}

// TestNewRunContext_MemoryParity — RunContext.MemoryBlocks equals the
// shared FetchMemoryBlocks helper's output for the same session-scoped
// args (NewRunContext composes the helper, does not reimplement it).
func TestNewRunContext_MemoryParity(t *testing.T) {
	bus := newFetchTestBus(t)
	store := newFetchTestStore(t, bus, memory.StrategyRollingSummary, memory.RetrievalDefault)
	q := parityQuad()
	sessionQ := identity.Quadruple{Identity: q.Identity}
	goal := "what did we discuss?"

	// Seed a turn so the projection is non-trivial (non-nil blocks).
	if err := store.AddTurn(context.Background(), sessionQ, memory.ConversationTurn{
		UserMessage:       "hello",
		AssistantResponse: "hi there",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	rc, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		Memory: store, Bus: bus,
	}, q, goal)
	if err != nil {
		t.Fatalf("NewRunContext: %v", err)
	}
	want, err := runctx.FetchMemoryBlocks(context.Background(), store, sessionQ, goal, memory.RecallSettings{}, nil)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}
	if want == nil {
		t.Fatal("fixture sanity: expected non-nil memory blocks after AddTurn")
	}
	if !reflect.DeepEqual(rc.MemoryBlocks, want) {
		t.Errorf("memory parity mismatch:\n NewRunContext = %+v\n FetchMemoryBlocks = %+v", rc.MemoryBlocks, want)
	}
}

// TestNewRunContext_SkillsParity — RunContext.SkillsContext equals
// ProjectSkillsDirectory(Directory.View(...)) for the same capability
// set NewRunContext derives from the catalog.
func TestNewRunContext_SkillsParity(t *testing.T) {
	bus := newFetchTestBus(t)
	q := parityQuad()
	cat := parityCatalog(t)
	dir := parityDirectory(t, bus, q)

	rc, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		SkillsDirectory: dir, Catalog: cat, Bus: bus,
	}, q, "goal")
	if err != nil {
		t.Fatalf("NewRunContext: %v", err)
	}

	filter := tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID}
	// Directory.View reads the identity triple from ctx — attach it the
	// same way NewRunContext does.
	viewCtx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	views, err := dir.View(viewCtx, skills.DirectoryCapability{
		AllowedTools: tools.VisibleNames(cat, filter),
	})
	if err != nil {
		t.Fatalf("Directory.View: %v", err)
	}
	want := runctx.ProjectSkillsDirectory(views)
	if len(want) == 0 {
		t.Fatal("fixture sanity: expected a visible skill in the directory view")
	}
	if !reflect.DeepEqual(rc.SkillsContext, want) {
		t.Errorf("skills parity mismatch:\n NewRunContext = %+v\n direct = %+v", rc.SkillsContext, want)
	}
}

// TestNewRunContext_SkillReaderSnapshot_SharedWithReadTools pins the
// consumer seam: the already-selected effective agent's immutable reader is
// used by both the Directory projection and the read tools. A deliberately
// different invoking-agent provenance value cannot influence selection.
func TestNewRunContext_SkillReaderSnapshot_SharedWithReadTools(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	q := parityQuad()
	selected := &snapshotSkillReader{skill: skills.Skill{
		Name: "selected-skill", Title: "Selected", Trigger: "selected trigger",
		Steps: []string{"selected step"}, Origin: skills.OriginPack, Scope: skills.ScopeProject,
	}}
	fallback := &snapshotSkillReader{skill: skills.Skill{
		Name: "fallback-skill", Title: "Fallback", Trigger: "fallback trigger",
		Steps: []string{"fallback step"}, Origin: skills.OriginPack, Scope: skills.ScopeProject,
	}}
	dir, err := skills.NewDirectory(fallback, skills.Deps{Bus: bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	snapshot, err := skills.NewRunSkillReaderSnapshot(q, "selected-agent", selected)
	if err != nil {
		t.Fatalf("NewRunSkillReaderSnapshot: %v", err)
	}
	if snapshot.EffectiveAgentID() != "selected-agent" {
		t.Fatalf("EffectiveAgentID = %q, want selected-agent", snapshot.EffectiveAgentID())
	}

	rc, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		SkillsDirectory: dir, Catalog: parityCatalog(t), Bus: bus,
	}, q, "goal", runctx.WithSkillReaderSnapshot(snapshot))
	if err != nil {
		t.Fatalf("NewRunContext: %v", err)
	}
	if len(rc.SkillsContext) != 1 {
		t.Fatalf("SkillsContext len = %d, want 1", len(rc.SkillsContext))
	}

	toolCtx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	toolCtx = tools.WithInvokingAgent(toolCtx, "unrelated-invoking-agent")
	toolCtx = skills.WithRunSkillReaderSnapshot(toolCtx, snapshot)
	if _, err := skilltools.GetHandler(toolCtx, fallback, bus, skilltools.GetArgs{
		Names: []string{"selected-skill"}, MaxTokens: 1024,
	}); err != nil {
		t.Fatalf("GetHandler: %v", err)
	}
	if _, err := skilltools.ListHandler(toolCtx, fallback, bus, skilltools.ListArgs{}); err != nil {
		t.Fatalf("ListHandler: %v", err)
	}
	if _, err := skilltools.SearchHandler(toolCtx, fallback, bus, skilltools.SearchArgs{Query: "selected"}); err != nil {
		t.Fatalf("SearchHandler: %v", err)
	}

	if got := selected.getCalls.Load(); got != 1 {
		t.Errorf("selected Get calls = %d, want 1", got)
	}
	if got := selected.listCalls.Load(); got != 2 {
		t.Errorf("selected List calls = %d, want 2 (Directory + skill_list)", got)
	}
	if got := selected.searchCalls.Load(); got != 1 {
		t.Errorf("selected Search calls = %d, want 1", got)
	}
	if got := fallback.getCalls.Load() + fallback.listCalls.Load() + fallback.searchCalls.Load(); got != 0 {
		t.Errorf("fallback reader calls = %d, want 0", got)
	}
}

// TestNewRunContext_ArtifactParity — RunContext.InputArtifacts equals
// the shared ResolveInputArtifacts helper's output for the same IDs.
func TestNewRunContext_ArtifactParity(t *testing.T) {
	bus := newFetchTestBus(t)
	q := parityQuad()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	scope := artifacts.ArtifactScope{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID}
	ref, err := store.PutText(context.Background(), scope, "hello world", artifacts.PutOpts{
		MimeType: "text/plain", Filename: "notes.txt",
	})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}

	rc, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		Artifacts: store, Bus: bus,
	}, q, "goal", runctx.WithInputArtifacts(ref.ID))
	if err != nil {
		t.Fatalf("NewRunContext: %v", err)
	}
	want := runctx.ResolveInputArtifacts(context.Background(), store, q, []string{ref.ID}, nil, runctx.InputArtifactOptions{})
	if len(want) == 0 {
		t.Fatal("fixture sanity: expected the seeded artifact to resolve")
	}
	if !reflect.DeepEqual(rc.InputArtifacts, want) {
		t.Errorf("artifact parity mismatch:\n NewRunContext = %+v\n direct = %+v", rc.InputArtifacts, want)
	}
}

// TestNewRunContext_StreamingSurface — the streaming surface
// (Emit/OnChunk) is wired when a bus is present and nil otherwise,
// matching the run-loop drivers' bus-backed projection.
func TestNewRunContext_StreamingSurface(t *testing.T) {
	bus := newFetchTestBus(t)
	q := parityQuad()

	withBus, err := runctx.NewRunContext(context.Background(), runctx.Sources{Bus: bus}, q, "goal")
	if err != nil {
		t.Fatalf("NewRunContext(bus): %v", err)
	}
	if withBus.Emit == nil || withBus.OnChunk == nil {
		t.Errorf("with a bus: Emit/OnChunk must be wired (Emit=%v OnChunk=%v)", withBus.Emit != nil, withBus.OnChunk != nil)
	}

	noBus, err := runctx.NewRunContext(context.Background(), runctx.Sources{}, q, "goal")
	if err != nil {
		t.Fatalf("NewRunContext(no bus): %v", err)
	}
	if noBus.Emit != nil || noBus.OnChunk != nil {
		t.Errorf("without a bus: Emit/OnChunk must be nil")
	}

	// Per-run fresh artifacts (RepairCounters, Trajectory) every call —
	// no shared mutable state across invocations (D-025).
	if withBus.RepairCounters == nil || withBus.Trajectory == nil {
		t.Fatal("per-run RepairCounters + Trajectory must be allocated")
	}
	if withBus.RepairCounters == noBus.RepairCounters {
		t.Error("RepairCounters must be a fresh pointer per call")
	}
	if withBus.Trajectory == noBus.Trajectory {
		t.Error("Trajectory must be a fresh pointer per call")
	}
}

// TestNewRunContext_OutputSchema_CompiledOnceAndThreaded — a non-empty
// Sources.OutputSchema is compiled ONCE and threaded onto
// RunContext.OutputSchema; the compiled validator enforces the schema.
// An empty schema leaves the field nil (plain run); an invalid schema
// fails the run loudly (no silent degradation, CLAUDE.md §13).
func TestNewRunContext_OutputSchema_CompiledOnceAndThreaded(t *testing.T) {
	q := parityQuad()
	const schema = `{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`

	rc, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		OutputSchema: json.RawMessage(schema),
	}, q, "goal")
	if err != nil {
		t.Fatalf("NewRunContext(schema): %v", err)
	}
	if rc.OutputSchema == nil {
		t.Fatal("RunContext.OutputSchema is nil, want the compiled validator")
	}
	if err := rc.OutputSchema.Validate(json.RawMessage(`{"ok":true}`)); err != nil {
		t.Errorf("Validate(conforming) = %v, want nil", err)
	}
	if err := rc.OutputSchema.Validate(json.RawMessage(`{"ok":"yes"}`)); err == nil {
		t.Error("Validate(non-conforming) = nil, want error")
	}

	// Empty schema → nil field (plain run).
	plain, err := runctx.NewRunContext(context.Background(), runctx.Sources{}, q, "goal")
	if err != nil {
		t.Fatalf("NewRunContext(no schema): %v", err)
	}
	if plain.OutputSchema != nil {
		t.Error("RunContext.OutputSchema must be nil on a plain run")
	}

	// Invalid schema → loud error.
	if _, err := runctx.NewRunContext(context.Background(), runctx.Sources{
		OutputSchema: json.RawMessage(`{not json`),
	}, q, "goal"); err == nil {
		t.Error("NewRunContext(invalid schema) = nil error, want a loud compile error")
	}
}
