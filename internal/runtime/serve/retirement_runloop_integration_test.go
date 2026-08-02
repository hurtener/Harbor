package serve

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/tasks"
)

type retirementBlockingPlanner struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (p *retirementBlockingPlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.enteredOnce.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return planner.Finish{Reason: planner.FinishGoal}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type retirementRunDetacher struct{ calls atomic.Int64 }

func (d *retirementRunDetacher) DetachConnection(context.Context, string, string, string) error {
	d.calls.Add(1)
	return nil
}

func TestRunLoopDriver_RetirementTombstonesNewStartsAndDrainsAdmittedRunBeforeCleanup(t *testing.T) {
	const agentID = "retirement-run-agent"
	q := identity.Quadruple{Identity: runLoopDriverTestID}
	cfgReg := acTestRegistry(t)
	revision, err := cfgReg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
			Name: "retirement-run-mcp", Transport: agentcfg.MCPTransportHTTP, URL: "https://run.example.test",
		}}},
	}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	env := newFailDriverEnv(t)
	gate := runsnapshot.NewGate()
	plannerProbe := &retirementBlockingPlanner{entered: make(chan struct{}), release: make(chan struct{})}
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = plannerProbe
		o.AgentConfig = cfgReg
		o.AgentConfigID = agentID
		o.RunSnapshots = gate
	})
	firstTask := spawnOn(t, env.reg, nil)
	select {
	case <-plannerProbe.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("admitted run never reached planner")
	}

	detacher := &retirementRunDetacher{}
	svc, err := agentcfgprotocol.NewService(cfgReg,
		agentcfgprotocol.WithConnectionDetacher(detacher),
		agentcfgprotocol.WithRunSnapshotGate(gate))
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigRetireRequest{
		Identity: prototypes.IdentityScope{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID},
		AgentID:  agentID, OperationID: "retire-admitted-run", ExpectedContentHash: revision.ContentHash,
	}
	type retireResult struct {
		response prototypes.AgentConfigRetireResponse
		err      error
	}
	retired := make(chan retireResult, 1)
	go func() {
		response, retireErr := svc.Retire(t.Context(), req)
		retired <- retireResult{response: response, err: retireErr}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		status, found, statusErr := cfgReg.(agentcfg.RetirementRegistry).RetirementStatus(t.Context(), q, agentID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if found {
			if status.OperationID != req.OperationID || status.Completed {
				t.Fatalf("tombstone while run held = %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retirement tombstone was not installed while admitted run was held")
		}
		runtime.Gosched()
	}
	if detacher.calls.Load() != 0 {
		t.Fatalf("cleanup ran while admitted run was held: calls=%d", detacher.calls.Load())
	}
	select {
	case result := <-retired:
		t.Fatalf("retirement returned before admitted run drained: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}

	secondTask := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, secondTask, tasks.StatusFailed, 5*time.Second); status != tasks.StatusFailed {
		t.Fatalf("post-tombstone task status=%s, want failed", status)
	}
	ctx, err := identity.With(t.Context(), runLoopDriverTestID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.reg.Get(ctx, secondTask)
	if err != nil {
		t.Fatal(err)
	}
	if second.Error == nil || second.Error.Code != "agent_retired" {
		t.Fatalf("post-tombstone task error=%+v, want agent_retired", second.Error)
	}

	close(plannerProbe.release)
	if status := waitForTaskStatus(t, env.reg, firstTask, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("admitted task status=%s, want complete", status)
	}
	select {
	case result := <-retired:
		if result.err != nil || !result.response.Status.Completed {
			t.Fatalf("retirement after run drain=(%+v,%v)", result.response, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not resume cleanup after admitted run completed")
	}
	if detacher.calls.Load() != 1 {
		t.Fatalf("cleanup calls=%d, want one after run drain", detacher.calls.Load())
	}
}
