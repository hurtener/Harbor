package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcp_attacher_reattach_test.go — the RUN-START ATTACH leg (phase 216, D-361)
// through the PRODUCTION attacher: the gates re-applied against current boot
// policy, the credential-plane neutrality asserted NEGATIVELY, the closed
// failure-class set, the bounded per-connection ctx, the bounded retry window,
// the under-lock idempotency re-check, and the two-owner concurrent-reuse run.
//
// The MCP fixture is spec-derived (the official go-sdk's server + streamable-HTTP
// handler), never a hand-authored transcript — a hand fixture cannot tell a
// right-field implementation from a wrong-field one (CLAUDE.md §17.8).

const (
	reTenant = "tenant-re"
	reAgent  = "agent-re"
)

func reOwner() toolauth.Owner { return toolauth.Owner{Tenant: reTenant, Agent: reAgent} }

func reQuad() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: reTenant, UserID: "user-re", SessionID: "sess-re"},
		RunID:    "run-re-1",
	}
}

// reAttacherFor builds a production attacher over a fresh catalog + registry,
// with the supplied gates, and returns it plus the collaborators the assertions
// read.
func reAttacherFor(t *testing.T, opts ...MCPAttacherOption) (*MCPConnectionAttacher, tools.ToolCatalog, *mcpdrv.Registry, events.EventBus) {
	t.Helper()
	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil, nil, opts...)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, cat, reg, bus
}

// reHTTPDesc is a declared http descriptor pointing at a fixture URL.
func reHTTPDesc(name, url string) agentcfg.MCPConnectionDescriptor {
	return agentcfg.MCPConnectionDescriptor{Name: name, Transport: agentcfg.MCPTransportHTTP, URL: url}
}

// reEvents subscribes to the bus and collects the re-attach lifecycle events an
// assertion reads. It drains in the background and is drained again on stop.
type reEvents struct {
	mu   sync.Mutex
	seen []events.Event
}

func newREvents(t *testing.T, bus events.EventBus, q identity.Quadruple) *reEvents {
	t.Helper()
	c := &reEvents{}
	sub, err := bus.Subscribe(context.Background(), events.Filter{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub.Events() {
			c.mu.Lock()
			c.seen = append(c.seen, ev)
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		sub.Cancel()
		<-done
	})
	return c
}

// waitFor polls until pred is satisfied or the bounded deadline expires. Bus
// delivery is asynchronous; this is an eventually-assertion with a real-time
// bound, never a sleep-as-synchronisation (CLAUDE.md §17.4).
func (c *reEvents) waitFor(t *testing.T, what string, pred func([]events.Event) bool) []events.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		snap := append([]events.Event(nil), c.seen...)
		c.mu.Unlock()
		if pred(snap) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	snap := append([]events.Event(nil), c.seen...)
	c.mu.Unlock()
	t.Fatalf("timed out waiting for %s; saw %d events: %v", what, len(snap), eventTypes(snap))
	return nil
}

func eventTypes(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, string(e.Type))
	}
	return out
}

// reattachPayloads returns the lifecycle payloads of every event of the given
// type.
func reattachPayloads(evs []events.Event, typ events.EventType) []agentcfg.MCPConnectionLifecyclePayload {
	var out []agentcfg.MCPConnectionLifecyclePayload
	for _, e := range evs {
		if e.Type != typ {
			continue
		}
		if p, ok := e.Payload.(agentcfg.MCPConnectionLifecyclePayload); ok {
			out = append(out, p)
		}
	}
	return out
}

func countType(evs []events.Event, typ events.EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// --- The gates, re-applied against CURRENT boot policy ------------------------

// TestMCPConnectionAttacher_Reattach_StdioReGatedAgainstCurrentAllowlist — a
// declared stdio descriptor whose argv[0] is absent from the allowlist in force
// NOW is refused, even though it was allowlisted when the revision was written.
// The empty (unthreaded) allowlist refuses every stdio re-attach.
func TestMCPConnectionAttacher_Reattach_StdioReGatedAgainstCurrentAllowlist(t *testing.T) {
	desc := agentcfg.MCPConnectionDescriptor{
		Name:      "stdio-srv",
		Transport: agentcfg.MCPTransportStdio,
		Command:   []string{"/usr/bin/definitely-not-allowlisted", "--serve"},
	}

	t.Run("tightened allowlist refuses", func(t *testing.T) {
		// The allowlist no longer carries the declared command.
		a, cat, reg, bus := reAttacherFor(t, WithReattachGates([]string{"/usr/bin/some-other-server"}, false))
		evs := newREvents(t, bus, reQuad())

		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
		if !errors.Is(err, ErrReattachStdioNotAllowed) {
			t.Fatalf("err = %v, want ErrReattachStdioNotAllowed", err)
		}
		// Nothing was spawned and nothing half-registered.
		if _, exists := reg.OwnerOf(desc.Name); exists {
			t.Fatal("a refused stdio re-attach must never register the server")
		}
		if _, ok := cat.Resolve(desc.Name + "_anything"); ok {
			t.Fatal("a refused stdio re-attach must never register catalog tools")
		}
		seen := evs.waitFor(t, "the reattach_failed event", func(e []events.Event) bool {
			return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) == 1
		})
		p := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0]
		if p.State != agentcfg.MCPReattachClassStdioNotAllowed {
			t.Fatalf("class = %q, want %q", p.State, agentcfg.MCPReattachClassStdioNotAllowed)
		}
	})

	t.Run("empty allowlist refuses every stdio re-attach", func(t *testing.T) {
		// No gates threaded at all — the fail-closed default.
		a, _, reg, _ := reAttacherFor(t)
		if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); !errors.Is(err, ErrReattachStdioNotAllowed) {
			t.Fatalf("err = %v, want ErrReattachStdioNotAllowed on an empty allowlist (fail-closed)", err)
		}
		if _, exists := reg.OwnerOf(desc.Name); exists {
			t.Fatal("fail-closed refusal must not register anything")
		}
	})

	t.Run("allowlisted command passes the gate", func(t *testing.T) {
		// The gate is what is under test, not the spawn: an allowlisted command
		// that does not exist on disk must get PAST the gate and fail at the
		// transport instead — a different, retryable class.
		a, _, _, _ := reAttacherFor(t,
			WithReattachGates([]string{"/usr/bin/definitely-not-allowlisted"}, false),
			WithReattachTimeout(3*time.Second))
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
		if errors.Is(err, ErrReattachStdioNotAllowed) {
			t.Fatalf("an allowlisted command must pass the stdio gate, got: %v", err)
		}
	})
}

// TestMCPConnectionAttacher_Reattach_InjectionKillSwitch — a persisted
// credential-injection mapping is NOT rebuilt after a restart with the
// fail-closed opt-in off, and IS rebuilt when it is on. The twin of the
// provider-side wire-descriptor kill-switch.
func TestMCPConnectionAttacher_Reattach_InjectionKillSwitch(t *testing.T) {
	fixture := reattachFixtureServer(t)
	desc := reHTTPDesc("inj-srv", fixture.URL)
	desc.Injection = &agentcfg.MCPCredentialInjectionDescriptor{
		Provider: "some-broker", Form: "header", Header: "x-downstream-api-key",
	}

	t.Run("opt-in OFF refuses", func(t *testing.T) {
		a, _, reg, bus := reAttacherFor(t, WithReattachGates(nil, false))
		evs := newREvents(t, bus, reQuad())
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
		if !errors.Is(err, ErrReattachInjectionDisabled) {
			t.Fatalf("err = %v, want ErrReattachInjectionDisabled", err)
		}
		if _, exists := reg.OwnerOf(desc.Name); exists {
			t.Fatal("a kill-switched re-attach must not register the server")
		}
		seen := evs.waitFor(t, "the reattach_failed event", func(e []events.Event) bool {
			return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) == 1
		})
		if got := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0].State; got != agentcfg.MCPReattachClassInjectionDisabled {
			t.Fatalf("class = %q, want %q", got, agentcfg.MCPReattachClassInjectionDisabled)
		}
	})

	t.Run("opt-in ON gets past the kill-switch", func(t *testing.T) {
		a, _, _, _ := reAttacherFor(t, WithReattachGates(nil, true))
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
		// The mapping now reaches the shared injection engine, which refuses it
		// for its OWN reason (the named broker is not a declared provider) — a
		// binding class, not the kill-switch.
		if errors.Is(err, ErrReattachInjectionDisabled) {
			t.Fatalf("the kill-switch must not fire with the opt-in on: %v", err)
		}
		if !errors.Is(err, mcpdrv.ErrOAuthBinding) {
			t.Fatalf("err = %v, want the injection engine's own binding refusal", err)
		}
	})
}

// --- The credential plane -----------------------------------------------------

// recordingProvider counts every credential-plane call. The re-attach must make
// NONE of them: the attach path resolves a provider by NAME and never touches a
// token (the bearer is minted per outbound CALL, one layer later).
type recordingProvider struct {
	hosts []string
	// tokenErr is what Token returns — the "no consent available" shape.
	tokenErr error

	tokenCalls    atomic.Int64
	initiateCalls atomic.Int64
	completeCalls atomic.Int64
	revokeCalls   atomic.Int64
	pendingCalls  atomic.Int64
	hostCalls     atomic.Int64
}

func (p *recordingProvider) Token(context.Context, tools.ToolSourceID) (toolauth.Token, error) {
	p.tokenCalls.Add(1)
	if p.tokenErr != nil {
		return toolauth.Token{}, p.tokenErr
	}
	return toolauth.Token{AccessToken: "unused-in-this-test"}, nil
}

func (p *recordingProvider) InitiateFlow(context.Context, tools.ToolSourceID) (toolauth.FlowInitiation, error) {
	p.initiateCalls.Add(1)
	return toolauth.FlowInitiation{}, errors.New("recordingProvider: InitiateFlow must never be called by the re-attach")
}

func (p *recordingProvider) CompleteFlow(context.Context, string, string) (toolauth.Token, error) {
	p.completeCalls.Add(1)
	return toolauth.Token{}, errors.New("recordingProvider: CompleteFlow must never be called by the re-attach")
}

func (p *recordingProvider) PendingFlow(context.Context, string) (toolauth.PendingFlowInfo, bool, error) {
	p.pendingCalls.Add(1)
	return toolauth.PendingFlowInfo{}, false, nil
}

func (p *recordingProvider) DenyFlow(context.Context, string, string) error { return nil }

func (p *recordingProvider) Revoke(context.Context, tools.ToolSourceID) error {
	p.revokeCalls.Add(1)
	return nil
}

func (p *recordingProvider) Close(context.Context) error { return nil }

func (p *recordingProvider) AllowedDownstreamHosts() []string {
	p.hostCalls.Add(1)
	return append([]string(nil), p.hosts...)
}

func (p *recordingProvider) credentialCalls() int64 {
	return p.tokenCalls.Load() + p.initiateCalls.Load() + p.completeCalls.Load() +
		p.revokeCalls.Load() + p.pendingCalls.Load()
}

// mapResolver adapts a name→provider map to the driver's bare-name resolver.
type mapResolver map[string]toolauth.OAuthProvider

func (m mapResolver) Get(name string) (toolauth.OAuthProvider, bool) { p, ok := m[name]; return p, ok }
func (m mapResolver) Names() []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	return out
}

// hostOf returns the host[:port] of a fixture URL, for a provider's
// boot-declared downstream-sink allow-list.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "https://")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		t.Fatalf("cannot derive a host from %q", rawURL)
	}
	return trimmed
}

// TestMCPConnectionAttacher_Reattach_OAuthProviderBindingResolves — the
// `oauth_provider` NAME binding resolves at run start EXACTLY as it does at the
// admin add door, for BOTH provider families, and the provider's boot-declared
// downstream-sink allow-list is the gate. Table-driven over the two families so
// a later reader cannot conclude one of them needs an interactive leg at attach.
func TestMCPConnectionAttacher_Reattach_OAuthProviderBindingResolves(t *testing.T) {
	fixture := reattachFixtureServer(t)
	host := hostOf(t, fixture.URL)

	for _, family := range []struct {
		name string
		// tokenErr models the family's runtime shape at the FIRST CALL: the
		// interactive family answers auth-required with no stored consent; the
		// brokered family answers auth-required when its broker is unreachable.
		// Neither shape is reachable during the attach — that is the point.
		tokenErr error
	}{
		{name: "interactive (persists a sealed bearer)", tokenErr: &toolauth.ErrAuthRequired{Source: "x"}},
		{name: "brokered (persists nothing)", tokenErr: errors.New("broker unreachable")},
	} {
		t.Run(family.name+"/host on the allow-list attaches", func(t *testing.T) {
			prov := &recordingProvider{hosts: []string{host}, tokenErr: family.tokenErr}
			a, _, reg, _ := reAttacherForWithProviders(t, mapResolver{"p": prov})
			desc := reHTTPDesc("bound-ok", fixture.URL)
			desc.OAuthProvider = "p"
			if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil {
				t.Fatalf("a name binding whose host is allow-listed must re-attach: %v", err)
			}
			if _, exists := reg.OwnerOf("bound-ok"); !exists {
				t.Fatal("the re-attached server is not in the registry")
			}
			// The credential-plane assertion: resolving the binding read the
			// allow-list and NOTHING else.
			if n := prov.credentialCalls(); n != 0 {
				t.Fatalf("the attach path made %d credential-plane calls, want 0 "+
					"(the binding is a NAME; the bearer is minted per outbound call)", n)
			}
			if prov.hostCalls.Load() == 0 {
				t.Fatal("the downstream-sink allow-list was never consulted — the gate is inert")
			}
		})

		t.Run(family.name+"/host off the allow-list is refused", func(t *testing.T) {
			prov := &recordingProvider{hosts: []string{"elsewhere.example.invalid"}, tokenErr: family.tokenErr}
			a, _, reg, bus := reAttacherForWithProviders(t, mapResolver{"p": prov})
			evs := newREvents(t, bus, reQuad())
			desc := reHTTPDesc("bound-refused", fixture.URL)
			desc.OAuthProvider = "p"
			_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
			if !errors.Is(err, mcpdrv.ErrOAuthBinding) {
				t.Fatalf("err = %v, want ErrOAuthBinding (host absent from the boot allow-list)", err)
			}
			if _, exists := reg.OwnerOf("bound-refused"); exists {
				t.Fatal("a refused binding must not register the server")
			}
			seen := evs.waitFor(t, "the reattach_failed event", func(e []events.Event) bool {
				return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) == 1
			})
			if got := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0].State; got != agentcfg.MCPReattachClassOAuthBinding {
				t.Fatalf("class = %q, want %q", got, agentcfg.MCPReattachClassOAuthBinding)
			}
			if n := prov.credentialCalls(); n != 0 {
				t.Fatalf("a refused binding made %d credential-plane calls, want 0", n)
			}
		})

		t.Run(family.name+"/unknown provider name is refused", func(t *testing.T) {
			prov := &recordingProvider{hosts: []string{host}, tokenErr: family.tokenErr}
			a, _, _, _ := reAttacherForWithProviders(t, mapResolver{"p": prov})
			desc := reHTTPDesc("bound-unknown", fixture.URL)
			desc.OAuthProvider = "no-such-provider"
			if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); !errors.Is(err, mcpdrv.ErrOAuthBinding) {
				t.Fatalf("err = %v, want ErrOAuthBinding for an unknown provider name", err)
			}
		})
	}
}

// reAttacherForWithProviders builds a production attacher whose runtime provider
// resolution is the supplied map.
func reAttacherForWithProviders(t *testing.T, resolver mapResolver) (*MCPConnectionAttacher, tools.ToolCatalog, *mcpdrv.Registry, events.EventBus) {
	t.Helper()
	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	providers := make(map[string]toolauth.OAuthProvider, len(resolver))
	for k, v := range resolver {
		providers[k] = v
	}
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, providers, nil, nil,
		WithReattachGates(nil, false), WithReattachTimeout(15*time.Second))
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, cat, reg, bus
}

// TestMCPConnectionAttacher_Reattach_IsCredentialNeutral is the acceptance
// centrepiece for "the credential plane does not widen": a re-attach whose bound
// provider has NO usable credential SUCCEEDS, and not one token API is touched
// during the attach.
func TestMCPConnectionAttacher_Reattach_IsCredentialNeutral(t *testing.T) {
	fixture := reattachFixtureServer(t)
	host := hostOf(t, fixture.URL)

	for _, tc := range []struct {
		name     string
		tokenErr error
	}{
		{
			// The interactive family with an EMPTY token store: Token would answer
			// the typed auth-required sentinel.
			name:     "interactive provider with no stored consent",
			tokenErr: &toolauth.ErrAuthRequired{Source: "creditless"},
		},
		{
			// The brokered family with an unreachable broker: Token would answer a
			// broker error.
			name:     "brokered provider with an unreachable broker",
			tokenErr: errors.New("credential broker unreachable"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProvider{hosts: []string{host}, tokenErr: tc.tokenErr}
			a, _, reg, bus := reAttacherForWithProviders(t, mapResolver{"p": prov})
			evs := newREvents(t, bus, reQuad())

			desc := reHTTPDesc("creditless", fixture.URL)
			desc.OAuthProvider = "p"
			if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil {
				t.Fatalf("a credential-less re-attach must SUCCEED (the attach has no token step): %v", err)
			}
			if _, exists := reg.OwnerOf("creditless"); !exists {
				t.Fatal("the credential-less connection is not live")
			}
			if n := prov.tokenCalls.Load(); n != 0 {
				t.Fatalf("Token called %d times during the attach, want 0", n)
			}
			if n := prov.initiateCalls.Load(); n != 0 {
				t.Fatalf("InitiateFlow called %d times during the attach, want 0 — the leg must never start a consent flow", n)
			}
			if n := prov.completeCalls.Load(); n != 0 {
				t.Fatalf("CompleteFlow called %d times during the attach, want 0", n)
			}
			if n := prov.credentialCalls(); n != 0 {
				t.Fatalf("%d credential-plane calls during the attach, want 0", n)
			}
			// And the success is REPORTED on the additive event, with the
			// reconciling run's RunID as the discriminator from an admin add.
			seen := evs.waitFor(t, "the reattached event", func(e []events.Event) bool {
				return countType(e, agentcfg.EventTypeMCPConnectionReattached) == 1
			})
			p := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattached)[0]
			if p.State != "online" {
				t.Fatalf("state = %q, want \"online\"", p.State)
			}
			if p.Author.RunID != reQuad().RunID {
				t.Fatalf("Author.RunID = %q, want the reconciling run's %q (the admin-add discriminator)", p.Author.RunID, reQuad().RunID)
			}
			if p.AgentID != reAgent {
				t.Fatalf("AgentID = %q, want %q", p.AgentID, reAgent)
			}
		})
	}
}

// TestMCPConnectionAttacher_Reattach_ShortfallSurfacesOnFirstCallNotOnAttach —
// after a credential-less re-attach, the shortfall surfaces on the FIRST TOOL
// CALL as the shipped typed auth-required error, through the EXISTING path. The
// re-attach leg adds no code to that path and must not pre-empt it.
func TestMCPConnectionAttacher_Reattach_ShortfallSurfacesOnFirstCallNotOnAttach(t *testing.T) {
	fixture := reattachFixtureServer(t)
	host := hostOf(t, fixture.URL)
	prov := &recordingProvider{hosts: []string{host}, tokenErr: &toolauth.ErrAuthRequired{Source: "shortfall"}}
	a, cat, _, _ := reAttacherForWithProviders(t, mapResolver{"p": prov})

	desc := reHTTPDesc("shortfall", fixture.URL)
	desc.OAuthProvider = "p"
	if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if prov.tokenCalls.Load() != 0 {
		t.Fatal("the attach touched the token plane")
	}

	// The tool is live in the catalog; invoking it is where the credential is
	// needed — and where the SHIPPED typed sentinel fires.
	d, ok := cat.Resolve("shortfall_echo")
	if !ok {
		t.Fatal("the re-attached server's tool is not in the catalog")
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	callCtx, idErr := identity.With(callCtx, reQuad().Identity)
	if idErr != nil {
		t.Fatalf("identity.With: %v", idErr)
	}
	_, callErr := d.Invoke(callCtx, []byte(`{"text":"hi"}`))
	if callErr == nil {
		t.Fatal("a call with no available credential must fail loud, not silently succeed unauthenticated")
	}
	var authReq *toolauth.ErrAuthRequired
	if !errors.As(callErr, &authReq) {
		t.Fatalf("first-call error = %v, want the SHIPPED typed *auth.ErrAuthRequired", callErr)
	}
	if prov.tokenCalls.Load() == 0 {
		t.Fatal("the first call never reached the token plane — the shortfall is not surfacing on the shipped path")
	}
}

// --- The closed failure-class set --------------------------------------------

// TestReattach_FailureClassesAreDistinctAndAllReported drives EACH class in the
// closed set and asserts it emits its own stable class on the failure event. A
// silently-absent connection is a bug; the report is the contract.
func TestReattach_FailureClassesAreDistinctAndAllReported(t *testing.T) {
	seen := map[string]bool{}

	// 1. transport_failed — a URL nothing answers on.
	t.Run(agentcfg.MCPReattachClassTransportFailed, func(t *testing.T) {
		dead := deadHTTPURL(t)
		a, _, _, bus := reAttacherFor(t, WithReattachTimeout(3*time.Second))
		evs := newREvents(t, bus, reQuad())
		if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), reHTTPDesc("dead", dead)); err == nil {
			t.Fatal("an unreachable server must fail loud")
		}
		assertClass(t, evs, agentcfg.MCPReattachClassTransportFailed)
		seen[agentcfg.MCPReattachClassTransportFailed] = true
	})

	// 2. stdio_not_allowed.
	t.Run(agentcfg.MCPReattachClassStdioNotAllowed, func(t *testing.T) {
		a, _, _, bus := reAttacherFor(t)
		evs := newREvents(t, bus, reQuad())
		_, _ = a.Reattach(context.Background(), reOwner(), reQuad(), agentcfg.MCPConnectionDescriptor{
			Name: "s", Transport: agentcfg.MCPTransportStdio, Command: []string{"/bin/nope"},
		})
		assertClass(t, evs, agentcfg.MCPReattachClassStdioNotAllowed)
		seen[agentcfg.MCPReattachClassStdioNotAllowed] = true
	})

	// 3. injection_disabled.
	t.Run(agentcfg.MCPReattachClassInjectionDisabled, func(t *testing.T) {
		a, _, _, bus := reAttacherFor(t, WithReattachGates(nil, false))
		evs := newREvents(t, bus, reQuad())
		d := reHTTPDesc("inj", "https://example.invalid/mcp")
		d.Injection = &agentcfg.MCPCredentialInjectionDescriptor{Provider: "b", Form: "header", Header: "x-api-key"}
		_, _ = a.Reattach(context.Background(), reOwner(), reQuad(), d)
		assertClass(t, evs, agentcfg.MCPReattachClassInjectionDisabled)
		seen[agentcfg.MCPReattachClassInjectionDisabled] = true
	})

	// 4. oauth_binding.
	t.Run(agentcfg.MCPReattachClassOAuthBinding, func(t *testing.T) {
		a, _, _, bus := reAttacherFor(t)
		evs := newREvents(t, bus, reQuad())
		d := reHTTPDesc("bind", "https://example.invalid/mcp")
		d.OAuthProvider = "unknown"
		_, _ = a.Reattach(context.Background(), reOwner(), reQuad(), d)
		assertClass(t, evs, agentcfg.MCPReattachClassOAuthBinding)
		seen[agentcfg.MCPReattachClassOAuthBinding] = true
	})

	// 5. owner_conflict — the name is live under a DIFFERENT owner.
	t.Run(agentcfg.MCPReattachClassOwnerConflict, func(t *testing.T) {
		a, _, reg, bus := reAttacherFor(t)
		evs := newREvents(t, bus, reQuad())
		other := toolauth.Owner{Tenant: "tenant-other", Agent: "agent-other"}
		otherProv := &reattachFakeProvider{id: tools.ToolSourceID("shared-name")}
		if err := reg.Register(reattachIDCtx(t), mcpdrv.ServerRegistration{
			Provider: otherProv, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline, Owner: other,
		}); err != nil {
			t.Fatalf("pre-seed the other owner's registration: %v", err)
		}
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), reHTTPDesc("shared-name", "https://example.invalid/mcp"))
		if !errors.Is(err, mcpdrv.ErrConnectionNameOwnerConflict) {
			t.Fatalf("err = %v, want the inherited ErrConnectionNameOwnerConflict", err)
		}
		// The other owner's live registration is untouched — never evicted.
		if otherProv.closeCount() != 0 {
			t.Fatal("a cross-owner conflict must NEVER tear down the other owner's transport")
		}
		if o, ok := reg.OwnerOf("shared-name"); !ok || o != other {
			t.Fatalf("the other owner's registration was replaced: owner=%+v ok=%t", o, ok)
		}
		assertClass(t, evs, agentcfg.MCPReattachClassOwnerConflict)
		seen[agentcfg.MCPReattachClassOwnerConflict] = true
	})

	// 6. ambiguous_server_id — the declared name is separator-ambiguous with a
	//    registered one.
	t.Run(agentcfg.MCPReattachClassAmbiguousServerID, func(t *testing.T) {
		a, _, reg, bus := reAttacherFor(t)
		evs := newREvents(t, bus, reQuad())
		existing := &reattachFakeProvider{id: tools.ToolSourceID("srv")}
		if err := reg.Register(reattachIDCtx(t), mcpdrv.ServerRegistration{
			Provider: existing, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline, Owner: reOwner(),
		}); err != nil {
			t.Fatalf("pre-seed: %v", err)
		}
		// "srv_extra" is an underscore-extension of the registered "srv".
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), reHTTPDesc("srv_extra", "https://example.invalid/mcp"))
		if !errors.Is(err, mcpdrv.ErrAmbiguousServerID) {
			t.Fatalf("err = %v, want ErrAmbiguousServerID", err)
		}
		assertClass(t, evs, agentcfg.MCPReattachClassAmbiguousServerID)
		seen[agentcfg.MCPReattachClassAmbiguousServerID] = true
	})

	// The set is CLOSED: every declared class was induced and reported. Deleting
	// one emit site (or one class) fails here.
	for _, c := range agentcfg.MCPReattachFailureClasses() {
		if !seen[c] {
			t.Errorf("class %q is declared in the closed set but no case induced + reported it", c)
		}
	}
}

// assertClass waits for exactly one failure event and asserts its class.
func assertClass(t *testing.T, evs *reEvents, want string) {
	t.Helper()
	seen := evs.waitFor(t, "the reattach_failed event for class "+want, func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) >= 1
	})
	ps := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)
	if ps[0].State != want {
		t.Fatalf("class = %q, want %q (reason=%q)", ps[0].State, want, ps[0].Reason)
	}
	if ps[0].Reason == "" {
		t.Fatal("a reported failure must carry an operator-facing reason")
	}
}

// deadHTTPURL returns a URL on a port that was bound and immediately released,
// so a dial fails fast and deterministically (no external network).
func deadHTTPURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr + "/mcp"
}

// TestMCPConnectionAttacher_Reattach_HeaderAuthenticatedServerFailsLoud is the
// header bound made a TEST rather than a caveat: the operator-supplied static
// headers are never persisted, so the re-attach dials without them; a server that
// required one refuses, and the outcome is classified and reported with nothing
// half-registered.
func TestMCPConnectionAttacher_Reattach_HeaderAuthenticatedServerFailsLoud(t *testing.T) {
	// A spec-derived MCP fixture behind a 401 gate keyed on a header the
	// descriptor cannot carry.
	inner := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return reattachEchoMCPServer() }, nil)
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Auth") != "expected-by-the-server" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(gated.Close)

	a, cat, reg, bus := reAttacherFor(t, WithReattachTimeout(10*time.Second))
	evs := newREvents(t, bus, reQuad())

	_, err := a.Reattach(context.Background(), reOwner(), reQuad(), reHTTPDesc("header-auth", gated.URL))
	if err == nil {
		t.Fatal("a header-authenticated server must NOT silently re-attach without its header")
	}
	if _, exists := reg.OwnerOf("header-auth"); exists {
		t.Fatal("nothing may be half-registered after a failed re-attach")
	}
	if _, ok := cat.Resolve("header-auth_echo"); ok {
		t.Fatal("no catalog tool may survive a failed re-attach")
	}
	// It is a TRANSPORT failure, not an auth_required park: a run-start reconcile
	// has no admin request to resume and no consenting principal.
	assertClass(t, evs, agentcfg.MCPReattachClassTransportFailed)
	if errors.Is(err, agentcfgprotocol.ErrAuthRequired) {
		t.Fatalf("the re-attach must never route onto the add door's auth-required park: %v", err)
	}
}

// --- Idempotency, bounds, and the retry window --------------------------------

// TestMCPConnectionAttacher_Reattach_AlreadyRegisteredUnderOwnerIsNoOp — the
// under-lock re-check: a name already live under the reconciling owner is a
// no-op, with NO transport churn (the live provider instance is unchanged).
func TestMCPConnectionAttacher_Reattach_AlreadyRegisteredUnderOwnerIsNoOp(t *testing.T) {
	fixture := reattachFixtureServer(t)
	a, _, reg, bus := reAttacherFor(t, WithReattachTimeout(15*time.Second))
	evs := newREvents(t, bus, reQuad())

	desc := reHTTPDesc("noop", fixture.URL)
	if changed, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil {
		t.Fatalf("first re-attach: %v", err)
	} else if !changed {
		t.Fatal("first re-attach reported no change")
	}
	before, err := reg.GetServer(reattachIDCtx(t), "noop")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}

	// A second reconcile for the SAME owner (the raced-view case).
	if changed, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil {
		t.Fatalf("second re-attach must be a clean no-op: %v", err)
	} else if changed {
		t.Fatal("exact descriptor re-attach reported a replacement")
	}
	after, err := reg.GetServer(reattachIDCtx(t), "noop")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if before.LastDiscoveryAt != after.LastDiscoveryAt {
		t.Fatalf("the live registration was replaced (transport churn): before=%v after=%v",
			before.LastDiscoveryAt, after.LastDiscoveryAt)
	}
	// Exactly ONE reattached event: the no-op re-attach reports nothing new.
	seen := evs.waitFor(t, "the single reattached event", func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattached) >= 1
	})
	if n := countType(seen, agentcfg.EventTypeMCPConnectionReattached); n != 1 {
		t.Fatalf("reattached events = %d, want exactly 1 (the no-op must not re-report)", n)
	}
}

func TestMCPConnectionAttacher_Reattach_SameOwnerChangedDescriptorReplaces(t *testing.T) {
	fixture := reattachFixtureServer(t)
	a, _, reg, _ := reAttacherFor(t, WithReattachTimeout(15*time.Second))
	desc := reHTTPDesc("replace", fixture.URL)
	if changed, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err != nil || !changed {
		t.Fatalf("first Reattach changed=%t err=%v", changed, err)
	}
	_, beforeFingerprint, ok := reg.RegistrationIdentity(desc.Name)
	if !ok {
		t.Fatal("first descriptor not registered")
	}
	edited := desc
	edited.MetaAnnotations = map[string]string{"deployment.generation": "2"}
	if changed, err := a.Reattach(context.Background(), reOwner(), reQuad(), edited); err != nil || !changed {
		t.Fatalf("changed Reattach changed=%t err=%v", changed, err)
	}
	owner, afterFingerprint, ok := reg.RegistrationIdentity(desc.Name)
	if !ok || owner != reOwner() {
		t.Fatalf("replacement registration owner=%+v ok=%t", owner, ok)
	}
	if beforeFingerprint == afterFingerprint || afterFingerprint != agentcfg.MCPConnectionFingerprint(edited) {
		t.Fatalf("replacement fingerprint before=%q after=%q", beforeFingerprint, afterFingerprint)
	}
}

// TestMCPConnectionAttacher_Reattach_BoundedContext pins the load-bearing bound:
// a fixture that accepts the connection and never answers the handshake must not
// stall the sweep — the MCP provider's Connect carries no internal timeout, so
// the re-attach's own bound is the only thing that ends it.
func TestMCPConnectionAttacher_Reattach_BoundedContext(t *testing.T) {
	block := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Accept, then never answer, until the request is cancelled or the test
		// tears down.
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run LIFO, so `close(block)` (registered LAST) runs FIRST and
	// releases any handler still parked before httptest's Close waits on it.
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(block) })

	const bound = 750 * time.Millisecond
	a, _, reg, _ := reAttacherFor(t, WithReattachTimeout(bound))

	start := time.Now()
	// The CALLER's ctx is generous: only the driver's own bounds can end this.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, err := a.Reattach(ctx, reOwner(), reQuad(), reHTTPDesc("hung", hung.URL))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that never answers the handshake must fail the re-attach")
	}
	// The assertion is BOUNDED, not fast. Two bounds compose here and both are
	// load-bearing: the per-connection re-attach timeout ends the handshake, and
	// the driver's unowned-request bound ends the MCP SDK's session teardown,
	// which runs on a context this runtime does not own. Without the SECOND one
	// this call never returns at all — a stall that reaches every run start.
	if elapsed > 60*time.Second {
		t.Fatalf("the re-attach took %v — an unresponsive server is not being bounded", elapsed)
	}
	if _, exists := reg.OwnerOf("hung"); exists {
		t.Fatal("a timed-out re-attach must not register the server")
	}
}

// TestMCPConnectionAttacher_Reattach_BackoffBoundsRetries — a permanently
// failing server is dialled a BOUNDED number of times across N run starts: the
// first failure emits and opens the window, the rest are suppressed and counted,
// and an operator's EDIT to the descriptor resets the window immediately.
func TestMCPConnectionAttacher_Reattach_BackoffBoundsRetries(t *testing.T) {
	dead := deadHTTPURL(t)
	var dials atomic.Int64
	// A controllable clock: the window never elapses unless the test advances it.
	var nowMu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}

	a, _, _, bus := reAttacherFor(t,
		WithReattachTimeout(2*time.Second), WithReattachClock(clock))
	evs := newREvents(t, bus, reQuad())
	desc := reHTTPDesc("flaky", dead)

	// Run start #1: dials, fails, emits, opens the window.
	if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); err == nil {
		t.Fatal("want a failure")
	} else if errors.Is(err, ErrReattachSuppressed) {
		t.Fatal("the FIRST failure must never be suppressed — it is the loud one")
	}
	dials.Add(1)

	// Run starts #2..#11: suppressed by the window, counted, NOT re-dialled.
	const suppressedRuns = 10
	for range suppressedRuns {
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc)
		if !errors.Is(err, ErrReattachSuppressed) {
			t.Fatalf("err = %v, want ErrReattachSuppressed inside the retry window", err)
		}
	}
	seen := evs.waitFor(t, "the first failure event", func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) >= 1
	})
	if n := countType(seen, agentcfg.EventTypeMCPConnectionReattachFailed); n != 1 {
		t.Fatalf("failure events = %d after %d suppressed run starts, want exactly 1 (bounded reporting)",
			n, suppressedRuns)
	}

	// Advance past the window: the next run start dials again, fails again, and
	// its event CARRIES the suppressed count — bounded, never silent.
	advance(reattachBackoffMax + time.Minute)
	if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), desc); errors.Is(err, ErrReattachSuppressed) {
		t.Fatal("the window elapsed — the next run start must retry")
	}
	seen = evs.waitFor(t, "the second failure event", func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) >= 2
	})
	ps := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)
	last := ps[len(ps)-1]
	if !strings.Contains(last.Reason, fmt.Sprintf("suppressed_attempts=%d", suppressedRuns)) {
		t.Fatalf("reason = %q, want it to carry suppressed_attempts=%d (the suppression must be reported)",
			last.Reason, suppressedRuns)
	}

	// An operator EDIT resets the window: the very next run start retries without
	// waiting, even though the window has not elapsed.
	edited := desc
	edited.URL = deadHTTPURL(t)
	if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), edited); errors.Is(err, ErrReattachSuppressed) {
		t.Fatal("an edited descriptor must retry immediately, not serve out the old window")
	}
}

// TestMCPConnectionAttacher_Reattach_TerminalClassReportsOnce — a class that
// cannot heal by re-dialling is reported ONCE per attempted descriptor, not on
// every run start, and is never retried until the descriptor changes.
func TestMCPConnectionAttacher_Reattach_TerminalClassReportsOnce(t *testing.T) {
	a, _, _, bus := reAttacherFor(t)
	evs := newREvents(t, bus, reQuad())
	d := reHTTPDesc("terminal", "https://example.invalid/mcp")
	d.OAuthProvider = "unknown-provider"

	for i := range 8 {
		_, err := a.Reattach(context.Background(), reOwner(), reQuad(), d)
		switch {
		case i == 0 && !errors.Is(err, mcpdrv.ErrOAuthBinding):
			t.Fatalf("first attempt = %v, want the loud binding error", err)
		case i > 0 && !errors.Is(err, ErrReattachSuppressed):
			t.Fatalf("attempt %d = %v, want ErrReattachSuppressed (a terminal class is not re-dialled)", i, err)
		}
	}
	seen := evs.waitFor(t, "the single failure event", func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) >= 1
	})
	if n := countType(seen, agentcfg.EventTypeMCPConnectionReattachFailed); n != 1 {
		t.Fatalf("failure events = %d across 8 run starts, want exactly 1 for a terminal class", n)
	}
}

// TestMCPConnectionAttacher_Reattach_OwnerMandatory — the fail-closed owner
// guard: an incomplete owner is refused loud rather than producing an
// unreconcilable orphan registration.
func TestMCPConnectionAttacher_Reattach_OwnerMandatory(t *testing.T) {
	a, _, reg, _ := reAttacherFor(t)
	for _, owner := range []toolauth.Owner{
		{},
		{Tenant: reTenant},
		{Agent: reAgent},
	} {
		_, err := a.Reattach(context.Background(), owner, reQuad(), reHTTPDesc("orphan", "https://example.invalid/mcp"))
		if !errors.Is(err, ErrRuntimeAddOwnerMissing) {
			t.Fatalf("owner %+v: err = %v, want ErrRuntimeAddOwnerMissing", owner, err)
		}
	}
	if _, exists := reg.OwnerOf("orphan"); exists {
		t.Fatal("an owner-less re-attach must never register anything")
	}
}

// TestReattach_EventPayloadsAreSafeAndScrubbed — both new event types are
// registered canonically, both payloads are SafePayload, and a sentinel secret
// planted in the descriptor's URL userinfo appears NOWHERE in an emitted payload.
func TestReattach_EventPayloadsAreSafeAndScrubbed(t *testing.T) {
	for _, typ := range []events.EventType{
		agentcfg.EventTypeMCPConnectionReattached,
		agentcfg.EventTypeMCPConnectionReattachFailed,
	} {
		if !events.IsValidEventType(typ) {
			t.Fatalf("event type %q is not registered in the canonical registry", typ)
		}
	}
	var _ events.SafePayload = agentcfg.MCPConnectionLifecyclePayload{}

	// The sentinel is a dummy fixture value, never a real credential.
	const sentinel = "s3nt1nel-not-a-real-secret"
	dead := deadHTTPURL(t)
	withUserinfo := strings.Replace(dead, "http://", "http://admin:"+sentinel+"@", 1)

	a, _, _, bus := reAttacherFor(t, WithReattachTimeout(3*time.Second))
	evs := newREvents(t, bus, reQuad())
	if _, err := a.Reattach(context.Background(), reOwner(), reQuad(), reHTTPDesc("scrub", withUserinfo)); err == nil {
		t.Fatal("want a transport failure")
	}
	seen := evs.waitFor(t, "the reattach_failed event", func(e []events.Event) bool {
		return countType(e, agentcfg.EventTypeMCPConnectionReattachFailed) >= 1
	})
	p := reattachPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0]
	if strings.Contains(p.Reason, sentinel) {
		t.Fatalf("the URL userinfo secret leaked into the event reason: %q", p.Reason)
	}
	if strings.Contains(fmt.Sprintf("%+v", p), sentinel) {
		t.Fatalf("the URL userinfo secret leaked somewhere in the payload: %+v", p)
	}
	if p.PauseToken != "" {
		t.Fatal("a run-start re-attach never parks — PauseToken must stay empty")
	}
}

// --- Concurrent reuse (D-025) -------------------------------------------------

// TestMCPConnectionAttacher_Reattach_ConcurrentOwners is the concurrent-reuse
// run: N=128 per owner, TWO owners, ONE shared attacher and ONE shared registry,
// under -race. Asserts exactly one attach lands per (owner, name) — no
// double-attach, no transport churn — no cross-owner bleed, no cancellation
// cross-talk, and a goroutine baseline restored after Close.
func TestMCPConnectionAttacher_Reattach_ConcurrentOwners(t *testing.T) {
	const N = 128
	base := goruntime.NumGoroutine()

	srvA := reattachEchoMCPServer()
	fixA := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srvA }, nil))
	srvB := reattachEchoMCPServer()
	fixB := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srvB }, nil))
	closeFixtures := sync.OnceFunc(func() { fixA.Close(); fixB.Close() })
	t.Cleanup(closeFixtures)

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil, nil,
		WithReattachTimeout(20*time.Second))

	ownerA := toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}
	ownerB := toolauth.Owner{Tenant: "tenant-b", Agent: "agent-b"}
	quadA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}, RunID: "run-a"}
	quadB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s"}, RunID: "run-b"}
	descA := reHTTPDesc("conn-a", fixA.URL)
	descB := reHTTPDesc("conn-b", fixB.URL)

	// Cancellation cross-talk: owner A's runs use a ctx the test cancels partway;
	// owner B's must be entirely unaffected.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	var wg sync.WaitGroup
	wg.Add(2 * N)
	errsB := make(chan error, N)
	for i := range N {
		go func() {
			defer wg.Done()
			if i == N/2 {
				cancelA()
			}
			// A's outcome is deliberately unasserted: half its runs race a
			// cancellation. What matters is that it never races B.
			_, _ = a.Reattach(ctxA, ownerA, quadA, descA)
		}()
		go func() {
			defer wg.Done()
			if _, err := a.Reattach(context.Background(), ownerB, quadB, descB); err != nil {
				errsB <- err
			}
		}()
	}
	wg.Wait()
	close(errsB)
	for err := range errsB {
		t.Fatalf("owner B's re-attach was disturbed by owner A's cancellation: %v", err)
	}

	// Exactly one registration per (owner, name), each under its OWN owner.
	if o, ok := reg.OwnerOf("conn-b"); !ok || o != ownerB {
		t.Fatalf("conn-b owner = %+v ok=%t, want %+v", o, ok, ownerB)
	}
	if o, ok := reg.OwnerOf("conn-a"); ok && o != ownerA {
		t.Fatalf("conn-a owner = %+v, want %+v (no cross-owner bleed)", o, ownerA)
	}
	servers, _, lerr := reg.ListServers(reattachIDCtx(t), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	names := map[string]int{}
	for _, s := range servers {
		names[s.Name]++
	}
	if names["conn-b"] != 1 {
		t.Fatalf("conn-b registrations = %d, want exactly 1 (no double-attach across %d concurrent run starts)", names["conn-b"], N)
	}
	if names["conn-a"] > 1 {
		t.Fatalf("conn-a registrations = %d, want at most 1", names["conn-a"])
	}
	// B's tool set is live and is B's — never A's.
	if d, ok := cat.Resolve("conn-b_echo"); !ok || d.Tool.Source != tools.ToolSourceID("conn-b") {
		t.Fatalf("conn-b_echo resolve = %+v ok=%t, want B's own descriptor", d.Tool, ok)
	}

	_ = a.Close(context.Background())
	closeFixtures()
	deadline := time.Now().Add(10 * time.Second)
	for goruntime.NumGoroutine() > base+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		goruntime.GC()
	}
	if leaked := goruntime.NumGoroutine() - base; leaked > 4 {
		t.Errorf("goroutine leak after the concurrent re-attach run + Close: baseline=%d now=%d (leaked ~%d)",
			base, goruntime.NumGoroutine(), leaked)
	}
}
