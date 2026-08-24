// assemble_test.go — Phase 110d (D-197) coverage for the promoted
// assembly entry point: golden boot, the forced-failure table
// (partial-stack + close-what-opened + goroutine-baseline), and the
// Skip knobs.
package assemble_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// Production driver aggregator (Phase 110c, D-196) — the single
	// sanctioned blank-import home; the same import the recipe tells a
	// headless embedder to add.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089): deliberately OUTSIDE the aggregator;
	// tests opt in explicitly.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/grant"
	llmreceipts "github.com/hurtener/Harbor/internal/llm/receipts"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

type recordingGrantDelivery struct {
	ch chan llm.AttemptUsageReceipt
}

func (d *recordingGrantDelivery) Deliver(_ context.Context, receipt llm.AttemptUsageReceipt) error {
	select {
	case d.ch <- receipt:
	default:
	}
	return nil
}

var _ llmreceipts.Delivery = (*recordingGrantDelivery)(nil)

// minimalCfg is the examples-shaped minimal config the golden boot
// uses (mirrors harbortest/devstack's fixture; mock LLM driver).
func minimalCfg(t *testing.T) *config.Config {
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
			ServiceName: "harbor-assemble-test",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Driver:               "mock",
			Timeout:              5 * time.Second,
			ContextWindowReserve: 0.05,
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
		t.Fatalf("minimalCfg: cfg.Validate(): %v", err)
	}
	return cfg
}

// settle gives detached driver goroutines a bounded window to drain
// before the goroutine-baseline comparison (no sleep-as-sync — the
// predicate is polled).
func settleGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine baseline not restored: started %d, now %d", baseline, runtime.NumGoroutine())
}

// TestAssemble_GoldenBoot_BuildsEveryLayer — the full fan-out on an
// examples-shaped config: every load-bearing field non-nil, Close
// clean + idempotent, goroutine baseline restored.
func TestAssemble_GoldenBoot_BuildsEveryLayer(t *testing.T) {
	baseline := runtime.NumGoroutine()
	stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stack.Redactor == nil || stack.State == nil || stack.Bus == nil ||
		stack.Metrics == nil || stack.Artifacts == nil || stack.Tasks == nil ||
		stack.Sessions == nil || stack.Agents == nil {
		t.Errorf("load-bearing core has nil fields: %+v", stack)
	}
	// Phase 111f (D-203) always-constructed telemetry band — asserted
	// so the test's name stays true (Wave C checkpoint audit): a
	// dropped telemetry band fails at the cheapest level, not one repo
	// layer away in the integration suite.
	if stack.Telemetry == nil || stack.Tracer == nil || stack.RunErrorHandler == nil {
		t.Errorf("telemetry band has nil fields: Telemetry=%v Tracer=%v RunErrorHandler set=%v",
			stack.Telemetry, stack.Tracer, stack.RunErrorHandler != nil)
	}
	if stack.LLM == nil || stack.LLMSnapshot.Driver != "mock" {
		t.Errorf("LLM not opened from cfg: client=%v snapshot=%+v", stack.LLM, stack.LLMSnapshot)
	}
	if stack.Memory == nil {
		t.Errorf("memory.driver=inmem must open the memory store")
	}
	if stack.Catalog == nil || stack.Coordinator == nil || stack.Gates == nil ||
		stack.OAuthProviders == nil || stack.MCPRegistry == nil || stack.Executor == nil {
		t.Errorf("catalog band has nil fields")
	}
	if stack.Steering == nil || stack.Planner == nil || stack.RunLoop == nil {
		t.Errorf("steering band has nil fields (planner/runloop must build when an LLM is configured)")
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("second Close must be a no-op, got %v", err)
	}
	settleGoroutines(t, baseline)
}

func TestAssemble_ExternalGrantConfigWiresRealLLMReservationAndOutbox(t *testing.T) {
	cfg := minimalCfg(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := grant.NewSigner("key-1", "harbor-runtime", private, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"model-fast": {ContextWindowTokens: 4096, TokenEstimator: "chars_div_4"},
	}
	cfg.LLM.ExternalGrant = config.LLMExternalGrantConfig{
		Mode:                    "required",
		Audience:                "harbor-runtime",
		RuntimeID:               "runtime-1",
		AuthorizedOrganizations: []string{"org-a", "org-b"},
		PublicKeys: map[string]string{
			"key-1": base64.RawURLEncoding.EncodeToString(signer.PublicKey()),
		},
	}
	binding := grant.NewBindingStore()
	if err := binding.Put(grant.Binding{
		Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "mock",
		ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "not-used-by-mock",
	}); err != nil {
		t.Fatal(err)
	}
	if err := binding.Put(grant.Binding{
		Handle: "binding-b", OrganizationID: "org-b", RuntimeID: "runtime-1", Provider: "mock",
		ProviderConnectionID: "connection-b", ProviderConnectionGeneration: 1, Generation: 1, Secret: "not-used-by-mock-b",
	}); err != nil {
		t.Fatal(err)
	}
	delivery := &recordingGrantDelivery{ch: make(chan llm.AttemptUsageReceipt, 2)}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		ExternalGrant:         llm.ExternalGrantConfig{Credentials: binding},
		ExternalGrantDelivery: delivery,
	})
	if err != nil {
		t.Fatalf("Assemble external grant: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	signed, err := signer.Sign(llm.ExternalGrant{
		Version: 1, GrantID: "grant-a", OrganizationID: "org-a", RuntimeID: "runtime-1",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a",
		Provider: "mock", ProviderModelID: "model-fast", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1,
		RouteID: "route-a", CredentialBindingHandle: "binding-a", CredentialAssetGeneration: 1, PolicyGeneration: 1,
		MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 512,
		Lease:    llm.ComputeLease{LeaseID: "lease-a", TokenUnits: 512, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	grantB := signed
	grantB.GrantID = "grant-b"
	grantB.OrganizationID = "org-b"
	grantB.ProviderConnectionID = "connection-b"
	grantB.CredentialBindingHandle = "binding-b"
	grantB.Lease.LeaseID = "lease-b"
	grantB.LogicalCallID = ""
	grantB.AttemptNonce = ""
	signed, err = signer.Sign(grantB)
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err = identity.WithRun(ctxB, id, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	grantA := grantB
	grantA.GrantID = "grant-a"
	grantA.OrganizationID = "org-a"
	grantA.ProviderConnectionID = "connection-a"
	grantA.CredentialBindingHandle = "binding-a"
	grantA.Lease.LeaseID = "lease-a"
	grantA.LogicalCallID = ""
	grantA.AttemptNonce = ""
	grantA, err = signer.Sign(grantA)
	if err != nil {
		t.Fatal(err)
	}
	ctxA := ctx
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, call := range []struct {
		ctx   context.Context
		grant llm.ExternalGrant
	}{
		{ctx: ctxA, grant: grantA},
		{ctx: ctxB, grant: signed},
	} {
		wg.Add(1)
		go func(call struct {
			ctx   context.Context
			grant llm.ExternalGrant
		}) {
			defer wg.Done()
			_, completeErr := stack.LLM.Complete(call.ctx, llm.CompleteRequest{
				Model: "model-fast", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: stringPtr("hello")}}}, ExternalGrant: &call.grant,
			})
			errs <- completeErr
		}(call)
	}
	wg.Wait()
	close(errs)
	for completeErr := range errs {
		if completeErr != nil {
			t.Fatalf("real assembled multi-organization LLM call: %v", completeErr)
		}
	}
	seenOrganizations := map[string]bool{}
	for range 2 {
		select {
		case receipt := <-delivery.ch:
			seenOrganizations[receipt.OrganizationID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("assembled outbox did not deliver both content-free receipts")
		}
	}
	if len(seenOrganizations) != 2 || !seenOrganizations["org-a"] || !seenOrganizations["org-b"] {
		t.Fatalf("multi-organization receipts = %+v, want org-a and org-b", seenOrganizations)
	}
}

func TestAssemble_ExternalGrantRuntimeDefaultUsesNativeProviderAndReceipts(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.LLM.Provider = "mock"
	cfg.LLM.Model = "model-fast"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"model-fast": {ContextWindowTokens: 4096, TokenEstimator: "chars_div_4"},
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := grant.NewSigner("key-default", "harbor-runtime", private, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLM.ExternalGrant = config.LLMExternalGrantConfig{
		Mode: "required",
		// Leave route_mode unrestricted: an explicit runtime_default grant
		// must still boot and execute without a coordinator credential resolver.
		Audience:   "harbor-runtime",
		RuntimeID:  "runtime-default",
		PublicKeys: map[string]string{"key-default": base64.RawURLEncoding.EncodeToString(signer.PublicKey())},
	}
	delivery := &recordingGrantDelivery{ch: make(chan llm.AttemptUsageReceipt, 1)}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{ExternalGrantDelivery: delivery})
	if err != nil {
		t.Fatalf("Assemble runtime-default external grant: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	id := identity.Identity{TenantID: "tenant-default", UserID: "user-default", SessionID: "session-default"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	signed, err := signer.Sign(llm.ExternalGrant{
		Version: 1, GrantID: "grant-default", OrganizationID: "org-default", RuntimeID: "runtime-default",
		TenantID: "tenant-default", UserID: "user-default", SessionID: "session-default", LogicalRunID: "run-default",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, PolicyGeneration: 3,
		MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 128,
		Lease:    llm.ComputeLease{LeaseID: "lease-default", TokenUnits: 256, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.LLM.Complete(ctx, llm.CompleteRequest{
		Messages:      []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: stringPtr("hello")}}},
		ExternalGrant: &signed,
	}); err != nil {
		t.Fatalf("runtime-default completion: %v", err)
	}
	select {
	case receipt := <-delivery.ch:
		if receipt.RouteMode != llm.ExternalGrantRouteRuntimeDefault || receipt.Provider != "mock" || receipt.ProviderModelID != "model-fast" {
			t.Fatalf("runtime-default receipt = %+v", receipt)
		}
		if receipt.ProviderConnectionID != "" || receipt.RouteID != "" || receipt.CredentialAssetGeneration != 0 {
			t.Fatalf("runtime-default receipt leaked coordinator route claims: %+v", receipt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime-default receipt was not delivered")
	}
}

func TestAssemble_ExternalGrantRuntimeDefaultReachesBifrostCustomProvider(t *testing.T) {
	const (
		envName  = "HARBOR_TEST_ASSEMBLED_RUNTIME_DEFAULT_KEY"
		provider = "runtime-openai-compatible"
		model    = "runtime/model-fast"
	)
	t.Setenv(envName, "runtime-provider-secret")
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer runtime-provider-secret" {
			t.Errorf("provider authorization = %q, want runtime configured secret", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if request.Model != model {
			t.Errorf("provider model = %q, want %q", request.Model, model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"runtime-default","object":"chat.completion","created":1,"model":"runtime/model-fast","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	}))
	defer server.Close()

	cfg := minimalCfg(t)
	cfg.LLM.Driver = "bifrost"
	cfg.LLM.Provider = provider
	cfg.LLM.Model = model
	cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{{
		Name: provider, BaseURL: server.URL, APIKeyEnvVar: envName,
		Models: []string{model}, Timeout: 5 * time.Second,
	}}
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		model: {ContextWindowTokens: 4096, TokenEstimator: "chars_div_4"},
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := grant.NewSigner("key-bifrost", "harbor-runtime", private, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLM.ExternalGrant = config.LLMExternalGrantConfig{
		Mode: "required", RouteMode: "runtime_default", Audience: "harbor-runtime", RuntimeID: "runtime-bifrost",
		PublicKeys: map[string]string{"key-bifrost": base64.RawURLEncoding.EncodeToString(signer.PublicKey())},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("runtime-default bifrost config: %v", err)
	}
	delivery := &recordingGrantDelivery{ch: make(chan llm.AttemptUsageReceipt, 1)}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{ExternalGrantDelivery: delivery})
	if err != nil {
		t.Fatalf("Assemble runtime-default bifrost: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	id := identity.Identity{TenantID: "tenant-bifrost", UserID: "user-bifrost", SessionID: "session-bifrost"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-bifrost")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	signed, err := signer.Sign(llm.ExternalGrant{
		Version: 1, GrantID: "grant-bifrost", OrganizationID: "org-bifrost", RuntimeID: "runtime-bifrost",
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, LogicalRunID: "run-bifrost",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, PolicyGeneration: 4,
		MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 128,
		Lease:    llm.ComputeLease{LeaseID: "lease-bifrost", TokenUnits: 256, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "hello"
	if _, err := stack.LLM.Complete(ctx, llm.CompleteRequest{
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}, ExternalGrant: &signed,
	}); err != nil {
		t.Fatalf("runtime-default bifrost completion: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	select {
	case receipt := <-delivery.ch:
		if receipt.RouteMode != llm.ExternalGrantRouteRuntimeDefault || receipt.Provider != provider || receipt.ProviderModelID != model {
			t.Fatalf("runtime-default bifrost receipt = %+v", receipt)
		}
		if receipt.ProviderConnectionID != "" || receipt.RouteID != "" || receipt.CredentialAssetGeneration != 0 {
			t.Fatalf("runtime-default bifrost receipt leaked coordinator route claims: %+v", receipt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime-default bifrost receipt was not delivered")
	}
}

func stringPtr(value string) *string { return &value }

// TestAssemble_TokenBudget_BuildsCompressionRunner — Phase 111e
// (D-202): a non-zero `planner.token_budget` makes the assembly
// construct the trajectory-compression runner (TrajectorySummariser
// over the configured LLM); zero leaves it nil (compression off).
func TestAssemble_TokenBudget_BuildsCompressionRunner(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Planner.TokenBudget = 2048
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate(token_budget): %v", err)
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() {
		if cerr := stack.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if stack.Compression == nil {
		t.Error("Stack.Compression is nil with planner.token_budget=2048 — the runner was not built")
	}

	// Zero budget: compression stays off.
	cfg2 := minimalCfg(t)
	stack2, err := assemble.Assemble(context.Background(), cfg2, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble(zero budget): %v", err)
	}
	defer func() {
		if cerr := stack2.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if stack2.Compression != nil {
		t.Error("Stack.Compression non-nil with planner.token_budget=0 — compression must default off")
	}
}

// TestAssemble_BatchSpawnCap_ThreadedIntoExecutor — the assembled
// ToolExecutor honours `planner.max_batch_spawns` from config: a stack
// built with a cap of 2 rejects a Batch carrying 3 spawns with the
// breadth-cap error. This proves `dispatch.WithMaxBatchSpawns(
// cfg.Planner.BatchSpawnCap())` is threaded through the ONE production
// assembly. (The companion `steering.WithHardCancelHook` wiring is
// exercised end-to-end by the integration suite's cancel-hierarchy
// test, where the run-level hard cancel's cascade is observable.)
func TestAssemble_BatchSpawnCap_ThreadedIntoExecutor(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Planner.MaxBatchSpawns = 2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate(max_batch_spawns): %v", err)
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() {
		if cerr := stack.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if stack.Executor == nil {
		t.Fatal("stack.Executor is nil")
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithRun(context.Background(), id, "run-batch-cap")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: "run-batch-cap"}}
	d := planner.Batch{
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
			{Spec: planner.SpawnSpec{Query: "c"}, CallID: "s2"},
		},
	}
	_, _, execErr := stack.Executor.ExecuteDecision(ctx, rc, d)
	if execErr == nil {
		t.Fatal("Batch with 3 spawns under cap=2 dispatched without error; cap not threaded from config")
	}
	if !strings.Contains(execErr.Error(), "max_batch_spawns") {
		t.Errorf("err = %q, want it to name max_batch_spawns (the threaded cap)", execErr.Error())
	}
}

// TestAssemble_TokenBudget_WithoutLLM_FailsLoud — Phase 111e (D-202):
// a configured budget without an LLM is a misconfiguration surfaced
// loudly at assembly, never a silently-inert knob (CLAUDE.md §13).
func TestAssemble_TokenBudget_WithoutLLM_FailsLoud(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.LLM = config.LLMConfig{}
	cfg.Memory.Strategy = "none"
	cfg.Planner.TokenBudget = 2048
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }() // partial-stack drain on the failure path
	}
	if err == nil || !strings.Contains(err.Error(), "token_budget") {
		t.Fatalf("expected loud token_budget-requires-LLM error, got %v", err)
	}
}

// TestAssemble_HTTPManifest_RegistersBeforeEntriesApply — the
// HTTP-manifest boot loader's ordering guarantee: a manifest tool
// registers on the catalog (with the "manifest:<name>" provenance
// stamp) BEFORE the catalog Builder applies tools.entries[], so an
// entry naming that tool resolves cleanly instead of failing with
// ErrToolNotRegistered.
func TestAssemble_HTTPManifest_RegistersBeforeEntriesApply(t *testing.T) {
	manifestDir := t.TempDir()
	manifestPath := filepath.Join(manifestDir, "weather.yaml")
	manifestYAML := "tools:\n  - name: weather.lookup\n    method: GET\n" +
		"    url_template: https://example.test/weather?city={{ .Args.city | urlquery }}\n" +
		"    description: test tool\n"
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfg := minimalCfg(t)
	cfg.Tools.HTTPManifests = []string{manifestPath}
	cfg.Tools.Entries = []config.ToolEntryConfig{
		{Name: "weather.lookup", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	desc, ok := stack.Catalog.Resolve("weather.lookup")
	if !ok {
		t.Fatal("weather.lookup did not register from the manifest")
	}
	if desc.Tool.Source != "manifest:weather.lookup" {
		t.Errorf("Tool.Source = %q, want manifest:weather.lookup (provenance)", desc.Tool.Source)
	}
	if _, ok := stack.Gates["weather.lookup"]; !ok {
		t.Error("tools.entries[] naming the manifest tool did not wire an approval gate — " +
			"ordering bug: the manifest load must precede the catalog Builder's Apply")
	}
}

// TestAssemble_NilConfig_FailsLoud — no silent default-config fallback.
func TestAssemble_NilConfig_FailsLoud(t *testing.T) {
	stack, err := assemble.Assemble(context.Background(), nil, assemble.Options{})
	if err == nil || !strings.Contains(err.Error(), "cfg is required") {
		t.Fatalf("expected cfg-required error, got %v", err)
	}
	if stack != nil {
		t.Errorf("expected nil stack on nil cfg")
	}
}

// TestAssemble_RegisterCatalog_PrePolicy_ApprovalWrapFires proves the
// compiled-tool registrar rides the pre-policy catalog seam: a tool
// registered via Options.RegisterCatalog AND named in cfg.Tools.Entries
// behind an approval gate gets that gate wired — the empirical proof
// that registration landed BEFORE the catalog Builder's tools.entries
// wrapping (not merely that the tool is registered).
func TestAssemble_RegisterCatalog_PrePolicy_ApprovalWrapFires(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Tools.Entries = []config.ToolEntryConfig{
		{Name: "compiled.tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	registerCalled := false
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		RegisterCatalog: func(cat tools.ToolCatalog) error {
			registerCalled = true
			return inproc.RegisterFunc[compiledIn, compiledOut](
				cat, "compiled.tool",
				func(_ context.Context, in compiledIn) (compiledOut, error) {
					return compiledOut{Echo: in.Msg}, nil
				},
				tools.WithDescription("compiled test tool"),
			)
		},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	if !registerCalled {
		t.Fatal("RegisterCatalog callback was never invoked")
	}
	if _, ok := stack.Catalog.Resolve("compiled.tool"); !ok {
		t.Fatal("compiled.tool did not register via RegisterCatalog")
	}
	// The approval gate is the empirical wrap: it only wires if the tool
	// was on the catalog BEFORE the Builder applied tools.entries.
	if _, ok := stack.Gates["compiled.tool"]; !ok {
		t.Error("tools.entries[] naming the RegisterCatalog tool did NOT wire an approval gate — " +
			"the registrar did not ride the pre-policy seam (registration landed after the Builder)")
	}
}

// TestAssemble_RegisterCatalog_Error_FailsLoud — a registrar error is
// surfaced loud (never a silent skip), and the partial stack drains.
func TestAssemble_RegisterCatalog_Error_FailsLoud(t *testing.T) {
	cfg := minimalCfg(t)
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		RegisterCatalog: func(tools.ToolCatalog) error {
			return errSentinelRegistrar
		},
	})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "register-catalog") {
		t.Fatalf("expected loud register-catalog error, got %v", err)
	}
}

// TestAssemble_RegisterCatalog_DuplicateName_FailsLoud — a registrar
// that collides with an already-registered name (its own double
// registration, a builtin, or a pre-registered fixture) fails Assemble
// loud through the catalog's duplicate-name rejection, never a silent
// last-writer-wins.
func TestAssemble_RegisterCatalog_DuplicateName_FailsLoud(t *testing.T) {
	cfg := minimalCfg(t)
	register := func(cat tools.ToolCatalog) error {
		for range 2 { // second registration of the same name must collide
			if err := inproc.RegisterFunc[compiledIn, compiledOut](
				cat, "dup.tool",
				func(_ context.Context, in compiledIn) (compiledOut, error) {
					return compiledOut{Echo: in.Msg}, nil
				},
			); err != nil {
				return err
			}
		}
		return nil
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		RegisterCatalog: register,
	})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }()
	}
	if err == nil {
		t.Fatal("Assemble succeeded with a name-colliding registrar — want a loud duplicate-name error")
	}
	if !strings.Contains(err.Error(), "register-catalog") || !strings.Contains(err.Error(), "dup.tool") {
		t.Errorf("error should carry the register-catalog seam and the colliding name, got %v", err)
	}
}

// TestAssemble_PostAssemblyRegister_SkipsTheWrap is the documented trap
// (D-292): a tool registered on the catalog AFTER Assemble returns does
// NOT receive the tools.entries wrapping — its approval gate is absent.
// Pinned so a future refactor cannot silently move the registrar off
// the pre-policy seam and rationalise it as equivalent.
func TestAssemble_PostAssemblyRegister_SkipsTheWrap(t *testing.T) {
	cfg := minimalCfg(t)
	// Note: "late.tool" is deliberately NOT in cfg.Tools.Entries — an
	// entry naming an unregistered tool fails Assemble loud
	// (fail-closed). The trap is that even a would-be-wrapped tool
	// registered post-assembly reaches the catalog WITHOUT a gate.
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	if err := inproc.RegisterFunc[compiledIn, compiledOut](
		stack.Catalog, "late.tool",
		func(_ context.Context, in compiledIn) (compiledOut, error) {
			return compiledOut{Echo: in.Msg}, nil
		},
	); err != nil {
		t.Fatalf("post-assembly Catalog.Register: %v", err)
	}
	if _, ok := stack.Catalog.Resolve("late.tool"); !ok {
		t.Fatal("late.tool did not register post-assembly")
	}
	if _, ok := stack.Gates["late.tool"]; ok {
		t.Error("a post-assembly-registered tool got an approval gate — the wrapping band must run only at assembly time")
	}
}

// compiledIn / compiledOut are the typed I/O for the RegisterCatalog
// test tools.
type compiledIn struct {
	Msg string `json:"msg"`
}
type compiledOut struct {
	Echo string `json:"echo"`
}

// errSentinelRegistrar is the forced registrar failure.
var errSentinelRegistrar = errBoom("registrar boom")

type errBoom string

func (e errBoom) Error() string { return string(e) }

// TestAssemble_ForcedFailures_PartialStackClosesClean — the
// table-driven mid-assembly failure gate: each stage's error returns
// the PARTIAL stack, Close drains whatever opened, and the goroutine
// baseline is restored (acceptance: forced mid-assembly failure
// closes everything already opened).
func TestAssemble_ForcedFailures_PartialStackClosesClean(t *testing.T) {
	// Fixtures for the HTTP-manifest boot-loader failure cases below:
	// a manifest that never exists on disk, and one whose sole tool
	// collides with a built-in name (the loader runs AFTER built-ins,
	// so the collision surfaces here, not silently).
	manifestDir := t.TempDir()
	missingManifest := filepath.Join(manifestDir, "does-not-exist.yaml")
	collideManifestPath := filepath.Join(manifestDir, "collide.yaml")
	collideManifestYAML := "tools:\n  - name: clock.now\n    method: GET\n    url_template: https://example.test/clock\n"
	if err := os.WriteFile(collideManifestPath, []byte(collideManifestYAML), 0o600); err != nil {
		t.Fatalf("write collide manifest fixture: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(cfg *config.Config)
		wantErr string
	}{
		{
			name:    "state stage",
			mutate:  func(cfg *config.Config) { cfg.State.Driver = "no-such-state" },
			wantErr: "state:",
		},
		{
			name:    "events stage",
			mutate:  func(cfg *config.Config) { cfg.Events.Driver = "no-such-events" },
			wantErr: "events:",
		},
		{
			name:    "artifacts stage",
			mutate:  func(cfg *config.Config) { cfg.Artifacts.Driver = "no-such-artifacts" },
			wantErr: "artifacts:",
		},
		{
			name:    "llm stage",
			mutate:  func(cfg *config.Config) { cfg.LLM.Driver = "no-such-llm" },
			wantErr: "llm:",
		},
		{
			name:    "memory stage",
			mutate:  func(cfg *config.Config) { cfg.Memory.Driver = "no-such-memory" },
			wantErr: "memory:",
		},
		{
			name: "memory rolling_summary without llm",
			mutate: func(cfg *config.Config) {
				cfg.LLM = config.LLMConfig{}
				cfg.Memory.Strategy = "rolling_summary"
			},
			wantErr: "rolling_summary requires an LLM",
		},
		{
			name:    "skills stage",
			mutate:  func(cfg *config.Config) { cfg.Skills.Driver = "no-such-skills" },
			wantErr: "skills:",
		},
		{
			name:    "builtin stage",
			mutate:  func(cfg *config.Config) { cfg.Tools.BuiltIn = []string{"no_such_builtin"} },
			wantErr: "tools/builtin:",
		},
		{
			name: "http manifest missing file stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.HTTPManifests = []string{missingManifest}
			},
			wantErr: "tools.http_manifests[0]",
		},
		{
			name: "http manifest duplicate tool name vs builtin stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.BuiltIn = []string{"clock.now"}
				cfg.Tools.HTTPManifests = []string{collideManifestPath}
			},
			wantErr: "tools.http_manifests[0]",
		},
		{
			name: "catalog builder stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.Entries = []config.ToolEntryConfig{
					{Name: "no_such_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
				}
			},
			wantErr: "tools/catalog:",
		},
		{
			name: "oauth providers stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.OAuthTokenKEKEnv = "HARBOR_ASSEMBLE_TEST_KEK_UNSET"
				cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
					Name: "gh", Driver: "oauth2",
					ClientIDEnv: "HARBOR_ASSEMBLE_TEST_ID", ClientSecretEnv: "HARBOR_ASSEMBLE_TEST_SECRET",
				}}
			},
			wantErr: "tools/catalog:",
		},
		{
			name: "mcp attach stage",
			mutate: func(cfg *config.Config) {
				cfg.Tools.MCPServers = []config.MCPServerConfig{{
					Name:          "bad",
					TransportMode: "stdio",
					Command:       []string{"/nonexistent/harbor-110d-no-such-binary"},
				}}
			},
			wantErr: "mcp[bad]:",
		},
		{
			name:    "planner stage",
			mutate:  func(cfg *config.Config) { cfg.Planner.Driver = "no-such-planner" },
			wantErr: "planner:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			cfg := minimalCfg(t)
			tc.mutate(cfg)
			stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
			if err == nil {
				if stack != nil {
					_ = stack.Close(context.Background())
				}
				t.Fatalf("expected forced failure at %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v does not carry stage prefix %q", err, tc.wantErr)
			}
			if stack == nil {
				t.Fatalf("expected PARTIAL stack on error so the caller can drain")
			}
			if cErr := stack.Close(context.Background()); cErr != nil {
				t.Errorf("partial-stack Close: %v", cErr)
			}
			settleGoroutines(t, baseline)
		})
	}
}

// TestAssemble_SkipKnobs — SkipCatalog / SkipSteering / SkipRunLoop
// leave exactly their bands nil (the devstack test conveniences,
// expressed as options instead of forks).
func TestAssemble_SkipKnobs(t *testing.T) {
	t.Run("SkipCatalog", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipCatalog: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.Catalog != nil || stack.Coordinator != nil || stack.MCPRegistry != nil || stack.Executor != nil {
			t.Errorf("catalog band must be nil under SkipCatalog")
		}
		if stack.RunLoop != nil {
			t.Errorf("RunLoop requires the catalog band's Coordinator; must be nil under SkipCatalog")
		}
		if stack.Steering == nil || stack.Planner == nil {
			t.Errorf("steering registry + planner still build under SkipCatalog")
		}
	})
	t.Run("SkipSteering", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipSteering: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.Steering != nil || stack.Planner != nil || stack.RunLoop != nil {
			t.Errorf("steering band must be nil under SkipSteering")
		}
		if stack.Catalog == nil {
			t.Errorf("catalog band still builds under SkipSteering")
		}
	})
	t.Run("SkipRunLoop", func(t *testing.T) {
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{SkipRunLoop: true})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.RunLoop != nil {
			t.Errorf("RunLoop must be nil under SkipRunLoop")
		}
		if stack.Steering == nil || stack.Planner == nil {
			t.Errorf("steering registry + planner still build under SkipRunLoop")
		}
	})
	t.Run("no LLM means no planner and no runloop", func(t *testing.T) {
		cfg := minimalCfg(t)
		cfg.LLM = config.LLMConfig{}
		stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer func() { _ = stack.Close(context.Background()) }()
		if stack.LLM != nil || stack.Planner != nil || stack.RunLoop != nil {
			t.Errorf("LLM-less config must leave LLM/planner/RunLoop nil")
		}
		if stack.Steering == nil {
			t.Errorf("steering registry still builds without an LLM")
		}
	})
}

// stub110dProvider is a minimal toolauth.OAuthProvider for the
// injection-override test below (unit-test stub; the integration
// suite drives real providers).
type stub110dProvider struct{}

func (stub110dProvider) Token(context.Context, tools.ToolSourceID) (toolauth.Token, error) {
	return toolauth.Token{}, nil
}
func (stub110dProvider) InitiateFlow(context.Context, tools.ToolSourceID) (toolauth.FlowInitiation, error) {
	return toolauth.FlowInitiation{}, nil
}
func (stub110dProvider) CompleteFlow(context.Context, string, string) (toolauth.Token, error) {
	return toolauth.Token{}, nil
}
func (stub110dProvider) PendingFlow(context.Context, string) (toolauth.PendingFlowInfo, bool, error) {
	return toolauth.PendingFlowInfo{}, false, nil
}
func (stub110dProvider) DenyFlow(context.Context, string, string) error   { return nil }
func (stub110dProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (stub110dProvider) Close(context.Context) error                      { return nil }
func (stub110dProvider) AllowedDownstreamHosts() []string                 { return nil }

// TestAssemble_InjectedOAuthProvider_OverridesCfgDeclaration — an
// Options.OAuthProviders entry overrides the same-named cfg
// declaration: the overridden entry is NOT constructed (the KEK env —
// deliberately unset here — is never read) and the injected instance
// lands in Stack.OAuthProviders. This is the devstack injection seam
// the pre-110d kit relied on, now with explicit per-name semantics.
func TestAssemble_InjectedOAuthProvider_OverridesCfgDeclaration(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Tools.OAuthTokenKEKEnv = "HARBOR_ASSEMBLE_KEK_NEVER_SET"
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{
		Name:            "stubbed",
		Driver:          "oauth2",
		ClientIDEnv:     "HARBOR_ASSEMBLE_ID_NEVER_SET",
		ClientSecretEnv: "HARBOR_ASSEMBLE_SECRET_NEVER_SET",
		AuthURL:         "https://example.com/authorize",
		TokenURL:        "https://example.com/token",
		RedirectURL:     "https://example.com/callback",
	}}
	injected := stub110dProvider{}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		OAuthProviders: map[string]toolauth.OAuthProvider{"stubbed": injected},
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("Assemble must not construct (or demand the KEK for) an injection-overridden provider: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	if got := stack.OAuthProviders["stubbed"]; got != toolauth.OAuthProvider(injected) {
		t.Errorf("injected provider must win: got %T", got)
	}
}
