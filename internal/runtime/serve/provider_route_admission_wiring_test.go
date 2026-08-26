package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestBoot_ProviderRouteUsesSharedKEKReachAdmissionFallback pins the boot-order
// invariant for runtimes without an OAuth provider. The control surface must
// capture the admission authority derived from the configured shared KEK;
// otherwise control.start persists no authenticated reach receipt and the run
// loop rejects every route-bearing task as provider_route_unauthorized.
func TestBoot_ProviderRouteUsesSharedKEKReachAdmissionFallback(t *testing.T) {
	const (
		kekEnv         = "HARBOR_PROVIDER_ROUTE_ADMISSION_TEST_KEK"
		providerKeyEnv = "HARBOR_PROVIDER_ROUTE_ADMISSION_PROVIDER_KEY"
		dummyKEKHex    = "0303030303030303030303030303030303030303030303030303030303030303"
	)
	t.Setenv(kekEnv, dummyKEKHex)
	t.Setenv(providerKeyEnv, "provider-route-admission-fixture-key")
	providerResponse := []byte(`{"id":"route-admission","object":"chat.completion","created":1700000000,"model":"route-model","choices":[{"index":0,"message":{"role":"assistant","content":"{\"tool\":\"_finish\",\"args\":{\"answer\":\"done\"}}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	if !json.Valid(providerResponse) {
		t.Fatal("provider route admission fixture response must be valid JSON")
	}
	var requestCount atomic.Int64
	var requestBodyJSON atomic.Bool
	var requestMethod, requestPath atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		requestMethod.Store(r.Method)
		requestPath.Store(r.URL.Path)
		requestBody, _ := io.ReadAll(r.Body)
		requestBodyJSON.Store(json.Valid(requestBody))
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(requestBody, &request)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"route-admission\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"route-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"tool\\\":\\\"_finish\\\",\\\"args\\\":{\\\"answer\\\":\\\"done\\\"}}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponse)
	}))
	defer provider.Close()

	signer := newTestSigner(t)
	opts := baseOptions(t)
	llmBlock := fmt.Sprintf(`llm:
  driver: bifrost
  provider: route-provider
  model: route-model
  timeout: 8s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: route-provider
      base_url: %s
      api_key_env_var: %s
      models: [route-model]
      timeout: 8s
      max_retries: 0
  model_profiles:
    route-model:
      context_window_tokens: 32768
      token_estimator: chars_div_4
`, provider.URL, providerKeyEnv)
	body := strings.Replace(serveTestYAML, "llm:\n  driver: mock\n  timeout: 30s\n  context_window_reserve: 0.05\n", llmBlock, 1)
	if body == serveTestYAML {
		t.Fatal("provider-route fixture did not replace the mock LLM block")
	}
	body += "\ntools:\n  oauth_token_kek_env: " + kekEnv + "\n"
	if err := os.WriteFile(opts.ConfigPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write shared-KEK config: %v", err)
	}
	opts.AuthValidatorFactory = signer.factory()
	var taskRegistry tasks.TaskRegistry
	previousPostBoot := opts.PostBoot
	opts.PostBoot = func(ctx context.Context, handles PostBootHandles) error {
		taskRegistry = handles.Tasks
		if previousPostBoot != nil {
			return previousPostBoot(ctx, handles)
		}
		return nil
	}
	endpointValue, endpointDigest, err := llm.NormalizeProviderEndpoint(provider.URL)
	if err != nil {
		t.Fatalf("normalize provider endpoint: %v", err)
	}
	opts.ProviderRoute = llm.ProviderRouteConfig{Resolver: admissionRouteResolver{endpoint: &llm.ProviderEndpointBinding{
		Kind: llm.ProviderEndpointOpenAICompatible, Value: endpointValue, Digest: endpointDigest,
	}}, RuntimeID: "route-runtime"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		h.Close(closeCtx)
	})

	id := identity.Identity{TenantID: "route-tenant", UserID: "route-user", SessionID: "route-session"}
	token := signWithAgentReach(t, signer, id, []string{"admin"}, []string{"harbor-dev-agent"})
	infoBody, err := json.Marshal(types.RuntimeInfoRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}})
	if err != nil {
		t.Fatalf("marshal runtime.info request: %v", err)
	}
	status, raw := postProviderRouteAdmission(t, h.Handler(), token, "/v1/control/runtime.info", infoBody)
	if status != http.StatusOK {
		t.Fatalf("runtime.info status = %d, body = %s", status, raw)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode runtime.info: %v", err)
	}
	if !hasProviderRouteCapability(info.Capabilities) {
		t.Fatalf("configured runtime capabilities = %v, want %q", info.Capabilities, types.CapLLMProviderRoute)
	}
	startBody, err := json.Marshal(types.StartRequest{
		Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:    "shared admission fallback",
		ProviderRoute: &types.LLMProviderRouteSelector{
			RouteID:                      "route-1",
			RouteGeneration:              1,
			ProviderConnectionID:         "connection-1",
			ProviderConnectionGeneration: 1,
			CredentialAssetGeneration:    1,
			ModelSelector:                "model-alias-1",
		},
	})
	if err != nil {
		t.Fatalf("marshal start request: %v", err)
	}
	status, raw = postProviderRouteAdmission(t, h.Handler(), token, "/v1/control/start", startBody)
	if status != http.StatusOK {
		t.Fatalf("control.start status = %d, body = %s", status, raw)
	}
	var started types.StartResponse
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.TaskID == "" {
		t.Fatal("control.start returned an empty task id")
	}

	// Boot starts the run loop asynchronously; allow the bounded provider
	// exchange and first task scheduling enough time on hosted runners while
	// retaining a finite failure deadline for a genuinely stuck task.
	deadline := time.Now().Add(10 * time.Second)
	for {
		getBody, err := json.Marshal(types.TaskGetRequest{
			Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
			ID:       started.TaskID,
		})
		if err != nil {
			t.Fatalf("marshal tasks.get request: %v", err)
		}
		status, raw = postProviderRouteAdmission(t, h.Handler(), token, "/v1/tasks/get", getBody)
		if status != http.StatusOK {
			t.Fatalf("tasks.get status = %d, body = %s", status, raw)
		}
		var detail types.TaskDetail
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode tasks.get response: %v", err)
		}
		switch detail.Task.Status {
		case types.TaskStatusComplete:
			return
		case types.TaskStatusFailed, types.TaskStatusCancelled:
			message := "<task registry unavailable>"
			if taskRegistry != nil {
				inspectCtx, inspectErr := identity.With(context.Background(), id)
				if inspectErr == nil {
					if task, getErr := taskRegistry.Get(inspectCtx, tasks.TaskID(started.TaskID)); getErr != nil {
						message = "<task registry get failed: " + getErr.Error() + ">"
					} else {
						message = strings.NewReplacer(provider.URL, "<endpoint>", "provider-route-attempt-fixture-key", "<credential>", "provider-route-admission-fixture-key", "<credential>").Replace(task.Error.Message)
					}
				}
			}
			method, _ := requestMethod.Load().(string)
			path, _ := requestPath.Load().(string)
			t.Fatalf("route-bearing task reached %q with error class %q; task error=%q; tasks.get=%s; provider requests=%d method=%q path=%q body_json=%t; shared-KEK admission was not restored",
				detail.Task.Status, detail.Task.ErrorClass, message, raw, requestCount.Load(), method, path, requestBodyJSON.Load())
		}
		if time.Now().After(deadline) {
			t.Fatalf("route-bearing task remained %q; last tasks.get=%s; want complete", detail.Task.Status, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasProviderRouteCapability(capabilities []types.Capability) bool {
	for _, capability := range capabilities {
		if capability == types.CapLLMProviderRoute {
			return true
		}
	}
	return false
}

type admissionRouteResolver struct{ endpoint *llm.ProviderEndpointBinding }

func (r admissionRouteResolver) SelectProviderRoute(_ context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	return llm.SelectedProviderRoute{
		Provider: "openai", Model: "route-model", KeyName: "admission route", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		Endpoint: r.endpoint, ExpiresAt: time.Now().Add(time.Minute),
	}, nil
}

func (r admissionRouteResolver) ResolveProviderRoute(_ context.Context, req llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	return llm.ResolvedProviderRoute{
		Provider: "openai", Model: "route-model", KeyName: "admission route", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
		Endpoint: r.endpoint, Credential: "provider-route-attempt-fixture-key", ExpiresAt: time.Now().Add(time.Minute),
	}, nil
}

func signWithAgentReach(t *testing.T, signer *testSigner, id identity.Identity, scopes, reach []string) string {
	t.Helper()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub":                id.UserID,
		"exp":                now.Add(time.Hour).Unix(),
		"nbf":                now.Add(-time.Minute).Unix(),
		"iat":                now.Unix(),
		"tenant":             id.TenantID,
		"user":               id.UserID,
		"session":            id.SessionID,
		"scopes":             scopes,
		auth.AgentReachClaim: reach,
	})
	token.Header["kid"] = signer.kid
	signed, err := token.SignedString(signer.priv)
	if err != nil {
		t.Fatalf("sign provider-route test token: %v", err)
	}
	return signed
}

func postProviderRouteAdmission(t *testing.T, handler http.Handler, token, path string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}
