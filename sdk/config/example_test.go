// example_test.go — runnable godoc example for the sdk/config facade:
// the canonical baseline (Defaults) plus the headless validation
// profile (ValidateCore) a Go embedder uses when it never serves the
// Protocol edge. The example body imports only the public sdk/config
// facade — the copyable code never reaches into internal/.
package config_test

import (
	"fmt"
	"log"

	"github.com/hurtener/Harbor/sdk/config"
)

// Example shows the hand-built configuration path: Defaults returns the
// canonical baseline (the same one Load applies to a harbor.yaml), the
// embedder points the LLM seam at a concrete driver, and ValidateCore
// runs the headless validation profile — every section EXCEPT the
// Protocol-server JWT ceremony a headless library never serves. A
// config that passes ValidateCore is ready to hand to assemble.Assemble.
func Example() {
	cfg := config.Defaults()

	// Point the LLM seam at a driver and declare its context window.
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}

	if err := cfg.ValidateCore(); err != nil {
		log.Fatalf("validate: %v", err)
	}

	// The baseline ships in-memory drivers across the persistence triad,
	// so a headless embedder boots with zero external dependencies.
	fmt.Println(cfg.State.Driver)
	// Output: inmem
}
