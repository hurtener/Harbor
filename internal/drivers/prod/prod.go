// Package prod is the production driver aggregator — the single
// sanctioned home of Harbor's driver blank-import block
// (AGENTS.md §4.4).
//
// Importing this package for effect:
//
//	import _ "github.com/hurtener/Harbor/internal/drivers/prod"
//
// seats EVERY production driver + LLM-wrapper registration the
// `harbor` binary boots with: the §4.4 self-registering factories
// (artifacts / audit / distributed / events / llm / memory / skills /
// state / tasks / telemetry / tools-OAuth / planner drivers), the LLM
// wrapper hooks (corrections / external grants / output-downgrade / retry /
// governance),
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
//     the subcommand boundary behind HARBOR_DEV_ALLOW_MOCK=1,
//     and tests blank-import it explicitly.
//
// Adding a new driver: add its blank import here (with a one-line
// comment naming the phase/decision) — not to main.go, not to
// devstack. The §4.4 rule "drivers are pulled in via blank import at
// the binary entry point" now reads "…via this aggregator,
// which the binary entry points import."
package prod

import (
	// Agent-config driver — StateStore-backed versioned desired-state
	// registry (the agent-config control plane), registered via init().
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	// Artifacts drivers — content-addressed blob store. Each V1
	// driver self-registers via init() so `artifacts.Open` can resolve
	// them. Harbor ships fs + inmem; Harbor adds sqlite +
	// postgres; Harbor adds the S3-style driver.
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"

	// Audit driver — production redactor, registered via init().
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Distributed drivers — loopback MessageBus + RemoteTransport.
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	// Distributed bus driver — StateStore-backed durable MessageBus,
	// registered via init(). Persists every BusEnvelope through the
	// shared StateStore and projects it onto the local event bus, with a
	// poller for cross-instance / restart-replay delivery; opt-in via
	// distributed.bus_driver: durable.
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/durable"
	// Distributed driver — A2A wire RemoteTransport (southbound).
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
	// Embeddings driver — bifrost-backed Embedder,
	// registered via init(). Feeds the opt-in semantic retrieval
	// modes in memory + skills and the à-la-carte embed path.
	_ "github.com/hurtener/Harbor/internal/embeddings/drivers/bifrost"
	// Events driver — production in-memory bus, registered via init().
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// Events driver — StateStore-backed durable event log,
	// registered via init(). Opens its StateStore from
	// EventsConfig.StateDriver / StateDSN; an empty StateDriver
	// auto-degrades to a best-effort ring buffer with a loud warning.
	_ "github.com/hurtener/Harbor/internal/events/drivers/durable"
	// LLM corrections — per-provider correction layer (RFC §6.5).
	// Self-registers a wrapper hook in
	// internal/llm via init() so `llm.Open()` composes
	// `corrections(safetyClient(driver))` by default.
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	// LLM external execution grants — verifies context-bound signed grants,
	// resolves opaque credential bindings at the Bifrost boundary, and queues
	// content-free attempt receipts. Legacy behavior remains disabled unless
	// the runtime explicitly enables the grant mode.
	_ "github.com/hurtener/Harbor/internal/llm/grant"
	// LLM driver — bifrost-backed LLMClient, registered via init().
	_ "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	// LLM output — structured-output downgrade chain (RFC §6.5).
	// Self-registers a wrapper hook so `llm.Open()` composes
	// `downgrade(corrections(safetyClient(driver)))`.
	_ "github.com/hurtener/Harbor/internal/llm/output"
	// LLM retry — retry-with-feedback (RFC §6.5). Self-
	// registers a wrapper hook so `llm.Open()` composes
	// `retry(downgrade(corrections(safetyClient(driver))))`.
	_ "github.com/hurtener/Harbor/internal/llm/retry"
	// Governance — + 36b cost accumulator + rate limiter +
	// MaxTokens enforcer (RFC §6.15). Self-registers a wrapper hook
	// so `llm.Open()` composes `governance(retry(...))` outermost.
	// **LATENT default:** with no factory registered via
	// `governance.SetFactory`, the wrapper is a pass-through — the
	// blank-import only seats the hook. The production assembly
	// (`assemble.Assemble`,) installs the factory
	// when `governance.identity_tiers` is non-empty, so configured
	// tiers ENFORCE through this hook.
	_ "github.com/hurtener/Harbor/internal/governance"
	// Memory driver — in-memory MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	// Memory driver — Postgres MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	// Memory driver — SQLite MemoryStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	// Skills driver — LocalDB SkillStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	// Skills driver — Postgres SkillStore (durable/shared storage),
	// registered via init().
	_ "github.com/hurtener/Harbor/internal/skills/drivers/postgres"
	// Skills planner tools — (`skill_search` / `skill_get` /
	// `skill_list`). The package has no init-time registration
	// (catalogs are constructed at boot, not from a factory registry);
	// the blank import documents the package's presence in the binary.
	// Since these handlers ARE the production path:
	// the `internal/tools/builtin` carrier delegates the skill_* set
	// to them, so capability filtering + redaction + the budgeter run
	// on every production call.
	_ "github.com/hurtener/Harbor/internal/skills/tools"
	// Skills generator — (`skill_propose(persist=true)`). The
	// package has no init-time registration (the catalog is built at
	// boot); the blank import documents the package's presence in the
	// binary. Since the builtin carrier registers
	// `skill_propose` as a delegation to `generator.Propose` when the
	// operator opts in via `tools.built_in`.
	_ "github.com/hurtener/Harbor/internal/skills/generator"
	// State driver — production in-memory StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	// State driver — Postgres StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	// State driver — production SQLite StateStore, registered via init().
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	// Tasks driver — production in-process TaskRegistry, registered via init().
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	// Tasks driver — StateStore-backed durable TaskRegistry, registered
	// via init(). Persists task/group/patch records through the shared
	// runtime StateStore so they survive a restart; opt-in via
	// tasks.driver: durable.
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/durable"
	// Telemetry span exporters — OTel traces. The noop driver
	// is the default (no collector); the otlp driver ships spans to an
	// OTLP/gRPC collector when telemetry.otel_endpoint is configured.
	// Both self-register via init() so `telemetry.NewTracer` resolves
	// them.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"

	// Telemetry metric exporters — OTel metrics. The
	// prometheus driver is the default (built-in /metrics pull
	// endpoint, no collector); the otlpmetric driver pushes metrics to
	// an OTLP/gRPC collector when telemetry.otel_endpoint is
	// configured. Both self-register via init() so
	// `telemetry.NewMetricsRegistry` resolves them.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"

	// Tools OAuth driver (closes issue #116). The `oauth2`
	// driver self-registers under that name via init() so
	// `tools.oauth_providers[].driver: oauth2` resolves at boot. New
	// OAuth flow strategies (device-code, vendor-specific) add a new
	// driver under `internal/tools/auth/drivers/<name>/` + a blank
	// import here, per the §4.4 seam pattern.
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/oauth2"
	// Tools external-credential provisioning driver. The
	// `tokenexchange` driver self-registers under that name via init()
	// so `tools.oauth_providers[].driver: tokenexchange` resolves at
	// boot — the pull-based, RFC-8693-shaped acquisition strategy that
	// obtains downstream tool credentials from an external credential
	// broker instead of Harbor's interactive authorization-code flow.
	_ "github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	// Tools OAuth provider credential sources. A provider's OWN
	// client credential resolves through the §4.4 credential-source seam:
	// the `env` source (boot-time process-env resolution, the default)
	// and the `remote` source (an authenticated pull from a coordinator
	// endpoint at first need). Each self-registers under its name via
	// init() so `tools.oauth_providers[].credential_source` resolves at
	// boot. New sources add a driver under
	// `internal/tools/auth/credsource/drivers/<name>/` + a blank import
	// here, per the §4.4 seam pattern.
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/env"
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/remote"

	// Planner driver (closes issue #126). The `react` driver
	// self-registers under that name via init() so
	// `planner.driver: react` resolves at boot. New planner concretes
	// (Plan-Execute, Workflow, Graph, Deterministic, Supervisor,
	// MultiAgent, HumanApproval per RFC §6.2) add a new driver under
	// `internal/planner/<name>/` + a blank import here, per the §4.4
	// seam pattern used for OAuth providers. The V1
	// reference planner remains the no-config-needed default.
	_ "github.com/hurtener/Harbor/internal/planner/react"
	// Notifications event topic — The package's init()
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
