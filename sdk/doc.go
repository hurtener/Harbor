// Package sdk is the root of Harbor's public SDK facade (RFC §3.6) —
// the curated, alias-based re-export tree that makes the runtime
// importable by external Go modules.
//
// Go's internal/ visibility rule makes every runtime package
// import-forbidden outside this module. Each sdk/<area> package
// re-exports its internal/<area> counterpart's public surface via
// type aliases (`type Identity = identity.Identity`), re-exported
// constants/sentinels, and thin function/variable forwards. An alias
// IS the internal type: values flow freely across the boundary,
// interface satisfiability is preserved, and the facade adds no
// mechanism of any kind.
//
// The curation contract: what sdk/ re-exports is the supported
// external API surface; what it omits is deliberately private.
// Additions are cheap and additive; removals follow the
// Protocol-style deprecation posture (RFC §5.3).
//
// The inventory (RFC §3.6 items 3 + 6): sdk/identity, sdk/events,
// sdk/config, sdk/tools (+ tools/inproc, tools/builtin), sdk/llm,
// sdk/memory, sdk/state, sdk/artifacts, sdk/skills, sdk/planner
// (+ planner/react, planner/deterministic), sdk/tasks, sdk/steering,
// sdk/dispatch, sdk/runctx, sdk/assemble, sdk/server (serve the
// Protocol from your own binary), and sdk/drivers/prod (the public
// blank-import driver aggregator).
//
// A minimal headless embedding (docs/recipes/embed-harbor-headless.md
// expressed through the facade):
//
//	import (
//	    _ "github.com/hurtener/Harbor/sdk/drivers/prod"
//
//	    "github.com/hurtener/Harbor/sdk/assemble"
//	    "github.com/hurtener/Harbor/sdk/config"
//	)
//
//	cfg := config.Defaults()
//	// ... populate cfg.LLM, validate with cfg.ValidateCore() ...
//	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
//
// This package itself exports nothing; the surface lives in the
// per-area subpackages.
package sdk
