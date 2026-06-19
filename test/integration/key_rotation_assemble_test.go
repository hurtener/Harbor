package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

// rotateCannedResponse is a minimal OpenAI-compatible chat completion the
// stub provider returns so a real bifrost Complete round-trips.
const rotateCannedResponse = `{"id":"x","object":"chat.completion","created":1700000000,` +
	`"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// TestE2E_KeyRotation_AssembleWiring is the load-bearing wiring gate: it
// proves that `assemble.Assemble` injects the SAME `llm.LiveKey` holder
// into the opened bifrost driver (the per-call read path) AND exposes it
// as `Stack.KeyRotator` (the admin write path) — so a rotate through the
// rotate service changes the `Authorization` header a REAL `Complete`
// sends. A fresh-holder regression in assemble.go (holder not shared)
// would make this test fail (the header would stay the boot key).
func TestE2E_KeyRotation_AssembleWiring(t *testing.T) {
	const envName = "HARBOR_TEST_ROTATE_KEY_ENV"
	t.Setenv(envName, "sk-boot-0001")

	var mu sync.Mutex
	var lastAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(rotateCannedResponse))
	}))
	defer server.Close()
	authSeen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastAuth
	}

	cfg := config.Defaults()
	cfg.LLM.Driver = "bifrost"
	cfg.LLM.Provider = "stub-openai"
	cfg.LLM.Model = "m"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"m": {ContextWindowTokens: 8192, TokenEstimator: "chars_div_4"},
	}
	cfg.LLM.CustomProviders = []config.LLMCustomProviderConfig{{
		Name:         "stub-openai",
		BaseURL:      server.URL,
		APIKeyEnvVar: envName,
		Models:       []string{"m"},
		Timeout:      5 * time.Second,
	}}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}

	ctx := context.Background()
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("Assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	if stack.LLM == nil {
		t.Fatal("stack.LLM is nil — no driver opened")
	}
	if stack.KeyRotator == nil {
		t.Fatal("stack.KeyRotator is nil — assemble did not expose the rotation seam")
	}

	idCtx, err := identity.With(ctx, identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	text := "hi"
	complete := func() {
		t.Helper()
		cctx, cancel := context.WithTimeout(idCtx, 5*time.Second)
		defer cancel()
		if _, err := stack.LLM.Complete(cctx, llm.CompleteRequest{
			Model:    "m",
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}

	// Boot key flows through the real Complete.
	complete()
	if got := authSeen(); !strings.Contains(got, "sk-boot-0001") {
		t.Fatalf("boot Authorization = %q, want it to carry sk-boot-0001", got)
	}

	// Rotate through the SERVICE over the exposed Stack.KeyRotator — the
	// exact production write path.
	svc, err := governanceprotocol.NewKeyRotateService(stack.KeyRotator)
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}
	if _, err := svc.RotateKey(ctx, prototypes.GovernanceRotateKeyRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "admin", Session: "s"},
		Provider: "stub-openai",
		Key:      "sk-rotated-9999",
	}); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// The NEXT real Complete must carry the rotated key — proving the
	// rotate service swapped the SAME holder the driver reads.
	complete()
	got := authSeen()
	if !strings.Contains(got, "sk-rotated-9999") {
		t.Fatalf("after rotate, Authorization = %q, want it to carry sk-rotated-9999 (holder not shared across assemble seam)", got)
	}
	if strings.Contains(got, "sk-boot-0001") {
		t.Fatalf("after rotate, the OLD key is still in use: %q", got)
	}
}
