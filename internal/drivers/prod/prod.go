// Package prod is the production driver aggregator — the single
// sanctioned home of Harbor's driver blank-import block (Phase 110c,
// D-196; AGENTS.md §4.4).
//
// Importing this package for effect:
//
//	import _ "github.com/hurtener/Harbor/internal/drivers/prod"
//
// seats EVERY production driver + LLM-wrapper registration the
// `harbor` binary boots with: the §4.4 self-registering factories
// (artifacts / audit / distributed / events / llm / memory / skills /
// state / tasks / telemetry / tools-OAuth / planner drivers), the LLM
// wrapper hooks (corrections / output-downgrade / retry / governance),
// and the notifications event-type registration. `cmd/harbor/main.go`,
// `harbortest/devstack`, and headless embedders all import this ONE
// package — closing the SDK friction audit's §7 finding (devstack
// previously hand-curated a partial list and composed the LLM client
// WITHOUT corrections / downgrade / retry; invisible under the mock
// driver, divergent against live providers).
//
// The package exports no identifiers. Deliberately NOT included:
//
//   - `internal/llm/mock` — the dev-only mock LLM driver. It is never
//     part of the production set (CLAUDE.md §13 "test stubs as
//     production defaults"); `harbor dev` conditionally imports it at
//     the subcommand boundary behind HARBOR_DEV_ALLOW_MOCK=1 (D-089),
//     and tests blank-import it explicitly.
//
// Adding a new driver: add its blank import here (with a one-line
// comment naming the phase/decision) — not to main.go, not to
// devstack. The §4.4 rule "drivers are pulled in via blank import at
// the binary entry point" reads, since 110c, "…via this aggregator,
// which the binary entry points import."
package prod

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
	// Events driver — Phase 57 StateStore-backed durable event log,
	// registered via init(). Opens its StateStore from
	// EventsConfig.StateDriver / StateDSN; an empty StateDriver
	// auto-degrades to a best-effort ring buffer with a loud warning.
	_ "github.com/hurtener/Harbor/internal/events/drivers/durable"
	// LLM corrections — Phase 34 per-provider correction layer (RFC §6.5,
	// brief 03 §4 + brief 08). Self-registers a wrapper hook in
	// internal/llm via init() so `llm.Open()` composes
	// `corrections(safetyClient(driver))` by default.
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
	// **LATENT default (D-044):** with no factory registered via
	// `governance.SetFactory`, the wrapper is a pass-through — the
	// blank-import only seats the hook. The production assembly
	// (`assemble.Assemble`, Phase 111a / D-198) installs the factory
	// when `governance.identity_tiers` is non-empty, so configured
	// tiers ENFORCE through this hook.
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
	// the blank import documents the package's presence in the binary.
	// HONESTY NOTE (SDK friction audit,
	// docs/notes/sdk-friction-audit.md §3): `skills/tools.Register` is
	// NOT called by any production path — the boot path registers the
	// thinner `internal/tools/builtin` skill_search/skill_get instead.
	// Picking ONE canonical skills-tool surface (builtin delegating to
	// these rich handlers, or a decisions.md entry formally superseding
	// Phase 38) is Wave C work — Phase 111d.
	_ "github.com/hurtener/Harbor/internal/skills/tools"
	// Skills generator — Phase 41 (`skill_propose(persist=true)`). The
	// package has no init-time registration (the catalog is built at
	// boot); the blank import documents the package's presence in the
	// binary. HONESTY NOTE: `skills/generator.Register` is NOT called
	// by any production path — same canonical-surface decision as
	// `skills/tools` above (Phase 111d).
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
	// events.ErrUnknownEventType. Blank-importing here keeps the
	// event-type registry consistent across every binary boot per the
	// §4.4 seam pattern.
	_ "github.com/hurtener/Harbor/internal/runtime/notifications"
)
