#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92j smoke — agent-config PER-AGENT LLM PARAMETERS (the versioned
# model/temperature/max-tokens/reasoning-effort section). Planning-stage
# skeleton: keeps preflight green until the surface lands. The implementing
# PR replaces the skip below with the real assertions:
#
#   - the `LLMParams` section symbol in internal/agentcfg
#   - the `agent_config.set_llm_params` method constant + handler branch +
#     admin-scope gate (non-admin 403)
#   - the `ActiveLLMOverrides` projection symbol, grepped in BOTH
#     cmd/harbor/cmd_dev_runloop.go AND harbortest/devstack/devstack.go
#     (the D-094 twin)
#   - the extended `ComposeLLMOverrides` arity (session > agent > tenant)
#   - the regenerated TS wire manifest + generated methods.md/types.md rows
#   - live (skip-if-404): admin sets a per-agent model (200) reflected by
#     agent_config.get; non-admin rejected (403); agent_config.diff reports
#     the LLM-params delta.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92j: planning-stage skeleton — per-agent LLM-params surface not yet implemented"

smoke_summary
