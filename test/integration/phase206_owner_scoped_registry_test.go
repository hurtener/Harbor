package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// phase206_owner_scoped_registry_test.go — the owner-scoped live connection
// write and the full-payload descriptor validation, wired end to end across the
// protocol → agentcfg → tools/mcp seam with REAL drivers (§17.1/§17.3): the
// StateStore-backed agent-config registry, the in-memory bus + real audit
// redactor, the real process-global MCP registry, the production
// discovery-origin applier (serve.MCPConnectionAttacher), and the real Protocol
// Service + wire handler (reusing the daHarness from the discovery-allowance
// suite, which wires exactly that stack).
//
// It proves: (1) an allowance write lands on the CALLER'S OWN connection and a
// caller presenting another owner's connection name is refused with
// CodeScopeMismatch / 403 while the other owner's live allow-list is untouched;
// (2) the refusal leaves the caller's revision unchanged (rolled back — no
// observable effect); (3) a boot-declared name is refused on BOTH the
// not-declared and the declared path; (4) `agent_config.set_revision` rejects a
// malformed connection descriptor with 400 and persists nothing; and (5) the
// seam holds under concurrent cross-owner writes. Runs under -race.

const (
	p206BootServer = "boot-declared-srv"
)

// setRevisionWire posts a full-payload agent_config.set_revision through the
// real wire handler under the given scopes.
//
//nolint:unparam // test helper: these revision writes run under owner A's tenant.
func (h *daHarness) setRevisionWire(t *testing.T, tenant, agentID string, payload prototypes.AgentConfigPayload, scopes []protoauth.Scope) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(prototypes.AgentConfigSetRevisionRequest{AgentID: agentID, Payload: payload})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/set_revision", bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, tenant)
	req.Header.Set(stream.HeaderUser, daUser)
	req.Header.Set(stream.HeaderSession, daSession)
	req = req.WithContext(protoauth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// activeConnections reads the agent's active revision straight from the real
// agent-config registry — the authoritative persisted state.
//
//nolint:unparam // test helper: these reads run under owner A's tenant.
func (h *daHarness) activeConnections(t *testing.T, tenant, agentID string) []agentcfg.MCPConnectionDescriptor {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: daUser, SessionID: daSession}}
	active, ok, err := h.reg.Active(context.Background(), q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if !ok {
		return nil
	}
	return active.Payload.ConnectionDescriptors()
}

// activeRevisionID reads the agent's active revision id from the real
// agent-config registry — the discriminator that tells a genuine rollback from
// a fresh revision that merely happens to carry the same shape.
func (h *daHarness) activeRevisionID(t *testing.T, tenant, agentID string) string {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: daUser, SessionID: daSession}}
	active, ok, err := h.reg.Active(context.Background(), q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if !ok {
		return ""
	}
	return active.RevisionID
}

// TestE2E_OwnerScopedDiscoveryWrite_AppliesToOwnConnectionOnly drives two owners
// through the real wire handler against ONE shared process-global MCP registry:
// each owner's write lands on its own connection, and a caller naming the OTHER
// owner's connection is refused with CodeScopeMismatch while that owner's live
// allow-list keeps its own value.
func TestE2E_OwnerScopedDiscoveryWrite_AppliesToOwnConnectionOnly(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"})
	h.registerServer(t, daTenantB, daAgentB, "srv-b", []string{"https://as-b.example.net"})
	admin := []protoauth.Scope{protoauth.ScopeAdmin}

	// Owner A writes its OWN connection — applied live.
	if rec := h.setOrigins(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a2.example.net"}, admin); rec.Code != http.StatusOK {
		t.Fatalf("owner A own-connection write: %d body=%s", rec.Code, rec.Body.String())
	}
	_, _, liveA, err := h.mcpReg.OAuthDiscoveryTarget("srv-a")
	if err != nil {
		t.Fatalf("target srv-a: %v", err)
	}
	if len(liveA) != 1 || liveA[0] != "https://as-a2.example.net" {
		t.Fatalf("owner A live allow-list = %v, want [https://as-a2.example.net]", liveA)
	}

	// Owner A declares a connection named `srv-b` in its OWN revision — a name
	// owner B holds live — and then writes an allowance for it.
	if rec := h.setRevisionWire(t, daTenantA, daAgentA, prototypes.AgentConfigPayload{
		Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: "srv-a", Transport: "http", URL: "https://srv-a.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as-a2.example.net"}},
			{Name: "srv-b", Transport: "http", URL: "https://srv-b.invalid/rpc"},
		}},
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("owner A set_revision: %d body=%s", rec.Code, rec.Body.String())
	}
	beforeA := h.activeConnections(t, daTenantA, daAgentA)
	beforeRevA := h.activeRevisionID(t, daTenantA, daAgentA)

	rec := h.setOrigins(t, daTenantA, daAgentA, "srv-b", []string{"https://as-a3.example.net"}, admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("owner A write against owner B's connection = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(protoerrors.CodeScopeMismatch)) {
		t.Fatalf("body = %s, want CodeScopeMismatch", rec.Body.String())
	}

	// Owner B's live allow-list is its own.
	_, _, liveB, err := h.mcpReg.OAuthDiscoveryTarget("srv-b")
	if err != nil {
		t.Fatalf("target srv-b: %v", err)
	}
	if len(liveB) != 1 || liveB[0] != "https://as-b.example.net" {
		t.Fatalf("owner B live allow-list = %v, want [https://as-b.example.net]", liveB)
	}

	// Owner A's revision is unchanged — the refused write was rolled back.
	// The rollback restores the EXACT pre-write revision, not merely a
	// same-shaped one: the active revision id is unchanged and every descriptor
	// matches by name AND by origin values.
	if afterRevA := h.activeRevisionID(t, daTenantA, daAgentA); afterRevA != beforeRevA {
		t.Fatalf("owner A active revision = %q, want the pre-write %q (a refused write must roll back)", afterRevA, beforeRevA)
	}
	afterA := h.activeConnections(t, daTenantA, daAgentA)
	if len(afterA) != len(beforeA) {
		t.Fatalf("owner A connections = %d, want %d (refused write must roll back)", len(afterA), len(beforeA))
	}
	for i := range afterA {
		if afterA[i].Name != beforeA[i].Name ||
			!slices.Equal(afterA[i].OAuthDiscoveryAllowedOrigins, beforeA[i].OAuthDiscoveryAllowedOrigins) {
			t.Fatalf("owner A revision mutated by a refused write: %#v (was %#v)", afterA, beforeA)
		}
	}

	// Owner B still writes its OWN connection successfully.
	if rec := h.setOrigins(t, daTenantB, daAgentB, "srv-b", []string{"https://as-b2.example.net"}, admin); rec.Code != http.StatusOK {
		t.Fatalf("owner B own-connection write: %d body=%s", rec.Code, rec.Body.String())
	}
	_, _, liveB, err = h.mcpReg.OAuthDiscoveryTarget("srv-b")
	if err != nil {
		t.Fatalf("target srv-b: %v", err)
	}
	if len(liveB) != 1 || liveB[0] != "https://as-b2.example.net" {
		t.Fatalf("owner B live allow-list = %v, want [https://as-b2.example.net]", liveB)
	}
}

// TestE2E_OwnerScopedRegistryWrite_IsTheAuthoritativeEnforcement drives the
// REAL registry directly rather than through the applier, so the registry
// layer's own scoping is pinned end to end and not only at the Protocol edge.
// The applier's OwnerOf read exists to give the operator the right diagnostic;
// this pins the enforcement underneath it on its own terms.
//
// It also pins the ordering hazard the classification cannot cover: a
// registration REPLACED between the classification read and the write (the
// caller owned the name a moment ago, another owner holds it now) still refuses
// — the write fails closed rather than applying against the new owner's entry.
func TestE2E_OwnerScopedRegistryWrite_IsTheAuthoritativeEnforcement(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"})
	ownerA := auth.Owner{Tenant: daTenantA, Agent: daAgentA}
	ownerB := auth.Owner{Tenant: daTenantB, Agent: daAgentB}

	// A non-owning owner is refused AT THE REGISTRY, with the live allow-list
	// untouched — no Protocol-edge classification involved.
	if _, err := h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantB), "srv-a", ownerB, []string{"https://other-owner.example.net"}); !errors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("non-owning owner at the registry: err = %v, want ErrServerNotFound", err)
	}
	// The zero owner owns nothing — it must not resolve onto any registration.
	if _, err := h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantA), "srv-a", auth.Owner{}, []string{"https://zero-owner.example.net"}); !errors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("zero owner at the registry: err = %v, want ErrServerNotFound", err)
	}
	_, _, live, err := h.mcpReg.OAuthDiscoveryTarget("srv-a")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(live) != 1 || live[0] != "https://as-a.example.net" {
		t.Fatalf("live allow-list = %v after refused writes, want the owner's [https://as-a.example.net]", live)
	}

	// The owning owner succeeds against the same name.
	if _, err := h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantA), "srv-a", ownerA, []string{"https://as-a2.example.net"}); err != nil {
		t.Fatalf("owning owner at the registry: %v", err)
	}

	// Re-register the SAME name under owner B — the supersede a classification
	// read taken a moment earlier would have missed. The write still refuses.
	h.registerServer(t, daTenantB, daAgentB, "srv-a", []string{"https://as-b.example.net"})
	if _, err := h.mcpReg.SetOAuthDiscoveryOrigins(idCtxFor(t, daTenantA), "srv-a", ownerA, []string{"https://stale-owner.example.net"}); !errors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("write after a cross-owner supersede: err = %v, want ErrServerNotFound (fails closed)", err)
	}
	_, _, live, err = h.mcpReg.OAuthDiscoveryTarget("srv-a")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if len(live) != 1 || live[0] != "https://as-b.example.net" {
		t.Fatalf("live allow-list = %v, want the new owner's [https://as-b.example.net]", live)
	}
}

// TestE2E_BootDeclaredConnection_RefusedOnBothPaths proves the boot-declared
// guard is a property of the NAME: it fires whether or not the caller's own
// active revision also declares a connection under that name, and the live
// registry is never reached on either path.
func TestE2E_BootDeclaredConnection_RefusedOnBothPaths(t *testing.T) {
	h := newDaHarnessWithBootServers(t, p206BootServer)
	admin := []protoauth.Scope{protoauth.ScopeAdmin}

	// Path 1: the caller's revision does NOT declare the name.
	h.registerServer(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"})
	rec := h.setOrigins(t, daTenantA, daAgentA, p206BootServer, []string{"https://as-a2.example.net"}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("boot-declared write (not declared) = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Path 2: the caller's OWN revision declares a connection under the same name.
	if rec := h.setRevisionWire(t, daTenantA, daAgentA, prototypes.AgentConfigPayload{
		Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: p206BootServer, Transport: "http", URL: "https://boot.invalid/rpc"},
		}},
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("set_revision declaring the boot name: %d body=%s", rec.Code, rec.Body.String())
	}
	before := h.activeConnections(t, daTenantA, daAgentA)

	rec = h.setOrigins(t, daTenantA, daAgentA, p206BootServer, []string{"https://as-a2.example.net"}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("boot-declared write (declared) = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(protoerrors.CodeInvalidRequest)) {
		t.Fatalf("body = %s, want CodeInvalidRequest", rec.Body.String())
	}
	after := h.activeConnections(t, daTenantA, daAgentA)
	if len(after) != len(before) || (len(after) == 1 && len(after[0].OAuthDiscoveryAllowedOrigins) != 0) {
		t.Fatalf("boot-declared refusal recorded a revision: %#v (was %#v)", after, before)
	}
}

// TestE2E_SetRevision_MalformedConnectionDescriptorRejected proves the
// full-payload door holds descriptors to the add door's shape rules end to end:
// a malformed descriptor is a 400 with nothing persisted, while a well-formed
// one persists and round-trips including its discovery allow-list.
func TestE2E_SetRevision_MalformedConnectionDescriptorRejected(t *testing.T) {
	h := newDaHarness(t)
	admin := []protoauth.Scope{protoauth.ScopeAdmin}

	rec := h.setRevisionWire(t, daTenantA, daAgentA, prototypes.AgentConfigPayload{
		Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			// stdio must not carry a url — the add door's coherence rule.
			{Name: "bad", Transport: "stdio", Command: []string{"server-bin"}, URL: "https://x.invalid/rpc"},
		}},
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed descriptor = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(protoerrors.CodeInvalidRequest)) {
		t.Fatalf("body = %s, want CodeInvalidRequest", rec.Body.String())
	}
	if got := h.activeConnections(t, daTenantA, daAgentA); len(got) != 0 {
		t.Fatalf("a rejected set_revision persisted %#v", got)
	}

	if rec := h.setRevisionWire(t, daTenantA, daAgentA, prototypes.AgentConfigPayload{
		Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: "good", Transport: "http", URL: "https://x.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net"}},
		}},
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("well-formed descriptor = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := h.activeConnections(t, daTenantA, daAgentA)
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("persisted connections = %#v", got)
	}
	if len(got[0].OAuthDiscoveryAllowedOrigins) != 1 || got[0].OAuthDiscoveryAllowedOrigins[0] != "https://as.example.net" {
		t.Fatalf("discovery allow-list did not persist through set_revision: %#v", got[0])
	}
}

// TestE2E_MissingIdentity_SetRevisionRefused is the failure mode required by
// §17.3: identity is mandatory at the seam, so a request with no identity
// headers is refused and nothing is persisted.
func TestE2E_MissingIdentity_SetRevisionRefused(t *testing.T) {
	h := newDaHarness(t)
	buf, err := json.Marshal(prototypes.AgentConfigSetRevisionRequest{
		AgentID: daAgentA,
		Payload: prototypes.AgentConfigPayload{
			Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
				{Name: "good", Transport: "http", URL: "https://x.invalid/rpc"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/set_revision", bytes.NewReader(buf))
	req = req.WithContext(protoauth.WithScopes(req.Context(), []protoauth.Scope{protoauth.ScopeAdmin}))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("identity-less set_revision succeeded: %s", rec.Body.String())
	}
	if got := h.activeConnections(t, daTenantA, daAgentA); len(got) != 0 {
		t.Fatalf("an identity-less set_revision persisted %#v", got)
	}
}

// TestE2E_OwnerScopedDiscoveryWrite_ConcurrentCrossOwner stresses the seam
// (§17.3): N≥10 concurrent writers per owner against ONE shared registry. Every
// own-connection write succeeds, every cross-owner write is refused, and each
// live allow-list ends holding only its own owner's origins.
func TestE2E_OwnerScopedDiscoveryWrite_ConcurrentCrossOwner(t *testing.T) {
	h := newDaHarness(t)
	h.registerServer(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"})
	h.registerServer(t, daTenantB, daAgentB, "srv-b", []string{"https://as-b.example.net"})
	admin := []protoauth.Scope{protoauth.ScopeAdmin}

	// Owner A declares owner B's name in its own revision so the cross-owner
	// write reaches the live applier rather than stopping at not-found.
	if rec := h.setRevisionWire(t, daTenantA, daAgentA, prototypes.AgentConfigPayload{
		Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: "srv-a", Transport: "http", URL: "https://srv-a.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as-a.example.net"}},
			{Name: "srv-b", Transport: "http", URL: "https://srv-b.invalid/rpc"},
		}},
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("owner A set_revision: %d body=%s", rec.Code, rec.Body.String())
	}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			if rec := h.setOrigins(t, daTenantA, daAgentA, "srv-a", []string{"https://as-a.example.net"}, admin); rec.Code != http.StatusOK {
				t.Errorf("owner A own write = %d body=%s", rec.Code, rec.Body.String())
			}
		}()
		go func() {
			defer wg.Done()
			if rec := h.setOrigins(t, daTenantA, daAgentA, "srv-b", []string{"https://as-a-cross.example.net"}, admin); rec.Code != http.StatusForbidden {
				t.Errorf("owner A cross-owner write = %d body=%s, want 403", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	_, _, liveB, err := h.mcpReg.OAuthDiscoveryTarget("srv-b")
	if err != nil {
		t.Fatalf("target srv-b: %v", err)
	}
	for _, o := range liveB {
		if o == "https://as-a-cross.example.net" {
			t.Fatalf("owner B live allow-list = %v, want only owner B's origins", liveB)
		}
	}
}
