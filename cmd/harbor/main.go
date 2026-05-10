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
	// Artifacts drivers — content-addressed blob store. Both V1
	// drivers self-register via init() so `artifacts.Open` can resolve
	// them. Phase 18 adds SQLite-blob + Postgres-blob; Phase 19 adds
	// S3-style.
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	// Audit driver — production redactor, registered via init().
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Events driver — production in-memory bus, registered via init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func main() {
	// Stub. Subcommand router + subsystem bootstrap lands in Phase 09+.
}
