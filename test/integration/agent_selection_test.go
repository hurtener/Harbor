// Caller-named agent selection, end to end (Phase 215 / D-360; RFC §5.2,
// §6.2, §6.16).
//
// REAL drivers at every seam, no mocks at the boundary (CLAUDE.md §17.3):
// a real StateStore-backed `agentcfg.Registry`, the real task registry
// (inprocess), the real Protocol ControlSurface wired to the PRODUCTION
// AgentResolver adapter, and the real per-task run-loop driver. The
// credential-plane leg additionally drives a real go-sdk MCP fixture
// server and a real RFC 8693 token-exchange broker (§17.8 — the
// assertions are made SERVER-SIDE against the actual wire).
//
// It proves:
//
//   - a `start` naming agent A vs agent B resolves each run's prompt +
//     LLM params from ITS OWN agent's config revision;
//   - omitting `agent_id` is the unchanged default path;
//   - a `ConfigScopeUser` layer under the named agent composes ABOVE that
//     agent's admin base, in the documented order;
//   - FAILURE MODE 1: a foreign tenant's agent id is refused AND no task
//     row is written;
//   - FAILURE MODE 2: a resolver whose backing store errors fails the
//     request LOUD rather than falling through to the default agent;
//   - the CREDENTIAL PLANE is untouched: a caller-named run's outbound
//     MCP `_meta.agent_id` and its RFC 8693 `actor_token` both carry the
//     runtime's BOOT agent id;
//   - identity propagates end to end and N≥10 concurrent runs across two
//     tenants × two agents never cross-talk.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	// selBoot is the runtime's boot-configured agent id — the value the
	// credential plane must keep asserting no matter what a caller names.
	selBoot = "sel-e2e-boot-agent"
	// selAlpha / selBravo are two caller-nameable agents with divergent
	// config revisions under the same tenant.
	selAlpha = "sel-e2e-alpha"
	selBravo = "sel-e2e-bravo"
	// selUnknown is deliberately in this fixture's signed reach but absent
	// from every tenant's config. The foreign-vs-unknown assertion below
	// compares selection results only after authority has admitted both ids.
	selUnknown = "never-existed-agent"
)

// selRig is the assembled end-to-end stack.
type selRig struct {
	surface *protocol.ControlSurface
	tasks   tasks.TaskRegistry
	cfg     agentcfg.Registry
	state   state.StateStore
	bus     events.EventBus
	obs     *selObserver
}

// selObserver captures, per run id, what the planner saw. It is the
// end-to-end read-out of the run-start projections.
type selObserver struct {
	mu   sync.Mutex
	seen map[string]selSeen
	ch   chan string
	// invoke, when non-nil, is called INSIDE planner.Next with the live
	// run ctx (the credential-plane leg drives the MCP tool there so the
	// ctx is not used after the run completes).
	invoke func(ctx context.Context)
}

type selSeen struct {
	agentProvenance string
	model           string
	basePrompt      string
	userPrompt      string
	tenant          string
}

func (o *selObserver) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	s := selSeen{}
	s.agentProvenance, _ = tools.InvokingAgentFrom(ctx)
	if id, ok := identity.From(ctx); ok {
		s.tenant = id.TenantID
	}
	if rc.LLMOverrides != nil {
		if rc.LLMOverrides.Model != nil {
			s.model = *rc.LLMOverrides.Model
		}
		if rc.LLMOverrides.BasePromptLayer != nil {
			s.basePrompt = *rc.LLMOverrides.BasePromptLayer
		}
		if rc.LLMOverrides.UserPromptLayer != nil {
			s.userPrompt = *rc.LLMOverrides.UserPromptLayer
		}
	}
	if o.invoke != nil {
		o.invoke(ctx)
	}
	o.mu.Lock()
	o.seen[rc.Quadruple.RunID] = s
	o.mu.Unlock()
	select {
	case o.ch <- rc.Quadruple.RunID:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (o *selObserver) get(runID string) (selSeen, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.seen[runID]
	return s, ok
}

// newSelRig assembles the stack. cfgRegOverride lets the resolver-error
// leg inject a CLOSED registry without disturbing the rest.
func newSelRig(t *testing.T) *selRig {
	t.Helper()
	red := patternsAudit.New()

	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	st, err := stateInmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	taskReg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store: st, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.OpenDriver: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	cfgReg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = cfgReg.Close(context.Background()) })

	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg,
		protocol.WithAgentResolver(serve.NewAgentResolverAdapter(cfgReg, selBoot)),
		protocol.WithAgentReachAuthorizer(protocolauth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	obs := &selObserver{seen: map[string]selSeen{}, ch: make(chan string, 64)}
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	driver, err := serve.NewRunLoopDriver(serve.RunLoopDriverOptions{
		Bus:           bus,
		RunLoop:       rl,
		Planner:       obs,
		Tasks:         taskReg,
		AgentConfig:   cfgReg,
		AgentConfigID: selBoot,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	return &selRig{surface: surface, tasks: taskReg, cfg: cfgReg, state: st, bus: bus, obs: obs}
}

func selID(tenant string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: "alice", SessionID: "s-sel"}
}

func selCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	return protocolauth.WithAgentReach(ctx, []string{selBoot, selAlpha, selBravo, selUnknown})
}

// selWriteAgent pins an admin (ConfigScopeAgent) revision.
func (r *selRig) selWriteAgent(t *testing.T, id identity.Identity, agentID, base, model string) {
	t.Helper()
	b, m := base, model
	if _, err := r.cfg.SetRevision(context.Background(),
		identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{
			PromptLayers: &agentcfg.PromptLayers{Base: &b},
			LLMParams:    &agentcfg.LLMParams{Model: &m},
		}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(%s/%s): %v", id.TenantID, agentID, err)
	}
}

// selStart dispatches a `start` and returns the spawned task id.
func (r *selRig) selStart(t *testing.T, id identity.Identity, agentID, key string) (string, error) {
	t.Helper()
	resp, err := r.surface.Dispatch(selCtx(t, id), methods.MethodStart, &prototypes.StartRequest{
		Identity:       prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "e2e agent selection",
		IdempotencyKey: key,
		AgentID:        agentID,
	})
	if err != nil {
		return "", err
	}
	return resp.(*prototypes.StartResponse).TaskID, nil
}

// selAwait waits for the run the given task id produced. The run id
// equals the task id on the foreground path.
func (r *selRig) selAwait(t *testing.T, taskID string) selSeen {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if s, ok := r.obs.get(taskID); ok {
			return s
		}
		select {
		case <-r.obs.ch:
		case <-deadline:
			t.Fatalf("run for task %s never reached the planner", taskID)
		}
	}
}

// TestE2E_AgentSelection_NamedAgentDrivesRunConfiguration is the happy
// path plus the D-309-upholding persistence check.
func TestE2E_AgentSelection_NamedAgentDrivesRunConfiguration(t *testing.T) {
	rig := newSelRig(t)
	id := selID("tenant-sel-a")
	rig.selWriteAgent(t, id, selAlpha, "ALPHA-BASE", "alpha-model")
	rig.selWriteAgent(t, id, selBravo, "BRAVO-BASE", "bravo-model")

	alphaTask, err := rig.selStart(t, id, selAlpha, "e2e-alpha")
	if err != nil {
		t.Fatalf("start(alpha): %v", err)
	}
	bravoTask, err := rig.selStart(t, id, selBravo, "e2e-bravo")
	if err != nil {
		t.Fatalf("start(bravo): %v", err)
	}
	plainTask, err := rig.selStart(t, id, "", "e2e-plain")
	if err != nil {
		t.Fatalf("start(no agent): %v", err)
	}

	alpha := rig.selAwait(t, alphaTask)
	bravo := rig.selAwait(t, bravoTask)
	plain := rig.selAwait(t, plainTask)

	if alpha.basePrompt != "ALPHA-BASE" || alpha.model != "alpha-model" {
		t.Errorf("alpha run resolved base=%q model=%q, want ALPHA-BASE/alpha-model", alpha.basePrompt, alpha.model)
	}
	if bravo.basePrompt != "BRAVO-BASE" || bravo.model != "bravo-model" {
		t.Errorf("bravo run resolved base=%q model=%q, want BRAVO-BASE/bravo-model", bravo.basePrompt, bravo.model)
	}
	if alpha.basePrompt == bravo.basePrompt {
		t.Error("two caller-named agents resolved the SAME prompt — selection is inert")
	}
	// Omitted agent_id: the boot agent has no revision, so the run
	// resolves nothing — byte-identical to the pre-field default path.
	if plain.basePrompt != "" || plain.model != "" {
		t.Errorf("a run naming no agent resolved base=%q model=%q, want the unchanged empty default",
			plain.basePrompt, plain.model)
	}
	// Identity propagated end to end.
	for name, s := range map[string]selSeen{"alpha": alpha, "bravo": bravo, "plain": plain} {
		if s.tenant != id.TenantID {
			t.Errorf("%s run saw tenant %q, want %q", name, s.tenant, id.TenantID)
		}
	}

	// The named agent is persisted on the TASK. The SESSION row is
	// untouched — a session may run several agents, so no single-valued
	// session→agent binding is introduced here.
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	task, err := rig.tasks.Get(ctx, tasks.TaskID(alphaTask))
	if err != nil {
		t.Fatalf("tasks.Get: %v", err)
	}
	if task.AgentID != selAlpha {
		t.Errorf("task.AgentID = %q, want %q", task.AgentID, selAlpha)
	}
	plainRow, err := rig.tasks.Get(ctx, tasks.TaskID(plainTask))
	if err != nil {
		t.Fatalf("tasks.Get(plain): %v", err)
	}
	if plainRow.AgentID != "" {
		t.Errorf("an unnamed run persisted AgentID=%q, want empty (defaulted)", plainRow.AgentID)
	}
}

// TestE2E_AgentSelection_UserLayerComposesUnderTheNamedAgent — a user's
// ConfigScopeUser revision keyed on the SAME agent id composes ABOVE that
// agent's admin base, without any chaining.
func TestE2E_AgentSelection_UserLayerComposesUnderTheNamedAgent(t *testing.T) {
	rig := newSelRig(t)
	id := selID("tenant-sel-user")
	rig.selWriteAgent(t, id, selAlpha, "ADMIN-BASE", "alpha-model")

	userLayer := "USER-LAYER"
	if _, err := rig.cfg.SetRevision(context.Background(),
		identity.Quadruple{Identity: id}, selAlpha, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{PromptLayers: &agentcfg.PromptLayers{User: &userLayer}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(user): %v", err)
	}

	taskID, err := rig.selStart(t, id, selAlpha, "e2e-userlayer")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	seen := rig.selAwait(t, taskID)
	if seen.basePrompt != "ADMIN-BASE" {
		t.Errorf("base layer = %q, want the ADMIN base (the user tier must never replace it)", seen.basePrompt)
	}
	if seen.userPrompt != "USER-LAYER" {
		t.Errorf("user layer = %q, want USER-LAYER composed above the admin base", seen.userPrompt)
	}
}

// TestE2E_AgentSelection_ForeignTenantRefusedAndNoTaskWritten is
// FAILURE MODE 1.
func TestE2E_AgentSelection_ForeignTenantRefusedAndNoTaskWritten(t *testing.T) {
	rig := newSelRig(t)
	owner := selID("tenant-sel-owner")
	intruder := selID("tenant-sel-intruder")
	rig.selWriteAgent(t, owner, selAlpha, "OWNER-BASE", "owner-model")

	ctx, err := identity.With(context.Background(), intruder)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	before, err := rig.tasks.List(ctx, intruder, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}

	_, foreignErr := rig.selStart(t, intruder, selAlpha, "e2e-foreign")
	if foreignErr == nil {
		t.Fatal("an intruder named another tenant's agent and the start SUCCEEDED")
	}
	if code := selCode(t, foreignErr); code != protoerrors.CodeInvalidRequest {
		t.Fatalf("refusal code = %q, want %q", code, protoerrors.CodeInvalidRequest)
	}
	after, err := rig.tasks.List(ctx, intruder, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("task count %d → %d: a refused start MUST NOT write a task row", len(before), len(after))
	}

	// The refusal is not an existence oracle: an unknown id that is also
	// authorized by this fixture's signed reach is refused with the identical
	// selection message. Missing or excluded reach is covered separately by
	// the Phase 232 recording-resolver tests.
	_, unknownErr := rig.selStart(t, intruder, selUnknown, "e2e-unknown")
	if unknownErr == nil {
		t.Fatal("an unknown agent id was accepted")
	}
	if foreignErr.Error() != unknownErr.Error() {
		t.Fatalf("cross-tenant existence oracle:\n foreign = %q\n unknown = %q", foreignErr.Error(), unknownErr.Error())
	}
	// And the owning tenant CAN name it — proving the refusal was
	// tenant-scoping and not a broken revision.
	if _, err := rig.selStart(t, owner, selAlpha, "e2e-owner"); err != nil {
		t.Fatalf("the owning tenant could not name its own agent: %v", err)
	}
}

// TestE2E_AgentSelection_ResolverStoreErrorFailsLoud is FAILURE MODE 2:
// the resolver's backing store is CLOSED, so every lookup errors. The
// request must fail loud, never fall through to the default agent.
func TestE2E_AgentSelection_ResolverStoreErrorFailsLoud(t *testing.T) {
	rig := newSelRig(t)
	id := selID("tenant-sel-err")

	// A second registry over a store we close underneath it.
	st, err := stateInmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	brokenReg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: st, Bus: rig.bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	if err := st.Close(context.Background()); err != nil {
		t.Fatalf("state.Close: %v", err)
	}

	surface, err := protocol.NewControlSurface(rig.tasks, steering.NewRegistry(),
		// defaultID deliberately EMPTY so check (i) cannot short-circuit
		// the store read this leg is about.
		protocol.WithAgentResolver(serve.NewAgentResolverAdapter(brokenReg, "")),
		protocol.WithAgentReachAuthorizer(protocolauth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	before, err := rig.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}

	_, dErr := surface.Dispatch(selCtx(t, id), methods.MethodStart, &prototypes.StartRequest{
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:    "broken store",
		AgentID:  selAlpha,
	})
	if dErr == nil {
		t.Fatal("a broken resolver store let the start through; want a loud failure, never a default fallback")
	}
	if code := selCode(t, dErr); code != protoerrors.CodeRuntimeError && code != protoerrors.CodeInvalidRequest {
		t.Fatalf("refusal code = %q, want a loud runtime/invalid-request failure", code)
	}
	after, err := rig.tasks.List(ctx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("task count %d → %d: a resolver failure MUST NOT write a task row", len(before), len(after))
	}
}

// TestE2E_AgentSelection_ConcurrentRunsDoNotCrossTalk — N≥10 concurrent
// starts across two tenants × two agents through ONE shared surface and
// ONE shared run-loop driver. Content-checked per run.
func TestE2E_AgentSelection_ConcurrentRunsDoNotCrossTalk(t *testing.T) {
	rig := newSelRig(t)
	tenants := []string{"tenant-conc-1", "tenant-conc-2"}
	specs := map[string][2]string{
		selAlpha: {"CONC-ALPHA", "conc-alpha-model"},
		selBravo: {"CONC-BRAVO", "conc-bravo-model"},
	}
	for _, ten := range tenants {
		for ag, sp := range specs {
			rig.selWriteAgent(t, selID(ten), ag, sp[0], sp[1])
		}
	}

	type want struct{ agent, tenant string }
	const n = 24
	started := make(map[string]want, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ten := tenants[i%len(tenants)]
			ag := selAlpha
			if (i/len(tenants))%2 == 1 {
				ag = selBravo
			}
			id := selID(ten)
			taskID, err := rig.selStart(t, id, ag, "")
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			started[taskID] = want{agent: ag, tenant: ten}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent start: %v", err)
	}

	for taskID, w := range started {
		seen := rig.selAwait(t, taskID)
		sp := specs[w.agent]
		if seen.basePrompt != sp[0] || seen.model != sp[1] {
			t.Errorf("run %s (%s/%s) resolved base=%q model=%q, want %q/%q — per-run agent bled",
				taskID, w.tenant, w.agent, seen.basePrompt, seen.model, sp[0], sp[1])
		}
		if seen.tenant != w.tenant {
			t.Errorf("run %s saw tenant %q, want %q — identity bled", taskID, seen.tenant, w.tenant)
		}
		// The credential plane stays boot-derived on every one of them.
		if seen.agentProvenance != selBoot {
			t.Errorf("run %s southbound provenance = %q, want the boot %q", taskID, seen.agentProvenance, selBoot)
		}
	}
}

// -----------------------------------------------------------------------
// Credential plane (Ruling A) — asserted SERVER-SIDE on the real wire.
// -----------------------------------------------------------------------

// selActorBroker is an RFC 8693 token-exchange broker fixture that
// records the `actor_token` form field of every exchange.
type selActorBroker struct {
	server *httptest.Server
	mu     sync.Mutex
	actors []string
}

func newSelActorBroker(t *testing.T) *selActorBroker {
	t.Helper()
	b := &selActorBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.actors = append(b.actors, r.Form.Get("actor_token"))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "sel-brokered-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *selActorBroker) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.actors))
	copy(out, b.actors)
	return out
}

// TestE2E_AgentSelection_CredentialPlaneStaysBootDerived drives a
// CALLER-NAMED run all the way through the real MCP transport and the
// real token-exchange broker, and asserts on the ACTUAL WIRE that both
// carry the runtime's BOOT agent id — not the caller's named one.
func TestE2E_AgentSelection_CredentialPlaneStaysBootDerived(t *testing.T) {
	rig := newSelRig(t)
	id := selID("tenant-sel-cred")
	rig.selWriteAgent(t, id, selAlpha, "CRED-BASE", "cred-model")

	// Real go-sdk MCP fixture server behind the `_meta`-recording proxy.
	mcpSrv, rec := p148MCPServer(t)
	broker := newSelActorBroker(t)

	// Real token-exchange provider WITH the actor-token opt-in — the one
	// configuration under which a caller-influenced acting principal
	// would reach an external authorization server.
	red := patternsAudit.New()
	rawState, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = rawState.Close(context.Background()) })
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i*11 + 5)
	}
	sealer, err := auth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	tokStore, err := auth.NewTokenStore(rawState, sealer)
	if err != nil {
		t.Fatalf("token store: %v", err)
	}
	flows, err := auth.NewFlowStore(rawState, sealer)
	if err != nil {
		t.Fatalf("flow store: %v", err)
	}
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   "sel-broker",
		CredentialSource:       credsource.Static("dummy-client-id-not-a-secret", "dummy-client-secret-not-a-secret"),
		TokenURL:               broker.server.URL + "/oauth2/token",
		Extra:                  map[string]string{"audience": "https://sel.example.test"},
		AllowedDownstreamHosts: []string{mcpSrv.URL},
		IncludeActorToken:      true,
	}, auth.FactoryDeps{
		Store: tokStore, Flows: flows, Bus: rig.bus, Redactor: red,
		Coordinator: pauseresume.New(),
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	p, err := mcpdrv.New(mcpdrv.Config{
		Name:            "selgraph",
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		URL:             mcpSrv.URL,
		Bus:             rig.bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   prov,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var echo tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == "selgraph_echo" {
			echo = d
		}
	}
	if echo.Invoke == nil {
		t.Fatal("echo descriptor not discovered")
	}

	// Drive the tool INSIDE the run, on the live run ctx.
	var invokeErr error
	rig.obs.invoke = func(ctx context.Context) {
		_, invokeErr = echo.Invoke(ctx, json.RawMessage(`{"text":"hi"}`))
	}

	taskID, err := rig.selStart(t, id, selAlpha, "e2e-cred")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	seen := rig.selAwait(t, taskID)
	if invokeErr != nil {
		t.Fatalf("MCP invoke inside the run: %v", invokeErr)
	}

	// The config plane DID follow the named agent — so the pin below is
	// not vacuous.
	if seen.basePrompt != "CRED-BASE" {
		t.Fatalf("config plane did not follow the named agent (base = %q)", seen.basePrompt)
	}

	// (1) Southbound MCP `_meta.agent_id`, asserted server-side.
	calls := rec.snapshot()
	if len(calls) == 0 {
		t.Fatal("the MCP fixture server saw no tools/call")
	}
	last := calls[len(calls)-1]
	if last.meta["agent_id"] != selBoot {
		t.Errorf("_meta.agent_id = %q, want the BOOT value %q — a caller-named agent must not reach southbound provenance",
			last.meta["agent_id"], selBoot)
	}
	if last.meta["tenant"] != id.TenantID {
		t.Errorf("_meta.tenant = %q, want %q", last.meta["tenant"], id.TenantID)
	}

	// (2) RFC 8693 actor_token, asserted broker-side.
	actors := broker.snapshot()
	if len(actors) == 0 {
		t.Fatal("the token-exchange broker saw no exchange — the actor-token pin would be vacuous")
	}
	for i, a := range actors {
		if a != selBoot {
			t.Errorf("exchange %d actor_token = %q, want the BOOT value %q — the acting principal is never client-supplied",
				i, a, selBoot)
		}
	}
}

// selCode extracts the stable Protocol error code.
func selCode(t *testing.T, err error) protoerrors.Code {
	t.Helper()
	var pe *protoerrors.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected a *protoerrors.Error, got %T: %v", err, err)
	}
	return pe.Code
}
