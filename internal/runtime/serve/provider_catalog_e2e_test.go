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

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestBoot_RuntimeOriginProviderCatalog_UsesBrokerCredentialAndProtocol proves
// the complete HA-71 path at the serving boundary: Boot opens the real Bifrost
// driver, the configured inference broker seeds its shared LiveKey, and an
// authenticated request over the bound Protocol listener reaches the same
// credential-backed provider catalog. The response is the bounded sanitized
// wire projection; neither the broker key nor the upstream response body can
// cross that boundary.
func TestBoot_RuntimeOriginProviderCatalog_UsesBrokerCredentialAndProtocol(t *testing.T) {
	const (
		brokerToken = "harbor-e2e-broker-token-not-secret"
		providerKey = "harbor-e2e-provider-key-not-secret"
		brokerEnv   = "HARBOR_SERVE_PROVIDER_CATALOG_BROKER_TOKEN"
	)
	t.Setenv(brokerEnv, brokerToken)

	var providerHits atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("provider path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+providerKey {
			t.Errorf("provider authorization = %q, want broker-pulled key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"runtime-model","object":"model","owned_by":"fixture","context_length":8192}]}`)
	}))
	t.Cleanup(provider.Close)
	broker := inferencebrokertest.New(t, brokerToken, providerKey)

	// Keep the complete normal serve fixture and replace only the LLM block.
	// The primary is brokered, so no provider key is placed in configuration or
	// the test process's LLM config; BootConnectPrimary must fetch it.
	llmBlock := fmt.Sprintf(`llm:
  driver: bifrost
  provider: openai
  model: runtime-model
  base_url: %s
  timeout: 2s
  credential_source: remote
  inference_broker: runtime-provider
  inference_brokers:
    - name: runtime-provider
      credential_url: %s
      auth_token_env: %s
      timeout: 2s
`, provider.URL, broker.URL(), brokerEnv)
	yaml := strings.Replace(serveTestYAML, "llm:\n  driver: mock\n  timeout: 30s\n  context_window_reserve: 0.05\n", llmBlock, 1)
	if yaml == serveTestYAML {
		t.Fatal("test fixture did not replace the mock LLM block")
	}

	signer := newTestSigner(t)
	opts := baseOptions(t)
	// baseOptions writes the default fixture, so replace its file with the
	// brokered Bifrost configuration before Boot loads it.
	if err := os.WriteFile(opts.ConfigPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write brokered config: %v", err)
	}
	opts.AuthValidatorFactory = signer.factory()

	bootCtx, cancelBoot := context.WithCancel(context.Background())
	h, err := Boot(bootCtx, opts)
	if err != nil {
		cancelBoot()
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		cancelBoot()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Close(closeCtx)
	}()

	serveDone := make(chan error, 1)
	go func() { serveDone <- h.Serve(bootCtx) }()
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	addr, err := h.WaitReady(readyCtx)
	cancelReady()
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	id := identity.Identity{TenantID: "tenant-e2e", UserID: "admin-e2e", SessionID: "session-e2e"}
	body, err := json.Marshal(types.RuntimeInfoRequest{
		Identity:          types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		ProviderOperation: "discover",
		ProviderID:        "openai",
		ProviderPageSize:  10,
		ProviderMaxPages:  1,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/control/llm.posture", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signer.sign(t, id, []string{"admin"}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Protocol request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("Protocol status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read Protocol response: %v", err)
	}
	if strings.Contains(string(raw), providerKey) {
		t.Fatal("Protocol response leaked the broker-pulled provider credential")
	}
	var got types.LLMProviderOperationResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode Protocol response: %v", err)
	}
	if !got.RuntimeOrigin || got.Operation != "discover" || got.ProviderID != "openai" {
		t.Fatalf("unexpected runtime-origin response: %+v", got)
	}
	if got.Discovery == nil || got.Discovery.Outcome.State != "supported" || !got.Discovery.Outcome.RuntimeOrigin {
		t.Fatalf("unexpected discovery outcome: %+v", got.Discovery)
	}
	if len(got.Discovery.Models) != 1 || got.Discovery.Models[0].ID != "openai/runtime-model" {
		t.Fatalf("unexpected discovered models: %+v", got.Discovery.Models)
	}
	if providerHits.Load() != 1 {
		t.Fatalf("provider model request count after discovery = %d, want exactly one bounded probe", providerHits.Load())
	}

	// The same booted runtime also exposes the bounded validation operation;
	// it must use the identical broker-backed account and sanitized outcome.
	validationBody, err := json.Marshal(types.RuntimeInfoRequest{
		Identity:          types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		ProviderOperation: "validate",
		ProviderID:        "openai",
	})
	if err != nil {
		t.Fatalf("marshal validation request: %v", err)
	}
	validationReq, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/control/llm.posture", strings.NewReader(string(validationBody)))
	if err != nil {
		t.Fatalf("validation request: %v", err)
	}
	validationReq.Header.Set("Content-Type", "application/json")
	validationReq.Header.Set("Authorization", "Bearer "+signer.sign(t, id, []string{"admin"}))
	validationResp, err := http.DefaultClient.Do(validationReq)
	if err != nil {
		t.Fatalf("validation Protocol request: %v", err)
	}
	defer validationResp.Body.Close()
	validationRaw, err := io.ReadAll(validationResp.Body)
	if err != nil {
		t.Fatalf("read validation response: %v", err)
	}
	if validationResp.StatusCode != http.StatusOK {
		t.Fatalf("validation Protocol status = %d, want 200; body=%s", validationResp.StatusCode, validationRaw)
	}
	if strings.Contains(string(validationRaw), providerKey) {
		t.Fatal("validation response leaked the broker-pulled provider credential")
	}
	var validation types.LLMProviderOperationResponse
	if err := json.Unmarshal(validationRaw, &validation); err != nil {
		t.Fatalf("decode validation response: %v", err)
	}
	if validation.Operation != "validate" || validation.Validation == nil ||
		validation.Validation.Outcome.State != "supported" || !validation.Validation.Outcome.RuntimeOrigin {
		t.Fatalf("unexpected validation response: %+v", validation)
	}
	if providerHits.Load() != 2 {
		t.Fatalf("provider model request count after validation = %d, want exactly two bounded probes", providerHits.Load())
	}

	// Ensure the Serve goroutine is not left behind by the test's cleanup path.
	cancelBoot()
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.Close(closeCtx)
	cancel()
	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			t.Fatalf("Serve: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after Close")
	}
}
