package tokenexchange_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	// This harness composes only the real credential and MCP drivers under test.
	mcpdriver "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const governedMintRefusalJSON = `{"error":"access_denied","error_description":"subject not authorized","reason":"governed_app_operation_required"}`

func TestGovernedMintRefusalRequiresExactTrustedBrokerProof(t *testing.T) {
	for _, tc := range []struct {
		name, body, mode string
		status           int
		want             bool
	}{
		{"exact", governedMintRefusalJSON, "", 403, true},
		{"missing-reason", `{"error":"access_denied"}`, "", 403, false},
		{"wrong-reason", `{"error":"access_denied","reason":"other"}`, "", 403, false},
		{"wrong-code", `{"error":"invalid_client","reason":"governed_app_operation_required"}`, "", 403, false},
		{"wrong-status", governedMintRefusalJSON, "", 401, false},
		{"server-error", governedMintRefusalJSON, "", 500, false},
		{"malformed", strings.TrimSuffix(governedMintRefusalJSON, "}"), "", 403, false},
		{"wrong-reason-type", `{"error":"access_denied","reason":7}`, "", 403, false},
		{"truncated-body", governedMintRefusalJSON + strings.Repeat(" ", 1<<16) + "invalid", "", 403, false},
		{"incomplete-body", governedMintRefusalJSON, "incomplete-body", 403, false},
		{"description-only", `{"error":"access_denied","error_description":"governed_app_operation_required"}`, "", 403, false},
		{"unsigned", governedMintRefusalJSON, "unsigned", 403, false},
		{"preparation", governedMintRefusalJSON, "preparation", 403, false},
		{"bound-operation", governedMintRefusalJSON, "operation", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.mode == "incomplete-body" {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+10))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(broker.Close)
			binding := signedCapabilityBindingFixture("https://downstream.example")
			cfg := auth.ProviderConfig{Name: binding.ProviderName, TokenURL: broker.URL, Audience: binding.Audience, ResourceIndicator: binding.Resource,
				AllowedDownstreamHosts: []string{"downstream.example"}, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret), SignedCapability: &binding}
			if tc.mode == "unsigned" {
				cfg.SignedCapability = nil
			}
			deps, _, _ := mkDeps(t)
			p, err := tokenexchange.New(cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close(context.Background()) })
			ctx := tools.WithEffectiveAgentConfig(mkCtx(t, identity.Identity{TenantID: binding.TenantID, UserID: "caller", SessionID: "session"}), binding.AgentID)
			if tc.mode == "preparation" {
				ctx = auth.WithSignedCapabilityPreparation(tools.WithInvokingAgent(ctx, binding.AgentID))
			}
			if tc.mode == "operation" {
				ctx, err = appoperation.WithRead(ctx, strings.Repeat("a", 64), binding.AgentID, "server", "ui://resource")
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = p.Token(ctx, "server")
			var governed *auth.GovernedAppMintRequiredError
			if errors.As(err, &governed) != tc.want || calls.Load() != 1 {
				t.Fatalf("classification want%t calls%d: %v", tc.want, calls.Load(), err)
			}
			if tc.status < 500 && (!errors.Is(err, auth.ErrCredentialRejected) || tools.ClassifyError(err, false) != tools.ErrClassPermanent) {
				t.Fatalf("lost permanent credential refusal: %v", err)
			}
		})
	}
}

func TestGovernedMintRefusalPreservesSharedSourceAndBoundResource(t *testing.T) {
	const resource = "ui://retained/workspace"
	var upstreamFailure atomic.Bool
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "governed-fixture", Version: "v0"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "metadata", InputSchema: map[string]any{"type": "object"}, Meta: mcpsdk.Meta{"ui": map[string]any{"visibility": []string{"app"}}}}, nil)
	server.AddResource(&mcpsdk.Resource{URI: resource, Name: "workspace", MIMEType: "text/html"}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: resource, MIMEType: "text/html", Text: "<html>retained</html>"}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var sourceRequests atomic.Int64
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		if upstreamFailure.Load() {
			http.Error(w, "source unavailable", 503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer catalog-token" && r.Header.Get("Authorization") != "Bearer resource-token" {
			http.Error(w, "denied", 403)
			return
		}
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var rpc struct{ Method string }
			if json.Unmarshal(raw, &rpc) != nil {
				http.Error(w, "invalid RPC", 400)
				return
			}
			if rpc.Method == "resources/read" && r.Header.Get("Authorization") != "Bearer resource-token" {
				http.Error(w, "bound resource bearer required", 403)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	binding := signedCapabilityBindingFixture(front.URL)
	useGate := &appOperationUseGate{}
	binding.UseAuthorizer = useGate
	coordinator := identity.Identity{TenantID: binding.TenantID, UserID: "svc:fixture-coordinator", SessionID: "management-session"}
	owner := auth.Owner{Tenant: binding.TenantID, Agent: binding.AgentID}
	physical := mcpdriver.PhysicalServerName("paired-source", owner)
	var refusalMode atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		subject := decodeSubject(r.Form.Get("subject_token"))
		if subject.TenantID != binding.TenantID {
			http.Error(w, "denied", 403)
			return
		}
		operationRaw := r.Form.Get("app_operation")
		if subject.UserID != coordinator.UserID && operationRaw == "" {
			switch refusalMode.Load() {
			case 1:
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			case 2:
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"access_denied"}`)
			default:
				w.WriteHeader(403)
				_, _ = io.WriteString(w, governedMintRefusalJSON)
			}
			return
		}
		response := map[string]any{"access_token": "catalog-token", "token_type": "Bearer", "expires_in": 3600, "audience": binding.Audience, "resource": binding.Resource}
		if operationRaw != "" {
			var operation appoperation.Binding
			if json.Unmarshal([]byte(operationRaw), &operation) != nil || operation.AgentID != owner.Agent || operation.CheckResource(physical, resource) != nil {
				http.Error(w, "invalid operation", 403)
				return
			}
			response["access_token"], response["expires_in"], response["app_operation_sha256"] = "resource-token", 30, operation.Digest()
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(broker.Close)
	deps, _, _ := mkDeps(t)
	oauth, err := tokenexchange.New(auth.ProviderConfig{Name: binding.ProviderName, TokenURL: broker.URL, Audience: binding.Audience, ResourceIndicator: binding.Resource,
		AllowedDownstreamHosts: []string{"127.0.0.1"}, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret), SignedCapability: &binding}, deps)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcpdriver.New(mcpdriver.Config{Name: physical, URL: front.URL, TransportMode: mcpdriver.TransportStreamableHTTP, Bus: deps.Bus, OAuthProvider: oauth, OwnOAuthProvider: true, DefaultIdentity: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	base, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctxFor := func(id identity.Identity) context.Context {
		ctx, err := identity.With(base, id)
		if err != nil {
			t.Fatal(err)
		}
		return tools.WithEffectiveAgentConfig(ctx, owner.Agent)
	}
	management := ctxFor(coordinator)
	if err := p.Connect(management); err != nil {
		t.Fatal(err)
	}
	descs, err := p.Discover(management)
	if err != nil {
		t.Fatal(err)
	}
	reg := mcpdriver.NewRegistry()
	if err := reg.Register(management, mcpdriver.ServerRegistration{Provider: p, Owner: owner, LogicalName: "paired-source", Transport: "streamable-http", InitialState: mcpdriver.ServerStateOnline}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordDiscovery(physical, descs); err != nil {
		t.Fatal(err)
	}
	caller := identity.Identity{TenantID: binding.TenantID, UserID: "normal-user", SessionID: "normal-session"}
	ctx := ctxFor(caller)
	before, err := reg.GetServer(ctx, physical)
	if err != nil {
		t.Fatal(err)
	}
	generation, ok := reg.CurrentGeneration(physical)
	if !ok {
		t.Fatal("no accepted catalog generation")
	}
	beforeRequests := sourceRequests.Load()
	if _, err := reg.RefreshDiscovery(ctx, physical); err == nil {
		t.Fatal("generic refresh silently succeeded")
	}
	probe, err := reg.Probe(ctx, physical)
	if err != nil || probe.OK || probe.Error == "" {
		t.Fatalf("probe must honestly fail: %+v %v", probe, err)
	}
	assertRetained := func() {
		t.Helper()
		after, err := reg.GetServer(ctx, physical)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("caller refusal changed shared state: before%+v after%+v error%v", before, after, err)
		}
		if next, _ := reg.CurrentGeneration(physical); next != generation {
			t.Fatal("caller refusal changed catalog generation")
		}
		if _, found := reg.ResolveAppTool(physical, physical+"_metadata"); !found {
			t.Fatal("caller refusal removed App metadata")
		}
	}
	assertRetained()
	if sourceRequests.Load() != beforeRequests {
		t.Fatal("refused generic credential reached source")
	}
	// Existing exact operation binding and provider acknowledgment still gate
	// the actual SDK resource read after the rejected generic control calls.
	readCtx, err := appoperation.WithRead(ctx, strings.Repeat("a", 64), owner.Agent, physical, resource)
	if err != nil {
		t.Fatal(err)
	}
	body, mime, err := reg.ReadResource(readCtx, physical, resource)
	if err != nil || string(body) != "<html>retained</html>" || mime != "text/html" {
		t.Fatalf("closed resource: %q %s %v", body, mime, err)
	}
	assertRetained()
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := caller
			id.SessionID = fmt.Sprintf("refused-%d", i)
			if _, err := reg.RefreshDiscovery(ctxFor(id), physical); err == nil {
				t.Error("concurrent refusal succeeded")
			}
		}()
	}
	wg.Wait()
	assertRetained()
	// Other broker/configuration failures remain shared failures.
	for _, mode := range []int64{1, 2} {
		refusalMode.Store(mode)
		if _, err := reg.RefreshDiscovery(ctx, physical); err == nil {
			t.Fatal("other broker refusal succeeded")
		}
		view, _ := reg.GetServer(ctx, physical)
		if view.State != mcpdriver.ServerStateError {
			t.Fatalf("other broker refusal lost Error state: %+v", view)
		}
		if _, err := reg.RefreshDiscovery(management, physical); err != nil {
			t.Fatal(err)
		}
	}
	useGate.revoked.Store(true)
	if _, err := reg.RefreshDiscovery(management, physical); err == nil {
		t.Fatal("revoked current publisher use succeeded")
	}
	view, _ := reg.GetServer(ctx, physical)
	if view.State != mcpdriver.ServerStateError {
		t.Fatal("current publisher denial lost Error state")
	}
	useGate.revoked.Store(false)
	if _, err := reg.RefreshDiscovery(management, physical); err != nil {
		t.Fatal(err)
	}
	upstreamFailure.Store(true)
	if _, err := reg.RefreshDiscovery(management, physical); err == nil {
		t.Fatal("upstream failure succeeded")
	}
	view, _ = reg.GetServer(ctx, physical)
	if view.State != mcpdriver.ServerStateError {
		t.Fatal("real upstream failure lost Error state")
	}
}
