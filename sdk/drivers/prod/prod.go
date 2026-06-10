// Package prod is the public SDK facade over Harbor's
// internal/drivers/prod package — the production driver aggregator
// (RFC §3.6; D-204/D-196). Importing this package for effect:
//
//	import _ "github.com/hurtener/Harbor/sdk/drivers/prod"
//
// seats, by init() transitivity, EVERY production driver + LLM-wrapper
// registration the `harbor` binary boots with: the §4.4
// self-registering factories (artifacts / audit / distributed /
// events / llm / memory / skills / state / tasks / telemetry /
// tools-OAuth / planner drivers), the LLM wrapper hooks (corrections /
// output-downgrade / retry / governance), and the notifications
// event-type registration. Parity with the internal aggregator is by
// construction: this package's ONLY content is the blank import below.
//
// Deliberately NOT included (same as the internal aggregator): the
// dev-only mock LLM driver (D-089) — it is never part of the
// production set.
//
// The package exports no identifiers.
package prod

import (
	// The single sanctioned home of Harbor's driver blank-import
	// block (Phase 110c, D-196). Importing it here makes the full
	// production registration set reachable from outside the module.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
)
