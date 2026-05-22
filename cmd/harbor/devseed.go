// cmd/harbor/devseed.go — the dev-only runtime-entity fixture seeder.
//
// Phase 75a (D-131). The Console e2e Playwright suite boots `harbor
// console` (D-091) and walks the 14 V1 pages. A fresh `harbor console`
// runtime boots EMPTY — no sessions, no agents, no tasks — so a per-page
// spec that asserts the rendered DATA shape (a `DataTable` row, a detail
// drill-down, an identity column) has nothing to render and SKIPs.
//
// `seedDevFixtures` closes that gap. When the operator sets the
// `HARBOR_DEV_SEED_FIXTURES=1` env var, `bootDevStack` calls this
// function once, immediately after the registries are constructed,
// to populate the in-memory stores with a small deterministic fixture
// set: a handful of sessions, agents, and tasks under the dev-token
// identity. The Console then renders real rows and the wave-end
// Playwright suite runs the full 14-page surface with no seed-skip.
//
// # Why this is a dev-only escape hatch, not a default
//
// Seeding fixture entities into a runtime is a TEST affordance — a
// production `harbor console` must boot empty (an operator's real
// runtime has their real sessions, not canned fixtures). This mirrors
// the §13 dev-only-escape-hatch posture of `HARBOR_DEV_ALLOW_MOCK`:
// the seed path is gated behind an explicit env var, is never the
// default, and prints a one-line stderr banner so the dev-only posture
// is unmistakable. The default path with no env var set leaves the
// runtime empty — operators get their own runtime, not a fixture kit.
//
// # Identity
//
// Every seeded entity is scoped to the dev-token identity triple
// `(dev, dev, dev)` family — the same identity the dev token carries
// (devauth.go). Sessions are opened under distinct SessionIDs within
// the `dev` tenant + `dev` user so the dev token's `admin` scope can
// fan them in via `sessions.list`. Seeding only ever writes entities
// under the `dev` tenant: it never crosses a tenant boundary, so it
// cannot corrupt a real operator's isolation surface even if the env
// var is set against a real config.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// EnvDevSeedFixtures is the env var that unlocks the dev-only runtime-
// entity fixture seeder. When set to "1", `bootDevStack` calls
// `seedDevFixtures` once at boot. Never the default — a production
// `harbor console` boots empty.
const EnvDevSeedFixtures = "HARBOR_DEV_SEED_FIXTURES"

// DevSeedBanner is the one-line stderr banner emitted on every boot
// when the fixture seeder fires, so the dev-only posture is
// unmistakable (the §13 escape-hatch surfacing convention).
const DevSeedBanner = "[DEV-ONLY FIXTURE SEED — runtime pre-populated with canned entities; DO NOT USE IN PRODUCTION]"

// devSeedSessionIDs is the deterministic set of session IDs the seeder
// opens under the dev identity. Six sessions exercise the Console
// Sessions-page DataTable pagination + the detail drill-down. The dev
// token carries the `admin` scope (devauth.go), so `sessions.list`
// fans these in across the `dev` tenant even though each session has a
// distinct SessionID.
var devSeedSessionIDs = []string{
	"dev-seed-session-1",
	"dev-seed-session-2",
	"dev-seed-session-3",
	"dev-seed-session-4",
	"dev-seed-session-5",
	"dev-seed-session-6",
}

// devSeedAgents is the deterministic agent fixture set the seeder
// registers. Three agents exercise the Console Agents-page catalog.
var devSeedAgents = []struct {
	key         string
	displayName string
	prompts     []string
}{
	{"dev-seed-research-agent", "Research Agent", []string{"You are a research assistant."}},
	{"dev-seed-triage-agent", "Triage Agent", []string{"You triage incoming requests."}},
	{"dev-seed-summary-agent", "Summary Agent", []string{"You summarise long documents."}},
}

// devSeedArtifacts is the deterministic artifact fixture set the seeder
// stores. Three small text artifacts exercise the Console Artifacts-page
// catalog DataTable + the preview pane.
var devSeedArtifacts = []struct {
	namespace string
	filename  string
	text      string
}{
	{"dev-seed-notes", "research-notes.txt", "Seeded research notes for the Console Artifacts page."},
	{"dev-seed-report", "triage-report.txt", "Seeded triage report fixture content."},
	{"dev-seed-summary", "thread-summary.txt", "Seeded archived-thread summary fixture content."},
}

// devSeedMemoryTurns is the deterministic conversation-turn fixture set
// the seeder writes into the memory store. Three turns exercise the
// Console Memory-page DataTable (`memory.list` projects one row per
// turn). Seeding is a no-op when the configured memory strategy is
// `none` (its `AddTurn` discards every turn); the embedded
// `harbor console` config uses `truncation` so the turns persist.
var devSeedMemoryTurns = []memory.ConversationTurn{
	{UserMessage: "What were the Q2 metrics?", AssistantResponse: "Q2 revenue grew 12%."},
	{UserMessage: "Summarise the archived threads.", AssistantResponse: "Three threads on the index rebuild."},
	{UserMessage: "Triage the incoming queue.", AssistantResponse: "Two items need operator review."},
}

// devSeedTools is the deterministic in-process tool fixture set the
// seeder registers into the catalog so the Console Tools page renders
// catalog rows. Each is a no-op in-process tool — the Console Tools
// page is a read-only catalog projection (Phase 73f), so the fixtures
// only need a name + description + schema, not real behaviour.
var devSeedTools = []struct {
	name        string
	description string
}{
	{"dev-seed-echo", "Echoes its input back — a seeded fixture tool."},
	{"dev-seed-search", "Runs a canned search — a seeded fixture tool."},
	{"dev-seed-summarise", "Summarises text — a seeded fixture tool."},
}

// devSeedFlows is the deterministic flow fixture set the seeder
// registers so the Console Flows page renders catalog rows. Each is a
// single-node pass-through flow — the Console Flows page (Phase 73i) is
// a read-only catalog projection, so the fixture flows only need to be
// structurally valid (one node, Entry == Exit, a non-nil node Func).
var devSeedFlows = []struct {
	name        string
	description string
}{
	{"dev-seed-research-flow", "A seeded single-node research flow fixture."},
	{"dev-seed-triage-flow", "A seeded single-node triage flow fixture."},
}

// devSeedDeps bundles the runtime registries + stores the seeder writes
// into. Constructed by `bootDevStack` once the subsystems exist; the
// seeder reads nothing else.
type devSeedDeps struct {
	sessions  *sessions.Registry
	agents    *registry.Registry
	tasks     tasks.TaskRegistry
	artifacts artifacts.ArtifactStore
	memory    memory.MemoryStore
	tools     tools.ToolCatalog
	flows     *flow.Registry
	// bus feeds the events-seeding step — a small Console-shaped event
	// stream so the Console Events page renders rows (D-132 / W11).
	bus    events.EventBus
	logger *slog.Logger
}

// seedDevFixtures populates the runtime registries with a small
// deterministic fixture set under the dev identity. It is called once,
// at boot, ONLY when `HARBOR_DEV_SEED_FIXTURES=1` — see the package
// doc for the rationale.
//
// Errors are returned (not swallowed) so a seeding failure surfaces
// loudly at boot per CLAUDE.md §5 "fail loudly"; `bootDevStack` wraps
// the error with the boot-context. A seeding failure aborts the boot —
// a runtime that booted with a half-seeded fixture set would render a
// confusingly partial Console.
func seedDevFixtures(ctx context.Context, deps devSeedDeps) error {
	// sessions / agents / tasks / artifacts / tools / flows are
	// mandatory; `memory` is optional (nil when no memory driver is
	// configured) and is guarded at its own seeding step.
	if deps.sessions == nil || deps.agents == nil || deps.tasks == nil ||
		deps.artifacts == nil || deps.tools == nil || deps.flows == nil {
		return fmt.Errorf("devseed: incomplete deps (sessions=%v agents=%v tasks=%v artifacts=%v tools=%v flows=%v)",
			deps.sessions != nil, deps.agents != nil, deps.tasks != nil,
			deps.artifacts != nil, deps.tools != nil, deps.flows != nil)
	}

	// 1. Sessions — open each under a distinct SessionID in the dev
	//    tenant. The dev token's `admin` scope fans these in via
	//    `sessions.list`.
	for _, sid := range devSeedSessionIDs {
		id := identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: sid}
		sctx, err := identity.With(ctx, id)
		if err != nil {
			return fmt.Errorf("devseed: session %q identity scope: %w", sid, err)
		}
		if _, err := deps.sessions.Open(sctx, sid, id); err != nil {
			return fmt.Errorf("devseed: open session %q: %w", sid, err)
		}
	}

	// 2. Agents — register each into the Agent Registry. The registry's
	//    enumeration index is one document per `(tenant, user, session)`
	//    (D-060), so agents are registered under the EXACT dev-token
	//    triple `(dev, dev, dev)` — that is the identity the Console
	//    queries `agents.list` with, so the registered set is visible.
	//    (`agent_id` is not an isolation principal — D-059 — but the
	//    registry's enumeration index still scopes by the caller triple.)
	devID := identity.Identity{TenantID: DevTenant, UserID: DevUser, SessionID: DevSession}
	devQuad := identity.Quadruple{Identity: devID}
	devCtx, err := identity.With(ctx, devID)
	if err != nil {
		return fmt.Errorf("devseed: dev identity scope: %w", err)
	}
	for _, a := range devSeedAgents {
		cfg := registry.AgentConfig{Prompts: a.prompts}
		if _, err := deps.agents.Register(devCtx, a.key, cfg,
			registry.RegisterOptions{DisplayName: a.displayName}); err != nil {
			return fmt.Errorf("devseed: register agent %q: %w", a.key, err)
		}
	}

	// 3. Tasks — spawn a mix of foreground + background tasks so the
	//    Console Tasks page + Background Jobs page render real rows.
	//    `tasks.list` is session-scoped (it returns the caller's
	//    session's tasks; the cross-session fan-in is admin-gated —
	//    D-079), so every task is spawned under the dev-token's exact
	//    session `dev` — the session the Console's `tasks.list` query
	//    carries — so the seeded tasks are visible without an admin
	//    fan-in.
	taskSpecs := []struct {
		kind        tasks.TaskKind
		description string
		query       string
	}{
		{tasks.KindForeground, "Foreground research turn", "Investigate the Q2 metrics"},
		{tasks.KindForeground, "Foreground triage turn", "Triage the incoming queue"},
		{tasks.KindBackground, "Background summary job", "Summarise the archived threads"},
		{tasks.KindBackground, "Background index rebuild", "Rebuild the memory index"},
	}
	for i, spec := range taskSpecs {
		req := tasks.SpawnRequest{
			Identity:       identity.Quadruple{Identity: devID},
			Kind:           spec.kind,
			Description:    spec.description,
			Query:          spec.query,
			IdempotencyKey: fmt.Sprintf("dev-seed-task-%d", i),
		}
		if _, err := deps.tasks.Spawn(devCtx, req); err != nil {
			return fmt.Errorf("devseed: spawn task %d (%s): %w", i, spec.description, err)
		}
	}

	// 4. Artifacts — store a small set of text artifacts so the
	//    Console Artifacts page renders catalog rows + a preview. The
	//    artifact store scopes by `(tenant, user, session [, task])`;
	//    the artifacts are stored under the dev-token triple so the
	//    Console's `artifacts.list` query (which carries that triple)
	//    finds them.
	artScope := artifacts.ArtifactScope{
		TenantID:  DevTenant,
		UserID:    DevUser,
		SessionID: DevSession,
	}
	for _, a := range devSeedArtifacts {
		if _, err := deps.artifacts.PutText(devCtx, artScope, a.text, artifacts.PutOpts{
			MimeType:  "text/plain; charset=utf-8",
			Filename:  a.filename,
			Namespace: a.namespace,
		}); err != nil {
			return fmt.Errorf("devseed: put artifact %q: %w", a.namespace, err)
		}
	}

	// 4b. Tools — register no-op in-process tools into the catalog so
	//     the Console Tools page renders catalog rows. The Tools page
	//     (Phase 73f) is a read-only catalog projection, so the fixture
	//     tools only need a name + description + a minimal args schema;
	//     `Invoke` is a no-op (the Console never dispatches them).
	emptyObjectSchema := json.RawMessage(`{"type":"object"}`)
	for _, td := range devSeedTools {
		desc := tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:        td.name,
				Description: td.description,
				ArgsSchema:  emptyObjectSchema,
				SideEffects: tools.SideEffectPure,
				Tags:        []string{"dev-seed"},
			},
			Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "dev-seed fixture tool — not dispatchable"}, nil
			},
			Validate: func(_ json.RawMessage) error { return nil },
		}
		if err := deps.tools.Register(desc); err != nil {
			return fmt.Errorf("devseed: register tool %q: %w", td.name, err)
		}
	}

	// 4c. Flows — register single-node pass-through flows so the
	//     Console Flows page renders catalog rows. The Flows page
	//     (Phase 73i) is a read-only catalog projection; the fixture
	//     flows only need to be structurally valid.
	for _, f := range devSeedFlows {
		const nodeID flow.NodeID = "passthrough"
		def := flow.Definition{
			Name:        f.name,
			Description: f.description,
			Entry:       nodeID,
			Exit:        nodeID,
			Nodes: map[flow.NodeID]flow.NodeSpec{
				nodeID: {
					Name: "passthrough",
					Func: func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
						return in, nil
					},
				},
			},
		}
		if err := deps.flows.Register(def, flow.Metadata{
			Owner:         "dev-seed",
			Version:       "v1",
			PlannerFamily: "graph",
			Source:        "cmd/harbor/devseed.go",
		}); err != nil {
			return fmt.Errorf("devseed: register flow %q: %w", f.name, err)
		}
		// Record two run-history entries per flow so the Console Flows
		// page detail's run-history table renders rows.
		for run := range 2 {
			if err := deps.flows.RecordRun(flow.RunRecord{
				RunID:     fmt.Sprintf("%s-run-%d", f.name, run),
				FlowName:  f.name,
				Identity:  devID,
				Trigger:   "user",
				Status:    "succeeded",
				StartedAt: time.Now().Add(-time.Duration(run+1) * time.Hour),
				Duration:  3 * time.Second,
			}); err != nil {
				return fmt.Errorf("devseed: record run for flow %q: %w", f.name, err)
			}
		}
	}

	// 5. Memory turns — append conversation turns so the Console Memory
	//    page renders catalog rows (`memory.list` projects one row per
	//    turn). The store is scoped by the `(tenant, user, session)`
	//    quadruple; turns are written under the dev-token triple. When
	//    no memory driver is configured (`deps.memory` nil) OR the
	//    configured strategy is `none` (its `AddTurn` discards turns),
	//    this step is a documented no-op — the Memory page then renders
	//    its Empty state, which is the correct shape for an unconfigured
	//    memory subsystem.
	memoryTurns := 0
	if deps.memory != nil {
		for i, turn := range devSeedMemoryTurns {
			t := turn
			t.Timestamp = time.Now()
			if err := deps.memory.AddTurn(devCtx, devQuad, t); err != nil {
				return fmt.Errorf("devseed: add memory turn %d: %w", i, err)
			}
		}
		memoryTurns = len(devSeedMemoryTurns)
	}

	// 6. Events — publish a small Console-shaped event stream onto the
	//    bus so the Console Events page renders rows AND the per-row
	//    detail-rail-on-select Playwright assertion (D-132 / W11 — the
	//    73g events-seed gap) flips from SKIP to OK. The events are a
	//    `tool.invoked` → `tool.completed` pair plus a `task.failed`,
	//    all SafePayload, published under the dev-token quadruple so the
	//    Console's identity-scoped `events.subscribe` / `events.aggregate`
	//    query observes them.
	eventsSeeded := 0
	if deps.bus != nil {
		now := time.Now()
		seedEvents := []events.Event{
			{
				Type:     tools.EventTypeToolInvoked,
				Identity: devQuad,
				Payload: tools.ToolInvokedPayload{
					Identity:  devQuad,
					ToolName:  "dev-seed-fs-read",
					Transport: tools.TransportInProcess,
					StartedAt: now,
				},
			},
			{
				Type:     tools.EventTypeToolCompleted,
				Identity: devQuad,
				Payload: tools.ToolCompletedPayload{
					Identity:   devQuad,
					ToolName:   "dev-seed-fs-read",
					Transport:  tools.TransportInProcess,
					Attempts:   1,
					DurationMS: 42,
				},
			},
			{
				Type:     tasks.EventTypeTaskFailed,
				Identity: devQuad,
				Payload: tasks.TaskFailedPayload{
					TaskID:    "dev-seed-failed-task",
					ErrorCode: "dev_seed_demo_failure",
				},
			},
		}
		for i, ev := range seedEvents {
			if err := deps.bus.Publish(ctx, ev); err != nil {
				return fmt.Errorf("devseed: publish event %d (%s): %w", i, ev.Type, err)
			}
			eventsSeeded++
		}
	}

	if deps.logger != nil {
		deps.logger.InfoContext(ctx, "devseed: runtime fixture set seeded",
			slog.Int("sessions", len(devSeedSessionIDs)),
			slog.Int("agents", len(devSeedAgents)),
			slog.Int("tasks", len(taskSpecs)),
			slog.Int("artifacts", len(devSeedArtifacts)),
			slog.Int("tools", len(devSeedTools)),
			slog.Int("flows", len(devSeedFlows)),
			slog.Int("memory_turns", memoryTurns),
			slog.Int("events", eventsSeeded),
		)
	}
	return nil
}
