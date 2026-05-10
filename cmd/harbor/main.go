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
	// Events driver — production in-memory bus, registered via init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	// State driver — Postgres StateStore (Phase 16), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	// State driver — production SQLite StateStore (Phase 15), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	// Tasks driver — production in-process TaskRegistry (Phase 20), registered via init().
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

func main() {
	// Stub. Subcommand router + subsystem bootstrap lands in Phase 09+.
}
