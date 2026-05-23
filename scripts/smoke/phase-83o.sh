#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83o — scaffold reads operator-edited harbor.yaml + materialises
# per-custom-tool Go stubs + `--patch` preserves operator code. D-154.
#
# All assertions are static-only (file existence, grep). The engine
# tests in cmd/harbor/scaffold/scaffold_from_yaml_test.go and the
# cobra-level tests in cmd/harbor/cmd_scaffold_test.go cover the
# behaviour end-to-end.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Config schema + validator.
# ----------------------------------------------------------------------------
assert_grep_present 'type CustomToolConfig struct' "internal/config/config.go" \
    "CustomToolConfig type declared (D-154)"
assert_grep_present 'Custom\s*\[\]CustomToolConfig' "internal/config/config.go" \
    "ToolsConfig.Custom field declared"
assert_grep_present 'allowedCustomToolTypes' "internal/config/validate.go" \
    "validator carries the V1.1 yaml-shorthand type allowlist"
assert_grep_present 'KnownCustomToolTypes' "internal/config/validate.go" \
    "config exposes KnownCustomToolTypes() for the scaffold engine"
assert_grep_present 'collides with tools.built_in entry' \
    "internal/config/validate.go" \
    "validator catches name collisions between tools.custom and tools.built_in"

# ----------------------------------------------------------------------------
# Scaffold engine changes.
# ----------------------------------------------------------------------------
assert_grep_present 'FromConfigPath string' "cmd/harbor/scaffold/scaffold.go" \
    "Options.FromConfigPath declared (D-154)"
assert_grep_present 'Patch\s*bool' "cmd/harbor/scaffold/scaffold.go" \
    "Options.Patch declared (D-154)"
assert_grep_present 'Skipped\s*\[\]string' "cmd/harbor/scaffold/scaffold.go" \
    "Result.Skipped declared (D-154)"
assert_grep_present 'ErrUpstreamConfigInvalid' \
    "cmd/harbor/scaffold/scaffold.go" \
    "ErrUpstreamConfigInvalid sentinel declared"
assert_grep_present 'func loadUpstreamConfig' \
    "cmd/harbor/scaffold/render.go" \
    "upstream-yaml loader declared"
assert_grep_present 'func renderCustomTools' \
    "cmd/harbor/scaffold/render.go" \
    "per-tool fan-out renderer declared"
assert_grep_present 'func copyUpstreamYAML' \
    "cmd/harbor/scaffold/render.go" \
    "upstream-yaml verbatim copy helper declared"

# ----------------------------------------------------------------------------
# Templates.
# ----------------------------------------------------------------------------
assert_file "cmd/harbor/scaffold/templates/minimal-react/tool.go.tmpl" \
    "per-custom-tool Go stub template ships"
assert_file "cmd/harbor/scaffold/templates/minimal-react/tool_test.go.tmpl" \
    "per-custom-tool test template ships"
assert_grep_present 'RegisterTools' \
    "cmd/harbor/scaffold/templates/minimal-react/agent.go.tmpl" \
    "agent.go template carries the RegisterTools function"

# ----------------------------------------------------------------------------
# Cobra wiring.
# ----------------------------------------------------------------------------
assert_grep_present 'flagScaffoldFromConfig' "cmd/harbor/cmd_scaffold.go" \
    "scaffold cobra subcommand wires --from-config"
assert_grep_present 'flagScaffoldPatch' "cmd/harbor/cmd_scaffold.go" \
    "scaffold cobra subcommand wires --patch"
assert_grep_present 'CodeUpstreamConfigInvalid' "cmd/harbor/cmd_scaffold.go" \
    "scaffold CLIError carries the upstream_config_invalid code"

# ----------------------------------------------------------------------------
# docs/CONFIG.md.
# ----------------------------------------------------------------------------
assert_grep_present '### tools.custom' "docs/CONFIG.md" \
    "CONFIG.md documents the new tools.custom field"

smoke_summary
