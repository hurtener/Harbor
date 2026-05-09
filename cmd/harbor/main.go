// Command harbor is the Harbor binary entry point. Phase 03 ships a
// minimal stub that exists only to host the audit-driver
// blank-import (so the patterns driver self-registers in any build
// of the binary). Phase 09+ replaces this with the real entry point
// (subcommands, dev server boot, config loader wiring).
package main

import (
	// Audit driver — production redactor, registered via init().
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Events driver — production in-memory bus, registered via init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func main() {
	// Stub. Subcommand router lands in Phase 09+.
}
