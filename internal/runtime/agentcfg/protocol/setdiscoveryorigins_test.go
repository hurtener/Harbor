package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// setdiscoveryorigins_test.go — unit coverage for
// `agent_config.set_mcp_discovery_origins`: the shared-validator origin gate,
// the four distinct loud errors (unknown / boot-declared / stdio /
// owner-mismatch), the revision write + sibling carry-forward, the live apply
// (owner-scoped) + granted/revoked delta, and the fail-closed audit (revert on
// emit failure).

// fakeApplier is a deterministic stand-in for the live registry applier; it
// records the current allow-list per connection so a test can assert both the
// applied set and a compensating revert.
type fakeApplier struct {
	mu      sync.Mutex
	current map[string][]string
	calls   int
	// owners records the (tenant, agent) owner presented on each apply — the
	// setter passes the caller's resolved owner through so the live write is
	// owner-scoped.
	owners [][2]string
}

func newFakeApplier() *fakeApplier { return &fakeApplier{current: map[string][]string{}} }

// seenOwners returns a copy of the owners presented on the apply calls.
func (f *fakeApplier) seenOwners() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.owners...)
}

func (f *fakeApplier) SetOAuthDiscoveryOrigins(_ context.Context, tenant, agentID, name string, origins []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.owners = append(f.owners, [2]string{tenant, agentID})
	prev := append([]string(nil), f.current[name]...)
	f.current[name] = append([]string(nil), origins...)
	return prev, nil
}

func (f *fakeApplier) get(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.current[name]...)
}

type failingBus struct{ events.EventBus }

func (failingBus) Publish(context.Context, events.Event) error { return errors.New("bus boom") }

//nolint:unparam // test helper: the agent id is fixed across these cases.
func seedDiscoveryRev(t *testing.T, ctx context.Context, reg agentcfg.Registry, agentID string, desc agentcfg.MCPConnectionDescriptor) {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	base := "keep-me"
	payload := agentcfg.ConfigPayload{
		Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{desc}},
		PromptLayers: &agentcfg.PromptLayers{Base: &base},
	}
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
}

//nolint:unparam // test helper: the agent id is fixed across these cases.
func discReq(agentID, name string, origins []string) prototypes.AgentConfigSetMCPDiscoveryOriginsRequest {
	return prototypes.AgentConfigSetMCPDiscoveryOriginsRequest{Identity: scope(), AgentID: agentID, Name: name, AllowedOrigins: origins}
}

func TestSetMCPDiscoveryOrigins_WritesRevisionAppliesLive(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	bus := newCollectingBus(t)
	applier := newFakeApplier()
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	resp, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"}))
	if err != nil {
		t.Fatalf("SetMCPDiscoveryOrigins: %v", err)
	}
	if !resp.AppliedLive {
		t.Error("applied_live = false, want true")
	}
	if len(resp.Granted) != 1 || resp.Granted[0] != "https://as.example.net" {
		t.Errorf("granted = %v", resp.Granted)
	}
	// Live applier saw the origins.
	if live := applier.get("srv"); len(live) != 1 || live[0] != "https://as.example.net" {
		t.Errorf("live applier = %v", live)
	}
	// The recorded revision descriptor carries the allow-list, and the sibling
	// prompt-layers section is carried forward (rebuild-completeness). Read the
	// domain active revision (authoritative).
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, _, err := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	descs := active.Payload.ConnectionDescriptors()
	if len(descs) != 1 || len(descs[0].OAuthDiscoveryAllowedOrigins) != 1 || descs[0].OAuthDiscoveryAllowedOrigins[0] != "https://as.example.net" {
		t.Fatalf("descriptor allow-list not persisted: %#v", descs)
	}
	if b, ok := active.Payload.BasePrompt(); !ok || b != "keep-me" {
		t.Errorf("sibling prompt-layers not carried forward: base=%q ok=%v", b, ok)
	}
	if len(bus.eventsOfType(agentcfg.EventTypeMCPDiscoveryOriginsSet)) != 1 {
		t.Errorf("want exactly one discovery_origins_set audit event")
	}
}

// notLiveApplier stands in for a connection that is declared in the revision but
// not attached in the live registry — the applier returns ErrDiscoveryTargetNotLive
// (what the real MCPConnectionAttacher translates the driver's ErrServerNotFound
// into), and the setter must DEGRADE to a revision-only write.
type notLiveApplier struct{ calls int }

func (a *notLiveApplier) SetOAuthDiscoveryOrigins(_ context.Context, _, _, name string, _ []string) ([]string, error) {
	a.calls++
	return nil, fmt.Errorf("%w: %q", agentcfgprotocol.ErrDiscoveryTargetNotLive, name)
}

func TestSetMCPDiscoveryOrigins_DegradesWhenConnectionNotLive(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	bus := newCollectingBus(t)
	applier := &notLiveApplier{}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	resp, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"}))
	if err != nil {
		t.Fatalf("SetMCPDiscoveryOrigins degrade: unexpected error %v (a not-live connection must record the revision, not fail)", err)
	}
	if resp.AppliedLive {
		t.Error("applied_live = true, want false for a not-live connection")
	}
	if len(resp.Granted) != 1 || resp.Granted[0] != "https://as.example.net" {
		t.Errorf("granted = %v", resp.Granted)
	}
	// The revision MUST be recorded (not rolled back) so the run-start reconcile
	// applies the allowance once the server comes online.
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, _, err := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	descs := active.Payload.ConnectionDescriptors()
	if len(descs) != 1 || len(descs[0].OAuthDiscoveryAllowedOrigins) != 1 || descs[0].OAuthDiscoveryAllowedOrigins[0] != "https://as.example.net" {
		t.Fatalf("degrade did not record the allow-list in the revision: %#v", descs)
	}
	if len(bus.eventsOfType(agentcfg.EventTypeMCPDiscoveryOriginsSet)) != 1 {
		t.Errorf("want exactly one discovery_origins_set audit event on a degraded write")
	}
}

// erroringApplier returns an arbitrary (non-not-live) live-apply error — the
// setter must roll the just-written revision back and surface the error loud,
// NEVER degrade. Guards the switch's default arm against a future refactor that
// might collapse it into the degrade arm.
type erroringApplier struct{ boom error }

func (a *erroringApplier) SetOAuthDiscoveryOrigins(context.Context, string, string, string, []string) ([]string, error) {
	return nil, a.boom
}

func TestSetMCPDiscoveryOrigins_RealApplyFailureRollsBackLoud(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	boom := errors.New("live registry exploded")
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(newCollectingBus(t)),
		agentcfgprotocol.WithDiscoveryOriginApplier(&erroringApplier{boom: boom}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as-initial.example.net"}})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"})); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the live-apply error (a real failure must fail loud, not degrade)", err)
	}
	// The revision was rolled back — the descriptor keeps its pre-write allow-list.
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, _, aerr := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if aerr != nil {
		t.Fatalf("active: %v", aerr)
	}
	descs := active.Payload.ConnectionDescriptors()
	if len(descs) != 1 || len(descs[0].OAuthDiscoveryAllowedOrigins) != 1 || descs[0].OAuthDiscoveryAllowedOrigins[0] != "https://as-initial.example.net" {
		t.Fatalf("revision not rolled back after a real apply failure: %#v", descs)
	}
}

func TestSetMCPDiscoveryOrigins_RejectsMalformedOrigin(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithDiscoveryOriginApplier(newFakeApplier()))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	for _, bad := range []string{"http://as.example.net", "https://as.example.net/path", "https://203.0.113.5", "not a url"} {
		if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{bad})); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
			t.Errorf("origin %q: err = %v, want ErrInvalidConnection", bad, err)
		}
	}
}

func TestSetMCPDiscoveryOrigins_UnknownConnection(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithDiscoveryOriginApplier(newFakeApplier()))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "nope", []string{"https://as.example.net"})); !errors.Is(err, agentcfgprotocol.ErrConnectionNotFound) {
		t.Fatalf("err = %v, want ErrConnectionNotFound", err)
	}
}

func TestSetMCPDiscoveryOrigins_BootDeclaredRejected(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithDiscoveryOriginApplier(newFakeApplier()),
		agentcfgprotocol.WithBootDeclaredMCPServers([]string{"boot-srv"}))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "boot-srv", []string{"https://as.example.net"})); !errors.Is(err, agentcfgprotocol.ErrBootDeclaredConnection) {
		t.Fatalf("err = %v, want ErrBootDeclaredConnection", err)
	}
}

// TestSetMCPDiscoveryOrigins_BootDeclaredRejectedWhenAlsoDeclaredInRevision
// pins the boot-declared guard on the DECLARED path: a caller whose own active
// revision carries a connection under a boot-declared name reaches exactly the
// same refusal as a caller whose revision does not, and the live allow-list is
// never touched. Boot state is not revisioned state on either path.
func TestSetMCPDiscoveryOrigins_BootDeclaredRejectedWhenAlsoDeclaredInRevision(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	applier := newFakeApplier()
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(newCollectingBus(t)),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier),
		agentcfgprotocol.WithBootDeclaredMCPServers([]string{"boot-srv"}))
	// The caller's OWN revision declares a connection under the boot-declared name.
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "boot-srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "boot-srv", []string{"https://as.example.net"})); !errors.Is(err, agentcfgprotocol.ErrBootDeclaredConnection) {
		t.Fatalf("err = %v, want ErrBootDeclaredConnection on the declared path too", err)
	}
	if applier.calls != 0 {
		t.Errorf("live applier calls = %d, want 0 (a boot-declared name never reaches the live registry)", applier.calls)
	}
	if live := applier.get("boot-srv"); len(live) != 0 {
		t.Errorf("live allow-list = %v, want untouched", live)
	}
}

// ownerMismatchApplier stands in for a live registry whose registration under
// this name belongs to a DIFFERENT owner — what the real MCPConnectionAttacher
// returns when its owner comparison refuses the write.
type ownerMismatchApplier struct{ calls int }

func (a *ownerMismatchApplier) SetOAuthDiscoveryOrigins(_ context.Context, _, _, name string, _ []string) ([]string, error) {
	a.calls++
	return nil, fmt.Errorf("%w: %q", agentcfgprotocol.ErrConnectionOwnerMismatch, name)
}

// TestSetMCPDiscoveryOrigins_OwnerMismatchFailsLoudAndRollsBack proves an owner
// refusal is surfaced (it maps to CodeScopeMismatch / 403 at the wire edge)
// rather than degrading to a revision-only write, and that the just-written
// revision is rolled back so the call leaves no observable effect.
func TestSetMCPDiscoveryOrigins_OwnerMismatchFailsLoudAndRollsBack(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	applier := &ownerMismatchApplier{}
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(newCollectingBus(t)),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as-initial.example.net"}})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"})); !errors.Is(err, agentcfgprotocol.ErrConnectionOwnerMismatch) {
		t.Fatalf("err = %v, want ErrConnectionOwnerMismatch (an owner refusal is loud, never a degrade)", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, _, aerr := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if aerr != nil {
		t.Fatalf("active: %v", aerr)
	}
	descs := active.Payload.ConnectionDescriptors()
	if len(descs) != 1 || len(descs[0].OAuthDiscoveryAllowedOrigins) != 1 || descs[0].OAuthDiscoveryAllowedOrigins[0] != "https://as-initial.example.net" {
		t.Fatalf("revision not rolled back after an owner refusal: %#v", descs)
	}
}

// TestSetMCPDiscoveryOrigins_PassesCallerOwnerToTheApplier proves the setter
// hands the live applier the caller's VERIFIED (tenant, agent) owner, so the
// live registry can scope the write to the caller's own registration.
func TestSetMCPDiscoveryOrigins_PassesCallerOwnerToTheApplier(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	applier := newFakeApplier()
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(newCollectingBus(t)),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"})); err != nil {
		t.Fatalf("SetMCPDiscoveryOrigins: %v", err)
	}
	seen := applier.seenOwners()
	if len(seen) != 1 || seen[0] != [2]string{"t", "agent-d"} {
		t.Fatalf("owners presented to the applier = %v, want [[t agent-d]]", seen)
	}
}

func TestSetMCPDiscoveryOrigins_StdioRejected(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, _ := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithDiscoveryOriginApplier(newFakeApplier()))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "cli", Transport: agentcfg.MCPTransportStdio, Command: []string{"srv"}})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "cli", []string{"https://as.example.net"})); !errors.Is(err, agentcfgprotocol.ErrDiscoveryOriginsNotHTTP) {
		t.Fatalf("err = %v, want ErrDiscoveryOriginsNotHTTP", err)
	}
}

func TestSetMCPDiscoveryOrigins_RevokeDelta(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	applier := newFakeApplier()
	s, _ := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(newCollectingBus(t)), agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://a.example.net", "https://b.example.net"})); err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://a.example.net"}))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(resp.Revoked) != 1 || resp.Revoked[0] != "https://b.example.net" {
		t.Errorf("revoked = %v, want [https://b.example.net]", resp.Revoked)
	}
	if len(resp.Granted) != 0 {
		t.Errorf("granted = %v, want empty", resp.Granted)
	}
}

// TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure pins the fail-closed
// audit posture: an emit failure reverts BOTH the live allow-list and the
// active revision pointer, so the call has no observable state change.
func TestSetMCPDiscoveryOrigins_FailsClosedOnAuditEmitFailure(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	applier := newFakeApplier()
	s, _ := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(failingBus{}),
		agentcfgprotocol.WithDiscoveryOriginApplier(applier))
	seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	before, _, err := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active before: %v", err)
	}

	if _, err := s.SetMCPDiscoveryOrigins(ctx, discReq("agent-d", "srv", []string{"https://as.example.net"})); err == nil {
		t.Fatal("want error on audit emit failure, got nil")
	}
	// Live allow-list reverted to its prior (empty) state.
	if live := applier.get("srv"); len(live) != 0 {
		t.Errorf("live allow-list = %v after fail-closed revert, want empty", live)
	}
	// Active pointer rolled back to the pre-write revision (no observable change).
	after, _, err := reg.Active(ctx, q, "agent-d", agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active after: %v", err)
	}
	if got := after.Payload.ConnectionDescriptors(); len(got) != 1 || len(got[0].OAuthDiscoveryAllowedOrigins) != 0 {
		t.Errorf("active revision carries the reverted allow-list: %#v", got)
	}
	_ = before
}
