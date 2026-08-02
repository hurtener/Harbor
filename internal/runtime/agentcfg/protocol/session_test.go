package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

type sessionControllerKey struct {
	tenant  string
	user    string
	session string
	agent   string
}

func controllerKey(id identity.Quadruple, agentID string) sessionControllerKey {
	return sessionControllerKey{tenant: id.TenantID, user: id.UserID, session: id.SessionID, agent: agentID}
}

type fakeSessionPersonalSkillController struct {
	mu        sync.Mutex
	byOwner   map[sessionControllerKey]map[string]skills.Skill
	listErr   error
	upsertErr error
	deleteErr error
	lists     int
	upserts   int
	deletes   int
}

func newSessionPersonalSkillController() *fakeSessionPersonalSkillController {
	return &fakeSessionPersonalSkillController{byOwner: make(map[sessionControllerKey]map[string]skills.Skill)}
}

func (c *fakeSessionPersonalSkillController) SessionSkills(_ context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	owned := c.byOwner[controllerKey(id, agentID)]
	out := make([]skills.Skill, 0, len(owned))
	for _, skill := range owned {
		out = append(out, cloneSessionSkill(skill))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *fakeSessionPersonalSkillController) UpsertSessionSkill(_ context.Context, id identity.Quadruple, agentID string, skill skills.Skill) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upserts++
	if c.upsertErr != nil {
		return c.upsertErr
	}
	if err := skill.Validate(); err != nil {
		return err
	}
	key := controllerKey(id, agentID)
	if c.byOwner[key] == nil {
		c.byOwner[key] = make(map[string]skills.Skill)
	}
	c.byOwner[key][skill.Name] = cloneSessionSkill(skill)
	return nil
}

func (c *fakeSessionPersonalSkillController) DeleteSessionSkill(_ context.Context, id identity.Quadruple, agentID, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	if c.deleteErr != nil {
		return c.deleteErr
	}
	key := controllerKey(id, agentID)
	if _, ok := c.byOwner[key][name]; !ok {
		return skills.ErrSkillNotFound
	}
	delete(c.byOwner[key], name)
	return nil
}

func (c *fakeSessionPersonalSkillController) seed(id identity.Quadruple, agentID string, skill skills.Skill) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := controllerKey(id, agentID)
	if c.byOwner[key] == nil {
		c.byOwner[key] = make(map[string]skills.Skill)
	}
	c.byOwner[key][skill.Name] = cloneSessionSkill(skill)
}

func (c *fakeSessionPersonalSkillController) calls() (lists, upserts, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists, c.upserts, c.deletes
}

func cloneSessionSkill(skill skills.Skill) skills.Skill {
	skill.Tags = append([]string(nil), skill.Tags...)
	skill.Steps = append([]string(nil), skill.Steps...)
	skill.Preconditions = append([]string(nil), skill.Preconditions...)
	skill.FailureModes = append([]string(nil), skill.FailureModes...)
	skill.RequiredTools = append([]string(nil), skill.RequiredTools...)
	skill.RequiredNS = append([]string(nil), skill.RequiredNS...)
	skill.RequiredTags = append([]string(nil), skill.RequiredTags...)
	return skill
}

type fakeSessionOverlay struct {
	mu             sync.Mutex
	byOwner        map[sessionControllerKey]sessionoverlay.Overlay
	personalWrites atomic.Int64
}

func newSessionOverlay() *fakeSessionOverlay {
	return &fakeSessionOverlay{byOwner: make(map[sessionControllerKey]sessionoverlay.Overlay)}
}

func (o *fakeSessionOverlay) Get(_ context.Context, id identity.Quadruple, agentID string) (sessionoverlay.Overlay, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	overlay, ok := o.byOwner[controllerKey(id, agentID)]
	return cloneSessionOverlay(overlay), ok, nil
}

func (o *fakeSessionOverlay) SetUserPrompt(_ context.Context, id identity.Quadruple, agentID, prompt string) (sessionoverlay.Overlay, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := controllerKey(id, agentID)
	overlay := o.byOwner[key]
	overlay.UserPrompt = prompt
	o.byOwner[key] = overlay
	return cloneSessionOverlay(overlay), nil
}

func (o *fakeSessionOverlay) SetSourceDisables(_ context.Context, id identity.Quadruple, agentID string, servers, tools []string) (sessionoverlay.Overlay, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := controllerKey(id, agentID)
	overlay := o.byOwner[key]
	overlay.DisabledServers = sortedUnique(servers)
	overlay.DisabledTools = sortedUnique(tools)
	o.byOwner[key] = overlay
	return cloneSessionOverlay(overlay), nil
}

func (o *fakeSessionOverlay) AddPersonalSkill(context.Context, identity.Quadruple, string, string) (sessionoverlay.Overlay, error) {
	o.personalWrites.Add(1)
	return sessionoverlay.Overlay{}, sessionoverlay.ErrCutoverPending
}

func (o *fakeSessionOverlay) RemovePersonalSkill(context.Context, identity.Quadruple, string, string) (sessionoverlay.Overlay, error) {
	o.personalWrites.Add(1)
	return sessionoverlay.Overlay{}, sessionoverlay.ErrCutoverPending
}

func (o *fakeSessionOverlay) Close(context.Context) error { return nil }

func cloneSessionOverlay(overlay sessionoverlay.Overlay) sessionoverlay.Overlay {
	overlay.DisabledServers = append([]string(nil), overlay.DisabledServers...)
	overlay.DisabledTools = append([]string(nil), overlay.DisabledTools...)
	overlay.PersonalSkills = append([]string(nil), overlay.PersonalSkills...)
	return overlay
}

func sortedUnique(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type skillStoreSpy struct {
	skills.SkillStore
	calls atomic.Int64
}

func (s *skillStoreSpy) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	s.calls.Add(1)
	return s.SkillStore.Get(ctx, id, name)
}

func (s *skillStoreSpy) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	s.calls.Add(1)
	return s.SkillStore.GetScope(ctx, id, name, scope)
}

func (s *skillStoreSpy) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	s.calls.Add(1)
	return s.SkillStore.List(ctx, id, filter)
}

func (s *skillStoreSpy) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	s.calls.Add(1)
	return s.SkillStore.Search(ctx, id, query, limit)
}

func (s *skillStoreSpy) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	s.calls.Add(1)
	return s.SkillStore.Upsert(ctx, id, skill)
}

func (s *skillStoreSpy) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	s.calls.Add(1)
	return s.SkillStore.Delete(ctx, id, name, scope)
}

// sessionSvc builds a Service with distinct shared-skill and agent-owned
// session-personal authorities.
func sessionSvc(t *testing.T) *agentcfgprotocol.Service {
	t.Helper()
	s, _, _, _ := sessionSvcWithController(t, newSessionPersonalSkillController())
	return s
}

func sessionSvcWithController(t *testing.T, controller *fakeSessionPersonalSkillController) (*agentcfgprotocol.Service, *skillStoreSpy, *fakeSessionOverlay, *fakeSessionPersonalSkillController) {
	t.Helper()
	overlay := newSessionOverlay()
	legacy := &skillStoreSpy{SkillStore: newSkills(t)}
	s, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithSkillStore(legacy),
		agentcfgprotocol.WithSessionOverlay(overlay),
		agentcfgprotocol.WithSessionPersonalSkillController(controller),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s, legacy, overlay, controller
}

// TestSessionSetUserPrompt_RecordsOverlay proves the session user prompt is
// stored and round-trips through the overlay. The session shape carries no
// Base field — base-unwritable is structural (there is nothing to assert
// against because there is no base path on the session surface at all).
func TestSessionSetUserPrompt_RecordsOverlay(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	resp, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: scope(), AgentID: testAgentID, UserPrompt: "be concise",
	})
	if err != nil {
		t.Fatalf("set user prompt: %v", err)
	}
	if resp.Overlay.UserPrompt != "be concise" {
		t.Fatalf("user prompt not recorded: %+v", resp.Overlay)
	}
}

// TestSessionSetSourceDisables_NarrowOnly proves the session disable set is
// recorded as a DISABLE list. There is no enable field on the request shape,
// so a session can never express "enable a not-allowed source" — widening is
// structurally impossible on this surface.
func TestSessionSetSourceDisables_NarrowOnly(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	resp, err := s.SessionSetSourceDisables(ctx, prototypes.AgentConfigSessionSetSourceDisablesRequest{
		Identity: scope(), AgentID: testAgentID,
		DisabledServers: []string{"srvA", "srvA"}, // de-duped
		DisabledTools:   []string{"toolZ"},
	})
	if err != nil {
		t.Fatalf("set source disables: %v", err)
	}
	if len(resp.Overlay.DisabledServers) != 1 || resp.Overlay.DisabledServers[0] != "srvA" {
		t.Fatalf("disabled servers = %+v", resp.Overlay.DisabledServers)
	}
	if len(resp.Overlay.DisabledTools) != 1 || resp.Overlay.DisabledTools[0] != "toolZ" {
		t.Fatalf("disabled tools = %+v", resp.Overlay.DisabledTools)
	}
}

// TestSessionSkills_UpsertListDelete proves the agent-owned personal-skill
// lifecycle: the controller owns the mutation and list, while overlay names
// are a freshly derived response projection rather than persisted state.
func TestSessionSkills_UpsertListDelete(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	up, err := s.SessionSkillsUpsert(ctx, prototypes.AgentConfigSessionSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{
			Name: "personal-a", Trigger: "when asked", Steps: []string{"do it"},
			Origin: "generated", Scope: "tenant", // requested scope ignored — forced to session
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if up.Skill.Scope != string(skills.ScopeSession) {
		t.Fatalf("personal skill must be session-scoped (never promote); got %q", up.Skill.Scope)
	}
	if len(up.Overlay.PersonalSkills) != 1 || up.Overlay.PersonalSkills[0] != "personal-a" {
		t.Fatalf("overlay personal skills = %+v", up.Overlay.PersonalSkills)
	}
	list, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(list.Skills) != 1 {
		t.Fatalf("list = %d err=%v", len(list.Skills), err)
	}
	del, err := s.SessionSkillsDelete(ctx, prototypes.AgentConfigSessionSkillsDeleteRequest{
		Identity: scope(), AgentID: testAgentID, Name: "personal-a",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(del.Overlay.PersonalSkills) != 0 {
		t.Fatalf("personal skill not removed from overlay: %+v", del.Overlay.PersonalSkills)
	}
}

// TestSession_Unwired proves the session verbs fail loud (not silently) when
// the overlay store is not wired — never a stub default.
func TestSession_Unwired(t *testing.T) {
	ctx := context.Background()
	s := svc(t, true) // skills wired, overlay NOT wired
	_, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: scope(), AgentID: testAgentID, UserPrompt: "x",
	})
	if !errors.Is(err, agentcfgprotocol.ErrSessionOverlayUnavailable) {
		t.Fatalf("unwired overlay should fail with ErrSessionOverlayUnavailable, got %v", err)
	}
}

func TestSession_ControllerUnwiredFailsLoudBeforeOverlayMutation(t *testing.T) {
	ctx := context.Background()
	overlay := newSessionOverlay()
	s, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithSkillStore(newSkills(t)),
		agentcfgprotocol.WithSessionOverlay(overlay),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: scope(), AgentID: testAgentID, UserPrompt: "must not persist",
	})
	if !errors.Is(err, agentcfgprotocol.ErrSkillsUnavailable) {
		t.Fatalf("unwired controller error = %v, want ErrSkillsUnavailable", err)
	}
	got, found, getErr := overlay.Get(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID)
	if getErr != nil || found || got.UserPrompt != "" {
		t.Fatalf("unwired controller mutated overlay: overlay=%+v found=%v err=%v", got, found, getErr)
	}
}

func TestSessionSkills_ControllerIsSoleAuthorityAndReloads(t *testing.T) {
	ctx := context.Background()
	controller := newSessionPersonalSkillController()
	s, legacy, overlay, controller := sessionSvcWithController(t, controller)
	req := prototypes.AgentConfigSessionSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "owned", Trigger: "when", Steps: []string{"do"}, Origin: "generated", Scope: "user"},
	}
	up, err := s.SessionSkillsUpsert(ctx, req)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if up.Skill.Scope != string(skills.ScopeSession) || len(up.Overlay.PersonalSkills) != 1 || up.Overlay.PersonalSkills[0] != "owned" {
		t.Fatalf("upsert response did not reload forced session tier: %+v", up)
	}
	if legacy.calls.Load() != 0 || overlay.personalWrites.Load() != 0 {
		t.Fatalf("session upsert touched legacy authority: skill-store calls=%d overlay personal writes=%d", legacy.calls.Load(), overlay.personalWrites.Load())
	}
	lists, upserts, deletes := controller.calls()
	if lists != 1 || upserts != 1 || deletes != 0 {
		t.Fatalf("controller calls after upsert = list:%d upsert:%d delete:%d", lists, upserts, deletes)
	}

	list, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(list.Skills) != 1 || list.Skills[0].Name != "owned" {
		t.Fatalf("list after upsert = %+v err=%v", list.Skills, err)
	}
	del, err := s.SessionSkillsDelete(ctx, prototypes.AgentConfigSessionSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "owned"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(del.Overlay.PersonalSkills) != 0 {
		t.Fatalf("delete response retained dynamic name: %+v", del.Overlay.PersonalSkills)
	}
	if legacy.calls.Load() != 0 || overlay.personalWrites.Load() != 0 {
		t.Fatalf("session lifecycle touched legacy authority: skill-store calls=%d overlay personal writes=%d", legacy.calls.Load(), overlay.personalWrites.Load())
	}
	lists, upserts, deletes = controller.calls()
	if lists != 3 || upserts != 1 || deletes != 1 {
		t.Fatalf("controller calls after lifecycle = list:%d upsert:%d delete:%d", lists, upserts, deletes)
	}
}

func TestSessionOverlayResponses_ProjectCurrentControllerNamesOnly(t *testing.T) {
	ctx := context.Background()
	controller := newSessionPersonalSkillController()
	s, legacy, overlay, _ := sessionSvcWithController(t, controller)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	overlay.mu.Lock()
	overlay.byOwner[controllerKey(id, testAgentID)] = sessionoverlay.Overlay{PersonalSkills: []string{"stale-legacy-name"}}
	overlay.mu.Unlock()
	controller.seed(id, testAgentID, skills.Skill{Name: "z-owned", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession})
	controller.seed(id, testAgentID, skills.Skill{Name: "not-session", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser})

	prompt, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{Identity: scope(), AgentID: testAgentID, UserPrompt: "prompt"})
	if err != nil {
		t.Fatalf("set prompt: %v", err)
	}
	if got := prompt.Overlay.PersonalSkills; len(got) != 1 || got[0] != "z-owned" {
		t.Fatalf("prompt dynamic names = %v", got)
	}
	controller.seed(id, testAgentID, skills.Skill{Name: "a-owned", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession})
	disables, err := s.SessionSetSourceDisables(ctx, prototypes.AgentConfigSessionSetSourceDisablesRequest{
		Identity: scope(), AgentID: testAgentID, DisabledServers: []string{"server"},
	})
	if err != nil {
		t.Fatalf("set disables: %v", err)
	}
	if got := disables.Overlay.PersonalSkills; len(got) != 2 || got[0] != "a-owned" || got[1] != "z-owned" {
		t.Fatalf("disable dynamic names = %v", got)
	}
	list, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Skills) != 2 || list.Skills[0].Scope != string(skills.ScopeSession) || list.Skills[1].Scope != string(skills.ScopeSession) {
		t.Fatalf("session list leaked a non-session tier: %+v", list.Skills)
	}
	if legacy.calls.Load() != 0 || overlay.personalWrites.Load() != 0 {
		t.Fatalf("session reads/projections touched legacy authority: skill-store calls=%d overlay personal writes=%d", legacy.calls.Load(), overlay.personalWrites.Load())
	}
}

func TestSessionSkills_ControllerSentinelsPassThrough(t *testing.T) {
	ctx := context.Background()
	t.Run("cutover upsert", func(t *testing.T) {
		controller := newSessionPersonalSkillController()
		controller.upsertErr = sessionoverlay.ErrCutoverPending
		s, _, _, _ := sessionSvcWithController(t, controller)
		_, err := s.SessionSkillsUpsert(ctx, prototypes.AgentConfigSessionSkillsUpsertRequest{
			Identity: scope(), AgentID: testAgentID,
			Skill: prototypes.AgentConfigSkillInput{Name: "owned", Trigger: "t", Steps: []string{"s"}, Origin: "generated"},
		})
		if !errors.Is(err, sessionoverlay.ErrCutoverPending) {
			t.Fatalf("upsert error = %v, want ErrCutoverPending", err)
		}
	})
	t.Run("cutover delete", func(t *testing.T) {
		controller := newSessionPersonalSkillController()
		controller.deleteErr = sessionoverlay.ErrCutoverPending
		s, _, _, _ := sessionSvcWithController(t, controller)
		_, err := s.SessionSkillsDelete(ctx, prototypes.AgentConfigSessionSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "owned"})
		if !errors.Is(err, sessionoverlay.ErrCutoverPending) {
			t.Fatalf("delete error = %v, want ErrCutoverPending", err)
		}
	})
	t.Run("unstable read", func(t *testing.T) {
		controller := newSessionPersonalSkillController()
		controller.listErr = sessionoverlay.ErrSessionSkillReadUnstable
		s, _, _, _ := sessionSvcWithController(t, controller)
		_, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: scope(), AgentID: testAgentID})
		if !errors.Is(err, sessionoverlay.ErrSessionSkillReadUnstable) {
			t.Fatalf("list error = %v, want ErrSessionSkillReadUnstable", err)
		}
	})
}

// TestSession_IdentityRequired proves an incomplete identity triple fails
// closed on the session surface.
func TestSession_IdentityRequired(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	_, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "", Session: "s"}, AgentID: testAgentID, UserPrompt: "x",
	})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should fail with ErrIdentityRequired, got %v", err)
	}
}

// TestSession_CrossSessionIsolation proves one session's overlay + personal
// skills are invisible to another session (the overlay is keyed by the real
// triple; the SkillStore is session-scoped).
func TestSession_CrossSessionIsolation(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	sessA := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionA"}
	sessB := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionB"}

	if _, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: sessA, AgentID: testAgentID, UserPrompt: "A-only",
	}); err != nil {
		t.Fatalf("set A: %v", err)
	}
	if _, err := s.SessionSkillsUpsert(ctx, prototypes.AgentConfigSessionSkillsUpsertRequest{
		Identity: sessA, AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "a-skill", Trigger: "t", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}); err != nil {
		t.Fatalf("upsert A skill: %v", err)
	}

	// Session B sees no overlay user prompt and no personal skill from A.
	bList, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: sessB, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bList.Skills) != 0 {
		t.Fatalf("session B must not see session A's personal skill: %+v", bList.Skills)
	}
	// Setting B's prompt does not affect A.
	bResp, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: sessB, AgentID: testAgentID, UserPrompt: "B-only",
	})
	if err != nil {
		t.Fatalf("set B: %v", err)
	}
	if bResp.Overlay.UserPrompt != "B-only" || len(bResp.Overlay.PersonalSkills) != 0 {
		t.Fatalf("session B overlay leaked from A: %+v", bResp.Overlay)
	}
}

func TestSession_ConcurrentTupleAndAgentIsolation(t *testing.T) {
	const callers = 128
	s := sessionSvc(t)
	errCh := make(chan error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessionID := fmt.Sprintf("session-%03d", i)
			agentID := fmt.Sprintf("agent-%d", i%4)
			name := fmt.Sprintf("skill-%03d", i)
			wireID := prototypes.IdentityScope{Tenant: "tenant", User: "user", Session: sessionID}
			if _, err := s.SessionSkillsUpsert(context.Background(), prototypes.AgentConfigSessionSkillsUpsertRequest{
				Identity: wireID, AgentID: agentID,
				Skill: prototypes.AgentConfigSkillInput{Name: name, Trigger: "t", Steps: []string{"s"}, Origin: "generated"},
			}); err != nil {
				errCh <- fmt.Errorf("upsert %s/%s: %w", sessionID, agentID, err)
				return
			}
			prompt, err := s.SessionSetUserPrompt(context.Background(), prototypes.AgentConfigSessionSetUserPromptRequest{
				Identity: wireID, AgentID: agentID, UserPrompt: name,
			})
			if err != nil {
				errCh <- fmt.Errorf("prompt %s/%s: %w", sessionID, agentID, err)
				return
			}
			if prompt.Overlay.UserPrompt != name || len(prompt.Overlay.PersonalSkills) != 1 || prompt.Overlay.PersonalSkills[0] != name {
				errCh <- fmt.Errorf("projection %s/%s leaked: %+v", sessionID, agentID, prompt.Overlay)
				return
			}
			list, err := s.SessionSkillsList(context.Background(), prototypes.AgentConfigSessionSkillsListRequest{Identity: wireID, AgentID: agentID})
			if err != nil {
				errCh <- fmt.Errorf("list %s/%s: %w", sessionID, agentID, err)
				return
			}
			if len(list.Skills) != 1 || list.Skills[0].Name != name {
				errCh <- fmt.Errorf("list %s/%s leaked: %+v", sessionID, agentID, list.Skills)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
