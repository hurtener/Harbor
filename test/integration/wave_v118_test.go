// wave_v118_test.go — the v1.18 wave-end E2E (CLAUDE.md §17.7 step 5).
//
// It spans the wave's shipped surfaces end-to-end over REAL drivers (the
// patterns audit redactor + inmem event bus + inmem state store + the real
// tokenexchange broker-pull provider + the real MCP driver + the real
// runtime-add attacher + the real agentcfg protocol service):
//
//   - HA-33 (D-339): a same-name re-attach on a running runtime is an
//     idempotent upsert (same owner), and a cross-owner same-name add is
//     rejected loud (isolation);
//   - HA-32 (D-340/D-303): with the fail-closed wire-OAuth opt-in OFF, a
//     set_oauth_provider carrying a sink-bearing wire descriptor is rejected;
//   - HA-34 (D-341): per-user credential injection sources + injects a per-user
//     credential for a receiver-style server, the audit redactor holds every
//     injected form to `***`, and two acting users get isolated values.
//
// Identity propagation (the multi-isolation triple) is asserted on every leg. A
// concurrency-stress subtest runs N≥10 interleaved per-user injection calls
// across ≥2 tenants against a SINGLE shared connection, asserting per-user value
// isolation (no cross-user bleed) and no goroutine leak. Runs under -race.
package integration

import (
	"context"
	"encoding/json"
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
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgproto "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	stateInmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// --- HA-33: idempotent re-attach + cross-owner isolation ---------------------

func TestE2E_WaveV118_HA33_ReAttachIdempotentAndOwnerIsolated(t *testing.T) {
	t.Parallel()
	hs, _ := p200MCPServer(t)
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	sysID := identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}
	attacher := serve.NewMCPConnectionAttacher(cat, reg, bus, nil, sysID, nil, nil, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })

	req := func(owner string) agentcfgproto.AttachRequest {
		return agentcfgproto.AttachRequest{
			Identity: sysID, AgentID: owner, Name: "recv",
			Transport: agentcfg.MCPTransportHTTP, URL: hs.URL,
		}
	}
	// First attach + an idempotent same-owner re-attach: both succeed, the tool
	// set is live after each.
	if err := attacher.Attach(context.Background(), req("agent-owner")); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if err := attacher.Attach(context.Background(), req("agent-owner")); err != nil {
		t.Fatalf("idempotent re-attach: %v", err)
	}
	if _, ok := cat.Resolve("recv_echo"); !ok {
		t.Fatal("recv_echo not live after re-attach")
	}
	// A cross-owner same-name add is rejected loud (never tears down another
	// owner's live registration).
	err = attacher.Attach(context.Background(), req("agent-other"))
	if err == nil {
		t.Fatal("cross-owner same-name attach succeeded — an isolation breach")
	}
	if !errors.Is(err, mcpdrv.ErrConnectionNameOwnerConflict) {
		t.Fatalf("cross-owner attach: want ErrConnectionNameOwnerConflict, got %v", err)
	}
}

// --- HA-32: wire OAuth descriptor rejected with the opt-in OFF ---------------

func TestE2E_WaveV118_HA32_WireDescriptorRejectedWhenOptInOff(t *testing.T) {
	t.Parallel()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	st, err := stateInmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	// Opt-in OFF (the D-303 production posture).
	svc, err := agentcfgproto.NewService(reg, agentcfgproto.WithAllowWireOAuthDescriptor(false))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.SetOAuthProvider(context.Background(), prototypes.AgentConfigSetOAuthProviderRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-A", User: "u", Session: "s"},
		AgentID:  "agent-v118",
		Provider: prototypes.AgentConfigOAuthProviderDescriptor{
			Name: "wp", Driver: "tokenexchange", CredentialSource: "remote",
			CredentialBroker: "boot-broker",
			TokenURL:         "https://broker.example.test/token", // the sink-bearing wire field
			Audience:         "https://graph.microsoft.com", Scopes: []string{"mail.read"},
		},
	})
	if err == nil {
		t.Fatal("wire descriptor accepted with the opt-in OFF — the D-303 posture is breached")
	}
	if !errors.Is(err, agentcfgproto.ErrWireDescriptorNotAllowed) {
		t.Fatalf("want ErrWireDescriptorNotAllowed, got %v", err)
	}
}

// --- HA-34: per-user injection + redaction + isolation + stress --------------

func TestE2E_WaveV118_HA34_InjectionRedactionIsolation(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:   mcpdrv.InjectionFormHeader,
		Header: "x-vendor-api-key",
	})
	// Two acting users on ONE shared connection get two DISTINCT injected values;
	// the audit redactor holds each to `***`.
	for _, id := range []identity.Identity{
		{TenantID: "t-a", UserID: "amy", SessionID: "s"},
		{TenantID: "t-b", UserID: "ben", SessionID: "s"},
	} {
		if _, err := echo.Invoke(p148Ctx(t, id, "agent-v118"), json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("Invoke(%s): %v", id.UserID, err)
		}
	}
	want := map[string]string{"amy": "brokered-t-a-amy", "ben": "brokered-t-b-ben"}
	for _, c := range rec.snapshot() {
		user, _ := c.meta["user"].(string)
		exp, ok := want[user]
		if !ok {
			t.Fatalf("unexpected user in _meta: %+v", c.meta)
		}
		if got := c.headers["X-Vendor-Api-Key"]; got != exp {
			t.Fatalf("user %s: injected header %q, want %q (cross-user bleed)", user, got, exp)
		}
		redHeaders := redactedPayload(t, c)["headers"].(map[string]any)
		if redHeaders["X-Vendor-Api-Key"] != "***" {
			t.Fatalf("injected credential not redacted for %s: %v", user, redHeaders["X-Vendor-Api-Key"])
		}
	}
}

// TestE2E_WaveV118_HA34_ConcurrencyStress runs N≥10 interleaved per-user
// injection calls across ≥2 tenants against ONE shared connection, asserting
// each identity's request carried its OWN credential (no bleed) and no goroutine
// leak after teardown. Under -race.
func TestE2E_WaveV118_HA34_ConcurrencyStress(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:   mcpdrv.InjectionFormHeader,
		Header: "x-vendor-api-key",
	})
	// Warm the connection (a first call establishes the SDK session's background
	// goroutines) before sampling the baseline, so the leak check isolates the
	// PER-CALL goroutines the N-way stress spawns from the fixture's own.
	if _, err := echo.Invoke(p148Ctx(t, identity.Identity{TenantID: "t-0", UserID: "warm", SessionID: "s"}, "agent-x"), json.RawMessage(`{"text":"warm"}`)); err != nil {
		t.Fatalf("warm invoke: %v", err)
	}
	runtime.GC()
	base := runtime.NumGoroutine()
	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i%2)
			user := fmt.Sprintf("u-%d", i%5)
			id := identity.Identity{TenantID: tenant, UserID: user, SessionID: "s"}
			if _, err := echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err != nil {
				t.Errorf("Invoke(%s/%s): %v", tenant, user, err)
			}
		}(i)
	}
	wg.Wait()

	calls := rec.snapshot()
	if len(calls) != n+1 { // +1 for the warm-up call
		t.Fatalf("server saw %d calls, want %d", len(calls), n+1)
	}
	for _, c := range calls {
		tenant, _ := c.meta["tenant"].(string)
		user, _ := c.meta["user"].(string)
		want := "brokered-" + tenant + "-" + user
		if got := c.headers["X-Vendor-Api-Key"]; got != want {
			t.Fatalf("cross-user bleed: %s/%s got %q want %q", tenant, user, got, want)
		}
	}

	// Background subscribers on the shared test server (the notifications
	// consume-loops) can still be spinning up when the stress goroutines join,
	// so the count is transiently above baseline right after wg.Wait(). Poll
	// until it settles within tolerance rather than sampling a single instant
	// (a bounded real-time eventually-assertion, §17.4 — not a leak if it drains).
	const tol = 10
	deadline := time.Now().Add(3 * time.Second)
	var got int
	for {
		runtime.GC()
		got = runtime.NumGoroutine()
		if got <= base+tol || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got > base+tol {
		t.Errorf("goroutine leak: base=%d after=%d (did not drain within 3s)", base, got)
	}
}

// interface assertions the wave binds against (compile-time; keeps the wave test
// honest that these production symbols exist as used).
var (
	_ = serve.NewMCPConnectionAttacher
	_ = toolauth.OAuthProvider(nil)
)
