package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// This integration test wires the agent-config control plane end-to-end
// with REAL drivers: the StateStore-backed registry, the in-memory event
// bus, the localdb SkillStore, the real Protocol Service, and the real
// wire handler. It drives the admin surface over HTTP, observes the
// canonical events on the bus, proves the skills consumer's run-start
// projection reflects the active skills-set across a rollback, and
// exercises two failure modes (rollback-to-missing; non-admin rejected).

const (
	acTenant  = "tenant-int"
	acUser    = "admin-int"
	acSession = "sess-int"
	acAgent   = "agent-int"
)

type acHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	skills   skills.SkillStore
	dir      *skills.Directory
	bus      events.EventBus
}

// agentSkillReader gives the directory the same agent-bound body view that a
// run-start reader snapshot supplies in production. Protocol upserts persist
// agent-owned bodies, so the fallback reader must select that agent namespace.
type agentSkillReader struct {
	skills.SkillStore
	agentID string
}

func (r agentSkillReader) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	filter.AgentID = r.agentID
	return r.SkillStore.ListAgent(ctx, id, r.agentID, filter)
}

func newACHarness(t *testing.T) *acHarness {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 128,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	skillStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	activateFixtureAgent(t, reg, identity.Identity{TenantID: acTenant, UserID: acUser, SessionID: acSession}, acAgent)
	dir, err := skills.NewDirectory(agentSkillReader{SkillStore: skillStore, agentID: acAgent}, skills.Deps{Bus: bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSkillStore(skillStore))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = skillStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &acHarness{handler: h, registry: reg, skills: skillStore, dir: dir, bus: bus}
}

// call POSTs to the handler with identity headers and the given scopes in
// the request context, returning the recorder.
func (h *acHarness) call(t *testing.T, path string, body any, scopes []auth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, acTenant)
	req.Header.Set(stream.HeaderUser, acUser)
	req.Header.Set(stream.HeaderSession, acSession)
	ctx := auth.WithScopes(req.Context(), scopes)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func adminScopes() []auth.Scope { return []auth.Scope{auth.ScopeAdmin} }

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return out
}

func TestE2E_AgentConfig_ControlPlane(t *testing.T) {
	h := newACHarness(t)
	ctx := context.Background()

	// Observe the canonical events on the bus (admin fan-in across the
	// agentcfg synthetic identities the registry writes under).
	sub, err := h.bus.Subscribe(ctx, events.Filter{
		Admin: true,
		Types: []events.EventType{agentcfg.EventTypeConfigRevised, agentcfg.EventTypeConfigReverted},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	revised := make(chan events.Event, 16)
	reverted := make(chan events.Event, 16)
	go func() {
		for ev := range sub.Events() {
			switch ev.Type {
			case agentcfg.EventTypeConfigRevised:
				revised <- ev
			case agentcfg.EventTypeConfigReverted:
				reverted <- ev
			}
		}
	}()

	// --- Skills consumer end-to-end: upsert two skills via the Protocol. ---
	up1 := decode[prototypes.AgentConfigSkillsUpsertResponse](t, h.call(t, "/v1/agent_config/skills/upsert", prototypes.AgentConfigSkillsUpsertRequest{
		AgentID: acAgent,
		Skill:   controlPlaneSkill("alpha"),
	}, adminScopes()))
	rev1 := up1.Revision.RevisionID
	_ = decode[prototypes.AgentConfigSkillsUpsertResponse](t, h.call(t, "/v1/agent_config/skills/upsert", prototypes.AgentConfigSkillsUpsertRequest{
		AgentID: acAgent,
		Skill:   controlPlaneSkill("beta"),
	}, adminScopes()))

	// Two revised events observed, identity propagated.
	for range 2 {
		ev := waitEvent(t, revised)
		if ev.Identity.TenantID != acTenant || ev.Identity.UserID != acUser {
			t.Fatalf("revised event identity=%+v", ev.Identity)
		}
		p, ok := ev.Payload.(agentcfg.ConfigRevisedPayload)
		if !ok || p.AgentID != acAgent || p.RevisionID == "" {
			t.Fatalf("revised payload=%+v ok=%v", ev.Payload, ok)
		}
	}

	// Run-start projection reflects the active skills-set {alpha, beta}.
	if got := h.projectedSkillNames(t); !sameSet(got, []string{"alpha", "beta"}) {
		t.Fatalf("projection after upserts=%v want {alpha,beta}", got)
	}

	// --- Diff across the two upsert revisions shows the set-diff; the initial lifecycle revision is also listed. ---
	list := decode[prototypes.AgentConfigListRevisionsResponse](t, h.call(t, "/v1/agent_config/list_revisions", prototypes.AgentConfigListRevisionsRequest{AgentID: acAgent}, adminScopes()))
	if len(list.Revisions) != 3 {
		t.Fatalf("list=%d want 3", len(list.Revisions))
	}
	diff := decode[prototypes.AgentConfigDiffResponse](t, h.call(t, "/v1/agent_config/diff", prototypes.AgentConfigDiffRequest{
		AgentID: acAgent, FromRevision: rev1, ToRevision: list.Revisions[0].RevisionID,
	}, adminScopes()))
	if len(diff.Diff.Skills.Added) != 1 || diff.Diff.Skills.Added[0] != "beta" {
		t.Fatalf("diff added=%v want [beta]", diff.Diff.Skills.Added)
	}

	// --- Rollback to rev1 repoints membership; projection reflects it. ---
	_ = decode[prototypes.AgentConfigRollbackResponse](t, h.call(t, "/v1/agent_config/rollback", prototypes.AgentConfigRollbackRequest{
		AgentID: acAgent, RevisionID: rev1,
	}, adminScopes()))
	ev := waitEvent(t, reverted)
	if rp, ok := ev.Payload.(agentcfg.ConfigRevertedPayload); !ok || rp.RevisionID != rev1 {
		t.Fatalf("reverted payload=%+v", ev.Payload)
	}
	if got := h.projectedSkillNames(t); !sameSet(got, []string{"alpha"}) {
		t.Fatalf("projection after rollback=%v want {alpha}", got)
	}

	// --- Failure mode 1: rollback to a missing revision → 404 CodeNotFound. ---
	rec := h.call(t, "/v1/agent_config/rollback", prototypes.AgentConfigRollbackRequest{AgentID: acAgent, RevisionID: "01ZZZNONE"}, adminScopes())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rollback-missing status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != protoerrors.CodeNotFound {
		t.Fatalf("rollback-missing code=%q want %q", code, protoerrors.CodeNotFound)
	}

	// --- Failure mode 2: non-admin rejected with CodeScopeMismatch (403). ---
	rec = h.call(t, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: acAgent}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("non-admin code=%q want %q", code, protoerrors.CodeScopeMismatch)
	}
}

func controlPlaneSkill(name string) prototypes.AgentConfigSkillInput {
	return prototypes.AgentConfigSkillInput{
		Name: name, Title: name + " skill", Description: "a persisted control-plane fixture skill",
		Trigger: "when the fixture asks", Steps: []string{"perform the fixture step"},
		Origin: "generated", Scope: "session",
	}
}

// projectedSkillNames drives the run-start skills projection the dev run
// loop performs by calling the SAME shared projection function the
// production driver + devstack twin call (CLAUDE.md §17.4 — the test
// exercises the real projection, not a test-local copy), over REAL drivers
// on the seam (the StateStore-backed registry + the skills directory).
func (h *acHarness) projectedSkillNames(t *testing.T) []string {
	t.Helper()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: acTenant, UserID: acUser, SessionID: acSession}}
	dirCtx, err := identity.With(context.Background(), id.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	views, err := h.dir.View(dirCtx, skills.DirectoryCapability{})
	if err != nil {
		t.Fatalf("directory view: %v", err)
	}
	projected, err := projection.ActiveSkillViews(context.Background(), h.registry, nil, acAgent, id, views)
	if err != nil {
		t.Fatalf("projection.ActiveSkillViews: %v", err)
	}
	out := make([]string, 0, len(projected))
	for _, v := range projected {
		out = append(out, v.Name)
	}
	return out
}

func waitEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
		return events.Event{}
	}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) protoerrors.Code {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return e.Code
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]struct{}{}
	for _, g := range got {
		m[g] = struct{}{}
	}
	for _, w := range want {
		if _, ok := m[w]; !ok {
			return false
		}
	}
	return true
}
