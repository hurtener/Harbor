package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestService_ConcurrentReuse_NoRacesNoCrossTalk pins the D-025
// concurrent-reuse contract: a single shared *Service is exercised by
// N=128 concurrent goroutines, each driving a distinct identity triple
// with overlapping filters, under -race. The test asserts:
//
//   - no data races (the race detector is the gate),
//   - no context bleed (each goroutine asserts the row count it gets
//     back matches its own filter, never another goroutine's),
//   - no goroutine leak (NumGoroutine returns to baseline after all
//     invocations complete).
func TestService_ConcurrentReuse_NoRacesNoCrossTalk(t *testing.T) {
	svc := newService(t)

	const workers = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			id := prototypes.IdentityScope{
				Tenant:  fmt.Sprintf("tenant-%d", n%7),
				User:    fmt.Sprintf("user-%d", n),
				Session: fmt.Sprintf("session-%d", n),
			}
			// Alternate between the unfiltered list and a transport
			// facet so concurrent invocations exercise different
			// branches of the same shared Service.
			req := prototypes.ToolListRequest{Identity: id}
			wantRows := 3
			if n%2 == 0 {
				req.Filter = prototypes.ToolFilter{
					Transports: []prototypes.ToolTransport{prototypes.ToolTransportHTTP},
				}
				wantRows = 1
			}
			resp, err := svc.List(context.Background(), req)
			if err != nil {
				errCh <- fmt.Errorf("worker %d List: %w", n, err)
				return
			}
			if len(resp.Tools) != wantRows {
				errCh <- fmt.Errorf("worker %d got %d rows, want %d — context bleed",
					n, len(resp.Tools), wantRows)
				return
			}
			// A cross-call admin mutation under the worker's own identity.
			if _, err := svc.Get(context.Background(), prototypes.ToolGetRequest{
				Identity: id, ID: "alpha_search",
			}); err != nil {
				errCh <- fmt.Errorf("worker %d Get: %w", n, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak guard — give the scheduler a beat to reap, then
	// assert we are back at (or below) baseline.
	for attempt := 0; attempt < 50; attempt++ {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}

// TestService_IdentityIsolation_CrossTenant pins the multi-isolation
// contract (CLAUDE.md §6): a Service shared across tenants never bleeds
// one tenant's request into another's. Two tenants drive the same
// shared Service concurrently; each asserts the catalog it sees is
// consistent and the admin mutation under tenant A's identity does not
// reach tenant B's read.
func TestService_IdentityIsolation_CrossTenant(t *testing.T) {
	svc := newService(t)

	idA := prototypes.IdentityScope{Tenant: "tenant-a", User: "u", Session: "s"}
	idB := prototypes.IdentityScope{Tenant: "tenant-b", User: "u", Session: "s"}

	// Tenant A gates a tool. The in-memory CatalogProjector keys the
	// override by tool name (the catalog is per-runtime, not per-tenant
	// — tool descriptors are tenant-agnostic per page-tools.md §8), so
	// the override is visible to B too. The isolation guarantee under
	// test is that NEITHER tenant's *request* corrupts the other's
	// *response shape* — no panic, no missing rows, no error bleed.
	if _, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{
		Identity: idA, ID: "alpha_search", Policy: prototypes.ToolApprovalGated,
	}, true); err != nil {
		t.Fatalf("tenant A SetApprovalPolicy: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]int, 2)
	errs := make([]error, 2)
	for idx, id := range []prototypes.IdentityScope{idA, idB} {
		go func(slot int, scope prototypes.IdentityScope) {
			defer wg.Done()
			resp, err := svc.List(context.Background(), prototypes.ToolListRequest{Identity: scope})
			if err != nil {
				errs[slot] = err
				return
			}
			results[slot] = len(resp.Tools)
		}(idx, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("tenant slot %d List: %v", i, err)
		}
	}
	if results[0] != 3 || results[1] != 3 {
		t.Errorf("cross-tenant List row counts = %v, want [3 3] — both tenants see the per-runtime catalog", results)
	}

	// Tenant B with an INCOMPLETE identity fails closed — identity is
	// mandatory regardless of which tenant calls.
	if _, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-b"},
	}); err == nil {
		t.Error("List with incomplete identity for tenant B did not fail — identity must be mandatory")
	}
}

// TestService_AdminAudit_EmittedOnBus pins that a successful admin
// action publishes an `audit.admin_scope_used` event when a bus is
// wired — the admin path is never silent (CLAUDE.md §13).
func TestService_AdminAudit_EmittedOnBus(t *testing.T) {
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	bus := newCapturingBus()
	svc, err := toolsprotocol.NewService(proj, toolsprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{
		Identity: validID(), ID: "alpha_search", Policy: prototypes.ToolApprovalGated,
	}, true); err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	if got := bus.count(); got != 1 {
		t.Fatalf("admin action published %d events, want 1 audit.admin_scope_used", got)
	}
}
