package tokenexchange_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
	mcpdriver "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compose production planner dispatch, MCP SDK transport, and signed exchange.
// The broker compares bound input with the actual HTTP tools/call below.
func TestAgentOperation_PlannerDispatchFreshBindingAndNoRetry(t *testing.T) {
	var ordinary, bound, calls, reads atomic.Int64
	var acknowledge atomic.Bool
	var fail atomic.Bool
	acknowledge.Store(true)
	var mu sync.Mutex
	refs := map[string]bool{}
	broker, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		raw := r.Form.Get("app_operation")
		if raw == "" {
			ordinary.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, governedMintRefusalJSON)
			return
		}
		bound.Add(1)
		var b appoperation.Binding
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Error(err)
			return
		}
		if b.AgentID != "agent-signed" || b.ServerID != "server" || r.Form.Get("scope") != "" || r.Form.Get("actor_token") == "" || r.Form.Get("subject_token") == "" {
			t.Error("lost authenticated coordinates or inherited broad scope")
		}
		if b.Kind == "agent_tool" {
			if b.Tool != "server_save" || string(b.Arguments) != `{"doc":"cGF5bG9hZA==","n":9007199254740993}` || b.ResourceURI != "" || b.Generation != "" {
				t.Errorf("wrong final operation: %+v", b)
			}
		} else if b.Kind != "agent_resource" || b.ResourceURI != "ui://workspace" || len(b.Arguments) != 0 {
			t.Errorf("wrong resource operation: %+v", b)
		}
		mu.Lock()
		if refs[b.Reference] {
			t.Error("operation reference reused")
		}
		refs[b.Reference] = true
		mu.Unlock()
		writeAppToken(t, w, r, 30, acknowledge.Load())
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx, err := identity.WithRun(ctx, identity.MustFrom(ctx), "agent-operation-run")
	if err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agent-operation-fixture", Version: "v0"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "save", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"doc": map[string]any{"type": "string"},
			"n":   map[string]any{"type": "integer"},
		},
	}}, func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		if !strings.Contains(string(r.Params.Arguments), "9007199254740993") || strings.Contains(string(r.Params.Arguments), "artifact-id") {
			t.Error("outgoing input corrupted")
		}
		if fail.Load() {
			return nil, errors.New("lost result after effect")
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "saved"}}}, nil
	})
	server.AddResource(&mcpsdk.Resource{URI: "ui://workspace", Name: "workspace", MIMEType: "text/html"}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		reads.Add(1)
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "ui://workspace", MIMEType: "text/html", Text: "<html></html>"}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var rpc struct {
				Method string
				Params struct {
					Name      string
					URI       string
					Arguments json.RawMessage
				}
			}
			if err := json.Unmarshal(body, &rpc); err != nil {
				t.Error(err)
				return
			}
			if rpc.Method == "tools/call" || rpc.Method == "resources/read" {
				var binding appoperation.Binding
				if err := json.Unmarshal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), &binding); err != nil {
					t.Error("missing operation bearer")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if rpc.Method == "tools/call" {
					args, err := appoperation.DecodeArguments(rpc.Params.Arguments)
					if err != nil || binding.CheckCall("server", "server_"+rpc.Params.Name, args) != nil {
						t.Error("wire differs from acknowledged operation")
						w.WriteHeader(http.StatusForbidden)
						return
					}
				} else if binding.CheckResource("server", rpc.Params.URI) != nil {
					t.Error("wrong bound resource")
					w.WriteHeader(http.StatusForbidden)
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	mapping, err := artifactegress.CompileMapping(map[string][]string{"save": {"doc"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcpdriver.New(mcpdriver.Config{Name: "server", URL: front.URL, TransportMode: mcpdriver.TransportStreamableHTTP,
		OAuthProvider: broker, DefaultIdentity: aliceID(), Bus: mkBus(t, mkRedactor()), ArtifactEgress: mapping, ArtifactEgressMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	var invoke tools.ToolDescriptor
	var resource string
	for _, d := range descs {
		if err := cat.Register(d); err != nil {
			t.Fatal(err)
		}
		if d.Tool.Name == "server_save" {
			invoke = d
		}
		if d.Tool.Form == tools.ToolFormResource {
			resource = d.Tool.Name
		}
	}
	if invoke.Invoke == nil || resource == "" {
		t.Fatal("missing discovered descriptors")
	}
	ctx = artifactref.WithResolver(ctx, artifactref.ResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("payload"), nil }))
	args := json.RawMessage(`{"doc":"artifact-id","n":9007199254740993}`)
	// An effective selection alone does not mark a browser/direct call trusted.
	if _, err := invoke.Invoke(ctx, args); err == nil || bound.Load() != 0 || calls.Load() != 0 {
		t.Fatal("untrusted direct call promoted")
	}
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	id := identity.MustFrom(ctx)
	ref, err := store.PutText(ctx, artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, "payload", artifacts.PutOpts{})
	if err != nil {
		t.Fatal(err)
	}
	plannerArgs, err := json.Marshal(map[string]any{"doc": ref.ID, "n": json.Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	exec := dispatch.NewToolExecutor(cat, store, nil)
	rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: "agent-operation-run"}, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})}
	call := planner.CallTool{Tool: "server_save", Args: plannerArgs}
	for range 2 {
		if _, _, err := exec.ExecuteDecision(ctx, rc, call); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || bound.Load() != 2 {
		t.Fatalf("fresh pulls lost: calls=%d bound=%d", calls.Load(), bound.Load())
	}
	if _, _, err := exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: resource}); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 || bound.Load() != 3 {
		t.Fatal("planner resource binding missing")
	}
	acknowledge.Store(false)
	if _, _, err := exec.ExecuteDecision(ctx, rc, call); err == nil {
		t.Fatal("unacknowledged binding accepted")
	}
	if calls.Load() != 2 || bound.Load() != 4 {
		t.Fatal("missing acknowledgement reached wire or retried")
	}
	acknowledge.Store(true)
	fail.Store(true)
	if _, _, err := exec.ExecuteDecision(ctx, rc, call); err == nil {
		t.Fatal("unknown effect returned success")
	}
	if calls.Load() != 3 || bound.Load() != 5 {
		t.Fatalf("unknown effect retried: calls=%d bound=%d", calls.Load(), bound.Load())
	}
	if ordinary.Load() != 6 {
		t.Fatalf("generic refusal probes=%d", ordinary.Load())
	}
}
