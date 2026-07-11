package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"

	// The devstack snapshot below runs the dev-only mock LLM driver
	// (cfg.LLM.Driver = "mock") so the probe stack boots without an
	// external provider — test-scoped import per §4.4 (the production
	// driver set is seated by devstack's prod-aggregator import).
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// The probe identity. The token tenant deliberately differs from
// devstack.DefaultDevTenant so the topology probe (whose cross-tenant
// gate compares the caller tenant against the ENGINE's tenant, not a
// filter) is a cross-tenant read for every probe token.
const (
	probeTenant = "t-auth-probe"
	probeUser   = "u-auth-probe"
	probeSess   = "s-auth-probe"
	// otherTenant is the foreign tenant every filter probe names to
	// request cross-tenant fan-in.
	otherTenant = "t-auth-probe-other"
)

// crossTenantProbe builds one method's cross-tenant wire request: the
// JSON body (nil for the SSE subscribe, whose fan-in request travels
// as `?admin=1`) plus an optional query string appended to the route.
type crossTenantProbe struct {
	query string
	body  func(id prototypes.IdentityScope) any
}

// crossTenantProbes is the per-method cross-tenant request builder for
// every methodTable row whose crossTenantPolicy claims a REJECTING
// cross-tenant gate (crossTenantAdminOnly / crossTenantAdminOrFleet).
// TestGen_AuthColumnMatchesHandlerGates pins the map both directions:
// a noted row without a probe fails (the note would be unverified), a
// probe for an un-noted row fails (the note was removed without
// retiring the probe).
var crossTenantProbes = map[methods.Method]crossTenantProbe{
	methods.MethodEventsSubscribe: {query: "?admin=1"},
	methods.MethodEventsAggregate: {body: func(id prototypes.IdentityScope) any {
		return prototypes.EventAggregateRequest{
			Identity: id,
			Filter:   prototypes.EventFilter{TenantIDs: []string{otherTenant}},
			Window:   time.Hour,
			Bucket:   time.Minute,
		}
	}},
	methods.MethodEventsList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.EventsListRequest{
			Identity: id,
			Filter:   prototypes.EventFilter{TenantIDs: []string{otherTenant}},
		}
	}},
	methods.MethodSearchQuery:       {body: searchProbeBody},
	methods.MethodSearchSessions:    {body: searchProbeBody},
	methods.MethodSearchTasks:       {body: searchProbeBody},
	methods.MethodSearchEvents:      {body: searchProbeBody},
	methods.MethodSearchArtifacts:   {body: searchProbeBody},
	methods.MethodRuntimeInfo:       {body: postureProbeBody},
	methods.MethodRuntimeHealth:     {body: postureProbeBody},
	methods.MethodRuntimeCounters:   {body: postureProbeBody},
	methods.MethodRuntimeDrivers:    {body: postureProbeBody},
	methods.MethodMetricsSnapshot:   {body: postureProbeBody},
	methods.MethodGovernancePosture: {body: postureProbeBody},
	methods.MethodLLMPosture:        {body: postureProbeBody},
	methods.MethodPauseList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.PauseListRequest{
			Identity: id,
			Filter:   prototypes.PauseFilter{TenantIDs: []string{otherTenant}},
		}
	}},
	// The topology gate compares the caller tenant against the engine's
	// tenant (devstack.DefaultDevTenant) — the probe identity is already
	// cross-tenant, so the body is the caller's own scope.
	methods.MethodTopologySnapshot: {body: func(id prototypes.IdentityScope) any {
		return prototypes.TopologySnapshotRequest{Identity: id}
	}},
	methods.MethodArtifactsList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.ArtifactsListRequest{
			Scope: prototypes.ArtifactScope{Tenant: otherTenant, User: id.User, Session: id.Session},
		}
	}},
	methods.MethodMemoryList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.MemoryListRequest{
			Identity: id,
			Filter:   prototypes.MemoryFilter{TenantIDs: []string{otherTenant}},
		}
	}},
	methods.MethodTasksList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.TaskListRequest{
			Identity: id,
			Filter: prototypes.TaskFilter{Identities: []prototypes.IdentityScope{
				{Tenant: otherTenant, User: id.User, Session: id.Session},
			}},
		}
	}},
	methods.MethodSessionsList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.SessionsListRequest{
			Identity: id,
			Filter:   prototypes.SessionFilter{TenantIDs: []string{otherTenant}},
		}
	}},
	methods.MethodFlowsList: {body: func(id prototypes.IdentityScope) any {
		return prototypes.FlowListRequest{
			Identity: id,
			Filter:   prototypes.FlowFilter{Tenants: []string{otherTenant}},
		}
	}},
	methods.MethodFlowsRunsList: {body: func(id prototypes.IdentityScope) any {
		// The cross-tenant gate fires before the flow-id lookup, so a
		// placeholder id still exercises the gate; the admin leg may
		// legitimately answer 404 (gate passed, flow unknown).
		return prototypes.FlowRunsListRequest{
			Identity: id,
			FlowID:   "auth-probe-flow",
			Tenants:  []string{otherTenant},
		}
	}},
}

func searchProbeBody(id prototypes.IdentityScope) any {
	return prototypes.SearchRequest{
		Filter: prototypes.SearchFilter{TenantIDs: []string{otherTenant}},
	}
}

func postureProbeBody(id prototypes.IdentityScope) any {
	// The posture gate compares the body tenant against the verified
	// tenant — a foreign body tenant is the cross-tenant read shape.
	return prototypes.RuntimeInfoRequest{
		Identity: prototypes.IdentityScope{Tenant: otherTenant, User: id.User, Session: id.Session},
	}
}

// TestGen_AuthColumnMatchesHandlerGates pins the methods.md Auth column
// against the deployed handler gates — the one join the §17.5
// Protocol-track audit found drifting (the doc claimed `console:fleet`
// cross-tenant fan-in on ~10 admin-only or no-fan-in surfaces, and
// omitted the note on the seven posture rows where it actually
// applies). For every row whose crossTenantPolicy claims a rejecting
// gate, the test drives a baseline, an admin-only, and a fleet-only
// token against the assembled dev stack and asserts the observed
// accept/reject matches the policy the rendered note derives from:
//
//   - baseline (no elevated scope) → rejected (401/403),
//   - admin-only                   → accepted,
//   - fleet-only                   → accepted iff the policy is
//     crossTenantAdminOrFleet; rejected when crossTenantAdminOnly.
//
// The probe map is itself lockstep-checked both directions so a policy
// change without a probe change fails before any wire traffic.
func TestGen_AuthColumnMatchesHandlerGates(t *testing.T) {
	table := methodTable()

	// Lockstep: probes exist exactly for the rows that claim a
	// rejecting cross-tenant gate.
	var probed []methods.Method
	for m, e := range table {
		_, hasProbe := crossTenantProbes[m]
		gated := e.CrossTenant == crossTenantAdminOnly || e.CrossTenant == crossTenantAdminOrFleet
		switch {
		case gated && !hasProbe:
			t.Errorf("method %q claims a rejecting cross-tenant gate but has no crossTenantProbes entry — the Auth note would go unverified", m)
		case !gated && hasProbe:
			t.Errorf("method %q has a crossTenantProbes entry but its crossTenantPolicy claims no rejecting gate — retire the probe or fix the policy", m)
		case gated:
			probed = append(probed, m)
		}
	}
	for m := range crossTenantProbes {
		if _, ok := table[m]; !ok {
			t.Errorf("crossTenantProbes names %q, which has no methodTable row", m)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	sort.Slice(probed, func(i, j int) bool { return probed[i] < probed[j] })

	// A real engine.Engine backs the TopologyAccessor (production
	// `harbor dev` hosts no engine graph, so the devstack leaves the
	// accessor nil and `topology.snapshot` would be unwired — the
	// Phase 74 integration test wires the same fixture shape). The
	// engine tenant is devstack.DefaultDevTenant, so the probe tokens
	// (probeTenant) are cross-tenant readers by construction.
	eng, err := engine.New([]engine.Adjacency{
		{From: engine.Node{Name: "ingress", Func: probeNodeFunc},
			To: []engine.Node{{Name: "egress", Func: probeNodeFunc}}},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	stack := devstack.Assemble(t, probeConfig(t), devstack.AssembleOpts{
		TopologyAccessor: &probeEngineAccessor{eng: eng, tenant: devstack.DefaultDevTenant},
	})
	defer stack.Close()
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	id := prototypes.IdentityScope{Tenant: probeTenant, User: probeUser, Session: probeSess}
	baselineTok := signProbeToken(t, stack.SigningKey, nil)
	adminTok := signProbeToken(t, stack.SigningKey, []string{string(auth.ScopeAdmin)})
	fleetTok := signProbeToken(t, stack.SigningKey, []string{string(auth.ScopeConsoleFleet)})

	for _, m := range probed {
		e := table[m]
		probe := crossTenantProbes[m]

		base := doProbe(t, srv, e.Route, probe, id, baselineTok)
		if !scopeRejected(base) {
			t.Errorf("method %q: baseline (no elevated scope) cross-tenant probe answered %d, want 401/403 — either the gate is gone or the probe no longer requests fan-in", m, base)
		}
		admin := doProbe(t, srv, e.Route, probe, id, adminTok)
		if scopeRejected(admin) {
			t.Errorf("method %q: admin-only token rejected (%d) but the Auth column claims admin cross-tenant fan-in", m, admin)
		}
		fleet := doProbe(t, srv, e.Route, probe, id, fleetTok)
		switch e.CrossTenant {
		case crossTenantAdminOrFleet:
			if scopeRejected(fleet) {
				t.Errorf("method %q: Auth column claims `console:fleet` satisfies the gate but the wire rejected the fleet-only token (%d) — fix the row's crossTenantPolicy", m, fleet)
			}
		case crossTenantAdminOnly:
			if !scopeRejected(fleet) {
				t.Errorf("method %q: Auth column claims `console:fleet` is not consulted but the wire accepted the fleet-only token (%d) — fix the row's crossTenantPolicy", m, fleet)
			}
		}
	}
}

// probeNodeFunc is the no-op node the probe engine's graph is built
// from — the test reads the topology surface, never runs the graph.
func probeNodeFunc(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return env, nil
}

// probeEngineAccessor adapts the real engine.Engine into a
// protocol.TopologyAccessor (the same adapter shape the Phase 74
// integration test and an engine-bearing production runtime use).
type probeEngineAccessor struct {
	eng    engine.Engine
	tenant string
}

func (a *probeEngineAccessor) Topology(ctx context.Context) (prototypes.TopologyProjection, error) {
	return a.eng.Topology(ctx)
}

func (a *probeEngineAccessor) TenantID() string { return a.tenant }

var _ protocol.TopologyAccessor = (*probeEngineAccessor)(nil)

// scopeRejected reports whether status is an auth/scope rejection. A
// 404 is deliberately NOT a rejection: on the baseline leg it means the
// surface is unwired (fail loudly), on the admin leg it means the gate
// passed and the probe's placeholder target was unknown (accepted).
func scopeRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// doProbe issues one probe request and returns the HTTP status.
func doProbe(t *testing.T, srv *httptest.Server, route string, probe crossTenantProbe, id prototypes.IdentityScope, token string) int {
	t.Helper()

	verb, path, ok := strings.Cut(route, " ")
	if !ok {
		t.Fatalf("route %q does not split into verb + path", route)
	}

	var bodyReader *bytes.Reader
	if probe.body != nil {
		raw, err := json.Marshal(probe.body(id))
		if err != nil {
			t.Fatalf("marshal probe body for %s: %v", route, err)
		}
		bodyReader = bytes.NewReader(raw)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, verb, srv.URL+path+probe.query, bodyReader)
	if err != nil {
		t.Fatalf("build probe request for %s: %v", route, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if probe.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("probe %s: %v", route, err)
	}
	// The SSE subscribe streams forever on accept — the status header
	// is all the probe needs; closing the body tears the stream down.
	resp.Body.Close()
	return resp.StatusCode
}

// signProbeToken mints an ES256 token under the probe identity with
// the supplied scope claims, mirroring devstack's dev-signer claim
// shape (`iss`/`aud`/`kid` must match the stack's validator).
func signProbeToken(t *testing.T, priv *ecdsa.PrivateKey, scopes []string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":     "harbor-test",
		"sub":     probeUser,
		"aud":     "harbor",
		"exp":     now.Add(time.Hour).Unix(),
		"nbf":     now.Add(-1 * time.Minute).Unix(),
		"iat":     now.Unix(),
		"tenant":  probeTenant,
		"user":    probeUser,
		"session": probeSess,
		"scopes":  scopes,
	})
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign probe token: %v", err)
	}
	return signed
}

// probeConfig mirrors devstack's minimal all-inmem snapshot: every
// driver in-memory, the dev-only mock LLM, memory strategy none.
func probeConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:0",
			ShutdownGracePeriod: 1 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"ES256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "text",
			LogLevel:    "error",
			ServiceName: "harbor-gen-protocol-docs-auth-probe",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
			ModelProfiles: map[string]config.LLMModelProfileConfig{
				"mock/echo": {
					ContextWindowTokens: 100000,
					TokenEstimator:      "chars_div_4",
				},
			},
		},
		Governance: config.GovernanceConfig{RepairAttempts: 1},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              2 * time.Second,
			DropWindow:               50 * time.Millisecond,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       24 * time.Hour,
			SweepInterval: 5 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    1 * time.Minute,
			ContinuationHopLimit: 4,
		},
		Distributed: config.DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: config.MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 8,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("probeConfig: cfg.Validate(): %v", err)
	}
	return cfg
}
