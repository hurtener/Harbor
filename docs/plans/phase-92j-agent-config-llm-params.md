# Phase 92j — agent-config control plane: per-agent LLM parameters

## Summary

Adds a **versioned, per-agent LLM-parameters section** (model / temperature / max-tokens / reasoning-effort) to the agent-config `ConfigPayload`, so an admin can pin an agent's sampling defaults durably and roll them back like any other config edit. This corrects the scope of the earlier tenant-default override work, which set those defaults **tenant-wide** (one spec for every agent in the tenant): the per-agent layer now sits **between** the session override and the tenant-wide baseline. The effective per-run resolution becomes **session › per-agent › tenant-wide baseline › config default**, resolved at run-start through the shared projection so the production run loop and the devstack twin cannot drift.

## RFC anchor

- RFC §6.15 — Governance (the model-override seam; the desired-state-then-reconcile pattern the agent-config registry generalises).
- RFC §6.16 — Agent Registry (the agent-config control plane this section extends).
- RFC §6.5 — LLM client (the `Model` / `Temperature` / `MaxTokens` / `ReasoningEffort` request surface the section pins).

## Briefs informing this phase

- brief 03
- brief 08

## Brief findings incorporated

- **brief 03 §"Two parallel LLM modes (the toggle smell)":** a second mechanism that re-implements an existing one in parallel is an anti-pattern — "Harbor picks one architecture." This phase does NOT add a parallel per-agent override engine; it adds one new layer to the SAME `ComposeLLMOverrides` precedence chain and the SAME run-start projection, so there is exactly one resolution path.
- **brief 03 (the `LLMClient` request shape, §"interface"):** the per-run sampling surface is `Model` / `Temperature` (float32) / `ReasoningEffort` ("off" | "low" | "medium" | "high" | "") — plus `MaxTokens`. The per-agent section pins exactly these fields and nothing else; the existing `applyLLMOverrides` is the single application point.
- **brief 08 §"Per-model seam (operator-facing)":** operators express per-model defaults including `reasoning_effort` (e.g. `reasoning_effort: low`) as operator-facing config. This phase mirrors that operator vocabulary but makes it **durable + versioned + per-agent** through the agent-config registry instead of static boot config — an admin changes an agent's model without a redeploy, and the change is a revision (diff/rollback for free).

## Findings I'm departing from (if any)

None. The tenant-wide override is intentionally retained as the BASELINE layer (the operator's decision: a tenant default that every agent inherits unless it pins its own), so this phase is additive — it does not remove or rescope the tenant-wide spec, it inserts a more-specific layer above it.

## Goals

- A new optional `LLMParams` section on the agent-config `ConfigPayload` (`internal/agentcfg`): `Model *string`, `Temperature *float64`, `MaxTokens *int`, `ReasoningEffort *string` — all pointer-optional, each field independently set-or-unset (a partial section is valid; an unset field falls through to the next layer).
- The section participates in the existing revision machinery for free: content-hash, parent pointer, `agent_config.set_revision` (full payload), a per-section convenience verb (`agent_config.set_llm_params`) that preserves sibling sections, the server-side `agent_config.diff` (a new structured `LLMParams` diff arm), `agent_config.rollback`, and the `agent.config.revised` event.
- Run-start resolution gains the per-agent layer: a shared projection helper reads the agent's active revision, extracts `LLMParams`, and `ComposeLLMOverrides` folds it in precedence **session › per-agent › tenant-wide baseline › config**. The production run loop (`resolveLLMOverrides`) and the devstack twin both call the shared helper (D-094).
- Admin-scoped writes only (the capability tier, D-235): pinning an agent's model/sampling is a deployment-level change. Session callers do not get an LLM-params verb in the safe subset (92g); their per-run knob remains the existing one-shot `runs.set_overrides`.
- Unknown-model fail-loud parity with the tenant swap (Phase 92): a pinned `Model` with no resolvable `ModelProfile` fails loudly at the point the tenant swap already fails, not silently.

## Non-goals

- Removing or rescoping the tenant-wide override (it stays as the baseline layer — see "Findings I'm departing from").
- A session-scoped per-agent LLM-params verb (out of the 92g safe subset; per-run session sampling stays on `runs.set_overrides`).
- Per-tool or per-skill model routing (a single agent-level sampling profile only).
- `ExtraInstructions` in the per-agent section — additive system-prompt text is already the agent-config **prompt layers** (92e); the LLM-params section is sampling parameters only, to avoid two homes for prompt text.
- Console rendering of the new section — that is 92i's "Model & sampling" area + the Settings copy clarification (twinned in the same wave, this phase ships the backend + the typed client + generated docs).

## Acceptance criteria

- [ ] `ConfigPayload` carries an optional `LLMParams` section (Model / Temperature / MaxTokens / ReasoningEffort, all pointer-optional); the canonical content-hash encoding includes it (a revision that only changes LLM params has a distinct hash).
- [ ] `agent_config.set_revision` accepts the section in the full payload; `agent_config.set_llm_params` writes a single-section revision that PRESERVES sibling sections (the bidirectional section-merge precedent from 92d/92e); both are admin-scoped, fail-closed on a non-admin (`CodeScopeMismatch`), authority from the verified ctx (D-219/D-235).
- [ ] `agent_config.diff` reports a structured LLM-params delta (which of model/temperature/max-tokens/reasoning-effort changed, with from/to values); the `agent.config.revised` event fires on every write.
- [ ] Run-start resolution composes **session › per-agent › tenant-wide baseline › config**: a per-agent `Model` overrides the tenant-wide `Model`; an unset per-agent field falls through to the tenant baseline then config. A test asserts every field's precedence independently.
- [ ] The resolution lives in the shared `projection` package and is called by BOTH `cmd/harbor/cmd_dev_runloop.go::resolveLLMOverrides` and the `harbortest/devstack` twin (D-094); a twin test asserts the two binaries resolve identically.
- [ ] A pinned `Model` with no `ModelProfile` fails loudly at run-start (parity with the Phase 92 tenant swap), never silently falls back.
- [ ] Identity scoped by the full triple; the per-agent layer keys on `{tenant, "__agentcfg__", agentID}` — `agent_id` is the registry key, NOT an isolation filter (§6).
- [ ] TS wire manifest + typed `AgentConfigNamespace` client + generated Protocol docs regenerated; `make protocol-ts-gen-check` and `make protocol-docs-gen-check` clean; `scripts/smoke/phase-92j.sh` green.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — the `LLMParams` section type + its place in `ConfigPayload` + the canonical-encoding/content-hash inclusion.
- `internal/runtime/agentcfg/protocol/{service.go,llmparams.go}` (+ tests) — the `set_llm_params` convenience verb (single-section revision, sibling-preserving) + the admin-scope gate + the diff arm.
- `internal/runtime/agentcfg/projection/projection.go` (+ tests) — `ActiveLLMOverrides(ctx, registry, agentCfgID, q) (*planner.LLMOverrides, error)` — read the active revision, extract `LLMParams`, return the override layer (mirrors `ActiveSkillViews`).
- `internal/runtime/runs/protocol/overrides.go` — extend `ComposeLLMOverrides` to fold the per-agent layer between session and tenant (or a 3-arg compose); the precedence stays per-field.
- `cmd/harbor/cmd_dev_runloop.go::resolveLLMOverrides` + `harbortest/devstack/devstack.go` — call the shared projection for the per-agent arm (D-094 twin).
- `internal/protocol/{methods/methods.go,types/agentconfig.go,errors/errors.go?,singlesource/singlesource.go,transports/stream/agentconfig_handler.go}` + the generator typeindex — the new method + wire types (`AgentConfigLLMParams`, `AgentConfigLLMParamsDiff`) + the diff response arm.
- `web/console/src/lib/protocol/agentconfig.ts` + `web/console/src/lib/protocol/wire-manifest.gen.json` (regenerated) + `client.ts` — the typed shape + verb.
- `docs/site/protocol/{methods.md,events.md,types.md}` (regenerated) — generated rows.
- `docs/skills/` — any skill that demonstrates an `agent_config` write (§18 same-PR rule).
- `scripts/smoke/phase-92j.sh`.

## Public API surface

```go
// LLMParams pins an agent's sampling defaults as a versioned config section.
// All fields are pointer-optional: an unset field falls through to the next
// resolution layer (tenant-wide baseline, then config default).
type LLMParams struct {
    Model           *string  `json:"model,omitempty"`
    Temperature     *float64 `json:"temperature,omitempty"`
    MaxTokens       *int     `json:"max_tokens,omitempty"`
    ReasoningEffort *string  `json:"reasoning_effort,omitempty"` // off|low|medium|high
}

// ActiveLLMOverrides resolves the per-agent LLM-params layer at run start
// from the agent's active config revision. nil ⇒ no per-agent override.
func ActiveLLMOverrides(ctx context.Context, reg Registry, agentCfgID string, q identity.Quadruple) (*planner.LLMOverrides, error)

// ComposeLLMOverrides folds the layers in precedence:
//   session › agent (per-agent agentcfg) › tenant (tenant-wide baseline).
// config default is applied last by applyLLMOverrides (unset ⇒ untouched).
func ComposeLLMOverrides(session *PendingOverride, agent, tenant *planner.LLMOverrides) *planner.LLMOverrides
```

## Test plan

- **Unit:** `LLMParams` content-hash inclusion (LLM-only edit ⇒ distinct hash); `set_llm_params` preserves siblings; admin-gate (non-admin ⇒ `CodeScopeMismatch`); the diff arm (each field's from/to); `ComposeLLMOverrides` per-field precedence (session beats agent beats tenant, independently per field); unknown-model fail-loud.
- **Integration:** `test/integration/agentcfg_llm_params_test.go` — real registry + bus + handler + a stub LLM capturing the resolved request: admin sets a tenant-wide baseline AND a per-agent model → a run for that agent uses the per-agent model; a run for a DIFFERENT agent (no per-agent pin) uses the tenant baseline; a session `runs.set_overrides` beats the per-agent pin for that one run; rollback of the per-agent revision restores the tenant baseline; under `-race`. Identity propagation asserted across the registry → projection → run-context → request path.
- **Conformance:** reuses the `internal/agentcfg/conformance` suite (the new section round-trips through all three drivers — in-mem / SQLite / Postgres — with parity).
- **Concurrency / leak:** N≥100 concurrent run-start resolutions against a single shared registry under `-race`; no cross-agent / cross-tenant bleed of the resolved params; goroutine baseline restored.

## Smoke script additions

- `scripts/smoke/phase-92j.sh`: static — the `LLMParams` section symbol in `internal/agentcfg`; the `set_llm_params` method constant + handler branch + admin gate; the `ActiveLLMOverrides` projection symbol called from BOTH `cmd_dev_runloop.go` and `devstack.go` (the twin grep); the extended `ComposeLLMOverrides` arity; the regenerated TS manifest + generated-docs rows. Live (skip-if-404) — an admin token sets a per-agent model via `set_llm_params` (200) and `agent_config.get` reflects it; a non-admin token is rejected (403); `agent_config.diff` reports the LLM-params delta.

## Coverage target

- `internal/agentcfg` (the section + encoding): 85%
- `internal/runtime/agentcfg/protocol` (the verb + gate + diff arm): 85%
- `internal/runtime/agentcfg/projection` (`ActiveLLMOverrides`): 85%

## Dependencies

- 92a (the registry + revision/diff/rollback primitive), 92 (the tenant-wide override + unknown-model fail-loud the per-agent layer composes with), 92b (tenant-override completion), 92d/92e (the sibling-preserving convenience-verb precedent), 110a (the run-start projection seam).

## Risks / open questions

- **Precedence must be airtight per-field.** The single load-bearing invariant: each of model / temperature / max-tokens / reasoning-effort resolves independently — a per-agent `Temperature` with an unset `Model` must let the tenant `Model` (or config) win for that run. Pinned by a precedence matrix test that sets each field at a different layer.
- **Twin drift (D-094).** The resolution MUST be shared between the production run loop and devstack. The §17.6 failure mode (a fix landing on one binary only) is pre-empted by putting `ActiveLLMOverrides` in `projection` and a twin test asserting identical resolution.
- **Operator clarity on "tenant default" vs "per-agent".** Two layers with similar names risk operator confusion. The semantic is settled here (per-agent overrides the tenant-wide baseline); 92i's Settings copy clarification + the "Model & sampling" agent-config area make it legible in the Console.
- **No silent fallback.** A pinned model that can't resolve a profile must fail loud (parity with Phase 92), never degrade to config default — that would be the §13 silent-degradation anti-pattern.

## Glossary additions

- **per-agent LLM parameters** — a versioned agent-config section (model / temperature / max-tokens / reasoning-effort) that pins one agent's sampling defaults durably; resolves between the session override and the tenant-wide baseline (precedence session › per-agent › tenant-wide baseline › config). Distinct from the **tenant default override** (one spec for every agent in the tenant).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared registry under `-race`).**
- [ ] **Integration test exists, real registry + bus + stub LLM, identity propagation, per-field precedence + unknown-model + rollback failure modes, `-race`.**
- [ ] TS manifest + generated Protocol docs regenerated (`make protocol-ts-gen-check`, `make protocol-docs-gen-check` clean)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-238)
