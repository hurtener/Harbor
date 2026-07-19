package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
)

// TestService_ConcurrentReuse_N100 is the mandatory D-025 concurrent-
// reuse test: N≥100 concurrent invocations of every `agents.*` method
// against a SINGLE shared *Service over a SINGLE shared real Agent
// Registry, under `-race`. It asserts:
//   - no data races (the -race detector is the gate);
//   - no context bleed (each goroutine carries its own identity ctx;
//     every successful List sees its own tenant's single agent only);
//   - no goroutine leak (baseline NumGoroutine restored after all
//     invocations return).
func TestService_ConcurrentReuse_N100(t *testing.T) {
	reg := newRealRegistry(t)
	proj, err := agentsprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	// Wire the controller so the five fleet-control verbs are exercised
	// against the shared instance too (Phase 108l / D-184 — the control
	// path is the part most likely to race on the registry's RMW mutex).
	svc, err := agentsprotocol.NewService(proj, agentsprotocol.WithController(reg))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const n = 120

	// Each goroutine gets its OWN tenant — so a List under goroutine i
	// must see exactly one agent (its own). Any cross-tenant bleed makes
	// the per-goroutine count assertion fail.
	type fixture struct {
		ctx     context.Context
		scope   prototypes.IdentityScope
		agentID string
	}
	fixtures := make([]fixture, n)
	for i := range fixtures {
		tenant := "tenant-" + itoa(i)
		ctx := idCtx(t, tenant, "u", "s")
		rec, rerr := reg.Register(ctx, "agent", registry.AgentConfig{}, registry.RegisterOptions{DisplayName: "Agent " + itoa(i)})
		if rerr != nil {
			t.Fatalf("Register %d: %v", i, rerr)
		}
		fixtures[i] = fixture{
			ctx:     ctx,
			scope:   prototypes.IdentityScope{Tenant: tenant, User: "u", Session: "s"},
			agentID: rec.AgentID,
		}
	}

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, n*8)
	wg.Add(n)
	for i := range n {
		go func(fx fixture) {
			defer wg.Done()

			// List — must see exactly this tenant's one agent.
			listResp, lerr := svc.List(fx.ctx, prototypes.AgentListRequest{Identity: fx.scope}, false)
			if lerr != nil {
				errCh <- lerr
				return
			}
			if listResp.TotalRows != 1 {
				errCh <- &countErr{got: listResp.TotalRows, want: 1, op: "List"}
				return
			}

			// Get / Tools / Memory / Governance / Skills / Permissions /
			// Metrics — each scoped to this goroutine's identity.
			if _, gerr := svc.Get(fx.ctx, prototypes.AgentGetRequest{Identity: fx.scope, ID: fx.agentID}); gerr != nil {
				errCh <- gerr
				return
			}
			if _, terr := svc.Tools(fx.ctx, prototypes.AgentToolsRequest{Identity: fx.scope, ID: fx.agentID}); terr != nil {
				errCh <- terr
				return
			}
			if _, merr := svc.Memory(fx.ctx, prototypes.AgentMemoryRequest{Identity: fx.scope, ID: fx.agentID}); merr != nil {
				errCh <- merr
				return
			}
			if _, gverr := svc.Governance(fx.ctx, prototypes.AgentGovernanceRequest{Identity: fx.scope, ID: fx.agentID}); gverr != nil {
				errCh <- gverr
				return
			}
			if _, skerr := svc.Skills(fx.ctx, prototypes.AgentSkillsRequest{Identity: fx.scope, ID: fx.agentID}); skerr != nil {
				errCh <- skerr
				return
			}
			if _, perr := svc.Permissions(fx.ctx, prototypes.AgentPermissionsRequest{Identity: fx.scope, ID: fx.agentID}); perr != nil {
				errCh <- perr
				return
			}
			mResp, mErr := svc.Metrics(fx.ctx, prototypes.AgentMetricsRequest{Identity: fx.scope})
			if mErr != nil {
				errCh <- mErr
				return
			}
			if mResp.Metrics.ActiveAgents != 1 {
				errCh <- &countErr{got: mResp.Metrics.ActiveAgents, want: 1, op: "Metrics.ActiveAgents"}
				return
			}

			// Fleet control — exercise the shared doControl + WithController
			// + registry-mutex RMW path under -race (D-184). Pause and
			// Restart are chosen because they leave the agent's status
			// `active` (the registry has no "paused" state and restart does
			// not clear Health), so the per-tenant active-agent invariant
			// above still holds; they traverse the identical shared control
			// path as drain/force_stop. controlScoped=true → doControl
			// attaches registry.WithControlScope.
			ctlReq := prototypes.AgentControlRequest{Identity: fx.scope, ID: fx.agentID}
			pResp, pErr := svc.Pause(fx.ctx, ctlReq, true)
			if pErr != nil {
				errCh <- pErr
				return
			}
			if pResp.Status != prototypes.AgentStatusActive {
				errCh <- fmt.Errorf("Pause re-read status = %q, want active (pause must not transition status)", pResp.Status)
				return
			}
			if _, rErr := svc.Restart(fx.ctx, ctlReq, true); rErr != nil {
				errCh <- rErr
				return
			}
		}(fixtures[i])
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent invocation failed: %v", err)
	}

	// Goroutine-leak gate — the baseline must be restored. Allow a brief
	// grace for the scheduler to reap finished goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: %d goroutines above baseline %d", leaked, baseline)
	}
}

// TestService_DefaultAgent_ConcurrentReuse_N128 is the D-025 concurrent-
// reuse guard for the synthetic default-agent seam: N=128 concurrent
// ListAgents / GetAgent calls, each under its OWN tenant against a SINGLE
// shared Service whose projector is configured WithDefaultAgent, under
// `-race`. It proves the synthetic row is stable per-call — the row's
// identity is derived from each call's ctx/id, never from shared mutable
// descriptor state on the artifact — with no data race, no context bleed,
// and no goroutine leak.
func TestService_DefaultAgent_ConcurrentReuse_N128(t *testing.T) {
	reg := newRealRegistry(t)
	proj, err := agentsprotocol.NewRegistryProjector(reg,
		agentsprotocol.WithDefaultAgent(agentsprotocol.DefaultAgentDescriptor{
			ID: "harbor-default-agent", DisplayName: "Harbor default agent",
		}))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := agentsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-" + itoa(i)
			ctx := idCtx(t, tenant, "u", "s")
			scope := prototypes.IdentityScope{Tenant: tenant, User: "u", Session: "s"}
			// The registry is empty for this tenant → exactly one synthetic row.
			resp, lerr := svc.List(ctx, prototypes.AgentListRequest{Identity: scope}, false)
			if lerr != nil {
				errCh <- lerr
				return
			}
			if len(resp.Agents) != 1 || !resp.Agents[0].IsDefault {
				errCh <- fmt.Errorf("tenant %s: rows=%d, want exactly 1 IsDefault row", tenant, len(resp.Agents))
				return
			}
			if resp.Agents[0].Identity.Tenant != tenant {
				errCh <- fmt.Errorf("tenant %s: synthetic row identity leaked tenant %q (context bleed)", tenant, resp.Agents[0].Identity.Tenant)
				return
			}
			if _, gerr := svc.Get(ctx, prototypes.AgentGetRequest{Identity: scope, ID: "harbor-default-agent"}); gerr != nil {
				errCh <- fmt.Errorf("tenant %s: Get(default id): %w", tenant, gerr)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent default-agent invocation failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: %d goroutines above baseline %d", leaked, baseline)
	}
}

// countErr is a typed error so the concurrent test reports a precise
// cross-talk diagnosis rather than a bare string.
type countErr struct {
	got, want int64
	op        string
}

func (e *countErr) Error() string {
	return e.op + " returned " + strconv.FormatInt(e.got, 10) +
		", want " + strconv.FormatInt(e.want, 10) + " (context bleed)"
}

func itoa(i int) string { return strconv.Itoa(i) }
