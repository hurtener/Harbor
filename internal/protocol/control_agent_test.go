// control_agent_test.go — the caller-named-agent edge validation on
// `start` (Phase 215 / D-360).
//
// The suite drives the REAL ControlSurface over the REAL task registry
// (inprocess over a real in-mem StateStore) and the REAL agent-config
// registry through the REAL production adapter
// (serve.NewAgentResolverAdapter) — no hand-rolled resolver stands in for
// the two-check rule, so a rule change in production is caught here
// (CLAUDE.md §17.3 #1).
package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// bootAgentID is the documented dummy "runtime's configured default"
// agent id these tests boot under. Check (i) accepts it WITHOUT any
// config revision — the case a registry-membership rule would refuse.
const bootAgentID = "test-boot-agent"

// agentFixture is the caller-named-agent rig: a ControlSurface wired to
// the production AgentResolver adapter over a real agent-config registry,
// plus the real task registry the spawn lands in.
type agentFixture struct {
	surface *protocol.ControlSurface
	tasks   tasks.TaskRegistry
	cfg     agentcfg.Registry
}

// newAgentFixture builds the rig. Pass wireResolver=false to build the
// UNWIRED surface (a control-only Runtime) whose fail-closed posture the
// suite pins.
func newAgentFixture(t *testing.T, wireResolver bool) *agentFixture {
	t.Helper()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: store, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	cfgReg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = cfgReg.Close(context.Background()) })

	opts := []protocol.Option{}
	if wireResolver {
		opts = append(opts,
			protocol.WithAgentResolver(serve.NewAgentResolverAdapter(cfgReg, bootAgentID)))
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(), opts...)
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	return &agentFixture{surface: surface, tasks: taskReg, cfg: cfgReg}
}

// ident is a documented dummy identity triple — no secrets.
func agentIdent(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// writeRevision pins a ConfigScopeAgent revision for (tenant, agentID),
// which is exactly what check (ii) reads back.
func (f *agentFixture) writeRevision(t *testing.T, id identity.Identity, agentID string) {
	t.Helper()
	_, err := f.cfg.SetRevision(context.Background(),
		identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"alpha"}}})
	if err != nil {
		t.Fatalf("SetRevision(%s/%s): %v", id.TenantID, agentID, err)
	}
}

// start dispatches a `start` naming agentID (empty = named none).
func (f *agentFixture) start(t *testing.T, id identity.Identity, agentID, key string) (*types.StartResponse, error) {
	t.Helper()
	resp, err := f.surface.Dispatch(authCtx(t, id), methods.MethodStart, &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "hello",
		IdempotencyKey: key,
		AgentID:        agentID,
	})
	if err != nil {
		return nil, err
	}
	sr, ok := resp.(*types.StartResponse)
	if !ok {
		t.Fatalf("start returned %T, want *types.StartResponse", resp)
	}
	return sr, nil
}

// TestDispatchStart_NamedAgent_TwoCheckRule is the validation table.
func TestDispatchStart_NamedAgent_TwoCheckRule(t *testing.T) {
	f := newAgentFixture(t, true)
	caller := agentIdent("tenant-a", "user-1", "session-x")
	f.writeRevision(t, caller, "configured-agent")

	// A revision under a DIFFERENT tenant, same agent id. Check (ii)
	// keys on the CALLER's tenant, so this must not make it resolvable.
	f.writeRevision(t, agentIdent("tenant-b", "user-9", "session-z"), "foreign-agent")

	t.Run("OmittedAgentIsAcceptedAndSpawnsWithEmptyAgentID", func(t *testing.T) {
		resp, err := f.start(t, caller, "", "omit-1")
		if err != nil {
			t.Fatalf("start with no agent_id: %v", err)
		}
		task, gErr := f.tasks.Get(mustIdentCtx(t, caller), tasks.TaskID(resp.TaskID))
		if gErr != nil {
			t.Fatalf("tasks.Get: %v", gErr)
		}
		if task.AgentID != "" {
			t.Fatalf("task.AgentID = %q, want empty (the unchanged default path)", task.AgentID)
		}
	})

	t.Run("ConfiguredDefaultIsAcceptedWithoutAnyRevision", func(t *testing.T) {
		// bootAgentID deliberately has NO revision written for it — this
		// is check (i), the case registry-membership validation refuses.
		if _, ok, err := f.cfg.Active(context.Background(),
			identity.Quadruple{Identity: caller}, bootAgentID, agentcfg.ConfigScopeAgent); err != nil || ok {
			t.Fatalf("precondition: boot agent must have NO active revision (ok=%v err=%v)", ok, err)
		}
		resp, err := f.start(t, caller, bootAgentID, "boot-1")
		if err != nil {
			t.Fatalf("start naming the configured default: %v", err)
		}
		task, gErr := f.tasks.Get(mustIdentCtx(t, caller), tasks.TaskID(resp.TaskID))
		if gErr != nil {
			t.Fatalf("tasks.Get: %v", gErr)
		}
		if task.AgentID != bootAgentID {
			t.Fatalf("task.AgentID = %q, want %q", task.AgentID, bootAgentID)
		}
	})

	t.Run("AgentWithARevisionIsAccepted", func(t *testing.T) {
		resp, err := f.start(t, caller, "configured-agent", "cfg-1")
		if err != nil {
			t.Fatalf("start naming a configured agent: %v", err)
		}
		task, gErr := f.tasks.Get(mustIdentCtx(t, caller), tasks.TaskID(resp.TaskID))
		if gErr != nil {
			t.Fatalf("tasks.Get: %v", gErr)
		}
		if task.AgentID != "configured-agent" {
			t.Fatalf("task.AgentID = %q, want configured-agent", task.AgentID)
		}
	})

	t.Run("UnknownAgentIsRefusedAndNoTaskIsCreated", func(t *testing.T) {
		before := countTasks(t, f.tasks, caller)
		_, err := f.start(t, caller, "no-such-agent", "unknown-1")
		if err == nil {
			t.Fatal("start naming an unknown agent succeeded, want CodeInvalidRequest")
		}
		if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
			t.Fatalf("error code = %q, want %q", got, protoerrors.CodeInvalidRequest)
		}
		if after := countTasks(t, f.tasks, caller); after != before {
			t.Fatalf("task count %d → %d: a refused start MUST NOT create a task", before, after)
		}
	})

	t.Run("ForeignTenantAgentRefusalIsByteIdenticalToUnknown", func(t *testing.T) {
		_, unknownErr := f.start(t, caller, "no-such-agent", "oracle-1")
		_, foreignErr := f.start(t, caller, "foreign-agent", "oracle-2")
		if unknownErr == nil || foreignErr == nil {
			t.Fatalf("both refusals expected, got unknown=%v foreign=%v", unknownErr, foreignErr)
		}
		if unknownErr.Error() != foreignErr.Error() {
			t.Fatalf("the edge is a cross-tenant existence oracle:\n unknown = %q\n foreign = %q",
				unknownErr.Error(), foreignErr.Error())
		}
		// And the foreign tenant's OWN start under that id succeeds —
		// proving the refusal was tenant scoping, not a bad revision.
		foreign := agentIdent("tenant-b", "user-9", "session-z")
		if _, err := f.start(t, foreign, "foreign-agent", "oracle-3"); err != nil {
			t.Fatalf("the owning tenant must be able to name its own agent: %v", err)
		}
	})
}

// TestDispatchStart_NamedAgent_UnwiredSurfaceFailsClosed pins the §13
// posture: a surface with NO resolver refuses a named agent rather than
// ignoring it — deliberately NOT the SessionEnsurer's skip-if-absent
// shape, which would silently accept any id and hand it to a driver.
func TestDispatchStart_NamedAgent_UnwiredSurfaceFailsClosed(t *testing.T) {
	f := newAgentFixture(t, false)
	caller := agentIdent("tenant-a", "user-1", "session-x")

	if _, err := f.start(t, caller, bootAgentID, "unwired-1"); err == nil {
		t.Fatal("a named agent against a resolver-less surface was ACCEPTED; want refusal")
	} else if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("error code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}

	// An OMITTED agent_id against the same surface is byte-identical to
	// a Runtime that never had the seam.
	if _, err := f.start(t, caller, "", "unwired-2"); err != nil {
		t.Fatalf("omitted agent_id against a resolver-less surface: %v", err)
	}
}

// errResolver fails every lookup — the "the backing store is down"
// failure mode. A resolve error must FAIL the request loud, never fall
// through to the default agent (CLAUDE.md §13).
type errResolver struct{}

func (errResolver) ResolveAgent(context.Context, identity.Identity, string) (bool, error) {
	return false, errors.New("agent-config store unavailable")
}

func TestDispatchStart_NamedAgent_ResolverErrorFailsLoud(t *testing.T) {
	f := newAgentFixture(t, false)
	surface, err := protocol.NewControlSurface(f.tasks, steering.NewRegistry(),
		protocol.WithAgentResolver(errResolver{}))
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	caller := agentIdent("tenant-a", "user-1", "session-x")
	before := countTasks(t, f.tasks, caller)

	_, dErr := surface.Dispatch(authCtx(t, caller), methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Query:    "hello",
		AgentID:  bootAgentID,
	})
	if dErr == nil {
		t.Fatal("a resolver error was swallowed and the start succeeded; want CodeRuntimeError")
	}
	if got := codeOf(t, dErr); got != protoerrors.CodeRuntimeError {
		t.Fatalf("error code = %q, want %q", got, protoerrors.CodeRuntimeError)
	}
	if after := countTasks(t, f.tasks, caller); after != before {
		t.Fatalf("task count %d → %d: a resolver error MUST NOT create a task", before, after)
	}
}

// TestDispatchStart_NamedAgent_IdempotencyKeyIsAgentSensitive — the
// caller-named agent is part of the task's content identity, so a reused
// key naming a DIFFERENT agent is a loud conflict rather than a silent
// adoption of the original task's agent.
func TestDispatchStart_NamedAgent_IdempotencyKeyIsAgentSensitive(t *testing.T) {
	f := newAgentFixture(t, true)
	caller := agentIdent("tenant-a", "user-1", "session-x")
	f.writeRevision(t, caller, "agent-one")
	f.writeRevision(t, caller, "agent-two")

	first, err := f.start(t, caller, "agent-one", "same-key")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Same key, same agent → a genuine retry returns the same handle.
	again, err := f.start(t, caller, "agent-one", "same-key")
	if err != nil {
		t.Fatalf("retry with the same agent: %v", err)
	}
	if again.TaskID != first.TaskID {
		t.Fatalf("retry minted a new task %q (want the existing %q)", again.TaskID, first.TaskID)
	}
	if !again.Reused {
		t.Fatal("retry did not report Reused=true")
	}
	// Same key, DIFFERENT agent → loud conflict.
	if _, err := f.start(t, caller, "agent-two", "same-key"); err == nil {
		t.Fatal("a reused idempotency key naming a different agent was accepted; want a conflict")
	}
}

// TestDispatchStart_NamedAgent_ConcurrentReuse is the D-025 run: N≥128
// concurrent starts across two agents × two tenants against ONE shared
// ControlSurface. The bug it guards is a per-run agent id parked on the
// shared surface — so the assertion is content-checked PER GOROUTINE
// (each start's task must carry ITS OWN agent), not merely "no panic".
func TestDispatchStart_NamedAgent_ConcurrentReuse(t *testing.T) {
	f := newAgentFixture(t, true)

	tenants := []string{"tenant-a", "tenant-b"}
	agents := []string{"agent-red", "agent-blue"}
	for _, ten := range tenants {
		for _, ag := range agents {
			f.writeRevision(t, agentIdent(ten, "user-1", "session-x"), ag)
		}
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	const n = 128
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ten := tenants[i%len(tenants)]
			ag := agents[(i/len(tenants))%len(agents)]
			caller := agentIdent(ten, "user-1", "session-x")
			resp, err := f.start(t, caller, ag, fmt.Sprintf("conc-%d", i))
			if err != nil {
				errCh <- fmt.Errorf("start %d (%s/%s): %w", i, ten, ag, err)
				return
			}
			task, gErr := f.tasks.Get(mustIdentCtx(t, caller), tasks.TaskID(resp.TaskID))
			if gErr != nil {
				errCh <- fmt.Errorf("get %d: %w", i, gErr)
				return
			}
			// Content check: run i resolved ITS OWN agent and ITS OWN tenant.
			if task.AgentID != ag {
				errCh <- fmt.Errorf("run %d: task.AgentID = %q, want %q (per-run agent bled)", i, task.AgentID, ag)
			}
			if task.Identity.TenantID != ten {
				errCh <- fmt.Errorf("run %d: tenant = %q, want %q (identity bled)", i, task.Identity.TenantID, ten)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Cancellation cross-talk: cancelling one start's ctx must not
	// disturb another's. The cancelled call is rejected or refused; the
	// live one still resolves its own agent.
	cancelled, cancel := context.WithCancel(authCtx(t, agentIdent("tenant-a", "user-1", "session-x")))
	cancel()
	_, _ = f.surface.Dispatch(cancelled, methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"},
		Query:    "cancelled",
		AgentID:  "agent-red",
	})
	live := agentIdent("tenant-b", "user-1", "session-x")
	resp, err := f.start(t, live, "agent-blue", "post-cancel")
	if err != nil {
		t.Fatalf("a cancelled sibling disturbed a live start: %v", err)
	}
	task, gErr := f.tasks.Get(mustIdentCtx(t, live), tasks.TaskID(resp.TaskID))
	if gErr != nil {
		t.Fatalf("tasks.Get: %v", gErr)
	}
	if task.AgentID != "agent-blue" {
		t.Fatalf("post-cancel task.AgentID = %q, want agent-blue", task.AgentID)
	}

	waitGoroutineBaseline(t, baseline)
}

// mustIdentCtx builds an identity-scoped ctx for a registry read.
func mustIdentCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// countTasks returns how many tasks the identity currently owns.
func countTasks(t *testing.T, reg tasks.TaskRegistry, id identity.Identity) int {
	t.Helper()
	list, err := reg.List(mustIdentCtx(t, id), id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	return len(list)
}

// waitGoroutineBaseline asserts the goroutine count returns to (roughly)
// its pre-test baseline, with a bounded real-time wait rather than a
// sleep-as-synchronisation (CLAUDE.md §17.4).
func waitGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		got := runtime.NumGoroutine()
		if got <= baseline+8 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count %d did not return to baseline %d (+8) — leak", got, baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
