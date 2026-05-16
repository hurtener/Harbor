// Helpers for the Phase 64 integration test. Per D-094, the per-test
// dev-stack assembly is centralised in `harbortest/devstack`. The
// production source of truth remains `cmd/harbor/cmd_dev.go::bootDevStack`;
// the helper here is a thin wrapper that picks the right Skip flags
// and (because the test fixture's config names a bifrost driver
// without a real key) injects an explicit LLM ConfigSnapshot overriding
// the driver to "mock" — the same shape the production dev cmd
// follows when `HARBOR_DEV_ALLOW_MOCK=1` fires.

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

// phase64TestStack is the test's slim handle on the assembled stack.
// Kept as a separate type so the existing test bodies keep their
// `stack.handler` / `stack.close()` shape unchanged.
type phase64TestStack struct {
	handler http.Handler
	close   func()
}

// buildPhase64TestStack assembles a Phase-64-shaped dev stack against
// the test's dev config via `devstack.Assemble`. The LLM driver is
// overridden to "mock" via an explicit `LLMConfigSnapshot` — the
// integration test thus exercises the SAME wiring path the dev cmd
// follows when `HARBOR_DEV_ALLOW_MOCK=1` is set. Returns the stack
// plus a Bearer token signed by the in-test ES256 key.
func buildPhase64TestStack(t *testing.T) (*phase64TestStack, string) {
	t.Helper()
	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Override the LLM driver to "mock" so the test stays hermetic.
	// The model profile is preserved from the cfg so the safety +
	// corrections + governance chain composes against the same
	// context-window knobs production would see.
	llmSnap := llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &llmSnap,
	})
	// Sanity assertions on layers the test relies on.
	if stack.Handler == nil {
		stack.Close()
		t.Fatal("phase64: expected stack.Handler to be non-nil")
	}
	if stack.Token == "" {
		stack.Close()
		t.Fatal("phase64: expected stack.Token to be non-empty")
	}
	return &phase64TestStack{
		handler: stack.Handler,
		close:   stack.Close,
	}, stack.Token
}
