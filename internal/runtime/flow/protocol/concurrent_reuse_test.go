package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
)

// TestSurface_ConcurrentReuse_NoCrossTalk pins the D-025 concurrent-reuse
// contract for the Flows-page Surface: N≥100 concurrent invocations
// against ONE shared Surface (backed by the real RegistryCatalog +
// FuncInvoker) under -race, asserting no data race, no context bleed
// (each goroutine's identity-scoped result stays its own), and no
// goroutine leak after every invocation returns.
func TestSurface_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	registry := flow.NewRegistry()
	// Register a handful of flows + per-tenant run history.
	const tenants = 8
	for f := 0; f < 4; f++ {
		name := fmt.Sprintf("flow-%d", f)
		if err := registry.Register(catFixtureDef(name), flow.Metadata{
			Owner: "team", PlannerFamily: "graph", Source: "internal/flows/" + name + ".go",
		}); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
		for ti := 0; ti < tenants; ti++ {
			for i := 0; i < 3; i++ {
				if err := registry.RecordRun(flow.RunRecord{
					FlowName:  name,
					RunID:     fmt.Sprintf("%s-t%d-r%d", name, ti, i),
					Identity:  identity.Identity{TenantID: fmt.Sprintf("t%d", ti), UserID: "u1", SessionID: "s1"},
					Status:    "succeeded",
					Trigger:   "user",
					StartedAt: time.Now().Add(-time.Duration(i+1) * time.Minute),
					Duration:  100 * time.Millisecond,
				}); err != nil {
					t.Fatalf("RecordRun: %v", err)
				}
			}
		}
	}

	cat, err := flowprotocol.NewRegistryCatalog(registry, newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	launch := func(_ context.Context, id identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
		return fmt.Sprintf("run-%s-%s", flowID, id.TenantID), time.Now(), nil
	}
	inv, err := flowprotocol.NewFuncInvoker(launch, registry)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	surface, err := flowprotocol.NewSurface(cat, inv)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	baseline := runtime.NumGoroutine()

	const goroutines = 240
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("t%d", g%tenants)
			id := prototypes.IdentityScope{Tenant: tenant, User: "u1", Session: "s1"}
			ctx := context.Background()

			// flows.list — non-admin: every returned flow's run aggregate
			// must be the caller-tenant slice (no context bleed).
			listResp, lerr := surface.List(ctx, prototypes.FlowListRequest{Identity: id}, false)
			if lerr != nil {
				errCh <- fmt.Errorf("List: %w", lerr)
				return
			}
			if len(listResp.Flows) != 4 {
				errCh <- fmt.Errorf("List: got %d flows, want 4", len(listResp.Flows))
				return
			}
			for _, fl := range listResp.Flows {
				if fl.Runs24h != 3 {
					errCh <- fmt.Errorf("List: flow %s Runs24h = %d, want 3 (own-tenant)", fl.ID, fl.Runs24h)
					return
				}
			}

			// flows.describe.
			if _, derr := surface.Describe(ctx, prototypes.FlowDescribeRequest{
				Identity: id, ID: fmt.Sprintf("flow-%d", g%4),
			}, false); derr != nil {
				errCh <- fmt.Errorf("Describe: %w", derr)
				return
			}

			// flows.runs.list — non-admin: every run must be caller-tenant.
			rl, rerr := surface.RunsList(ctx, prototypes.FlowRunsListRequest{
				Identity: id, FlowID: fmt.Sprintf("flow-%d", g%4),
			}, false)
			if rerr != nil {
				errCh <- fmt.Errorf("RunsList: %w", rerr)
				return
			}
			for _, run := range rl.Runs {
				if run.Identity.Tenant != tenant {
					errCh <- fmt.Errorf("RunsList: context bleed — got tenant %q, want %q", run.Identity.Tenant, tenant)
					return
				}
			}

			// flows.run — admin scope.
			runResp, runErr := surface.Run(ctx, prototypes.FlowRunRequest{
				Identity: id, FlowID: fmt.Sprintf("flow-%d", g%4),
			}, true)
			if runErr != nil {
				errCh <- fmt.Errorf("Run: %w", runErr)
				return
			}
			wantRunID := fmt.Sprintf("run-flow-%d-%s", g%4, tenant)
			if runResp.RunID != wantRunID {
				errCh <- fmt.Errorf("Run: context bleed — RunID = %q, want %q", runResp.RunID, wantRunID)
				return
			}

			// flows.metrics.
			if _, merr := surface.Metrics(ctx, prototypes.FlowMetricsRequest{
				Identity: id, FlowID: fmt.Sprintf("flow-%d", g%4),
			}, false); merr != nil {
				errCh <- fmt.Errorf("Metrics: %w", merr)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak check: allow a brief settle, then assert the count
	// returns to (near) baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 5 {
		t.Errorf("goroutine leak: %d goroutines above baseline %d", leaked, baseline)
	}
}
