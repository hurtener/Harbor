// Command harbor is the Harbor binary entry point.
//
// Wave 2 ships only the driver-registration blank-imports: every
// production driver self-registers in its own init() so the
// audit / events / state factories resolve when something Opens
// them. The actual subcommand router (`harbor dev`, `harbor
// scaffold`, …) plus full subsystem bootstrap lands in Phase 09+
// per the master plan.
//
// Until that lands, `./bin/harbor` builds, runs, and exits cleanly
// — the preflight gate detects the clean exit and skips the boot
// step (see scripts/preflight.sh).
package main

import (
	// Artifacts drivers — content-addressed blob store. Each V1
	// driver self-registers via init() so `artifacts.Open` can resolve
	// them. Phase 17 ships fs + inmem; Phase 18 adds sqlite +
	// postgres; Phase 19 adds the S3-style driver.
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	// Audit driver — production redactor, registered via init().
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Distributed drivers — Phase 22 loopback MessageBus + RemoteTransport.
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	// Distributed driver — Phase 29 A2A wire RemoteTransport (southbound).
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
	// Events driver — production in-memory bus, registered via init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// LLM corrections — Phase 34 per-provider correction layer (RFC §6.5,
	// brief 03 §4 + brief 08). Self-registers a wrapper hook in
	// internal/llm via init() so `llm.Open()` composes
	// `corrections(safetyClient(driver))` by default. Blank-imported
	// so the registration fires at process boot.
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	// LLM driver — Phase 33 bifrost-backed LLMClient, registered via init().
	_ "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	// LLM output — Phase 35 structured-output downgrade chain (RFC §6.5).
	// Self-registers a wrapper hook so `llm.Open()` composes
	// `downgrade(corrections(safetyClient(driver)))`.
	_ "github.com/hurtener/Harbor/internal/llm/output"
	// LLM retry — Phase 36 retry-with-feedback (RFC §6.5). Self-
	// registers a wrapper hook so `llm.Open()` composes
	// `retry(downgrade(corrections(safetyClient(driver))))`.
	_ "github.com/hurtener/Harbor/internal/llm/retry"
	// Governance — Phase 36a + 36b cost accumulator + rate limiter +
	// MaxTokens enforcer (RFC §6.15). Self-registers a wrapper hook
	// so `llm.Open()` composes `governance(retry(...))` outermost.
	// **LATENT default:** with no factory registered via
	// `governance.SetFactory`, the wrapper is a pass-through — the
	// blank-import only seats the hook (D-044).
	_ "github.com/hurtener/Harbor/internal/governance"
	// Memory driver — Phase 23 in-memory MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	// Memory driver — Phase 25 Postgres MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	// Memory driver — Phase 25 SQLite MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	// Skills driver — Phase 37 LocalDB SkillStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	// Skills planner tools — Phase 38 (`skill_search` / `skill_get` /
	// `skill_list`). The package has no init-time registration
	// (catalogs are constructed at boot, not from a factory registry);
	// the blank import documents the package's presence in the binary
	// so deployment-time reviewers can confirm it's wired. The Phase 60+
	// bootstrap will call `skills/tools.Register(catalog, store, deps)`.
	_ "github.com/hurtener/Harbor/internal/skills/tools"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	// State driver — Postgres StateStore (Phase 16), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	// State driver — production SQLite StateStore (Phase 15), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	// Tasks driver — production in-process TaskRegistry (Phase 20), registered via init().
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	// Tools driver — Phase 29 A2A southbound ToolProvider. The package
	// has no init-time registration (catalogs are constructed in code,
	// not from a factory registry); the blank import documents the
	// driver's presence in the binary so deployment-time reviewers can
	// confirm it's wired.
	_ "github.com/hurtener/Harbor/internal/tools/drivers/a2a"
)

func main() {
	// Stub. Subcommand router + subsystem bootstrap lands in Phase 09+.
}
