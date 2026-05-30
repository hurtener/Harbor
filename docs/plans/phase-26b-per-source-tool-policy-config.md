# Phase 26b — per-source-tool-policy-config

## Summary

Expose the tool retry/timeout policy (`tools.ToolPolicy`) as operator YAML —
a `policy: { max_attempts, timeout_ms, ... }` block on each MCP server (and a
per-tool override map keyed by tool name), projected onto the descriptors the
MCP driver builds. Today every tool inherits the hardcoded `tools.DefaultPolicy()`
(30 s per-attempt deadline, 4 total attempts) with no operator knob, so a slow
or throttled tool (e.g. a YouTube metadata call) burns ~4×30 s before failing
loud. This phase makes the budget configurable per source and per tool, with
the same field-level zero-value resolution `tools.ToolPolicy` already uses.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 03
- brief 11

## Brief findings incorporated

- brief 03 §4: the MCP southbound attachment is operator-config-driven
  (`transport_mode` / `command` / `url`); per the §4.4 seam pattern its tuning
  knobs belong in the `MCPServerConfig` YAML, validated pre-boot rather than at
  runtime. This phase adds the policy knob in that same place + style.
- brief 11 §"Tools view": operators reason about tools as catalog entries with
  per-tool posture (auth, approval, content size). A per-tool retry/timeout
  override fits that mental model — a slow tool gets a longer single attempt;
  a flaky one gets more retries — without touching the others.

## Findings I'm departing from (if any)

None.

## Goals

- A `policy:` block on `MCPServerConfig` sets the per-server default
  `tools.ToolPolicy` for every tool that source registers.
- A per-tool override (keyed by tool name) on the same server config replaces
  the per-server default for that tool only.
- The YAML shape is intuitive: `max_attempts` (TOTAL attempts, incl. the first
  — projected to `MaxRetries = max_attempts - 1`) and `timeout_ms` (per-attempt
  deadline). Optional `retry_on` (error-class allowlist) and backoff knobs.
- Zero/omitted fields fall through to `tools.DefaultPolicy()` per-field (the
  existing `ToolPolicy.resolved()` semantics) — a `policy:` that sets only
  `timeout_ms` keeps the default 4 attempts. Documented, not surprising.
- The projection is the single translation seam (config → `tools.ToolPolicy`);
  no second policy definition (CLAUDE.md §13 single-source).
- The operator skills for the `mcp` + `tools` surfaces document the knob
  (§18 same-PR rule).

## Non-goals

- A Protocol method or Console surface for editing policy at runtime — config
  is restart-required (no `reload:"live"` tag). The existing Tools-page
  `tools.content_stats` DisplayMode snapshot is unaffected.
- Per-tool policy for in-proc / HTTP tools wired via embedding Go code — those
  already take `tools.WithPolicy` / `http.WithPolicy` programmatically. A
  `policy:` field on `ToolEntryConfig` (YAML-declared catalog entries) is a
  reasonable consistency follow-up but out of scope here.
- Changing `tools.DefaultPolicy()` itself — the default stays 30 s / 4 attempts.

## Acceptance criteria

- [ ] **AC-1** `MCPServerConfig` gains an optional `policy: ToolPolicyConfig`
  (per-server default) and `tool_policies: map[string]ToolPolicyConfig` (per-tool
  overrides). Both optional; omitting them preserves today's behaviour exactly.
- [ ] **AC-2** `ToolPolicyConfig` exposes `max_attempts int`, `timeout_ms int`,
  optional `retry_on []string`, `backoff_base_ms`, `backoff_mult`,
  `backoff_max_ms`. A projection helper maps it to `tools.ToolPolicy`
  (`MaxRetries = max_attempts - 1` when `max_attempts >= 1`; `TimeoutMS` direct).
- [ ] **AC-3** The per-server policy is injected as `mcpdrv.Config.DefaultPolicy`
  in `cmd/harbor/cmd_dev.go::attachDevMCPServer`; per-tool overrides are threaded
  into `buildToolDescriptor` so a descriptor's `Tool.Policy` reflects the
  override before its Invoke closure captures it.
- [ ] **AC-4** A tool with a per-tool override of `max_attempts: 1, timeout_ms:
  60000` makes exactly ONE attempt with a 60 s deadline (unit/integration test
  asserts attempt count + per-attempt timeout via a fake clock / fake transport).
- [ ] **AC-5** Validation (`internal/config/validate.go`): `max_attempts >= 1`,
  `timeout_ms >= 0`, `retry_on` values in the `ErrorClass` allowlist, per-tool
  override keys non-empty + unique. Field-error style matches the existing MCP
  server validation (`fieldError(prefix+".policy.timeout_ms", ...)`).
- [ ] **AC-6** Omitting `policy:` yields `tools.DefaultPolicy()` for every tool
  (regression: existing MCP smoke + an explicit "no policy → defaults" test).
- [ ] **AC-7** `examples/` config gains a documented `policy:` + `tool_policies:`
  block on an MCP server entry (§10 example-config rule).
- [ ] **AC-8** `scripts/smoke/phase-26b.sh` asserts the projection: a config with
  a per-tool override loads + validates; the projected `tools.ToolPolicy` for that
  tool has the overridden `MaxRetries`/`TimeoutMS` (a `go test` assertion on the
  projection helper, classification `unit-tests`).
- [ ] **AC-9** The `mcp` + `tools` operator skills document the knob + the
  per-field zero-value fall-through (§18 same-PR).

## Files added or changed

- `internal/config/config.go` — `ToolPolicyConfig` struct; `Policy` +
  `ToolPolicies` fields on `MCPServerConfig`.
- `internal/config/validate.go` — validate the new block.
- `internal/config/policy_projection.go` (**NEW**) — `ToolPolicyConfig` →
  `tools.ToolPolicy` projection helper + its `_test.go`.
- `internal/tools/drivers/mcp/mcp.go` — `Config` gains a per-tool policy map;
  `buildToolDescriptor` applies the per-tool override over the server default.
- `cmd/harbor/cmd_dev.go` — `attachDevMCPServer` injects `DefaultPolicy` + the
  per-tool map from `ms.Policy` / `ms.ToolPolicies`.
- `examples/*.yaml` — documented `policy:` block (AC-7).
- `scripts/smoke/phase-26b.sh` (**NEW**).
- `docs/skills/add-an-in-process-tool/SKILL.md` + the MCP-surface skill — the
  knob (§18).
- `docs/decisions.md` — D-175.
- `docs/plans/README.md` — Status + row.

## Public API surface

- `config.ToolPolicyConfig` (YAML-facing struct).
- `config.MCPServerConfig.Policy *ToolPolicyConfig` +
  `config.MCPServerConfig.ToolPolicies map[string]ToolPolicyConfig`.
- A projection function `func (ToolPolicyConfig) ToToolPolicy() tools.ToolPolicy`
  (or a package-level helper) — the single config→policy seam.
- `mcpdrv.Config` gains an optional `ToolPolicies map[string]tools.ToolPolicy`
  (per-tool overrides); `DefaultPolicy` already exists.

## Test plan

- **Unit:** the projection helper (`max_attempts → MaxRetries` off-by-one;
  per-field zero fall-through; `retry_on` class mapping); validation rejects
  (`max_attempts: 0`, unknown `retry_on`, duplicate/empty override key).
- **Integration:** an MCP provider built with a per-server default + a per-tool
  override over a fake transport; assert the overridden tool makes the configured
  attempt count with the configured per-attempt deadline, and a sibling tool uses
  the server default. Real `mcp` driver + fake transport on the seam, identity
  propagated, ≥1 failure mode (a forced timeout class), under `-race`.
- **Conformance:** N/A — no new driver interface; the existing tool-policy shell
  (`RunWithPolicy`) is unchanged.
- **Concurrency / leak:** the MCP provider is a reusable artifact; the existing
  concurrent-reuse test covers it — extend it to assert a per-tool override does
  not leak across concurrent invocations of different tools on one provider.

## Smoke script additions

- `scripts/smoke/phase-26b.sh` (classification `unit-tests`): `go test` the
  projection helper + a load-and-validate of a fixture config carrying a per-tool
  override; assert the projected policy's `MaxRetries`/`TimeoutMS`.

## Coverage target

- `internal/config`: 85% (projection + validation).
- `internal/tools/drivers/mcp`: 85% (per-tool override application).

## Dependencies

- 26 (Tool catalog core — `tools.ToolPolicy` + `RunWithPolicy`).
- 28 (MCP southbound driver — the `Config.DefaultPolicy` slot + `buildToolDescriptor`).

## Risks / open questions

- **`max_attempts` (YAML, total) vs `MaxRetries` (struct, total−1).** The
  off-by-one lives only in the projection helper; documented in AC-2 + the skill.
  Chosen `max_attempts` because operators think in total attempts, not retries.
- **Per-field zero-value fall-through** (`ToolPolicy.resolved()`): a partial
  `policy:` inherits defaults per field. Setting only `timeout_ms: 5000` still
  yields 4 attempts. Documented, not surprising — surfaced in the skill.
- **Registry `Policy` vs descriptor `Policy` are two fields.** Runtime behaviour
  is governed by the descriptor's `Tool.Policy` (set in `buildToolDescriptor`),
  NOT the registry's `ServerRegistration.Policy` (a Console read projection). This
  phase wires the descriptor path; it MAY also set the registry projection so the
  Console reflects the configured policy, but execution correctness rides on the
  descriptor.

## Glossary additions

- **Tool policy (operator-configurable)** — extend the existing `tools.ToolPolicy`
  glossary note with the YAML `policy:` / `tool_policies:` knob (per-MCP-server
  default + per-tool override) and the `max_attempts`/`timeout_ms` projection.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, policy is identity-independent; tool invocation already carries identity.
- [ ] **Concurrent-reuse test passes** — the MCP provider is reusable; the per-tool override must not bleed across concurrent invocations (extend the existing N≥100 test).
- [ ] **Integration test exists** — MCP provider with per-server + per-tool policy over a fake transport, asserting attempt count + per-attempt deadline, under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
