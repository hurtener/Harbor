// Package integration — the same-origin MCP OAuth-discovery dial fix (HA-19)
// production-path end-to-end (§17.1).
//
// Phase 164's discovery walker enforced SSRF policy in two guards that
// disagreed: the per-hop policy PERMITTED the same-origin protected-resource
// hop to a private-address server, but the dial-time backstop UNCONDITIONALLY
// refused every private/loopback resolved IP — so discovery could never
// complete against a localhost / compose / VPC MCP server (the ordinary
// self-hosted posture). This test proves the fix on the PRODUCTION construction
// path (NO WithPrivateNetworkAccessForTest escape hatch), wiring the real
// mcpconsole probe path + the real MCPSurface dispatcher + a real events bus +
// a real audit redactor, against the spec-derived RFC 9728/8414 fixtures on
// loopback. It asserts, under -race:
//
//   - both self-hosted posture legs (an IP-literal ServerURL and a plain-HTTP
//     NON-loopback service name that resolves to the loopback fixture) complete
//     the protected-resource hop and REACH the AS hop's needs_allowance;
//   - the DNS-rebinding refusal (a same-origin name whose dial resolution moves
//     off the walk-start pin to a different private IP) is refused;
//   - the intra-host different-port refusal is refused;
//   - the cross-origin AS hop to a private address stays refused;
//   - identity propagation on the probe/read path + a missing-identity refusal
//     (≥1 failure mode; MCP config is not tenant-partitioned, so fail-closed on
//     incomplete identity is the isolation boundary here).
//
// An env-gated live leg (HARBOR_LIVE_MCP_OAUTH) against a real OAuth-protected
// MCP server on a private address is the wave's live-verification step, not CI
// (§17.8): the fixture proves the dial policy; the live leg proves it against a
// real server.
package integration

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// newProdDiscoverySurface builds an MCPSurface whose Discoverer is on the
// PRODUCTION construction path (no WithPrivateNetworkAccessForTest). An optional
// resolver maps a service name to loopback (the compose/k8s posture) or models a
// rebind — it relaxes NO SSRF guardrail; the dial pin still governs every dial.
func newProdDiscoverySurface(t *testing.T, reg *mcp.Registry, resolver *net.Resolver) *protocol.MCPSurface {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	opts := []auth.DiscovererOption{auth.WithDiscoveryTimeout(2 * time.Second)}
	if resolver != nil {
		opts = append(opts, auth.WithResolverForTest(resolver))
	}
	acc, err := mcpconsole.NewRegistryAccessor(reg,
		mcpconsole.WithOAuthDiscoverer(auth.NewDiscoverer(opts...)))
	if err != nil {
		t.Fatalf("registry accessor: %v", err)
	}
	surface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      acc,
		OAuth:    mcpconsole.NewNoOAuthAccessor(),
		Redactor: auditpatterns.New(),
		Bus:      bus,
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	return surface
}

// prResourceDoc is an RFC 9728 protected-resource doc advertising a public AS
// (so the AS hop halts at needs_allowance, the intended self-hosted outcome).
func prResourceDoc() []byte {
	return []byte(`{"resource":"x","authorization_servers":["https://as.example.net"]}`)
}

// probeAndGet runs the admin probe (triggers discovery off the captured
// challenge) then the admin get, returning the projected requirement (or nil).
func probeAndGet(t *testing.T, surface *protocol.MCPSurface, name string) *types.MCPOAuthRequirementView {
	t.Helper()
	adminCtx := protoauth.WithScopes(context.Background(), []protoauth.Scope{protoauth.ScopeAdmin})
	if _, err := surface.Dispatch(adminCtx, methods.MethodMCPServersProbe,
		&types.MCPServerProbeRequest{Identity: discoveryIdentity(), Name: name}); err != nil {
		t.Logf("probe returned (expected, stub transport): %v", err)
	}
	resp, err := surface.Dispatch(adminCtx, methods.MethodMCPServersGet,
		&types.MCPServerGetRequest{Identity: discoveryIdentity(), Name: name})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return resp.(*types.MCPServerGetResponse).Server.OAuthRequirement
}

// prViewStatus returns the protected_resource step status from a projected view.
func prViewStatus(req *types.MCPOAuthRequirementView) *types.MCPDiscoveryStepStatusView {
	if req == nil {
		return nil
	}
	for i := range req.Status {
		if req.Status[i].Step == "protected_resource" {
			return &req.Status[i]
		}
	}
	return nil
}

// asViewStatus returns the first authorization_server step status.
func asViewStatus(req *types.MCPOAuthRequirementView) *types.MCPDiscoveryStepStatusView {
	if req == nil {
		return nil
	}
	for i := range req.Status {
		if req.Status[i].Step == "authorization_server" {
			return &req.Status[i]
		}
	}
	return nil
}

func registerServer(t *testing.T, reg *mcp.Registry, id, serverURL, metadataURL string, allowed []string) {
	t.Helper()
	if err := reg.Register(context.Background(), mcp.ServerRegistration{
		Provider:                     &oauthDiscoveryStubProvider{id: tools.ToolSourceID(id)},
		Transport:                    "streamable-http",
		URLOrCommand:                 serverURL,
		InitialState:                 mcp.ServerStateOnline,
		OAuthDiscoveryAllowedOrigins: allowed,
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	reg.RecordAuthChallenge(id, mcp.AuthChallenge{
		Scheme:              "Bearer",
		ResourceMetadataURL: metadataURL,
		CapturedAt:          time.Now(),
	})
}

// Leg 1 + Leg 2: the two self-hosted posture legs COMPLETE the protected-resource
// hop on the production path and reach needs_allowance on the AS hop.
func TestE2E_Phase170_SelfHostedPostures_ReachNeedsAllowance(t *testing.T) {
	t.Run("ip_literal_serverurl", func(t *testing.T) {
		pr := &recordingFixture{routes: map[string][]byte{
			"/.well-known/oauth-protected-resource": prResourceDoc(),
		}}
		prServer := httptest.NewServer(pr)
		defer prServer.Close()

		reg := mcp.NewRegistry()
		registerServer(t, reg, "mcp-ip", prServer.URL, // http://127.0.0.1:PORT
			prServer.URL+"/.well-known/oauth-protected-resource", nil)
		surface := newProdDiscoverySurface(t, reg, nil)

		req := probeAndGet(t, surface, "mcp-ip")
		if st := prViewStatus(req); st == nil || !st.OK {
			t.Fatalf("IP-literal PR hop should complete on the production path; req=%+v", req)
		}
		if st := asViewStatus(req); st == nil || st.OK || st.Reason != "needs_allowance" {
			t.Fatalf("AS hop should reach needs_allowance; req=%+v", req)
		}
		assertOnlyMetadataFetches(t, pr, "/.well-known/oauth-protected-resource")
		assertNoDiscoveryCredentials(t, pr)
	})

	t.Run("plain_http_nonloopback_service_name", func(t *testing.T) {
		pr := &recordingFixture{routes: map[string][]byte{
			"/.well-known/oauth-protected-resource": prResourceDoc(),
		}}
		prServer := httptest.NewServer(pr)
		defer prServer.Close()
		port := hostPort(t, prServer.URL)
		svcURL := "http://mcp.svc.:" + port // plain-HTTP, non-loopback name

		reg := mcp.NewRegistry()
		registerServer(t, reg, "mcp-svc", svcURL,
			svcURL+"/.well-known/oauth-protected-resource", nil)
		// mcp.svc. → 127.0.0.1 (the loopback fixture): the compose/k8s posture.
		surface := newProdDiscoverySurface(t, reg, newFakeDNSResolver(t, "127.0.0.1"))

		req := probeAndGet(t, surface, "mcp-svc")
		if st := prViewStatus(req); st == nil || !st.OK {
			t.Fatalf("plain-HTTP non-loopback PR hop should complete on the production path; req=%+v", req)
		}
		if st := asViewStatus(req); st == nil || st.OK || st.Reason != "needs_allowance" {
			t.Fatalf("AS hop should reach needs_allowance; req=%+v", req)
		}
	})
}

// The DNS-rebinding refusal end-to-end: a same-origin service name whose dial
// resolution moves off the walk-start pin (10.99.99.99 at pin time, 127.0.0.1 at
// dial time) is refused — proven fixture-live because the DIAL target is the live
// loopback fixture, so an origin-string-only pin would WRONGLY complete.
func TestE2E_Phase170_Rebind_StillRefused(t *testing.T) {
	pr := &recordingFixture{routes: map[string][]byte{
		"/.well-known/oauth-protected-resource": prResourceDoc(),
	}}
	prServer := httptest.NewServer(pr)
	defer prServer.Close()
	port := hostPort(t, prServer.URL)
	svcURL := "http://mcp.svc.:" + port

	reg := mcp.NewRegistry()
	registerServer(t, reg, "mcp-rebind", svcURL,
		svcURL+"/.well-known/oauth-protected-resource", nil)
	// pin → dead private IP; dial → live loopback fixture (the discriminator).
	surface := newProdDiscoverySurface(t, reg, newFakeDNSResolver(t, "10.99.99.99", "127.0.0.1"))

	req := probeAndGet(t, surface, "mcp-rebind")
	if st := prViewStatus(req); st == nil || st.OK {
		t.Fatalf("rebind off the walk-start pin must be refused; req=%+v", req)
	}
}

// The intra-host different-port refusal end-to-end: a same-origin 302 to a
// different port on the pinned IP is refused (port is part of the pin).
func TestE2E_Phase170_DifferentPort_StillRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		http.Redirect(w, r, "http://"+net.JoinHostPort(host, "6379")+
			"/.well-known/oauth-protected-resource", http.StatusFound)
	}))
	defer srv.Close()

	reg := mcp.NewRegistry()
	registerServer(t, reg, "mcp-port", srv.URL,
		srv.URL+"/.well-known/oauth-protected-resource", nil)
	surface := newProdDiscoverySurface(t, reg, nil)

	req := probeAndGet(t, surface, "mcp-port")
	if st := prViewStatus(req); st == nil || st.OK {
		t.Fatalf("same-origin redirect to a different port must be refused; req=%+v", req)
	}
}

// The cross-origin AS hop to a private address stays refused on the production
// path (the relaxation is protected-resource-hop-only). The PR hop completes
// (pinned same-origin); the cross-origin loopback AS hop is refused.
func TestE2E_Phase170_CrossOriginPrivateAS_StillRefused(t *testing.T) {
	asFixture := &recordingFixture{routes: map[string][]byte{
		"/.well-known/oauth-authorization-server": specFixture(t, "rfc8414_authorization_server.json"),
	}}
	asServer := httptest.NewServer(asFixture)
	defer asServer.Close()

	pr := &recordingFixture{routes: map[string][]byte{
		"/.well-known/oauth-protected-resource": rewriteAuthServers(t, asServer.URL),
	}}
	prServer := httptest.NewServer(pr)
	defer prServer.Close()

	reg := mcp.NewRegistry()
	registerServer(t, reg, "mcp-xo", prServer.URL,
		prServer.URL+"/.well-known/oauth-protected-resource", []string{asServer.URL})
	surface := newProdDiscoverySurface(t, reg, nil)

	req := probeAndGet(t, surface, "mcp-xo")
	if st := prViewStatus(req); st == nil || !st.OK {
		t.Fatalf("same-origin PR hop should complete; req=%+v", req)
	}
	if st := asViewStatus(req); st == nil || st.OK {
		t.Fatalf("cross-origin AS hop to a private address must stay refused; req=%+v", req)
	}
	if req != nil && len(req.AuthorizationServers) != 0 {
		t.Fatalf("cross-origin private AS must not resolve: %+v", req.AuthorizationServers)
	}
}

// Identity propagation + failure mode: a valid admin identity reads the projected
// requirement; a missing-identity read is refused fail-closed (MCP config is not
// tenant-partitioned — incomplete identity is the isolation boundary here).
func TestE2E_Phase170_IdentityPropagation_MissingIdentityRefused(t *testing.T) {
	pr := &recordingFixture{routes: map[string][]byte{
		"/.well-known/oauth-protected-resource": prResourceDoc(),
	}}
	prServer := httptest.NewServer(pr)
	defer prServer.Close()

	reg := mcp.NewRegistry()
	registerServer(t, reg, "mcp-id", prServer.URL,
		prServer.URL+"/.well-known/oauth-protected-resource", nil)
	surface := newProdDiscoverySurface(t, reg, nil)

	// Propagation: a complete admin identity reads the requirement.
	if req := probeAndGet(t, surface, "mcp-id"); prViewStatus(req) == nil {
		t.Fatalf("valid-identity read should project the requirement")
	}
	// Failure mode: a missing-identity read is refused.
	adminCtx := protoauth.WithScopes(context.Background(), []protoauth.Scope{protoauth.ScopeAdmin})
	if _, err := surface.Dispatch(adminCtx, methods.MethodMCPServersGet,
		&types.MCPServerGetRequest{Identity: types.IdentityScope{}, Name: "mcp-id"}); err == nil {
		t.Fatalf("missing-identity get should be refused")
	}
}

// Env-gated live verification (§17.8): drive discovery against a real
// OAuth-protected MCP server on a private address. Skipped in CI.
func TestE2E_Phase170_LiveMCPOAuth(t *testing.T) {
	serverURL := os.Getenv("HARBOR_LIVE_MCP_OAUTH")
	if serverURL == "" {
		t.Skip("reason: set HARBOR_LIVE_MCP_OAUTH to a private-address OAuth-protected MCP server URL")
	}
	reg := mcp.NewRegistry()
	registerServer(t, reg, "mcp-live", serverURL,
		serverURL+"/.well-known/oauth-protected-resource", nil)
	surface := newProdDiscoverySurface(t, reg, nil)
	req := probeAndGet(t, surface, "mcp-live")
	if prViewStatus(req) == nil {
		t.Fatalf("live discovery projected no protected-resource status; req=%+v", req)
	}
	t.Logf("live discovery requirement: %+v", req)
}

// hostPort extracts the port from a URL (test helper).
func hostPort(t *testing.T, raw string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(raw[len("http://"):])
	if err != nil {
		t.Fatalf("split host:port from %q: %v", raw, err)
	}
	return port
}

// ---------------------------------------------------------------------------
// fakeDNS — a minimal in-test DNS server (mirrors the auth package's helper, not
// importable across packages). Answers every A query with the next IPv4 in aSeq
// (repeating the last once exhausted) and every AAAA query with NODATA, so a
// service name maps to loopback on the PRODUCTION dial path — or, with two
// elements, models a rebind (IP #1 at pin time, IP #2 at dial time).
// ---------------------------------------------------------------------------

type fakeDNS struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	aSeq  []string
	aUsed int
}

func newFakeDNSResolver(t *testing.T, aSeq ...string) *net.Resolver {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	f := &fakeDNS{conn: pc, aSeq: aSeq}
	go f.serve()
	t.Cleanup(func() { _ = pc.Close() })
	dnsAddr := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", dnsAddr)
		},
	}
}

func (f *fakeDNS) serve() {
	buf := make([]byte, 512)
	for {
		n, addr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if resp := f.respond(buf[:n]); resp != nil {
			_, _ = f.conn.WriteToUDP(resp, addr)
		}
	}
}

func (f *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	off := 12
	for off < len(q) {
		l := int(q[off])
		if l == 0 {
			off++
			break
		}
		off += l + 1
	}
	if off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	questionEnd := off + 4

	resp := make([]byte, 0, 64)
	resp = append(resp, q[0:2]...)
	resp = append(resp, 0x81, 0x80)
	resp = append(resp, 0x00, 0x01)
	ancountIdx := len(resp)
	resp = append(resp, 0x00, 0x00)
	resp = append(resp, 0x00, 0x00)
	resp = append(resp, 0x00, 0x00)
	resp = append(resp, q[12:questionEnd]...)

	var ancount uint16
	if qtype == 1 { // A
		f.mu.Lock()
		var ipStr string
		if len(f.aSeq) > 0 {
			i := f.aUsed
			if i >= len(f.aSeq) {
				i = len(f.aSeq) - 1
			}
			ipStr = f.aSeq[i]
			f.aUsed++
		}
		f.mu.Unlock()
		if ipv4 := net.ParseIP(ipStr).To4(); ipv4 != nil {
			resp = append(resp, 0xC0, 0x0C)
			resp = append(resp, 0x00, 0x01)
			resp = append(resp, 0x00, 0x01)
			resp = append(resp, 0x00, 0x00, 0x00, 0x1E)
			resp = append(resp, 0x00, 0x04)
			resp = append(resp, ipv4...)
			ancount = 1
		}
	}
	binary.BigEndian.PutUint16(resp[ancountIdx:], ancount)
	return resp
}
