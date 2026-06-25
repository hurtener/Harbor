package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

const upAgent = "agent-policy"

func upToolCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	for _, n := range []string{"srvX_one", "srvX_two", "srvY_three"} {
		name, source := n, tools.ToolSourceID("srvX")
		if n == "srvY_three" {
			source = "srvY"
		}
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return cat
}

func upViewHas(v tools.PlannerCatalogView, name string) bool {
	for _, tl := range v.List() {
		if tl.Name == name {
			return true
		}
	}
	return false
}

// TestE2E_Phase126c_UserToolPolicyShapesRuns is the §13 producer→consumer
// round-trip for the durable user-scope tool policy: a disable set written
// through 126a's real verb (Service.UserSetRevision, ConfigScopeUser) shapes
// the user's runs at run start — across sessions — and is isolated per user.
func TestE2E_Phase126c_UserToolPolicyShapesRuns(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	cat := upToolCatalog(t)

	view := func(tenant, user, session string) tools.PlannerCatalogView {
		t.Helper()
		id := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session}}
		v, verr := projection.ActivePlannerCatalogView(ctx, reg, nil, upAgent, id,
			cat, tools.CatalogFilter{TenantID: tenant, UserID: user, SessionID: session})
		if verr != nil {
			t.Fatalf("projection (%s/%s/%s): %v", tenant, user, session, verr)
		}
		return v
	}

	// PRODUCER — alice disables srvX_one through the real 126a user verb, under
	// session sX.
	if _, err := svc.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "alice", Session: "sX"},
		AgentID:  upAgent,
		Payload:  prototypes.AgentConfigUserPayload{DisabledTools: []string{"srvX_one"}},
	}); err != nil {
		t.Fatalf("UserSetRevision: %v", err)
	}

	// PERSISTENCE ACROSS SESSIONS — alice's DIFFERENT session sY inherits the
	// disable (the ConfigScopeUser key zeroes the session, so it spans her
	// sessions for the agent).
	vY := view("t", "alice", "sY")
	if upViewHas(vY, "srvX_one") {
		t.Fatalf("durable user disable did not span alice's sessions: %v", names(vY))
	}
	if !upViewHas(vY, "srvX_two") || !upViewHas(vY, "srvY_three") {
		t.Fatalf("user disable over-excluded: %v", names(vY))
	}

	// CROSS-USER ISOLATION — bob (same agent_id) is unaffected.
	vBob := view("t", "bob", "sB")
	if !upViewHas(vBob, "srvX_one") {
		t.Fatalf("cross-user bleed: alice's disable reached bob's run: %v", names(vBob))
	}

	// CROSS-TENANT ISOLATION — same user id in a different tenant is unaffected.
	vT2 := view("t2", "alice", "sX")
	if !upViewHas(vT2, "srvX_one") {
		t.Fatalf("cross-tenant bleed: alice@t's disable reached alice@t2: %v", names(vT2))
	}

	// FAILURE MODE — a run reaching the projection with an incomplete identity
	// fails closed (never the unfiltered view).
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice"}} // no session
	if _, verr := projection.ActivePlannerCatalogView(ctx, reg, nil, upAgent, bad,
		cat, tools.CatalogFilter{TenantID: "t", UserID: "alice"}); verr == nil {
		t.Fatal("incomplete identity must fail the projection closed, got nil")
	}

	// CONCURRENCY — N>=10 distinct users run concurrently against the same
	// agent; each sees only its own policy, no cross-talk, no goroutine leak.
	runtime.GC()
	baseline := runtime.NumGoroutine()
	const n = 16
	// half disable srvX_two for themselves; assert each only affects itself.
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		user := fmt.Sprintf("u%d", i)
		if i%2 == 0 {
			if _, err := svc.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
				Identity: prototypes.IdentityScope{Tenant: "t", User: user, Session: "s"},
				AgentID:  upAgent,
				Payload:  prototypes.AgentConfigUserPayload{DisabledTools: []string{"srvX_two"}},
			}); err != nil {
				t.Fatalf("seed %s: %v", user, err)
			}
		}
	}
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d", i)
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: user, SessionID: "s2"}}
			v, verr := projection.ActivePlannerCatalogView(ctx, reg, nil, upAgent, id,
				cat, tools.CatalogFilter{TenantID: "t", UserID: user, SessionID: "s2"})
			if verr != nil {
				errCh <- fmt.Errorf("%s: %w", user, verr)
				return
			}
			disabled := !upViewHas(v, "srvX_two")
			if disabled != (i%2 == 0) {
				errCh <- fmt.Errorf("%s: srvX_two disabled=%v, want %v (cross-user bleed)", user, disabled, i%2 == 0)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d", baseline, runtime.NumGoroutine())
	}
}

func names(v tools.PlannerCatalogView) []string {
	out := make([]string, 0)
	for _, tl := range v.List() {
		out = append(out, tl.Name)
	}
	return out
}
