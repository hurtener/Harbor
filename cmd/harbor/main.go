// Command harbor is the Harbor binary entry point.
//
// Phase 63 (RFC §8, D-084) turns this from a driver-registration stub
// into a cobra-rooted CLI. The blank-import block below keeps every
// production driver self-registering via its init() (so the audit /
// events / state / artifacts / memory / skills / tools / telemetry /
// llm / governance / distributed factories resolve when something
// Opens them — the §4.4 seam pattern). The cobra command tree lives in
// root.go (NewRootCmd) and is executed here.
//
// Only `harbor version` is fully implemented at this phase. The other
// six subcommands (`dev`, `scaffold`, `validate`, `inspect-events`,
// `inspect-runs`, `inspect-topology`) are stubs that exit non-zero
// with a structured CLIError pointing to their implementing phase —
// the §13 "test stubs as production defaults" amendment is satisfied
// by the structured error + non-zero exit.
package main

import (
	"fmt"
	"os"

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
	// Events driver — Phase 57 StateStore-backed durable event log,
	// registered via init(). Opens its StateStore from
	// EventsConfig.StateDriver / StateDSN; an empty StateDriver
	// auto-degrades to a best-effort ring buffer with a loud warning.
	_ "github.com/hurtener/Harbor/internal/events/drivers/durable"
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
	// Skills generator — Phase 41 (`skill_propose(persist=true)`). The
	// package has no init-time registration (the catalog is built at
	// boot); the blank import documents the package's presence in the
	// binary. The Phase 60+ bootstrap will call
	// `skills/generator.Register(catalog, store, deps)`.
	_ "github.com/hurtener/Harbor/internal/skills/generator"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	// State driver — Postgres StateStore (Phase 16), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	// State driver — production SQLite StateStore (Phase 15), registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	// Tasks driver — production in-process TaskRegistry (Phase 20), registered via init().
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	// Telemetry span exporters — Phase 55 OTel traces. The noop driver
	// is the default (no collector); the otlp driver ships spans to an
	// OTLP/gRPC collector when telemetry.otel_endpoint is configured.
	// Both self-register via init() so `telemetry.NewTracer` resolves
	// them.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"

	// Telemetry metric exporters — Phase 56 OTel metrics. The
	// prometheus driver is the default (built-in /metrics pull
	// endpoint, no collector); the otlpmetric driver pushes metrics to
	// an OTLP/gRPC collector when telemetry.otel_endpoint is
	// configured. Both self-register via init() so
	// `telemetry.NewMetricsRegistry` resolves them.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"

	// Tools driver — Phase 29 A2A southbound ToolProvider. The package
	// has no init-time registration (catalogs are constructed in code,
	// not from a factory registry); the blank import documents the
	// driver's presence in the binary so deployment-time reviewers can
	// confirm it's wired.
	_ "github.com/hurtener/Harbor/internal/tools/drivers/a2a"
	// Tools OAuth driver — D-095 (closes issue #116). The `oauth2`
	// driver self-registers under that name via init() so
	// `tools.oauth_providers[].driver: oauth2` resolves at boot. New
	// OAuth flow strategies (device-code, vendor-specific) add a new
	// driver under `internal/tools/auth/drivers/<name>/` + a blank
	// import here, per the §4.4 seam pattern.
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/oauth2"
	// Planner driver — D-103 (closes issue #126). The `react` driver
	// self-registers under that name via init() so
	// `planner.driver: react` resolves at boot. New planner concretes
	// (Plan-Execute, Workflow, Graph, Deterministic, Supervisor,
	// MultiAgent, HumanApproval per RFC §6.2) add a new driver under
	// `internal/planner/<name>/` + a blank import here, per the §4.4
	// seam pattern that D-095 uses for OAuth providers. The V1
	// reference planner remains the no-config-needed default.
	_ "github.com/hurtener/Harbor/internal/planner/react"
	// Notifications event topic — Phase 72d (D-109). The package's init()
	// registers the five V1 notification.* event-type constants
	// (notification.task_failed / tool_approval_requested /
	// governance_budget_exceeded / auth_required / pause_requested) plus
	// notification.identity_rejected onto the canonical events registry
	// so any future Publish from a constructed Subscriber (or a Console-
	// side Protocol consumer subscribing to the topic) doesn't trip
	// events.ErrUnknownEventType. Phase 72d ships zero new HTTP routes
	// and no boot-side Subscriber construction; the long-lived
	// Subscriber wires up in a later phase when its UI consumer ships.
	// Blank-importing here keeps the event-type registry consistent
	// across every binary boot per the §4.4 seam pattern.
	_ "github.com/hurtener/Harbor/internal/runtime/notifications"
)

func main() {
	root := NewRootCmd()
	err := root.Execute()
	if err == nil {
		return
	}
	// Cobra has already routed a CLIError through the subcommand's
	// RunE → emitCLIError → PrintCLIError chain (SilenceErrors on the
	// root suppresses cobra's own printing). Defensive fallback: if
	// the error is NOT a CLIError (e.g. flag-parse failure before any
	// RunE ran), surface a structured error here so the operator
	// never sees a bare cobra trace. Fail loudly per CLAUDE.md §5.
	cli, ok := asCLIError(err)
	if !ok {
		fallback := CLIError{
			Message: fmt.Sprintf("invocation error: %s", err.Error()),
			Code:    "invocation_error",
		}
		//nolint:errcheck // last-ditch error print before exit; if stderr is unwritable there is nothing left to do
		_ = PrintCLIError(os.Stderr, false, fallback)
		os.Exit(1)
	}
	os.Exit(exitCodeFor(cli))
}

// exitCodeFor maps a CLIError.Code to the binary's exit code. Phase 68
// introduced the distinction between "validation found issues" (exit 1)
// and "unexpected / internal error" (exit 2). All other codes
// (`not_implemented`, `invocation_error`, ...) collapse to exit 1.
// The mapping is centralised here so future codes pick a deliberate
// exit slot rather than inheriting "1" by accident.
func exitCodeFor(cli CLIError) int {
	switch cli.Code {
	case CodeValidationInternal:
		return 2
	default:
		return 1
	}
}
