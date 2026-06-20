package devstack

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// devstackSessionCfg is a minimal config that builds the run loop (mock
// LLM + a model profile) so RunLoopDriver + RunsOverrideStore are present.
func devstackSessionCfg() *config.Config {
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	return cfg
}

// TestDevStack_SessionOverrideStore_SharedWithDriver is the Phase 92b
// D-094 / §17.6 guard: the devstack's mounted runs.set_overrides Store and
// the run-loop driver's run-start Consume MUST be the SAME instance — else
// a recorded session override is inert (the bug the adversarial pass
// found). This internal test asserts pointer identity directly + a
// Set→Consume round-trip through the driver's store.
func TestDevStack_SessionOverrideStore_SharedWithDriver(t *testing.T) {
	stack := Assemble(t, devstackSessionCfg(), AssembleOpts{})

	if stack.RunLoopDriver == nil {
		t.Fatal("RunLoopDriver nil — run loop did not build")
	}
	if stack.RunsOverrideStore == nil {
		t.Fatal("RunsOverrideStore nil — shared session-override Store not exposed")
	}
	// The driver Consumes from the SAME Store the runs Service writes into.
	if stack.RunLoopDriver.sessionOverrides != stack.RunsOverrideStore {
		t.Fatal("driver.sessionOverrides is NOT the exposed RunsOverrideStore — the SET↔CONSUME seam is split (runs.set_overrides would be inert)")
	}
	// A set into the shared store is Consumable through the driver's handle.
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	model := "mock/echo"
	stack.RunsOverrideStore.Set(id, runsprotocol.PendingOverride{Model: &model})
	po, found := stack.RunLoopDriver.sessionOverrides.Consume(id)
	if !found || po.Model == nil || *po.Model != "mock/echo" {
		t.Fatalf("driver did not Consume the set override: found=%v model=%v", found, po.Model)
	}
}
