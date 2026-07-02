# Phase 151 — Runtime loading-mode control on tool exposure

## Summary

Extends the agent-config tool-exposure section (`agentcfg.ToolExposure`, Phase 92d / D-234) with **loading-mode overrides** — a per-server default (`server_loading_modes`) plus per-tool overrides (`tool_loading_modes`), mirroring the shipped `PausedServers` (per-server) / `DisabledTools` (per-tool) split — so an admin can flip injected tools between `always` and `deferred` at runtime, revisioned, without a yaml edit + restart. The overrides ride the existing `agent_config.set_tool_exposure` verb (no new verb) and apply at the ONE existing run-start projection seam (`projection.ActivePlannerCatalogView`, next-turn per D-234; in-flight runs unaffected): a new `tools.LoadingOverrideView` composes beside the shipped `ExclusionView`, and `tools.describe` learns to report the projected **effective** mode. This closes the two verified v1.9 gaps: (a) the MCP driver's injected tools are pinned `LoadingAlways` (`internal/tools/drivers/mcp/mcp.go:539`) with no runtime path to defer them; (b) the runtime exposure surface carries no loading field at all. One precedence order is pinned (§13 — no second parallel knob): **exposure per-tool > exposure per-server > boot `tools.entries[].loading_mode` > driver/registration default**.

## RFC anchor

- RFC §6.4 (tool catalog and transports — `Tool.Loading`, `CatalogFilter.LoadingModes`, the MCP southbound driver whose injected-tool default this makes overridable)
- RFC §6.16 (Agent Registry — the agent-config control plane this section extends; `agent_id` as registration identity, never an isolation principal)

## Briefs informing this phase

- brief 15
- brief 03
- brief 13

## Brief findings incorporated

- brief 15 §4: "**Per-tool `ToolLoadingMode` is a first-class field.** Operators decide which tools are always-visible vs deferred at agent-yaml authoring time — no runtime guesswork, no implicit promotion." — This phase extends the same explicit-decision posture from authoring time to runtime: an override is an explicit, admin-authored, content-hashed config revision (diff/rollback for free), never an implicit runtime promotion. The runtime layer sits ABOVE the yaml layer in one pinned precedence order, so the two knobs compose instead of competing.
- brief 15 §4: "**The discovery surface is two meta-tools, not a special API.** `tool_search` and `tool_get` are tools the LLM calls like any other tool." — Discovery semantics are untouched (a Non-goal). A tool overridden to `deferred` behaves exactly like a registered-deferred tool: absent from the prompt-time catalog, surfaced by `tool_search`, resolvable on the two-turn discovery cycle (D-167 item 3).
- brief 15 §3 (path B2): "Mark each tool with a loading mode (`always` / `deferred`). Always-tools are declared every turn alongside two built-in meta-tools … the per-call declaration cost stays bounded." — The override exists precisely to let operators manage that per-call declaration cost live: an MCP server that injects 40 always-loaded tools (the mcp.go:539 default) can be re-moded to deferred for the next run, collapsing the prompt catalog to essentials without touching the transport.
- brief 03 §"Tool resolution": "the catalog applies a `CatalogFilter` to produce the planner-visible `[]Tool`" and §"Planners receive a filtered `[]Tool` view at each step, never a `ToolDescriptor`". — The override is implemented AT that filter/view seam (a `PlannerCatalogView` wrapper), not as prompt mode-switching and not as a descriptor mutation; the catalog's registered state stays immutable (D-025), per-run effect only.
- brief 13 §1 (twelve-section prompt): "Tool catalog rendering is delegated to a single per-tool renderer — the catalog row format is one place to change." — Loading overrides change WHICH tools the `<available_tools>` section renders, never HOW; no prompt fork, no new prompt section.

## Findings I'm departing from (if any)

None. (Note, for the record: brief 15 §10 sketched this engine as a "Phase 110-band" decomposition; it shipped as Phase 107c / D-167. The master plan's 110-band is the unrelated promotion band. This plan cites the shipped phase numbers.)

## Goals

- **One exposure mechanism, deepened (§13).** `agentcfg.ToolExposure` gains `ServerLoadingModes map[string]string` (keyed by `ToolSourceID`; the server-wide default for that server's tool-form descriptors) and `ToolLoadingModes map[string]string` (keyed by full catalog name; exact, unconditional). Values are exactly `"always" | "deferred"`; anything else fails the `agent_config.set_tool_exposure` request loud (`CodeInvalidRequest`, HTTP 400) with **no revision recorded and no event emitted**. Flip-back is desired-state removal (the section-replace semantics 92d already ships), not a counter-override.
- **One precedence order, stated everywhere.** Per descriptor: `tool_loading_modes[name]` > `server_loading_modes[source]` (tool-form descriptors only) > boot `tools.entries[].loading_mode` > driver/registration default. Mechanically the last two layers are already materialized into the catalog's `Tool.Loading` at boot (`internal/tools/catalog/catalog.go:318-333` — the Builder writes the yaml entry over the registrar default), so the projection applies exactly two runtime layers over the boot-effective mode. The order is pinned by a table test, stated in the `ToolExposure` godoc, and recorded in D-281.
- **Applied at the existing projection seam, next-turn only (D-234 item 3).** `projection.ActivePlannerCatalogView` (the one shared helper both the production run loop `cmd/harbor/cmd_dev_runloop.go:592` and the devstack twin `harbortest/devstack/devstack.go:1614` call — D-094 keeps them from drifting) reads the loading maps from the **admin (ConfigScopeAgent) active revision** and, when non-empty, builds the base view over a both-modes filter and wraps it in a new `tools.LoadingOverrideView`: `List()` re-applies the caller's prompt-time loading predicate against the EFFECTIVE mode (and rewrites `Tool.Loading` to it); `Resolve()` never filters on loading (matching `PlannerView.Resolve` — this is what keeps a deferred-overridden tool reachable through the D-167 two-turn discovery cycle) but rewrites `Loading`. `ExclusionView` composes outside, unchanged: paused/disabled stays hidden from BOTH `List` and `Resolve`; deferred stays hidden from `List` only. That distinction — disable removes capability, defer only removes prompt presence — is pinned by test.
- **The MCP hardcode becomes an overridable default.** `mcp.go:539` keeps registering injected tools as `LoadingAlways` — re-documented as the driver DEFAULT the boot config and the exposure override sit above, no longer a dead end. Nuance verified and preserved: MCP resources (`mcp.go:772`) and prompts (`mcp.go:814`) already register `LoadingDeferred` and stay driver-default (see the ToolForm decision below).
- **Per-server scope = tool-form descriptors only, via an additive `tools.Tool.Form` field.** A server-level `always` must not blanket-flip a server's dozens of wrapped resources/prompts into every prompt — that would recreate the prompt-budget problem deferred loading exists to solve. The projection cannot §13-cleanly sniff the MCP driver's name conventions (`resource.` / `prompt.` sub-names are driver-internal constants), so `tools.Tool` gains an additive classification field `Form ToolForm` (`""`/`ToolFormTool`, `ToolFormResource`, `ToolFormPrompt`) that the MCP driver stamps at build time. Classification only — never a dispatch or isolation input. Per-TOOL overrides remain exact and unconditional (an operator who explicitly names a resource-form descriptor gets what they asked for).
- **Transport-agnostic by construction.** The projection seam operates on `PlannerCatalogView` + `Tool.Source`/`Tool.Name` — nothing MCP-specific. HTTP and A2A provider descriptors are covered by the same mechanism with zero driver changes; only the MCP driver's always-hardcode needed the narrative fix, and its only code change is the `Form` stamp + godoc.
- **The read surface reports the effective mode.** Verified: `tools.describe` reads the RAW catalog (`internal/tools/protocol/catalog_projector.go:252-265` — boot-effective `t.Loading`, agent-config-unaware). `ToolDescribeRequest` gains optional `agent_id`; the `CatalogProjector` gains a narrow injected resolver seam (mirroring its existing `annotator` precedent) that consults the agent's active admin revision and reports the projected EFFECTIVE `loading_mode`. Absent `agent_id` (or no active revision) → boot-effective, byte-compatible with today.
- **Full wire lockstep in the same PR.** Wire-type changes (`AgentConfigToolExposure`, `AgentConfigToolExposureDiff` + a new `LoadingModeChange` shape, `ToolDescribeRequest`) are additive (no ProtocolVersion bump): `make protocol-ts-gen` + the hand-mirrored TS interfaces (`web/console/src/lib/protocol/agentconfig.ts`, the tools page module) + `make protocol-ts-gen-check` green (D-223), `make protocol-docs-gen` + committed pages (D-209). `agentcfg.DiffToolExposure` gains loading arms so the revision diff (and the Console's diff rendering) shows loading changes; audit surface = `agent.config.revised` + the structured diff (no new event type).

## Non-goals

- **New Protocol verbs.** `agent_config.set_tool_exposure` carries the new fields; no `set_loading_mode` sibling.
- **Per-run (non-revisioned) loading overrides.** Every change is a config revision (diff/rollback, next-turn projection). A run-scoped one-shot knob is a different primitive and would need its own decision.
- **User-tier / session-overlay loading fields.** The D-256/D-258 tiers stay narrow-only grow-only DISABLE sets — a precedence-bearing map has no commutative cross-tier merge, and loading is not capability-narrowing (a deferred tool stays reachable via `tool_search`; `tools.VisibleNames` spans both modes), so it belongs to the admin desired-state tier only.
- **`tool_search` / discovery semantics changes.** The meta-tools, the two-turn cycle, `RunContext.DiscoveredTools` — all untouched.
- **A2A/HTTP driver loading changes** beyond what the transport-agnostic projection gives free (verified: nothing driver-side needed).
- **New event types.** Loading edits ride `agent.config.revised` + the structured diff. `mcp.connection.paused`/`.resumed` remain pause-specific.
- **Console UI.** The typed client mirrors the wire (D-223 obligation); an agent-config panel affordance for loading is a later Console page phase (D-062 ordering is satisfied — the Protocol surface lands first).
- **The app→host current-state gate** (`gateToolExposure`, D-234 item 4) is deliberately untouched: loading mode is prompt-presence, not authorization — pause/disable remain its only inputs.

## Acceptance criteria

- [x] `agentcfg.ToolExposure` carries `ServerLoadingModes` / `ToolLoadingModes` (`map[string]string`, json `server_loading_modes` / `tool_loading_modes`); normalization sorts/dedups deterministically (empty keys and empty maps normalize away — no phantom diff), values are validated to `always|deferred`, and a content-hash stability test proves an idempotent re-set produces a zero diff and the same hash.
- [x] `agent_config.set_tool_exposure` with an unknown loading value (either map) fails loud with `CodeInvalidRequest` (HTTP 400) BEFORE any registry write: the revision chain is unchanged and no event fires (asserted).
- [x] `projection.ActivePlannerCatalogView` applies the effective mode via `tools.LoadingOverrideView`: with an `always→deferred` per-tool override, the next run's prompt-time `List()` excludes the tool while `Resolve()` still returns it (discovery-callable) and `tool_search` still surfaces it; with a `deferred→always` override the tool appears in the prompt-time `List()`. Removing the override restores boot-effective behavior. In-flight runs keep their snapshot (D-025/D-234 — asserted by starting a run before the flip).
- [x] Paused/disabled remains strictly stronger than deferred: an excluded tool is absent from BOTH `List` and `Resolve` (the `ExclusionView` contract), pinned side-by-side with the deferred case in one test.
- [x] Per-server overrides apply only to `Form == tool` descriptors: an MCP fixture's resources/prompts keep `LoadingDeferred` under a server-level `always`; `tools.Tool.Form` is stamped by the MCP driver (`resource` / `prompt` at mcp.go:772/:814 build sites; tools stay zero-value).
- [x] The precedence table test pins all four layers: exposure per-tool > exposure per-server > `tools.entries[].loading_mode` (boot) > driver default — including the interaction rows (boot-deferred tool + server-level `always`; boot-always tool + per-tool `deferred`; per-tool beats per-server).
- [x] `tools.describe` with `agent_id` reports the projected effective `loading_mode`; without `agent_id` it reports the boot-effective mode byte-compatibly with today (both asserted).
- [x] D-223 lockstep green in the same PR: `make protocol-ts-gen` regenerates `wire-manifest.gen.json`; the hand-mirrored TS interfaces gain the fields; `make protocol-ts-gen-check` + `npm run lint` pass. D-209: `make protocol-docs-gen` regenerated pages committed.
- [x] `agentcfg.DiffToolExposure` renders loading changes as structured `LoadingModeChange` entries (server + tool arms); the `agent_config.diff` wire projection carries them.
- [x] Integration test (`test/integration/`, `-race`, real drivers incl. the real MCP stdio fixture `cmd/harbor-mcptest-stdio`): the full flip/flip-back scenario, identity propagation, the invalid-mode failure mode, and cross-agent + cross-session isolation (a second `agent_id` and a second tenant's identical-named agent see no change — §6 rule 10).
- [x] Concurrency: N≥100 concurrent runs share one catalog + registry while an admin flips loading overrides; every run's view is internally consistent (no torn snapshot: a run never sees the tool in `List()` under one mode and reports another via `Resolve().Loading`), no goroutine leak, `-race` clean.
- [x] `scripts/smoke/phase-151.sh` flips from skeleton to real assertions (static greps + the live flip below) with FAIL = 0 and OK ≥ 6; prior 92-band smokes still pass.
- [x] §18 sweep: `docs/skills/use-the-harbor-protocol/SKILL.md`'s agent-config paragraph gains the loading-override sentence in the same PR; `docs/site` generated protocol pages regenerated.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — `ToolExposure` fields, normalization, accessors, `DiffToolExposure` loading arms, `LoadingModeChange`
- `internal/agentcfg/agentcfg_test.go` — normalization / hash-stability / diff tests
- `internal/runtime/agentcfg/protocol/mcppolicy.go` (+ `_test.go`) — edge validation, revision preservation of the new fields
- `internal/runtime/agentcfg/projection/projection.go` (+ `_test.go`) — effective-mode resolution + `LoadingOverrideView` wiring in `ActivePlannerCatalogView`
- `internal/tools/tools.go` — `ToolForm` type + `Tool.Form` field (additive)
- `internal/tools/planner_view.go` (+ `planner_view_test.go`) — `LoadingOverrideView` / `NewLoadingOverrideView`
- `internal/tools/drivers/mcp/mcp.go` (+ tests) — `Form` stamps at the resource/prompt build sites; godoc reframe of the `LoadingAlways` default at the tool build site (⚠ coordinate with Phase 148 — same file)
- `internal/tools/protocol/catalog_projector.go` (+ tests) — the injected effective-loading resolver seam; `DescribeTool` honors `agent_id`
- `internal/protocol/types/agentconfig.go`, `internal/protocol/types/tools.go` — wire fields (`server_loading_modes` / `tool_loading_modes`, diff arms, `ToolDescribeRequest.agent_id`) (⚠ agentconfig.go also touched by Phase 148)
- `web/console/src/lib/protocol/agentconfig.ts`, the tools page wire module, `web/console/src/lib/protocol/wire-manifest.gen.json` (regenerated) — D-223 lockstep
- `docs/site/protocol/` — regenerated (D-209)
- `cmd/harbor/cmd_dev_runloop.go` / `harbortest/devstack/devstack.go` — no changes expected (both call the shared projection helper); the boot wiring that injects the projector's loading resolver lands beside the existing annotator wiring
- `test/integration/agentcfg_loading_exposure_test.go`
- `scripts/smoke/phase-151.sh`
- `docs/skills/use-the-harbor-protocol/SKILL.md`, `docs/glossary.md`, `docs/decisions.md` (D-281), `docs/plans/README.md` (row + detail block)

## Public API surface

- `agentcfg.ToolExposure{ …, ServerLoadingModes map[string]string, ToolLoadingModes map[string]string }`
- `agentcfg.LoadingModeChange{ Key, From, To string }` (diff element; `From`/`To` `""` = unset)
- `tools.ToolForm` (`ToolFormTool` zero / `ToolFormResource` / `ToolFormPrompt`) + `tools.Tool.Form`
- `tools.NewLoadingOverrideView(inner PlannerCatalogView, effective map[string]LoadingMode, visibleModes []LoadingMode) LoadingOverrideView`
- Wire (additive): `AgentConfigToolExposure.server_loading_modes` / `.tool_loading_modes`; `AgentConfigToolExposureDiff` loading arms; `ToolDescribeRequest.agent_id` (optional)

## Test plan

- **Unit:** precedence table (all four layers + interaction rows); `LoadingOverrideView` List/Resolve semantics incl. `Loading` rewrite and the Resolve-never-filters rule; edge validation table (both maps, both bad-value shapes, empty-key rejection); normalization + content-hash stability; `DiffToolExposure` loading arms; describe with/without `agent_id`; `Form` stamping in the MCP driver's descriptor builders.
- **Integration:** `test/integration/agentcfg_loading_exposure_test.go` — real drivers on every seam (StateStore-backed agentcfg registry, inmem bus/state, real Protocol service + wire handler, real catalog, the shared run-start projection, the real MCP stdio fixture `cmd/harbor-mcptest-stdio` per §17.8): `set_tool_exposure` flips the fixture's `echo` tool to deferred → next projection's prompt-time list excludes it, `tool_search` surfaces it, `Resolve` returns it; flip back → visible; invalid mode → loud 400, revision chain unchanged; identity propagation end-to-end; cross-agent + cross-tenant isolation; a disabled tool cannot be dispatched even if `tool_search` (raw-catalog-backed) still lists it. All `-race`.
- **Conformance:** N/A — no new driver seam (the agentcfg driver conformance suite gains the new-fields round-trip case in its existing payload tests).
- **Concurrency / leak:** N≥100 concurrent `ActivePlannerCatalogView`+run cycles against one shared catalog/registry with concurrent admin flips — per-run snapshot consistency, no cross-run bleed, goroutine baseline restored, `-race` (D-025).

## Smoke script additions

`scripts/smoke/phase-151.sh` (`# PREFLIGHT_REQUIRES: live-server`):

- Static greps: the two map fields on `agentcfg.ToolExposure` + the wire type; `LoadingOverrideView` in `internal/tools/planner_view.go`; the projection wiring in `projection.go`; `Form` in `tools.go`; the TS mirror fields in `web/console/src/lib/protocol/agentconfig.ts`; the regenerated docs row.
- Live (mirroring the phase-114 bootstrap pattern): bootstrap an admin dev token; resolve the dev agent id (`agents.list`, SKIP when absent); `tools.describe` a known built-in and pin `loading_mode`; `agent_config.set_tool_exposure` with a `tool_loading_modes` flip → re-describe with `agent_id` asserting the effective `loading_mode` flipped; a request with `"loading":"sometimes"` → 400 `invalid_request`; restore (set exposure without the override) → describe reports the original mode. Unauthenticated POST stays non-200 (the 92d gate assertion, re-checked).

## Coverage target

- `internal/agentcfg`: ≥ 85%
- `internal/runtime/agentcfg/protocol` + `internal/runtime/agentcfg/projection`: ≥ 85% on touched files
- `internal/tools` (planner_view + protocol projector touched lines): no regression below current package coverage
- `internal/tools/drivers/mcp`: no regression

## Dependencies

- 92a (the agent-config registry primitive + revision machinery — D-234/D-235)
- 92d (the ToolExposure section, `set_tool_exposure`, `ExclusionView`, the run-start projection this extends)
- 107c (the deferred-loading engine: `LoadingMode`, `CatalogFilter.LoadingModes`, `tool_search`, the two-turn discovery cycle — D-167; brief 15's "110-band" sketch, shipped under this number)
- 110a (`tools.NewPlannerView` — the promoted view seam the projection composes on)
- 118 (the D-223 TS lockstep gate the wire changes must satisfy)
- **Coordination note (staging, not semantic):** lands AFTER Phase 148 merges — 148 touches the same files (`internal/tools/drivers/mcp/mcp.go`, `internal/protocol/types/agentconfig.go`, `web/console/src/lib/protocol/agentconfig.ts`); serializing avoids conflict churn, nothing in this design depends on 148's content.

## Risks / open questions

- **`Tool.Form` vs the RFC §6.4 struct sketch.** The field is additive with precedent (`HandlesMIME` shipped beyond the sketch), and it exists to avoid §13-unclean name-convention sniffing across the driver boundary. If review deems it RFC territory, the implementing PR adds the one field line to the §6.4 sketch (a mechanical additive edit) per §4.3 — flagged here so it is a decision, not drift. Rejected alternatives are recorded in D-281.
- **`tool_search` sees the raw catalog, not the run view (pre-existing, adjacent).** The meta-tool's closure captures the catalog at registration, so a paused/disabled tool can still appear in search results even though the run view refuses to resolve/dispatch it (safe but confusing). This phase's integration test pins the safety half (no dispatch); re-basing `tool_search` on the run's projected view is a named follow-up if operator reports surface the UX half — changing `tool_search` semantics is an explicit Non-goal here.
- **Effective-mode reads and multi-agent runtimes.** `tools.describe` reports per-`agent_id` effective mode from the ADMIN tier only (where the overrides live). If a later phase adds user-tier loading (currently a Non-goal), the describe seam must grow a tier argument — the narrow resolver interface keeps that additive.
- **Map fields in the content hash.** Go's canonical JSON encoding sorts map keys, so hashes are stable; the normalization test pins this so a future refactor to a non-map shape cannot silently perturb existing revision hashes.
- **Torn-view hazard.** The projection reads the active revision ONCE and materializes one effective map per run; the concurrency AC exists because an implementor could be tempted to resolve per-`Resolve()` call against live state — that would break D-025's snapshot semantics.

## Glossary additions

- **loading-mode override** — the runtime, revisioned layer of the tool loading-mode precedence chain: `ToolExposure.ToolLoadingModes` (per-tool, exact) over `ServerLoadingModes` (per-server, tool-form descriptors only) over the boot `tools.entries[].loading_mode` over the driver/registration default. Applied next-turn at the run-start projection; not capability-narrowing (a deferred tool stays reachable via `tool_search`). Phase 151, D-281.
- **`LoadingOverrideView`** — the `PlannerCatalogView` wrapper that materializes effective loading modes for one run: `List()` applies the prompt-time loading predicate against the effective mode; `Resolve()` never filters on loading (preserving the two-turn discovery cycle); both rewrite `Tool.Loading` to the effective mode. Sibling of `ExclusionView`. Phase 151, D-281.
- **`ToolForm`** — the additive `tools.Tool` classification (`tool` zero-value / `resource` / `prompt`) drivers stamp so cross-cutting policy (per-server loading overrides) can distinguish callable tools from wrapped MCP resources/prompts without name-convention sniffing. Classification only — never a dispatch or isolation input. Phase 151, D-281.

(Added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes — deferred to CI per the dispatch contract (local run skipped; `HARBOR_PREFLIGHT_SKIP=1` on the commit, justified in the PR body). `go vet`, `go test -race ./...`, the static smoke legs, and the D-223/D-209 gates were all run and are green locally.
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (the exposure registry is identity-scoped — the cross-agent/cross-tenant integration assertions are mandatory)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (`LoadingOverrideView` + the extended projection are per-run value types over shared artifacts — the N≥100 flip-under-load test is mandatory.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 92a/92d/107c/110a — `test/integration/agentcfg_loading_exposure_test.go` is mandatory.)
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
